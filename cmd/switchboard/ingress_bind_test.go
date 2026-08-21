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
	"strings"
	"sync"
	"testing"

	"github.com/go-steer/switchboard/pkg/chat"
	"github.com/go-steer/switchboard/pkg/daemon"
)

// stubBinder stands in for the router's binding half: it records what it was
// asked and answers with whatever the test set.
type stubBinder struct {
	head       int64
	prepareErr error

	mu        sync.Mutex
	prepared  []string // the conversations PrepareBind was asked about
	committed []pendingBind
	commitAt  []string // the conversations CommitBind recorded against
	aborted   []daemon.Session
}

func (b *stubBinder) PrepareBind(_ context.Context, conv string, _ daemon.Session) (int64, error) {
	b.mu.Lock()
	b.prepared = append(b.prepared, conv)
	b.mu.Unlock()
	if b.prepareErr != nil {
		return 0, b.prepareErr
	}
	return b.head, nil
}

func (b *stubBinder) CommitBind(conv string, sess daemon.Session, since int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.commitAt = append(b.commitAt, conv)
	b.committed = append(b.committed, pendingBind{sess: sess, since: since})
}

func (b *stubBinder) AbortBind(sess daemon.Session) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.aborted = append(b.aborted, sess)
}

func (b *stubBinder) calls() (prepared, commitAt []string, committed []pendingBind) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.prepared...), append([]string(nil), b.commitAt...),
		append([]pendingBind(nil), b.committed...)
}

func (b *stubBinder) aborts() []daemon.Session {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]daemon.Session(nil), b.aborted...)
}

