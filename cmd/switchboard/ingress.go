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
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/go-steer/switchboard/pkg/chat"
)

// The outbound ingress: an authenticated HTTP surface that lets another
// service post — and later edit or extend — a message in a conversation with
// no inbound chat event to reply to (#21). It is the same egress the router
// uses for replies and progress edits, reached from outside instead of from a
// thread: a scheduled digest, a monitoring escalation, an approval prompt
// raised at 3am. The scheduling stays entirely on the caller's side;
// switchboard remains a transport.
//
//	POST  /v1/messages  {"conversation":"C0123","text":"..."}
//	                    → 200 {"conversation":"C0123","id":"1723742401.001900"}
//	PATCH /v1/messages  {"conversation":"C0123","id":"...","text":"..."}
//	                    → 204   (replace the whole message)
//	PATCH /v1/messages  {"conversation":"C0123","id":"...","append":"..."}
//	                    → 204   (add a line to it)
//	                    → 200 {"conversation":"C0123:...","id":"..."} on rollover
//
// There is no platform field: an instance bridges the one platform it was
// started with, so the target is implied. The conversation is that platform's
// conversation key — for Slack a channel ID (post a new top-level message) or
// "channel:thread_ts" (post into an existing thread). The id returned by POST
// is also the thread the message roots, so a caller that wants to follow up
// under its own post sends "<conversation>:<id>" next.
//
// # Appending
//
// A slow incident wants one message that grows, not a wall of partial posts.
// The platforms only offer whole-message replacement, so appending means
// knowing what the message currently says — and the ingress remembers that
// for the messages it posted, rather than reading it back (which would need a
// history scope switchboard otherwise has no use for). That memory is
// deliberately small and deliberately lossy: it lives in this process, holds
// the last maxTrackedBodies messages, and is gone on restart. When it cannot
// answer, append fails with 409 and the caller resends the full text, which
// it can always do. Nothing is durable here; a caller that needs durability
// keeps its own timeline and uses text.
const (
	// ingressPath is the single route; both verbs share it.
	ingressPath = "/v1/messages"

	// maxIngressBody caps a request body. A digest is text, and the adapter
	// chunks anything long, so a megabyte is already far past generous.
	maxIngressBody = 1 << 20

	// idempotencyHeader carries the caller's replay key. A scheduler that
	// retries a request it never saw the response to reuses the key and gets
	// the original outcome back rather than double-posting. It matters most on
	// an append, which is the one verb here that is not naturally idempotent:
	// replaying it blind would write the same line twice.
	idempotencyHeader = "Idempotency-Key"

	// maxIdempotencyKeys bounds the in-memory replay map. Keys are evicted
	// oldest-first once it is full — a retry of a request that has since aged
	// out will run again, which is the trade for holding no state on disk.
	maxIdempotencyKeys = 1024

	// maxIdempotencyKeyLen keeps a caller from parking megabytes of header in
	// that map. Long enough for a UUID or a "<job>-<timestamp>" several times
	// over.
	maxIdempotencyKeyLen = 256

	// maxTrackedBodies bounds how many message bodies are remembered for
	// append, evicted oldest-first.
	maxTrackedBodies = 1024

	// platformTimeout bounds one call into the chat platform. slack-go's
	// default HTTP client has no timeout of its own, so without this a
	// blackholed connection would park a request — and every duplicate
	// waiting on it — indefinitely. Kept well inside Kubernetes' default 30s
	// termination grace so a shutdown can drain rather than sever.
	platformTimeout = 15 * time.Second
)

// ingressConfig constructs an ingress.
type ingressConfig struct {
	// Token is the bearer token callers must present. Required — deliberately
	// distinct from the daemon token: different direction, different trust.
	Token string
	// Allow is the set of conversations callers may post into. An entry is
	// either a full conversation key or a bare channel/space, which permits
	// every thread in it. Empty means every conversation the bot can reach.
	Allow []string
	// Out is the platform egress (the chat.Adapter serve built). If it also
	// implements chat.TextFitter, append is available.
	Out sender
	// Metrics may be nil (recording becomes a no-op).
	Metrics *metrics
	// Logf may be nil.
	Logf func(string, ...any)
}

