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
	"strings"
	"testing"
	"time"

	"github.com/go-steer/switchboard/pkg/approval"
	"github.com/go-steer/switchboard/pkg/chat"
)

// With several agents, an answer names the agent that wrote it, so the
// adapter can show who answered.
func TestAnAnswerNamesItsAgent(t *testing.T) {
	r, _, _, fake := twoAgentRouter(t)
	r.agents.byName["b"].display = "Agent B"
	r.agents.byName["b"].icon = "https://example.com/b.png"
	e := liveEntryIn(r, "C2:1", "C2")
	e.agent = "b"

	r.deliverText(context.Background(), e, "C2:1", "the answer", 1)

	got := recvReply(t, fake.replies)
	if got.Agent == nil || got.Agent.Name != "b" || got.Agent.Label != "Agent B" || got.Agent.IconURL != "https://example.com/b.png" {
		t.Errorf("reply agent = %+v, want agent b's identity", got.Agent)
	}
}

// A permission prompt is the agent asking, so it is signed like an answer.
func TestAPromptNamesItsAgent(t *testing.T) {
	r, _, _, fake := twoAgentRouter(t)
	e := liveEntryIn(r, "C2:1", "C2")
	e.agent = "b"

	r.postPrompt(context.Background(), "C2:1", e, approval.Prompt{ID: "pr1", Kind: "bash", Tool: "bash"})

	got := recvReply(t, fake.replies)
	if got.Kind != chat.KindDecision || got.Agent == nil || got.Agent.Name != "b" {
		t.Errorf("reply = kind %q agent %+v, want a decision signed by b", got.Kind, got.Agent)
	}
}

// The usage footer's edit rewrites the answer whole, so it must sign it too:
// unsigned, it took the agent's card header off (caught in review).
func TestTheFooterEditKeepsTheAnswerSigned(t *testing.T) {
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	dc := answerFirstDaemon(t)
	r, ctx := narrationRouter(t, nil, fitsAllSender{fake}, time.Hour)
	set, err := newAgentSet("x", &agent{name: "x", display: "Agent X", daemon: dc})
	if err != nil {
		t.Fatal(err)
	}
	r.setAgents(set)
	r.setShowUsage(true)
	r.footDelay = 50 * time.Millisecond

	handleOne(t, r, ctx, fake)
	if answer := recvReply(t, fake.replies); answer.Agent == nil {
		t.Fatalf("answer %q unsigned", answer.Text)
	}
	waitFor(t, func() bool {
		for _, u := range fake.updatedCalls() {
			if u.reply.Usage != nil {
				return true
			}
		}
		return false
	}, "the footer edit")
	for _, u := range fake.updatedCalls() {
		if u.reply.Usage != nil && (u.reply.Agent == nil || u.reply.Agent.Label != "Agent X") {
			t.Errorf("footer edit agent = %+v, want Agent X", u.reply.Agent)
		}
	}
}

// A conversation on the default agent, stored as "", is still signed by it.
func TestTheDefaultAgentSignsToo(t *testing.T) {
	r, _, _, _ := twoAgentRouter(t)
	e := liveEntryIn(r, "C1:1", "C1")
	if id := r.identityOf(e); id == nil || id.Name != "a" || id.Label != "a" {
		t.Errorf("identity = %+v, want agent a, labelled by its name", id)
	}
}

// One agent behind the gateway: nobody to tell apart, so nothing changes.
func TestASingleAgentGatewaySignsNothing(t *testing.T) {
	d := newRecordingDaemon(t, "s1")
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	r := NewRouter(d.client, fake, ProgressOff, nil, nil)
	e := liveEntryIn(r, "C1:1", "C1")

	r.deliverText(context.Background(), e, "C1:1", "the answer", 1)

	if got := recvReply(t, fake.replies); got.Agent != nil {
		t.Errorf("reply agent = %+v, want none", got.Agent)
	}
}

// An agent removed since the thread began is not named.
func TestAGoneAgentSignsNothing(t *testing.T) {
	r, _, _, _ := twoAgentRouter(t)
	e := liveEntryIn(r, "C1:1", "C1")
	e.agent = "removed"
	if id := r.identityOf(e); id != nil {
		t.Errorf("identity = %+v, want none", id)
	}
}

func TestIconURLMustBeHTTPS(t *testing.T) {
	t.Setenv("TOK_A", "a")
	for _, icon := range []string{"http://example.com/a.png", "example.com/a.png", "https://"} {
		_, err := buildAgents([]AgentConfig{{Name: "a", DaemonURL: "http://a", TokenEnv: "TOK_A", IconURL: icon}}, "", false)
		if err == nil || !strings.Contains(err.Error(), "icon_url") {
			t.Errorf("icon %q: err = %v, want an icon_url error", icon, err)
		}
	}
	set, err := buildAgents([]AgentConfig{{Name: "a", DaemonURL: "http://a", TokenEnv: "TOK_A", IconURL: " https://example.com/a.png "}}, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := set.byName["a"].icon; got != "https://example.com/a.png" {
		t.Errorf("icon = %q", got)
	}
}
