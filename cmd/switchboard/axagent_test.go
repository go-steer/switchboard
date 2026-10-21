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
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/go-steer/switchboard/internal/axapi"
	"github.com/go-steer/switchboard/pkg/ax"
	"github.com/go-steer/switchboard/pkg/chat"
)

// axServer is ax-server as far as switchboard uses it, on a real port: tasks
// by name, a second create refused, resume and get.
type axServer struct {
	axapi.UnimplementedAXServer
	mu       sync.Mutex
	tasks    map[string]bool
	creates  []string
	failAll  bool // every create and get fails: ax-server in trouble
	listener string
}

func (s *axServer) CreateTask(_ context.Context, r *axapi.CreateTaskRequest) (*axapi.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := r.Task.Metadata.Name
	s.creates = append(s.creates, name)
	if s.failAll {
		return nil, status.Error(codes.Unavailable, "ax-server is down")
	}
	if s.tasks[name] {
		return nil, status.Error(codes.FailedPrecondition, "already exists and is immutable")
	}
	s.tasks[name] = true
	return r.Task, nil
}

func (s *axServer) GetTask(_ context.Context, r *axapi.GetTaskRequest) (*axapi.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.failAll && s.tasks[r.Name] {
		return &axapi.Task{Metadata: &axapi.ObjectMeta{Name: r.Name}}, nil
	}
	return nil, status.Error(codes.NotFound, "no such task")
}

func (s *axServer) ResumeTask(_ context.Context, r *axapi.ResumeTaskRequest) (*axapi.Task, error) {
	return &axapi.Task{Metadata: &axapi.ObjectMeta{Name: r.Name}}, nil
}

func (s *axServer) createCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.creates)
}

func newAXServer(t *testing.T) *axServer {
	t.Helper()
	s := &axServer{tasks: map[string]bool{}}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	axapi.RegisterAXServer(srv, s)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	s.listener = lis.Addr().String()
	return s
}

// axRouter is the atenet router in front of tasks that each serve the
// session contract: it records which actor every request was for.
type axRouter struct {
	mu       sync.Mutex
	calls    []string // "actor METHOD path"
	bearer   []string
	notReady atomic.Int64
	srv      *httptest.Server
}

