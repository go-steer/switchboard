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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/go-steer/switchboard/internal/logging"
	"github.com/go-steer/switchboard/pkg/chat"
)

const ingressToken = "tok_ingress"

// stubSender is a scriptable chat egress: it records every call and lets a
// test decide what Send/Update return. Unlike router_test's fakeSender it
// never blocks, since the ingress path is synchronous with the HTTP request.
type stubSender struct {
	mu       sync.Mutex
	sent     []chat.Reply
	updates  []fakeUpdate
	sendErr  error
	sendRef  chat.MessageRef // zero => derive a ref from the reply
	updErr   error
	sendHook func() // optional: runs inside Send, for concurrency tests
	sends    atomic.Int64
}

func (s *stubSender) Send(_ context.Context, r chat.Reply) (chat.MessageRef, error) {
	s.sends.Add(1)
	if s.sendHook != nil {
		s.sendHook()
	}
	s.mu.Lock()
	s.sent = append(s.sent, r)
	n := len(s.sent)
	s.mu.Unlock()
	if s.sendErr != nil {
		return chat.MessageRef{}, s.sendErr
	}
	if s.sendRef != (chat.MessageRef{}) {
		return s.sendRef, nil
	}
	return chat.MessageRef{Conversation: r.Conversation, ID: fmt.Sprintf("ts%d", n)}, nil
}

func (s *stubSender) Update(_ context.Context, ref chat.MessageRef, r chat.Reply) error {
	s.mu.Lock()
	s.updates = append(s.updates, fakeUpdate{ref: ref, text: r.Text})
	s.mu.Unlock()
	return s.updErr
}

func (s *stubSender) Delete(context.Context, chat.MessageRef) error { return nil }

func (s *stubSender) sentReplies() []chat.Reply {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]chat.Reply(nil), s.sent...)
}

func (s *stubSender) updateCalls() []fakeUpdate {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]fakeUpdate(nil), s.updates...)
}

// newTestIngress builds an ingress over out with the given allowlist.
func newTestIngress(t *testing.T, out sender, allow ...string) *ingress {
	t.Helper()
	i, err := newIngress(ingressConfig{
		Token:   ingressToken,
		Allow:   allow,
		Out:     out,
		Metrics: newMetrics(),
	})
	if err != nil {
		t.Fatalf("newIngress: %v", err)
	}
	return i
}

// do issues one request against the ingress handler and returns the recorder.
func do(t *testing.T, i *ingress, method, body string, opts ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, ingressPath, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+ingressToken)
	req.Header.Set("Content-Type", "application/json")
	for _, o := range opts {
		o(req)
	}
	w := httptest.NewRecorder()
	i.handler().ServeHTTP(w, req)
	return w
}

func withHeader(k, v string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set(k, v) }
}

// TestIngressPostPosts checks the happy path: the message reaches the adapter
// verbatim and the caller gets back the ref it needs to edit it later.
func TestIngressPostPosts(t *testing.T) {
	out := &stubSender{}
	i := newTestIngress(t, out)

	w := do(t, i, http.MethodPost, `{"conversation":"C0123","text":"disk is filling up"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST = %d %s, want 200", w.Code, w.Body)
	}
	var got messageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v (%s)", err, w.Body)
	}
	if got.Conversation != "C0123" || got.ID != "ts1" {
		t.Errorf("response = %+v, want {C0123 ts1}", got)
	}
	sent := out.sentReplies()
	if len(sent) != 1 || sent[0].Conversation != "C0123" || sent[0].Text != "disk is filling up" {
		t.Errorf("sent = %+v, want one reply to C0123", sent)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

// TestIngressPatchEdits checks PATCH edits the ref'd message in place — the
// post-then-edit pattern a slow escalation depends on.
func TestIngressPatchEdits(t *testing.T) {
	out := &stubSender{}
	i := newTestIngress(t, out)

	w := do(t, i, http.MethodPatch, `{"conversation":"C0123","id":"ts1","text":"final assessment"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("PATCH = %d %s, want 204", w.Code, w.Body)
	}
	if w.Body.Len() != 0 {
		t.Errorf("204 carried a body: %s", w.Body)
	}
	calls := out.updateCalls()
	if len(calls) != 1 {
		t.Fatalf("Update calls = %d, want 1", len(calls))
	}
	if calls[0].ref != (chat.MessageRef{Conversation: "C0123", ID: "ts1"}) || calls[0].text != "final assessment" {
		t.Errorf("Update(%+v, %q), want ref {C0123 ts1} text \"final assessment\"", calls[0].ref, calls[0].text)
	}
}

// TestIngressPatchUnsupported checks a platform that cannot edit surfaces as
// 501 rather than a generic failure, so a caller can degrade to posting again.
func TestIngressPatchUnsupported(t *testing.T) {
	out := &stubSender{updErr: chat.ErrUnsupported}
	i := newTestIngress(t, out)

	w := do(t, i, http.MethodPatch, `{"conversation":"C0123","id":"ts1","text":"x"}`)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("PATCH = %d %s, want 501", w.Code, w.Body)
	}
}

