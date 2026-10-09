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
	"fmt"
	"strings"
	"unicode"

	"github.com/go-steer/switchboard/pkg/chat"
)

// The `agent` command (#140, phase 3): list the agents a channel offers, or
// start a conversation with one.
//
//	agent                  the agents here, and which is the default
//	agent <name> [prompt]  a new thread with <name>, opened with prompt
//
// One agent per thread, and the thread is always one switchboard starts: it
// posts a starter message naming the agent and the prompt, and the
// conversation lives in its thread. Not the command's own thread on Google
// Chat, though a Chat command is a message with a thread: Chat will not take
// an app's reply into a slash command's thread, and silently starts a new
// top-level thread for each post instead (measured on the GKE deployment,
// #140) — an answer arriving as a loose message, its follow-ups scattering.
// An app's own message has no such limit. Typed inside a thread that already
// has an agent, the command does not switch it: it says which agent the
// thread talks to.
//
// The pickers (a Chat dropdown card, a Slack modal) are a front end to the
// same path, startAgentThread.

// agentCommand runs `agent` and returns the acknowledgment the adapter shows
// the invoker (ephemerally on Slack, in the thread on Chat). Empty when the
// answer itself is the acknowledgment.
func (r *Router) agentCommand(ctx context.Context, cmd chat.Command) string {
	if cmd.Channel == "" {
		return "Agents are chosen in a channel or a direct message."
	}
	if r.agents.implicit {
		return "This gateway has one agent: just mention the app to talk to it."
	}
	cs := r.settingsFor(cmd.Channel)
	if len(cmd.Args) == 0 {
		return r.agentListing(cs)
	}
	name := strings.ToLower(cmd.Args[0])
	a, ok := r.agents.byName[name]
	if !ok || !agentAllowed(r.agents.allowedIn(cs), name) {
		return fmt.Sprintf("There is no agent %q here. %s", name, r.agentListing(cs))
	}
	prompt := afterFirstWord(cmd.Text)

	if cmd.Conversation != "" {
		if cur, has := r.conversationAgent(cmd.Conversation); has {
			curLabel := r.agentLabel(cur)
			if cur == name {
				if prompt != "" {
					// Same agent: the prompt is just this thread's next turn.
					if err := r.Handle(ctx, chat.Message{Conversation: cmd.Conversation, Channel: cmd.Channel, Caller: cmd.Caller, Text: prompt}); err != nil {
						r.logf.Warnf("agent %s: %v", cmd.Conversation, err)
					}
					return ""
				}
				return fmt.Sprintf("This thread already talks to %s.", curLabel)
			}
			return fmt.Sprintf("This thread talks to %s. To talk to %s, start a new thread with `agent %s <prompt>`.",
				curLabel, a.label(), name)
		}
	}
	// Started in the background: the command answers now. A Slack slash
	// command must be acknowledged within three seconds, and the adapter runs
	// it inline on the loop every other Slack event waits behind — while this
	// path posts a starter, opens a session and injects a turn, any one of
	// which can take longer (caught in review). ctx is the adapter's run
	// context, which outlives the command, as the relay it starts must.
	go func() {
		if err := r.startAgentThread(ctx, cmd, a, prompt); err != nil {
			r.logf.Errorf("agent %s: start a thread with %s: %v", cmd.Channel, name, err)
		}
	}()
	if cmd.Conversation != "" {
		// Google Chat: the starter is the acknowledgment, and an ack would be
		// one more loose message (it, too, cannot land in the command's thread).
		return ""
	}
	return fmt.Sprintf("Starting a thread with %s…", a.label())
}

