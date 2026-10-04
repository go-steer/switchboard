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
)

func TestToChatText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain", "just words", "just words"},
		{"bold", "a **b** c", "a *b* c"},
		{"italic", "a *b* c", "a _b_ c"},
		{"underscore italic left alone", "a _b_ c", "a _b_ c"},
		{"bold italic", "a ***b*** c", "a *_b_* c"},
		{"strikethrough", "a ~~b~~ c", "a ~b~ c"},
		{"link", "see [docs](https://example.com)", "see <https://example.com|docs>"},
		{"image left literal", "![alt](https://example.com/i.png)", "![alt](https://example.com/i.png)"},
		{"link with empty url keeps the label", "[docs]()", "docs"},
		{"header becomes bold", "## Results\nbody", "*Results*\nbody"},
		{"header drops inner bold", "### **Loud**", "*Loud*"},
		{"inline code untouched", "run `a **b** c` now", "run `a **b** c` now"},
		{"horizontal rule", "a\n---\nb", "a\n" + hrText + "\nb"},
		{
			name: "fence keeps body and drops the language tag",
			in:   "before\n```go\nx := **not bold**\n```\nafter",
			want: "before\n```\nx := **not bold**\n```\nafter",
		},
		{
			name: "mention is defused",
			in:   "hey <users/all> and <users/123>",
			want: "hey users/all and users/123",
		},
		{
			name: "existing chat link survives the emphasis passes",
			in:   "<https://example.com/a_b_c|link>",
			want: "<https://example.com/a_b_c|link>",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := toChatText(tt.in); got != tt.want {
				t.Fatalf("toChatText(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestToCardHTML(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"bold", "a *b* c", "a <b>b</b> c"},
		{"italic", "a _b_ c", "a <i>b</i> c"},
		{"strike", "a ~b~ c", "a <s>b</s> c"},
		{"newlines become breaks", "one\ntwo", "one<br>two"},
		{
			// The reason this pass exists: a gateway ack quotes a command whose
			// argument list is full of angle brackets.
			name: "angle brackets in code are escaped, not rendered as markup",
			in:   "Set it with `progress <off|stream>`.",
			want: "Set it with <tt>progress &lt;off|stream&gt;</tt>.",
		},
		{"bare angle brackets escaped", "a < b & c > d", "a &lt; b &amp; c &gt; d"},
		{
			name: "link becomes an anchor",
			in:   "see <https://example.com/a?x=1&y=2|the docs>",
			want: `see <a href="https://example.com/a?x=1&amp;y=2">the docs</a>`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := toCardHTML(tt.in); got != tt.want {
				t.Fatalf("toCardHTML(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestToCardHTMLRoundTripsRouterText checks the two passes compose: the router
// writes markdown-ish text, toChatText normalizes it, and toCardHTML renders it
// without ever emitting markup the card did not intend.
func TestToCardHTMLRoundTripsRouterText(t *testing.T) {
	const routerAck = "Progress mode for this channel is *indicator*. " +
		"Change it with `progress <off|indicator|status|stream>`."
	got := toCardHTML(toChatText(routerAck))
	// Single-* is markdown italic, the same reading the Slack adapter takes.
	if !strings.Contains(got, "<i>indicator</i>") {
		t.Fatalf("emphasis lost: %q", got)
	}
	if strings.Contains(got, "<off|") {
		t.Fatalf("unescaped angle bracket would break the card: %q", got)
	}
	if !strings.Contains(got, "&lt;off|indicator|status|stream&gt;") {
		t.Fatalf("argument list not escaped: %q", got)
	}
}

// Stripping a mention's brackets rather than escaping them means one pass is
// not enough: the outer brackets of a nested mention re-pair around the
// survivor. A model turn must not be able to page a whole space.
func TestToChatTextDefusesNestedMentions(t *testing.T) {
	for _, in := range []string{
		"<users/all>",
		"<<users/all>>",
		"<<<users/all>>>",
		"<users/<users/all>>",
		"ping <users/123456> now",
	} {
		got := toChatText(in)
		if mentionRE.MatchString(got) {
			t.Fatalf("toChatText(%q) = %q, which still contains a live mention", in, got)
		}
	}
}

// A double quote in a URL would otherwise close the href attribute and let
// everything after it be parsed as further attributes.
func TestToCardHTMLEscapesQuotesInLinks(t *testing.T) {
	got := toCardHTML(toChatText(`<https://evil.example/x"onmouseover=alert(1)|click>`))
	if strings.Contains(got, `x"on`) {
		t.Fatalf("unescaped quote breaks out of href: %q", got)
	}
	if !strings.Contains(got, "&quot;onmouseover") {
		t.Fatalf("quote not escaped: %q", got)
	}
}

// The card emphasis pass runs over router text full of snake_case identifiers;
// without the same neighbor guards toChatText uses, they render as emphasis.
func TestToCardHTMLLeavesSnakeCaseAlone(t *testing.T) {
	for _, in := range []string{
		"turn failed: max_turns_reached in run_loop",
		"2 * 3 and 4 * 5",
	} {
		got := toCardHTML(in)
		if strings.Contains(got, "<i>") || strings.Contains(got, "<b>") {
			t.Fatalf("toCardHTML(%q) = %q, want no emphasis", in, got)
		}
	}
}

func TestStripLeadEmoji(t *testing.T) {
	tests := []struct{ in, want string }{
		{"⏳ Working…", "Working…"},
		{"🔧 Running `bash`", "Running `bash`"},
		{"⚠️ That turn didn't go through.", "That turn didn't go through."},
		{"no emoji here", "no emoji here"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := stripLeadEmoji(tt.in); got != tt.want {
			t.Fatalf("stripLeadEmoji(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestClamp(t *testing.T) {
	if got := clamp("short", 100); got != "short" {
		t.Fatalf("clamp should leave a short string alone, got %q", got)
	}
	long := strings.Repeat("x", 100)
	got := clamp(long, 20)
	if len(got) > 20 {
		t.Fatalf("clamp(%d) produced %d bytes", 20, len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("clamped text should be marked with an ellipsis, got %q", got)
	}
	// A cut inside a multi-byte rune must back up to the boundary.
	multi := strings.Repeat("世", 10)
	if c := clamp(multi, 11); strings.ContainsRune(c, '�') {
		t.Fatalf("clamp split a rune: %q", c)
	}
}

// A pipe table reaches Chat as aligned columns in a code block: Chat renders
// no tables, and the raw source's delimiter row read like a blank first row
// (seen live on the first HTTP-ingress session). Markup in the cells is
// flattened rather than left to show literally inside the block.
func TestToChatTextRendersATableAsAlignedCode(t *testing.T) {
	in := "Clusters:\n\n| Name | **Nodes** |\n| :--- | ---: |\n| ap-agones | 5 |\n| kg-sandbox | 12 |"
	want := "Clusters:\n\n```\nName        Nodes\n----------  -----\nap-agones       5\nkg-sandbox     12\n```"
	if got := toChatText(in); got != want {
		t.Errorf("toChatText:\n%s\nwant:\n%s", got, want)
	}
}

// The card path converts too: an answer with a header (so it earns a card)
// and a table carries the table as a fenced block in a MARKDOWN paragraph.
func TestAnswerCardRendersATableAsAlignedCode(t *testing.T) {
	card := answerCard("## Clusters\n\n| Name | Nodes |\n|---|---|\n| ap-agones | 5 |")
	if card == nil {
		t.Fatal("no card")
	}
	got := cardText(card)
	if !strings.Contains(got, "```\nName       Nodes\n---------  -----\nap-agones  5\n```") {
		t.Errorf("card text does not carry the aligned table:\n%s", got)
	}
	if strings.Contains(got, "|---|") {
		t.Errorf("the raw delimiter row survived:\n%s", got)
	}
}
