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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
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
// The typed lifecycle events below are separate names switchboard consumes but
// does not relay verbatim.
//
// The daemon does not list this one in the event types it advertises: it is the
// legacy name, and the capabilities frame describes the logical surface, where
// the same traffic appears as "stream-chunk", "tool-call" and "tool-result".
const EventAgent = "agent"

// The typed lifecycle events switchboard consumes. None is relayed verbatim:
// EventUsage and EventTurnComplete are between them the only source for what a
// turn cost (see TurnUsage), and EventTurnError is the only announcement that a
// turn died inside the daemon — without it the thread simply goes quiet (#34).
//
// EventStatusUpdate and EventCapabilities describe the session rather than a
// turn's output: what the daemon is doing right now, and what it can do at all.
const (
	EventUsage        = "usage-update"
	EventTurnComplete = "turn-complete"
	EventTurnError    = "turn-error"
	EventStatusUpdate = "status-update"
	EventCapabilities = "capabilities"
)

// The advertised names for the traffic that arrives on EventAgent. Nothing
// subscribes to these — the wire event stays "agent" — but they are what a
// capabilities frame calls the surface switchboard reads every answer and every
// tool notice out of, so they are the names to check it against. EventToolResult
// is here for the pairing; switchboard reads the calls and not their results.
const (
	EventStreamChunk = "stream-chunk"
	EventToolCall    = "tool-call"
	EventToolResult  = "tool-result"
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

// Turn states, per the daemon's event-stream spec §2.2. As with the turn-error
// kinds, an unrecognized value must not be forced into one of these: the spec
// reserves the right to add states, and a gateway that reads anything it does
// not know as "idle" would end turns that are still running.
//
// Only two of the four are observed in practice. core-agent declares all four
// and emits exactly streaming and idle; the awaiting_ pair is spec surface with
// no emission site as of protocol 1.5.0. They are modeled anyway because the
// consumer has to be written for the state it will be handed, not the state it
// sees today, and the shorthand for "streaming or nothing" quietly makes a
// permission prompt look like the end of a turn.
const (
	TurnStateIdle               = "idle"
	TurnStateStreaming          = "streaming"
	TurnStateAwaitingPermission = "awaiting_permission"
	TurnStateAwaitingElicit     = "awaiting_elicit"
)

// SessionStatus is what an EventStatusUpdate says the session is doing. The
// event also carries the model, provider, permission mode and context usage;
// none of those has a consumer here, and modeling a field nothing reads only
// invites someone to trust it.
type SessionStatus struct {
	// TurnState is one of the TurnState constants, or a value this build does
	// not know. Always present on a conformant frame — the spec requires it on
	// every emission, snapshot or delta.
	TurnState string
}

// Working reports whether the daemon is actively running a turn.
func (s SessionStatus) Working() bool { return s.TurnState == TurnStateStreaming }

// Blocked reports whether the turn has stopped on something only a human can
// answer — a permission prompt or an elicitation. It is emphatically not Idle:
// the turn is still owed, and treating it as over would retire the thread's
// progress message while the daemon waits. No daemon emits either state today
// (see the constants), so this is a guard against the first one that does.
func (s SessionStatus) Blocked() bool {
	return s.TurnState == TurnStateAwaitingPermission || s.TurnState == TurnStateAwaitingElicit
}

// Idle reports whether the session is between turns.
func (s SessionStatus) Idle() bool { return s.TurnState == TurnStateIdle }

// statusFrame is the JSON payload of an EventStatusUpdate event.
type statusFrame struct {
	TurnState string `json:"turn_state"`
}

// StatusUpdated parses an EventStatusUpdate payload. ok is false on a parse
// failure or a frame with no turn_state, which is the only field this reads —
// a status update that does not say what the session is doing is not one.
func StatusUpdated(data string) (s SessionStatus, ok bool) {
	var f statusFrame
	if err := json.Unmarshal([]byte(data), &f); err != nil {
		return SessionStatus{}, false
	}
	if f.TurnState == "" {
		return SessionStatus{}, false
	}
	return SessionStatus{TurnState: f.TurnState}, true
}

// Capabilities is the frame the daemon opens every stream with: what it is,
// what protocol it speaks, and which events it will actually send.
//
// Switchboard does not negotiate on it — there is nothing to negotiate, the
// daemon sends what it sends. It is read so an operator can be told when the
// daemon on the other end will not be sending something switchboard relies on,
// which otherwise shows up as a feature quietly not working: no error notice
// for a failed turn, no usage footer, no turn boundary for the progress clock.
type Capabilities struct {
	// ProtocolVersion is the event-stream spec version, e.g. "1.5.0".
	ProtocolVersion string
	// Server is the daemon's self-description, e.g. "core-agent/0.9.2".
	Server string
	// EventTypes is the logical event surface the daemon advertises. It is not
	// the set of SSE event names: the daemon lists "stream-chunk", "tool-call"
	// and "tool-result" separately even though all three ride on EventAgent,
	// and does not list EventAgent itself. Nothing may test for "agent" here.
	EventTypes []string
}

// Advertises reports whether the daemon said it sends this event type.
func (c Capabilities) Advertises(t string) bool {
	return slices.Contains(c.EventTypes, t)
}

// Missing returns the given event types the daemon did not advertise, in the
// order asked. Empty when the daemon advertised nothing at all: a frame with no
// event_types is an older or partial implementation, and reporting every type
// as missing would be a wall of noise about one fact.
func (c Capabilities) Missing(want ...string) []string {
	if len(c.EventTypes) == 0 {
		return nil
	}
	var out []string
	for _, t := range want {
		if !c.Advertises(t) {
			out = append(out, t)
		}
	}
	return out
}

// capabilitiesFrame is the JSON payload of an EventCapabilities event.
type capabilitiesFrame struct {
	ProtocolVersion string   `json:"protocol_version"`
	Server          string   `json:"server"`
	EventTypes      []string `json:"event_types"`
}

// StreamOpened parses an EventCapabilities payload. ok is false on a parse
// failure or a frame carrying none of the three fields, which says nothing
// worth logging.
func StreamOpened(data string) (c Capabilities, ok bool) {
	var f capabilitiesFrame
	if err := json.Unmarshal([]byte(data), &f); err != nil {
		return Capabilities{}, false
	}
	if f.ProtocolVersion == "" && f.Server == "" && len(f.EventTypes) == 0 {
		return Capabilities{}, false
	}
	return Capabilities{
		ProtocolVersion: f.ProtocolVersion,
		Server:          f.Server,
		EventTypes:      f.EventTypes,
	}, true
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
					ID   string         `json:"id"`
					Name string         `json:"name"`
					Args map[string]any `json:"args"`
				} `json:"functionCall"`
				FunctionResponse *struct {
					ID       string         `json:"id"`
					Name     string         `json:"name"`
					Response map[string]any `json:"response"`
				} `json:"functionResponse"`
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

// ToolCall is one tool invocation the agent started.
type ToolCall struct {
	// ID is the daemon's identifier for the call, echoed on the result that
	// eventually answers it. Empty for a daemon that does not send one, which
	// leaves the caller to pair by name.
	ID string
	// Name is the tool, e.g. "bash".
	Name string
	// Arg is a one-line summary of a single argument, or empty when the call
	// has no argument that can be summarised safely. See summariseArg for what
	// "safely" is doing there — it is a disclosure decision, not a formatting
	// one, and it is deliberately lossy.
	Arg string
}

// ToolResult is one tool result: whether the call finished, and — if it
// failed — a short descriptor of how. Never the output. See ToolResults.
type ToolResult struct {
	ID     string
	Name   string
	Failed bool
	// Detail is a few words about the failure, e.g. "exit 1". Empty for a
	// success, and for a failure whose shape this does not recognise.
	Detail string
}

// ToolCalls returns the tool (function) calls carried by an EventAgent
// payload, in the order they appear, and is empty for events that carry none
// (plain model text, tool results, user turns, or a parse failure). It pairs
// with AgentText: an agent event is either model text (AgentText ok) or tool
// activity (ToolCalls non-empty), letting the gateway surface "the agent is
// running <tool>" progress distinctly from the answer text.
func ToolCalls(data string) []ToolCall {
	var f agentFrame
	if err := json.Unmarshal([]byte(data), &f); err != nil {
		return nil
	}
	if f.Event == nil || f.Event.Content == nil || f.Event.Content.Role != "model" {
		return nil
	}
	var calls []ToolCall
	for _, p := range f.Event.Content.Parts {
		if p.FunctionCall == nil || p.FunctionCall.Name == "" {
			continue
		}
		calls = append(calls, ToolCall{
			ID:   p.FunctionCall.ID,
			Name: p.FunctionCall.Name,
			Arg:  summariseArg(p.FunctionCall.Args),
		})
	}
	return calls
}

// ToolResults returns the tool results carried by an EventAgent payload.
//
// Unlike ToolCalls this does not filter on role. A result is authored by the
// tool, not the model, and the daemon labels the event "user" — the same label
// it puts on an injected turn echoed back. Requiring "model" here is how tool
// results stayed invisible to the gateway for as long as they did.
//
// What comes back is a verdict, never the output. The response object of a
// single `kubectl get pods -A` carries the whole listing, and a progress notice
// is not where that belongs — it belongs in the answer, if the model decides it
// does.
func ToolResults(data string) []ToolResult {
	var f agentFrame
	if err := json.Unmarshal([]byte(data), &f); err != nil {
		return nil
	}
	if f.Event == nil || f.Event.Content == nil {
		return nil
	}
	var results []ToolResult
	for _, p := range f.Event.Content.Parts {
		if p.FunctionResponse == nil || p.FunctionResponse.Name == "" {
			continue
		}
		failed, detail := verdict(p.FunctionResponse.Response)
		results = append(results, ToolResult{
			ID:     p.FunctionResponse.ID,
			Name:   p.FunctionResponse.Name,
			Failed: failed,
			Detail: detail,
		})
	}
	return results
}

// verdict reads success or failure out of a tool response object.
//
// There is no schema for this: every tool answers in its own shape. Two
// conventions are common enough to read — a numeric exit_code, and an error
// field — and anything else is reported as success, because a tool that
// answered at all usually did run. Guessing failure from an unrecognised shape
// would put a red cross against calls that worked.
func verdict(resp map[string]any) (failed bool, detail string) {
	if resp == nil {
		return false, ""
	}
	// An HTTP status is not an exit code: 0 is not success and 200 is not
	// failure. Read it on its own terms, and only in the failing direction.
	// A 4xx or 5xx settles the call, but a 2xx says only that the request
	// arrived — the least specific signal in the object, not the most — so
	// {"status_code": 200, "error": "connection reset"} must go on to read the
	// error rather than stop here calling it a success.
	// Both spellings, as with exit_code below: statusCode is what a tool built
	// on Node reports, since that is the field name on its response object.
	for _, key := range []string{"status_code", "statusCode"} {
		code, ok := resp[key].(float64)
		if !ok {
			continue
		}
		if code >= 400 && code < 600 && code == float64(int(code)) {
			return true, "HTTP " + strconv.Itoa(int(code))
		}
	}
	for _, key := range []string{"exit_code", "exitCode", "returncode"} {
		code, ok := resp[key]
		if !ok {
			continue
		}
		n, ok := code.(float64) // encoding/json numbers
		if !ok {
			continue
		}
		if n == 0 {
			return false, ""
		}
		// Non-zero is a failure whatever the number is, but the number itself
		// is tool-authored and goes into a chat room, so only a plausible exit
		// code is rendered. Anything else fails with no detail: int() on an
		// out-of-range float64 is not even defined in Go, and "exit
		// 1234567890123456789" is not what "a few words" means.
		// The range test comes first and short-circuits, so int() is only ever
		// reached for a value it can hold — NaN and ±Inf fail both comparisons.
		inRange := n >= minExitCode && n <= maxExitCode
		if !inRange || n != float64(int(n)) {
			return true, ""
		}
		return true, "exit " + strconv.Itoa(int(n))
	}
	for _, key := range []string{"error", "err"} {
		v, ok := resp[key]
		if !ok || !truthy(v) {
			continue
		}
		// Deliberately not the error text: it is tool-authored, unbounded, and
		// as able to carry a secret as the arguments are.
		return true, ""
	}
	return false, ""
}

// truthy reports whether a JSON value in an "error" field actually says an
// error happened. A tool that reports success as `"error": false`, `"error": 0`
// or `"error": {}` is as common as one that omits the field, and reading mere
// presence as failure marks every one of those calls ❌.
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		return strings.TrimSpace(t) != ""
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

// minExitCode and maxExitCode bound what verdict is willing to render as an
// exit code. A signal-killed process reports a negative code and a shell
// reports 0-255; the ceiling is loose enough for the tools that report an HTTP
// status here instead.
const (
	minExitCode = -256
	maxExitCode = 65535
)

// argSummaryCap bounds one argument summary. Long enough for a recognisable
// command, short enough that a notice stays a notice.
const argSummaryCap = 120

// preferredArgs are the argument names worth showing, in order. Each is the
// one field that says what a call is actually doing — the command a shell is
// running, the file being read — so a thread full of `bash` becomes a thread
// full of distinguishable lines.
var preferredArgs = []string{"command", "cmd", "path", "file_path", "filename", "query", "url", "pattern"}

// summariseArg picks one argument out of a tool call and renders it as a
// single clamped, redacted line. It returns "" when there is nothing scalar to
// show.
//
// # Why this is lossy on purpose
//
// Tool arguments are untrusted, unbounded, and are exactly where a secret turns
// up: a shell command line with a token in it, a private path, the contents of
// a file being written. Posting them into a shared channel is a disclosure
// decision, and this is the whole of that decision:
//
//   - one argument, never the object. A tool called with a token in a
//     second field does not leak it because the first field is what is shown.
//   - scalars only. Nested objects and arrays are skipped rather than
//     serialised, so the size of what can be disclosed stays bounded by one
//     field rather than by the call.
//   - flattened to one line, and clamped to argSummaryCap on both sides of the
//     redaction pass — see safeArg — so a file body passed as an argument
//     contributes at most its first sentence.
//   - run through redact, which knocks out the credential shapes it knows.
//
// That last step is a net, not a guarantee — no pattern set recognises every
// secret, and the ones above are what keeps the blast radius of a miss to one
// clamped field. A deployment that cannot accept even that should run
// progress-mode status, which names tools and shows no arguments, or
// indicator, which shows neither.
func summariseArg(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	for _, key := range preferredArgs {
		if s, ok := scalar(args[key]); ok {
			return safeArg(s)
		}
	}
	// No preferred key: fall back to the first scalar by name. Sorted, because
	// map order is random and a notice that renders a different argument on
	// every reconnect replay is worse than one that renders none.
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if s, ok := scalar(args[k]); ok {
			return safeArg(s)
		}
	}
	return ""
}