// ingress serves the outbound message API over a chat adapter's egress.
type ingress struct {
	out     sender
	token   string
	allow   []string
	metrics *metrics
	logf    func(string, ...any)

	// fits reports whether a text stays in one platform message. Nil when the
	// egress does not implement chat.TextFitter, which disables append: with
	// no way to tell when a message is full there is no way to know when to
	// roll over, and silently truncating an incident timeline is worse than
	// not offering the verb.
	fits func(string) bool

	// mu guards both bounded maps below and their eviction order.
	mu sync.Mutex

	// ops holds one entry per idempotency key seen, in flight or completed, so
	// a concurrent duplicate waits for the original rather than racing it.
	ops       map[string]*opEntry
	opOrder   []string
	bodies    map[string]*bodyEntry
	bodyOrder []string
}

// opEntry is one idempotency key's operation: its result, the fingerprint of
// the request that started it, and the channel that releases waiters once it
// has been attempted.
type opEntry struct {
	ready       chan struct{}
	fingerprint string
	res         opResult
	err         error
}

// opResult is what an operation left behind: the message it wrote, and
// whether that is a *new* message the caller must address from now on (an
// append that rolled over into a continuation).
type opResult struct {
	ref    chat.MessageRef
	rolled bool
}

// bodyEntry is the remembered text of one message. Its mutex serializes the
// read-modify-write of an append — two concurrent appends to the same message
// would otherwise both read the same prior text and one line would vanish. It
// is held across the platform call, so appends to one message queue behind
// each other; platformTimeout bounds that queue.
type bodyEntry struct {
	mu   sync.Mutex
	text string
}

// newIngress validates the config and builds the ingress.
func newIngress(cfg ingressConfig) (*ingress, error) {
	if cfg.Token == "" {
		return nil, errors.New("ingress: token is required")
	}
	if cfg.Out == nil {
		return nil, errors.New("ingress: no chat egress")
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	i := &ingress{
		out:     cfg.Out,
		token:   cfg.Token,
		allow:   slices.Clone(cfg.Allow),
		metrics: cfg.Metrics,
		logf:    logf,
		ops:     make(map[string]*opEntry),
		bodies:  make(map[string]*bodyEntry),
	}
	if f, ok := cfg.Out.(chat.TextFitter); ok {
		i.fits = f.FitsOneMessage
	}
	return i, nil
}

// handler builds the ingress mux. Only ingressPath is served; anything else is
// a 404, so the surface is exactly the two documented verbs.
func (i *ingress) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(ingressPath, i.serveMessages)
	return mux
}

// serveIngress runs the outbound ingress on addr until ctx is cancelled.
// Callers start it in a goroutine; a bind failure returns immediately so serve
// can exit non-zero rather than silently dropping the surface a caller depends
// on.
func serveIngress(ctx context.Context, addr string, i *ingress) error {
	if addr == "" {
		<-ctx.Done()
		return nil
	}
	return serveHTTP(ctx, "ingress", addr, i.handler())
}

// serveMessages is the one route: authorize, dispatch on the verb, and turn
// whatever the handler returns into a status code, a JSON error body, and a
// metric. Handlers write their own success response and return nil.
func (i *ingress) serveMessages(w http.ResponseWriter, r *http.Request) {
	err := i.route(w, r)
	i.metrics.recordIngress(opLabel(r.Method), err)
	if err == nil {
		return
	}
	var ie *ingressError
	if !errors.As(err, &ie) {
		ie = errf(http.StatusInternalServerError, "internal error")
	}
	// The full error (cause included) goes to the log; the caller sees only
	// the message. Both the path and the error can quote caller-supplied text,
	// so strip control characters on the way out — a forged log line is not a
	// thing this should be able to write.
	i.logf("ingress %s %s: %d %s", r.Method, logSafe(r.URL.Path), ie.status, logSafe(err.Error()))
	writeJSON(w, ie.status, map[string]string{"error": ie.msg})
}

// opLabel maps a request method onto the bounded metric label.
func opLabel(method string) string {
	switch method {
	case http.MethodPost:
		return "post"
	case http.MethodPatch:
		return "patch"
	default:
		return "other"
	}
}

func (i *ingress) route(w http.ResponseWriter, r *http.Request) error {
	if err := i.authorize(w, r); err != nil {
		return err
	}
	switch r.Method {
	case http.MethodPost:
		return i.postMessage(w, r)
	case http.MethodPatch:
		return i.patchMessage(w, r)
	default:
		w.Header().Set("Allow", "POST, PATCH")
		return errf(http.StatusMethodNotAllowed, "method not allowed")
	}
}

