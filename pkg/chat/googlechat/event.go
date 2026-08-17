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

// This file decodes what Chat delivers into one normalized inbound event.
//
// Chat speaks two dialects, and which one an app receives is a deployment
// choice made in the Cloud console, not a code choice:
//
//   - Google Workspace add-on (the framework this gateway targets): every
//     event is wrapped in a top-level "chat" object carrying exactly one
//     payload — messagePayload, appCommandPayload, buttonClickedPayload,
//     addedToSpacePayload, removedFromSpacePayload.
//   - Chat API interaction events (the older framework, what the Pub/Sub MVP
//     shipped against): a flat event with a "type" discriminator.
//
// Converting an app to an add-on is irreversible and takes effect for every
// user at once, so an operator does it on their own schedule. Rather than make
// that a flag they must remember to flip in lockstep, decode detects the
// dialect per event — the add-on wrapper is unambiguous — and normalizes both
// into the same inbound value. The rest of the package never learns which
// dialect a turn arrived in.
package googlechat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	chatv1 "google.golang.org/api/chat/v1"
)

// Legacy (interaction-events) event types the gateway acts on. The add-on
// dialect has no type discriminator — the payload that is set *is* the type.
const (
	legacyTypeMessage      = "MESSAGE"
	legacyTypeCardClicked  = "CARD_CLICKED"
	legacyTypeAddedToSpace = "ADDED_TO_SPACE"
)

// senderTypeBot marks a message authored by an app (including this one). Chat
// does not normally deliver an app its own messages, but guarding on it keeps a
// misconfiguration from looping the gateway against itself.
const senderTypeBot = "BOT"

// chatTextLimit is Google Chat's per-message text ceiling (4096 characters); a
// longer reply is split across several in-thread posts. Enforced in bytes,
// which is conservative for multi-byte text (fewer runes than bytes).
const chatTextLimit = 4096

// eventKind is what an inbound event turned out to be, once the dialect is
// stripped away.
type eventKind int

const (
	// kindIgnore is anything the gateway does not act on — a lifecycle event
	// it has no answer for, a bot's own message, a payload with nothing
	// runnable in it.
	kindIgnore eventKind = iota
	// kindMessage is a human turn for the agent.
	kindMessage
	// kindCommand is a gateway control command (slash or quick command).
	kindCommand
	// kindButton is a click on a button the gateway put on one of its own
	// cards. It carries a command like kindCommand, but also the message that
	// hosts the card, so the card can be updated in place afterwards.
	kindButton
	// kindWelcome is the app being added to a space.
	kindWelcome
)

// inbound is one decoded event, normalized across both dialects. Only the
// fields relevant to its kind are populated.
type inbound struct {
	kind eventKind

	// space and thread locate the conversation (thread may be empty in a
	// flat space); caller is the sender's users/NNN resource name.
	space, thread, caller string

	// text is the human-readable turn body (kindMessage).
	text string

	// cmdID is the configured Chat command ID (kindCommand); 0 when the
	// dialect did not carry one. cmdArgs is the argument text the user typed
	// after the command word.
	cmdID   int64
	cmdArgs string

	// messageName is the resource name of the message hosting the clicked
	// card (kindButton) — what Update patches to reflect the new state.
	messageName string

	// params are the action parameters carried by a button click. Add-ons
	// that extend Chat never populate commonEventObject.invokedFunction, so
	// parameters is where a card action's identity has to live.
	params map[string]string
}

// wireEvent is the union of the two dialects. Exactly one side is populated
// for any real event: Chat (add-on) or the flat legacy fields.
type wireEvent struct {
	// Add-on dialect.
	Chat   *addonChat                `json:"chat"`
	Common *chatv1.CommonEventObject `json:"commonEventObject"`

	// Legacy interaction-events dialect.
	Type         string                    `json:"type"`
	Message      *chatv1.Message           `json:"message"`
	Space        *chatv1.Space             `json:"space"`
	User         *chatv1.User              `json:"user"`
	LegacyCommon *chatv1.CommonEventObject `json:"common"`
}

// addonChat is the "chat" wrapper of a Google Workspace add-on event object.
// Exactly one payload is set.
type addonChat struct {
	User  *chatv1.User  `json:"user"`
	Space *chatv1.Space `json:"space"`

	MessagePayload          *addonMessagePayload    `json:"messagePayload"`
	AppCommandPayload       *addonAppCommandPayload `json:"appCommandPayload"`
	ButtonClickedPayload    *addonButtonPayload     `json:"buttonClickedPayload"`
	AddedToSpacePayload     *addonAddedPayload      `json:"addedToSpacePayload"`
	RemovedFromSpacePayload json.RawMessage         `json:"removedFromSpacePayload"`
}

