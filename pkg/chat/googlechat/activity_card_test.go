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
	"strings"
	"testing"

	"github.com/go-steer/switchboard/pkg/chat"
)

// A stream notice carries its commands in code blocks, which an icon line's
// HTML cannot render; it goes out as a MARKDOWN paragraph instead. A notice
// with no block (status mode's terse line) keeps its icon line.
func TestAStreamNoticeWithACodeBlockRendersAsMarkdown(t *testing.T) {
	a := &Adapter{cards: CardsStatus}
	block := a.cardFor(chat.Reply{Kind: chat.KindActivity, Text: "✅ Ran **bash**\n```\nkubectl get pods -A\n```\n⏱ 12s"})
	if block == nil || block.Sections[0].Widgets[0].TextParagraph == nil ||
		block.Sections[0].Widgets[0].TextParagraph.TextSyntax != "MARKDOWN" {
		t.Fatalf("notice with a code block = %+v, want a MARKDOWN paragraph", block)
	}
	if got := block.Sections[0].Widgets[0].TextParagraph.Text; !strings.Contains(got, "```\nkubectl get pods -A\n```\n⏱ 12s") {
		t.Errorf("paragraph = %q, want the block and the duration line intact", got)
	}
	terse := a.cardFor(chat.Reply{Kind: chat.KindActivity, Text: "🔧 Running `bash`"})
	if terse == nil || terse.Sections[0].Widgets[0].DecoratedText == nil {
		t.Errorf("terse notice = %+v, want its icon line", terse)
	}
}