// safeArg turns one raw argument into the line that goes in a chat room:
// clamped, redacted, and clamped again.
//
// The order matters in both directions. Clamping first bounds the work the
// redaction pass does on an argument that may be a whole file body. Clamping
// again after it is what makes argSummaryCap true: `<redacted>` is longer than
// some of what it replaces, so redaction can *grow* a string, and a bound that
// only holds before the growth is not a bound. A second cut can land inside a
// `<redacted>` marker, which is ugly and still redacted — the direction the
// tradeoff should fail in.
//
// The first cut is to redactWindow rather than to argSummaryCap, and it lands
// on a word boundary, because a cut through the middle of a token destroys the
// very shape redaction matches on: an `sk-` key truncated below its length
// floor stops looking like a key, and its head is then published where the
// whole thing would have been elided. Widening the window does not fix that on
// its own — redaction *shrinks* what it matches, so three long secrets ahead of
// the key can pull the cut back into view no matter where it was put. Keeping
// whole tokens does fix it: a credential is one token, so it is either wholly
// inside the window and matched, or wholly outside it and absent. Absent is
// safe; half of one is not.
func safeArg(s string) string {
	return clampArg(redact(clampWords(s, redactWindow)))
}

// scalar renders a JSON value as text if it is one, and reports false for the
// composites (objects, arrays, null) that summariseArg refuses to flatten.
func scalar(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		if strings.TrimSpace(t) == "" {
			return "", false
		}
		return t, true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(t), true
	}
	return "", false
}

