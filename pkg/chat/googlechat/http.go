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

// This file is the HTTP ingress: the second way an event can arrive, and the
// only one that can deliver a card click (#29, docs/DESIGN.md §3.4).
//
// Everything about *decoding* an event is shared with Pub/Sub — handleEvent
// takes bytes and both dialects normalize into one inbound. What is different
// is everything around it, and it is all here:
//
//   - The endpoint is reachable by anyone. Over Pub/Sub, authorization is the
//     subscription's IAM and nothing reaches the process that Google did not
//     put there; here the request has to prove it came from Chat.
//   - Chat waits ~30 seconds and a turn routinely takes longer, so the
//     response cannot be the answer. The handler is acknowledged immediately
//     and the turn runs on, posting through the same REST egress Pub/Sub uses.
//   - "Always ack" becomes "always answer 200": an event this gateway cannot
//     act on must not be retried into a duplicate turn.
package googlechat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	chatv1 "google.golang.org/api/chat/v1"
	"google.golang.org/api/idtoken"

	"github.com/go-steer/switchboard/pkg/chat"
)

// IngressMode selects how the adapter receives events.
type IngressMode string

const (
	// IngressPubSub pulls events from a Pub/Sub subscription. The default,
	// because it exposes no inbound surface.
	IngressPubSub IngressMode = "pubsub"
	// IngressHTTP serves an endpoint Chat posts events to. Required for card
	// clicks, and a public attack surface a deployment that wants no buttons
	// should not be paying for.
	IngressHTTP IngressMode = "http"
)

// ParseIngressMode validates an ingress mode. The empty string is IngressPubSub,
// so an unset config keeps the posture it had before this existed.
func ParseIngressMode(s string) (IngressMode, bool) {
	switch IngressMode(s) {
	case "":
		return IngressPubSub, true
	case IngressPubSub, IngressHTTP:
		return IngressMode(s), true
	}
	return "", false
}

// IngressPath is where the HTTP ingress serves events. Fixed rather than
// configurable: it is half of the audience every request's token is checked
// against, so a value that can drift between the console and the process is a
// verification failure waiting to be debugged as a delivery failure.
const IngressPath = "/chat"

// maxEventBytes caps a request body. Chat's own messages are far smaller; the
// cap is what stops an unauthenticated caller — verification reads the body,
// so the read happens before the caller is known — from spending the
// process's memory.
const maxEventBytes = 1 << 20

// chatIngressGrace is how long in-flight turns get to finish once the run
// context is cancelled. A turn that is mid-round-trip has already been
// injected, so cutting it off loses the answer rather than the work.
const chatIngressGrace = 25 * time.Second

// clickBudget is how long a click's response waits for the edit it causes.
// Chat allows the endpoint ~30 seconds, but the server's WriteTimeout is 15
// from the start of the request, and the response has to be written inside
// both; a press that takes longer than this is answered "{}" and its edit
// goes over REST instead, which is the documented way to acknowledge an
// interaction whose result comes later.
const clickBudget = 10 * time.Second

// clickKey is the context key for a click's pending response.
type clickKey struct{}

// clickResponse is the edit a click caused to the message hosting its card,
// held for the click's HTTP response instead of being sent over REST.
//
// It exists because the response is the faster channel and the router does
// not know there is one. An approval must visibly do something the moment it
// is pressed (docs/DESIGN.md §3.4), and a REST patch is a round-trip later at
// best. But the router records a decision by calling Update, exactly as it
// does for Slack, and it should not learn that one platform can answer a click
// in-band — so the capture sits in rewrite, keyed to the one message the click
// came from, and everything else the press does (a notice beside the question,
// a refusal) goes out over REST as usual.
//
// Faster, not authoritative. Nothing tells the handler whether Chat received
// the response or accepted the card in it — net/http buffers the write, so it
// "succeeds" past a deadline or a dropped connection — and an edit that is
// lost leaves a settled question with live buttons that every later press
// answers with nothing visible. So the captured edit is also sent over REST
// once the response is written (answerClick), through rewrite's ordinary path
// with its card-rejection fallback. A patch is idempotent, and the in-band copy
// is the one the presser sees first.
type clickResponse struct {
	name string // the hosting message; only its edits are captured

	mu     sync.Mutex
	edit   *clickEdit
	closed bool
}

