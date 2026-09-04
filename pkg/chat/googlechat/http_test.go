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

package googlechat

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/api/idtoken"

	"github.com/go-steer/switchboard/pkg/chat"
)

const (
	testChatSA   = "service-123456789@gcp-sa-gsuiteaddons.iam.gserviceaccount.com"
	testAudience = "https://switchboard.example.com/chat"
)

// stubValidate accepts exactly one token string and reports the email it
// carries, so a test can separate "the signature did not check out" from "the
// token was minted for somebody else" — which are the two halves of the check
// and fail for very different reasons.
func stubValidate(good, email string) validateFunc {
	return func(_ context.Context, token, audience string) (*idtoken.Payload, error) {
		if token != good {
			return nil, errors.New("bad signature")
		}
		if audience != testAudience {
			return nil, errors.New("audience mismatch: " + audience)
		}
		return &idtoken.Payload{Claims: map[string]any{"email": email}}, nil
	}
}

// waitFor polls until cond holds, for the handful of assertions that race a
// goroutine the response did not wait for.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func testVerifier(good, email string) verifier {
	return verifier{audience: testAudience, expect: testChatSA, validate: stubValidate(good, email)}
}

func postEvent(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, testAudience, strings.NewReader(body))
	r.Host = "switchboard.example.com"
	return r
}

