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

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-steer/switchboard/pkg/approval"
	"github.com/go-steer/switchboard/pkg/ax"
	"github.com/go-steer/switchboard/pkg/chat"
	"github.com/go-steer/switchboard/pkg/daemon"
)

// ax agents (#170, docs/ax-backend.md).
//
// An agent of kind "ax" is not a daemon somebody runs: it is a task template,
// and each conversation gets a task of its own, created from it on the first
// message, woken by Agent Substrate's router on every one after, and reached
// through that router. Inside the task the agent serves the same session
// contract a daemon does, so past the task's start nothing in the router
// knows the difference: daemonOf and approvalsOf hand back the task's
// clients.

const agentKindAX = "ax"

// AXConfig is an ax agent's ax block.
type AXConfig struct {
	// Server is ax-server's gRPC address, host:port.
	Server string `json:"server"`
	// Router is the atenet router's base URL.
	Router string `json:"router"`
	// Atespace the tasks live in; empty is ax's default.
	Atespace string `json:"atespace,omitempty"`
	// TaskTemplateFile is a full ax Task manifest every task is created
	// from. A file, because it carries the agent's token in its env.
	TaskTemplateFile string `json:"task_template_file"`
	// Scope is which conversations share a task (#172): "thread" (the
	// default), a task per conversation; "owner", a task per person — their
	// personal agent, every thread they start with it a session in it; or
	// "shared", one task for everyone.
	Scope string `json:"scope,omitempty"`
	// ReadyTimeout bounds a task's start; empty is two minutes.
	ReadyTimeout string `json:"ready_timeout,omitempty"`
	// StreamLinger is how long after its last traffic a conversation keeps
	// its streams to the task open; empty is one minute, and at least ten
	// seconds. An open stream is traffic to ax, whose idle clock starts when
	// the stream closes, so a task suspends about linger + suspendAfter after
	// its last turn (#171).
	StreamLinger string `json:"stream_linger,omitempty"`
}

// minStreamLinger bounds stream_linger from below: shorter, a conversation
// would let go between a message and its reply.
const minStreamLinger = 10 * time.Second

// defaultStreamLinger is how long an ax conversation keeps its streams open
// after its last traffic.
const defaultStreamLinger = time.Minute

// The scopes.
const (
	axScopeThread = "thread"
	axScopeOwner  = "owner"
	axScopeShared = "shared"
)

// axAgent is what an ax agent holds instead of a daemon client: the ax-server
// client, and each task's clients, made on first use.
type axAgent struct {
	client    *ax.Client
	token     string
	scope     string
	approvals bool
	idle      bool          // the template lets tasks suspend
	idleAfter time.Duration // the template's suspendAfter
	linger    time.Duration // streams close this long after the last traffic
}

// taskClients are one task's contract clients.
type taskClients struct {
	daemon    *daemon.Client
	approvals *approval.Client
}

// forTask is task's clients: the contract through the router, addressed to
// the task by the actor header. Built per call — they hold no connections of
// their own (the transport is shared), and a cache would grow with every
// conversation ever seen (caught in review).
func (x *axAgent) forTask(task string) (*taskClients, error) {
	cfg := daemon.Config{BaseURL: x.client.Router(), BearerToken: x.token, HTTPClient: x.client.HTTPClient(task)}
	dc, err := daemon.New(cfg)
	if err != nil {
		return nil, err
	}
	tc := &taskClients{daemon: dc}
	if x.approvals {
		if tc.approvals, err = approval.New(cfg); err != nil {
			return nil, err
		}
	}
	return tc, nil
}

// taskName is the task a conversation of this agent lives in: deterministic,
// so a restart or another gateway pod arrives at the same one. caller is who
// starts the conversation, which names an owner-scope task.
func (x *axAgent) taskName(agentName, gateway, conv, caller string) string {
	switch x.scope {
	case axScopeOwner:
		return ax.TaskName("sb-"+agentName+"-u", gateway, caller)
	case axScopeShared:
		return ax.TaskName("sb-"+agentName+"-shared", gateway)
	}
	return ax.TaskName("sb-"+agentName, gateway, conv)
}

// axClients caches ax-server clients by everything that builds one, so the
// registry, which rebuilds every runtime agent on each change, does not open
// a connection to ax-server every time and leak the last one.
type axClients struct {
	mu      sync.Mutex
	clients map[string]axClient
}

