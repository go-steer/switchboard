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
// The router takes mTLS only (Agent Substrate v1.0): NewRouterClient builds
// the client that presents the pod's certificate and verifies the router's.
// A plain http:// router still works, for a development router or a test.
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
	"io"
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
	"github.com/go-steer/switchboard/pkg/daemon"
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
	// IdleAfter is spec.idle.suspendAfter: how long a task must see no
	// traffic before ax suspends it. Zero without Idle.
	IdleAfter time.Duration
}

// LoadTemplate reads a Task manifest (YAML or JSON), as k8s-lookout's
// --ax-task-template takes one. It is strict, so a mistake fails at startup
// rather than at the first thread: unknown fields, duplicate keys and a
// second document are refused, and so is anything ax's own validation would
// refuse about the port and idling. A template without spec.http.port is
// refused too: nothing in its tasks would answer the router.
//
// Only the spec is kept. Name and atespace are each task's, and a status or
// creation time copied into every task would give them all one identity
// (caught in review).
func LoadTemplate(path string) (*Template, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ax task template: %w", err)
	}
	return ParseTemplate(raw, path)
}

// ParseTemplate is LoadTemplate for a manifest already read; path names it in
// errors.
func ParseTemplate(raw []byte, path string) (*Template, error) {
	if moreDocuments(raw) {
		return nil, fmt.Errorf("ax task template %s: one document, not several", path)
	}
	js, err := yaml.YAMLToJSONStrict(raw)
	if err != nil {
		return nil, fmt.Errorf("ax task template %s: %w", path, err)
	}
	var t axapi.Task
	if err := protojson.Unmarshal(js, &t); err != nil {
		return nil, fmt.Errorf("ax task template %s: %w", path, err)
	}
	if t.GetKind() != "" && t.GetKind() != "Task" {
		return nil, fmt.Errorf("ax task template %s: kind is %q, want Task", path, t.GetKind())
	}
	if t.GetSpec() == nil {
		return nil, fmt.Errorf("ax task template %s: spec is required", path)
	}
	if err := checkSpec(t.GetSpec()); err != nil {
		return nil, fmt.Errorf("ax task template %s: %w", path, err)
	}
	tmpl := &Template{
		task: &axapi.Task{ApiVersion: t.GetApiVersion(), Kind: t.GetKind(), Spec: t.GetSpec()},
		Idle: t.GetSpec().GetIdle() != nil,
	}
	if tmpl.Idle {
		tmpl.IdleAfter, _ = time.ParseDuration(t.GetSpec().GetIdle().GetSuspendAfter()) // checked above
	}
	return tmpl, nil
}

// The runner's own ports, which a task's server cannot take
// (ax pkg/apis/v1alpha1/types.go).
const (
	runnerPort         = 80
	runnerMetadataPort = 8099
	minSuspendAfter    = 10 * time.Second
)

// checkSpec is ax's validation of the port and idling, which a template
// should fail at startup rather than at every thread's CreateTask.
func checkSpec(spec *axapi.TaskSpec) error {
	port := spec.GetHttp().GetPort()
	switch {
	case port == 0:
		return errors.New("spec.http.port is required: it is the port the agent serves the session contract on, which the router forwards to")
	case port < 1 || port > 65535:
		return fmt.Errorf("spec.http.port: %d is not a valid port", port)
	case port == runnerPort || port == runnerMetadataPort:
		return fmt.Errorf("spec.http.port: %d is one of the task runner's own ports", port)
	}
	if idle := spec.GetIdle(); idle != nil {
		d, err := time.ParseDuration(idle.GetSuspendAfter())
		switch {
		case idle.GetSuspendAfter() == "":
			return errors.New("spec.idle.suspendAfter: is required")
		case err != nil:
			return fmt.Errorf("spec.idle.suspendAfter: %w", err)
		case d < minSuspendAfter:
			return fmt.Errorf("spec.idle.suspendAfter: %s is shorter than ax's minimum of %s", d, minSuspendAfter)
		}
		if p := idle.GetBusyPath(); p != "" && !strings.HasPrefix(p, "/") {
			return fmt.Errorf("spec.idle.busyPath: %q must start with /", p)
		}
	}
	return nil
}

// moreDocuments reports whether raw holds a second YAML document, which a
// YAML-to-JSON conversion would silently drop.
func moreDocuments(raw []byte) bool {
	seen := false
	for _, line := range strings.Split(string(raw), "\n") {
		t := strings.TrimSpace(line)
		if t == "---" || strings.HasPrefix(t, "--- ") {
			if seen {
				return true
			}
			continue
		}
		if t != "" && !strings.HasPrefix(t, "#") {
			seen = true
		}
	}
	return false
}

