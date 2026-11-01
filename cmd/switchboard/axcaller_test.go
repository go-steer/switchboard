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
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/switchboard/pkg/chat"
)

// ax agents assert no caller (#186). mast in a task authenticates one bearer
// token, with no table allowing a proxy, and refuses X-Asserted-Caller rather
// than ignoring it: v0.9.0 sent it, and every session failed to open with
// "caller is not permitted to proxy". The ax fixture's router refuses the
// header as mast does, so every ax test is a regression test; these say so
// outright.

// A thread's session, turn and stream reach mast with no asserted caller, and
// switchboard's own audit still names the person.
func TestAnAXAgentAssertsNoCaller(t *testing.T) {
	f := newAXFixture(t)
	al, buf := testAudit()
	f.r.setAudit(al)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	f.say(t, ctx, "C1:1", "hello")
	waitFor(t, func() bool { return len(f.router.callsMatching("/events")) > 0 }, "the event stream opened")
	for _, c := range []string{"POST /sessions", "/inject", "/events"} {
		if len(f.router.callsMatching(c)) == 0 {
			t.Errorf("router never served %s", c)
		}
	}
	f.router.mu.Lock()
	asserted := append([]string(nil), f.router.asserted...)
	f.router.mu.Unlock()
	if len(asserted) != 0 {
		t.Errorf("requests carried X-Asserted-Caller: %v", asserted)
	}
	var injected bool
	for _, rec := range records(t, buf) {
		if rec.Kind == "turn" && rec.Caller == "u@x.com" {
			injected = true
		}
	}
	if !injected {
		t.Errorf("the audit log does not name the caller: %s", buf.String())
	}
}

// assert_caller: true puts the header back, which is what mast refuses: the
// override reaches the wire.
func TestAssertCallerTrueSendsTheHeader(t *testing.T) {
	f := newAXFixture(t)
	yes := true
	f.cfg.AssertCaller = &yes
	set, err := buildAgents([]AgentConfig{f.cfg}, "triage", agentBuilder{ax: &axClients{}})
	if err != nil {
		t.Fatal(err)
	}
	f.r.setAgents(set)
	err = f.r.Handle(context.Background(), chat.Message{Conversation: "C1:1", Channel: "C1", Caller: "u@x.com", Text: "hi"})
	if err == nil || !strings.Contains(err.Error(), "not permitted to proxy") {
		t.Errorf("err = %v, want mast's refusal", err)
	}
}