// authorize checks the bearer token in constant time. The comparison leaks the
// token's length and nothing else.
func (i *ingress) authorize(w http.ResponseWriter, r *http.Request) error {
	const scheme = "bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(scheme) || !strings.EqualFold(h[:len(scheme)], scheme) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		return errf(http.StatusUnauthorized, "unauthorized")
	}
	if subtle.ConstantTimeCompare([]byte(h[len(scheme):]), []byte(i.token)) != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		return errf(http.StatusUnauthorized, "unauthorized")
	}
	return nil
}

// messageRequest is the body of both verbs. ID is empty on POST (the platform
// assigns it) and required on PATCH. Text and Append are alternatives on
// PATCH: replace the message, or add to it.
type messageRequest struct {
	Conversation string `json:"conversation"`
	ID           string `json:"id,omitempty"`
	Text         string `json:"text,omitempty"`
	Append       string `json:"append,omitempty"`
}

// messageResponse is the ref a POST hands back: everything the caller needs to
// edit the message later. An append that rolled over answers with the same
// shape, naming the continuation message to append to from now on.
type messageResponse struct {
	Conversation string `json:"conversation"`
	ID           string `json:"id"`
}

// postMessage posts a new message into a conversation and returns its ref.
func (i *ingress) postMessage(w http.ResponseWriter, r *http.Request) error {
	req, err := i.decodeMessage(w, r)
	if err != nil {
		return err
	}
	if req.ID != "" {
		return errf(http.StatusBadRequest, "id is set by the platform; PATCH to edit an existing message")
	}
	if req.Append != "" {
		return errf(http.StatusBadRequest, "append is only valid on PATCH; POST takes text")
	}
	if strings.TrimSpace(req.Text) == "" {
		return errf(http.StatusBadRequest, "text is required")
	}
	key, err := idempotencyKey(r)
	if err != nil {
		return err
	}
	res, err := i.do(r.Context(), key, fingerprint("post", req), func(ctx context.Context) (opResult, error) {
		ref, perr := i.post(ctx, chat.Reply{Conversation: req.Conversation, Text: req.Text})
		if perr != nil {
			return opResult{}, perr
		}
		i.track(req.Conversation, ref.ID, req.Text)
		return opResult{ref: ref}, nil
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, messageResponse{Conversation: res.ref.Conversation, ID: res.ref.ID})
	return nil
}

// patchMessage edits a message posted earlier — the half that matters for slow
// work: post once, then revise in place as results land, rather than a thread
// of partial posts. text replaces the whole message; append adds to what is
// already there. On a platform that cannot edit it answers 501.
func (i *ingress) patchMessage(w http.ResponseWriter, r *http.Request) error {
	req, err := i.decodeMessage(w, r)
	if err != nil {
		return err
	}
	if req.ID == "" {
		return errf(http.StatusBadRequest, "id is required")
	}
	hasText, hasAppend := strings.TrimSpace(req.Text) != "", strings.TrimSpace(req.Append) != ""
	switch {
	case hasText && hasAppend:
		return errf(http.StatusBadRequest, "text and append are alternatives; send one")
	case !hasText && !hasAppend:
		return errf(http.StatusBadRequest, "text or append is required")
	}
	key, err := idempotencyKey(r)
	if err != nil {
		return err
	}

	ref := chat.MessageRef{Conversation: req.Conversation, ID: req.ID}
	op := func(ctx context.Context) (opResult, error) { return i.replace(ctx, ref, req.Text) }
	if hasAppend {
		op = func(ctx context.Context) (opResult, error) { return i.appendTo(ctx, ref, req.Append) }
	}
	res, err := i.do(r.Context(), key, fingerprint("patch", req), op)
	if err != nil {
		return err
	}
	if res.rolled {
		// The message filled up. The caller gets the continuation's ref and
		// keeps appending to that one.
		writeJSON(w, http.StatusOK, messageResponse{Conversation: res.ref.Conversation, ID: res.ref.ID})
		return nil
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// replace rewrites a message's whole body, and resets what append remembers of
// it — a replace is the caller declaring the new full text.
func (i *ingress) replace(ctx context.Context, ref chat.MessageRef, text string) (opResult, error) {
	if err := i.update(ctx, ref, text); err != nil {
		return opResult{}, err
	}
	i.track(ref.Conversation, ref.ID, text)
	return opResult{ref: ref}, nil
}

// appendTo adds a line to a message this ingress remembers the text of,
// rolling over into a continuation message in the same thread once the
// platform's single-message limit is in reach.
func (i *ingress) appendTo(ctx context.Context, ref chat.MessageRef, add string) (opResult, error) {
	if i.fits == nil {
		return opResult{}, errf(http.StatusNotImplemented,
			"this platform does not support append; send the full text")
	}
	be := i.tracked(ref.Conversation, ref.ID)
	if be == nil {
		return opResult{}, errf(http.StatusConflict,
			"no remembered text for this message; send the full text instead")
	}
	// Held across the platform call so concurrent appends to one message
	// cannot each read the same prior text and lose a line.
	be.mu.Lock()
	defer be.mu.Unlock()
	if be.text == "" {
		return opResult{}, errf(http.StatusConflict,
			"no remembered text for this message; send the full text instead")
	}

	combined := be.text + "\n" + add
	if !i.fits(combined) {
		cont, err := i.post(ctx, chat.Reply{Conversation: continuationKey(ref), Text: add})
		if err != nil {
			return opResult{}, err
		}
		i.track(cont.Conversation, cont.ID, add)
		return opResult{ref: cont, rolled: true}, nil
	}
	if err := i.update(ctx, ref, combined); err != nil {
		return opResult{}, err
	}
	be.text = combined
	return opResult{ref: ref}, nil
}

// continuationKey is where an overflowing message's next part goes: the thread
// the message is already in, or — for a top-level message — the thread it
// roots, so the timeline stays in one place instead of scattering across the
// channel.
func continuationKey(ref chat.MessageRef) string {
	if strings.Contains(ref.Conversation, ":") {
		return ref.Conversation
	}
	return ref.Conversation + ":" + ref.ID
}

// decodeMessage reads and validates the JSON body shared by both verbs. The
// per-verb rules about text, append and id live in the verbs.
func (i *ingress) decodeMessage(w http.ResponseWriter, r *http.Request) (messageRequest, error) {
	var req messageRequest
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mt, _, err := mime.ParseMediaType(ct)
		if err != nil || mt != "application/json" {
			return req, errf(http.StatusUnsupportedMediaType, "body must be application/json")
		}
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxIngressBody))
	// Strict: a typo'd field ("message" for "text") is a silent no-op
	// otherwise, and this contract is young enough to be worth policing.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return req, errf(http.StatusRequestEntityTooLarge, "request body too large")
		}
		return req, errf(http.StatusBadRequest, "malformed request body: %v", err)
	}
	// One object, nothing after it: two concatenated bodies would otherwise
	// silently apply only the first.
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return req, errf(http.StatusBadRequest, "malformed request body: unexpected data after the JSON object")
	}

	req.Conversation = strings.TrimSpace(req.Conversation)
	req.ID = strings.TrimSpace(req.ID)
	if req.Conversation == "" {
		return req, errf(http.StatusBadRequest, "conversation is required")
	}
	// A conversation key is an opaque platform identifier, never prose: no
	// platform's key holds whitespace or control characters, and both the log
	// line and the error body below quote it back.
	if hasBlankOrControl(req.Conversation) || hasBlankOrControl(req.ID) {
		return req, errf(http.StatusBadRequest, "conversation and id must not contain whitespace or control characters")
	}
	if !i.permitted(req.Conversation) {
		return req, errf(http.StatusForbidden, "conversation %q is not in the ingress allowlist", req.Conversation)
	}
	return req, nil
}

