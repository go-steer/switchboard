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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/switchboard/pkg/chat"
)

// fittingSender is a stubSender that also reports a single-message limit
// (chat.TextFitter), which is what makes append available. A plain stubSender
// deliberately does not, so the "platform cannot append" path stays covered
// by every other test in the package.
type fittingSender struct {
	stubSender
	limit int
}

func (f *fittingSender) FitsOneMessage(text string) bool { return len(text) <= f.limit }

// postFor posts a message through the ingress and returns its ref, so append
// tests start from a message the ingress actually remembers.
func postFor(t *testing.T, i *ingress, conversation, text string) messageResponse {
	t.Helper()
	body, err := json.Marshal(messageRequest{Conversation: conversation, Text: text})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	w := do(t, i, http.MethodPost, string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("seed POST = %d %s, want 200", w.Code, w.Body)
	}
	var res messageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode seed response: %v (%s)", err, w.Body)
	}
	return res
}

// appendTo issues one append against a ref.
func appendReq(t *testing.T, i *ingress, ref messageResponse, add string, opts ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(messageRequest{Conversation: ref.Conversation, ID: ref.ID, Append: add})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return do(t, i, http.MethodPatch, string(body), opts...)
}

// TestIngressAppendExtends checks the growing-timeline case: each append edits
// the one message, adding a line to what is already there rather than
// replacing it.
func TestIngressAppendExtends(t *testing.T) {
	out := &fittingSender{limit: 4000}
	i := newTestIngress(t, out)
	ref := postFor(t, i, "C0123", "🚨 disk-pressure on node-7")

	for _, add := range []string{"• 12:04 kubelet restarted", "• 12:09 reclaimed 40Gi"} {
		if w := appendReq(t, i, ref, add); w.Code != http.StatusNoContent {
			t.Fatalf("append %q = %d %s, want 204", add, w.Code, w.Body)
		}
	}

	calls := out.updateCalls()
	if len(calls) != 2 {
		t.Fatalf("made %d edits, want 2: %+v", len(calls), calls)
	}
	want := "🚨 disk-pressure on node-7\n• 12:04 kubelet restarted\n• 12:09 reclaimed 40Gi"
	if calls[1].text != want {
		t.Errorf("final message =\n%q\nwant\n%q", calls[1].text, want)
	}
	if calls[1].ref.ID != ref.ID {
		t.Errorf("edited %q, want the original message %q", calls[1].ref.ID, ref.ID)
	}
	// Appending edits; it never posts a second message.
	if n := out.sends.Load(); n != 1 {
		t.Errorf("posted %d messages, want 1", n)
	}
}

// TestIngressAppendAfterReplace checks a full-text PATCH resets what append
// extends — a replace is the caller declaring the new whole message.
func TestIngressAppendAfterReplace(t *testing.T) {
	out := &fittingSender{limit: 4000}
	i := newTestIngress(t, out)
	ref := postFor(t, i, "C0123", "first")

	body := fmt.Sprintf(`{"conversation":%q,"id":%q,"text":"rewritten"}`, ref.Conversation, ref.ID)
	if w := do(t, i, http.MethodPatch, body); w.Code != http.StatusNoContent {
		t.Fatalf("replace = %d %s, want 204", w.Code, w.Body)
	}
	if w := appendReq(t, i, ref, "added"); w.Code != http.StatusNoContent {
		t.Fatalf("append = %d %s, want 204", w.Code, w.Body)
	}

	calls := out.updateCalls()
	if got, want := calls[len(calls)-1].text, "rewritten\nadded"; got != want {
		t.Errorf("final message = %q, want %q", got, want)
	}
}