// Config is what a Client needs.
type Config struct {
	// Server is ax-server's gRPC address, host:port.
	Server string
	// Router is the atenet router's base URL, scheme://host[:port]. An
	// https router needs HTTPClient from NewRouterClient.
	Router string
	// Atespace the tasks live in; empty is DefaultAtespace.
	Atespace string
	// Template every task is created from.
	Template *Template
	// ReadyTimeout bounds all of Ensure — create, resume and the wait for the
	// task to answer; zero is DefaultReadyTimeout.
	ReadyTimeout time.Duration
	// HTTPClient is the base for requests through the router: for an https
	// router, RouterClient.HTTP, which holds the client certificate. For an
	// http router nil is a client that follows no redirects
	// (daemon.NoRedirects). Its transport is wrapped, never replaced, and
	// its Timeout is dropped: these clients carry SSE streams.
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
	case cfg.Template == nil || cfg.Template.task == nil:
		return nil, errors.New("ax: a task template is required (LoadTemplate)")
	case cfg.Server == "" && cfg.Conn == nil:
		return nil, errors.New("ax: the ax-server address is required")
	case !validRouter(cfg.Router):
		return nil, fmt.Errorf("ax: router %q: want http(s)://host[:port]", cfg.Router)
	case RouterIsTLS(cfg.Router) && cfg.HTTPClient == nil:
		// A default client would trust the public roots and present no
		// certificate, which the router refuses; fail here instead.
		return nil, fmt.Errorf("ax: router %s: an https router needs a client with the pod's certificate (NewRouterClient)", cfg.Router)
	}
	if cfg.Atespace == "" {
		cfg.Atespace = DefaultAtespace
	}
	if err := ValidName(cfg.Atespace); err != nil {
		return nil, fmt.Errorf("ax: atespace: %w", err)
	}
	if cfg.ReadyTimeout <= 0 {
		cfg.ReadyTimeout = DefaultReadyTimeout
	}
	if cfg.HTTPClient == nil {
		// No redirects: these clients go to the daemon contract, whose
		// bearer and asserted caller a redirect would carry elsewhere
		// (caught in review).
		cfg.HTTPClient = &http.Client{CheckRedirect: daemon.NoRedirects}
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

// Ensured is what Ensure found.
type Ensured struct {
	// Created: the task did not exist.
	Created bool
	// Drifted: the task existed with a spec that is not the template's.
	// Tasks are immutable, so a template change (a rotated token, say)
	// never reaches a task created before it; the caller decides whether
	// that matters (caught in review).
	Drifted bool
}

// Phases Ensure acts on (ax pkg/apis/v1alpha1/types.go).
const (
	phaseRunning     = "Running"
	phaseTerminating = "Terminating"
)

// Ensure makes sure task exists, is running and answers through the router.
// It creates the task from the template; if ax refuses because it exists
// (tasks are immutable), it reuses it — unless it is being deleted — and
// resumes it unless it is already running. Then it waits for the task's
// /readyz: with spec.http.port the runner reports ready only once the
// agent's own server is listening.
func (c *Client) Ensure(ctx context.Context, task string) (Ensured, error) {
	if err := ValidName(task); err != nil {
		return Ensured{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.ReadyTimeout)
	defer cancel()

	t := proto.Clone(c.cfg.Template.task).(*axapi.Task)
	t.Metadata = &axapi.ObjectMeta{Name: task, Atespace: c.cfg.Atespace}
	if t.ApiVersion == "" {
		t.ApiVersion = "ax.io/v1alpha1"
	}
	if t.Kind == "" {
		t.Kind = "Task"
	}
	var out Ensured
	resume := true
	if _, err := c.api.CreateTask(ctx, &axapi.CreateTaskRequest{Task: t}); err != nil {
		// Ask rather than match the error. ax saves a task before
		// provisioning it, so even a create that failed for another reason
		// can leave one to resume; a resume that fails too is reported then.
		existing, gerr := c.api.GetTask(ctx, &axapi.GetTaskRequest{Atespace: c.cfg.Atespace, Name: task})
		if gerr != nil {
			return out, fmt.Errorf("ax: create task %s: %w", c.actor(task), err)
		}
		switch existing.GetStatus().GetPhase() {
		case phaseTerminating:
			// Resuming it would bring back a task being deleted (caught in
			// review).
			return out, fmt.Errorf("ax: task %s is being deleted; try again once it is gone", c.actor(task))
		case phaseRunning:
			resume = false // a resume would only reconcile again
		}
		out.Drifted = !proto.Equal(existing.GetSpec(), t.GetSpec())
	} else {
		out.Created = true
	}
	if resume {
		if _, err := c.api.ResumeTask(ctx, &axapi.ResumeTaskRequest{Atespace: c.cfg.Atespace, Name: task}); err != nil {
			return out, fmt.Errorf("ax: resume task %s: %w", c.actor(task), err)
		}
	}
	return out, c.waitReady(ctx, task)
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
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10)) // keep the connection
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			last = resp.Status
		} else if ctx.Err() == nil || last == "" {
			// A request cut short by the deadline says nothing about the
			// task; keep what it last answered.
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

// Inspection is what Inspect found of a task.
type Inspection struct {
	Exists  bool
	Phase   string
	Drifted bool // exists with a spec that is not the template's
}

// Inspect looks a task up without touching it — no create, no resume — so a
// caller can act on a drifted task before waking it (caught in review: a
// drifted task that could not start was never replaced).
func (c *Client) Inspect(ctx context.Context, task string) (Inspection, error) {
	t, err := c.api.GetTask(ctx, &axapi.GetTaskRequest{Atespace: c.cfg.Atespace, Name: task})
	if isNotFound(err) {
		return Inspection{}, nil
	}
	if err != nil {
		return Inspection{}, fmt.Errorf("ax: inspect task %s: %w", c.actor(task), err)
	}
	return Inspection{Exists: true, Phase: t.GetStatus().GetPhase(), Drifted: !proto.Equal(t.GetSpec(), c.cfg.Template.task.GetSpec())}, nil
}

// Task is a task as List reports it.
type Task struct {
	Name  string
	Phase string
}

// listPage is how many tasks List asks ax for at a time, and maxListPages
// how many pages it reads before giving up: a server ignoring the offset
// would otherwise keep it going forever.
const (
	listPage     = 100
	maxListPages = 100
)

// List reports the tasks in the client's atespace whose names match, page by
// page; a nil match is every task.
func (c *Client) List(ctx context.Context, match func(name string) bool) ([]Task, error) {
	var out []Task
	seen := map[string]bool{} // pages over a changing set can repeat a task
	for page := range maxListPages {
		resp, err := c.api.ListTasks(ctx, &axapi.ListTasksRequest{Atespace: c.cfg.Atespace, Limit: listPage, Offset: int64(page) * listPage})
		if err != nil {
			return out, fmt.Errorf("ax: list tasks in %s: %w", c.cfg.Atespace, err)
		}
		for _, t := range resp.GetTasks() {
			if n := t.GetMetadata().GetName(); !seen[n] && (match == nil || match(n)) {
				seen[n] = true
				out = append(out, Task{Name: n, Phase: t.GetStatus().GetPhase()})
			}
		}
		if len(resp.GetTasks()) < listPage {
			return out, nil
		}
	}
	return out, fmt.Errorf("ax: list tasks in %s: more than %d pages", c.cfg.Atespace, maxListPages)
}

// NameMatcher reports whether a name is one TaskName could give for prefix:
// the prefix as TaskName writes it, a dash, and the 16-character hash.
func NameMatcher(prefix string) func(string) bool {
	p := TaskName(prefix)
	p = p[:len(p)-16] // the prefix and its dash, as TaskName clamps them
	return func(name string) bool {
		if !strings.HasPrefix(name, p) || len(name) != len(p)+16 {
			return false
		}
		for _, r := range name[len(p):] {
			if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
				return false
			}
		}
		return true
	}
}