// redactWindow bounds the input the redaction pass sees. An argument can be a
// whole file body, and while RE2 is linear there is no reason to scan a
// megabyte to produce 120 bytes. Comfortably wider than argSummaryCap so that
// redaction, which shrinks what it matches, still has material to fill the
// visible summary with — see safeArg.
const redactWindow = 8 * argSummaryCap

// clampArg flattens an argument to one line and bounds it to argSummaryCap.
func clampArg(s string) string { return clamp(s, argSummaryCap) }

// clampWords bounds s to roughly limit bytes without ever splitting a word,
// running on to the end of the one the limit falls inside. That overshoot is
// the point: this is the window redaction reads, not text anyone sees, and a
// pattern cannot recognise a credential it has been handed half of. A single
// token longer than the limit is returned whole, since there is no boundary to
// stop at before it ends.
func clampWords(s string, limit int) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if len(s) <= limit {
		return s
	}
	if i := strings.IndexByte(s[limit:], ' '); i >= 0 {
		return s[:limit+i]
	}
	return s
}

// clamp flattens s to one line and bounds it to limit bytes, cutting on a rune
// boundary so a chat client is never handed half a rune to render.
func clamp(s string, limit int) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if len(s) <= limit {
		return s
	}
	cut := limit - 1
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// secretish matches the credential shapes worth knocking out of an argument
// summary: a value assigned to a suggestively named flag or variable, and the
// issued-token prefixes that are recognisable on their own.
//
// Conservative by construction — it can only ever catch what it has seen
// before. See summariseArg for why that is accepted rather than solved here.
// The credential words, and the two ways a name can be built around them.
//
// What the word may be followed by is where the false positives are decided.
// A separator continues a credential name (AWS_SECRET_ACCESS_KEY), and so do
// digits (SECRET1) and the plurals that are themselves credential words
// (--secrets, "credentials":). A letter does not, which is what tells those
// from --max_tokens (a sampling parameter), tokenizer:rename and
// /var/log/tokens:latest — a summary that redacts ordinary commands teaches
// people to ignore it, and this repository's own tooling is full of
// --max-tokens. `token` is therefore the one word left unpluralised: a
// `tokens` is nearly always a count, and reading it as a secret costs more
// than the rare `TOKENS=` it would catch.
//
// What comes *before* the word is deliberately unconstrained: a credential
// word at the tail of a longer name is still a credential name, so mytoken=
// and accessToken: both match, and the leading text is simply left outside the
// match and preserved.
const (
	credWord = `(?:api[-_]?keys?|secrets?|token|passwo?r?ds?|passwds?|credentials?)`
	// A variable or field name: AWS_SECRET_ACCESS_KEY, github.token, TOKEN.
	credName = `(?:[a-z0-9_.-]*[_.-])?` + credWord + `(?:[_.-][a-z0-9_.-]*|[0-9]+)?`
	// A flag name. `auth` and `bearer` are here and not in credName because as
	// bare words they are far too common — and even here the boundary matters:
	// unanchored, the `auth` in curl's real `--anyauth` matched, so
	// `--anyauth -u alice:letmein` hid the flag and left the credential.
	credFlag = `(?:[a-z0-9-]*-)?(?:` + credWord + `|auth|bearer)(?:-[a-z0-9-]*)?`
)

