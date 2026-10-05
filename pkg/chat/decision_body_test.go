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

import "testing"

func TestDecisionBodyUndoesDecisionReplyText(t *testing.T) {
	d := &Decision{ID: "q", Options: []DecisionOption{{Value: "deny", Label: "Deny"}, {Value: "allow-once", Label: "Allow once"}}}
	body := "**Permission needed** — `bash`"
	text := DecisionReplyText(body, d)
	if want := body + "\n\n• Deny\n• Allow once"; text != want {
		t.Fatalf("DecisionReplyText = %q, want %q", text, want)
	}
	if got := DecisionBody(text, d); got != body {
		t.Errorf("DecisionBody = %q, want the body alone %q", got, body)
	}
	if got := DecisionBody("unrelated text", d); got != "unrelated text" {
		t.Errorf("DecisionBody changed text it did not build: %q", got)
	}
	if got := DecisionReplyText(body, &Decision{ID: "q"}); got != body {
		t.Errorf("an option-less question gained a trailer: %q", got)
	}
}