// clickEdit is one captured edit, kept in rewrite's terms so it can be
// replayed through rewrite.
type clickEdit struct {
	card *chatv1.GoogleAppsCardV1Card
	text string
}

// message renders the edit as the Message an updateMessageAction carries.
func (e *clickEdit) message() *chatv1.Message {
	if cards := singleCard(e.card); cards != nil {
		return &chatv1.Message{CardsV2: cards}
	}
	return &chatv1.Message{Text: e.text}
}

// take captures an edit to name, reporting whether it did. Once the response
// is written it captures nothing, and the edit falls through to REST — so a
// press slower than clickBudget still lands, just later.
func (c *clickResponse) take(name string, card *chatv1.GoogleAppsCardV1Card, text string) bool {
	if c == nil || name != c.name {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	// The latest edit wins, as it would over REST.
	c.edit = &clickEdit{card: card, text: text}
	return true
}

// close ends capturing and returns what was captured, if anything.
func (c *clickResponse) close() *clickEdit {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return c.edit
}

// clickFrom returns the click response ctx is answering, or nil.
func clickFrom(ctx context.Context) *clickResponse {
	c, _ := ctx.Value(clickKey{}).(*clickResponse)
	return c
}

// updateMessageResponse is the add-on envelope that edits the message a click
// came from. Not the legacy actionResponse: the add-on framework answers with
// hostAppDataAction (docs/DESIGN.md §3.4).
func updateMessageResponse(msg *chatv1.Message) ([]byte, error) {
	return json.Marshal(map[string]any{
		"hostAppDataAction": map[string]any{
			"chatDataAction": map[string]any{
				"updateMessageAction": map[string]any{"message": msg},
			},
		},
	})
}

// authorizationEventObject is the add-on framework's carrier for the
// Google-signed ID token. The same token arrives in the Authorization header,
// but both are checked: the header is the one a proxy is most likely to strip,
// and the body is the one that survives it.
type authorizationEventObject struct {
	AuthorizationEventObject *struct {
		SystemIDToken string `json:"systemIdToken"`
	} `json:"authorizationEventObject"`
}

// validateFunc is idtoken.Validate, injectable so the verifier can be tested
// without reaching Google's certificate endpoint.
type validateFunc func(ctx context.Context, token, audience string) (*idtoken.Payload, error)

// verifier decides whether a request really came from Google Chat.
//
// The check is a Google-signed ID token, and what makes it mean anything is
// the pair: the signature proves Google minted it, the audience proves it was
// minted for this endpoint rather than replayed from another, and the email
// proves it was minted for *this* add-on. The last one is the easy one to
// leave out and the one that matters most — every Workspace add-on's token is
// signed by an address of the same shape, derived from its own project number,
// so accepting the shape accepts every add-on on the platform.
type verifier struct {
	// audience pins the expected aud. Empty derives it per request, which is
	// what a deployment behind an ingress that rewrites Host needs to avoid.
	audience string
	// expect is the service-account email the token must carry. Required —
	// there is no unpinned mode, because an unpinned check is not a check.
	expect   string
	validate validateFunc
}

// audienceFor returns the URL the token should have been minted for. The
// configured value wins; otherwise it is this request's own URL, since the aud
// Chat used is by definition the URL Chat called.
//
// The scheme is always https and not sniffed from the request. Chat calls
// https, so a derived "http://…" audience could only ever fail to match, and
// it would fail as an authentication error long after the actual mistake.
func (v verifier) audienceFor(r *http.Request) string {
	if v.audience != "" {
		return v.audience
	}
	return "https://" + r.Host + r.URL.Path
}

// bearerToken pulls the credential out of an Authorization header, or returns
// "" if there is nothing there to check. The scheme is matched
// case-insensitively because RFC 7235 says it is one; Chat sends "Bearer", but
// a proxy that normalized it would otherwise fail as "no ID token at all",
// which is the most misleading of the errors this file can produce.
func bearerToken(header string) string {
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

// verify reports whether the request carries a valid token from the expected
// caller. Both carriers are tried and either one suffices; the error names
// which checks ran so a misconfiguration is diagnosable without turning on
// payload logging.
func (v verifier) verify(ctx context.Context, r *http.Request, body []byte) error {
	audience := v.audienceFor(r)
	var tried []string
	var errs []error

	check := func(source, token string) bool {
		if token == "" {
			return false
		}
		tried = append(tried, source)
		payload, err := v.validate(ctx, token, audience)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", source, err))
			return false
		}
		email, _ := payload.Claims["email"].(string)
		if email != v.expect {
			errs = append(errs, fmt.Errorf("%s: token is from %q, want %q", source, email, v.expect))
			return false
		}
		return true
	}

	if check("header", bearerToken(r.Header.Get("Authorization"))) {
		return nil
	}
	var envelope authorizationEventObject
	// A body that does not parse is not an authentication failure to report in
	// detail — handleEvent will reject it too — but it must not read as a
	// missing token either, so it simply contributes no carrier.
	if err := json.Unmarshal(body, &envelope); err == nil &&
		envelope.AuthorizationEventObject != nil {
		if check("systemIdToken", envelope.AuthorizationEventObject.SystemIDToken) {
			return nil
		}
	}

	if len(tried) == 0 {
		return errors.New("no ID token in the Authorization header or the event body")
	}
	return fmt.Errorf("no valid ID token for audience %s: %w", audience, errors.Join(errs...))
}

// serveIngress binds the ingress and serves it until ctx is cancelled. It is
// Run's HTTP arm; the Pub/Sub arm is Receive.
func (a *Adapter) serveIngress(ctx context.Context, h chat.Handler) error {
	ln, err := net.Listen("tcp", a.listen)
	if err != nil {
		return fmt.Errorf("googlechat: listen %s: %w", a.listen, err)
	}
	a.logf.Infof("googlechat: serving the interaction endpoint on %s%s", a.listen, IngressPath)
	return a.serveOn(ctx, ln, h)
}

// serveOn is serveIngress once the listener exists, which is the seam a test
// binds its own port through.
func (a *Adapter) serveOn(ctx context.Context, ln net.Listener, h chat.Handler) error {
	mux := http.NewServeMux()
	// A turn outlives the request that started it, so it cannot run on the
	// request's context — that is cancelled the moment the response is
	// written. It runs on the run context instead, which means shutdown
	// cancels it and the turn group below waits for it.
	var turns turnGroup
	mux.Handle(IngressPath, a.eventHandler(ctx, &turns, h))

	server := &http.Server{
		Handler:  mux,
		ErrorLog: a.logf.StdLogger(chat.LevelError, "googlechat ingress: "),
		// The handler answers without waiting for the turn, so none of these
		// need to accommodate a daemon round-trip.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.Serve(ln) }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), chatIngressGrace)
		defer cancel()
		// Refused from here, not from drain: the run context is already
		// cancelled, so a request still being served during Shutdown's grace
		// would otherwise be answered 200 and start a turn that dies at once.
		turns.refuseNew()
		err := server.Shutdown(shutdownCtx)
		// Shutdown returns once the listener is closed and requests have
		// drained, but the turns those requests started are on their own
		// goroutines. Waiting for them is what keeps a shutdown from
		// abandoning an answer that was about to be posted — and bounding
		// that wait is what keeps one turn that ignores its cancelled context
		// from holding the process open until something kills it.
		a.drain(shutdownCtx, &turns)
		if err != nil {
			return fmt.Errorf("googlechat: ingress shutdown: %w", err)
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), chatIngressGrace)
			defer cancel()
			a.drain(shutdownCtx, &turns)
			return nil
		}
		return fmt.Errorf("googlechat: ingress: %w", err)
	}
}

