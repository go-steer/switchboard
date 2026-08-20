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

// Package chat defines the provider-neutral seam between a chat platform
// and switchboard's session router. Each platform (Slack, Google Chat)
// implements Adapter; the router (cmd/switchboard) owns the mapping from
// a conversation to a core-agent session and shuttles turns across the
// pkg/daemon contract. Keeping the platform specifics behind this
// interface is what lets Slack land first (Phase 1) and Google Chat
// follow (Phase 3) without touching the router.
package chat

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Message is one inbound turn from a chat platform, normalized across
// providers.
type Message struct {
	// Conversation is the stable key the router maps onto a core-agent
	// session — a Slack channel+thread_ts, or a Google Chat space+thread.
	// Same Conversation across turns => same session.
	Conversation string

	// Channel is the platform channel/space the conversation lives in,
	// independent of thread — a Slack channel ID or a Google Chat space.
	// The router uses it for channel-scoped gateway settings (e.g. a
	// per-channel progress mode) that apply across every thread in the
	// channel.
	Channel string

	// Caller is the platform identity of the human who sent the message,
	// in the daemon's asserted-caller form (e.g. "alice@example.com").
	// The router forwards it as X-Asserted-Caller so the daemon attributes
	// the turn and resolves per-caller MCP credentials (W0).
	Caller string

	// Text is the message body with platform mention markup stripped.
	Text string
}

// CallerMode selects how a platform user maps onto the daemon's
// X-Asserted-Caller identity. It lives here rather than in one adapter
// because the choice is the same question on every platform, and the
// answer has to be consistent: the daemon keys per-caller credentials off
// whatever arrives, so two adapters asserting different forms for the same
// human would look like two people.
type CallerMode string

const (
	// CallerEmail asserts the sender's email address, which is the form
	// the daemon's per-caller credential lookup expects. An adapter that
	// cannot obtain one for a given turn falls back to the platform ID
	// rather than dropping the turn.
	CallerEmail CallerMode = "email"
	// CallerID asserts the raw platform user ID — a Slack U0123ABC, a
	// Google Chat users/1234567890 — with no lookup and no extra scope.
	CallerID CallerMode = "id"
)

// ParseCallerMode validates a caller-mode string, reporting whether it
// named a mode.
func ParseCallerMode(s string) (CallerMode, bool) {
	switch CallerMode(s) {
	case CallerEmail:
		return CallerEmail, true
	case CallerID:
		return CallerID, true
	}
	return "", false
}

// ReplyKind classifies what a Reply *is*, so an adapter can present it in
// the platform's idiom rather than as one undifferentiated blob of text: a
// progress placeholder can render as a card with a spinner, an error notice
// in a warning colour, a command acknowledgment as a compact one-liner. It is
// deliberately provider-neutral — the router says what a message means and
// the adapter decides how that looks, which is the same division of labour
// as the rest of this seam. An adapter that ignores Kind entirely still
// behaves correctly: every Reply carries Text that says the same thing.
type ReplyKind string

const (
	// KindAnswer is an agent turn — the answer itself. The zero value, so a
	// Reply built without a Kind is treated as content.
	KindAnswer ReplyKind = ""
	// KindProgress is the transient "working on it" placeholder posted on
	// wake and retired when the answer lands.
	KindProgress ReplyKind = "progress"
	// KindActivity names the tools an agent is currently running.
	KindActivity ReplyKind = "activity"
	// KindNotice is a gateway-level warning — a turn that could not run.
	KindNotice ReplyKind = "notice"
	// KindAck is a gateway command's acknowledgment (see Handler.HandleCommand).
	KindAck ReplyKind = "ack"
)

// Reply is one outbound turn switchboard relays back into a conversation.
type Reply struct {
	// Conversation echoes Message.Conversation so the adapter posts into
	// the originating thread.
	Conversation string

	// Text is the reply body in the platform's markup dialect (the
	// adapter is responsible for any final formatting).
	Text string

	// Kind classifies the reply so an adapter can render it in the
	// platform's idiom. The zero value (KindAnswer) is an agent turn.
	Kind ReplyKind

	// Usage is what the turn cost, for an adapter to render as a footer on
	// the reply. Nil — the usual case — means show nothing: the router only
	// populates it for an answer, and only when the operator opted in.
	Usage *Usage
}

