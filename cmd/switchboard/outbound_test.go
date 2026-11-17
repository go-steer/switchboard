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
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/go-steer/switchboard/pkg/chat"
	"github.com/go-steer/switchboard/pkg/daemon"
)

// threadedSender stands in for a real Slack or Google Chat adapter, whose Send
// always names the thread the message landed in (landedKey) and optionally
// reports single-message fit for append/rollover tests.
type threadedSender struct {
	mu      sync.Mutex
	nextID  int
	fitMax  int // <= 0 disables FitsOneMessage limit when fittingThreadedSender wraps this
	gchat   bool
	sent    []chat.Reply
	updates []fakeUpdate
}

func (s *threadedSender) Send(_ context.Context, r chat.Reply) (chat.MessageRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	s.sent = append(s.sent, r)
	id := fmt.Sprintf("ts%d", s.nextID)
	conv := r.Conversation
	if !namesThread(conv) {
		ch := channelPart(conv)
		if s.gchat {
			id = fmt.Sprintf("%s/messages/m%d", ch, s.nextID)
			conv = fmt.Sprintf("%s:%s/threads/t%d", ch, ch, s.nextID)
		} else {
			conv = ch + ":" + id
		}
	} else if s.gchat {
		ch := channelPart(conv)
		id = fmt.Sprintf("%s/messages/m%d", ch, s.nextID)
	}
	return chat.MessageRef{Conversation: conv, ID: id}, nil
}

func (s *threadedSender) Update(_ context.Context, ref chat.MessageRef, r chat.Reply) error {
	s.mu.Lock()
	s.updates = append(s.updates, fakeUpdate{ref: ref, text: r.Text, reply: r})
	s.mu.Unlock()
	return nil
}

func (s *threadedSender) Delete(context.Context, chat.MessageRef) error { return nil }

type fittingThreadedSender struct {
	*threadedSender
}

func (f fittingThreadedSender) FitsOneMessage(text string) bool {
	if f.fitMax <= 0 {
		return true
	}
	return len(text) <= f.fitMax
}

type sseFrame struct {
	event string
	data  string
}

type seedTestDaemon struct {
	creates    atomic.Int64
	injects    atomic.Int64
	failCreate atomic.Int32
	failInject atomic.Int32
	injected   chan string

	createEntered chan struct{}
	createGate    <-chan struct{}

	onInject func(w http.ResponseWriter, r *http.Request, msg string) bool

	mu         sync.Mutex
	streams    []chan sseFrame
	subscribed chan struct{}
}

func (d *seedTestDaemon) emit(event, data string) {
	d.mu.Lock()
	subs := append([]chan sseFrame(nil), d.streams...)
	d.mu.Unlock()
	for _, ch := range subs {
		ch <- sseFrame{event: event, data: data}
	}
}

