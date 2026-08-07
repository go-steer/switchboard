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
)

// blockTypes returns the ordered "type" of each rendered block.
func blockTypes(blocks []map[string]any) []string {
	out := make([]string, len(blocks))
	for i, b := range blocks {
		out[i], _ = b["type"].(string)
	}
	return out
}

func TestRenderBlocksNilCases(t *testing.T) {
	if renderBlocks("", toMrkdwn) != nil {
		t.Error("empty input should render nil")
	}
	if renderBlocks("   \n\t\n", toMrkdwn) != nil {
		t.Error("whitespace-only input should render nil")
	}
}

func TestRenderBlocksParagraph(t *testing.T) {
	blocks := renderBlocks("**hello** world", toMrkdwn)
	if got := blockTypes(blocks); len(got) != 1 || got[0] != "section" {
		t.Fatalf("types = %v, want [section]", got)
	}
	text := blocks[0]["text"].(map[string]any)
	if text["type"] != "mrkdwn" {
		t.Errorf("section text type = %v, want mrkdwn", text["type"])
	}
	// Paragraphs go through toMrkdwn, so **hello** becomes *hello*.
	if s := text["text"].(string); !strings.Contains(s, "*hello*") {
		t.Errorf("section text = %q, want it to contain *hello*", s)
	}
}

func TestRenderBlocksHeaderAndDivider(t *testing.T) {
	blocks := renderBlocks("# Title\n\n---\n\nbody", toMrkdwn)
	got := blockTypes(blocks)
	want := []string{"header", "divider", "section"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("types = %v, want %v", got, want)
	}
	h := blocks[0]["text"].(map[string]any)
	if h["type"] != "plain_text" || h["text"] != "Title" {
		t.Errorf("header = %v, want plain_text 'Title'", h)
	}
}

func TestRenderBlocksHeaderStripsInlineMarkup(t *testing.T) {
	blocks := renderBlocks("## Sub **bold** `code`", toMrkdwn)
	h := blocks[0]["text"].(map[string]any)
	if h["text"] != "Sub bold code" {
		t.Errorf("header text = %q, want %q", h["text"], "Sub bold code")
	}
}