// startAgentThread opens a conversation with agent a in the thread of a
// starter message it posts in the channel and, if there is a prompt, runs it
// as the thread's first turn. Whatever goes wrong before the conversation is
// open is said in the channel, and a starter with nothing behind it is taken
// down.
func (r *Router) startAgentThread(ctx context.Context, cmd chat.Command, a *agent, prompt string) error {
	ref, err := r.out.Send(ctx, chat.Reply{Conversation: cmd.Channel, Text: starterText(a, cmd.CallerMention, prompt), Kind: chat.KindNotice})
	if err != nil {
		r.tellAgentFailure(ctx, cmd.Channel, a)
		return fmt.Errorf("post the starter: %w", err)
	}
	if ref.Conversation == "" || ref.Conversation == cmd.Channel {
		r.tellAgentFailure(ctx, cmd.Channel, a)
		return fmt.Errorf("the starter landed in no thread (%q)", ref.Conversation)
	}
	conv, starter := ref.Conversation, ref
	e, err := r.sessionAs(ctx, conv, cmd.Channel, cmd.Caller, a.name)
	if err != nil {
		// Nothing behind it: take the starter down rather than leave a
		// conversation in the channel that cannot be answered.
		if derr := r.out.Delete(ctx, starter); derr != nil {
			r.logf.Warnf("agent %s: delete the starter: %v", conv, derr)
		}
		r.tellAgentFailure(ctx, cmd.Channel, a)
		return err
	}
	// sessionAs returns an existing conversation as it is: a message that got
	// here first may have opened it on another agent (caught in review).
	if cur := r.agents.resolve(e.agent); cur != a.name {
		notice := fmt.Sprintf("This thread talks to %s. To talk to %s, start a new thread with `agent %s <prompt>`.",
			r.agentLabel(cur), a.label(), a.name)
		if err := r.surfaceNotice(ctx, conv, notice); err != nil {
			r.logf.Warnf("agent %s: %v", conv, err)
		}
		return nil
	}
	if prompt != "" {
		if err := r.Handle(ctx, chat.Message{Conversation: conv, Channel: cmd.Channel, Caller: cmd.Caller, Text: prompt}); err != nil {
			// The thread exists and is the agent's; the turn's own failure is
			// already in it (Handle surfaces its errors).
			r.logf.Warnf("agent %s: first turn: %v", conv, err)
		}
	}
	return nil
}

// starterText is the message a conversation with an agent opens with: the
// agent, who asked (where the platform can name them), and the prompt.
func starterText(a *agent, mention, prompt string) string {
	text := "**" + a.label() + "**"
	if mention != "" {
		text += " · asked by " + mention
	}
	if prompt != "" {
		text += "\n" + prompt
	} else {
		text += "\nReply in this thread to talk to it."
	}
	return text
}

// tellAgentFailure says, where it can, that a conversation could not start.
func (r *Router) tellAgentFailure(ctx context.Context, conv string, a *agent) {
	notice := fmt.Sprintf("Could not start a conversation with %s. Check the logs or contact an admin.", a.label())
	if err := r.surfaceNotice(ctx, conv, notice); err != nil {
		r.logf.Warnf("agent %s: %v", conv, err)
	}
}

// conversationAgent reports the agent a conversation already belongs to — a
// live entry, a dormant record, or an ingress binding — resolved to a name.
func (r *Router) conversationAgent(conv string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.sessions[conv]; ok {
		return r.agents.resolve(e.agent), true
	}
	if rec, ok := r.dormant[conv]; ok {
		return r.agents.resolve(rec.Agent), true
	}
	if b, ok := r.bindings[conv]; ok {
		return r.agents.resolve(b.agent), true
	}
	return "", false
}

// agentLabel names an agent to a person, falling back to its name.
func (r *Router) agentLabel(name string) string {
	if a, ok := r.agents.byName[name]; ok {
		return a.label()
	}
	return name
}

// agentListing is the reply to a bare `agent`: who is here, and how to start.
func (r *Router) agentListing(cs channelSettings) string {
	def, _ := r.agents.channelAgent(cs)
	def = r.agents.resolve(def)
	var lines []string
	for _, a := range r.agents.allowedIn(cs) {
		item := "• **" + a.label() + "** (`" + a.name + "`)"
		if a.name == def {
			item += ", the default"
		}
		lines = append(lines, item)
	}
	if len(lines) == 0 {
		return "No agents are available here."
	}
	return "**Agents here**\n" + strings.Join(lines, "\n") +
		"\nStart a thread with one: `agent <name> <prompt>`. A plain message goes to the default."
}

func agentAllowed(list []*agent, name string) bool {
	for _, a := range list {
		if a.name == name {
			return true
		}
	}
	return false
}

// afterFirstWord is s without its first word, spacing kept: the prompt after
// an agent's name.
func afterFirstWord(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexFunc(s, unicode.IsSpace); i >= 0 {
		return strings.TrimSpace(s[i:])
	}
	return ""
}
