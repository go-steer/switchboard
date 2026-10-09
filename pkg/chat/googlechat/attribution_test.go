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
	"context"
	"strings"
	"testing"

	"github.com/go-steer/switchboard/pkg/chat"
)

var agentB = &chat.AgentIdentity{Name: "b", Label: "Agent B", IconURL: "https://example.com/b.png"}

// With cards, an agent's answer carries the agent in its header — a plain
// answer included, which earns a card for it as it does for a usage footer.
func TestAnAgentsAnswerCardIsHeadedWithTheAgent(t *testing.T) {
	a := &Adapter{cards: CardsRich}
	for name, text := range map[string]string{
		"plain":      "Just a sentence.",
		"structured": "## Pods\n\nAll running.\n\n---\n\nDone.",
	} {
		card := a.cardFor(chat.Reply{Text: text, Agent: agentB})
		if card == nil || card.Header == nil {
			t.Fatalf("%s: card = %+v, want one with a header", name, card)
		}
		if card.Header.Title != "Agent B" || card.Header.ImageUrl != agentB.IconURL {
			t.Errorf("%s: header = %+v", name, card.Header)
		}
	}
	if card := a.cardFor(chat.Reply{Text: "Just a sentence."}); card != nil {
		t.Errorf("an unsigned plain answer got a card: %+v", card)
	}
}

// A decision card is headed too: the agent is the one asking.
func TestAnAgentsQuestionIsHeadedWithTheAgent(t *testing.T) {
	a := &Adapter{cards: CardsRich, ingress: IngressHTTP}
	a.verify.audience = testAudience
	card := a.cardFor(chat.Reply{Text: goldenDecisionText, Kind: chat.KindDecision, Decision: goldenDecision, Agent: agentB})
	if card == nil || card.Header == nil || card.Header.Title != "Agent B" {
		t.Fatalf("decision card = %+v, want a header naming the agent", card)
	}
}

// The name never changes how a reply is split. An answer that fits one
// message only without it goes unnamed: split after the name, the name alone
// was the first message, and the footer's edit then rewrote that message with
// the whole answer — in the thread twice (caught in review).
func TestTheNameNeverSplitsAReply(t *testing.T) {
	f := &fakeMessenger{}
	a := newTestAdapter(f) // cards off
	near := strings.Repeat("a", chatTextLimit-3)
	ref, err := a.Send(context.Background(), chat.Reply{Conversation: "spaces/A:spaces/A/threads/T", Text: near, Agent: agentB})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.creates) != 1 || f.creates[0].text != near {
		t.Fatalf("posted %d parts, first %d bytes; want the answer alone in one", len(f.creates), len(f.creates[0].text))
	}
	if !a.FitsOneMessage(near) {
		t.Fatal("FitsOneMessage disagrees with what Send posted")
	}
	if err := a.Update(context.Background(), ref, chat.Reply{Conversation: ref.Conversation, Text: near, Agent: agentB, Usage: &chat.Usage{TokensOut: 1}}); err != nil {
		t.Fatal(err)
	}
	if n := len(f.patches); n != 1 {
		t.Errorf("patches = %d, want the one message edited", n)
	}

	long := strings.Repeat("word ", chatTextLimit/5+50)
	if got := replyText(chat.Reply{Text: long, Agent: agentB}); strings.HasPrefix(got, "*Agent B*") {
		t.Error("a reply that is split anyway was named")
	}
}

func TestTheNamesMarkupIsStripped(t *testing.T) {
	got := replyText(chat.Reply{Text: "hi", Agent: &chat.AgentIdentity{Label: "*Big* _agent_ `x`"}})
	if got != "*Big agent x*\nhi" {
		t.Errorf("replyText = %q", got)
	}
}

// Without a card, the agent's name leads the text.
func TestAnAgentsTextAnswerLeadsWithItsName(t *testing.T) {
	f := &fakeMessenger{}
	a := newTestAdapter(f) // cards off
	if _, err := a.Send(context.Background(), chat.Reply{Conversation: "spaces/A:spaces/A/threads/T", Text: "Hello **there**", Agent: agentB}); err != nil {
		t.Fatal(err)
	}
	if len(f.creates) != 1 || !strings.HasPrefix(f.creates[0].text, "*Agent B*\nHello *there*") {
		t.Errorf("creates = %+v, want the name leading", f.creates)
	}
	f.creates = nil
	if _, err := a.Send(context.Background(), chat.Reply{Conversation: "spaces/A:spaces/A/threads/T", Text: "Hello"}); err != nil {
		t.Fatal(err)
	}
	if f.creates[0].text != "Hello" {
		t.Errorf("an unsigned reply = %q, want it untouched", f.creates[0].text)
	}
}