func TestRenderBlocksCodeFence(t *testing.T) {
	blocks := renderBlocks("```go\nx := 1\n*not bold*\n```", toMrkdwn)
	if got := blockTypes(blocks); len(got) != 1 || got[0] != "rich_text" {
		t.Fatalf("types = %v, want [rich_text]", got)
	}
	pre := blocks[0]["elements"].([]any)[0].(map[string]any)
	if pre["type"] != "rich_text_preformatted" {
		t.Fatalf("inner type = %v, want rich_text_preformatted", pre["type"])
	}
	// Code is opaque: inner markdown is not interpreted.
	txt := pre["elements"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(txt, "*not bold*") || !strings.Contains(txt, "x := 1") {
		t.Errorf("preformatted text = %q, want literal code", txt)
	}
}

func TestRenderBlocksBulletList(t *testing.T) {
	blocks := renderBlocks("- one\n- two\n- three", toMrkdwn)
	if got := blockTypes(blocks); len(got) != 1 || got[0] != "rich_text" {
		t.Fatalf("types = %v, want [rich_text]", got)
	}
	lists := blocks[0]["elements"].([]any)
	if len(lists) != 1 {
		t.Fatalf("got %d rich_text_list runs, want 1", len(lists))
	}
	l := lists[0].(map[string]any)
	if l["style"] != "bullet" || l["indent"] != 0 {
		t.Errorf("list = %v, want bullet/indent 0", l)
	}
	if items := l["elements"].([]any); len(items) != 3 {
		t.Errorf("got %d items, want 3", len(items))
	}
}

func TestRenderBlocksNestedList(t *testing.T) {
	// A change in indent or ordered-ness starts a new rich_text_list run,
	// which is how Slack renders nesting.
	blocks := renderBlocks("- top\n  - nested\n- back", toMrkdwn)
	lists := blocks[0]["elements"].([]any)
	if len(lists) != 3 {
		t.Fatalf("got %d runs, want 3 (top, nested, back)", len(lists))
	}
	nested := lists[1].(map[string]any)
	if nested["indent"] != 1 {
		t.Errorf("nested indent = %v, want 1", nested["indent"])
	}
}

func TestRenderBlocksOrderedList(t *testing.T) {
	blocks := renderBlocks("1. first\n2. second", toMrkdwn)
	l := blocks[0]["elements"].([]any)[0].(map[string]any)
	if l["style"] != "ordered" {
		t.Errorf("style = %v, want ordered", l["style"])
	}
}

func TestRenderBlocksBlockquote(t *testing.T) {
	blocks := renderBlocks("> line one\n> line two", toMrkdwn)
	if got := blockTypes(blocks); len(got) != 1 || got[0] != "rich_text" {
		t.Fatalf("types = %v, want [rich_text]", got)
	}
	q := blocks[0]["elements"].([]any)[0].(map[string]any)
	if q["type"] != "rich_text_quote" {
		t.Errorf("inner type = %v, want rich_text_quote", q["type"])
	}
}

func TestRenderBlocksNativeTable(t *testing.T) {
	md := "| a | b |\n| --- | ---: |\n| 1 | 2 |\n| 3 | 4 |"
	blocks := renderBlocks(md, toMrkdwn)
	if got := blockTypes(blocks); len(got) != 1 || got[0] != "table" {
		t.Fatalf("types = %v, want [table]", got)
	}
	rows := blocks[0]["rows"].([]any)
	if len(rows) != 3 { // header + 2 body rows
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	// Right-alignment on column 2 must surface in column_settings.
	cs, ok := blocks[0]["column_settings"].([]any)
	if !ok || len(cs) < 2 || cs[1].(map[string]any)["align"] != "right" {
		t.Errorf("column_settings = %v, want col 2 right-aligned", blocks[0]["column_settings"])
	}
}

func TestRenderBlocksTableFallbackWhenTooWide(t *testing.T) {
	// More columns than Slack allows -> monospace preformatted fallback,
	// never a dropped table.
	var header, sep strings.Builder
	for i := 0; i < maxTableCols+2; i++ {
		header.WriteString("| c ")
		sep.WriteString("| - ")
	}
	header.WriteString("|")
	sep.WriteString("|")
	md := header.String() + "\n" + sep.String() + "\n" + header.String()
	blocks := renderBlocks(md, toMrkdwn)
	if got := blockTypes(blocks); len(got) != 1 || got[0] != "rich_text" {
		t.Fatalf("types = %v, want [rich_text] fallback", got)
	}
	pre := blocks[0]["elements"].([]any)[0].(map[string]any)
	if pre["type"] != "rich_text_preformatted" {
		t.Errorf("fallback inner = %v, want rich_text_preformatted", pre["type"])
	}
}

func TestRenderBlocksEscapedPipeInCell(t *testing.T) {
	blocks := renderBlocks("| a | b |\n| - | - |\n| x \\| y | z |", toMrkdwn)
	rows := blocks[0]["rows"].([]any)
	bodyRow := rows[1].([]any)
	cell := bodyRow[0].(map[string]any)
	sec := cell["elements"].([]any)[0].(map[string]any)
	txt := sec["elements"].([]any)[0].(map[string]any)["text"].(string)
	if txt != "x | y" {
		t.Errorf("cell text = %q, want %q (escaped pipe unescaped)", txt, "x | y")
	}
}

func TestRenderBlocksTooManyBlocksReturnsNil(t *testing.T) {
	// Over 50 dividers -> renderer declines so the caller sends plain text.
	var sb strings.Builder
	for i := 0; i < maxBlocks+5; i++ {
		sb.WriteString("---\n\n")
	}
	if renderBlocks(sb.String(), toMrkdwn) != nil {
		t.Error("over-limit block count should render nil")
	}
}

func TestInlineElementsStyles(t *testing.T) {
	els := inlineElements("plain **b** _i_ ~~s~~ `c`")
	// Find the bold element and confirm its style.
	var sawBold, sawCode bool
	for _, e := range els {
		m := e.(map[string]any)
		st, _ := m["style"].(map[string]any)
		if m["text"] == "b" && st["bold"] == true {
			sawBold = true
		}
		if m["text"] == "c" && st["code"] == true {
			sawCode = true
		}
	}
	if !sawBold {
		t.Error("expected a bold-styled element for **b**")
	}
	if !sawCode {
		t.Error("expected a code-styled element for `c`")
	}
}

func TestInlineElementsLink(t *testing.T) {
	els := inlineElements("see [Google](https://g.com) now")
	var link map[string]any
	for _, e := range els {
		if m := e.(map[string]any); m["type"] == "link" {
			link = m
		}
	}
	if link == nil {
		t.Fatal("expected a link element")
	}
	if link["url"] != "https://g.com" || link["text"] != "Google" {
		t.Errorf("link = %v, want url/text Google", link)
	}
}

func TestInlineElementsImageNotLink(t *testing.T) {
	els := inlineElements("![alt](http://x)")
	for _, e := range els {
		if m := e.(map[string]any); m["type"] == "link" {
			t.Errorf("image should not become a link element: %v", m)
		}
	}
}

func TestFindItalic(t *testing.T) {
	cases := []struct {
		in      string
		wantOK  bool
		content string
	}{
		{"*hi*", true, "hi"},
		{"a *hi* b", true, "hi"},
		{"**bold**", false, ""},   // adjacent delims -> not single italic
		{"* spaced *", false, ""}, // leading space after opener
		{"no italics here", false, ""},
		{"_und_", true, "und"},
	}
	for _, tc := range cases {
		start, end, cs, ce, ok := findItalic(tc.in)
		if ok != tc.wantOK {
			t.Errorf("findItalic(%q) ok = %v, want %v", tc.in, ok, tc.wantOK)
			continue
		}
		if ok {
			if got := tc.in[cs:ce]; got != tc.content {
				t.Errorf("findItalic(%q) content = %q, want %q", tc.in, got, tc.content)
			}
			if start < 0 || end > len(tc.in) {
				t.Errorf("findItalic(%q) span out of range: %d..%d", tc.in, start, end)
			}
		}
	}
}

func TestSanitizeBlocksClamps(t *testing.T) {
	long := strings.Repeat("x", maxSectionText+500)
	blocks := []map[string]any{
		sectionBlock(long),
		{"type": "header", "text": map[string]any{"type": "plain_text", "text": strings.Repeat("H", maxHeaderText+50)}},
		sectionBlock("   "), // empty -> dropped
		dividerBlock(),
	}
	out := sanitizeBlocks(blocks)
	if out == nil {
		t.Fatal("sanitizeBlocks returned nil for valid input")
	}
	if len(out) != 3 { // empty section dropped
		t.Fatalf("got %d blocks, want 3 (empty section dropped)", len(out))
	}
	secText := out[0]["text"].(map[string]any)["text"].(string)
	if len(secText) > maxSectionText {
		t.Errorf("section text len = %d, want <= %d", len(secText), maxSectionText)
	}
	hdrText := out[1]["text"].(map[string]any)["text"].(string)
	if len(hdrText) > maxHeaderText {
		t.Errorf("header text len = %d, want <= %d", len(hdrText), maxHeaderText)
	}
}

func TestSanitizeBlocksCapsCount(t *testing.T) {
	var blocks []map[string]any
	for i := 0; i < maxBlocks+10; i++ {
		blocks = append(blocks, dividerBlock())
	}
	out := sanitizeBlocks(blocks)
	if len(out) != maxBlocks {
		t.Errorf("got %d blocks, want capped at %d", len(out), maxBlocks)
	}
}

func TestSanitizeBlocksAllEmptyReturnsNil(t *testing.T) {
	blocks := []map[string]any{sectionBlock(""), sectionBlock("   ")}
	if sanitizeBlocks(blocks) != nil {
		t.Error("all-empty blocks should sanitize to nil")
	}
}

func TestIsBlockRejection(t *testing.T) {
	if !isBlockRejection(errorString("invalid_blocks")) {
		t.Error("invalid_blocks should be a block rejection")
	}
	if isBlockRejection(errorString("channel_not_found")) {
		t.Error("channel_not_found should not be a block rejection")
	}
	if isBlockRejection(nil) {
		t.Error("nil should not be a block rejection")
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }

// TestToSlackBlocksMarshal proves the raw-map blocks serialize as their
// underlying object (via rawBlock.MarshalJSON) — the actual wire format sent
// to Slack — rather than an empty struct.
func TestToSlackBlocksMarshal(t *testing.T) {
	md := "# Heading\n\n- a\n- b\n\n| x | y |\n| - | - |\n| 1 | 2 |"
	blocks := sanitizeBlocks(renderBlocks(md, toMrkdwn))
	if blocks == nil {
		t.Fatal("expected blocks")
	}
	data, err := json.Marshal(toSlackBlocks(blocks))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(data)
	for _, want := range []string{`"type":"header"`, `"type":"rich_text_list"`, `"type":"table"`} {
		if !strings.Contains(s, want) {
			t.Errorf("marshaled blocks missing %s\n%s", want, s)
		}
	}
	// Round-trips back to a list of objects, each carrying a "type" key.
	var round []map[string]any
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(round) != len(blocks) {
		t.Fatalf("round-trip block count = %d, want %d", len(round), len(blocks))
	}
	for i, b := range round {
		if _, ok := b["type"].(string); !ok {
			t.Errorf("block %d has no type after round-trip: %v", i, b)
		}
	}
}