func newSeedTestRig(t *testing.T, out sender) (*Router, *ingress, *seedTestDaemon) {
	t.Helper()
	d := &seedTestDaemon{
		injected:   make(chan string, 16),
		subscribed: make(chan struct{}, 8),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, r *http.Request) {
		n := d.creates.Add(1)
		if d.createEntered != nil {
			select {
			case d.createEntered <- struct{}{}:
			default:
			}
		}
		if d.createGate != nil {
			select {
			case <-d.createGate:
			case <-r.Context().Done():
				return
			}
		}
		if rem := d.failCreate.Load(); rem > 0 {
			d.failCreate.Add(-1)
			http.Error(w, `{"error":"unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"app":"core-agent","sessionID":"s%d"}`, n)
	})
	mux.HandleFunc("POST /sessions/{app}/{sid}/inject", func(w http.ResponseWriter, r *http.Request) {
		d.injects.Add(1)
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Message string `json:"message"`
		}
		msg := string(raw)
		if err := json.Unmarshal(raw, &body); err == nil && body.Message != "" {
			msg = body.Message
		}
		if d.onInject != nil && d.onInject(w, r, msg) {
			return
		}
		if rem := d.failInject.Load(); rem > 0 {
			d.failInject.Add(-1)
			http.Error(w, `{"error":"unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		d.injected <- msg
		fmt.Fprint(w, `{"injected":"ok"}`)
	})
	mux.HandleFunc("GET /sessions/{app}/{sid}/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		fl.Flush()
		ch := make(chan sseFrame, 16)
		d.mu.Lock()
		d.streams = append(d.streams, ch)
		d.mu.Unlock()
		select {
		case d.subscribed <- struct{}{}:
		default:
		}
		for {
			select {
			case <-r.Context().Done():
				return
			case f := <-ch:
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", f.event, f.data)
				fl.Flush()
			}
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	dc, err := daemon.New(daemon.Config{BaseURL: srv.URL, BearerToken: "tok", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	router := NewRouter(dc, out, ProgressOff, newMetrics(), nil)
	ing := newBindingIngress(t, out, router)
	return router, ing, d
}

func recvInjected(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for injected message")
		return ""
	}
}

// TestOutboundPostSeedsFirstInboundTurnOnly verifies the unbound incident ->
// chat reply flow: an outbound POST with no session records the root text on
// the thread it created; the first human reply in that thread carries that
// context ahead of the reply; the second human reply carries only the new
// reply.
func TestOutboundPostSeedsFirstInboundTurnOnly(t *testing.T) {
	out := &threadedSender{}
	router, ing, d := newSeedTestRig(t, out)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := do(t, ing, http.MethodPost, `{"conversation":"C0123","text":"Deployment triage-lab/billing is OOMKilled (16Mi limit)."}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST = %d %s, want 200", w.Code, w.Body)
	}
	var posted messageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &posted); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if posted.Conversation != "C0123:ts1" {
		t.Fatalf("posted.Conversation = %q, want C0123:ts1", posted.Conversation)
	}

	if err := router.Handle(ctx, chat.Message{
		Conversation: posted.Conversation,
		Channel:      "C0123",
		Caller:       "alice@example.com",
		Text:         "open a PR for this",
	}); err != nil {
		t.Fatalf("Handle turn 1: %v", err)
	}
	got1 := recvInjected(t, d.injected)
	want1 := formatSeededTurn("Deployment triage-lab/billing is OOMKilled (16Mi limit).", "open a PR for this")
	if got1 != want1 {
		t.Errorf("turn 1 injected =\n%s\nwant:\n%s", got1, want1)
	}

	if err := router.Handle(ctx, chat.Message{
		Conversation: posted.Conversation,
		Channel:      "C0123",
		Caller:       "alice@example.com",
		Text:         "also raise requests.memory to 64Mi",
	}); err != nil {
		t.Fatalf("Handle turn 2: %v", err)
	}
	got2 := recvInjected(t, d.injected)
	if got2 != "also raise requests.memory to 64Mi" {
		t.Errorf("turn 2 injected = %q, want bare reply without repeated seed", got2)
	}

	router.mu.Lock()
	rem := len(router.outbound)
	router.mu.Unlock()
	if rem != 0 {
		t.Errorf("outbound entries remaining = %d, want 0 after turn 1 consumed the seed", rem)
	}
}

// TestOutboundPatchReplaceAndAppendUpdateSeed verifies that revising a message
// in place (replace or append), rolling over into a continuation message, and
// editing the continuation (including bare-space PATCHes on Google Chat) keeps
// the thread's seed synchronized with what is actually on screen.
func TestOutboundPatchReplaceAndAppendUpdateSeed(t *testing.T) {
	base := &threadedSender{gchat: true, fitMax: 60}
	out := fittingThreadedSender{threadedSender: base}
	router, ing, d := newSeedTestRig(t, out)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initial post into a Google Chat space -> assigns thread t1 and message m1.
	w := do(t, ing, http.MethodPost, `{"conversation":"spaces/AAA","text":"investigating billing..."}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST = %d %s", w.Code, w.Body)
	}
	var root messageResponse
	_ = json.Unmarshal(w.Body.Bytes(), &root)

	// Replace root using bare space conversation key.
	w = do(t, ing, http.MethodPatch, fmt.Sprintf(`{"conversation":"spaces/AAA","id":%q,"text":"billing OOMKilled (16Mi)"}`, root.ID))
	if w.Code != http.StatusNoContent {
		t.Fatalf("PATCH replace = %d %s, want 204", w.Code, w.Body)
	}

	// In-place append that still fits within 60 bytes.
	w = do(t, ing, http.MethodPatch, fmt.Sprintf(`{"conversation":%q,"id":%q,"append":"exit code 137"}`, root.Conversation, root.ID))
	if w.Code != http.StatusNoContent {
		t.Fatalf("PATCH append (in-place) = %d %s, want 204", w.Code, w.Body)
	}

	// Append that overflows 60 bytes -> rolls over into continuation message m2 in the same thread.
	w = do(t, ing, http.MethodPatch, fmt.Sprintf(`{"conversation":%q,"id":%q,"append":"manifest: apps/billing/deployment.yaml"}`, root.Conversation, root.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH append (rollover) = %d %s, want 200", w.Code, w.Body)
	}
	var cont messageResponse
	_ = json.Unmarshal(w.Body.Bytes(), &cont)

	// Replace the continuation message m2 using bare space key -> must update m2 without clobbering m1.
	w = do(t, ing, http.MethodPatch, fmt.Sprintf(`{"conversation":"spaces/AAA","id":%q,"text":"manifest: apps/billing/deployment.yaml (main)"}`, cont.ID))
	if w.Code != http.StatusNoContent {
		t.Fatalf("PATCH replace continuation = %d %s, want 204", w.Code, w.Body)
	}

	if err := router.Handle(ctx, chat.Message{
		Conversation: root.Conversation,
		Channel:      "spaces/AAA",
		Caller:       "bob@example.com",
		Text:         "please open a PR",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got := recvInjected(t, d.injected)
	wantSeed := "billing OOMKilled (16Mi)\nexit code 137\n\nmanifest: apps/billing/deployment.yaml (main)"
	if want := formatSeededTurn(wantSeed, "please open a PR"); got != want {
		t.Errorf("injected =\n%s\nwant:\n%s", got, want)
	}
}

// TestOutboundOversizePostStillSeedsWhenAppendCacheSkipsIt ensures a long
// incident report that exceeds FitsOneMessage (which ingress.bodies drops so
// append cannot corrupt a split post) is still delivered to the router's first
// inbound turn.
func TestOutboundOversizePostStillSeedsWhenAppendCacheSkipsIt(t *testing.T) {
	base := &threadedSender{fitMax: 40}
	out := fittingThreadedSender{threadedSender: base}
	router, ing, d := newSeedTestRig(t, out)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	longReport := strings.Repeat("OOMKilled in triage-lab/billing. ", 5) // > 40 bytes
	w := do(t, ing, http.MethodPost, fmt.Sprintf(`{"conversation":"C0123","text":%q}`, longReport))
	if w.Code != http.StatusOK {
		t.Fatalf("POST = %d %s", w.Code, w.Body)
	}
	var posted messageResponse
	_ = json.Unmarshal(w.Body.Bytes(), &posted)

	if err := router.Handle(ctx, chat.Message{
		Conversation: posted.Conversation,
		Channel:      "C0123",
		Caller:       "alice@example.com",
		Text:         "open a PR",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got, want := recvInjected(t, d.injected), formatSeededTurn(longReport, "open a PR"); got != want {
		t.Errorf("injected =\n%s\nwant:\n%s", got, want)
	}
}

// TestOutboundTransientCreateOrInjectFailurePreservesSeed checks that neither a
// failed CreateSession nor a failed Inject loses the thread's outbound context.
func TestOutboundTransientCreateOrInjectFailurePreservesSeed(t *testing.T) {
	out := &threadedSender{}
	router, ing, d := newSeedTestRig(t, out)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := do(t, ing, http.MethodPost, `{"conversation":"C0123","text":"checkout readiness failing"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST = %d %s", w.Code, w.Body)
	}
	var posted messageResponse
	_ = json.Unmarshal(w.Body.Bytes(), &posted)

	msg := chat.Message{Conversation: posted.Conversation, Channel: "C0123", Caller: "alice@example.com", Text: "fix it"}

	// Attempt 1: CreateSession fails (503).
	d.failCreate.Store(1)
	if err := router.Handle(ctx, msg); err == nil {
		t.Fatal("attempt 1 succeeded despite failCreate")
	}

	// Attempt 2: CreateSession succeeds, Inject fails (503).
	d.failInject.Store(1)
	if err := router.Handle(ctx, msg); err == nil {
		t.Fatal("attempt 2 succeeded despite failInject")
	}

	// Attempt 3: succeeds and still carries the seed.
	if err := router.Handle(ctx, msg); err != nil {
		t.Fatalf("attempt 3 Handle: %v", err)
	}
	if got, want := recvInjected(t, d.injected), formatSeededTurn("checkout readiness failing", "fix it"); got != want {
		t.Errorf("attempt 3 injected =\n%s\nwant:\n%s", got, want)
	}

	// Attempt 4: subsequent turn does not repeat the seed.
	msg.Text = "thanks"
	if err := router.Handle(ctx, msg); err != nil {
		t.Fatalf("attempt 4 Handle: %v", err)
	}
	if got := recvInjected(t, d.injected); got != "thanks" {
		t.Errorf("attempt 4 injected = %q, want bare reply", got)
	}
}

// TestOutboundConcurrentWaitersSerializeFirstInject verifies that two turns
// waiting on <-e.ready serialize on entry.seedMu: when the first Inject fails,
// the second waiter still delivers the seed (leaving no zombie seed for a third
// turn), and when the first Inject succeeds, it lands at the daemon seeded
// before the second waiter lands bare.
func TestOutboundConcurrentWaitersSerializeFirstInject(t *testing.T) {
	t.Run("first_inject_fails_second_waiter_delivers_seed", func(t *testing.T) {
		out := &threadedSender{}
		router, ing, d := newSeedTestRig(t, out)
		entered := make(chan struct{}, 1)
		gate := make(chan struct{})
		d.createEntered = entered
		d.createGate = gate
		d.failInject.Store(1)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		w := do(t, ing, http.MethodPost, `{"conversation":"C0123","text":"cart crashloop"}`)
		var posted messageResponse
		_ = json.Unmarshal(w.Body.Bytes(), &posted)

		errs := make(chan error, 2)
		go func() {
			errs <- router.Handle(ctx, chat.Message{
				Conversation: posted.Conversation,
				Channel:      "C0123",
				Caller:       "alice@example.com",
				Text:         "msg-A",
			})
		}()
		<-entered

		// Wait until msg-B is queued behind <-e.ready on the opening entry.
		go func() {
			errs <- router.Handle(ctx, chat.Message{
				Conversation: posted.Conversation,
				Channel:      "C0123",
				Caller:       "alice@example.com",
				Text:         "msg-B",
			})
		}()
		waitFor(t, func() bool {
			router.mu.Lock()
			_, ok := router.sessions[posted.Conversation]
			router.mu.Unlock()
			return ok
		}, "session entry registered")
		time.Sleep(20 * time.Millisecond)

		close(gate)

		err1 := <-errs
		err2 := <-errs
		if (err1 == nil) == (err2 == nil) {
			t.Fatalf("expected exactly one Inject failure, got err1=%v err2=%v", err1, err2)
		}

		// The winning waiter must have carried the seed.
		got := recvInjected(t, d.injected)
		wantA := formatSeededTurn("cart crashloop", "msg-A")
		wantB := formatSeededTurn("cart crashloop", "msg-B")
		if got != wantA && got != wantB {
			t.Fatalf("winning waiter injected = %q, want seeded turn", got)
		}

		// Turn 3 must NOT inherit a zombie seed.
		if err := router.Handle(ctx, chat.Message{
			Conversation: posted.Conversation,
			Channel:      "C0123",
			Caller:       "alice@example.com",
			Text:         "msg-C",
		}); err != nil {
			t.Fatalf("Handle msg-C: %v", err)
		}
		if got3 := recvInjected(t, d.injected); got3 != "msg-C" {
			t.Errorf("turn 3 injected = %q, want bare msg-C", got3)
		}
	})

	t.Run("first_inject_succeeds_before_second_waiter_proceeds_bare", func(t *testing.T) {
		out := &threadedSender{}
		router, ing, d := newSeedTestRig(t, out)
		entered := make(chan struct{}, 1)
		gate := make(chan struct{})
		d.createEntered = entered
		d.createGate = gate

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		w := do(t, ing, http.MethodPost, `{"conversation":"C0123","text":"api 500 spike"}`)
		var posted messageResponse
		_ = json.Unmarshal(w.Body.Bytes(), &posted)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := router.Handle(ctx, chat.Message{
				Conversation: posted.Conversation,
				Channel:      "C0123",
				Caller:       "alice@example.com",
				Text:         "first",
			}); err != nil {
				t.Errorf("Handle first: %v", err)
			}
		}()
		<-entered
		go func() {
			defer wg.Done()
			if err := router.Handle(ctx, chat.Message{
				Conversation: posted.Conversation,
				Channel:      "C0123",
				Caller:       "alice@example.com",
				Text:         "second",
			}); err != nil {
				t.Errorf("Handle second: %v", err)
			}
		}()
		time.Sleep(20 * time.Millisecond)
		close(gate)
		wg.Wait()

		firstGot := recvInjected(t, d.injected)
		secondGot := recvInjected(t, d.injected)
		if !strings.HasPrefix(firstGot, "[Thread context") {
			t.Errorf("first daemon inject = %q, want seeded turn first", firstGot)
		}
		if strings.Contains(secondGot, "[Thread context") {
			t.Errorf("second daemon inject = %q, want bare turn without seed", secondGot)
		}
	})
}

// TestOutboundPatchWhileCreateSessionInFlightUpdatesSeed verifies that a PATCH
// or continuation landing while CreateSession (or an ax cold wake) is still
// opening updates the unconsumed outbound seed rather than being dropped.
func TestOutboundPatchWhileCreateSessionInFlightUpdatesSeed(t *testing.T) {
	base := &threadedSender{fitMax: 80}
	out := fittingThreadedSender{threadedSender: base}
	router, ing, d := newSeedTestRig(t, out)
	entered := make(chan struct{}, 1)
	gate := make(chan struct{})
	d.createEntered = entered
	d.createGate = gate

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := do(t, ing, http.MethodPost, `{"conversation":"C0123","text":"investigating billing..."}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST = %d %s", w.Code, w.Body)
	}
	var root messageResponse
	_ = json.Unmarshal(w.Body.Bytes(), &root)

	done := make(chan error, 1)
	go func() {
		done <- router.Handle(ctx, chat.Message{
			Conversation: root.Conversation,
			Channel:      "C0123",
			Caller:       "alice@example.com",
			Text:         "open a PR for this",
		})
	}()
	<-entered

	// While CreateSession is blocked in flight, the alert pipeline edits the
	// root post and rolls over a continuation line.
	w = do(t, ing, http.MethodPatch, fmt.Sprintf(`{"conversation":%q,"id":%q,"text":"billing OOMKilled (16Mi limit, exit 137)"}`, root.Conversation, root.ID))
	if w.Code != http.StatusNoContent {
		t.Fatalf("PATCH while opening = %d %s, want 204", w.Code, w.Body)
	}
	w = do(t, ing, http.MethodPatch, fmt.Sprintf(`{"conversation":%q,"id":%q,"append":"manifest: apps/billing/deployment.yaml (repo main)"}`, root.Conversation, root.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH rollover while opening = %d %s, want 200", w.Code, w.Body)
	}

	close(gate)
	if err := <-done; err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got := recvInjected(t, d.injected)
	wantSeed := "billing OOMKilled (16Mi limit, exit 137)\n\nmanifest: apps/billing/deployment.yaml (repo main)"
	if want := formatSeededTurn(wantSeed, "open a PR for this"); got != want {
		t.Errorf("injected =\n%s\nwant:\n%s", got, want)
	}
}

// TestOutboundAmbiguousInjectErrorFollowedByStreamActivityClearsSeed verifies
// that if Inject errors client-side after the daemon actually accepted the turn
// and emitted activity on the SSE stream, relay marks the session seeded and
// clears the outbound record so the user's retry is not double-seeded.
func TestOutboundAmbiguousInjectErrorFollowedByStreamActivityClearsSeed(t *testing.T) {
	out := &threadedSender{}
	router, ing, d := newSeedTestRig(t, out)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls atomic.Int32
	d.onInject = func(w http.ResponseWriter, r *http.Request, msg string) bool {
		if calls.Add(1) != 1 {
			return false
		}
		// Wait until relay has subscribed, then emit the daemon's user-echo
		// agent event (seq 1) before failing the HTTP response with 502.
		select {
		case <-d.subscribed:
		case <-time.After(2 * time.Second):
		}
		d.injected <- msg
		d.emit(daemon.EventAgent, `{"seq":1,"event":{"Content":{"parts":[{"text":"echo"}],"role":"user"},"Partial":false}}`)
		waitFor(t, func() bool {
			router.mu.Lock()
			_, still := router.outbound["C0123:ts1"]
			router.mu.Unlock()
			return !still
		}, "relay cleared outbound seed on seq 1")
		http.Error(w, `{"error":"gateway timeout"}`, http.StatusBadGateway)
		return true
	}

	w := do(t, ing, http.MethodPost, `{"conversation":"C0123","text":"payments high latency"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST = %d %s", w.Code, w.Body)
	}
	var posted messageResponse
	_ = json.Unmarshal(w.Body.Bytes(), &posted)

	if err := router.Handle(ctx, chat.Message{
		Conversation: posted.Conversation,
		Channel:      "C0123",
		Caller:       "alice@example.com",
		Text:         "check p99",
	}); err == nil {
		t.Fatal("expected turn 1 Handle to return 502 error")
	}
	if got1, want1 := recvInjected(t, d.injected), formatSeededTurn("payments high latency", "check p99"); got1 != want1 {
		t.Errorf("turn 1 injected =\n%s\nwant:\n%s", got1, want1)
	}

	if err := router.Handle(ctx, chat.Message{
		Conversation: posted.Conversation,
		Channel:      "C0123",
		Caller:       "alice@example.com",
		Text:         "open a PR",
	}); err != nil {
		t.Fatalf("turn 2 Handle: %v", err)
	}
	if got2 := recvInjected(t, d.injected); got2 != "open a PR" {
		t.Errorf("turn 2 injected = %q, want bare reply after stream activity cleared seed", got2)
	}
}

// TestOutboundReapIdleWithZeroRelayed covers both idle-reap paths where
// Relayed == 0: (a) a failed first Inject followed by reapIdle preserves the
// seed (and still accepts PATCH updates while dormant), and (b) a succeeded
// first Inject that ended in turn-error (Relayed == 0) records Seeded == true
// so reviving after reapIdle never repeats the seed or accepts late outbound
// posts.
func TestOutboundReapIdleWithZeroRelayed(t *testing.T) {
	t.Run("failed_first_inject_reaped_preserves_and_updates_seed", func(t *testing.T) {
		store, err := openFileStore(t.TempDir())
		if err != nil {
			t.Fatalf("openFileStore: %v", err)
		}
		out := &threadedSender{}
		router, ing, d := newSeedTestRig(t, out)
		router.setStore(store)
		router.setIdleTTL(time.Minute)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		w := do(t, ing, http.MethodPost, `{"conversation":"C0123","text":"initial alert"}`)
		var posted messageResponse
		_ = json.Unmarshal(w.Body.Bytes(), &posted)

		d.failInject.Store(1)
		if err := router.Handle(ctx, chat.Message{
			Conversation: posted.Conversation,
			Channel:      "C0123",
			Caller:       "alice@example.com",
			Text:         "fix it",
		}); err == nil {
			t.Fatal("expected turn 1 Inject failure")
		}

		if n := router.reapIdle(time.Now().Add(2 * time.Minute)); n != 1 {
			t.Fatalf("reapIdle = %d, want 1", n)
		}

		// PATCH while the unseeded session sits in r.dormant must still revise the seed.
		w = do(t, ing, http.MethodPatch, fmt.Sprintf(`{"conversation":%q,"id":%q,"text":"revised alert while dormant"}`, posted.Conversation, posted.ID))
		if w.Code != http.StatusNoContent {
			t.Fatalf("PATCH while dormant = %d %s", w.Code, w.Body)
		}

		if err := router.Handle(ctx, chat.Message{
			Conversation: posted.Conversation,
			Channel:      "C0123",
			Caller:       "alice@example.com",
			Text:         "fix it now",
		}); err != nil {
			t.Fatalf("Handle after revive: %v", err)
		}
		if got, want := recvInjected(t, d.injected), formatSeededTurn("revised alert while dormant", "fix it now"); got != want {
			t.Errorf("injected after revive =\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("succeeded_first_inject_with_turn_error_reaped_does_not_reseed", func(t *testing.T) {
		store, err := openFileStore(t.TempDir())
		if err != nil {
			t.Fatalf("openFileStore: %v", err)
		}
		out := &threadedSender{}
		router, ing, d := newSeedTestRig(t, out)
		router.setStore(store)
		router.setIdleTTL(time.Minute)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		w := do(t, ing, http.MethodPost, `{"conversation":"C0123","text":"redis connection refused"}`)
		var posted messageResponse
		_ = json.Unmarshal(w.Body.Bytes(), &posted)

		if err := router.Handle(ctx, chat.Message{
			Conversation: posted.Conversation,
			Channel:      "C0123",
			Caller:       "alice@example.com",
			Text:         "open PR",
		}); err != nil {
			t.Fatalf("Handle turn 1: %v", err)
		}
		_ = recvInjected(t, d.injected)
		<-d.subscribed

		// Turn 1 fails inside the daemon before producing any model answer (Relayed == 0).
		d.emit(daemon.EventTurnError, `{"kind":"transient_network","message":"upstream reset","retryable":true}`)
		waitFor(t, func() bool {
			router.mu.Lock()
			e := router.sessions[posted.Conversation]
			router.mu.Unlock()
			return e != nil && !e.turnInFlight()
		}, "turn-error ended in-flight state")

		if n := router.reapIdle(time.Now().Add(2 * time.Minute)); n != 1 {
			t.Fatalf("reapIdle = %d, want 1", n)
		}

		// Late unbound post into an already-seeded dormant thread must be ignored.
		w = do(t, ing, http.MethodPost, fmt.Sprintf(`{"conversation":%q,"text":"late sidecar note"}`, posted.Conversation))
		if w.Code != http.StatusOK {
			t.Fatalf("late POST = %d %s", w.Code, w.Body)
		}

		if err := router.Handle(ctx, chat.Message{
			Conversation: posted.Conversation,
			Channel:      "C0123",
			Caller:       "alice@example.com",
			Text:         "try again",
		}); err != nil {
			t.Fatalf("Handle turn 2 after revive: %v", err)
		}
		if got := recvInjected(t, d.injected); got != "try again" {
			t.Errorf("turn 2 injected = %q, want bare reply without re-seeding", got)
		}
	})
}

// TestOutboundBoundOrActiveThreadIsNotSeeded verifies that bound threads (both
// bound at initial POST and bound on a follow-up POST before any reply) and
// threads that already have a live session never prepend duplicate seed text.
func TestOutboundBoundOrActiveThreadIsNotSeeded(t *testing.T) {
	bd := &boundDaemon{resumed: make(chan string, 8)}
	bd.publish(agentFrame(1, "prior incident work"))
	injected := make(chan string, 8)

	mux := bd.mux(t)
	wrap := http.NewServeMux()
	wrap.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/inject") {
			raw, _ := io.ReadAll(r.Body)
			var body struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal(raw, &body)
			injected <- body.Message
			r.Body = io.NopCloser(strings.NewReader(string(raw)))
		}
		mux.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(wrap)
	defer srv.Close()

	dc, err := daemon.New(daemon.Config{BaseURL: srv.URL, BearerToken: "tok", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	out := &threadedSender{}
	router := NewRouter(dc, out, ProgressOff, newMetrics(), nil)
	ing := newBindingIngress(t, out, router)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Unbound root post followed by a bound post in the same thread before any human reply.
	w := do(t, ing, http.MethodPost, `{"conversation":"C0123","text":"preliminary notice"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("unbound POST = %d %s", w.Code, w.Body)
	}
	var root messageResponse
	_ = json.Unmarshal(w.Body.Bytes(), &root)

	w = do(t, ing, http.MethodPost, fmt.Sprintf(`{"conversation":%q,"text":"bound update","session":%q}`, root.Conversation, boundSession))
	if w.Code != http.StatusOK {
		t.Fatalf("bound POST = %d %s", w.Code, w.Body)
	}

	if err := router.Handle(ctx, chat.Message{
		Conversation: root.Conversation,
		Channel:      "C0123",
		Caller:       "alice@example.com",
		Text:         "roll it back",
	}); err != nil {
		t.Fatalf("Handle bound thread: %v", err)
	}
	if got := recvInjected(t, injected); got != "roll it back" {
		t.Errorf("bound thread injected = %q, want bare reply without outbound seed", got)
	}

	// Posting unbound into an already-live thread is also ignored by NoteOutbound.
	w = do(t, ing, http.MethodPost, fmt.Sprintf(`{"conversation":%q,"text":"late sidecar note"}`, root.Conversation))
	if w.Code != http.StatusOK {
		t.Fatalf("late POST = %d %s", w.Code, w.Body)
	}
	router.mu.Lock()
	_, seeded := router.outbound[root.Conversation]
	router.mu.Unlock()
	if seeded {
		t.Errorf("live conversation %s was recorded in router.outbound", root.Conversation)
	}
}

// TestOutboundStateSnapshotAndRestore verifies that unconsumed outbound thread
// seeds survive a restart via --state-dir, while threads whose session already
// progressed (Relayed, Noticed, Asked, or Seeded) drop any stale outbound seed.
func TestOutboundStateSnapshotAndRestore(t *testing.T) {
	dir := t.TempDir()
	store1, err := openFileStore(dir)
	if err != nil {
		t.Fatalf("openFileStore: %v", err)
	}

	out1 := &threadedSender{}
	router1, ing1, _ := newSeedTestRig(t, out1)
	router1.setStore(store1)

	w := do(t, ing1, http.MethodPost, `{"conversation":"C0123","text":"pod payments-abc OOMKilled"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST = %d %s", w.Code, w.Body)
	}
	var posted messageResponse
	_ = json.Unmarshal(w.Body.Bytes(), &posted)

	if err := router1.trySaveState(); err != nil {
		t.Fatalf("trySaveState: %v", err)
	}

	// Second process loads the snapshot (plus synthetic progressed records to
	// verify restore drops stale outbound seeds when Noticed or Asked is set).
	store2, err := openFileStore(dir)
	if err != nil {
		t.Fatalf("openFileStore 2: %v", err)
	}
	st, err := store2.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	now := time.Now().UTC()
	st.Sessions = map[string]sessionRecord{
		"C0123:progressed-noticed": {
			Channel: "C0123",
			Session: "core-agent/s90",
			Noticed: 2,
			Touched: now.Add(-2 * time.Hour),
		},
		"C0123:progressed-asked": {
			Channel: "C0123",
			Session: "core-agent/s91",
			Asked:   []askedRecord{{ID: "p1", Text: "allow?", Kind: "tool"}},
			Touched: now.Add(-2 * time.Hour),
		},
	}
	st.Outbound = append(st.Outbound,
		outboundRecord{Conversation: "C0123:progressed-noticed", Messages: []outboundMsg{{ID: "n1", Text: "stale"}}, Touched: now},
		outboundRecord{Conversation: "C0123:progressed-asked", Messages: []outboundMsg{{ID: "a1", Text: "stale"}}, Touched: now},
	)

	out2 := &threadedSender{}
	router2, _, d2 := newSeedTestRig(t, out2)
	router2.setStore(store2)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	router2.restore(ctx, st, now)

	router2.mu.Lock()
	_, hasNoticedSeed := router2.outbound["C0123:progressed-noticed"]
	_, hasAskedSeed := router2.outbound["C0123:progressed-asked"]
	router2.mu.Unlock()
	if hasNoticedSeed || hasAskedSeed {
		t.Errorf("restore kept stale outbound seeds on progressed sessions: noticed=%v asked=%v", hasNoticedSeed, hasAskedSeed)
	}

	if err := router2.Handle(ctx, chat.Message{
		Conversation: posted.Conversation,
		Channel:      "C0123",
		Caller:       "alice@example.com",
		Text:         "open a PR",
	}); err != nil {
		t.Fatalf("Handle after restore: %v", err)
	}
	if got, want := recvInjected(t, d2.injected), formatSeededTurn("pod payments-abc OOMKilled", "open a PR"); got != want {
		t.Errorf("injected after restore =\n%s\nwant:\n%s", got, want)
	}
}

// TestOutboundEvictionUTF8CapAndReaperExpiry verifies bounded thread count,
// bounded message count per thread, multi-message near-boundary truncation
// always carrying outboundTruncatedSuffix while keeping message IDs resolvable,
// O(1) UTF-8 clamping with interior invalid-byte sanitization, and reaper
// pruning of expired outbound threads past dormantMaxAge.
func TestOutboundEvictionUTF8CapAndReaperExpiry(t *testing.T) {
	out := &threadedSender{}
	router, _, _ := newSeedTestRig(t, out)

	for i := range maxOutboundThreads + 5 {
		conv := fmt.Sprintf("C0:ts-%d", i)
		router.NoteOutbound(conv, fmt.Sprintf("ts-%d", i), fmt.Sprintf("alert %d", i))
	}
	router.mu.Lock()
	n := len(router.outbound)
	_, hasOldest := router.outbound["C0:ts-0"]
	_, hasFirstKept := router.outbound["C0:ts-5"]
	_, hasNewest := router.outbound[fmt.Sprintf("C0:ts-%d", maxOutboundThreads+4)]
	router.mu.Unlock()

	if n != maxOutboundThreads {
		t.Errorf("len(outbound) = %d, want %d", n, maxOutboundThreads)
	}
	if hasOldest || !hasFirstKept || !hasNewest {
		t.Errorf("eviction wrong: hasOldest=%v hasFirstKept=%v hasNewest=%v", hasOldest, hasFirstKept, hasNewest)
	}

	// Multi-byte UTF-8 rune ("🔥" = 4 bytes) repeated past maxOutboundSeedBytes.
	huge := strings.Repeat("🔥", (maxOutboundSeedBytes/4)+500)
	router.NoteOutbound("C0:utf8", "u1", huge)
	router.mu.Lock()
	rendered := router.outbound["C0:utf8"].render()
	router.mu.Unlock()
	if len(rendered) > maxOutboundSeedBytes {
		t.Errorf("len(rendered) = %d, exceeds %d", len(rendered), maxOutboundSeedBytes)
	}
	if !utf8.ValidString(rendered) {
		t.Error("clamped seed is not valid UTF-8")
	}
	if !strings.HasSuffix(rendered, outboundTruncatedSuffix) {
		t.Errorf("clamped seed missing %q suffix", outboundTruncatedSuffix)
	}

	// Interior invalid UTF-8 bytes are sanitized rather than dropping trailing text.
	router.NoteOutbound("C0:badutf8", "b1", "head-\xff\xfe-tail")
	router.mu.Lock()
	sanitized := router.outbound["C0:badutf8"].render()
	router.mu.Unlock()
	if !utf8.ValidString(sanitized) || !strings.Contains(sanitized, "head-") || !strings.Contains(sanitized, "-tail") {
		t.Errorf("sanitized = %q, want valid UTF-8 preserving head and tail", sanitized)
	}

	// Multi-message near-boundary cap: m1 leaves 6 bytes (< suffix length), m2
	// overflows; render() must still end with outboundTruncatedSuffix, and m2's
	// ID remains resolvable for bare-space PATCH once m1 shrinks.
	nearCap := strings.Repeat("a", maxOutboundSeedBytes-6)
	router.NoteOutbound("spaces/AAA:spaces/AAA/threads/t9", "spaces/AAA/messages/m1", nearCap)
	router.NoteOutbound("spaces/AAA:spaces/AAA/threads/t9", "spaces/AAA/messages/m2", "second message")
	router.mu.Lock()
	nearRendered := router.outbound["spaces/AAA:spaces/AAA/threads/t9"].render()
	router.mu.Unlock()
	if len(nearRendered) > maxOutboundSeedBytes || !strings.HasSuffix(nearRendered, outboundTruncatedSuffix) {
		t.Errorf("near-boundary multi-msg render len=%d, suffix=%v", len(nearRendered), strings.HasSuffix(nearRendered, outboundTruncatedSuffix))
	}
	router.EditOutbound("spaces/AAA", "spaces/AAA/messages/m1", "short m1")
	router.EditOutbound("spaces/AAA", "spaces/AAA/messages/m2", "short m2")
	router.mu.Lock()
	recovered := router.outbound["spaces/AAA:spaces/AAA/threads/t9"].render()
	router.mu.Unlock()
	if recovered != "short m1\n\nshort m2" {
		t.Errorf("recovered after replacing both messages = %q, want %q", recovered, "short m1\n\nshort m2")
	}

	// Message-count cap (> maxOutboundMsgsPerThread) appends truncation suffix.
	for i := range maxOutboundMsgsPerThread + 10 {
		router.NoteOutbound("C0:many", fmt.Sprintf("m-%d", i), fmt.Sprintf("line %d", i))
	}
	router.mu.Lock()
	manyThread := router.outbound["C0:many"]
	manyCount := len(manyThread.msgs)
	manyRendered := manyThread.render()
	router.mu.Unlock()
	if manyCount != maxOutboundMsgsPerThread {
		t.Errorf("len(msgs) = %d, want %d", manyCount, maxOutboundMsgsPerThread)
	}
	if !strings.HasSuffix(manyRendered, outboundTruncatedSuffix) {
		t.Errorf("message-count capped render missing %q suffix", outboundTruncatedSuffix)
	}

	// Reaper prunes unvisited outbound threads older than dormantMaxAge.
	router.reapIdle(time.Now().UTC().Add(dormantMaxAge + time.Hour))
	router.mu.Lock()
	remOutbound := len(router.outbound)
	remOrder := len(router.outboundOrder)
	router.mu.Unlock()
	if remOutbound != 0 || remOrder != 0 {
		t.Errorf("after dormantMaxAge sweep: outbound=%d order=%d, want 0/0", remOutbound, remOrder)
	}
}
