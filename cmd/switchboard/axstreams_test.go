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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-steer/switchboard/pkg/ax"
	"github.com/go-steer/switchboard/pkg/chat"
)

// streamRouter is the atenet router in front of tasks whose agent answers:
// every inject is followed, on the task's event stream, by an answer and the
// turn's end. It counts the streams open to it — what keeps a task awake.
type streamRouter struct {
	srv    *httptest.Server
	open   atomic.Int64
	silent atomic.Bool // injects get no answer: a turn that runs on

	mu      sync.Mutex
	seq     int64
	streams map[string]chan string // actor -> frames for its open stream
	sinces  []string               // the since of every stream opened
	history map[string][]seqFrame  // actor -> agent frames, replayed after since
}

// seqFrame is an agent frame and its seq, for replay.
type seqFrame struct {
	seq   int64
	frame string
}

const (
	frameStreaming = "event: status-update\ndata: {\"turn_state\":\"streaming\"}\n\n"
	frameIdle      = "event: status-update\ndata: {\"turn_state\":\"idle\"}\n\n"
)

// answer queues an agent answer to actor's stream, as the daemon would, and
// keeps it for a reconnect to replay.
func (r *streamRouter) answer(actor, text string) {
	r.mu.Lock()
	r.seq++
	seq := r.seq
	f := fmt.Sprintf("event: agent\ndata: {\"seq\":%d,\"event\":{\"Content\":{\"parts\":[{\"text\":%q}],\"role\":\"model\"},\"Partial\":false}}\n\n", seq, text)
	r.history[actor] = append(r.history[actor], seqFrame{seq, f})
	r.mu.Unlock()
	r.queue(actor) <- f
}