func newAXRouter(t *testing.T) *axRouter {
	t.Helper()
	r := &axRouter{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		actor := req.Header.Get(ax.ActorHeader)
		r.mu.Lock()
		r.calls = append(r.calls, actor+" "+req.Method+" "+req.URL.Path)
		if req.URL.Path != "/readyz" {
			r.bearer = append(r.bearer, req.Header.Get("Authorization"))
		}
		r.mu.Unlock()
		switch {
		case req.URL.Path == "/readyz":
			if r.notReady.Add(-1) >= 0 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
		case req.Method == http.MethodPost && req.URL.Path == "/sessions":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"app":"mast","sessionID":"s-%s"}`, strings.ReplaceAll(actor, "/", "-"))
		case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/inject"):
			fmt.Fprint(w, `{"injected":"ok"}`)
		case strings.HasSuffix(req.URL.Path, "/events"):
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-req.Context().Done()
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *axRouter) callsMatching(sub string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, c := range r.calls {
		if strings.Contains(c, sub) {
			out = append(out, c)
		}
	}
	return out
}

const axTemplate = `kind: Task
spec:
  image: agent-mast
  env:
    - name: MAST_ATTACH_TOKEN
      value: t0ken
  http:
    port: 8484
  idle:
    suspendAfter: 10m
`

// axFixture is a gateway whose one agent, "triage", is an ax agent.
type axFixture struct {
	r      *Router
	out    *fakeSender
	server *axServer
	router *axRouter
	cfg    AgentConfig
}

func newAXFixture(t *testing.T) *axFixture {
	t.Helper()
	t.Setenv("SWITCHBOARD_AGENT_TOKEN_TRIAGE", "t0ken")
	f := &axFixture{server: newAXServer(t), router: newAXRouter(t), out: &fakeSender{replies: make(chan chat.Reply, 16)}}
	tmpl := filepath.Join(t.TempDir(), "triage.yaml")
	if err := os.WriteFile(tmpl, []byte(axTemplate), 0o600); err != nil {
		t.Fatal(err)
	}
	f.cfg = AgentConfig{
		Name: "triage", DisplayName: "SRE triage", Kind: agentKindAX, TokenEnv: "SWITCHBOARD_AGENT_TOKEN_TRIAGE",
		AX: &AXConfig{Server: f.server.listener, Router: f.router.srv.URL, Atespace: "agents", TaskTemplateFile: tmpl, ReadyTimeout: "5s"},
	}
	set, err := buildAgents([]AgentConfig{f.cfg}, "triage", agentBuilder{ax: &axClients{}})
	if err != nil {
		t.Fatal(err)
	}
	f.r = NewRouter(nil, f.out, ProgressOff, nil, nil)
	f.r.setAgents(set)
	f.r.setGateway("slack")
	return f
}

func (f *axFixture) say(t *testing.T, ctx context.Context, conv, text string) {
	t.Helper()
	if err := f.r.Handle(ctx, chat.Message{Conversation: conv, Channel: "C1", Caller: "u@x.com", Text: text}); err != nil {
		t.Fatalf("Handle %s: %v", conv, err)
	}
}

// A thread starts its own task, and the session lives in it: every request
// reaches the task through the router with the agent's bearer, and a second
// message reuses the task without asking ax again.
func TestAThreadStartsItsTask(t *testing.T) {
	f := newAXFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	f.say(t, ctx, "C1:1", "hello")
	f.say(t, ctx, "C1:1", "again")

	task := ax.TaskName("sb-triage", "slack", "C1:1")
	if f.server.createCount() != 1 || f.server.creates[0] != task {
		t.Errorf("creates = %v, want one, %s", f.server.creates, task)
	}
	actor := "agents/" + task
	waitFor(t, func() bool { return len(f.router.callsMatching(actor+" POST /sessions/")) == 2 }, "two injects into the task")
	if got := f.router.callsMatching(actor + " POST /sessions"); len(got) < 3 {
		t.Errorf("router calls for the task = %v", got)
	}
	f.router.mu.Lock()
	for _, b := range f.router.bearer {
		if b != "Bearer t0ken" {
			t.Errorf("bearer = %q", b)
		}
	}
	f.router.mu.Unlock()
}

// Two threads, two tasks.
func TestEachThreadHasItsOwnTask(t *testing.T) {
	f := newAXFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.say(t, ctx, "C1:1", "one")
	f.say(t, ctx, "C1:2", "two")
	if n := f.server.createCount(); n != 2 || f.server.creates[0] == f.server.creates[1] {
		t.Errorf("creates = %v, want two different tasks", f.server.creates)
	}
}

// After a restart the thread goes back to its task — not at boot, which would
// wake every task, but at its next message.
func TestARestartedThreadReturnsToItsTask(t *testing.T) {
	f := newAXFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.say(t, ctx, "C1:1", "hello")
	st := f.r.snapshot()
	if st.Sessions["C1:1"].Task == "" {
		t.Fatalf("record = %+v, want the task in it", st.Sessions["C1:1"])
	}

	g := newAXFixtureSharing(t, f)
	before := len(f.router.callsMatching(""))
	if revived, _ := g.r.restore(ctx, st, time.Now()); revived != 0 {
		t.Errorf("revived %d at boot, want none: an ax thread waits for its next message", revived)
	}
	if after := len(f.router.callsMatching("")); after != before {
		t.Errorf("the router was called at boot: %v", f.router.callsMatching("")[before:])
	}
	g.say(t, ctx, "C1:1", "back again")
	actor := "agents/" + ax.TaskName("sb-triage", "slack", "C1:1")
	waitFor(t, func() bool { return len(f.router.callsMatching(actor+" POST /sessions/")) == 2 }, "the second inject into the same task")
	if posts := f.router.callsMatching(actor + " POST /sessions"); len(posts) != 3 {
		// one create, two injects: the restored session was reused
		t.Errorf("calls = %v", posts)
	}
}

// newAXFixtureSharing is a second gateway process in front of the same ax.
func newAXFixtureSharing(t *testing.T, f *axFixture) *axFixture {
	t.Helper()
	set, err := buildAgents([]AgentConfig{f.cfg}, "triage", agentBuilder{ax: &axClients{}})
	if err != nil {
		t.Fatal(err)
	}
	g := &axFixture{server: f.server, router: f.router, cfg: f.cfg, out: &fakeSender{replies: make(chan chat.Reply, 16)}}
	g.r = NewRouter(nil, g.out, ProgressOff, nil, nil)
	g.r.setAgents(set)
	g.r.setGateway("slack")
	return g
}

// A task that will not start fails the turn with a reason, and nothing is
// sent to it.
func TestATaskThatWillNotStartFailsTheTurn(t *testing.T) {
	f := newAXFixture(t)
	f.server.failAll = true
	err := f.r.Handle(context.Background(), chat.Message{Conversation: "C1:1", Channel: "C1", Caller: "u@x.com", Text: "hi"})
	if err == nil || !strings.Contains(err.Error(), "could not be started") {
		t.Errorf("err = %v", err)
	}
	if got := f.router.callsMatching("/sessions"); len(got) != 0 {
		t.Errorf("sent to a task that never started: %v", got)
	}
}

// A slow start says so in the thread, and the line is finalised once the
// task answers.
func TestASlowStartIsShown(t *testing.T) {
	f := newAXFixture(t)
	old := axStartNoticeAfter
	axStartNoticeAfter = 20 * time.Millisecond
	defer func() { axStartNoticeAfter = old }()
	f.router.notReady.Store(1) // one 503, then ready: a poll later
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.say(t, ctx, "C1:1", "hello")

	got := recvReply(t, f.out.replies)
	if got.Kind != chat.KindProgress || !strings.Contains(got.Text, "Starting SRE triage") {
		t.Errorf("first post = %+v, want the starting line", got)
	}
	waitFor(t, func() bool { return len(f.out.deletedCalls()) > 0 || len(f.out.updatedCalls()) > 0 }, "the starting line to be retired")
}

func TestAXAgentEntriesAreValidated(t *testing.T) {
	f := newAXFixture(t)
	base := f.cfg
	for name, mutate := range map[string]func(*AgentConfig){
		"no ax block":         func(c *AgentConfig) { c.AX = nil },
		"a daemon_url too":    func(c *AgentConfig) { c.DaemonURL = "http://x:1" },
		"an auth block":       func(c *AgentConfig) { c.Auth = &AgentAuth{Method: authPKI} },
		"no token_env":        func(c *AgentConfig) { c.TokenEnv = "" },
		"an unknown scope":    func(c *AgentConfig) { ax := *c.AX; ax.Scope = "team"; c.AX = &ax },
		"a bad timeout":       func(c *AgentConfig) { ax := *c.AX; ax.ReadyTimeout = "soon"; c.AX = &ax },
		"a long timeout":      func(c *AgentConfig) { ax := *c.AX; ax.ReadyTimeout = "11m"; c.AX = &ax },
		"no template":         func(c *AgentConfig) { ax := *c.AX; ax.TaskTemplateFile = "/nope.yaml"; c.AX = &ax },
		"an ax block on mast": func(c *AgentConfig) { c.Kind = "mast"; c.DaemonURL = "http://x:1" },
	} {
		c := base
		mutate(&c)
		if _, err := buildAgent(c, agentBuilder{ax: &axClients{}}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Rebuilding an ax agent — the registry does on every change — reuses its
// connection to ax-server rather than opening another.
func TestAnAXAgentsClientIsReusedAcrossRebuilds(t *testing.T) {
	f := newAXFixture(t)
	b := agentBuilder{ax: &axClients{}}
	a1, err := buildAgent(f.cfg, b)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := buildAgent(f.cfg, b)
	if err != nil {
		t.Fatal(err)
	}
	if a1.ax.client != a2.ax.client {
		t.Error("a second ax-server client was built for the same agent")
	}
}

// Paths that address an agent rather than a conversation get a refusal from
// an ax agent, not a nil daemon.
func TestAnAXAgentHasNoDaemonOfItsOwn(t *testing.T) {
	f := newAXFixture(t)
	if _, err := f.r.daemonFor("triage").CreateSession(context.Background(), "u"); err != errNeedsTask {
		t.Errorf("err = %v", err)
	}
}

// A runtime ax agent's task template must be in --admin-ax-template-dir: the
// gateway reads the file, and an error can quote it, so an admin must not be
// able to aim it at a mounted secret.
func TestARuntimeAXAgentsTemplateIsFenced(t *testing.T) {
	f := newAXFixture(t)
	base, err := buildAgents([]AgentConfig{{Name: "a", DaemonURL: "http://a:1", TokenEnv: "SWITCHBOARD_AGENT_TOKEN_TRIAGE"}}, "a", agentBuilder{ax: &axClients{}})
	if err != nil {
		t.Fatal(err)
	}
	r := NewRouter(nil, f.out, ProgressOff, nil, nil)
	r.setAgents(base)
	admin := newRegistryAdmin(r, registryConfig{Base: base, TokenPrefix: "SWITCHBOARD_AGENT_TOKEN_", Builder: agentBuilder{ax: &axClients{}}})

	if err := admin.putAgent(f.cfg); !errors.Is(err, errRejected) || !strings.Contains(err.Error(), "--admin-ax-template-dir") {
		t.Errorf("no template dir: err = %v", err)
	}
	admin.axTemplateDir = t.TempDir() // not where the fixture's template is
	if err := admin.putAgent(f.cfg); !errors.Is(err, errRejected) || !strings.Contains(err.Error(), "must be in") {
		t.Errorf("a template outside the dir: err = %v", err)
	}
	escape := f.cfg
	axc := *f.cfg.AX
	axc.TaskTemplateFile = filepath.Join(admin.axTemplateDir, "..", filepath.Base(f.cfg.AX.TaskTemplateFile))
	escape.AX = &axc
	if err := admin.putAgent(escape); !errors.Is(err, errRejected) {
		t.Errorf("a template escaping the dir with ..: err = %v", err)
	}
	admin.axTemplateDir = filepath.Dir(f.cfg.AX.TaskTemplateFile)
	if err := admin.putAgent(f.cfg); err != nil {
		t.Errorf("a template inside the dir: %v", err)
	}
}

// An ax-only gateway counts its agents' approvals: their clients are their
// tasks', and the press gate refused every press (caught in review).
func TestAnAXOnlyGatewayTakesPresses(t *testing.T) {
	f := newAXFixture(t)
	set, err := buildAgents([]AgentConfig{f.cfg}, "triage", agentBuilder{approvals: true, ax: &axClients{}})
	if err != nil {
		t.Fatal(err)
	}
	if !set.anyApprovals() {
		t.Error("an ax agent with approvals is not counted")
	}
}

// While a dormant conversation's task wakes, the conversation stays in every
// snapshot (caught in review: a save in that window lost it).
func TestAWakingConversationStaysInTheSnapshot(t *testing.T) {
	f := newAXFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.say(t, ctx, "C1:1", "hello")
	st := f.r.snapshot()

	g := newAXFixtureSharing(t, f)
	g.r.restore(ctx, st, time.Now())
	f.router.notReady.Store(2) // two polls of a second each: a slow wake
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = g.r.Handle(ctx, chat.Message{Conversation: "C1:1", Channel: "C1", Caller: "u@x.com", Text: "again"})
	}()
	waitFor(t, func() bool { return len(f.router.callsMatching("/readyz")) >= 2 }, "the wake to begin")
	if _, ok := g.r.snapshot().Sessions["C1:1"]; !ok {
		t.Error("the conversation dropped out of the snapshot while its task woke")
	}
	<-done
}

// A conversation whose turn was in flight at the restart reattaches at boot,
// so its answer and placeholder are not left waiting for the next message
// (caught in review).
func TestAnInFlightAXConversationReattachesAtBoot(t *testing.T) {
	f := newAXFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.say(t, ctx, "C1:1", "hello")
	st := f.r.snapshot()
	rec := st.Sessions["C1:1"]
	rec.Placeholder = "ph-1"
	st.Sessions["C1:1"] = rec

	g := newAXFixtureSharing(t, f)
	if revived, _ := g.r.restore(ctx, st, time.Now()); revived != 1 {
		t.Errorf("revived %d, want the in-flight conversation", revived)
	}
}

// A task deleted while its conversation slept is started afresh, and the
// conversation starts over in it at once, the thread told — not left to lose
// the next message to a 404 (caught in review).
func TestARecreatedTaskStartsTheConversationOver(t *testing.T) {
	f := newAXFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.say(t, ctx, "C1:1", "hello")
	st := f.r.snapshot()

	f.server.mu.Lock()
	f.server.tasks = map[string]bool{} // deleted while dormant
	f.server.mu.Unlock()
	g := newAXFixtureSharing(t, f)
	g.r.restore(ctx, st, time.Now())
	g.say(t, ctx, "C1:1", "back")

	actor := "agents/" + ax.TaskName("sb-triage", "slack", "C1:1")
	if creates := f.router.callsMatching(actor + " POST /sessions"); len(creates) < 3 {
		t.Errorf("calls = %v, want a second session created", creates)
	}
	got := recvReply(t, g.out.replies)
	if !strings.Contains(got.Text, "starts over") {
		t.Errorf("notice = %q", got.Text)
	}
}

// A record of an ax agent with no task — the agent was redefined — opens a
// new session rather than a relay that can never connect (caught in review).
func TestATasklessRecordOfAnAXAgentStartsOver(t *testing.T) {
	f := newAXFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := routerState{Version: stateVersion, Sessions: map[string]sessionRecord{
		"C1:1": {Channel: "C1", Session: "core-agent/old", Agent: "triage", Touched: time.Now()},
	}}
	f.r.restore(ctx, st, time.Now())
	f.say(t, ctx, "C1:1", "hi")
	if f.server.createCount() != 1 {
		t.Errorf("creates = %d, want the conversation's task started", f.server.createCount())
	}
}

// Binding a session to the ax default agent is refused as such, not as a
// backend that would not answer (caught in review).
func TestBindingToAnAXAgentIsAClearConflict(t *testing.T) {
	ie := bindError(testSession, errNeedsTask)
	if ie.status != http.StatusConflict || !strings.Contains(ie.msg, "task per conversation") {
		t.Errorf("bindError = %d %q", ie.status, ie.msg)
	}
}