// idempotencyKey reads and bounds the caller's replay key.
func idempotencyKey(r *http.Request) (string, error) {
	key := r.Header.Get(idempotencyHeader)
	if len(key) > maxIdempotencyKeyLen {
		return "", errf(http.StatusBadRequest, "%s must be at most %d characters",
			idempotencyHeader, maxIdempotencyKeyLen)
	}
	return key, nil
}

// fingerprint digests the request an idempotency key was first used for, so
// reusing that key for a *different* request is caught instead of silently
// replaying the old result — a caller that recycles a key by mistake would
// otherwise get 200 and a ref to someone else's message while its own never
// posted.
func fingerprint(op string, req messageRequest) string {
	h := sha256.New()
	for _, part := range []string{op, req.Conversation, req.ID, req.Text, req.Append} {
		fmt.Fprintf(h, "%d:%s", len(part), part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// logSafe replaces control characters so caller-supplied text cannot forge a
// line in the log.
func logSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, s)
}

// hasBlankOrControl reports whether s holds a space or control character.
func hasBlankOrControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	})
}

// permitted reports whether the allowlist admits a conversation. An empty
// allowlist admits everything (serve warns at startup); an entry matches the
// whole key, or a bare channel/space, which admits every thread in it.
func (i *ingress) permitted(conv string) bool {
	if len(i.allow) == 0 {
		return true
	}
	for _, a := range i.allow {
		if conv == a || strings.HasPrefix(conv, a+":") {
			return true
		}
	}
	return false
}