// newBindingIngress builds an ingress that can bind, over out.
func newBindingIngress(t *testing.T, out sender, b binder) *ingress {
	t.Helper()
	i, err := newIngress(ingressConfig{
		Token:   ingressToken,
		Out:     out,
		Bind:    b,
		Metrics: newMetrics(),
		Logf:    func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("newIngress: %v", err)
	}
	return i
}

// TestIngressBindsTheThreadTheMessageLandedIn is the seam the whole feature
// turns on. The caller posts to a space; the platform decides which thread
// that becomes; the binding has to name the thread, because that is the
// conversation a reply will arrive on. On Google Chat nobody could have
// predicted it — the thread id is assigned by the platform.
func TestIngressBindsTheThreadTheMessageLandedIn(t *testing.T) {
	out := &stubSender{sendRef: chat.MessageRef{Conversation: "spaces/AAA:spaces/AAA/threads/BBB", ID: "spaces/AAA/messages/CCC"}}
	b := &stubBinder{head: 12}
	i := newBindingIngress(t, out, b)

	w := do(t, i, http.MethodPost, `{"conversation":"spaces/AAA","text":"pods are restarting","session":"core-agent/incident-7"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST = %d %s, want 200", w.Code, w.Body)
	}

	prepared, commitAt, committed := b.calls()
	if len(prepared) != 1 || prepared[0] != "spaces/AAA" {
		t.Errorf("PrepareBind asked about %v, want the conversation the caller addressed", prepared)
	}
	if len(commitAt) != 1 || commitAt[0] != "spaces/AAA:spaces/AAA/threads/BBB" {
		t.Errorf("CommitBind recorded %v, want the thread the message landed in", commitAt)
	}
	if len(committed) != 1 || committed[0].sess != (daemon.Session{App: "core-agent", ID: "incident-7"}) {
		t.Errorf("bound %+v, want core-agent/incident-7", committed)
	}
	if committed[0].since != 12 {
		t.Errorf("since = %d, want the head PrepareBind read (12)", committed[0].since)
	}
}

// TestIngressPostWithoutASessionBindsNothing: the field is optional, and the
// path that does not use it must not have grown a daemon round trip.
func TestIngressPostWithoutASessionBindsNothing(t *testing.T) {
	b := &stubBinder{head: 3}
	i := newBindingIngress(t, &stubSender{}, b)

	if w := do(t, i, http.MethodPost, `{"conversation":"C0123","text":"just a digest"}`); w.Code != http.StatusOK {
		t.Fatalf("POST = %d %s, want 200", w.Code, w.Body)
	}
	if prepared, commitAt, _ := b.calls(); len(prepared) != 0 || len(commitAt) != 0 {
		t.Errorf("an unbound post touched the binder: prepared=%v committed=%v", prepared, commitAt)
	}
}

// TestIngressRefusesASessionWithNoInboundPath. An outbound-only deployment has
// no router and no way for a reply to arrive; accepting the field would leave
// the caller believing the thread was a conversation.
func TestIngressRefusesASessionWithNoInboundPath(t *testing.T) {
	out := &stubSender{}
	i := newTestIngress(t, out) // no binder: --outbound-only

	w := do(t, i, http.MethodPost, `{"conversation":"C0123","text":"pods are restarting","session":"core-agent/incident-7"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("POST = %d %s, want 400", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "outbound-only") {
		t.Errorf("body = %s, want it to name the reason", w.Body)
	}
	if got := out.sends.Load(); got != 0 {
		t.Errorf("sends = %d; a refused bind must not post", got)
	}
}

// TestIngressRefusesAMalformedSession, before posting: the caller can fix the
// reference and retry, which is only true if nothing went out.
func TestIngressRefusesAMalformedSession(t *testing.T) {
	for _, bad := range []string{"incident-7", "core-agent/", "core-agent/../x", "core-agent/a?b"} {
		out := &stubSender{}
		i := newBindingIngress(t, out, &stubBinder{})
		body := `{"conversation":"C0123","text":"hi","session":"` + bad + `"}`
		w := do(t, i, http.MethodPost, body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("POST session=%q = %d %s, want 400", bad, w.Code, w.Body)
		}
		if got := out.sends.Load(); got != 0 {
			t.Errorf("POST session=%q posted anyway", bad)
		}
	}
}

// TestIngressBindRefusalsMapToStatuses. Each of these is a different thing for
// the caller to do about it, so each gets its own code: fix the request, or
// look at the agent backend.
func TestIngressBindRefusalsMapToStatuses(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		want     int
		wantBody string
	}{
		{"conversation taken", errConversationBound, http.StatusConflict, "already has an agent session"},
		{"session taken", errSessionBound, http.StatusConflict, "already bound to another conversation"},
		{"no such session", &daemon.StatusError{StatusCode: http.StatusNotFound, Message: "gone"},
			http.StatusNotFound, "no session"},
		{"daemon down", &daemon.StatusError{StatusCode: http.StatusServiceUnavailable, Message: "later"},
			http.StatusBadGateway, "would not confirm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := &stubSender{}
			i := newBindingIngress(t, out, &stubBinder{prepareErr: tc.err})

			w := do(t, i, http.MethodPost, `{"conversation":"C0123","text":"hi","session":"core-agent/incident-7"}`)
			if w.Code != tc.want {
				t.Fatalf("POST = %d %s, want %d", w.Code, w.Body, tc.want)
			}
			if !strings.Contains(w.Body.String(), tc.wantBody) {
				t.Errorf("body = %s, want it to mention %q", w.Body, tc.wantBody)
			}
			if got := out.sends.Load(); got != 0 {
				t.Errorf("sends = %d; a refused bind must not post", got)
			}
		})
	}
}

// TestIngressDoesNotBindAMessageThatNeverPosted: the binding names the thread
// the message landed in, and there is no thread.
func TestIngressDoesNotBindAMessageThatNeverPosted(t *testing.T) {
	out := &stubSender{sendErr: chat.ErrNotFound}
	b := &stubBinder{head: 4}
	i := newBindingIngress(t, out, b)

	if w := do(t, i, http.MethodPost, `{"conversation":"C0123","text":"hi","session":"core-agent/incident-7"}`); w.Code != http.StatusNotFound {
		t.Fatalf("POST = %d %s, want 404 from the platform", w.Code, w.Body)
	}
	if _, commitAt, _ := b.calls(); len(commitAt) != 0 {
		t.Errorf("bound %v after a failed post", commitAt)
	}
	// And the reservation the prepare took is given back, or the session could
	// never be bound again without a restart.
	if got := b.aborts(); len(got) != 1 {
		t.Errorf("AbortBind ran %d times, want 1", len(got))
	}
}

// TestIngressPatchRefusesASession. An edit is not where a thread is decided,
// and a PATCH addressed to a bare channel does not name one.
func TestIngressPatchRefusesASession(t *testing.T) {
	out := &stubSender{}
	i := newBindingIngress(t, out, &stubBinder{})

	w := do(t, i, http.MethodPatch, `{"conversation":"C0123","id":"ts1","text":"revised","session":"core-agent/incident-7"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("PATCH = %d %s, want 400", w.Code, w.Body)
	}
	if len(out.updateCalls()) != 0 {
		t.Error("the edit went through anyway")
	}
}

// TestIngressReplayedPostBindsOnce: a scheduler retrying a request it never
// saw the response to gets the original outcome, and the thread is not rebound
// behind it.
func TestIngressReplayedPostBindsOnce(t *testing.T) {
	out := &stubSender{}
	b := &stubBinder{head: 7}
	i := newBindingIngress(t, out, b)
	body := `{"conversation":"C0123","text":"hi","session":"core-agent/incident-7"}`

	for range 2 {
		if w := do(t, i, http.MethodPost, body, withHeader(idempotencyHeader, "k1")); w.Code != http.StatusOK {
			t.Fatalf("POST = %d %s, want 200", w.Code, w.Body)
		}
	}
	if got := out.sends.Load(); got != 1 {
		t.Errorf("sends = %d, want 1", got)
	}
	if _, commitAt, _ := b.calls(); len(commitAt) != 1 {
		t.Errorf("CommitBind ran %d times, want 1", len(commitAt))
	}
}

// TestIngressIdempotencyKeyCoversTheSession: reusing a key for a post that
// binds a *different* session is the caller making a mistake, not asking for
// the same thing twice.
func TestIngressIdempotencyKeyCoversTheSession(t *testing.T) {
	i := newBindingIngress(t, &stubSender{}, &stubBinder{head: 1})

	first := `{"conversation":"C0123","text":"hi","session":"core-agent/incident-7"}`
	if w := do(t, i, http.MethodPost, first, withHeader(idempotencyHeader, "k1")); w.Code != http.StatusOK {
		t.Fatalf("POST = %d %s, want 200", w.Code, w.Body)
	}
	second := `{"conversation":"C0123","text":"hi","session":"core-agent/incident-8"}`
	if w := do(t, i, http.MethodPost, second, withHeader(idempotencyHeader, "k1")); w.Code != http.StatusConflict {
		t.Fatalf("POST = %d %s, want 409", w.Code, w.Body)
	}
}

// TestIngressReplayedPostReplaysTheOriginalOutcome runs the retry against the
// real binder, over a ref shaped like Google Chat's: the message lands in a
// thread the caller could not have named, so the binding is recorded under a
// conversation the request does not mention. A retry that re-checked the bind
// would find that thread holding the session and refuse — turning the one case
// idempotency exists for, a caller that never saw its answer, into a 409 it
// cannot tell from a real conflict.
func TestIngressReplayedPostReplaysTheOriginalOutcome(t *testing.T) {
	const thread = "spaces/AAA:spaces/AAA/threads/BBB"
	router := probeOnlyRouter(t)
	out := &stubSender{sendRef: chat.MessageRef{Conversation: thread, ID: "spaces/AAA/messages/CCC"}}
	i := newBindingIngress(t, out, router)
	body := `{"conversation":"spaces/AAA","text":"rollout is wedged","session":"core-agent/incident-7"}`

	var first string
	for n := range 2 {
		w := do(t, i, http.MethodPost, body, withHeader(idempotencyHeader, "k1"))
		if w.Code != http.StatusOK {
			t.Fatalf("POST %d = %d %s, want 200", n+1, w.Code, w.Body)
		}
		if n == 0 {
			first = w.Body.String()
		} else if w.Body.String() != first {
			t.Errorf("retry answered %s, want the original %s", w.Body, first)
		}
	}
	if got := out.sends.Load(); got != 1 {
		t.Errorf("sends = %d, want 1", got)
	}

	router.mu.Lock()
	defer router.mu.Unlock()
	if len(router.bindings) != 1 {
		t.Errorf("bindings = %v, want exactly the one thread", router.bindings)
	}
	if _, ok := router.bindings[thread]; !ok {
		t.Errorf("bindings = %v, want the thread the message landed in", router.bindings)
	}
	if len(router.reserving) != 0 {
		t.Errorf("reserving = %v, want nothing held after the request", router.reserving)
	}
}

// TestIngressRePostingToTheBoundThreadIsAccepted is the pattern the docs tell
// callers to use: follow up with the conversation the first post answered with.
// It has to keep working without an idempotency key, because the second update
// is a different message.
func TestIngressRePostingToTheBoundThreadIsAccepted(t *testing.T) {
	const thread = "spaces/AAA:spaces/AAA/threads/BBB"
	router := probeOnlyRouter(t)
	out := &stubSender{sendRef: chat.MessageRef{Conversation: thread, ID: "spaces/AAA/messages/CCC"}}
	i := newBindingIngress(t, out, router)

	if w := do(t, i, http.MethodPost,
		`{"conversation":"spaces/AAA","text":"rollout is wedged","session":"core-agent/incident-7"}`); w.Code != http.StatusOK {
		t.Fatalf("first POST = %d %s, want 200", w.Code, w.Body)
	}
	if w := do(t, i, http.MethodPost,
		`{"conversation":"`+thread+`","text":"still wedged","session":"core-agent/incident-7"}`); w.Code != http.StatusOK {
		t.Fatalf("second POST to the bound thread = %d %s, want 200", w.Code, w.Body)
	}
	if got := out.sends.Load(); got != 2 {
		t.Errorf("sends = %d, want 2", got)
	}
}

// TestIngressRefusesABareSpaceRePostAndSaysWhere. The same caller addressing
// the space again is a conflict — its first post created a thread, and that
// thread holds the session — so the update is refused rather than posted where
// no reply could reach the agent. The refusal names the thread, which is the
// caller's way out of it.
func TestIngressRefusesABareSpaceRePostAndSaysWhere(t *testing.T) {
	const thread = "spaces/AAA:spaces/AAA/threads/BBB"
	router := probeOnlyRouter(t)
	out := &stubSender{sendRef: chat.MessageRef{Conversation: thread, ID: "spaces/AAA/messages/CCC"}}
	i := newBindingIngress(t, out, router)
	body := `{"conversation":"spaces/AAA","text":"an update","session":"core-agent/incident-7"}`

	if w := do(t, i, http.MethodPost, body); w.Code != http.StatusOK {
		t.Fatalf("first POST = %d %s, want 200", w.Code, w.Body)
	}
	w := do(t, i, http.MethodPost, body)
	if w.Code != http.StatusConflict {
		t.Fatalf("second POST = %d %s, want 409", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), thread) {
		t.Errorf("body = %s, want it to name %q", w.Body, thread)
	}
	if got := out.sends.Load(); got != 1 {
		t.Errorf("sends = %d; the refused post went out anyway", got)
	}
}

// TestIngressRefusesAnOversizedSession without quoting a megabyte of it back.
func TestIngressRefusesAnOversizedSession(t *testing.T) {
	out := &stubSender{}
	i := newBindingIngress(t, out, &stubBinder{})
	huge := "core-agent/" + strings.Repeat("s", maxSessionRefLen)

	w := do(t, i, http.MethodPost, `{"conversation":"C0123","text":"hi","session":"`+huge+`"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("POST = %d, want 400", w.Code)
	}
	if w.Body.Len() > 512 {
		t.Errorf("refusal is %d bytes; it echoed the session back", w.Body.Len())
	}
	if got := out.sends.Load(); got != 0 {
		t.Errorf("sends = %d, want 0", got)
	}
}

// TestAPanickingPostStillGivesTheReservationBack. A reservation is held across
// the post so two callers cannot bind one session at once, and it is only ever
// released on the way out of the handler. A panic — a nil map in a platform
// client, a bad type assertion on a response — leaves by a route that is not
// the way out, and the session it reserved would then be unbindable for the
// life of the process, by anyone, with nothing anywhere saying why.
func TestAPanickingPostStillGivesTheReservationBack(t *testing.T) {
	out := &stubSender{sendHook: func() { panic("the platform client blew up") }}
	b := &stubBinder{head: 4}
	i := newBindingIngress(t, out, b)

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("Send did not panic; this test is no longer testing anything")
			}
		}()
		do(t, i, http.MethodPost, `{"conversation":"C0123","text":"hi","session":"core-agent/incident-7"}`)
	}()

	if got := b.aborts(); len(got) != 1 {
		t.Fatalf("AbortBind ran %d times, want 1: the reservation outlived the request", len(got))
	}
	if _, commitAt, _ := b.calls(); len(commitAt) != 0 {
		t.Errorf("bound %v after a panic", commitAt)
	}
}
