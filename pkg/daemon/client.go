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

// Package daemon is switchboard's thin client for the frozen core-agent
// daemon contract. A chat gateway maps a conversation (a Slack thread, a
// Google Chat space) onto a core-agent session and shuttles turns across
// the four verbs core-agent already ships:
//
//	POST   /sessions                  -> create a session (returns SessionID)
//	POST   /sessions/<sid>/inject     -> queue a user message on its inbox
//	POST   /sessions/<sid>/wake       -> nudge a sleeping session to run a turn
//	GET    /sessions/<sid>/events     -> SSE stream of the session's output
//
// Auth is a static Bearer token; per-turn attribution rides the
// X-Asserted-Caller header (the daemon stamps it as the session Owner and
// resolves per-caller MCP credentials from it — the seam where W0 and W1
// meet). This mirrors k8s-lookout's pkg/inject wire client; switchboard
// adds the /wake and /events verbs the interactive round-trip needs.
package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Config captures the daemon-side surface switchboard posts against.
// Constructed from CLI flags / env in main.go.
type Config struct {
	// BaseURL is the scheme + host + port with NO trailing slash —
	// e.g. "http://127.0.0.1:7777".
	BaseURL string

	// BearerToken authenticates switchboard to the daemon. Loaded
	// from an env var by main.go (never a bare flag).
	BearerToken string

	// HTTPClient lets tests swap in a client pointed at an
	// httptest.Server. Nil in production.
	HTTPClient *http.Client
}

// Client is the thinnest wire client that covers the interactive
// chat round-trip. It is safe for concurrent use.
type Client struct {
	cfg  Config
	http *http.Client
}

// New validates the config and returns a Client.
func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("daemon: BaseURL is required")
	}
	if strings.HasSuffix(cfg.BaseURL, "/") {
		return nil, fmt.Errorf("daemon: BaseURL must not end with '/' (got %q)", cfg.BaseURL)
	}
	if cfg.BearerToken == "" {
		return nil, errors.New("daemon: BearerToken is required")
	}
	hc := cfg.HTTPClient
	if hc == nil {
		// No client-wide Timeout: it would also bound reading the
		// response body, which force-closes the long-lived Subscribe SSE
		// stream. Unary calls get a deadline via context in do() instead;
		// Subscribe is bounded only by its caller's context.
		hc = &http.Client{}
	}
	return &Client{cfg: cfg, http: hc}, nil
}

// unaryTimeout bounds a single create/inject/wake round-trip. It is
// applied per-request via context so it never touches the streaming
// Subscribe path.
const unaryTimeout = 30 * time.Second

// Session identifies a core-agent session. The daemon namespaces every
// session under an app; switchboard addresses sessions by the
// app-qualified route (/sessions/<app>/<id>/...) rather than the
// /sessions/<id>/... shortcut, which the daemon rejects with 409 when an
// id is ambiguous across apps. Both fields come back from CreateSession.
type Session struct {
	App string
	ID  string
}

// path builds an app-qualified session route with the given suffix
// (e.g. "/inject", "/events", or "" for the session root).
func (s Session) path(suffix string) string {
	return "/sessions/" + s.App + "/" + s.ID + suffix
}

// CreateSession opens a new session and returns it. assertedCaller (may
// be empty) is the identity the daemon stamps as Owner; switchboard must
// be listed in the daemon's attach.multi_session.proxy_identities for it
// to be honored. The daemon requires an authenticated, non-anonymous
// caller here, so a create against a multi-session daemon needs either a
// real bearer identity or a valid asserted caller.
func (c *Client) CreateSession(ctx context.Context, assertedCaller string) (Session, error) {
	// The daemon reads no request body on create but browserWriteGuard
	// still requires Content-Type: application/json, which do() sets for
	// the (empty) struct payload below.
	var out struct {
		App       string `json:"app"`
		SessionID string `json:"sessionID"` // daemon uses camelCase, not session_id
	}
	if err := c.do(ctx, http.MethodPost, "/sessions", assertedCaller, struct{}{}, &out); err != nil {
		return Session{}, err
	}
	if out.SessionID == "" {
		return Session{}, errors.New("daemon: create session returned empty sessionID")
	}
	if out.App == "" {
		// Without an app, every app-qualified route would be malformed
		// (/sessions//<id>/...). Fail loudly at create instead.
		return Session{}, errors.New("daemon: create session returned empty app")
	}
	return Session{App: out.App, ID: out.SessionID}, nil
}