// do runs op at most once per idempotency key. An unkeyed request runs
// straight through. A keyed one publishes an in-flight entry before starting
// so a concurrent duplicate waits on it instead of racing, and the entry is
// dropped again if the op failed — a failure is not a result worth replaying,
// and the caller's retry should really retry.
func (i *ingress) do(ctx context.Context, key, fp string, op func(context.Context) (opResult, error)) (opResult, error) {
	if key == "" {
		return op(ctx)
	}
	i.mu.Lock()
	if e, ok := i.ops[key]; ok {
		mismatch := e.fingerprint != fp
		i.mu.Unlock()
		if mismatch {
			return opResult{}, errf(http.StatusConflict,
				"%s was already used for a different request", idempotencyHeader)
		}
		if err := awaitReady(ctx, e.ready); err != nil {
			return opResult{}, err
		}
		return e.res, e.err
	}
	e := &opEntry{ready: make(chan struct{}), fingerprint: fp}
	i.remember(key, e)
	i.mu.Unlock()

	// close(ready) is deferred: if op panics, net/http recovers it in this
	// goroutine and every waiter on this key would otherwise block forever on
	// a channel nobody is left to close.
	done := false
	defer func() {
		if !done {
			e.err = errf(http.StatusInternalServerError, "internal error")
		}
		if e.err != nil {
			i.forget(key, e)
		}
		close(e.ready)
	}()
	e.res, e.err = op(ctx)
	done = true
	return e.res, e.err
}

// awaitReady waits for an identical in-flight request to finish, or for this
// caller to give up first — a waiter must not be pinned to the publisher's
// fate.
func awaitReady(ctx context.Context, ready <-chan struct{}) error {
	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		return wrapf(http.StatusGatewayTimeout, ctx.Err(),
			"gave up waiting for an identical in-flight request")
	}
}

// platformContext detaches a platform call from the caller's connection. A
// caller that hangs up (or times out client-side) must not abort a post the
// platform may already be committing — that would leave a message posted, its
// idempotency entry dropped, and the retry double-posting. The call is instead
// bounded by platformTimeout.
func platformContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), platformTimeout)
}

// post is the single platform send, with its metric and error mapping.
func (i *ingress) post(ctx context.Context, reply chat.Reply) (chat.MessageRef, error) {
	ctx, cancel := platformContext(ctx)
	defer cancel()
	ref, err := i.out.Send(ctx, reply)
	i.metrics.recordReply(err)
	if err != nil {
		return ref, statusFor(err,
			"this platform cannot post messages",
			"the chat platform rejected the message")
	}
	if ref.ID == "" {
		// The adapter posted nothing (everything rendered away). Report it
		// rather than handing back a ref that cannot be edited.
		return ref, errf(http.StatusBadGateway, "the chat platform posted no message")
	}
	return ref, nil
}

// update is the single platform edit, with its metric and error mapping.
func (i *ingress) update(ctx context.Context, ref chat.MessageRef, text string) error {
	ctx, cancel := platformContext(ctx)
	defer cancel()
	err := i.out.Update(ctx, ref, chat.Reply{Conversation: ref.Conversation, Text: text})
	i.metrics.recordReply(err)
	if err != nil {
		return statusFor(err,
			"this platform cannot edit messages",
			"the chat platform rejected the edit")
	}
	return nil
}

