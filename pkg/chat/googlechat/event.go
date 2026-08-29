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

	chatv1 "google.golang.org/api/chat/v1"

	"github.com/go-steer/switchboard/pkg/chat"
)

// Legacy (interaction-events) event types the gateway acts on. The add-on
// dialect has no type discriminator — the payload that is set *is* the type.
const (
	legacyTypeMessage      = "MESSAGE"
	legacyTypeCardClicked  = "CARD_CLICKED"
	legacyTypeAddedToSpace = "ADDED_TO_SPACE"
	// APP_COMMAND is how the legacy dialect delivers a quick command, which
	// has no message to ride along with. Slash commands arrive both ways
	// depending on the app's configuration, so this path handles them too.
	legacyTypeAppCommand = "APP_COMMAND"
)

// Action parameter keys on a card button. A click reaches HandleCommand as
// though the invoker had typed the command, so the parameters are exactly a
// command's verb and its single argument — and they are where the identity has
// to live, because an add-on that extends Chat never populates
// commonEventObject.invokedFunction. The legacy dialect carries the same keys
// in common.parameters, so one encoding serves both.
//
// Decode-only today: no card this gateway sends has a button, since Chat
// delivers no click over Pub/Sub (#28). The writer comes back with #29.
const (
	paramCommand = "switchboard_command"
	paramArg     = "switchboard_arg"
)

// senderTypeBot marks a message authored by an app (including this one). Chat
// does not normally deliver an app its own messages, but guarding on it keeps a
// misconfiguration from looping the gateway against itself.
const senderTypeBot = "BOT"

// annotationUserMention is the Annotation.Type Chat sets on the span of a
// message that @mentions a user or app.
const annotationUserMention = "USER_MENTION"

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
	// kindButton is a click on a button on one of the gateway's own cards. It
	// carries a command like kindCommand, but also the message that hosts the
	// card, so the card can be updated in place afterwards.
	//
	// Unreached today, in both directions: no card this gateway sends has a
	// button, because a click never arrived over Pub/Sub (#28). Decoding is
	// kept for the HTTP interaction endpoint (#29), which is the ingress that
	// delivers them.
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

	// email is the sender's address when the event carried one — the
	// add-on dialect does, on chat.user. Decoding keeps both forms and
	// leaves the choice between them to CallerMode, so which identity is
	// asserted stays a configuration question rather than a decoding one.
	email string

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
	Type               string                    `json:"type"`
	Message            *chatv1.Message           `json:"message"`
	Space              *chatv1.Space             `json:"space"`
	User               *wireUser                 `json:"user"`
	LegacyCommon       *chatv1.CommonEventObject `json:"common"`
	AppCommandMetadata *addonCommandMetadata     `json:"appCommandMetadata"`
}

// wireUser is the actor on an event. It exists because the generated
// chat/v1 User type has no Email field at all, while real add-on payloads
// carry one on chat.user — and an email is what the daemon's per-caller
// credential lookup is keyed by, so decoding through the generated type
// silently throws away the useful half of the identity. Confirmed against
// live traffic on 2026-08-17; see docs/googlechat-setup.md §C.
type wireUser struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Type  string `json:"type"`
}

// addonChat is the "chat" wrapper of a Google Workspace add-on event object.
// Exactly one payload is set.
type addonChat struct {
	User  *wireUser     `json:"user"`
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

// addonCommandMetadata identifies the invoked command. The reference types
// AppCommandMetadata.appCommandId as a bare int32 while the older
// SlashCommand.commandId is an int64-format *string*, so the two carry the
// same console-assigned number in different shapes. It is decoded through
// commandID, which accepts either: a quoting mismatch must not fail the whole
// event.
type addonCommandMetadata struct {
	AppCommandId   commandID `json:"appCommandId"`
	AppCommandType string    `json:"appCommandType"`
}

// commandID is an int64 that unmarshals from a JSON number or a quoted one.
type commandID int64

// UnmarshalJSON never fails. An ID this code cannot read leaves the zero
// value, which maps to no configured command and gets ignored — losing one
// command is a better outcome than dropping the whole event, and it is the
// same tolerance the quoted/unquoted split calls for.
func (c *commandID) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(strings.Trim(strings.TrimSpace(string(b)), `"`))
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		*c = commandID(n)
	}
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
	// The add-on dialect names the actor in chat.user, and a message payload
	// may omit the message's own sender entirely — so this, not the message
	// sender, is the guard that keeps the gateway from answering itself.
	if c.User != nil && c.User.Type == senderTypeBot {
		return inbound{}
	}
	in := inbound{
		space:  spaceNameOf(c.Space),
		caller: userName(c.User),
		email:  userEmail(c.User),
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
		if in.space == "" {
			return inbound{}
		}
		if in.text == "" {
			return welcomeOrIgnore(in, m)
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
		// answering both would double-post into a brand new space. That second
		// event does now always answer: a bare @mention decodes to the welcome
		// rather than to nothing (#55). Deferring to an event that then dropped
		// the add on the floor is what made this suppression a silence.
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
		email:  userEmail(ev.User),
	}
	switch ev.Type {
	case legacyTypeMessage:
		return legacyTurn(in, ev.Message)

	case legacyTypeAppCommand:
		// A quick command carries no message, so the space and the command ID
		// come off the event itself. A slash command routed here still has one.
		in.space = orSpaceOf(in.space, ev.Message)
		in.thread = threadOf(ev.Message)
		in.caller = orSender(in.caller, ev.Message)
		if ev.AppCommandMetadata != nil {
			in.cmdID = int64(ev.AppCommandMetadata.AppCommandId)
		} else if ev.Message != nil && ev.Message.SlashCommand != nil {
			in.cmdID = ev.Message.SlashCommand.CommandId
		}
		if ev.Message != nil {
			in.cmdArgs = strings.TrimSpace(ev.Message.ArgumentText)
		}
		if in.space == "" {
			return inbound{}
		}
		in.kind = kindCommand
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
		// The legacy dialect folds an @mention-add or command-add into this one
		// event and inlines the triggering message, because — unlike the add-on
		// dialect's interactionAdd — no second event follows. Answer the
		// message if there is one; a bare add earns the welcome instead.
		if turn := legacyTurn(in, ev.Message); turn.kind != kindIgnore {
			return turn
		}
		in.kind = kindWelcome
		return in
	}
	return inbound{}
}

