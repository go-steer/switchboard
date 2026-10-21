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
}

func newStreamRouter(t *testing.T) *streamRouter {
	t.Helper()
	r := &streamRouter{streams: map[string]chan string{}}
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
			r.mu.Lock()
			r.seq++
			seq := r.seq
			r.mu.Unlock()
			if ch := r.queue(actor); ch != nil {
				ch <- fmt.Sprintf("event: agent\ndata: {\"seq\":%d,\"event\":{\"Content\":{\"parts\":[{\"text\":\"answer %d\"}],\"role\":\"model\"},\"Partial\":false}}\n\n", seq, seq)
				ch <- "event: turn-complete\ndata: {\"prompt_id\":\"p\",\"model\":\"m\",\"latency_ms\":5}\n\n"
			}
		case strings.HasSuffix(req.URL.Path, "/events"):
			ch := r.queue(actor)
			r.mu.Lock()
			r.sinces = append(r.sinces, req.URL.Query().Get("since"))
			r.mu.Unlock()
			r.open.Add(1)
			defer r.open.Add(-1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "event: capabilities\ndata: %s\n\n", capsWithBoundary)
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

// answered waits for the thread to receive text.
func answered(t *testing.T, out *fakeSender, text string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case r := <-out.replies:
			if r.Text == text {
				return
			}
		case <-deadline:
			t.Fatalf("no %q in the thread", text)
		}
	}
}

// Once its turn is over and its linger has passed, an ax conversation lets
// go of its streams, so the task can suspend; the next message picks the
// stream up where it left off, and nothing is delivered twice.
func TestAnIdleAXConversationLetsGoOfItsTask(t *testing.T) {
	f, sr := newStreamFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	f.say(t, ctx, "C1:1", "hello")
	answered(t, f.out, "answer 1")
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
	answered(t, f.out, "answer 2")
	sr.mu.Lock()
	defer sr.mu.Unlock()
	if len(sr.sinces) != 2 || sr.sinces[1] != "1" {
		t.Errorf("streams opened with since %v, want the second from the first answer", sr.sinces)
	}
	select {
	case r := <-f.out.replies:
		if r.Text == "answer 1" {
			t.Error("the first answer was delivered again")
		}
	default:
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
	for _, linger := range []string{"10m", "1h", "soon", "-1s"} {
		c := f.cfg
		axc := *c.AX
		axc.StreamLinger = linger
		c.AX = &axc
		if _, err := buildAgent(c, agentBuilder{ax: &axClients{}}); err == nil {
			t.Errorf("stream_linger %s: accepted against suspendAfter 10m", linger)
		}
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
	sr.silent.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.say(t, ctx, "C1:1", "a long one")
	waitFor(t, func() bool { return sr.open.Load() == 1 }, "the stream")
	actor := "agents/" + ax.TaskName("sb-triage", "slack", "C1:1")

	sr.queue(actor) <- cutStream
	waitFor(t, func() bool {
		sr.mu.Lock()
		defer sr.mu.Unlock()
		return len(sr.sinces) == 2
	}, "the relay to reconnect after the cut")
	sr.queue(actor) <- "event: agent\ndata: {\"seq\":7,\"event\":{\"Content\":{\"parts\":[{\"text\":\"late answer\"}],\"role\":\"model\"},\"Partial\":false}}\n\n"
	sr.queue(actor) <- "event: turn-complete\ndata: {\"prompt_id\":\"p\",\"model\":\"m\",\"latency_ms\":5}\n\n"
	answered(t, f.out, "late answer")
	time.Sleep(100 * time.Millisecond)
	for drained := false; !drained; {
		select {
		case r := <-f.out.replies:
			if r.Text == "late answer" {
				t.Fatal("the answer was delivered twice")
			}
		default:
			drained = true
		}
	}
}