// Usage is the token and cost accounting for the turn a Reply carries. It is
// structured rather than pre-rendered because the platforms have genuinely
// different places to put it (a Block Kit context block, a Chat card footer)
// and may want to arrange it differently; Line is the shared one-line form
// both use today.
type Usage struct {
	// Model is the model that ran the turn, e.g. "gemini-3.7-flash".
	Model string
	// TokensIn and TokensOut are the turn's prompt and completion tokens.
	TokensIn, TokensOut int64
	// CostUSD is the turn's cost in US dollars.
	CostUSD float64
	// Latency is the turn's wall-clock duration.
	Latency time.Duration
}

// Line renders the usage as one line of plain text — e.g.
// "gemini-3.7-flash · 5,000 in / 1 out · $0.0038 · 3.1s". A field the daemon
// did not report is left out rather than shown as a zero, so a partial
// report degrades to a shorter line; an empty Usage renders as "" and the
// adapter then shows no footer at all.
func (u Usage) Line() string {
	var parts []string
	if u.Model != "" {
		parts = append(parts, u.Model)
	}
	if u.TokensIn > 0 || u.TokensOut > 0 {
		parts = append(parts, commas(u.TokensIn)+" in / "+commas(u.TokensOut)+" out")
	}
	if u.CostUSD > 0 {
		parts = append(parts, formatCost(u.CostUSD))
	}
	if u.Latency > 0 {
		parts = append(parts, formatLatency(u.Latency))
	}
	return strings.Join(parts, " · ")
}

// formatCost renders a dollar amount at a precision that stays informative
// across four orders of magnitude: a single cheap turn costs a fraction of a
// cent, where two decimals would round every turn to "$0.00".
func formatCost(c float64) string {
	switch {
	case c < 0.0001:
		return "<$0.0001"
	case c < 1:
		return fmt.Sprintf("$%.4f", c)
	default:
		return fmt.Sprintf("$%.2f", c)
	}
}