// TestIngressAppendUnknownMessage checks the honest failure: the ingress does
// not know what a message it never posted says, so it says so with 409 rather
// than guessing or silently replacing the body.
func TestIngressAppendUnknownMessage(t *testing.T) {
	i := newTestIngress(t, &fittingSender{limit: 4000})
	w := appendReq(t, i, messageResponse{Conversation: "C0123", ID: "ts-from-another-process"}, "line")
	if w.Code != http.StatusConflict {
		t.Fatalf("append to an unknown message = %d %s, want 409", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "full text") {
		t.Errorf("409 body does not tell the caller what to do instead: %s", w.Body)
	}
}

// TestIngressAppendUnsupportedPlatform checks append is refused outright,
// rather than half-working, on an egress that cannot report its message limit.
func TestIngressAppendUnsupportedPlatform(t *testing.T) {
	out := &stubSender{} // no chat.TextFitter
	i := newTestIngress(t, out)
	ref := postFor(t, i, "C0123", "seed")
	w := appendReq(t, i, ref, "line")
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("append on a non-fitting egress = %d %s, want 501", w.Code, w.Body)
	}
	if calls := out.updateCalls(); len(calls) != 0 {
		t.Errorf("refused append still edited the message: %+v", calls)
	}
}

// TestIngressAppendNotTrackedWhenSplit checks a post too long for one message
// is not remembered: the adapter split it and the ref names only the first
// part, so appending would quietly edit a fragment.
func TestIngressAppendNotTrackedWhenSplit(t *testing.T) {
	out := &fittingSender{limit: 8}
	i := newTestIngress(t, out)
	ref := postFor(t, i, "C0123", "far too long for one message")
	if w := appendReq(t, i, ref, "line"); w.Code != http.StatusConflict {
		t.Fatalf("append to a split message = %d %s, want 409", w.Code, w.Body)
	}
}

// TestIngressAppendRollsOver checks the overflow path: once the message is
// full the ingress posts a continuation in the message's own thread and hands
// back its ref, so the timeline keeps going instead of erroring or truncating.
func TestIngressAppendRollsOver(t *testing.T) {
	out := &fittingSender{limit: 20}
	i := newTestIngress(t, out)
	ref := postFor(t, i, "C0123", "0123456789") // 10 of 20 used

	// 10 + "\n" + 10 = 21, one past the limit.
	w := appendReq(t, i, ref, "abcdefghij")
	if w.Code != http.StatusOK {
		t.Fatalf("overflowing append = %d %s, want 200", w.Code, w.Body)
	}
	var cont messageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &cont); err != nil {
		t.Fatalf("decode rollover response: %v (%s)", err, w.Body)
	}
	if cont.Conversation != "C0123:"+ref.ID {
		t.Errorf("continuation posted to %q, want the thread the message roots (C0123:%s)", cont.Conversation, ref.ID)
	}
	if cont.ID == ref.ID {
		t.Error("rollover returned the original message, not a new one")
	}
	if n := out.sends.Load(); n != 2 {
		t.Errorf("posted %d messages, want 2 (original + continuation)", n)
	}
	if calls := out.updateCalls(); len(calls) != 0 {
		t.Errorf("rollover edited the full message instead of continuing: %+v", calls)
	}
	// The continuation is itself appendable, so the caller just keeps going.
	if w := appendReq(t, i, cont, "next"); w.Code != http.StatusNoContent {
		t.Fatalf("append to the continuation = %d %s, want 204", w.Code, w.Body)
	}
}

// TestIngressAppendRollsOverInsideThread checks a message that already lives
// in a thread continues in that same thread, not in a new one.
func TestIngressAppendRollsOverInsideThread(t *testing.T) {
	out := &fittingSender{limit: 20}
	i := newTestIngress(t, out)
	ref := postFor(t, i, "C0123:ts-root", "0123456789")

	// 10 + "\n" + 10 = 21, one past the limit.
	w := appendReq(t, i, ref, "abcdefghij")
	if w.Code != http.StatusOK {
		t.Fatalf("overflowing append = %d %s, want 200", w.Code, w.Body)
	}
	sent := out.sentReplies()
	if len(sent) != 2 || sent[1].Conversation != "C0123:ts-root" {
		t.Errorf("continuation went to %+v, want the existing thread C0123:ts-root", sent)
	}
}

// threadingSender is a Google Chat-shaped egress: a post into a bare space is
// assigned a thread by the platform, and the ref names it (the adapter's
// landedKey). Its ids are message resource names, which is what makes the
// distinction matter — unlike a Slack thread_ts, an id here is not a thread and
// must never be used as one.
type threadingSender struct {
	fittingSender
	space string
}