func newStreamRouter(t *testing.T) *streamRouter {
	t.Helper()
	r := &streamRouter{streams: map[string]chan string{}, history: map[string][]seqFrame{}}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		actor := req.Header.Get(ax.ActorHeader)
		switch {
		case req.URL.Path == "/readyz":
			w.WriteHeader(http.StatusOK)
		case req.Method == http.MethodPost && req.URL.Path == "/sessions":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"app":"mast","sessionID":"s1"}`)
		case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/inject"):
			fmt.Fprint(w, `{"injected":"ok"}`)
			if r.silent.Load() {
				return
			}
			// mast's pattern: streaming, the answer, the boundary, then the
			// post-turn state.
			r.queue(actor) <- frameStreaming
			r.mu.Lock()
			n := r.seq + 1
			r.mu.Unlock()
			r.answer(actor, fmt.Sprintf("answer %d", n))
			r.queue(actor) <- "event: turn-complete\ndata: {\"prompt_id\":\"p\",\"model\":\"m\",\"latency_ms\":5}\n\n"
			r.queue(actor) <- frameIdle
		case strings.HasSuffix(req.URL.Path, "/events"):
			ch := r.queue(actor)
			since := req.URL.Query().Get("since")
			r.mu.Lock()
			r.sinces = append(r.sinces, since)
			var replay []string
			for _, f := range r.history[actor] {
				if f.seq > atoi(since) {
					replay = append(replay, f.frame)
				}
			}
			r.mu.Unlock()
			r.open.Add(1)
			defer r.open.Add(-1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "event: capabilities\ndata: %s\n\n", capsWithBoundary)
			for _, f := range replay {
				fmt.Fprint(w, f)
			}
			w.(http.Flusher).Flush()
			for {
				select {
				case <-req.Context().Done():
					return
				case f := <-ch:
					if f == cutStream {
						return // the router's route timeout
					}
					fmt.Fprint(w, f)
					w.(http.Flusher).Flush()
				}
			}
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func atoi(s string) int64 {
	var n int64
	fmt.Sscan(s, &n)
	return n
}

// cutStream, queued to a stream, ends it as the atenet router does at its
// five-minute route timeout.
const cutStream = "\x00cut"

// queue is actor's frames, made by whichever side asks first: an inject can
// land before the relay's stream has connected, and its frames must wait.
func (r *streamRouter) queue(actor string) chan string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ch, ok := r.streams[actor]
	if !ok {
		ch = make(chan string, 16)
		r.streams[actor] = ch
	}
	return ch
}

// newStreamFixture is the ax fixture in front of a streamRouter.
func newStreamFixture(t *testing.T) (*axFixture, *streamRouter) {
	t.Helper()
	t.Setenv("SWITCHBOARD_AGENT_TOKEN_TRIAGE", "t0ken")
	sr := newStreamRouter(t)
	f := &axFixture{server: newAXServer(t), out: &fakeSender{replies: make(chan chat.Reply, 16)}}
	tmpl := filepath.Join(t.TempDir(), "triage.yaml")
	if err := os.WriteFile(tmpl, []byte(axTemplate), 0o600); err != nil {
		t.Fatal(err)
	}
	f.cfg = AgentConfig{
		Name: "triage", DisplayName: "SRE triage", Kind: agentKindAX, TokenEnv: "SWITCHBOARD_AGENT_TOKEN_TRIAGE",
		AX: &AXConfig{Server: f.server.listener, Router: sr.srv.URL, Atespace: "agents", TaskTemplateFile: tmpl},
	}
	set, err := buildAgents([]AgentConfig{f.cfg}, "triage", agentBuilder{ax: &axClients{}})
	if err != nil {
		t.Fatal(err)
	}
	f.r = NewRouter(nil, f.out, ProgressOff, nil, nil)
	f.r.setAgents(set)
	f.r.setGateway("slack")
	return f, sr
}

// thread collects what a fakeSender posts, so a test can wait for one text
// and still count every other.
type thread struct {
	mu   sync.Mutex
	seen map[string]int
}

func watch(out *fakeSender) *thread {
	th := &thread{seen: map[string]int{}}
	go func() {
		for r := range out.replies {
			th.mu.Lock()
			th.seen[r.Text]++
			th.mu.Unlock()
		}
	}()
	return th
}

func (th *thread) count(text string) int {
	th.mu.Lock()
	defer th.mu.Unlock()
	return th.seen[text]
}

// answered waits for the thread to receive text.
func (th *thread) answered(t *testing.T, text string) {
	t.Helper()
	waitFor(t, func() bool { return th.count(text) > 0 }, "the thread to receive "+text)
}

// Once its turn is over and its linger has passed, an ax conversation lets
// go of its streams, so the task can suspend; the next message picks the
// stream up where it left off, and nothing is delivered twice.
func TestAnIdleAXConversationLetsGoOfItsTask(t *testing.T) {
	f, sr := newStreamFixture(t)
	th := watch(f.out)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	f.say(t, ctx, "C1:1", "hello")
	th.answered(t, "answer 1")
	waitFor(t, func() bool {
		f.r.mu.Lock()
		e := f.r.sessions["C1:1"]
		f.r.mu.Unlock()
		return e != nil && !e.turnInFlight() && !e.awaitingAnswer()
	}, "the turn to end")
	if sr.open.Load() != 1 {
		t.Fatalf("open streams = %d, want 1 while the conversation is live", sr.open.Load())
	}
	if n := f.r.releaseIdleAX(time.Now().Add(30 * time.Second)); n != 0 {
		t.Errorf("released %d inside the linger", n)
	}
	if n := f.r.releaseIdleAX(time.Now().Add(2 * time.Minute)); n != 1 {
		t.Fatalf("released %d past the linger, want 1", n)
	}
	waitFor(t, func() bool { return sr.open.Load() == 0 }, "every stream to the task to close")
	if _, ok := f.r.snapshot().Sessions["C1:1"]; !ok {
		t.Error("the released conversation left no record")
	}

	f.say(t, ctx, "C1:1", "again")
	th.answered(t, "answer 2")
	sr.mu.Lock()
	defer sr.mu.Unlock()
	if len(sr.sinces) != 2 || sr.sinces[1] != "1" {
		t.Errorf("streams opened with since %v, want the second from the first answer", sr.sinces)
	}
	if n := th.count("answer 1"); n != 1 {
		t.Errorf("the first answer was delivered %d times", n)
	}
}

// A turn still running keeps its streams, however long it takes.
func TestARunningTurnKeepsItsStreams(t *testing.T) {
	f, sr := newStreamFixture(t)
	sr.silent.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.say(t, ctx, "C1:1", "a long one")
	waitFor(t, func() bool { return sr.open.Load() == 1 }, "the stream")
	if n := f.r.releaseIdleAX(time.Now().Add(time.Hour)); n != 0 {
		t.Errorf("released %d conversation(s) mid-turn", n)
	}
}

// Non-ax conversations are not the sweep's business, however idle: they
// keep their streams until --session-idle-ttl.
func TestTheAXSweepLeavesDaemonAgentsAlone(t *testing.T) {
	r, _, _, _ := twoAgentRouter(t)
	liveEntryIn(r, "C1:1", "C1") // between turns, untouched for ages
	if n := r.releaseIdleAX(time.Now().Add(24 * time.Hour)); n != 0 {
		t.Errorf("released %d daemon conversation(s)", n)
	}
}

// A linger as long as the template's idle timeout would keep every task
// awake, and is refused.
func TestAStreamLingerMustUndercutIdle(t *testing.T) {
	f := newAXFixture(t)
	for _, linger := range []string{"5s", "soon", "-1s"} {
		c := f.cfg
		axc := *c.AX
		axc.StreamLinger = linger
		c.AX = &axc
		if _, err := buildAgent(c, agentBuilder{ax: &axClients{}}); err == nil {
			t.Errorf("stream_linger %s: accepted", linger)
		}
	}
	// Past suspendAfter only delays suspension (ax's clock starts when the
	// stream closes): warned about at startup, not refused (caught in review).
	c0 := f.cfg
	ax0 := *c0.AX
	ax0.StreamLinger = "1h"
	c0.AX = &ax0
	if _, err := buildAgent(c0, agentBuilder{ax: &axClients{}}); err != nil {
		t.Errorf("stream_linger 1h: %v", err)
	}
	c := f.cfg
	axc := *c.AX
	axc.StreamLinger = "30s"
	c.AX = &axc
	a, err := buildAgent(c, agentBuilder{ax: &axClients{}})
	if err != nil || a.ax.linger != 30*time.Second {
		t.Errorf("30s: %v, %v", a, err)
	}
}

// The router cuts any stream at its route timeout, five minutes, which a long
// turn outlasts: the relay reconnects from its cursor and the answer still
// arrives, once.
func TestALongTurnSurvivesTheRoutersCut(t *testing.T) {
	f, sr := newStreamFixture(t)
	th := watch(f.out)
	sr.silent.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.say(t, ctx, "C1:1", "a long one")
	waitFor(t, func() bool { return sr.open.Load() == 1 }, "the stream")
	actor := "agents/" + ax.TaskName("sb-triage", "slack", "C1:1")

	sr.answer(actor, "early part") // delivered before the cut
	th.answered(t, "early part")
	sr.queue(actor) <- cutStream
	waitFor(t, func() bool {
		sr.mu.Lock()
		defer sr.mu.Unlock()
		return len(sr.sinces) == 2
	}, "the relay to reconnect after the cut")
	sr.answer(actor, "late answer")
	sr.queue(actor) <- "event: turn-complete\ndata: {\"prompt_id\":\"p\",\"model\":\"m\",\"latency_ms\":5}\n\n"
	th.answered(t, "late answer")
	time.Sleep(100 * time.Millisecond)
	sr.mu.Lock()
	sinces := append([]string(nil), sr.sinces...)
	sr.mu.Unlock()
	if sinces[1] != "1" {
		t.Errorf("reconnected with since %q, want 1 (the part before the cut)", sinces[1])
	}
	if th.count("early part") != 1 || th.count("late answer") != 1 {
		t.Errorf("deliveries: early %d, late %d; want each once", th.count("early part"), th.count("late answer"))
	}
}

// What the daemon says outranks switchboard's own idea of the turn: after a
// reconnect, a long turn's narration is taken as its answer and the turn is
// marked over, while the daemon is still working. Released then, the answer
// would wait for the next message and a prompt would never be posted (caught
// in review).
func TestAConversationTheDaemonCallsBusyKeepsItsStreams(t *testing.T) {
	f, sr := newStreamFixture(t)
	sr.silent.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.say(t, ctx, "C1:1", "a long one")
	actor := "agents/" + ax.TaskName("sb-triage", "slack", "C1:1")
	sr.queue(actor) <- frameStreaming
	e := entryOf(f.r, "C1:1")
	waitFor(t, func() bool { return e.agentBusy.Load() }, "the daemon to say it is working")
	e.endTurn() // switchboard believes the turn over

	if n := f.r.releaseIdleAX(time.Now().Add(time.Hour)); n != 0 {
		t.Fatalf("released %d while the daemon was working", n)
	}
	sr.queue(actor) <- frameIdle
	waitFor(t, func() bool { return !e.agentBusy.Load() }, "the daemon to say it is idle")
	if n := f.r.releaseIdleAX(time.Now().Add(time.Hour)); n != 1 {
		t.Errorf("released %d once idle, want 1", n)
	}
}

// A question on screen keeps its conversation's streams: the prompt stream is
// what would carry the next one, and the press revives nothing sooner.
func TestAnOpenQuestionKeepsItsStreams(t *testing.T) {
	f, _ := newStreamFixture(t)
	th := watch(f.out)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.say(t, ctx, "C1:1", "hello")
	th.answered(t, "answer 1")
	e := entryOf(f.r, "C1:1")
	waitFor(t, func() bool { return !e.turnInFlight() && !e.awaitingAnswer() }, "the turn to end")
	e.qmu.Lock()
	e.asked = map[string]*askRecord{"p1": {text: "run it?", posted: true}}
	e.qmu.Unlock()
	if n := f.r.releaseIdleAX(time.Now().Add(time.Hour)); n != 0 {
		t.Errorf("released %d with a question unanswered", n)
	}
}

// A turn being set up — found, its placeholder posting, not yet in flight —
// is not released from under it (caught in review).
func TestATurnBeingSetUpPinsItsConversation(t *testing.T) {
	f, _ := newStreamFixture(t)
	th := watch(f.out)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.say(t, ctx, "C1:1", "hello")
	th.answered(t, "answer 1")
	e := entryOf(f.r, "C1:1")
	waitFor(t, func() bool { return !e.turnInFlight() && !e.awaitingAnswer() }, "the turn to end")
	e.pins.Add(1)
	if n := f.r.releaseIdleAX(time.Now().Add(time.Hour)); n != 0 {
		t.Errorf("released %d while a turn was being set up", n)
	}
	e.pins.Add(-1)
	if n := f.r.releaseIdleAX(time.Now().Add(time.Hour)); n != 1 {
		t.Errorf("released %d once unpinned, want 1", n)
	}
}

// entryOf is a conversation's live entry, read under the lock.
func entryOf(r *Router, conv string) *sessionEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[conv]
}

// A template without spec.idle never suspends its tasks, so its conversations
// keep their streams: letting go would only cost a revive (caught in review).
func TestATemplateWithoutIdleIsNotSwept(t *testing.T) {
	f, _ := newStreamFixture(t)
	th := watch(f.out)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.say(t, ctx, "C1:1", "hello")
	th.answered(t, "answer 1")
	e := entryOf(f.r, "C1:1")
	waitFor(t, func() bool { return !e.turnInFlight() && !e.awaitingAnswer() && !e.agentBusy.Load() }, "the turn to end")
	f.r.agents().byName["triage"].ax.idle = false
	if n := f.r.releaseIdleAX(time.Now().Add(time.Hour)); n != 0 {
		t.Errorf("released %d for a template that never suspends", n)
	}
}