// formatLatency renders a turn duration the way people say it: milliseconds
// under a second, then one decimal place of seconds.
func formatLatency(d time.Duration) string {
	if d < time.Second {
		return strconv.FormatInt(d.Milliseconds(), 10) + "ms"
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// commas groups an integer in thousands. Token counts are the one number
// here people compare at a glance, and "5,000" is legible where "5000" is
// merely readable.
func commas(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		s = "-" + s
	}
	return s
}

// MessageRef identifies a reply already posted to a conversation so the
// router can update or delete it later — the mechanism behind long-turn
// progress indicators and in-place status edits. It is opaque to the
// router; only the originating Adapter interprets ID (for Slack, the posted
// message's timestamp). A zero MessageRef (empty ID) means "no message"
// and is safe to pass to Update/Delete, which no-op on it.
type MessageRef struct {
	// Conversation echoes the Reply's conversation so the adapter can
	// locate the message (Slack needs the channel to edit or delete).
	Conversation string
	// ID is the platform message identifier (Slack message ts).
	ID string
}

// ErrUnsupported is returned by Update/Delete on a platform that cannot
// edit or remove an already-posted message. The router treats it as
// non-fatal and degrades to plain Send, so progress features never break a
// platform that lacks them.
var ErrUnsupported = errors.New("chat: operation not supported by this platform")

// ErrNotFound and ErrDenied classify a platform's refusal as permanent, so a
// caller can tell "this will never work" from "try again later". An Adapter
// wraps them alongside its own error (the platform's own wording stays in the
// message; only the classification is portable) when the platform reports a
// missing conversation or message, or refuses the operation outright — the
// bot is not in the channel, the channel is archived, the message belongs to
// someone else. Everything else is left unclassified and treated as
// transient.
var (
	ErrNotFound = errors.New("chat: no such conversation or message")
	ErrDenied   = errors.New("chat: the platform refused the operation")
)

// TextFitter is an optional Adapter capability: reporting whether a text fits
// in a single platform message once the adapter has rendered it. Send already
// splits anything longer across several messages, so this matters only to a
// caller that must keep one editable message — the outbound ingress appending
// to a running timeline, which rolls over into a new message rather than
// letting the platform truncate one. An Adapter that does not implement it
// simply never triggers that rollover.
type TextFitter interface {
	// FitsOneMessage reports whether text renders into a single message.
	FitsOneMessage(text string) bool
}

// Command is a normalized gateway control command — a platform's native
// slash command (Slack /switchboard, a Google Chat slash command) or a
// recognized mention subcommand (@switchboard progress status). Unlike a
// Message it never reaches the daemon: it configures the gateway itself.
// Adding a platform means mapping its native command surface onto this
// type; the router interprets Name/Args and never learns the platform.
type Command struct {
	// Channel is the platform channel/space the command applies to. Gateway
	// settings are channel-scoped, so a Command carries the channel rather
	// than a full conversation key.
	Channel string

	// Caller is the invoker's asserted-caller identity (for logging/authz).
	Caller string

	// Name is the command verb, lower-cased, e.g. "progress".
	Name string

	// Args are the remaining whitespace-separated tokens, e.g. ["status"].
	Args []string
}

// Handler receives normalized inbound turns and gateway commands. The
// router implements it; an Adapter calls Handle once per inbound message
// and HandleCommand once per recognized command.
type Handler interface {
	// Handle processes one inbound message (an agent turn).
	Handle(ctx context.Context, msg Message) error

	// HandleCommand processes a gateway control command and returns a short,
	// human-readable acknowledgment for the adapter to surface to the invoker
	// (ephemerally for a slash command, or as a thread reply for a mention
	// subcommand). An error is returned only for an internal failure; an
	// unknown or malformed command yields a helpful ack, not an error.
	HandleCommand(ctx context.Context, cmd Command) (string, error)
}

// CommandChoices is an optional Handler capability: reporting the values a
// gateway setting accepts. It exists so an adapter can name those values in
// whatever way its platform affords — spelled out in the message text, or, on a
// platform whose interactive controls actually reach the app, offered as
// buttons that re-invoke HandleCommand with the chosen value as the single
// argument, exactly as typing it would have. The handler stays the authority on
// what a command means; the adapter learns only the surface, so no platform
// gains a special case in the router and no router vocabulary is hard-coded in
// an adapter.
//
// It is asked outside a command too: the Google Chat adapter, its only caller
// today, uses it for the welcome a new space gets, which names the values
// before anyone has run anything. So a Handler that does not implement it (or
// returns nil for a command it has no fixed choices for) is not merely
// falling back to its own acknowledgment text — on that surface there is no
// acknowledgment, and whatever the adapter says instead names nothing. An
// adapter must still work without it, and a Handler that has a fixed list is
// better off reporting it.
type CommandChoices interface {
	// Choices returns the accepted argument values for the named command,
	// or nil when it takes none, is unknown, or is free-form.
	Choices(name string) []string
}

// Adapter is a single chat platform's ingress + egress. Run blocks,
// delivering inbound messages to h until ctx is cancelled; Send posts a
// reply back to the platform. Implementations live in
// pkg/chat/slack and pkg/chat/googlechat.
type Adapter interface {
	// Name identifies the platform for logs and metrics ("slack",
	// "googlechat").
	Name() string

	// Run consumes the platform's event source (Slack Socket Mode,
	// Google Chat Pub/Sub) and dispatches each inbound turn to h. It
	// returns when ctx is cancelled or the source fails unrecoverably.
	Run(ctx context.Context, h Handler) error

	// Send posts a reply into its originating conversation and returns a
	// ref to the posted message so it can later be updated or deleted. When
	// a reply spans multiple platform messages, the ref identifies the
	// first one.
	Send(ctx context.Context, r Reply) (MessageRef, error)

	// Update replaces the content of a previously posted message. It
	// returns ErrUnsupported on platforms that cannot edit messages, and
	// no-ops on a zero ref.
	Update(ctx context.Context, ref MessageRef, r Reply) error

	// Delete removes a previously posted message (e.g. a progress
	// placeholder once the real reply is ready). It returns ErrUnsupported
	// on platforms that cannot delete messages, and no-ops on a zero ref.
	Delete(ctx context.Context, ref MessageRef) error
}
