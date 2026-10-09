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
	"testing"

	"github.com/slack-go/slack"
)

// A free-text argument (an agent prompt) keeps its spacing and line breaks;
// Args, split on whitespace, cannot carry them. A slash command belongs to no
// thread, so it names no conversation.
func TestASlashCommandKeepsItsArgumentText(t *testing.T) {
	cmd := parseSlashCommand(slack.SlashCommand{ChannelID: "C1", Text: "agent  infra list\n  the nodes"})
	if cmd.Name != "agent" || cmd.Text != "infra list\n  the nodes" {
		t.Errorf("cmd = %+v, want name agent and the argument text as typed", cmd)
	}
	if cmd.Conversation != "" {
		t.Errorf("Conversation = %q, want none for a slash command", cmd.Conversation)
	}
	if bare := parseSlashCommand(slack.SlashCommand{ChannelID: "C1", Text: "agent"}); bare.Text != "" {
		t.Errorf("bare command Text = %q, want empty", bare.Text)
	}
}
