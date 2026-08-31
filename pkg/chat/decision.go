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

package chat

import "strings"

// Decision is a question switchboard is asking the thread, together with the
// answers it will accept — the agent has stopped on something only a person
// can settle, and this is that something rendered as a choice.
//
// It rides on a Reply rather than being its own outbound type because it *is*
// a message: it goes to a conversation, it has a body, and on a platform whose
// interactive controls do not reach switchboard it has to degrade to that body
// and nothing else. An adapter that ignores this field entirely still posts a
// readable question — Reply.Text names the options in prose — it just cannot
// collect the answer.
//
// Nothing here knows what the question is about. The router builds a Decision
// out of whatever the daemon is asking and reads the answer back the same way;
// this package carries opaque strings so that adding a second kind of question
// later is not a change to every adapter.
type Decision struct {
	// ID correlates the press with the question. Opaque, and echoed back
	// verbatim on the Press so the router can find what it was asking about.
	ID string

	// Options are the answers, in the order they should be offered. An empty
	// or one-option Decision is not a question; adapters should treat it as a
	// plain message, since a lone button is a thing to be clicked rather than
	// a thing to be decided.
	Options []DecisionOption
}

// DecisionOption is one answer: what to send back, and what to write on it.
type DecisionOption struct {
	// Value is returned verbatim as Press.Option. Opaque to every adapter.
	Value string

	// Label is the button text, already sized for a button and already
	// specific — "Allow every git this session", not "allow-session-verb".
	// The person deciding is reading this and nothing else.
	Label string

	// Broad marks an answer whose effect outlives the question being asked:
	// the rest of a session, or longer. It is a request for friction, not a
	// styling hint — an adapter with a confirmation affordance should spend it
	// here, because these are the presses that are regretted, and the label
	// alone has one line in which to say why.
	Broad bool
}

// Deciding reports whether this Decision is one worth rendering as a choice.
// Guarding on it rather than on nil-ness alone keeps an adapter from building
// an interactive surface out of a question with nothing to answer.
func (d *Decision) Deciding() bool { return d != nil && len(d.Options) > 1 }

// Press is one inbound answer: someone chose an option on a Decision
// switchboard posted. It is not a Message — it never becomes an agent turn,
// and it carries no text — but it arrives on the same inbound path and is
// attributed the same way, because who pressed the button is the entire point
// of asking in a thread rather than answering on the agent's behalf.
type Press struct {
	// Conversation is the thread the question was asked in, in the same form
	// Message.Conversation takes, so the router can find the session it
	// belongs to without a lookup table of its own.
	Conversation string

	// Channel is the platform channel the conversation lives in.
	Channel string

	// Caller is the presser's asserted-caller identity — the same form
	// Message.Caller takes, resolved the same way. This is what gets recorded
	// as the approver, so an adapter must resolve it from the press itself and
	// never from whoever the surrounding message was addressed to.
	Caller string

	// DecisionID and Option echo the Decision's ID and the chosen
	// DecisionOption's Value.
	DecisionID string
	Option     string

	// Message refs the posted question, so it can be edited to record what was
	// decided. Zero when the platform did not say which message the press came
	// from, which costs the edit and nothing else.
	Message MessageRef
}

// DecisionText renders a Decision's options as prose, for the Reply body that
// carries it. Every platform gets this; only some also get buttons.
//
// It exists so the fallback is written once, here, instead of once per adapter
// with a different idea of how to say the same thing — and so a Decision
// posted to a platform switchboard cannot collect an answer on is still a
// question a person can read and act on by other means.
func DecisionText(d *Decision) string {
	if d == nil {
		return ""
	}
	// No separate empty check: an option-less Decision writes nothing and
	// returns "" by the same path, and a guard whose removal changes no output
	// is a line that only looks like it is doing something.
	var b strings.Builder
	for i, o := range d.Options {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("• ")
		b.WriteString(o.Label)
	}
	return b.String()
}
