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