// legacyTurn reads a legacy message into either a gateway command or an agent
// turn. Shared by MESSAGE and by the mention-add that inlines its message.
func legacyTurn(in inbound, m *chatv1.Message) inbound {
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
		return welcomeOrIgnore(in, m)
	}
	in.kind = kindMessage
	return in
}

// welcomeOrIgnore resolves a message the agent has no turn to run from. A bare
// @mention of the app is not nothing — it is someone addressing the app without
// asking it anything, which is exactly what the welcome answers (#55) — while a
// message carrying no body at all, an attachment on its own, really is nothing
// to reply to.
//
// This is what makes an @mention-add reach a new space. The add-on dialect
// splits that one user action across two events and the added-to-space half
// suppresses itself in favour of the message half, so the message half has to
// have an answer or the whole add is silent. The legacy dialect inlines the
// triggering message into the add instead, and arrives at the same welcome by
// the same route.
//
// Answering a bare mention anywhere, rather than only in a space the app was
// just added to, is deliberate: the gateway then needs to remember nothing, and
// "@Switchboard" on its own is a reasonable way to ask what the app is called
// and what it accepts long after the add.
func welcomeOrIgnore(in inbound, m *chatv1.Message) inbound {
	if !isBareMention(m) {
		return inbound{}
	}
	in.kind = kindWelcome
	return in
}

// isBareMention reports whether the whole message was an @mention of the app.
// Chat strips the mention out of argumentText, so a message that is nothing but
// the mention leaves it empty while the annotation stays behind.
//
// The annotation check does not say *who* was mentioned, and does not need to:
// Chat strips only the app's own mentions from argumentText, so "@Alice" leaves
// it non-empty and never reaches here. An empty argumentText next to a mention
// means the app's mention was the whole message.
func isBareMention(m *chatv1.Message) bool {
	return m != nil && strings.TrimSpace(m.ArgumentText) == "" && hasMention(m)
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
	// An empty argumentText on a message that mentions the app means the whole
	// message *was* the mention. Falling back to text there would prompt the
	// agent with the literal "@switchboard", so there is no turn to run here —
	// which is not the same as there being nothing to say, see welcomeOrIgnore.
	// A DM reaches neither branch: addon-live-message.json shows Chat setting
	// argumentText there too, equal to the text, since there is no app mention
	// in it to strip.
	if hasMention(m) {
		return ""
	}
	return strings.TrimSpace(m.Text)
}

// hasMention reports whether Chat annotated the message with an @mention.
func hasMention(m *chatv1.Message) bool {
	for _, a := range m.Annotations {
		if a != nil && a.Type == annotationUserMention {
			return true
		}
	}
	return false
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
func userName(u *wireUser) string {
	if u == nil {
		return ""
	}
	return u.Name
}

// userEmail reads a user's email address, tolerating a nil. Empty when the
// dialect did not carry one, which is the signal to fall back to the
// resource name rather than assert nothing.
func userEmail(u *wireUser) string {
	if u == nil {
		return ""
	}
	return u.Email
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
//
// A bare space with no separator is accepted as a top-level conversation: the
// ingress API documents a conversation as a full key *or* a bare channel, and
// requiring a trailing colon there would make Google Chat the odd platform out.
func splitConversation(key string) (space, thread string, ok bool) {
	space, thread, _ = strings.Cut(key, ":")
	if space == "" {
		return "", "", false
	}
	return space, thread, true
}

// landedKey names the conversation a just-created message actually went into,
// for the ref post hands back. conv is the key it was asked to post to, and
// thread is what splitConversation read from it.
//
// A post into a bare space names no thread, so Chat assigns one — and the ref
// has to say which, because a ref is an address to come back to. The outbound
// ingress builds the continuation of an overflowing message from
// ref.Conversation, and a key with no thread in it sends the ingress down the
// "append the id" path meant for Slack, where a top-level message's id *is*
// its thread. Here the id is a message resource name, and the result was the
// malformed key spaces/AAA:spaces/AAA/messages/CCC (#39).
//
// When the caller already named a thread the key is returned unchanged: it is
// the more specific of the two and Chat cannot have moved the message. Chat
// reporting no thread at all is not expected on a created message, and the
// original key is the honest answer to it rather than one invented here.
func landedKey(conv, space, thread string, created *chatv1.Message) string {
	if thread != "" {
		return conv
	}
	if t := threadOf(created); t != "" {
		return conversationKey(space, t)
	}
	return conv
}

// chunk splits s into pieces no longer than limit bytes for posting as several
// ordered in-thread messages. Shared with the other adapters (pkg/chat), which
// is what closes #31: Chat text uses the same ``` syntax as markdown and
// renders an odd one literally, so a split landing inside a fenced block used
// to put raw backticks on screen in both halves of a long answer.
func chunk(s string, limit int) []string {
	return chat.ChunkText(s, limit)
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