// Inject queues a user message on the session's inbox and, as part of that,
// runs a turn — the daemon requests its own wake on inject, so callers must
// not follow this with Wake or the session runs the turn twice.
// assertedCaller attributes this turn to the originating chat user.
func (c *Client) Inject(ctx context.Context, sess Session, assertedCaller, text string) error {
	body := map[string]string{"message": text}
	return c.do(ctx, http.MethodPost, sess.path("/inject"), assertedCaller, body, nil)
}

// Wake runs a turn on a session with no new input to give it. It is not the
// companion to Inject — see Inject — but part of the daemon's frozen verb set
// and the way to rouse a session that has gone idle on its own.
func (c *Client) Wake(ctx context.Context, sess Session, assertedCaller string) error {
	return c.do(ctx, http.MethodPost, sess.path("/wake"), assertedCaller, struct{}{}, nil)
}

// Event is one server-sent event from a session's output stream. Type is
// the SSE event name (e.g. "agent", "status-update", "turn-complete") and
// Data is its raw JSON payload; use AgentText to pull assistant text out
// of an "agent" event.
type Event struct {
	Type string
	Data string
}

// EventAgent is the SSE event name carrying the agent's streamed output —
// model text, tool calls, and tool results are all multiplexed onto it.
// The typed lifecycle events ("status-update", "turn-error") are separate
// names switchboard does not relay verbatim.
const EventAgent = "agent"

// The typed lifecycle events switchboard consumes. None is relayed verbatim:
// EventUsage and EventTurnComplete are between them the only source for what a
// turn cost (see TurnUsage), and EventTurnError is the only announcement that a
// turn died inside the daemon — without it the thread simply goes quiet (#34).
const (
	EventUsage        = "usage-update"
	EventTurnComplete = "turn-complete"
	EventTurnError    = "turn-error"
)

// UsageTotals is a session's running accounting, as carried by every
// EventUsage event.
//
// The totals are what a caller wanting a *conversational* turn's cost has to
// work from, because the daemon does not report one. Its "turn" is a single
// model call: one user message that drives five tool calls produces six
// EventUsage events, each with a last_turn describing only the call that just
// finished, and turns_total climbing by six. Confirmed live on 2026-08-19 —
// last_turn.tokens_in was 5,660 on the final event of a turn that had
// consumed 33,340. Differencing these totals across the turn is the only
// figure that is not an undercount.
type UsageTotals struct {
	// Model is the model that ran the most recent call.
	Model string
	// TokensIn, TokensOut and CostUSD are the session's cumulative totals.
	TokensIn, TokensOut int64
	CostUSD             float64
	// Calls is the daemon's turns_total: model calls, not user turns.
	Calls int64
}

// TurnUsage is the accounting for one conversational turn. No single event
// carries it: the tokens and cost come from differencing UsageTotals across
// the turn, and Latency from "turn-complete".
type TurnUsage struct {
	// Model is the model that ran the turn, e.g. "gemini-3.7-flash".
	Model string
	// TokensIn and TokensOut are the turn's prompt and completion tokens.
	TokensIn, TokensOut int64
	// CostUSD is the turn's cost in US dollars, from "usage-update" only.
	CostUSD float64
	// Latency is the turn's wall-clock duration, from "turn-complete" only.
	Latency time.Duration
}

// Empty reports whether the usage carries nothing worth showing.
func (u TurnUsage) Empty() bool {
	return u.Model == "" && u.TokensIn == 0 && u.TokensOut == 0 && u.CostUSD == 0 && u.Latency == 0
}

// usageFrame is the JSON payload of an EventUsage event. last_turn is
// modeled only for the model name it carries — see UsageTotals for why its
// token and cost figures are not what a conversational turn cost.
// The totals are pointers so a report of zero can be told from a payload that
// is not a usage report at all: the daemon's subscribe-time priming event is
// all zeros on a fresh session and is the baseline everything after it is
// differenced against, so discarding it would cost the session's first turn
// its numbers.
type usageFrame struct {
	TokensIn  *int64   `json:"tokens_in_total"`
	TokensOut *int64   `json:"tokens_out_total"`
	CostUSD   *float64 `json:"cost_usd_total"`
	Calls     *int64   `json:"turns_total"`
	LastTurn  *struct {
		Model string `json:"model"`
	} `json:"last_turn"`
}