// TestIngressPlatformFailures checks a platform error becomes a 502 and never
// leaks the underlying message to the caller.
func TestIngressPlatformFailures(t *testing.T) {
	out := &stubSender{sendErr: errors.New("slack: channel_not_found"), updErr: errors.New("slack: message_not_found")}
	i := newTestIngress(t, out)

	for _, tc := range []struct{ method, body string }{
		{http.MethodPost, `{"conversation":"C0123","text":"x"}`},
		{http.MethodPatch, `{"conversation":"C0123","id":"ts1","text":"x"}`},
	} {
		w := do(t, i, tc.method, tc.body)
		if w.Code != http.StatusBadGateway {
			t.Errorf("%s = %d %s, want 502", tc.method, w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), "not_found") {
			t.Errorf("%s leaked the platform error: %s", tc.method, w.Body)
		}
	}
}

// TestIngressPostNothingPosted checks an adapter that posts nothing (a body
// that renders away) is an error, not a 200 with an unusable ref.
func TestIngressPostNothingPosted(t *testing.T) {
	i := newTestIngress(t, &emptyRefSender{})
	w := do(t, i, http.MethodPost, `{"conversation":"C0123","text":"x"}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("POST = %d %s, want 502", w.Code, w.Body)
	}
}

// emptyRefSender reports success but hands back no message ref — what an
// adapter does when the body renders away to nothing.
type emptyRefSender struct{ stubSender }

func (s *emptyRefSender) Send(context.Context, chat.Reply) (chat.MessageRef, error) {
	return chat.MessageRef{}, nil
}

// TestIngressAuth checks every unauthenticated shape is refused before the
// adapter is ever touched.
func TestIngressAuth(t *testing.T) {
	cases := []struct {
		name, header string
	}{
		{"missing", ""},
		{"wrong token", "Bearer nope"},
		{"empty token", "Bearer "},
		{"not bearer", "Basic " + ingressToken},
		{"bare token", ingressToken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := &stubSender{}
			i := newTestIngress(t, out)
			req := httptest.NewRequest(http.MethodPost, ingressPath,
				strings.NewReader(`{"conversation":"C0123","text":"x"}`))
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			w := httptest.NewRecorder()
			i.handler().ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d %s, want 401", w.Code, w.Body)
			}
			if got := w.Header().Get("WWW-Authenticate"); got != "Bearer" {
				t.Errorf("WWW-Authenticate = %q, want Bearer", got)
			}
			if n := out.sends.Load(); n != 0 {
				t.Errorf("unauthorized request reached the adapter (%d sends)", n)
			}
		})
	}
}

// TestIngressAuthCaseInsensitiveScheme checks the auth scheme is matched
// case-insensitively, as RFC 7235 requires, while the token is not.
func TestIngressAuthCaseInsensitiveScheme(t *testing.T) {
	i := newTestIngress(t, &stubSender{})
	w := do(t, i, http.MethodPost, `{"conversation":"C0123","text":"x"}`,
		withHeader("Authorization", "bearer "+ingressToken))
	if w.Code != http.StatusOK {
		t.Fatalf("lowercase scheme = %d %s, want 200", w.Code, w.Body)
	}
	w = do(t, i, http.MethodPost, `{"conversation":"C0123","text":"x"}`,
		withHeader("Authorization", "Bearer "+strings.ToUpper(ingressToken)))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("uppercased token = %d, want 401", w.Code)
	}
}

// TestIngressAllowlist checks an allowlisted channel admits both the channel
// itself and its threads, and that everything else is refused.
func TestIngressAllowlist(t *testing.T) {
	out := &stubSender{}
	i := newTestIngress(t, out, "C0123", "C9999:100.5")

	cases := []struct {
		conv string
		want int
	}{
		{"C0123", http.StatusOK},       // the allowlisted channel
		{"C0123:100.5", http.StatusOK}, // a thread inside it
		{"C9999:100.5", http.StatusOK}, // an exactly-allowlisted thread
		{"C9999:200.5", http.StatusForbidden},
		{"C9999", http.StatusForbidden},
		{"C0123X", http.StatusForbidden}, // prefix must be a whole segment
	}
	for _, tc := range cases {
		w := do(t, i, http.MethodPost, fmt.Sprintf(`{"conversation":%q,"text":"x"}`, tc.conv))
		if w.Code != tc.want {
			t.Errorf("POST %s = %d %s, want %d", tc.conv, w.Code, w.Body, tc.want)
		}
	}
	// The allowlist gates edits too.
	w := do(t, i, http.MethodPatch, `{"conversation":"CBAD","id":"ts1","text":"x"}`)
	if w.Code != http.StatusForbidden {
		t.Errorf("PATCH to a non-allowlisted conversation = %d, want 403", w.Code)
	}
}

// TestIngressEmptyAllowlistAdmitsAll documents the default: with no allowlist
// configured the ingress posts anywhere the bot can reach (serve warns).
func TestIngressEmptyAllowlistAdmitsAll(t *testing.T) {
	i := newTestIngress(t, &stubSender{})
	w := do(t, i, http.MethodPost, `{"conversation":"CANYTHING","text":"x"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST = %d %s, want 200", w.Code, w.Body)
	}
}

// TestIngressBadRequests checks the validation surface: a malformed or
// incomplete body is a 400 naming what is wrong, and never reaches the
// adapter.
func TestIngressBadRequests(t *testing.T) {
	cases := []struct {
		name, method, body string
		want               int
	}{
		{"not json", http.MethodPost, `{`, http.StatusBadRequest},
		{"unknown field", http.MethodPost, `{"conversation":"C0","message":"x"}`, http.StatusBadRequest},
		{"no conversation", http.MethodPost, `{"text":"x"}`, http.StatusBadRequest},
		{"blank conversation", http.MethodPost, `{"conversation":"  ","text":"x"}`, http.StatusBadRequest},
		{"no text", http.MethodPost, `{"conversation":"C0"}`, http.StatusBadRequest},
		{"blank text", http.MethodPost, `{"conversation":"C0","text":"  "}`, http.StatusBadRequest},
		{"post with id", http.MethodPost, `{"conversation":"C0","id":"ts1","text":"x"}`, http.StatusBadRequest},
		{"patch without id", http.MethodPatch, `{"conversation":"C0","text":"x"}`, http.StatusBadRequest},
		{"newline in conversation", http.MethodPost, `{"conversation":"C0\nfake log line","text":"x"}`, http.StatusBadRequest},
		{"space in conversation", http.MethodPost, `{"conversation":"C0 C1","text":"x"}`, http.StatusBadRequest},
		{"control char in id", http.MethodPatch, `{"conversation":"C0","id":"ts\u0007","text":"x"}`, http.StatusBadRequest},
		{"post with append", http.MethodPost, `{"conversation":"C0","append":"x"}`, http.StatusBadRequest},
		{"patch with text and append", http.MethodPatch, `{"conversation":"C0","id":"ts1","text":"x","append":"y"}`, http.StatusBadRequest},
		{"patch with neither", http.MethodPatch, `{"conversation":"C0","id":"ts1"}`, http.StatusBadRequest},
		{"trailing garbage", http.MethodPost, `{"conversation":"C0","text":"x"}{"conversation":"C9","text":"y"}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := &stubSender{}
			i := newTestIngress(t, out)
			w := do(t, i, tc.method, tc.body)
			if w.Code != tc.want {
				t.Fatalf("status = %d %s, want %d", w.Code, w.Body, tc.want)
			}
			var body map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["error"] == "" {
				t.Errorf("error body = %s, want a JSON {\"error\":...}", w.Body)
			}
			if n := out.sends.Load(); n != 0 {
				t.Errorf("rejected request reached the adapter (%d sends)", n)
			}
			if calls := out.updateCalls(); len(calls) != 0 {
				t.Errorf("rejected request edited a message: %+v", calls)
			}
		})
	}
}

// TestLogSafe checks the sanitizer itself: control characters become '?' so a
// string that reaches the log cannot open a line of its own. This is the test
// that fails if logSafe stops working — the request-level test below cannot
// be, because encoding/json escapes the only caller-controlled text that
// currently reaches the log before logSafe ever sees it.
func TestLogSafe(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain", "plain"},
		{"forged\nswitchboard: all is well", "forged?switchboard: all is well"},
		{"bell\atab\tcr\r", "bell?tab?cr?"},
		{"unicode é ok", "unicode é ok"},
	} {
		if got := logSafe(tc.in); got != tc.want {
			t.Errorf("logSafe(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestIngressLogsAreNotForgeable checks the end result at the request level: a
// crafted field name cannot put a newline in the log. Note this passes on the
// strength of encoding/json's %q quoting as much as logSafe's — both stand
// between the caller and the log, and the belt-and-braces is deliberate.
func TestIngressLogsAreNotForgeable(t *testing.T) {
	var lines []string
	i, err := newIngress(ingressConfig{
		Token: ingressToken,
		Out:   &stubSender{},
		Logf:  func(_ logging.Level, f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) },
	})
	if err != nil {
		t.Fatalf("newIngress: %v", err)
	}
	w := do(t, i, http.MethodPost, "{\"conversation\":\"C0\",\"forged\\nswitchboard: all is well\":1}")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("POST = %d %s, want 400", w.Code, w.Body)
	}
	if len(lines) != 1 {
		t.Fatalf("logged %d lines, want 1: %q", len(lines), lines)
	}
	if strings.Contains(lines[0], "\n") {
		t.Errorf("log line carries a newline from the request: %q", lines[0])
	}
}

// TestIngressLogLevelFollowsTheStatus pins the split the error path makes
// (#49). A 4xx is a caller being refused, which is the ingress working; a 5xx
// is switchboard failing, and is the line worth paging on.
//
// 501 is the case that does not follow the digit. It means this deployment's
// platform cannot do what was asked — append on a channel with no TextFitter,
// anything answering chat.ErrUnsupported — which is permanent and is the
// caller's problem, so an escalation client that keeps asking would otherwise
// open an Error Reporting group per request against a healthy deployment.
func TestIngressLogLevelFollowsTheStatus(t *testing.T) {
	const ok = `{"conversation":"C0","text":"x"}`
	cases := []struct {
		name    string
		sendErr error
		body    string
		status  int
		want    logging.Level
	}{
		{"a malformed body is the caller's", nil, `{`, http.StatusBadRequest, logging.LevelWarn},
		{"a platform that failed is ours", errors.New("slack: 503"), ok, http.StatusBadGateway, logging.LevelError},
		{"a platform that cannot is the caller's", chat.ErrUnsupported, ok, http.StatusNotImplemented, logging.LevelWarn},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []logging.Level
			i, err := newIngress(ingressConfig{
				Token: ingressToken,
				Out:   &stubSender{sendErr: tc.sendErr},
				Logf:  func(lv logging.Level, _ string, _ ...any) { got = append(got, lv) },
			})
			if err != nil {
				t.Fatalf("newIngress: %v", err)
			}
			if w := do(t, i, http.MethodPost, tc.body); w.Code != tc.status {
				t.Fatalf("POST = %d %s, want %d", w.Code, w.Body, tc.status)
			}
			if len(got) != 1 {
				t.Fatalf("logged %d lines, want 1", len(got))
			}
			if got[0] != tc.want {
				t.Errorf("%d logged at %v, want %v", tc.status, got[0], tc.want)
			}
		})
	}
}

// TestIngressWrongContentType checks a non-JSON content type is refused with
// 415 rather than parsed hopefully.
func TestIngressWrongContentType(t *testing.T) {
	i := newTestIngress(t, &stubSender{})
	w := do(t, i, http.MethodPost, `{"conversation":"C0","text":"x"}`,
		withHeader("Content-Type", "text/plain"))
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d %s, want 415", w.Code, w.Body)
	}
}

// TestIngressOversizeBody checks a body past the cap is rejected as 413
// instead of being buffered into memory.
func TestIngressOversizeBody(t *testing.T) {
	i := newTestIngress(t, &stubSender{})
	body := fmt.Sprintf(`{"conversation":"C0","text":%q}`, strings.Repeat("a", maxIngressBody+1))
	w := do(t, i, http.MethodPost, body)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d %s, want 413", w.Code, w.Body)
	}
}

// TestIngressMethodAndPath checks the surface is exactly the two documented
// verbs on the one documented path.
func TestIngressMethodAndPath(t *testing.T) {
	i := newTestIngress(t, &stubSender{})

	w := do(t, i, http.MethodGet, "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d, want 405", w.Code)
	}
	if allow := w.Header().Get("Allow"); allow != "POST, PATCH" {
		t.Errorf("Allow = %q, want \"POST, PATCH\"", allow)
	}
	w = do(t, i, http.MethodDelete, "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE = %d, want 405", w.Code)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/nope", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+ingressToken)
	rec := httptest.NewRecorder()
	i.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown path = %d, want 404", rec.Code)
	}
}

// TestIngressIdempotency checks a retried POST under the same key posts once
// and returns the original ref — what keeps a scheduler's retry from
// double-posting a digest.
func TestIngressIdempotency(t *testing.T) {
	out := &stubSender{}
	i := newTestIngress(t, out)

	body := `{"conversation":"C0123","text":"digest"}`
	first := do(t, i, http.MethodPost, body, withHeader(idempotencyHeader, "digest-2026-08-15"))
	second := do(t, i, http.MethodPost, body, withHeader(idempotencyHeader, "digest-2026-08-15"))
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("statuses = %d/%d, want 200/200", first.Code, second.Code)
	}
	if first.Body.String() != second.Body.String() {
		t.Errorf("replay returned %s, want the original %s", second.Body, first.Body)
	}
	if n := out.sends.Load(); n != 1 {
		t.Errorf("adapter sends = %d, want 1", n)
	}
	// A different key is a different message.
	do(t, i, http.MethodPost, body, withHeader(idempotencyHeader, "digest-2026-08-16"))
	if n := out.sends.Load(); n != 2 {
		t.Errorf("adapter sends after a new key = %d, want 2", n)
	}
	// No key at all never dedupes.
	do(t, i, http.MethodPost, body)
	do(t, i, http.MethodPost, body)
	if n := out.sends.Load(); n != 4 {
		t.Errorf("adapter sends after two unkeyed posts = %d, want 4", n)
	}
}

// TestIngressIdempotencyKeyTooLong checks an oversized key is refused rather
// than parked in the replay map.
func TestIngressIdempotencyKeyTooLong(t *testing.T) {
	out := &stubSender{}
	i := newTestIngress(t, out)
	w := do(t, i, http.MethodPost, `{"conversation":"C0123","text":"x"}`,
		withHeader(idempotencyHeader, strings.Repeat("k", maxIdempotencyKeyLen+1)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("POST = %d %s, want 400", w.Code, w.Body)
	}
	if n := out.sends.Load(); n != 0 {
		t.Errorf("adapter sends = %d, want 0", n)
	}
	// The cap itself is fine.
	w = do(t, i, http.MethodPost, `{"conversation":"C0123","text":"x"}`,
		withHeader(idempotencyHeader, strings.Repeat("k", maxIdempotencyKeyLen)))
	if w.Code != http.StatusOK {
		t.Fatalf("POST at the cap = %d %s, want 200", w.Code, w.Body)
	}
}

// TestIngressIdempotencyConcurrent checks two in-flight duplicates collapse
// into one post — the retry may well arrive before the first one answers.
func TestIngressIdempotencyConcurrent(t *testing.T) {
	release := make(chan struct{})
	out := &stubSender{sendHook: func() { <-release }}
	i := newTestIngress(t, out)

	const n = 4
	codes := make([]int, n)
	bodies := make([]string, n)
	var wg sync.WaitGroup
	for k := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := do(t, i, http.MethodPost, `{"conversation":"C0123","text":"x"}`,
				withHeader(idempotencyHeader, "same"))
			codes[k], bodies[k] = w.Code, w.Body.String()
		}()
	}
	// Let the first send through once every request is queued behind it.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := out.sends.Load(); got != 1 {
		t.Errorf("adapter sends = %d, want 1", got)
	}
	for k := range n {
		if codes[k] != http.StatusOK {
			t.Errorf("request %d = %d", k, codes[k])
		}
		if bodies[k] != bodies[0] {
			t.Errorf("request %d body = %s, want %s", k, bodies[k], bodies[0])
		}
	}
}

// TestIngressIdempotencyForgetsFailures checks a failed post is not cached: a
// retry under the same key must really retry.
func TestIngressIdempotencyForgetsFailures(t *testing.T) {
	out := &stubSender{sendErr: errors.New("slack: rate limited")}
	i := newTestIngress(t, out)

	body := `{"conversation":"C0123","text":"x"}`
	if w := do(t, i, http.MethodPost, body, withHeader(idempotencyHeader, "k")); w.Code != http.StatusBadGateway {
		t.Fatalf("first POST = %d, want 502", w.Code)
	}
	out.sendErr = nil
	w := do(t, i, http.MethodPost, body, withHeader(idempotencyHeader, "k"))
	if w.Code != http.StatusOK {
		t.Fatalf("retry after failure = %d %s, want 200", w.Code, w.Body)
	}
	if n := out.sends.Load(); n != 2 {
		t.Errorf("adapter sends = %d, want 2 (the failure must not be replayed)", n)
	}
	i.mu.Lock()
	orderLen := len(i.opOrder)
	i.mu.Unlock()
	if orderLen != 1 {
		t.Errorf("eviction order holds %d keys, want 1 (the failed key was not forgotten)", orderLen)
	}
}

// TestIngressIdempotencyEviction checks the replay map stays bounded.
func TestIngressIdempotencyEviction(t *testing.T) {
	out := &stubSender{}
	i := newTestIngress(t, out)
	for k := range maxIdempotencyKeys + 10 {
		w := do(t, i, http.MethodPost, `{"conversation":"C0123","text":"x"}`,
			withHeader(idempotencyHeader, fmt.Sprintf("key-%d", k)))
		if w.Code != http.StatusOK {
			t.Fatalf("POST %d = %d %s", k, w.Code, w.Body)
		}
	}
	i.mu.Lock()
	posts, order := len(i.ops), len(i.opOrder)
	i.mu.Unlock()
	if posts != maxIdempotencyKeys || order != maxIdempotencyKeys {
		t.Errorf("map/order = %d/%d, want %d/%d", posts, order, maxIdempotencyKeys, maxIdempotencyKeys)
	}
	// The oldest keys were evicted, so replaying one posts again.
	before := out.sends.Load()
	do(t, i, http.MethodPost, `{"conversation":"C0123","text":"x"}`, withHeader(idempotencyHeader, "key-0"))
	if out.sends.Load() != before+1 {
		t.Errorf("replay of an evicted key did not post again")
	}
}

// TestIngressRecordsMetrics checks each request lands on the ingress counter
// under the right verb and outcome.
func TestIngressRecordsMetrics(t *testing.T) {
	m := newMetrics()
	i, err := newIngress(ingressConfig{
		Token:   ingressToken,
		Out:     &stubSender{},
		Metrics: m,
	})
	if err != nil {
		t.Fatalf("newIngress: %v", err)
	}
	do(t, i, http.MethodPost, `{"conversation":"C0123","text":"x"}`)
	do(t, i, http.MethodPost, `{"bad":`)
	do(t, i, http.MethodPatch, `{"conversation":"C0123","id":"ts1","text":"x"}`)
	do(t, i, http.MethodGet, "")

	for _, want := range []struct {
		op, outcome string
		count       float64
	}{
		{"post", "ok", 1},
		{"post", "error", 1},
		{"patch", "ok", 1},
		{"other", "error", 1},
	} {
		got := testutil.ToFloat64(m.ingressRequests.WithLabelValues(want.op, want.outcome))
		if got != want.count {
			t.Errorf("ingress_requests{op=%q,outcome=%q} = %v, want %v", want.op, want.outcome, got, want.count)
		}
	}
	// The platform sends are also counted as replies, like the router's.
	if got := testutil.ToFloat64(m.repliesSent.WithLabelValues("ok")); got != 2 {
		t.Errorf("replies_sent{ok} = %v, want 2", got)
	}
}

// TestNewIngressValidation checks the constructor refuses a config that would
// stand up an unauthenticated or inert ingress.
func TestNewIngressValidation(t *testing.T) {
	if _, err := newIngress(ingressConfig{Out: &stubSender{}}); err == nil {
		t.Error("expected an error without a token")
	}
	if _, err := newIngress(ingressConfig{Token: ingressToken}); err == nil {
		t.Error("expected an error without a chat egress")
	}
	i, err := newIngress(ingressConfig{Token: ingressToken, Out: &stubSender{}})
	if err != nil {
		t.Fatalf("newIngress: %v", err)
	}
	// No Logf wired, and the error path logs: the zero hook has to discard
	// rather than panic, which is what retired the constructor's guard (#49).
	rec := httptest.NewRecorder()
	i.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, ingressPath, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated POST = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// TestServeIngressDisabled verifies an empty addr binds nothing and returns
// only once ctx is cancelled.
func TestServeIngressDisabled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveIngress(ctx, "", newTestIngress(t, &stubSender{})) }()
	select {
	case err := <-done:
		t.Fatalf("serveIngress returned before cancel: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveIngress(disabled) = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("serveIngress did not return after cancel")
	}
}

// TestServeIngressEndToEnd runs the ingress over a real listener and drives a
// post-then-edit round trip through it, the way an outbound caller will.
func TestServeIngressEndToEnd(t *testing.T) {
	out := &stubSender{}
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveIngress(ctx, addr, newTestIngress(t, out, "C0123")) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serveIngress = %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("serveIngress did not shut down after cancel")
		}
	})

	base := "http://" + addr + ingressPath
	waitIngressReady(t, base)

	code, body := ingressCall(t, http.MethodPost, base, `{"conversation":"C0123","text":"hello"}`)
	if code != http.StatusOK {
		t.Fatalf("POST = %d %s", code, body)
	}
	var ref messageResponse
	if err := json.Unmarshal([]byte(body), &ref); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	patch := fmt.Sprintf(`{"conversation":%q,"id":%q,"text":"edited"}`, ref.Conversation, ref.ID)
	if code, body := ingressCall(t, http.MethodPatch, base, patch); code != http.StatusNoContent {
		t.Fatalf("PATCH = %d %s", code, body)
	}
	if calls := out.updateCalls(); len(calls) != 1 || calls[0].ref.ID != ref.ID {
		t.Errorf("Update calls = %+v, want one edit of %s", calls, ref.ID)
	}
}

// TestServeIngressBindError verifies a port already in use surfaces as an
// error, which is what lets serve fail fast rather than run without the
// ingress a caller depends on.
func TestServeIngressBindError(t *testing.T) {
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	blocker := make(chan error, 1)
	go func() { blocker <- serveIngress(ctx, addr, newTestIngress(t, &stubSender{})) }()
	waitIngressReady(t, "http://"+addr+ingressPath)

	if err := serveIngress(ctx, addr, newTestIngress(t, &stubSender{})); err == nil {
		t.Fatal("serveIngress on a busy port returned nil, want error")
	}
	cancel()
	<-blocker
}

// ingressCall issues one authenticated request against a running ingress.
func ingressCall(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+ingressToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	buf := new(strings.Builder)
	if _, err := io.Copy(buf, resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, buf.String()
}

// waitIngressReady blocks until the listener answers (any status will do).
func waitIngressReady(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("ingress at %s never became ready", url)
}