type addonMessagePayload struct {
	Message *chatv1.Message `json:"message"`
	Space   *chatv1.Space   `json:"space"`
}

type addonAppCommandPayload struct {
	AppCommandMetadata *addonCommandMetadata `json:"appCommandMetadata"`
	Message            *chatv1.Message       `json:"message"`
	Space              *chatv1.Space         `json:"space"`
	Thread             *chatv1.Thread        `json:"thread"`
}

// addonCommandMetadata identifies the invoked command. AppCommandId is an
// int64 the reference documents as an "int64-format string", and the two
// dialects disagree in practice about whether it is quoted, so it is decoded
// through commandID, which accepts either — a quoting mismatch must not fail
// the whole event.
type addonCommandMetadata struct {
	AppCommandId   commandID `json:"appCommandId"`
	AppCommandType string    `json:"appCommandType"`
}

// commandID is an int64 that unmarshals from a JSON number or a quoted one.
type commandID int64

func (c *commandID) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("googlechat: bad appCommandId %s: %w", b, err)
	}
	*c = commandID(n)
	return nil
}

type addonButtonPayload struct {
	Message *chatv1.Message `json:"message"`
	Space   *chatv1.Space   `json:"space"`
}

type addonAddedPayload struct {
	Space *chatv1.Space `json:"space"`
	// InteractionAdd is true when the app was added by an @mention or a
	// command, in which case Chat sends a *second* event carrying that
	// message — so the welcome must not also answer this one, or the space
	// gets two replies to one action.
	InteractionAdd bool `json:"interactionAdd"`
}

// decodeEvent parses the JSON body of a Pub/Sub message into a normalized
// event. A payload that parses but says nothing actionable decodes to
// kindIgnore rather than an error: only malformed JSON is an error.
func decodeEvent(data []byte) (inbound, error) {
	var ev wireEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return inbound{}, fmt.Errorf("googlechat: decode event: %w", err)
	}
	if ev.Chat != nil {
		return normalizeAddon(&ev), nil
	}
	return normalizeLegacy(&ev), nil
}

// normalizeAddon reduces a Google Workspace add-on event object to an inbound.
func normalizeAddon(ev *wireEvent) inbound {
	c := ev.Chat
	in := inbound{
		space:  spaceNameOf(c.Space),
		caller: userName(c.User),
	}
	switch {
	case c.MessagePayload != nil:
		m := c.MessagePayload.Message
		if in.space == "" {
			in.space = spaceNameOf(c.MessagePayload.Space)
		}
		if isBotMessage(m) {
			return inbound{}
		}
		in.space = orSpaceOf(in.space, m)
		in.thread = threadOf(m)
		in.caller = orSender(in.caller, m)
		in.text = bodyText(m)
		if in.space == "" || in.text == "" {
			return inbound{}
		}
		in.kind = kindMessage
		return in

	case c.AppCommandPayload != nil:
		p := c.AppCommandPayload
		if in.space == "" {
			in.space = spaceNameOf(p.Space)
		}
		in.space = orSpaceOf(in.space, p.Message)
		in.thread = threadNameOf(p.Thread)
		if in.thread == "" {
			in.thread = threadOf(p.Message)
		}
		in.caller = orSender(in.caller, p.Message)
		if p.AppCommandMetadata != nil {
			in.cmdID = int64(p.AppCommandMetadata.AppCommandId)
		}
		// A slash command's arguments ride in the message's argumentText; a
		// quick command has no message at all and is identified by its ID.
		if p.Message != nil {
			in.cmdArgs = strings.TrimSpace(p.Message.ArgumentText)
		}
		if in.space == "" {
			return inbound{}
		}
		in.kind = kindCommand
		return in

	case c.ButtonClickedPayload != nil:
		p := c.ButtonClickedPayload
		if in.space == "" {
			in.space = spaceNameOf(p.Space)
		}
		in.space = orSpaceOf(in.space, p.Message)
		in.thread = threadOf(p.Message)
		if p.Message != nil {
			in.messageName = p.Message.Name
		}
		if ev.Common != nil {
			in.params = ev.Common.Parameters
		}
		if in.space == "" || len(in.params) == 0 {
			return inbound{} // not one of our buttons
		}
		in.kind = kindButton
		return in

	case c.AddedToSpacePayload != nil:
		p := c.AddedToSpacePayload
		if in.space == "" {
			in.space = spaceNameOf(p.Space)
		}
		// The mention or command that added the app arrives as its own event;
		// answering both would double-post into a brand new space.
		if in.space == "" || p.InteractionAdd {
			return inbound{}
		}
		in.kind = kindWelcome
		return in
	}
	return inbound{}
}