var secretish = regexp.MustCompile(
	// "api_key": "xyz" — a quoted value, ended by its own closing quote so the
	// JSON punctuation around it survives.
	`(?i:("?` + credName + `"?\s*[:=]\s*")[^"]*)` +
		// TOKEN=xyz, --token=xyz — an unquoted value adjacent to the operator.
		`|(?i:("?` + credName + `"?\s*[:=])[^\s"]+)` +
		// password: hunter2 — an unquoted value a space after the colon.
		`|(?i:("?` + credName + `"?\s*:\s+)\S+)` +
		// api_key = abc, export TOKEN = abc — the same, with an operator that
		// has space on *both* sides. Requiring the leading space is what stops
		// an empty flag value reaching forward to take the next word:
		// `run --token= next` became `run --token= <redacted>`, hiding an
		// argument and redacting no secret. `--token=` has no space before its
		// operator, so it can only ever match the adjacent form above, which
		// needs a value hard against it.
		`|(?i:("?` + credName + `"?\s+=\s+)\S+)` +
		// --password xyz: the same names separated by a space rather than by an
		// operator, but only behind a flag. Without that anchor the "auth" in
		// `gcloud auth login` swallows the subcommand.
		`|(?i:(--?` + credFlag + `\s+)\S+)` +
		// Authorization: Bearer xyz — the header form, where the credential
		// follows the scheme rather than the field name. Matching the field
		// name instead would elide the word "Bearer" and leave the token, so
		// `auth` is absent from the alternatives above. The length floor keeps
		// prose ("Bearer authentication is required") out of it.
		`|(?i:(bearer\s+)[A-Za-z0-9._\-/+=]{16,})` +
		// Bare issued credentials: GitHub, Slack, OpenAI, AWS, Google, JWTs.
		// Case-sensitive, deliberately: these prefixes are issued in one
		// casing, so matching either way only adds false positives. `sk-` also
		// carries a length floor, because an issued key is dozens of characters
		// and `sk-` alone appears inside ordinary words — it was redacting
		// `s3://bucket/some-sk-thing`, which destroys information and finds
		// no secret.
		`|\b(?:gh[pousr]_|github_pat_|xox[baprs]-|sk-[A-Za-z0-9_-]{20,}|AKIA|ASIA|ya29\.|AIza|eyJ[A-Za-z0-9_-]{6})[A-Za-z0-9._\-/+]*` +
		// anything shaped like a PEM block header
		`|(?i:-----BEGIN[^-]*-----)`)