// turnCompleteFrame is the JSON payload of an EventTurnComplete event. Unlike
// EventUsage it fires exactly once per conversational turn, and its
// latency_ms spans the whole of it — but its tokens describe only the last
// model call, so they are deliberately not modeled.
type turnCompleteFrame struct {
	Model     string `json:"model"`
	LatencyMS int64  `json:"latency_ms"`
}

// SessionUsage parses an EventUsage payload and reports the session's running
// totals. A report of all zeros is valid and meaningful — it is what a fresh
// session's priming event says — so ok is false only for a payload carrying no
// totals at all: a parse failure, or an object with none of the fields.
func SessionUsage(data string) (t UsageTotals, ok bool) {
	var f usageFrame
	if err := json.Unmarshal([]byte(data), &f); err != nil {
		return UsageTotals{}, false
	}
	if f.TokensIn == nil && f.TokensOut == nil && f.CostUSD == nil && f.Calls == nil {
		return UsageTotals{}, false
	}
	if f.TokensIn != nil {
		t.TokensIn = *f.TokensIn
	}
	if f.TokensOut != nil {
		t.TokensOut = *f.TokensOut
	}
	if f.CostUSD != nil {
		t.CostUSD = *f.CostUSD
	}
	if f.Calls != nil {
		t.Calls = *f.Calls
	}
	if f.LastTurn != nil {
		t.Model = f.LastTurn.Model
	}
	return t, true
}

// TurnCompleted parses an EventTurnComplete payload for the two things only
// it knows: that a conversational turn has ended, and how long it took. ok is
// false on a parse failure or a payload with neither.
func TurnCompleted(data string) (u TurnUsage, ok bool) {
	var f turnCompleteFrame
	if err := json.Unmarshal([]byte(data), &f); err != nil {
		return TurnUsage{}, false
	}
	u = TurnUsage{Model: f.Model, Latency: time.Duration(f.LatencyMS) * time.Millisecond}
	return u, !u.Empty()
}

// TurnError kinds, mirroring the daemon's event-stream spec §2.6. Unknown
// values must be treated as TurnErrorUnknown — the spec reserves the right to
// add categories, and a gateway that switches exhaustively on today's list
// would go silent on tomorrow's.
const (
	TurnErrorConfig        = "config_error"
	TurnErrorAuth          = "auth_error"
	TurnErrorModelNotFound = "model_not_found"
	TurnErrorRateLimited   = "rate_limited"
	TurnErrorTransientNet  = "transient_network"
	// TurnErrorCostCeiling and TurnErrorWatchdog are the two guardrail trips.
	// They differ from every other kind in what they demand of the reader: the
	// agent refuses further turns until an operator resets it, so "try again"
	// is actively wrong advice.
	TurnErrorCostCeiling = "cost_ceiling"
	TurnErrorWatchdog    = "watchdog"
	TurnErrorUnknown     = "unknown"
)

// TurnError is a turn that failed inside the daemon, as carried by an
// EventTurnError event. The daemon emits one only for failures an operator
// should see — a retry that succeeded is not one — so every TurnError received
// is worth surfacing.
type TurnError struct {
	// Kind is the stable category, one of the TurnError* constants. Treat an
	// unrecognized value as TurnErrorUnknown rather than dropping the event.
	Kind string
	// Code is the provider's status code when one could be extracted
	// ("NOT_FOUND", "429"), empty otherwise.
	Code string
	// Message is a single human-readable line — but not a bounded one. The
	// daemon's classifier caps what it produces, and the guardrail trips
	// bypass that classifier and build their message directly, interpolating
	// text a caller does not control (the watchdog's trigger reason). Clamp
	// before rendering.
	Message string
	// Retryable is the daemon's own judgement on whether sending the same
	// message again could work.
	Retryable bool
	// Hint is the most actionable next step when one is obvious, empty
	// otherwise — e.g. which IAM role the runtime service account is missing.
	Hint string
}

// guardrailRefusal is the clause the daemon puts in every guardrail message,
// cost ceiling and watchdog alike. Matching prose is a fallback, not the
// primary signal — see Guardrail.
const guardrailRefusal = "refuse new turns until the operator resets it"