// turnGroup tracks the turns requests have started, and stops new ones once
// shutdown starts waiting for them (#93). A bare WaitGroup cannot do the
// second half: Shutdown gives up on its deadline with handlers still running,
// and one of them reaching Add while drain is in Wait with the counter at zero
// is WaitGroup misuse, which the runtime may report as a panic. Taking the
// flag and calling Add under one lock makes "started" and "draining" mutually
// ordered — a turn is either counted before the wait begins, or refused.
type turnGroup struct {
	mu       sync.Mutex
	draining bool
	wg       sync.WaitGroup
}

// start counts a new turn, or reports false once draining has begun, in which
// case the caller must not start one.
func (g *turnGroup) start() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.draining {
		return false
	}
	g.wg.Add(1)
	return true
}

func (g *turnGroup) done() { g.wg.Done() }

// Wait waits for every counted turn. It does not refuse new ones; drain does.
func (g *turnGroup) Wait() { g.wg.Wait() }

// refuseNew marks the group draining: every later start reports false.
func (g *turnGroup) refuseNew() {
	g.mu.Lock()
	g.draining = true
	g.mu.Unlock()
}

// drain refuses new turns, then waits for the in-flight ones, giving up when
// ctx does.
func (a *Adapter) drain(ctx context.Context, turns *turnGroup) {
	turns.refuseNew()
	done := make(chan struct{})
	go func() { turns.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		// Said out loud: the answers those turns were about to post are the
		// ones that will never appear in a thread, and a silent shutdown is
		// how that becomes a mystery instead of a log line.
		a.logf.Warnf("googlechat: ingress: gave up after %s waiting for in-flight turns", chatIngressGrace)
	}
}

