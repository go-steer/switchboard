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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-steer/switchboard/pkg/chat"
	"github.com/go-steer/switchboard/pkg/daemon"
)

// recordingDaemon is a fake daemon that counts what reached it: sessions it
// created and turns injected into them. Its stream stays open and quiet.
type recordingDaemon struct {
	creates, injects atomic.Int64
	client           *daemon.Client
}

func newRecordingDaemon(t *testing.T, sid string) *recordingDaemon {
	t.Helper()
	d := &recordingDaemon{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, r *http.Request) {
		d.creates.Add(1)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"app":"core-agent","sessionID":%q}`, sid)
	})
	mux.HandleFunc("POST /sessions/{app}/{sid}/inject", func(w http.ResponseWriter, r *http.Request) {
		d.injects.Add(1)
		fmt.Fprintf(w, `{"injected":"ok","session":%q}`, sid)
	})
	mux.HandleFunc("GET /sessions/{app}/{sid}/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, err := daemon.New(daemon.Config{BaseURL: srv.URL, BearerToken: "tok", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	d.client = c
	return d
}

// twoAgentRouter registers agents "a" (the global default) and "b", with
// channel C2 defaulting to "b".
func twoAgentRouter(t *testing.T) (*Router, *recordingDaemon, *recordingDaemon, *fakeSender) {
	t.Helper()
	a, b := newRecordingDaemon(t, "sa"), newRecordingDaemon(t, "sb")
	set, err := newAgentSet("a", &agent{name: "a", daemon: a.client}, &agent{name: "b", daemon: b.client})
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	r := NewRouter(nil, fake, ProgressOff, nil, nil)
	r.setAgents(set)
	r.setChannels(map[string]channelSettings{"C2": {defaultAgent: "b"}})
	return r, a, b, fake
}

func waitCount(t *testing.T, n *atomic.Int64, want int64, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for n.Load() != want {
		if time.Now().After(deadline) {
			t.Fatalf("%s = %d, want %d", what, n.Load(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A conversation goes to its channel's default agent, else the global one,
// and every later turn in the thread stays with the agent it started on.
func TestAConversationGoesToItsChannelsAgentAndStays(t *testing.T) {
	r, a, b, _ := twoAgentRouter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := r.Handle(ctx, chat.Message{Conversation: "C1:1", Channel: "C1", Caller: "u@x.com", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	waitCount(t, &a.injects, 1, "agent a injects")

	for _, text := range []string{"one", "two"} {
		if err := r.Handle(ctx, chat.Message{Conversation: "C2:9", Channel: "C2", Caller: "u@x.com", Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	waitCount(t, &b.injects, 2, "agent b injects")
	if a.creates.Load() != 1 || b.creates.Load() != 1 {
		t.Errorf("creates a=%d b=%d, want one session each", a.creates.Load(), b.creates.Load())
	}
	if a.injects.Load() != 1 {
		t.Errorf("agent a injects = %d, want only C1's turn", a.injects.Load())
	}
}

// The agent is part of the record, so a restarted gateway reattaches the
// conversation to the daemon that holds its session; a record from before the
// registry names no agent and belongs to the default.
func TestARecordKeepsItsAgent(t *testing.T) {
	r, _, b, _ := twoAgentRouter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Handle(ctx, chat.Message{Conversation: "C2:9", Channel: "C2", Caller: "u@x.com", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	waitCount(t, &b.injects, 1, "agent b injects")

	rec := r.snapshot().Sessions["C2:9"]
	if rec.Agent != "b" {
		t.Fatalf("record agent = %q, want b", rec.Agent)
	}
	e, err := entryFromRecord(rec, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := r.daemonOf(e); got != daemonAPI(b.client) {
		t.Error("revived conversation does not use agent b's daemon")
	}
	legacy := rec
	legacy.Agent = ""
	if e, _ := entryFromRecord(legacy, ""); r.daemonOf(e) != daemonAPI(r.agents.byName["a"].daemon) {
		t.Error("a record with no agent did not resolve to the default agent")
	}
}

// A thread whose agent has been removed is told so, and nothing is routed to
// another agent behind its back.
func TestARemovedAgentGetsANoticeNotAReroute(t *testing.T) {
	r, a, b, fake := twoAgentRouter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := liveEntryIn(r, "C1:5", "C1")
	e.agent = "removed"

	err := r.Handle(ctx, chat.Message{Conversation: "C1:5", Channel: "C1", Caller: "u@x.com", Text: "still there?"})
	if !errors.Is(err, errAgentGone) {
		t.Fatalf("Handle = %v, want errAgentGone", err)
	}
	got := recvReply(t, fake.replies)
	if got.Kind != chat.KindNotice || !strings.Contains(got.Text, "(removed) is no longer available") {
		t.Errorf("notice = %q (%v), want the agent-gone notice", got.Text, got.Kind)
	}
	if a.injects.Load()+b.injects.Load()+a.creates.Load()+b.creates.Load() != 0 {
		t.Error("the turn was routed to another agent")
	}
}

// An agent no channel may use is refused before anything is created.
func TestAChannelsAllowListGatesTheDefault(t *testing.T) {
	r, a, b, _ := twoAgentRouter(t)
	r.setChannels(map[string]channelSettings{"C3": {agents: []string{"b"}}}) // global default "a" not allowed
	_, err := r.session(context.Background(), "C3:1", "C3", "u@x.com")
	if err == nil || !strings.Contains(err.Error(), "not in this channel's allowed agents") {
		t.Fatalf("session = %v, want the allow-list refusal", err)
	}
	if a.creates.Load()+b.creates.Load() != 0 {
		t.Error("a session was created despite the refusal")
	}
}

func TestDecisionRefsCarryTheAgent(t *testing.T) {
	sess := daemon.Session{App: "core-agent", ID: "s1"}
	ref := agentDecisionRef("platform", sess, "p1")
	if ref != "platform@core-agent/s1#p1" {
		t.Fatalf("agentDecisionRef = %q", ref)
	}
	agent, s, id, ok := splitAgentDecisionRef(ref)
	if !ok || agent != "platform" || s != "core-agent/s1" || id != "p1" {
		t.Errorf("split = %q %q %q %v", agent, s, id, ok)
	}
	// A button posted before the registry: no agent, still answerable.
	agent, s, id, ok = splitAgentDecisionRef(decisionRef(sess, "p1"))
	if !ok || agent != "" || s != "core-agent/s1" || id != "p1" {
		t.Errorf("legacy split = %q %q %q %v", agent, s, id, ok)
	}
	if s, id, ok := splitDecisionRef(ref); !ok || s != "core-agent/s1" || id != "p1" {
		t.Errorf("splitDecisionRef(agent ref) = %q %q %v", s, id, ok)
	}
}

func TestBuildAgentsRefusesBadEntries(t *testing.T) {
	t.Setenv("TOK_A", "a")
	ok := AgentConfig{Name: "a", DaemonURL: "http://a", TokenEnv: "TOK_A"}
	for name, tc := range map[string]struct {
		list []AgentConfig
		def  string
		want string
	}{
		"bad name":        {[]AgentConfig{{Name: "Platform Agent", DaemonURL: "http://a", TokenEnv: "TOK_A"}}, "", "short lowercase name"},
		"no url":          {[]AgentConfig{{Name: "a", TokenEnv: "TOK_A"}}, "", "daemon_url is required"},
		"no token env":    {[]AgentConfig{{Name: "a", DaemonURL: "http://a"}}, "", "token_env is required"},
		"unset token":     {[]AgentConfig{{Name: "a", DaemonURL: "http://a", TokenEnv: "TOK_UNSET"}}, "", "no daemon token in $TOK_UNSET"},
		"bad kind":        {[]AgentConfig{{Name: "a", DaemonURL: "http://a", TokenEnv: "TOK_A", Kind: "other"}}, "", "kind"},
		"duplicate":       {[]AgentConfig{ok, ok}, "", "registered twice"},
		"unknown default": {[]AgentConfig{ok}, "b", "default agent \"b\""},
	} {
		if _, err := buildAgents(tc.list, tc.def, false); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: buildAgents = %v, want an error containing %q", name, err, tc.want)
		}
	}
	set, err := buildAgents([]AgentConfig{ok, {Name: "b", DaemonURL: "http://b", TokenEnv: "TOK_A", Kind: "mast"}}, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if set.def != "a" || set.byName["b"].approvals == nil {
		t.Errorf("default = %q, b approvals = %v; want the first agent as default and approval clients built", set.def, set.byName["b"].approvals)
	}
}

func TestDefaultAgentNeedsAnAgentsList(t *testing.T) {
	def := "platform"
	if _, err := agentsFrom(&Config{DefaultAgent: &def}, "http://d", "TOK_X", false); err == nil ||
		!strings.Contains(err.Error(), "no agents list") {
		t.Errorf("agentsFrom = %v, want the missing-list refusal", err)
	}
}

func TestCheckChannelAgents(t *testing.T) {
	set, _ := newAgentSet("a", &agent{name: "a"}, &agent{name: "b"})
	for name, tc := range map[string]struct {
		cs   channelSettings
		want string
	}{
		"unknown in allow list": {channelSettings{agents: []string{"c"}}, `names "c"`},
		"unknown default":       {channelSettings{defaultAgent: "c"}, `default_agent "c"`},
		"default not allowed":   {channelSettings{agents: []string{"b"}}, "not in this channel's allowed agents"},
	} {
		if err := checkChannelAgents(set, "channels[\"C1\"]", tc.cs); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an error containing %q", name, err, tc.want)
		}
	}
	if err := checkChannelAgents(set, "defaults", channelSettings{defaultAgent: "b", agents: []string{"b"}}); err != nil {
		t.Errorf("a consistent channel was refused: %v", err)
	}
}

// A press whose button names another agent than the conversation's is a stale
// button, refused like one for a replaced session; a button that names none
// (posted before the registry) is still answered on the session alone.
func TestAPressForAnotherAgentIsNotAnswered(t *testing.T) {
	d := newPermsDaemon(t)
	r, fake, _ := permsRouter(t, d)
	liveEntry(r, "C1:1") // holds testSession, on the default agent

	err := r.HandlePress(context.Background(), chat.Press{
		Conversation: "C1:1", Caller: "presser@example.com",
		DecisionID: agentDecisionRef("other", testSession, "pr1"),
		Option:     "allow-once",
	})
	if err != nil {
		t.Fatalf("HandlePress: %v", err)
	}
	if posts := d.posts(); len(posts) != 0 {
		t.Fatalf("answered %v for a different agent's question", posts)
	}
	if got := recvReply(t, fake.replies); got.Text != noticeStalePress {
		t.Errorf("thread got %q, want the stale-question notice", got.Text)
	}

	// A gateway with no agents list names no agent on its buttons.
	if err := r.HandlePress(context.Background(), chat.Press{
		Conversation: "C1:1", Caller: "presser@example.com",
		DecisionID: agentDecisionRef(r.agents.stored(""), testSession, "pr1"),
		Option:     "allow-once",
	}); err != nil {
		t.Fatalf("HandlePress with the conversation's own agent: %v", err)
	}
	if posts := d.posts(); len(posts) != 1 {
		t.Errorf("posts = %v, want the press for the conversation's own agent answered", posts)
	}
}

// The agents list loads through the strict decoder, channel blocks can name
// a default and an allow list, and a token value written where the variable
// name belongs is refused by the secret scan like any other.
func TestConfigFileAgents(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	if err := os.WriteFile(good, []byte(`{
  "agents": [
    {"name": "platform", "display_name": "Platform agent", "daemon_url": "http://p:7777", "token_env": "TOK_P", "kind": "core-agent"},
    {"name": "mast", "daemon_url": "http://m:7777", "token_env": "TOK_M", "kind": "mast", "labels": {"team": "infra"}}
  ],
  "default_agent": "platform",
  "channels": {"C0123ABCD": {"default_agent": "mast", "agents": ["mast", "platform"]}}
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(good)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(cfg.Agents) != 2 || cfg.Agents[1].Labels["team"] != "infra" || *cfg.DefaultAgent != "platform" {
		t.Errorf("agents = %+v, default = %v", cfg.Agents, cfg.DefaultAgent)
	}
	cs := channelSettings{}
	applyAgentSettings(&cs, cfg.Channels["C0123ABCD"])
	if cs.defaultAgent != "mast" || len(cs.agents) != 2 {
		t.Errorf("channel agent settings = %+v", cs)
	}

	leak := filepath.Join(dir, "leak.json")
	if err := os.WriteFile(leak, []byte(`{"agents": [{"name": "p", "daemon_url": "http://p", "token_env": "xoxb-1-2-abcdef"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(leak); err == nil {
		t.Error("a token value in an agent's token_env was accepted")
	}
}

// A gateway with no agents list names no agent in its records or buttons, so a
// later move to an agents list keeps every such thread on the new default —
// rather than pinning them to a literal "default" no list will have (caught in
// review).
func TestMovingToAnAgentsListKeepsSingleDaemonThreads(t *testing.T) {
	d := newRecordingDaemon(t, "s1")
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	r := NewRouter(d.client, fake, ProgressOff, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Handle(ctx, chat.Message{Conversation: "C1:1", Channel: "C1", Caller: "u@x.com", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	waitCount(t, &d.injects, 1, "injects")
	rec := r.snapshot().Sessions["C1:1"]
	if rec.Agent != "" {
		t.Fatalf("single-daemon record names agent %q, want none", rec.Agent)
	}
	r.mu.Lock()
	live := r.sessions["C1:1"]
	r.mu.Unlock()
	if ref := agentDecisionRef(r.agents.stored(live.agent), live.sess, "p"); strings.Contains(ref, "@") {
		t.Errorf("single-daemon button id %q names an agent", ref)
	}

	// The operator registers the same daemon as "platform", the default.
	set, err := newAgentSet("platform", &agent{name: "platform", daemon: d.client}, &agent{name: "other"})
	if err != nil {
		t.Fatal(err)
	}
	r2 := NewRouter(nil, fake, ProgressOff, nil, nil)
	r2.setAgents(set)
	e, err := entryFromRecord(rec, "")
	if err != nil {
		t.Fatal(err)
	}
	if r2.daemonOf(e) != daemonAPI(d.client) {
		t.Error("a single-daemon thread did not follow the new default agent")
	}
}
