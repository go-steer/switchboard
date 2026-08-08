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
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	chatv1 "google.golang.org/api/chat/v1"

	"github.com/go-steer/switchboard/pkg/chat"
)

// eventTypeMessage is the Google Chat event type for a user message directed at
// the app (an @mention in a space, or any message in a DM). It is the only
// event type the MVP turns into an agent turn; membership/space lifecycle
// events are acked and ignored.
const eventTypeMessage = "MESSAGE"

// senderTypeBot marks a message authored by an app (including this one). Chat
// does not normally deliver an app its own messages, but guarding on it keeps a
// misconfiguration from looping the gateway against itself.
const senderTypeBot = "BOT"

// chatTextLimit is Google Chat's per-message text ceiling (4096 characters); a
// longer reply is split across several in-thread posts. Enforced in bytes,
// which is conservative for multi-byte text (fewer runes than bytes).
const chatTextLimit = 4096

// messageFromEvent parses a Google Chat event (the JSON body of a Pub/Sub
// message) into a normalized chat.Message. ok is false when the event is not a
// user message worth running — a non-MESSAGE lifecycle event, a bot-authored
// message, or one with no text — in which case the caller acks and moves on.
// err is non-nil only on a malformed payload, which the caller logs (and still
// acks, so a poison message is not redelivered forever).
func messageFromEvent(data []byte) (chat.Message, bool, error) {
	var ev chatv1.DeprecatedEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return chat.Message{}, false, fmt.Errorf("googlechat: decode event: %w", err)
	}
	if ev.Type != eventTypeMessage || ev.Message == nil {
		return chat.Message{}, false, nil
	}
	m := ev.Message
	if m.Sender != nil && m.Sender.Type == senderTypeBot {
		return chat.Message{}, false, nil
	}
	space := spaceName(&ev)
	if space == "" {
		return chat.Message{}, false, fmt.Errorf("googlechat: MESSAGE event has no space")
	}
	// ArgumentText is the body with the app mention (and any slash command)
	// stripped — the human-readable turn; fall back to raw Text if absent.
	text := strings.TrimSpace(m.ArgumentText)
	if text == "" {
		text = strings.TrimSpace(m.Text)
	}
	if text == "" {
		return chat.Message{}, false, nil // nothing to run (e.g. attachment-only)
	}
	thread := ""
	if m.Thread != nil {
		thread = m.Thread.Name
	}
	return chat.Message{
		Conversation: conversationKey(space, thread),
		Channel:      space,
		Caller:       callerID(&ev),
		Text:         text,
	}, true, nil
}

// spaceName resolves the space resource name (spaces/AAAA) an event belongs to,
// preferring the top-level Space and falling back to the message's own Space.
func spaceName(ev *chatv1.DeprecatedEvent) string {
	if ev.Space != nil && ev.Space.Name != "" {
		return ev.Space.Name
	}
	if ev.Message != nil && ev.Message.Space != nil {
		return ev.Message.Space.Name
	}
	return ""
}

// callerID resolves the human sender's identity as the daemon's asserted-caller
// value — the Google Chat user resource name (users/NNN). core-agent keys
// per-caller MCP credentials off this; verified identity (email) is established
// later by core-agent's own OAuth, so the gateway asserts only the stable
// resource name. Prefers the event User, falling back to the message Sender.
func callerID(ev *chatv1.DeprecatedEvent) string {
	if ev.User != nil && ev.User.Name != "" {
		return ev.User.Name
	}
	if ev.Message != nil && ev.Message.Sender != nil {
		return ev.Message.Sender.Name
	}
	return ""
}

// conversationKey encodes space + thread into the stable chat.Message
// conversation key (same key => same core-agent session). Chat resource names
// contain slashes but no colon, so a single colon separator round-trips via
// splitConversation. The thread may be empty (an unthreaded space), which
// splitConversation preserves so egress posts a top-level message.
func conversationKey(space, thread string) string {
	return space + ":" + thread
}

// splitConversation is the inverse of conversationKey. thread may be empty; ok
// is false only when the space is missing (a malformed key).
func splitConversation(key string) (space, thread string, ok bool) {
	space, thread, found := strings.Cut(key, ":")
	if !found || space == "" {
		return "", "", false
	}
	return space, thread, true
}

// chunk splits s into pieces no longer than limit bytes, preferring to break on
// a newline so a reply's block structure survives the split, and never cutting
// a multi-byte rune. A short string is returned whole.
func chunk(s string, limit int) []string {
	if len(s) <= limit {
		return []string{s}
	}
	var out []string
	for len(s) > limit {
		cut := limit
		// Back up to a rune boundary so a multi-byte rune is never split.
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		if cut == 0 {
			// limit is smaller than the first rune; emit that whole rune so the
			// loop always makes progress (unreachable at Chat's 4096 limit, but
			// keeps chunk total for any caller).
			_, size := utf8.DecodeRuneInString(s)
			cut = size
		} else if nl := strings.LastIndexByte(s[:cut], '\n'); nl > 0 {
			// Prefer the last newline within the window for a cleaner break.
			cut = nl + 1
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	if len(s) > 0 {
		out = append(out, s)
	}
	return out
}