// statusFor maps a platform failure onto a status code. The distinction that
// matters to a caller is permanent versus transient: 5xx invites a retry, and
// an escalation daemon that retries "channel does not exist" forever is worse
// than one that gives up and logs. cause is carried for the log only.
func statusFor(err error, unsupported, generic string) *ingressError {
	switch {
	case errors.Is(err, chat.ErrUnsupported):
		return wrapf(http.StatusNotImplemented, err, "%s", unsupported)
	case errors.Is(err, chat.ErrNotFound):
		return wrapf(http.StatusNotFound, err, "no such conversation or message")
	case errors.Is(err, chat.ErrDenied):
		return wrapf(http.StatusForbidden, err, "the chat platform refused: check the bot's access")
	}
	return wrapf(http.StatusBadGateway, err, "%s", generic)
}

// remember records an idempotency entry, evicting oldest-first once the map is
// full. The caller holds i.mu.
func (i *ingress) remember(key string, e *opEntry) {
	i.ops[key] = e
	i.opOrder = append(i.opOrder, key)
	for len(i.opOrder) > maxIdempotencyKeys {
		delete(i.ops, i.opOrder[0])
		i.opOrder = i.opOrder[1:]
	}
}

// forget drops a failed operation's entry, but only if it is still the one we
// recorded, so a retry that already replaced it is left alone.
func (i *ingress) forget(key string, e *opEntry) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.ops[key] != e {
		return
	}
	delete(i.ops, key)
	i.opOrder = slices.DeleteFunc(i.opOrder, func(k string) bool { return k == key })
}

// bodyKey identifies a message for the append memory. A message id is unique
// within its channel and the thread part of a conversation key plays no role
// in addressing it, so "C0123" and "C0123:1723742400.0001" name the same
// message when the id matches — a caller need not echo the exact conversation
// string it posted with.
func bodyKey(conv, id string) string {
	channel, _, _ := strings.Cut(conv, ":")
	return channel + "\x00" + id
}

// track remembers a message's current text so a later append can extend it.
// Text that does not fit in one message was split across several by the
// adapter, and the ref names only the first — remembering it would make append
// silently edit a fragment, so that message is forgotten instead.
func (i *ingress) track(conv, id, text string) {
	if id == "" {
		return
	}
	if i.fits == nil || !i.fits(text) {
		i.untrack(conv, id)
		return
	}
	k := bodyKey(conv, id)
	i.mu.Lock()
	be, ok := i.bodies[k]
	if !ok {
		be = &bodyEntry{}
		i.bodies[k] = be
		i.bodyOrder = append(i.bodyOrder, k)
		for len(i.bodyOrder) > maxTrackedBodies {
			delete(i.bodies, i.bodyOrder[0])
			i.bodyOrder = i.bodyOrder[1:]
		}
	}
	i.mu.Unlock()
	// i.mu is released first: an in-flight append holds be.mu across a
	// platform call, and blocking every other request behind it would be a
	// self-inflicted outage.
	be.mu.Lock()
	be.text = text
	be.mu.Unlock()
}

// untrack forgets a message's text, so a later append is told to send the full
// text rather than extending something stale.
func (i *ingress) untrack(conv, id string) {
	k := bodyKey(conv, id)
	i.mu.Lock()
	defer i.mu.Unlock()
	if _, ok := i.bodies[k]; !ok {
		return
	}
	delete(i.bodies, k)
	i.bodyOrder = slices.DeleteFunc(i.bodyOrder, func(x string) bool { return x == k })
}

// tracked returns the remembered body of a message, or nil.
func (i *ingress) tracked(conv, id string) *bodyEntry {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.bodies[bodyKey(conv, id)]
}

// ingressError is a handler failure carrying the status and the caller-facing
// message. cause is logged but never serialized: a platform error can name
// internals a caller has no business seeing. (msg itself may quote the
// caller's own input back — that is theirs already.)
type ingressError struct {
	status int
	msg    string
	cause  error
}

func (e *ingressError) Error() string {
	if e.cause != nil {
		return e.msg + ": " + e.cause.Error()
	}
	return e.msg
}

func (e *ingressError) Unwrap() error { return e.cause }

func errf(status int, format string, a ...any) *ingressError {
	return &ingressError{status: status, msg: fmt.Sprintf(format, a...)}
}

func wrapf(status int, cause error, format string, a ...any) *ingressError {
	return &ingressError{status: status, msg: fmt.Sprintf(format, a...), cause: cause}
}

// writeJSON writes v as the response body with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
