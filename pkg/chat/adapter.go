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

import "context"

// Message is one inbound turn from a chat platform, normalized across
// providers.
type Message struct {
	// Conversation is the stable key the router maps onto a core-agent
	// session — a Slack channel+thread_ts, or a Google Chat space+thread.
	// Same Conversation across turns => same session.
	Conversation string

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

// Handler receives normalized inbound messages. The router implements it;
// an Adapter calls it once per inbound turn.
type Handler interface {
	Handle(ctx context.Context, msg Message) error
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

	// Send posts a reply into its originating conversation.
	Send(ctx context.Context, r Reply) error
}