// eventHandler answers one Chat event.
//
// runCtx, not the request context: the response is written before the turn
// finishes, and the request context is cancelled at that moment.
func (a *Adapter) eventHandler(runCtx context.Context, turns *turnGroup, h chat.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxEventBytes))
		if err != nil {
			a.logf.Warnf("googlechat: ingress: read body: %v", err)
			// 413 for a body over the cap, so an operator whose events are
			// genuinely that large reads the limit rather than hunting a
			// malformed payload.
			status := http.StatusBadRequest
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				status = http.StatusRequestEntityTooLarge
			}
			http.Error(w, http.StatusText(status), status)
			return
		}
		// Before anything looks at the payload: an unverified body is an
		// anonymous caller's, and handleEvent would inject it as a turn under
		// whatever identity it claimed.
		if err := a.verify.verify(r.Context(), r, body); err != nil {
			a.logf.Errorf("googlechat: ingress: rejected a request: %v", err)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		a.learnURL(r)

		// A click is the one event worth holding the response for: what it
		// changes is the card it was pressed on, and the response is where
		// that change shows up at once. Only the add-on dialect's envelope is
		// known here; a legacy click is acknowledged like any other event and
		// answered over REST, which is how that dialect works over Pub/Sub.
		if in, err := decodeEvent(body); err == nil &&
			in.kind == kindButton && in.addon && in.messageName != "" {
			a.answerClick(w, r, runCtx, turns, h, body, in.messageName)
			return
		}

		// Answer now, work after. Chat gives the endpoint ~30 seconds and a
		// turn is routinely longer, so holding the response until the answer
		// exists would time the request out and earn a retry — which is a
		// duplicate turn, not a second chance.
		//
		// On a platform that throttles CPU once the response is written
		// (Cloud Run's default allocation) this goroutine is not reliably
		// scheduled; switchboard ships as a Deployment, where it is. See
		// docs/DESIGN.md §3.4.
		if runCtx.Err() != nil || !turns.start() {
			refuseDraining(w)
			return
		}
		go func() {
			defer turns.done()
			a.handleEvent(runCtx, h, body)
		}()

		// Always 200, for the same reason dispatch always acks: an event this
		// gateway cannot act on must not come back.
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte("{}")); err != nil {
			a.logf.Warnf("googlechat: ingress: write response: %v", err)
		}
	})
}