// Guardrail reports whether the failure is a tripped guardrail, which the agent
// will not clear on its own: it refuses new turns until an operator resets it.
// The distinction matters to a reader, because unlike every other terminal
// failure there is a specific thing to go and do.
//
// Only the *first* trip carries one of the two guardrail kinds. Every turn sent
// afterwards is refused before it runs, and that refusal is an ordinary error
// routed through the daemon's generic classifier, whose categories do not cover
// guardrails — so it arrives as TurnErrorUnknown. Since the refusals outnumber
// the trip (one trip, then every message the reader sends until someone
// resets), keying on kind alone would get the advice right once and wrong
// thereafter. Hence the message fallback: if the clause stops matching some day
// the reader gets the generic terminal notice, which is exactly what they would
// have got without it.
func (e TurnError) Guardrail() bool {
	if e.Kind == TurnErrorCostCeiling || e.Kind == TurnErrorWatchdog {
		return true
	}
	return strings.Contains(e.Message, guardrailRefusal)
}

// turnErrorFrame is the JSON payload of an EventTurnError event.
type turnErrorFrame struct {
	Kind      string `json:"kind"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	Hint      string `json:"hint"`
}

// TurnFailed parses an EventTurnError payload. ok is false on a parse failure
// or a payload that says nothing — a frame with neither a kind nor a message
// cannot be rendered into a notice worth posting, and announcing "something
// went wrong" with no detail is worse than the silence it replaces.
//
// An unrecognized kind is preserved rather than normalized: it still routes
// correctly (only the two guardrail kinds are special-cased) and the raw value
// is more use in a log than "unknown" would be. A frame with a message but no
// kind is reported as TurnErrorUnknown.
func TurnFailed(data string) (e TurnError, ok bool) {
	var f turnErrorFrame
	if err := json.Unmarshal([]byte(data), &f); err != nil {
		return TurnError{}, false
	}
	if f.Kind == "" && f.Message == "" {
		return TurnError{}, false
	}
	e = TurnError{
		Kind:      f.Kind,
		Code:      f.Code,
		Message:   f.Message,
		Retryable: f.Retryable,
		Hint:      f.Hint,
	}
	if e.Kind == "" {
		e.Kind = TurnErrorUnknown
	}
	return e, true
}

// agentFrame is the JSON payload of an EventAgent event. It wraps an ADK
// session.Event, whose own fields carry no JSON tags and so serialize
// under their Go names (Content, Partial, Author, ...); the nested
// genai.Content does carry tags (parts, role, text). Only the fields
// switchboard needs are modeled.
type agentFrame struct {
	Seq   int64 `json:"seq"`
	Event *struct {
		Content *struct {
			Parts []struct {
				Text         string `json:"text"`
				FunctionCall *struct {
					Name string `json:"name"`
				} `json:"functionCall"`
			} `json:"parts"`
			Role string `json:"role"`
		} `json:"Content"`
		Partial bool   `json:"Partial"`
		Author  string `json:"Author"`
	} `json:"event"`
}

// AgentReply is the assistant text carried by one EventAgent event.
type AgentReply struct {
	// Seq is the event's monotonic sequence number; feed the last one
	// back to Subscribe as `since` to resume a stream without replaying.
	Seq int64
	// Text is the concatenated model-authored text of the event, empty
	// for tool-call/tool-result-only or user-authored events.
	Text string
	// Partial is true for an in-progress streaming chunk. The daemon
	// repeats the full text in a final Partial:false event, so relaying
	// only non-partial events yields one message per assistant turn.
	Partial bool
}

// AgentText parses an EventAgent payload and reports the model-authored
// text it carries. ok is false when the event is not model output worth
// relaying (a parse failure, a non-agent/user-authored event, or an event
// with no text — e.g. a tool call). Callers typically relay r.Text when
// ok && !r.Partial && r.Text != "".
func AgentText(data string) (r AgentReply, ok bool) {
	var f agentFrame
	if err := json.Unmarshal([]byte(data), &f); err != nil {
		return AgentReply{}, false
	}
	r.Seq = f.Seq
	if f.Event == nil || f.Event.Content == nil {
		return AgentReply{Seq: f.Seq}, false
	}
	// role is "model" for assistant output and "user" for injected turns
	// echoed back onto the stream; only the former is relayed.
	if f.Event.Content.Role != "model" {
		return AgentReply{Seq: f.Seq}, false
	}
	r.Partial = f.Event.Partial
	var b strings.Builder
	for _, p := range f.Event.Content.Parts {
		b.WriteString(p.Text)
	}
	r.Text = b.String()
	if r.Text == "" {
		return r, false
	}
	return r, true
}

// ToolCalls returns the names of the tool (function) calls carried by an
// EventAgent payload, in the order they appear, and is empty for events that
// carry none (plain model text, tool results, user turns, or a parse failure).
// It pairs with AgentText: an agent event is either model text (AgentText ok)
// or tool activity (ToolCalls non-empty), letting the gateway surface "the
// agent is running <tool>" progress distinctly from the answer text.
func ToolCalls(data string) []string {
	var f agentFrame
	if err := json.Unmarshal([]byte(data), &f); err != nil {
		return nil
	}
	if f.Event == nil || f.Event.Content == nil || f.Event.Content.Role != "model" {
		return nil
	}
	var names []string
	for _, p := range f.Event.Content.Parts {
		if p.FunctionCall != nil && p.FunctionCall.Name != "" {
			names = append(names, p.FunctionCall.Name)
		}
	}
	return names
}

// protocolVersion is the attach protocol switchboard speaks. The daemon
// accepts any declaration sharing its major version (additive minor/patch
// fields) and 409s on a major mismatch, so this must track the daemon's
// major. Sending it lets the daemon reject an incompatible client early
// instead of streaming frames switchboard cannot parse.
const protocolVersion = "1.4.0"

// Subscribe opens the SSE stream for a session and delivers events to fn
// until the context is cancelled, the stream ends, or fn returns an
// error. It is the read half of the round-trip: the gateway relays these
// back into the chat thread. since replays frames with seq greater than
// it (0 = from the start of the daemon's replay window); track the last
// seq from AgentText to resume without re-delivering old turns.
func (c *Client) Subscribe(ctx context.Context, sess Session, assertedCaller string, since int64, fn func(Event) error) error {
	q := url.Values{}
	q.Set("since", strconv.FormatInt(since, 10))
	q.Set("protocol", protocolVersion)
	req, err := c.newRequest(ctx, http.MethodGet, sess.path("/events")+"?"+q.Encode(), assertedCaller, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return c.statusErr(resp)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var ev Event
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "": // dispatch on blank-line boundary
			if ev.Type != "" || ev.Data != "" {
				if err := fn(ev); err != nil {
					return err
				}
			}
			ev = Event{}
		case strings.HasPrefix(line, "event:"):
			ev.Type = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			d := strings.TrimSpace(line[len("data:"):])
			if ev.Data != "" {
				ev.Data += "\n" + d
			} else {
				ev.Data = d
			}
		}
	}
	return sc.Err()
}

// do performs a JSON request/response round-trip. out may be nil when
// the response body is not needed. Each call is bounded by unaryTimeout
// via context (not the http.Client, which must stay timeout-free for the
// streaming Subscribe path).
func (c *Client) do(ctx context.Context, method, path, assertedCaller string, in, out any) error {
	ctx, cancel := context.WithTimeout(ctx, unaryTimeout)
	defer cancel()
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("daemon: marshal request: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := c.newRequest(ctx, method, path, assertedCaller, body)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.statusErr(resp)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("daemon: decode response: %w", err)
		}
	}
	return nil
}

func (c *Client) newRequest(ctx context.Context, method, path, assertedCaller string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.BearerToken)
	if assertedCaller != "" {
		req.Header.Set("X-Asserted-Caller", assertedCaller)
	}
	return req, nil
}

func (c *Client) statusErr(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(b))
	if msg == "" {
		msg = resp.Status
	}
	return &StatusError{
		Method:     resp.Request.Method,
		Path:       resp.Request.URL.Path,
		StatusCode: resp.StatusCode,
		Message:    msg,
	}
}

// StatusError is a non-2xx response from the daemon. The status code lets a
// caller distinguish a terminal client error (4xx: bad request, unknown
// session) from a transient server-side failure (5xx) worth telling the user
// to retry rather than treating as a lost cause.
type StatusError struct {
	Method     string
	Path       string
	StatusCode int
	Message    string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("daemon: %s %s: %s", e.Method, e.Path, e.Message)
}

// Transient reports whether the failure is the daemon's fault rather than the
// request's: a 5xx means the same request could succeed on retry, a 4xx means
// it won't.
func (e *StatusError) Transient() bool {
	return e.StatusCode >= 500
}

// IsTransient reports whether err is worth a chat-facing "try again" rather
// than a hard failure notice. A *StatusError defers to its own Transient; any
// other error (network failure, timeout, connection refused — the request
// never got a structured rejection from the daemon) is treated as transient,
// since it says nothing about the request itself being invalid.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.Transient()
	}
	return true
}