// normalizeLegacy reduces a Chat API interaction event to an inbound. Kept so
// an operator can convert their app to an add-on when it suits them rather
// than in lockstep with a switchboard rollout.
func normalizeLegacy(ev *wireEvent) inbound {
	in := inbound{
		space:  spaceNameOf(ev.Space),
		caller: userName(ev.User),
	}
	switch ev.Type {
	case legacyTypeMessage:
		m := ev.Message
		if m == nil || isBotMessage(m) {
			return inbound{}
		}
		in.space = orSpaceOf(in.space, m)
		in.thread = threadOf(m)
		in.caller = orSender(in.caller, m)
		if in.space == "" {
			return inbound{}
		}
		// A native slash command configures the gateway; anything else is an
		// agent turn.
		if m.SlashCommand != nil {
			in.kind = kindCommand
			in.cmdID = m.SlashCommand.CommandId
			in.cmdArgs = strings.TrimSpace(m.ArgumentText)
			return in
		}
		if in.text = bodyText(m); in.text == "" {
			return inbound{}
		}
		in.kind = kindMessage
		return in

	case legacyTypeCardClicked:
		m := ev.Message
		in.space = orSpaceOf(in.space, m)
		in.thread = threadOf(m)
		if m != nil {
			in.messageName = m.Name
		}
		if ev.LegacyCommon != nil {
			in.params = ev.LegacyCommon.Parameters
		}
		if in.space == "" || len(in.params) == 0 {
			return inbound{}
		}
		in.kind = kindButton
		return in

	case legacyTypeAddedToSpace:
		if in.space == "" {
			return inbound{}
		}
		// The legacy dialect folds an @mention-add into this one event and
		// carries the message with it; the message path already answers that,
		// so only a bare add earns a welcome.
		if ev.Message != nil {
			return inbound{}
		}
		in.kind = kindWelcome
		return in
	}
	return inbound{}
}

// isBotMessage reports whether a message was authored by an app.
func isBotMessage(m *chatv1.Message) bool {
	return m != nil && m.Sender != nil && m.Sender.Type == senderTypeBot
}

// bodyText is the human-readable turn body: argumentText (the message with the
// app mention and any command word already stripped by Chat), falling back to
// the raw text. Empty when there is nothing to run — an attachment-only
// message, say.
func bodyText(m *chatv1.Message) string {
	if m == nil {
		return ""
	}
	if t := strings.TrimSpace(m.ArgumentText); t != "" {
		return t
	}
	return strings.TrimSpace(m.Text)
}

// spaceNameOf reads a space's resource name (spaces/AAAA), tolerating a nil.
func spaceNameOf(s *chatv1.Space) string {
	if s == nil {
		return ""
	}
	return s.Name
}

// threadNameOf reads a thread's resource name, tolerating a nil.
func threadNameOf(t *chatv1.Thread) string {
	if t == nil {
		return ""
	}
	return t.Name
}

// userName reads a user's resource name (users/NNN), tolerating a nil.
func userName(u *chatv1.User) string {
	if u == nil {
		return ""
	}
	return u.Name
}

// orSpaceOf falls back to the space a message says it belongs to.
func orSpaceOf(space string, m *chatv1.Message) string {
	if space != "" {
		return space
	}
	if m != nil && m.Space != nil {
		return m.Space.Name
	}
	return ""
}

// orSender falls back to the message's sender for the caller identity.
func orSender(caller string, m *chatv1.Message) string {
	if caller != "" {
		return caller
	}
	if m != nil && m.Sender != nil {
		return m.Sender.Name
	}
	return ""
}

// threadOf reads the thread a message belongs to, or "" in a flat space.
func threadOf(m *chatv1.Message) string {
	if m == nil || m.Thread == nil {
		return ""
	}
	return m.Thread.Name
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

// compactJSON renders a payload as one line for the event log, falling back to
// the raw bytes when it is not JSON — a payload that failed to parse is exactly
// the one worth seeing verbatim.
func compactJSON(data []byte) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, data); err != nil {
		return string(data)
	}
	return buf.String()
}