// axClient is a cached client and what its template said about idling.
type axClient struct {
	client    *ax.Client
	idle      bool
	idleAfter time.Duration
}

func (c *axClients) get(key string, mk func() (axClient, error)) (axClient, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cl, ok := c.clients[key]; ok {
		return cl, nil
	}
	cl, err := mk()
	if err != nil {
		return axClient{}, err
	}
	if c.clients == nil {
		c.clients = make(map[string]axClient)
	}
	c.clients[key] = cl
	return cl, nil
}

// buildAXAgent validates an ax agent's entry and builds its clients.
func buildAXAgent(ac AgentConfig, b agentBuilder) (*axAgent, error) {
	c := ac.AX
	switch {
	case c == nil:
		return nil, errors.New(`.ax is required for kind "ax": server, router and task_template_file`)
	case ac.DaemonURL != "":
		return nil, errors.New(`.daemon_url is not for kind "ax": its tasks are reached through .ax.router`)
	case ac.Auth != nil:
		// The router and ax-server take no credentials today (docs/ax-backend.md).
		return nil, errors.New(`.auth is not supported for kind "ax" yet: Agent Substrate's router takes no client certificate`)
	case strings.TrimSpace(ac.TokenEnv) == "":
		return nil, errors.New(".token_env is required: the variable holding the token the task's agent expects (its MAST_ATTACH_TOKEN)")
	case c.Server == "" || c.Router == "" || c.TaskTemplateFile == "":
		return nil, errors.New(".ax needs server, router and task_template_file")
	}
	scope := c.Scope
	if scope == "" {
		scope = axScopeThread
	}
	switch scope {
	case axScopeThread, axScopeOwner, axScopeShared:
	default:
		return nil, fmt.Errorf(".ax.scope %q: want %q, %q or %q", c.Scope, axScopeThread, axScopeOwner, axScopeShared)
	}
	var ready time.Duration
	if c.ReadyTimeout != "" {
		d, err := time.ParseDuration(c.ReadyTimeout)
		if err != nil || d <= 0 || d > maxReadyTimeout {
			// Capped: every other message in the thread waits on a start.
			return nil, fmt.Errorf(".ax.ready_timeout %q: want a duration up to %s, like 2m", c.ReadyTimeout, maxReadyTimeout)
		}
		ready = d
	}
	token := os.Getenv(ac.TokenEnv)
	if token == "" {
		return nil, fmt.Errorf(": no agent token in $%s", ac.TokenEnv)
	}
	raw, err := os.ReadFile(c.TaskTemplateFile)
	if err != nil {
		return nil, fmt.Errorf(".ax.task_template_file: %w", err)
	}
	sum := sha256.Sum256(raw)
	key := strings.Join([]string{c.Server, c.Router, c.Atespace, c.TaskTemplateFile, hex.EncodeToString(sum[:]), ready.String()}, "|")
	cl, err := b.axCache().get(key, func() (axClient, error) {
		// From the bytes the key hashes, not a second read (caught in
		// review: a file changed between the two was cached under the old
		// hash).
		t, err := ax.ParseTemplate(raw, c.TaskTemplateFile)
		if err != nil {
			return axClient{}, err
		}
		client, err := ax.New(ax.Config{
			Server: c.Server, Router: c.Router, Atespace: c.Atespace, Template: t, ReadyTimeout: ready,
			// No redirects through the router either (daemon.NoRedirects).
			HTTPClient: &http.Client{CheckRedirect: daemon.NoRedirects},
		})
		return axClient{client: client, idle: t.Idle, idleAfter: t.IdleAfter}, err
	})
	if err != nil {
		return nil, fmt.Errorf(".ax: %w", err)
	}
	linger := defaultStreamLinger
	if c.StreamLinger != "" {
		d, err := time.ParseDuration(c.StreamLinger)
		if err != nil || d < minStreamLinger {
			return nil, fmt.Errorf(".ax.stream_linger %q: want a duration of at least %s, like 1m", c.StreamLinger, minStreamLinger)
		}
		linger = d
	}
	// A linger past suspendAfter only delays suspension (ax's idle clock
	// starts when the stream closes), so it is a startup warning, not an
	// error (caught in review: the check refused valid templates).
	return &axAgent{client: cl.client, token: token, scope: scope, approvals: b.approvals, idle: cl.idle, idleAfter: cl.idleAfter, linger: linger}, nil
}

