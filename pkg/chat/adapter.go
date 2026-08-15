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

// Reply is one outbound turn switchboard relays back into a conversation.
type Reply struct {
	// Conversation echoes Message.Conversation so the adapter posts into
	// the originating thread.
	Conversation string

	// Text is the reply body in the platform's markup dialect (the
	// adapter is responsible for any final formatting).
	Text string
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
