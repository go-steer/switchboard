// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package ax starts the ax tasks switchboard's ax agents run in
// (docs/ax-backend.md, #169). It is the only code that talks to ax-server, and
// it exposes none of ax's types: a Template loaded from a file, a Client that
// makes sure a named task exists and is ready, and the transport that reaches
// a task through Agent Substrate's atenet router.
//
// Reaching the agent inside a task is not this package's business. A task
// serves the same session contract a daemon does (spec.http.port), so the
// router plus Transport is a daemon URL like any other for pkg/daemon.
//
// ax-server has no authentication: the client dials it in plaintext, in the
// cluster, as k8s-lookout's ax sink does. A NetworkPolicy is the protection
// until ax grows some.
package ax

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"sigs.k8s.io/yaml"

	"github.com/go-steer/switchboard/internal/axapi"
)

// ActorHeader is the header the atenet router routes on: <atespace>/<task>.
const ActorHeader = "ate-target-actor"

// DefaultAtespace is ax's own default.
const DefaultAtespace = "default"

// Template is a task manifest every task of an agent is created from. Only
// its name and atespace are filled in per task.
type Template struct {
	task *axapi.Task
	// Idle reports whether the template lets its tasks suspend when idle;
	// without it a task runs until it is deleted.
	Idle bool
}

// LoadTemplate reads a Task manifest (YAML or JSON), as k8s-lookout's
// --ax-task-template takes one. Unknown fields are errors, so a typo fails at
// startup rather than at the first thread. A template without spec.http.port
// is refused: nothing in its tasks would answer the router.
func LoadTemplate(path string) (*Template, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ax task template: %w", err)
	}
	js, err := yaml.YAMLToJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("ax task template %s: %w", path, err)
	}
	var t axapi.Task
	if err := protojson.Unmarshal(js, &t); err != nil {
		return nil, fmt.Errorf("ax task template %s: %w", path, err)
	}
	switch {
	case t.GetKind() != "" && t.GetKind() != "Task":
		return nil, fmt.Errorf("ax task template %s: kind is %q, want Task", path, t.GetKind())
	case t.GetSpec() == nil:
		return nil, fmt.Errorf("ax task template %s: spec is required", path)
	case t.GetSpec().GetHttp().GetPort() <= 0:
		return nil, fmt.Errorf("ax task template %s: spec.http.port is required: it is the port the agent serves the session contract on, which the router forwards to", path)
	}
	return &Template{task: &t, Idle: t.GetSpec().GetIdle().GetSuspendAfter() != ""}, nil
}

// Config is what a Client needs.
type Config struct {
	// Server is ax-server's gRPC address, host:port.
	Server string
	// Router is the atenet router's base URL, scheme://host[:port].
	Router string
	// Atespace the tasks live in; empty is DefaultAtespace.
	Atespace string
	// Template every task is created from.
	Template *Template
	// ReadyTimeout bounds Ensure's wait for a task to answer; zero is
	// DefaultReadyTimeout.
	ReadyTimeout time.Duration
	// HTTPClient is the base for requests through the router; nil is a plain
	// client. Its transport is wrapped, never replaced.
	HTTPClient *http.Client
	// Conn overrides the connection to ax-server; nil dials Server. A test
	// seam.
	Conn grpc.ClientConnInterface
}

// DefaultReadyTimeout is how long Ensure waits for a task, which covers a
// cold wake after a long suspend.
const DefaultReadyTimeout = 2 * time.Minute

// Client makes sure tasks exist and answer.
type Client struct {
	cfg  Config
	api  axapi.AXClient
	conn *grpc.ClientConn // owned: closed by Close
}

// New validates cfg and connects to ax-server. The connection is lazy: an
// unreachable server is an error from the first call, not from New.
func New(cfg Config) (*Client, error) {
	switch {
	case cfg.Template == nil:
		return nil, errors.New("ax: a task template is required")
	case cfg.Server == "" && cfg.Conn == nil:
		return nil, errors.New("ax: the ax-server address is required")
	case !validRouter(cfg.Router):
		return nil, fmt.Errorf("ax: router %q: want http(s)://host[:port]", cfg.Router)
	}
	if cfg.Atespace == "" {
		cfg.Atespace = DefaultAtespace
	}
	if cfg.ReadyTimeout <= 0 {
		cfg.ReadyTimeout = DefaultReadyTimeout
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{}
	}
	c := &Client{cfg: cfg}
	conn := cfg.Conn
	if conn == nil {
		cc, err := grpc.NewClient(cfg.Server, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return nil, fmt.Errorf("ax: ax-server %s: %w", cfg.Server, err)
		}
		c.conn, conn = cc, cc
	}
	c.api = axapi.NewAXClient(conn)
	return c, nil
}