func (s *threadingSender) Send(ctx context.Context, r chat.Reply) (chat.MessageRef, error) {
	ref, err := s.fittingSender.Send(ctx, r)
	if err != nil || ref.ID == "" {
		return ref, err
	}
	ref.ID = s.space + "/messages/" + ref.ID
	if !strings.Contains(strings.TrimSuffix(r.Conversation, ":"), ":") {
		ref.Conversation = s.space + ":" + s.space + "/threads/assigned"
	}
	return ref, nil
}

// TestIngressAppendRollsOverOnGoogleChat walks the whole Chat-shaped round trip
// an alert makes: post to a bare space, append until it overflows, keep
// appending to the continuation. It is a shape test, not the regression test
// for #39 — a caller that follows the ref it was handed never reaches the
// branch that was broken, and this would have passed before the fix too. The
// two tests below are the ones that fail on pre-fix code.
func TestIngressAppendRollsOverOnGoogleChat(t *testing.T) {
	const space = "spaces/AAA"
	out := &threadingSender{fittingSender: fittingSender{limit: 20}, space: space}
	i := newTestIngress(t, out)

	ref := postFor(t, i, space, "0123456789") // 10 of 20 used
	wantThread := space + ":" + space + "/threads/assigned"
	if ref.Conversation != wantThread {
		t.Fatalf("POST answered %q, want the thread it landed in (%q)", ref.Conversation, wantThread)
	}

	// 10 + "\n" + 10 = 21, one past the limit.
	w := appendReq(t, i, ref, "abcdefghij")
	if w.Code != http.StatusOK {
		t.Fatalf("overflowing append = %d %s, want 200", w.Code, w.Body)
	}
	var cont messageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &cont); err != nil {
		t.Fatalf("decode rollover response: %v (%s)", err, w.Body)
	}

	sent := out.sentReplies()
	if len(sent) != 2 {
		t.Fatalf("posted %d messages, want 2 (original + continuation)", len(sent))
	}
	if sent[1].Conversation != wantThread {
		t.Errorf("continuation went to %q, want the thread the original landed in (%q)",
			sent[1].Conversation, wantThread)
	}
	if strings.Contains(sent[1].Conversation, "/messages/") {
		t.Errorf("continuation key %q names a message where a thread belongs", sent[1].Conversation)
	}
	// And the caller can keep appending to what it was handed back.
	if w := appendReq(t, i, cont, "next"); w.Code != http.StatusNoContent {
		t.Fatalf("append to the continuation = %d %s, want 204", w.Code, w.Body)
	}
}

// TestIngressAppendRollsOverFromTheKeyItWasPostedTo checks the rollover uses
// where the adapter said the message went, not what the request addressed it
// by. The two differ on purpose: bodyKey drops the thread part so "a caller
// need not echo the exact conversation string it posted with", so an append can
// arrive naming the bare space. Rebuilding the thread from that request put a
// message resource name in a thread field on Chat — the same malformed key #39
// is about, reached the long way round.
func TestIngressAppendRollsOverFromTheKeyItWasPostedTo(t *testing.T) {
	const space = "spaces/AAA"
	out := &threadingSender{fittingSender: fittingSender{limit: 20}, space: space}
	i := newTestIngress(t, out)

	ref := postFor(t, i, space, "0123456789")
	// The caller appends with the space it posted to, not the thread key it was
	// handed back. Both address the same message, and the ingress says so.
	bare := messageResponse{Conversation: space, ID: ref.ID}

	w := appendReq(t, i, bare, "abcdefghij") // 10 + "\n" + 10 = 21, one over
	if w.Code != http.StatusOK {
		t.Fatalf("overflowing append = %d %s, want 200", w.Code, w.Body)
	}
	sent := out.sentReplies()
	if len(sent) != 2 {
		t.Fatalf("posted %d messages, want 2 (original + continuation)", len(sent))
	}
	wantThread := space + ":" + space + "/threads/assigned"
	if sent[1].Conversation != wantThread {
		t.Errorf("continuation went to %q, want the thread the original landed in (%q)",
			sent[1].Conversation, wantThread)
	}
	if strings.Contains(sent[1].Conversation, "/messages/") {
		t.Errorf("continuation key %q names a message where a thread belongs", sent[1].Conversation)
	}
}