// urlCreds matches a password in a URL's userinfo: postgres://user:pw@host,
// and redis://:pw@host, where the username is empty. It is a pass of its own
// because the trailing "@" is what distinguishes a credential from an ordinary
// host:port, and it has to be matched to be required — RE2 has no lookahead —
// which means putting it back afterwards.
//
// Having to reach forward for that "@" is why the character class is as narrow
// as it is. The argument has already been flattened to one line, so whitespace
// alone does not bound the search: in {"url":"http://host:8080","email":
// "alice@example.com"} the run from the port to a perfectly ordinary email
// address is unbroken, and a class that allowed quotes and commas ate the lot.
// Nothing that terminates a URL in running text belongs in a userinfo field.
var urlCreds = regexp.MustCompile(`(://[^:@/?\s"',;<>{}\[\]]*:)[^@/?\s"',;<>{}\[\]]+@`)

// redact blanks the credential shapes it knows about, keeping the flag, key
// name or scheme so the line still says what kind of thing was elided. At most
// one of secretish's six prefix groups can match at a time — they are in
// different alternatives — so the other five expand to nothing.
func redact(s string) string {
	s = urlCreds.ReplaceAllString(s, "${1}<redacted>@")
	return secretish.ReplaceAllString(s, "${1}${2}${3}${4}${5}${6}<redacted>")
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
	return c.subscribe(ctx, sess, assertedCaller, since, nil, fn)
}