// axCache is the builder's client cache, created on first use.
func (b agentBuilder) axCache() *axClients {
	if b.ax != nil {
		return b.ax
	}
	return &axClients{} // a builder without one caches nothing
}

// --- in the router -------------------------------------------------------------

// maxReadyTimeout bounds ax.ready_timeout.
const maxReadyTimeout = 10 * time.Minute

// errTaskRestarted is a conversation whose task was gone and was started
// afresh, so its session is gone: what a turn waiting on the revive gets.
var errTaskRestarted = errors.New("this conversation's task was restarted; send the message again")

// errNeedsTask is an ax agent asked for a daemon with no task named: a path
// that addresses an agent rather than a conversation, such as an ingress
// adoption, which ax agents do not support.
var errNeedsTask = errors.New("this agent runs a task per conversation, and this operation names none")

// needsTask refuses every call, like goneDaemon.
type needsTask struct{}

func (needsTask) CreateSession(context.Context, string) (daemon.Session, error) {
	return daemon.Session{}, errNeedsTask
}
func (needsTask) Inject(context.Context, daemon.Session, string, string) error { return errNeedsTask }
func (needsTask) Subscribe(context.Context, daemon.Session, string, int64, func(daemon.Event) error) error {
	return errNeedsTask
}
func (needsTask) HeadSeq(context.Context, daemon.Session, string) (int64, error) {
	return 0, errNeedsTask
}

// setGateway names this gateway in its tasks' names, so two gateways in one
// cluster never share a conversation's task. Called once at startup.
func (r *Router) setGateway(id string) { r.gatewayID = id }

// startAXTask makes sure e's task is running and answers before anything is
// sent to it. A start that takes more than a moment says so in the thread:
// waking a task suspended for days can take a while, and silence would read
// as the app ignoring the message.
func (r *Router) startAXTask(ctx context.Context, conv string, a *agent, e *sessionEntry) (created bool, err error) {
	done := make(chan struct{})
	var notice chat.MessageRef
	var noticed sync.WaitGroup
	noticed.Add(1)
	go func() {
		defer noticed.Done()
		select {
		case <-done:
			return
		case <-time.After(axStartNoticeAfter):
		}
		pctx, cancel := platformContext(ctx)
		defer cancel()
		ref, err := r.out.Send(pctx, chat.Reply{Conversation: conv, Text: "⏳ Starting " + a.label() + "…", Kind: chat.KindProgress})
		if err == nil {
			notice = ref
		}
	}()
	start := time.Now()
	got, err := a.ax.client.Ensure(ctx, e.task)
	close(done)
	created = got.Created
	noticed.Wait()
	if notice.ID != "" {
		pctx, cancel := platformContext(ctx)
		r.retireMessage(pctx, conv, notice, start, err == nil)
		cancel()
	}
	if err != nil {
		r.logf.Warnf("session %s: start task %s for agent %s: %v", conv, e.task, a.name, err)
		return created, fmt.Errorf("%s could not be started: %w", a.label(), err)
	}
	if got.Drifted {
		// Tasks are immutable: one created before the template changed keeps
		// the old spec — an old token, say, which fails every request. Said,
		// so an operator can delete the task; replacing it is #173's.
		r.logf.Warnf("session %s: task %s was created from an earlier version of agent %s's template; it keeps the old spec until it is deleted", conv, e.task, a.name)
	}
	r.logf.Infof("session %s: task %s ready for agent %s in %s", conv, e.task, a.name, time.Since(start).Round(time.Millisecond))
	return created, nil
}

// personalToSomeoneElse reports whether e is an owner-scope conversation and
// caller is not its owner.
func (r *Router) personalToSomeoneElse(e *sessionEntry, caller string) bool {
	a, ok := r.agents().lookup(e.agent)
	return ok && a.ax != nil && a.ax.scope == axScopeOwner && e.owner != "" && caller != e.owner
}

// axStartNoticeAfter is how long a task start runs before the thread is told.
var axStartNoticeAfter = 2 * time.Second