// refuseDraining answers a request that arrived after shutdown stopped
// starting turns. 503 rather than the usual 200, because this event was not
// acted on and a 200 would say it had: the refusal shows up as Chat's own
// error, where the person can send it again, instead of as a message that
// silently went nowhere. Nothing was started, so a resend duplicates nothing.
// Whether Chat itself redelivers an HTTP event after a 5xx is unmeasured, and
// nothing here relies on it.
func refuseDraining(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	http.Error(w, "shutting down", http.StatusServiceUnavailable)
}

// learnURL records the URL a verified request was addressed to, for actionURL.
// Only when none is configured: a configured one is the audience, and is what
// buttons use.
func (a *Adapter) learnURL(r *http.Request) {
	if a.verify.audience != "" {
		return
	}
	u := a.verify.audienceFor(r)
	if cur := a.learnedURL.Load(); cur == nil || *cur != u {
		a.learnedURL.Store(&u)
	}
}

// answerClick runs a click and answers it with the edit it made to the
// hosting card, if that edit is ready within clickBudget, and with "{}"
// otherwise — after which the edit, whenever it comes, goes over REST. An
// edit that did ride in the response is sent over REST as well, right after
// it; see clickResponse.
//
// The click runs on the run context like a turn does, not on the request's:
// the response may be written before it finishes, and a press that has reached
// the daemon must not be cut off by a response deadline.
func (a *Adapter) answerClick(w http.ResponseWriter, r *http.Request, runCtx context.Context,
	turns *turnGroup, h chat.Handler, body []byte, hosting string) {
	click := &clickResponse{name: hosting}
	ctx := context.WithValue(runCtx, clickKey{}, click)
	done := make(chan struct{})
	if runCtx.Err() != nil || !turns.start() {
		refuseDraining(w)
		return
	}
	go func() {
		defer turns.done()
		defer close(done)
		a.handleEvent(ctx, h, body)
	}()

	budget := a.clickWait
	if budget == 0 {
		budget = clickBudget
	}
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		a.logf.Warnf("googlechat: ingress: click on %s took over %s; its edit will follow over REST", hosting, budget)
	case <-r.Context().Done():
	}

	edit := click.close()
	resp := []byte("{}")
	if edit != nil {
		if b, err := updateMessageResponse(edit.message()); err != nil {
			// Unreachable for a chatv1.Message; the edit still lands, over
			// REST below.
			a.logf.Errorf("googlechat: ingress: encode click response for %s: %v", hosting, err)
		} else {
			resp = b
		}
	}
	if r.Context().Err() == nil {
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write(resp); err != nil {
			a.logf.Warnf("googlechat: ingress: write click response: %v", err)
		}
		// Flushed before the replay below, not when the handler returns:
		// Write only fills net/http's buffer, and holding the in-band edit
		// behind a REST round trip would make it the slower copy of the two —
		// and could push it past the server's WriteTimeout altogether.
		if err := http.NewResponseController(w).Flush(); err != nil {
			a.logf.Warnf("googlechat: ingress: flush click response: %v", err)
		}
	}
	if edit != nil {
		// And over REST, every time: see clickResponse for why the response
		// cannot be trusted to have landed. On the run context rather than
		// the click's, so rewrite takes its ordinary path — capture closed,
		// card-rejection fallback intact — and bounded, since a hung platform
		// call here holds the handler open.
		pctx, cancel := context.WithTimeout(context.WithoutCancel(runCtx), clickBudget)
		defer cancel()
		if err := a.rewrite(pctx, hosting, edit.card, edit.text); err != nil {
			a.logf.Errorf("googlechat: ingress: click edit %s over REST: %v", hosting, err)
		}
	}
}