func TestVerifyAcceptsTheAuthorizationHeader(t *testing.T) {
	v := testVerifier("good-token", testChatSA)
	r := postEvent(`{}`)
	r.Header.Set("Authorization", "Bearer good-token")
	if err := v.verify(context.Background(), r, []byte(`{}`)); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// TestVerifyAcceptsTheBodyToken: the same token rides in the event body, and
// that copy is the one that survives a proxy stripping the Authorization
// header. A verifier that only read the header would reject real traffic.
func TestVerifyAcceptsTheBodyToken(t *testing.T) {
	v := testVerifier("good-token", testChatSA)
	body := `{"authorizationEventObject": {"systemIdToken": "good-token"}}`
	if err := v.verify(context.Background(), postEvent(body), []byte(body)); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// TestVerifyAcceptsALowercasedScheme: the auth scheme is case-insensitive per
// RFC 7235, and a proxy that normalizes it would otherwise be rejected as
// carrying no token at all — the one error message that would send an operator
// looking at Chat's configuration instead of at their own hop.
func TestVerifyAcceptsALowercasedScheme(t *testing.T) {
	v := testVerifier("good-token", testChatSA)
	r := postEvent(`{}`)
	r.Header.Set("Authorization", "bearer good-token")
	if err := v.verify(context.Background(), r, []byte(`{}`)); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// TestVerifyRejectsAnotherAddOn is the check that carries the most weight.
// Every Workspace add-on's requests are signed by an address of the same
// gcp-sa-gsuiteaddons shape, derived from its own project number, so a
// verifier that stopped at "Google signed it" would accept every add-on on the
// platform as this one.
func TestVerifyRejectsAnotherAddOn(t *testing.T) {
	other := "service-999999999@gcp-sa-gsuiteaddons.iam.gserviceaccount.com"
	v := testVerifier("good-token", other)
	r := postEvent(`{}`)
	r.Header.Set("Authorization", "Bearer good-token")

	err := v.verify(context.Background(), r, []byte(`{}`))
	if err == nil {
		t.Fatal("a valid token from a different add-on was accepted")
	}
	if !strings.Contains(err.Error(), other) {
		t.Fatalf("error should name the rejected caller, got %v", err)
	}
}

func TestVerifyRejectsMissingAndBadTokens(t *testing.T) {
	v := testVerifier("good-token", testChatSA)
	tests := []struct {
		name, header, body, want string
	}{
		{"no token at all", "", `{}`, "no ID token"},
		{"empty bearer", "Bearer ", `{}`, "no ID token"},
		{"not a bearer scheme", "Basic abc", `{}`, "no ID token"},
		{"bad signature in header", "Bearer forged", `{}`, "bad signature"},
		{"bad signature in body", "", `{"authorizationEventObject":{"systemIdToken":"forged"}}`, "bad signature"},
		{"unparseable body", "", `not json`, "no ID token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := postEvent(tt.body)
			if tt.header != "" {
				r.Header.Set("Authorization", tt.header)
			}
			err := v.verify(context.Background(), r, []byte(tt.body))
			if err == nil {
				t.Fatal("want a rejection, got none")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

// TestAudienceIsTheRequestsOwnURL: the token's aud is whatever URL Chat called,
// so with nothing pinned that is what the check has to compare against. The
// scheme is always https because Chat only ever calls https — deriving "http"
// from a plaintext hop behind a TLS-terminating proxy would turn every request
// into an authentication failure.
func TestAudienceIsTheRequestsOwnURL(t *testing.T) {
	derive := verifier{expect: testChatSA}
	r := httptest.NewRequest(http.MethodPost, "http://internal:8080/chat", nil)
	r.Host = "switchboard.example.com"
	if got := derive.audienceFor(r); got != testAudience {
		t.Fatalf("audience = %q, want %q", got, testAudience)
	}

	pinned := verifier{audience: "https://pinned.example/chat", expect: testChatSA}
	if got := pinned.audienceFor(r); got != "https://pinned.example/chat" {
		t.Fatalf("a pinned audience must win, got %q", got)
	}
}

// newIngressAdapter builds an adapter whose egress is a fake and whose token
// check accepts one known token, so a test can drive the endpoint end to end
// without Google on either side.
func newIngressAdapter(t *testing.T, f *fakeMessenger) *Adapter {
	t.Helper()
	a := newTestAdapter(f)
	a.ingress = IngressHTTP
	a.verify = testVerifier("good-token", testChatSA)
	return a
}

// serveOne drives the handler and waits for the turn the response did not wait
// for, which is the whole shape of this ingress: 200 first, work after.
func serveOne(t *testing.T, a *Adapter, h *fakeHandler, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	var wg sync.WaitGroup
	rec := httptest.NewRecorder()
	a.eventHandler(context.Background(), &wg, h).ServeHTTP(rec, r)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the turn started by the request never finished")
	}
	return rec
}

func TestIngressRoutesAVerifiedMessage(t *testing.T) {
	f := &fakeMessenger{}
	h := &fakeHandler{}
	a := newIngressAdapter(t, f)

	body := `{"chat": {
		"user": {"name": "users/5", "email": "someone@example.com"},
		"space": {"name": "spaces/AAA"},
		"messagePayload": {"message": {"text": "hello", "sender": {"name": "users/5"},
			"thread": {"name": "spaces/AAA/threads/T1"}}}
	}}`
	r := postEvent(body)
	r.Header.Set("Authorization", "Bearer good-token")
	rec := serveOne(t, a, h, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(h.msgs) != 1 {
		t.Fatalf("want 1 turn, got %d", len(h.msgs))
	}
	if h.msgs[0].Text != "hello" || h.msgs[0].Caller != "someone@example.com" {
		t.Fatalf("unexpected turn %+v", h.msgs[0])
	}
}

// TestIngressRejectsBeforeReadingThePayload is the one that matters: an
// unverified body must not become a turn. Without the check, anyone who can
// reach the endpoint can inject a turn into the daemon as any caller they care
// to name.
func TestIngressRejectsBeforeReadingThePayload(t *testing.T) {
	f := &fakeMessenger{}
	h := &fakeHandler{}
	a := newIngressAdapter(t, f)

	body := `{"chat": {
		"user": {"name": "users/5", "email": "ceo@example.com"},
		"space": {"name": "spaces/AAA"},
		"messagePayload": {"message": {"text": "delete everything", "sender": {"name": "users/5"}}}
	}}`
	r := postEvent(body)
	r.Header.Set("Authorization", "Bearer forged")
	rec := serveOne(t, a, h, r)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if len(h.msgs) != 0 {
		t.Fatalf("a forged request became a turn: %+v", h.msgs)
	}
	if len(f.creates) != 0 {
		t.Fatalf("a forged request posted to Chat: %+v", f.creates)
	}
}

// TestIngressAnswersAnEventItCannotUse: always 200, for the same reason
// dispatch always acks. A non-2xx earns a redelivery, and a redelivered
// message event is a duplicate turn rather than a second chance.
func TestIngressAnswersAnEventItCannotUse(t *testing.T) {
	f := &fakeMessenger{}
	h := &fakeHandler{}
	a := newIngressAdapter(t, f)

	for _, body := range []string{
		`{"chat": {"removedFromSpacePayload": {"space": {"name": "spaces/AAA"}}}}`,
		`{not json at all`,
	} {
		r := postEvent(body)
		r.Header.Set("Authorization", "Bearer good-token")
		if rec := serveOne(t, a, h, r); rec.Code != http.StatusOK {
			t.Fatalf("body %q: status = %d, want 200", body, rec.Code)
		}
	}
	if len(h.msgs) != 0 {
		t.Fatalf("nothing should have become a turn: %+v", h.msgs)
	}
}

func TestIngressRejectsNonPost(t *testing.T) {
	f := &fakeMessenger{}
	h := &fakeHandler{}
	a := newIngressAdapter(t, f)

	r := httptest.NewRequest(http.MethodGet, testAudience, nil)
	r.Host = "switchboard.example.com"
	rec := serveOne(t, a, h, r)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

// TestIngressCapsTheBody: the body is read before the caller is known — the
// token is in it — so the read is the one thing an anonymous caller can make
// this process do. 413 rather than 400 so the limit is what an operator reads
// if real traffic ever grows into it.
func TestIngressCapsTheBody(t *testing.T) {
	f := &fakeMessenger{}
	h := &fakeHandler{}
	a := newIngressAdapter(t, f)

	r := postEvent(`{"chat": {"padding": "` + strings.Repeat("x", maxEventBytes) + `"}}`)
	r.Header.Set("Authorization", "Bearer good-token")
	rec := serveOne(t, a, h, r)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if len(h.msgs) != 0 {
		t.Fatalf("an oversized body became a turn: %+v", h.msgs)
	}
}

// TestServeOnAnswersOverASocketAndDrains runs the real server: a real listener,
// a real client, a real shutdown. What httptest cannot show is the part that
// matters most about this ingress — that cancelling the run context returns
// from serve only *after* the turn the last request started has finished, since
// that turn is the reply nobody would otherwise ever see in the thread.
func TestServeOnAnswersOverASocketAndDrains(t *testing.T) {
	f := &fakeMessenger{}
	a := newIngressAdapter(t, f)
	// Blocks until the test lets it go, so the drain has something to wait for.
	release := make(chan struct{})
	h := &blockingHandler{release: release}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	// The audience stays pinned to testAudience rather than being derived from
	// this plaintext socket: derivation has its own test, and pinning is also
	// what a deployment behind a TLS terminator ends up doing.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- a.serveOn(ctx, ln, h) }()

	body := `{"chat": {
		"user": {"name": "users/5", "email": "someone@example.com"},
		"space": {"name": "spaces/AAA"},
		"messagePayload": {"message": {"text": "hello", "sender": {"name": "users/5"}}}
	}}`
	req, err := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+IngressPath,
		strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer good-token")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	answer, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(answer) != "{}" {
		t.Fatalf("response = %d %q, want 200 {}", resp.StatusCode, answer)
	}
	// The response arrived while the turn is still blocked, which is the whole
	// reason this ingress can outlive Chat's ~30-second budget. The turn is
	// started by a goroutine the response did not wait for either, so this
	// waits for it to begin rather than assuming it already has.
	waitFor(t, "the turn to start", func() bool { return h.started() == 1 })
	if h.finished() != 0 {
		t.Fatal("the response waited for the turn")
	}

	cancel()
	select {
	case err := <-served:
		t.Fatalf("serveOn returned while a turn was still running: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serveOn: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveOn never returned after the turn finished")
	}
	if h.finished() != 1 {
		t.Fatalf("the shutdown abandoned the turn it was draining")
	}
}

// TestServeOnStopsServingAfterShutdown: the listener is closed, not merely
// ignored, so a request arriving after the run context is cancelled is refused
// by the kernel rather than accepted into a process on its way out.
func TestServeOnStopsServingAfterShutdown(t *testing.T) {
	f := &fakeMessenger{}
	h := &fakeHandler{}
	a := newIngressAdapter(t, f)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- a.serveOn(ctx, ln, h) }()

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serveOn: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveOn never returned")
	}

	if _, err := http.Post("http://"+addr+IngressPath, "application/json",
		strings.NewReader(`{}`)); err == nil {
		t.Fatal("the endpoint still answers after shutdown")
	}
}

// TestNewRefusesAnUnpinnedHTTPIngress: the endpoint is public, so starting
// without an expected caller is the failure mode worth making impossible
// rather than merely discouraged.
func TestNewRefusesAnUnpinnedHTTPIngress(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			"no expected caller",
			Config{Ingress: IngressHTTP, ListenAddr: ":8081"},
			"ChatServiceAccount is required",
		},
		{
			"no listen address",
			Config{Ingress: IngressHTTP, ChatServiceAccount: testChatSA},
			"ListenAddr is required",
		},
		{
			"both transports at once",
			Config{Ingress: IngressHTTP, ListenAddr: ":8081", ChatServiceAccount: testChatSA,
				ProjectID: "p", SubscriptionID: "s"},
			"SubscriptionID is for the pubsub ingress",
		},
		{
			"unknown ingress",
			Config{Ingress: "webhook"},
			"invalid ingress",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.cfg)
			if err == nil {
				t.Fatal("want a refusal, got none")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

// blockingHandler holds a turn open until it is released, so a test can look
// at the gap between the response and the answer — which on this ingress is
// where the whole turn lives.
type blockingHandler struct {
	fakeHandler
	release chan struct{}

	mu          sync.Mutex
	begun, done int
}

// Handle deliberately ignores its context. A turn that stopped the moment the
// run context was cancelled would make the drain look like it worked whether
// or not it waited, which is the thing under test.
func (h *blockingHandler) Handle(_ context.Context, _ chat.Message) error {
	h.mu.Lock()
	h.begun++
	h.mu.Unlock()
	select {
	case <-h.release:
	case <-time.After(30 * time.Second): // a stuck test, not a hung one
	}
	h.mu.Lock()
	h.done++
	h.mu.Unlock()
	return nil
}

func (h *blockingHandler) started() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.begun
}

func (h *blockingHandler) finished() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.done
}

func TestParseIngressMode(t *testing.T) {
	tests := []struct {
		in   string
		want IngressMode
		ok   bool
	}{
		{"", IngressPubSub, true},
		{"pubsub", IngressPubSub, true},
		{"http", IngressHTTP, true},
		{"HTTP", "", false},
		{"webhook", "", false},
	}
	for _, tt := range tests {
		got, ok := ParseIngressMode(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Fatalf("ParseIngressMode(%q) = %q, %v; want %q, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}