// The default: false for kind "ax", true for every other kind, and the entry's
// own value when it gives one.
func TestAssertCallerDefaults(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		ac   AgentConfig
		want bool
	}{
		{AgentConfig{Kind: agentKindAX}, false},
		{AgentConfig{Kind: agentKindAX, AssertCaller: &yes}, true},
		{AgentConfig{}, true},
		{AgentConfig{Kind: "mast"}, true},
		{AgentConfig{Kind: "core-agent", AssertCaller: &no}, false},
	} {
		if got := assertsCaller(tc.ac); got != tc.want {
			t.Errorf("kind %q assert_caller %v: got %v, want %v", tc.ac.Kind, tc.ac.AssertCaller, got, tc.want)
		}
	}
	// A daemon agent told not to assert sends no header either.
	d := newPermsDaemon(t)
	d.rejectAsserted.Store(true)
	t.Setenv("TOK_D", "tok")
	a, err := buildAgent(AgentConfig{Name: "d", DaemonURL: d.srv.URL, TokenEnv: "TOK_D", AssertCaller: &no}, agentBuilder{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.daemon.CreateSession(context.Background(), "u@x.com"); err != nil {
		t.Errorf("CreateSession: %v", err)
	}
	if got := d.assertedSeen(); len(got) != 0 {
		t.Errorf("asserted %v", got)
	}
	b, err := buildAgent(AgentConfig{Name: "e", DaemonURL: d.srv.URL, TokenEnv: "TOK_D"}, agentBuilder{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.daemon.CreateSession(context.Background(), "u@x.com"); err == nil {
		t.Error("a daemon agent stopped asserting by default")
	}
}

// axPermsRouter is a gateway whose one agent, "triage", is an ax agent whose
// router is d, with prompts relayed and alice the only approver.
func axPermsRouter(t *testing.T, d *permsDaemon, assertCaller *bool) (*Router, *fakeSender, *syncBuffer) {
	t.Helper()
	d.caps = `{"protocol_version":"1","server":"mast","features":{"perms_stream":true}}`
	t.Setenv("SWITCHBOARD_AGENT_TOKEN_TRIAGE", "t0ken")
	srv := newAXServer(t)
	tmpl := filepath.Join(t.TempDir(), "triage.yaml")
	if err := os.WriteFile(tmpl, []byte(axTemplate), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := AgentConfig{
		Name: "triage", Kind: agentKindAX, TokenEnv: "SWITCHBOARD_AGENT_TOKEN_TRIAGE", AssertCaller: assertCaller,
		AX: &AXConfig{Server: srv.listener, Router: d.srv.URL, Atespace: "agents", TaskTemplateFile: tmpl, ReadyTimeout: "5s"},
	}
	set, err := buildAgents([]AgentConfig{cfg}, "triage", agentBuilder{approvals: true, ax: &axClients{}})
	if err != nil {
		t.Fatal(err)
	}
	out := &fakeSender{replies: make(chan chat.Reply, 64)}
	r := NewRouter(nil, out, ProgressOff, nil, nil)
	r.setAgents(set)
	r.setGateway("slack")
	r.defaults.approvals = true
	r.setApprovers(mustApprovers(t, "alice@x.com"))
	al, buf := testAudit()
	r.setAudit(al)
	return r, out, buf
}

// askAndPress runs a turn as alice until the agent's question lands, then
// presses it as mallory (not an approver) and as alice.
func askAndPress(t *testing.T, ctx context.Context, r *Router, out *fakeSender, d *permsDaemon) {
	t.Helper()
	if err := r.Handle(ctx, chat.Message{Conversation: "C1:1", Channel: "C1", Caller: "alice@x.com", Text: "clean up"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	deadline := time.After(5 * time.Second)
	for asked := false; !asked; {
		select {
		case rep := <-out.replies:
			asked = rep.Kind == chat.KindDecision
		case <-deadline:
			t.Fatalf("the question never reached the thread; refused %v", d.assertedSeen())
		}
	}
	for _, who := range []string{"mallory@x.com", "alice@x.com"} {
		if err := r.HandlePress(ctx, chat.Press{
			Conversation: "C1:1", Channel: "C1", Caller: who,
			DecisionID: agentDecisionRef("triage", testSession, "pr1"),
			Option:     "allow-once",
		}); err != nil {
			t.Fatalf("HandlePress by %s: %v", who, err)
		}
	}
}

// Approvals on an ax agent: the prompt stream and the answer go out with no
// asserted caller, and who may answer is still switchboard's decision, made
// before anything is sent.
func TestAnAXAgentsApprovalsAssertNoCaller(t *testing.T) {
	d := newPermsDaemon(t, bashPrompt)
	d.rejectAsserted.Store(true) // mast with one token
	r, out, buf := axPermsRouter(t, d, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	askAndPress(t, ctx, r, out, d)

	posts := d.posts()
	if len(posts) != 1 || posts[0].caller != "" {
		t.Fatalf("posts = %+v, want alice's answer alone, with no asserted caller (mallory is not an approver)", posts)
	}
	if got := d.assertedSeen(); len(got) != 0 {
		t.Errorf("requests carried X-Asserted-Caller: %v", got)
	}
	var applied, refused bool
	for _, rec := range records(t, buf) {
		switch {
		case rec.Outcome == auditApplied && rec.Caller == "alice@x.com":
			applied = true
		case rec.Outcome == auditNotApprover && rec.Caller == "mallory@x.com":
			refused = true
		}
	}
	if !applied || !refused {
		t.Errorf("audit: applied by alice %v, refused mallory %v:\n%s", applied, refused, buf.String())
	}
	// The thread names the presser switchboard let through, not whatever the
	// agent recorded against its token (the fake says presser@example.com).
	var said []string
	for drained := false; !drained; {
		select {
		case rep := <-out.replies:
			said = append(said, rep.Text)
		default:
			drained = true
		}
	}
	var named bool
	for _, text := range said {
		if strings.Contains(text, "presser@example.com") {
			t.Errorf("the thread names the agent's recorded approver: %q", text)
		}
		named = named || (strings.Contains(text, "Allowed") && strings.Contains(text, "alice@x.com"))
	}
	if !named {
		t.Errorf("nothing in the thread names alice as the approver: %q", said)
	}
}

// An agent told no caller cannot refuse a presser by its session ACL, so a
// 404 to a fellow approver's answer means the question is settled, not
// "maybe refused".
func TestAnAXAgentsNotFoundIsSettled(t *testing.T) {
	d := newPermsDaemon(t, bashPrompt)
	d.rejectAsserted.Store(true)
	d.respondStatus = http.StatusNotFound
	r, out, buf := axPermsRouter(t, d, nil)
	r.setApprovers(mustApprovers(t, "alice@x.com,bob@x.com"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Handle(ctx, chat.Message{Conversation: "C1:1", Channel: "C1", Caller: "alice@x.com", Text: "clean up"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	deadline := time.After(5 * time.Second)
	for asked := false; !asked; {
		select {
		case rep := <-out.replies:
			asked = rep.Kind == chat.KindDecision
		case <-deadline:
			t.Fatal("the question never reached the thread")
		}
	}
	if err := r.HandlePress(ctx, chat.Press{Conversation: "C1:1", Channel: "C1", Caller: "bob@x.com",
		DecisionID: agentDecisionRef("triage", testSession, "pr1"), Option: "allow-once"}); err != nil {
		t.Fatalf("HandlePress: %v", err)
	}
	var outcome string
	for _, rec := range records(t, buf) {
		if rec.Kind == "press" {
			outcome = rec.Outcome
		}
	}
	if outcome != auditSettledElsewhere {
		t.Errorf("press outcome = %q, want %q", outcome, auditSettledElsewhere)
	}
}

// assert_caller: true on an ax agent — core-agent in the task, with
// proxy_identities — names the chat user on every path: session create,
// inject, the event stream, the prompt stream and the answer.
func TestAnAXAgentWithAssertCallerNamesTheUserEverywhere(t *testing.T) {
	d := newPermsDaemon(t, bashPrompt)
	yes := true
	r, out, _ := axPermsRouter(t, d, &yes)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	askAndPress(t, ctx, r, out, d)

	seen := d.callersSeen()
	for _, path := range []string{"POST /sessions ", "/inject ", "/events ", "/perms/stream ", "/perms/respond "} {
		var found bool
		for _, s := range seen {
			if strings.Contains(s, path) {
				found = true
				if !strings.HasSuffix(s, " alice@x.com") {
					t.Errorf("%q: want alice asserted", s)
				}
			}
		}
		if !found {
			t.Errorf("no request matching %q in %v", path, seen)
		}
	}
	if posts := d.posts(); len(posts) != 1 || posts[0].caller != "alice@x.com" {
		t.Errorf("posts = %+v, want alice's answer, asserted", posts)
	}
}

// assert_caller round-trips through the admin API: a runtime agent can set
// it, and the agent view shows it.
func TestAssertCallerThroughTheAdminAPI(t *testing.T) {
	f := newAdminFixture(t)
	body := `{"daemon_url":"` + f.c.url + `","token_env":"TOK_REG","assert_caller":false}`
	if got, resp := f.do(t, admin, "PUT", "/v1/agents/c", body); got != http.StatusOK {
		t.Fatalf("PUT: %d %s", got, resp)
	}
	_, resp := f.do(t, admin, "GET", "/v1/agents/c", "")
	if !strings.Contains(resp, `"assert_caller":false`) {
		t.Errorf("agent view = %s, want assert_caller shown", resp)
	}
	if a, ok := f.r.agents().lookup("c"); !ok || !a.omitCaller {
		t.Errorf("the runtime agent asserts a caller despite assert_caller false")
	}
	if got, resp := f.do(t, admin, "PUT", "/v1/agents/c", `{"daemon_url":"`+f.c.url+`","token_env":"TOK_REG","assert_caller":"no"}`); got != http.StatusBadRequest {
		t.Errorf("a non-boolean assert_caller: %d %s", got, resp)
	}
}