// Router is the router's base URL: the daemon URL of every task.
func (c *Client) Router() string { return c.cfg.Router }

// HTTPClient is a client whose requests reach task through the router: the
// base client with ActorHeader added.
func (c *Client) HTTPClient(task string) *http.Client {
	hc := *c.cfg.HTTPClient
	hc.Transport = Transport(c.cfg.HTTPClient.Transport, c.actor(task))
	hc.Timeout = 0 // it would cut the SSE streams these clients carry
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

// TaskName builds a deterministic task name: the prefix made name-safe, then a
// hash of the prefix as given and the parts (16 hex characters), clamped so
// the result is always a valid name. The same arguments always give the same
// name — a restart, or another gateway pod, finds the same task — and the
// hash keeps conversation IDs and callers out of it. The raw prefix is hashed
// because making it name-safe loses characters: "sb-triage" and "sb-triage-"
// would otherwise name one task for two agents (caught in review).
func TaskName(prefix string, parts ...string) string {
	h := sha256.New()
	for _, p := range append([]string{prefix}, parts...) {
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

// RouterIsTLS reports whether router is an https URL.
func RouterIsTLS(router string) bool {
	u, err := url.Parse(router)
	return err == nil && u.Scheme == "https"
}

// validRouter: the router is a base URL the contract's paths are appended to,
// so scheme and host only.
func validRouter(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" &&
		u.Path == "" && u.User == nil && u.RawQuery == "" && !u.ForceQuery && !strings.Contains(s, "#")
}
