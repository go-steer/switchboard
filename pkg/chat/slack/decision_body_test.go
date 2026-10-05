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

package slack

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/go-steer/switchboard/pkg/chat"
)

// With buttons, the question's section shows the body alone: the answers
// listed again above their own buttons were noise (reported from the live
// rig). The prose list stays in the text for every path without buttons.
func TestAButtonedQuestionDoesNotListItsAnswers(t *testing.T) {
	d := twoOptions()
	text := chat.DecisionReplyText("**Permission needed** — `bash`", d)
	blocks := decisionBlocks(text, d, toMrkdwn)
	if blocks == nil {
		t.Fatal("no blocks for a two-answer question")
	}
	raw, _ := json.Marshal(blocks[0])
	if strings.Contains(string(raw), "•") {
		t.Errorf("section above the buttons still lists the answers: %s", raw)
	}
	if !strings.Contains(string(raw), "Permission needed") {
		t.Errorf("section lost the question itself: %s", raw)
	}
}

// An answer that could not become a button (no value) is named only by the
// list, so with one dropped the list stays (caught in review).
func TestAQuestionMissingAButtonKeepsItsList(t *testing.T) {
	d := &chat.Decision{ID: "s#p", Options: []chat.DecisionOption{
		{Value: "deny", Label: "Deny"}, {Value: "allow-once", Label: "Allow once"}, {Value: "", Label: "Ask the operator"},
	}}
	blocks := decisionBlocks(chat.DecisionReplyText("**Permission needed**", d), d, toMrkdwn)
	raw, _ := json.Marshal(blocks[0])
	if !strings.Contains(string(raw), "Ask the operator") {
		t.Errorf("the button-less answer vanished: %s", raw)
	}
}