// Close releases the connection to ax-server.
func (c *Client) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// Ensure makes sure task exists, is running and answers through the router:
// it creates the task from the template, reuses it if it already exists
// (tasks are immutable, so a second create of the name is refused), resumes
// it, and waits for its /readyz. With spec.http.port the runner reports ready
// only once the agent's own server is listening.
func (c *Client) Ensure(ctx context.Context, task string) error {
	if err := ValidName(task); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.ReadyTimeout)
	defer cancel()

	t := proto.Clone(c.cfg.Template.task).(*axapi.Task)
	if t.Metadata == nil {
		t.Metadata = &axapi.ObjectMeta{}
	}
	t.Metadata.Name = task
	t.Metadata.Atespace = c.cfg.Atespace
	if t.ApiVersion == "" {
		t.ApiVersion = "ax.io/v1alpha1"
	}
	if t.Kind == "" {
		t.Kind = "Task"
	}
	if _, err := c.api.CreateTask(ctx, &axapi.CreateTaskRequest{Task: t}); err != nil {
		// Ask rather than match the error, so a failure that is not "it
		// exists" still surfaces.
		if _, gerr := c.api.GetTask(ctx, &axapi.GetTaskRequest{Atespace: c.cfg.Atespace, Name: task}); gerr != nil {
			return fmt.Errorf("ax: create task %s: %w", c.actor(task), err)
		}
	}
	if _, err := c.api.ResumeTask(ctx, &axapi.ResumeTaskRequest{Atespace: c.cfg.Atespace, Name: task}); err != nil {
		return fmt.Errorf("ax: resume task %s: %w", c.actor(task), err)
	}
	return c.waitReady(ctx, task)
}

// waitReady polls /readyz through the router until it answers 200.
func (c *Client) waitReady(ctx context.Context, task string) error {
	hc := c.HTTPClient(task)
	var last string
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.Router+"/readyz", nil)
		if err != nil {
			return err
		}
		resp, err := hc.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			last = resp.Status
		} else {
			last = err.Error()
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("ax: task %s not ready (last: %s): %w", c.actor(task), last, ctx.Err())
		case <-time.After(readyPoll):
		}
	}
}

// readyPoll is how often waitReady asks.
var readyPoll = time.Second

// Delete removes a task. A task that is already gone is not an error.
func (c *Client) Delete(ctx context.Context, task string) error {
	_, err := c.api.DeleteTask(ctx, &axapi.DeleteTaskRequest{Atespace: c.cfg.Atespace, Name: task})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("ax: delete task %s: %w", c.actor(task), err)
	}
	return nil
}

// Router is the router's base URL: the daemon URL of every task.
func (c *Client) Router() string { return c.cfg.Router }

// HTTPClient is a client whose requests reach task through the router: the
// base client with ActorHeader added.
func (c *Client) HTTPClient(task string) *http.Client {
	hc := *c.cfg.HTTPClient
	hc.Transport = Transport(c.cfg.HTTPClient.Transport, c.actor(task))
	return &hc
}

func (c *Client) actor(task string) string { return c.cfg.Atespace + "/" + task }

// Transport adds ActorHeader to every request base carries; nil base is
// http.DefaultTransport.
func Transport(base http.RoundTripper, actor string) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return actorTransport{base: base, actor: actor}
}

type actorTransport struct {
	base  http.RoundTripper
	actor string
}

func (t actorTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(ActorHeader, t.actor)
	return t.base.RoundTrip(r)
}

// --- names -------------------------------------------------------------------

// nameRE is a task name ax accepts: a lowercase RFC 1123 label.
var nameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// maxName is the longest task name ax accepts.
const maxName = 63

// ValidName reports whether ax would accept name.
func ValidName(name string) error {
	if len(name) > maxName || !nameRE.MatchString(name) {
		return fmt.Errorf("ax: task name %q: want a lowercase RFC 1123 label of at most %d characters", name, maxName)
	}
	return nil
}

// TaskName builds a deterministic task name: prefix, then a hash of the parts
// (16 hex characters), clamped so the result is always a valid name. The same
// parts always give the same name — a restart, or another gateway pod, finds
// the same task — and the hash keeps conversation IDs and callers out of it.
func TaskName(prefix string, parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0}) // so ("ab","c") and ("a","bc") differ
	}
	sum := hex.EncodeToString(h.Sum(nil))[:16]
	prefix = slug(prefix)
	if room := maxName - len(sum) - 1; len(prefix) > room {
		prefix = strings.TrimRight(prefix[:room], "-")
	}
	if prefix == "" {
		return "t-" + sum
	}
	return prefix + "-" + sum
}

// slug lowercases s and turns everything a name can't carry into '-'.
func slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// validRouter: the router is a base URL the contract's paths are appended to,
// so scheme and host only.
func validRouter(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" &&
		u.Path == "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}
