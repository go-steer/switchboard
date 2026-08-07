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
	"strings"
	"testing"
	"unicode/utf8"
)

func TestToMrkdwn(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain", "just words", "just words"},

		{"bold", "**bold**", "*bold*"},
		{"italic star", "*italic*", "_italic_"},
		{"italic underscore untouched", "_italic_", "_italic_"},
		{"bold italic", "***both***", "*_both_*"},
		{"strikethrough", "~~gone~~", "~gone~"},
		{"bold and italic together", "**b** and *i*", "*b* and _i_"},

		// Slack truncates a message when a closing * follows a non-word char,
		// so a zero-width space is inserted before it.
		{"bold trailing punctuation", "**done.**", "*done.\u200b*"},

		// Literal asterisks with whitespace on the inner edge are not emphasis.
		{"spaced stars not italic", "a * b * c", "a * b * c"},

		{"header", "# Title", "*Title*"},
		{"header strips inner bold", "## Sub **b**", "*Sub b*"},

		{"link", "[Google](https://g.com)", "<https://g.com|Google>"},
		{"image left literal", "![alt](http://x)", "![alt](http://x)"},
		{"link nested in bold", "**[G](http://g)**", "*<http://g|G>\u200b*"},

		// Existing Slack entities/links must survive untouched (not escaped).
		{"user mention preserved", "hi <@U123>", "hi <@U123>"},
		{"autolink preserved", "<https://x.com>", "<https://x.com>"},

		// Broadcast mentions are defused so model output cannot page a channel:
		// the leading < is neutralized, and the trailing > is escaped by the
		// control-char pass (harmless — the mention is already dead).
		{"broadcast channel escaped", "<!channel> ping", "&lt;!channel&gt; ping"},
		{"broadcast labelled escaped", "<!here|here>", "&lt;!here|here&gt;"},

		// Code is opaque: no conversion, no escaping inside.
		{"inline code opaque", "`*not bold*`", "`*not bold*`"},
		{"inline code keeps angles", "`<b>`", "`<b>`"},
		{"fence strips language tag", "```go\nx < y\n```", "```\nx < y\n```"},

		// Control characters in plain text are escaped, idempotently.
		{"control chars escaped", "a & b < c > d", "a &amp; b &lt; c &gt; d"},
		{"no double escape", "a &amp; b", "a &amp; b"},

		{"blockquote marker preserved", "> quote", "> quote"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := toMrkdwn(tc.in); got != tc.want {
				t.Errorf("toMrkdwn(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestChunkMessageShort(t *testing.T) {
	got := chunkMessage("hello world", slackTextLimit)
	if len(got) != 1 || got[0] != "hello world" {
		t.Fatalf("short text should pass through unsplit: %q", got)
	}
}

func TestChunkMessageSplitsOnNewline(t *testing.T) {
	got := chunkMessage("aaaa\nbbbb\ncccc", 10)
	if len(got) != 2 {
		t.Fatalf("got %d chunks, want 2: %q", len(got), got)
	}
	if got[0] != "aaaa\nbbbb" || got[1] != "cccc" {
		t.Errorf("chunks = %q, want [\"aaaa\\nbbbb\" \"cccc\"]", got)
	}
	// No content is lost across the split (only the boundary newline).
	if joined := strings.Join(got, "\n"); joined != "aaaa\nbbbb\ncccc" {
		t.Errorf("rejoined = %q", joined)
	}
}

func TestChunkMessageHardCutKeepsRunesIntact(t *testing.T) {
	// Multi-byte runes, no newline to split on, so the cut is a hard one that
	// must land on a rune boundary.
	got := chunkMessage(strings.Repeat("α", 5), 4) // 2 bytes each => 10 bytes
	if len(got) < 2 {
		t.Fatalf("expected multiple chunks, got %q", got)
	}
	var rebuilt strings.Builder
	for _, c := range got {
		if !utf8.ValidString(c) {
			t.Errorf("chunk %q is not valid UTF-8", c)
		}
		rebuilt.WriteString(c)
	}
	if rebuilt.String() != strings.Repeat("α", 5) {
		t.Errorf("rebuilt = %q, want %q", rebuilt.String(), strings.Repeat("α", 5))
	}
}

func TestChunkMessageLimitSmallerThanRune(t *testing.T) {
	// A limit below the first rune's byte width must still make progress
	// (one rune per chunk) rather than loop forever on a zero-length cut.
	got := chunkMessage("αβγ", 1) // each rune is 2 bytes, limit 1
	if len(got) != 3 {
		t.Fatalf("got %d chunks, want 3 (one rune each): %q", len(got), got)
	}
	for _, c := range got {
		if !utf8.ValidString(c) || utf8.RuneCountInString(c) != 1 {
			t.Errorf("chunk %q should be exactly one valid rune", c)
		}
	}
}

func TestChunkMessageBalancesCodeFences(t *testing.T) {
	text := "```\n" + strings.Repeat("line\n", 20) + "```"
	got := chunkMessage(text, 20)
	if len(got) < 2 {
		t.Fatalf("expected the fenced block to split, got %d chunk(s)", len(got))
	}
	for i, c := range got {
		if strings.Count(c, "```")%2 != 0 {
			t.Errorf("chunk %d has unbalanced code fences: %q", i, c)
		}
	}
}

func TestRuneBoundary(t *testing.T) {
	s := "aαb" // a=1 byte, α=2 bytes (idx 1..2), b=1 byte
	cases := []struct {
		n    int
		want int
	}{
		{0, 0},
		{1, 1},
		{2, 1}, // mid-α: snap back to the α start
		{3, 3},
		{99, len(s)},
	}
	for _, tc := range cases {
		if got := runeBoundary(s, tc.n); got != tc.want {
			t.Errorf("runeBoundary(%q, %d) = %d, want %d", s, tc.n, got, tc.want)
		}
	}
}