// subscribe is Subscribe with a hook that fires once the daemon has accepted
// the stream — after the status line, so a rejection is still an error and
// never a connection. HeadSeq is the only caller that needs it: what it
// measures is silence on an *open* stream, and a clock started before the dial
// would time the connection as if it were the daemon having nothing to say.
func (c *Client) subscribe(ctx context.Context, sess Session, assertedCaller string, since int64, onConnect func(), fn func(Event) error) error {
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
	if onConnect != nil {
		onConnect()
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

const (
	// headProbeQuiet is how long a probe stream must carry nothing before
	// HeadSeq concludes the daemon has finished replaying what it holds. The
	// replay is written as fast as the connection takes it, so a gap this long
	// mid-backlog would mean the daemon is stalled — in which case a probe that
	// waited longer would only be more wrong about the same thing.
	headProbeQuiet = 300 * time.Millisecond
)

// headProbeCap bounds the whole probe, for a daemon that never goes quiet
// because the session is mid-turn. What comes back then is a head from partway
// through that turn, which is the conservative direction: the frames after it
// are new work, and relaying new work is the point.
//
// A var only so a test can shorten it — the branch it guards is reached by
// waiting, and five seconds of waiting is not worth testing at full price.
// Nothing outside a test assigns it.
var headProbeCap = 5 * time.Second

// HeadSeq reports the highest event seq the daemon currently holds for a
// session, so a reader that wants only what happens *next* can subscribe from
// there. It is how switchboard adopts a session it did not create (#38): the
// alternative, subscribing from 0, replays the daemon's whole window — for an
// incident session that has been running for an hour, straight into the chat
// thread.
//
// The protocol offers no way to ask, so this is a measurement: open the
// stream, read the backlog, and take the last seq on it. The stream is closed
// once it has been quiet for headProbeQuiet, or at headProbeCap, whichever
// comes first — so a call normally costs a fraction of a second, and at worst
// headProbeCap, which is what a session mid-turn costs because it never goes
// quiet. Neither deadline is an error once the stream is open — the quiet one
// is how this ends, and the cap yields a head from partway through a live turn,
// which is the conservative direction. Running out of time *before* the daemon
// accepts the stream is a different thing entirely, and is reported as an
// error: nothing was measured and nothing was refused.
//
// The other half of this call matters just as much. A session with nothing to
// replay reports 0 — exactly what a session that does not exist would report —
// so the existence check is the subscribe itself: a session the daemon has
// never heard of answers with a 404, which comes back here as a *StatusError.
// Binding a thread to a session that is not there is the failure this exists to
// make loud.
func (c *Client) HeadSeq(ctx context.Context, sess Session, assertedCaller string) (int64, error) {
	probe, cancel := context.WithTimeout(ctx, headProbeCap)
	defer cancel()

	// The last moment the open stream was known to be carrying something —
	// written by the connect hook and the event callback (both of which run on
	// this goroutine) and read by the watchdog below, so it is atomic rather
	// than plain.
	//
	// Held as nanoseconds since the call began, plus one, rather than as a wall
	// clock: elapsed time here is read from the monotonic clock, which a
	// mid-probe NTP step cannot move. On the wall clock a step backwards would
	// keep the quiet window from ever closing (a settled session then costs the
	// whole cap, and the caller's post waits for it) and a step forwards would
	// close it mid-backlog, resuming the thread partway through a turn. The
	// plus-one keeps zero meaning "the stream is not open yet", and the watchdog
	// waits for that: a quiet clock started at call time would be measuring the
	// dial, and a daemon that took longer than headProbeQuiet to answer would
	// have its request cancelled underneath it. That failure is invisible in the
	// result — 0, and no error — which is a head of "replay everything" and an
	// existence check that says yes about a session that is not there.
	start := time.Now()
	var lastFrame atomic.Int64
	alive := func() { lastFrame.Store(int64(time.Since(start)) + 1) }
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(headProbeQuiet / 2)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-probe.Done():
				return
			case <-t.C:
				at := lastFrame.Load()
				if at != 0 && time.Since(start)-time.Duration(at-1) >= headProbeQuiet {
					cancel()
					return
				}
			}
		}
	}()

	var head int64
	err := c.subscribe(probe, sess, assertedCaller, 0, alive, func(ev Event) error {
		alive()
		if ev.Type != EventAgent {
			return nil
		}
		// Seq, not text: a tool call is as much a position in the stream as an
		// answer is, and AgentText fills in the seq of every frame it can parse
		// whether or not it found anything worth relaying.
		if r, _ := AgentText(ev.Data); r.Seq > head {
			head = r.Seq
		}
		return nil
	})
	// Our own deadline firing is the expected way this ends. The caller's
	// context going away is not: that is a real failure, and reporting a head
	// read from half a backlog would have the thread resume mid-turn.
	if err != nil && ctx.Err() == nil && probe.Err() == nil {
		return 0, err
	}
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if lastFrame.Load() == 0 {
		// The cap ran out with the stream never open. Nothing was measured and
		// nothing was refused, so there is no answer to give: saying 0 here
		// would be the two silent failures above wearing a return value.
		return 0, fmt.Errorf("daemon: session %s/%s: the event stream did not open within %s",
			sess.App, sess.ID, headProbeCap)
	}
	return head, nil
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