// TestIngressAppendRollsOverAfterAReplace checks a replace does not overwrite
// where the ingress knows the message lives. A PATCH names the message by
// whatever key the caller has, and an edit is the caller restating the text,
// not correcting its address.
func TestIngressAppendRollsOverAfterAReplace(t *testing.T) {
	const space = "spaces/AAA"
	out := &threadingSender{fittingSender: fittingSender{limit: 20}, space: space}
	i := newTestIngress(t, out)

	ref := postFor(t, i, space, "seed")
	bare := messageResponse{Conversation: space, ID: ref.ID}

	body, err := json.Marshal(messageRequest{Conversation: space, ID: ref.ID, Text: "0123456789"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if w := do(t, i, http.MethodPatch, string(body)); w.Code != http.StatusNoContent {
		t.Fatalf("replace = %d %s, want 204", w.Code, w.Body)
	}
	if w := appendReq(t, i, bare, "abcdefghij"); w.Code != http.StatusOK {
		t.Fatalf("overflowing append = %d %s, want 200", w.Code, w.Body)
	}

	sent := out.sentReplies()
	wantThread := space + ":" + space + "/threads/assigned"
	if len(sent) != 2 || sent[1].Conversation != wantThread {
		t.Errorf("continuation went to %+v, want the thread the original landed in (%q)",
			sent, wantThread)
	}
}

// TestIngressAppendRollsOverIntoTheThreadTheRequestNames checks the remembered
// conversation does not shout down a better one on the request. For a message
// this ingress posted, what it remembers came from the adapter and names the
// thread. For one it first saw through a PATCH — a message posted by another
// replica, or before a restart — what it remembers is only what that caller
// typed, which may be a bare channel. Preferring it unconditionally would throw
// away a thread the current request states outright, and send the continuation
// somewhere the original message is not.
func TestIngressAppendRollsOverIntoTheThreadTheRequestNames(t *testing.T) {
	out := &fittingSender{limit: 20}
	i := newTestIngress(t, out)

	// First seen through a PATCH addressed by the bare channel: nothing here
	// came from the adapter, so nothing here knows about the thread.
	const id = "ts-1"
	body, err := json.Marshal(messageRequest{Conversation: "C0123", ID: id, Text: "0123456789"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if w := do(t, i, http.MethodPatch, string(body)); w.Code != http.StatusNoContent {
		t.Fatalf("replace = %d %s, want 204", w.Code, w.Body)
	}

	// Now the caller appends and does say which thread the message is in.
	threaded := messageResponse{Conversation: "C0123:ts-root", ID: id}
	if w := appendReq(t, i, threaded, "abcdefghij"); w.Code != http.StatusOK {
		t.Fatalf("overflowing append = %d %s, want 200", w.Code, w.Body)
	}

	sent := out.sentReplies()
	if len(sent) != 1 {
		t.Fatalf("posted %d continuations, want 1", len(sent))
	}
	if sent[0].Conversation != "C0123:ts-root" {
		t.Errorf("continuation went to %q, want the thread the request named (C0123:ts-root)",
			sent[0].Conversation)
	}
}

// TestIngressAppendRollsOverFromATrailingColon checks a key that ends in a
// colon is read as naming no thread, which is what both adapters' egress does
// with it. Testing "contains a colon" instead would take the key at face value
// and post the continuation at the top level of the channel, scattering the
// timeline away from the message it continues.
func TestIngressAppendRollsOverFromATrailingColon(t *testing.T) {
	out := &fittingSender{limit: 20}
	i := newTestIngress(t, out)

	// Addressed by PATCH, since no adapter hands back a key shaped like this;
	// a caller holding one does.
	const id = "ts-1"
	body, err := json.Marshal(messageRequest{Conversation: "C0123:", ID: id, Text: "0123456789"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if w := do(t, i, http.MethodPatch, string(body)); w.Code != http.StatusNoContent {
		t.Fatalf("replace = %d %s, want 204", w.Code, w.Body)
	}
	if w := appendReq(t, i, messageResponse{Conversation: "C0123:", ID: id}, "abcdefghij"); w.Code != http.StatusOK {
		t.Fatalf("overflowing append = %d %s, want 200", w.Code, w.Body)
	}

	sent := out.sentReplies()
	if len(sent) != 1 {
		t.Fatalf("posted %d continuations, want 1", len(sent))
	}
	if sent[0].Conversation != "C0123:"+id {
		t.Errorf("continuation went to %q, want the thread the message roots (C0123:%s)",
			sent[0].Conversation, id)
	}
}

// TestIngressAppendConcurrent checks concurrent appends to one message do not
// lose a line — the read-modify-write is serialized per message, so every
// caller's text survives even when they race.
func TestIngressAppendConcurrent(t *testing.T) {
	out := &fittingSender{limit: 4000}
	i := newTestIngress(t, out)
	ref := postFor(t, i, "C0123", "seed")

	const n = 8
	var wg sync.WaitGroup
	for k := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if w := appendReq(t, i, ref, fmt.Sprintf("line-%d", k)); w.Code != http.StatusNoContent {
				t.Errorf("append %d = %d %s, want 204", k, w.Code, w.Body)
			}
		}()
	}
	wg.Wait()

	calls := out.updateCalls()
	if len(calls) != n {
		t.Fatalf("made %d edits, want %d", len(calls), n)
	}
	final := calls[len(calls)-1].text
	for k := range n {
		if !strings.Contains(final, fmt.Sprintf("line-%d", k)) {
			t.Errorf("line-%d was lost:\n%s", k, final)
		}
	}
}

// TestIngressAppendIsIdempotent checks the verb that most needs a replay key
// honors one: a retried append adds its line once, not twice.
func TestIngressAppendIsIdempotent(t *testing.T) {
	out := &fittingSender{limit: 4000}
	i := newTestIngress(t, out)
	ref := postFor(t, i, "C0123", "seed")

	for range 2 {
		w := appendReq(t, i, ref, "only once", withHeader(idempotencyHeader, "retry-me"))
		if w.Code != http.StatusNoContent {
			t.Fatalf("append = %d %s, want 204", w.Code, w.Body)
		}
	}
	calls := out.updateCalls()
	if len(calls) != 1 {
		t.Fatalf("retried append edited %d times, want 1: %+v", len(calls), calls)
	}
	if got, want := calls[0].text, "seed\nonly once"; got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
}

// TestIngressIdempotencyKeyReuseWithDifferentBody checks a recycled key is
// caught rather than silently replaying the first result — answering 200 with
// a ref to an unrelated message while the caller's own never posted is the
// worst possible failure here.
func TestIngressIdempotencyKeyReuseWithDifferentBody(t *testing.T) {
	out := &stubSender{}
	i := newTestIngress(t, out)

	first := do(t, i, http.MethodPost, `{"conversation":"C0123","text":"nightly digest"}`,
		withHeader(idempotencyHeader, "nightly"))
	if first.Code != http.StatusOK {
		t.Fatalf("first POST = %d %s, want 200", first.Code, first.Body)
	}
	second := do(t, i, http.MethodPost, `{"conversation":"C9999","text":"PAGE: prod is down"}`,
		withHeader(idempotencyHeader, "nightly"))
	if second.Code != http.StatusConflict {
		t.Fatalf("key reuse with a different body = %d %s, want 409", second.Code, second.Body)
	}
	if n := out.sends.Load(); n != 1 {
		t.Errorf("posted %d messages, want 1", n)
	}
}

// TestIngressPermanentPlatformErrors checks a failure the caller can never
// retry its way out of is not dressed up as a 502. An escalation daemon that
// treats 5xx as transient would otherwise retry "no such channel" forever.
func TestIngressPermanentPlatformErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"missing", fmt.Errorf("slack: post to C0123: %w", chat.ErrNotFound), http.StatusNotFound},
		{"denied", fmt.Errorf("slack: post to C0123: %w", chat.ErrDenied), http.StatusForbidden},
		{"unclassified", errors.New("slack: post to C0123: internal_error"), http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := &stubSender{sendErr: tc.err, updErr: tc.err}
			i := newTestIngress(t, out)
			for _, req := range []struct{ method, body string }{
				{http.MethodPost, `{"conversation":"C0123","text":"x"}`},
				{http.MethodPatch, `{"conversation":"C0123","id":"ts1","text":"x"}`},
			} {
				w := do(t, i, req.method, req.body)
				if w.Code != tc.want {
					t.Errorf("%s = %d %s, want %d", req.method, w.Code, w.Body, tc.want)
				}
				if strings.Contains(w.Body.String(), "slack:") {
					t.Errorf("%s leaked the platform error: %s", req.method, w.Body)
				}
			}
		})
	}
}

