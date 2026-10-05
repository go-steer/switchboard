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
	"strings"
	"testing"

	"github.com/go-steer/switchboard/pkg/chat"
)

// With buttons, the card shows the question alone, not the answers listed
// again above their own buttons (reported from the live rig). Without them —
// no action URL, as on Pub/Sub — there is no card, and the text, which keeps
// the prose list, is the whole question.
func TestAButtonedQuestionCardDoesNotListItsAnswers(t *testing.T) {
	text := chat.DecisionReplyText("**Permission needed** — `bash`", goldenDecision)
	card := decisionCard(text, goldenDecision, testAudience)
	if card == nil {
		t.Fatal("no card for a question with an action URL")
	}
	raw, _ := json.Marshal(card)
	if strings.Contains(string(raw), "•") {
		t.Errorf("card still lists the answers above its buttons: %s", raw)
	}
	if !strings.Contains(string(raw), "Permission needed") {
		t.Errorf("card lost the question itself: %s", raw)
	}
	if decisionCard(text, goldenDecision, "") != nil {
		t.Error("a card without buttons was built; the text fallback carries the answers")
	}
	if !strings.Contains(text, "• ") {
		t.Errorf("the text fallback lost its answers: %q", text)
	}
}

// An answer that could not become a button (no value) is named only by the
// list, so with one dropped the card keeps it (caught in review).
func TestAQuestionCardMissingAButtonKeepsItsList(t *testing.T) {
	d := &chat.Decision{ID: "s#p", Options: []chat.DecisionOption{
		{Value: "deny", Label: "Deny"}, {Value: "allow-once", Label: "Allow once"}, {Value: "", Label: "Ask the operator"},
	}}
	card := decisionCard(chat.DecisionReplyText("**Permission needed**", d), d, testAudience)
	raw, _ := json.Marshal(card)
	if !strings.Contains(string(raw), "Ask the operator") {
		t.Errorf("the button-less answer vanished: %s", raw)
	}
}