// TestIngressPanicDoesNotWedgeKey checks a panicking egress does not strand an
// idempotency key with a channel nobody is left to close — every later request
// using that key would block on it forever, leaking a goroutine apiece.
func TestIngressPanicDoesNotWedgeKey(t *testing.T) {
	out := &stubSender{}
	out.sendHook = func() { panic("adapter blew up") }
	i := newTestIngress(t, out)

	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected the panic to propagate to the server's recovery")
			}
		}()
		do(t, i, http.MethodPost, `{"conversation":"C0123","text":"x"}`, withHeader(idempotencyHeader, "wedged"))
	}()

	out.sendHook = nil
	done := make(chan int, 1)
	go func() {
		w := do(t, i, http.MethodPost, `{"conversation":"C0123","text":"x"}`, withHeader(idempotencyHeader, "wedged"))
		done <- w.Code
	}()
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Errorf("retry after a panic = %d, want 200", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a request reusing the panicked key blocked forever")
	}
}

// TestIngressWaiterGivesUp checks a duplicate waiting on an in-flight request
// is not pinned to it: when its own caller goes away it returns, rather than
// holding a connection until the platform answers.
func TestIngressWaiterGivesUp(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	out := &stubSender{}
	out.sendHook = func() { once.Do(func() { <-release }) }
	i := newTestIngress(t, out)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		do(t, i, http.MethodPost, `{"conversation":"C0123","text":"x"}`, withHeader(idempotencyHeader, "slow"))
	}()
	// Unblock the in-flight post before waiting on it — the order matters, or
	// the cleanup deadlocks on the very goroutine it is holding up.
	defer func() {
		close(release)
		wg.Wait()
	}()

	waitFor(t, func() bool { return out.sends.Load() == 1 }, "the first post never reached the egress")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // this caller has already hung up
	req := httptest.NewRequest(http.MethodPost, ingressPath,
		strings.NewReader(`{"conversation":"C0123","text":"x"}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+ingressToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(idempotencyHeader, "slow")

	w := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		i.handler().ServeHTTP(w, req)
		close(served)
	}()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("a waiter whose caller gave up stayed blocked on the in-flight request")
	}
	if w.Code != http.StatusGatewayTimeout {
		t.Errorf("abandoned waiter = %d %s, want 504", w.Code, w.Body)
	}
	if n := out.sends.Load(); n != 1 {
		t.Errorf("the waiter posted its own message (%d sends, want 1)", n)
	}
}

// TestPlatformContextDetaches checks a post is not aborted because its caller
// hung up: the platform may already have committed the message, and cancelling
// mid-flight would leave the outcome unknowable and the retry double-posting.
func TestPlatformContextDetaches(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	ctx, done := platformContext(parent)
	defer done()

	cancel()
	if err := ctx.Err(); err != nil {
		t.Errorf("platform context died with its caller: %v", err)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("platform context has no deadline; a hung platform would park the request forever")
	}
	if d := time.Until(deadline); d > platformTimeout+time.Second {
		t.Errorf("deadline is %v out, want at most %v", d, platformTimeout)
	}
}

// TestIngressAcceptsJSONCharset checks a content type with parameters — what
// most HTTP clients actually send — is not mistaken for a non-JSON body.
func TestIngressAcceptsJSONCharset(t *testing.T) {
	i := newTestIngress(t, &stubSender{})
	w := do(t, i, http.MethodPost, `{"conversation":"C0123","text":"x"}`,
		withHeader("Content-Type", "application/json; charset=utf-8"))
	if w.Code != http.StatusOK {
		t.Fatalf("POST with a charset parameter = %d %s, want 200", w.Code, w.Body)
	}
}
