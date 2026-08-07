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

// This file renders a model turn's markdown into Slack Block Kit blocks — the
// opt-in rich alternative to the flat mrkdwn text payload (see mrkdwn.go),
// giving structural primitives that plain mrkdwn can only approximate: real
// headers, dividers, nested rich_text lists, native table blocks, and
// blockquotes. It is the substrate for interactive elements (buttons, selects)
// added later. A faithful port of hermes-agent's block_kit.py so fixes port
// between the repos.
//
// Two hard rules from that design, preserved here:
//   - renderBlocks returns nil on empty / over-limit / unexpected input; the
//     caller then falls back to the plain mrkdwn text. A rich render is a
//     nice-to-have — it must never lose a message.
//   - Every blocks payload the adapter sends is paired with a mrkdwn text
//     fallback (Slack uses it for notifications, screen readers, old clients),
//     and sanitizeBlocks clamps the payload to Slack's hard limits just before
//     the API call.
//
// Blocks are raw map[string]any (mirroring the upstream dicts) wrapped as
// slack.Block at send time; Go's RE2 lacks lookaround, so inline passes that
// inspect bordering characters do it with index checks (as in mrkdwn.go).
package slack

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/slack-go/slack"
)

// Slack Block Kit hard limits.
const (
	maxBlocks      = 50
	maxSectionText = 3000
	maxHeaderText  = 150
	maxTableRows   = 100
	maxTableCols   = 20
	maxTableChars  = 10000 // aggregate across all cells
)

// rawBlock wraps a Block Kit block expressed as a JSON object so it satisfies
// slack.Block (for MsgOptionBlocks) while marshaling as the underlying map —
// letting us emit block types slack-go has no typed struct for (e.g. table).
type rawBlock struct{ m map[string]any }

func (b rawBlock) BlockType() slack.MessageBlockType {
	if t, ok := b.m["type"].(string); ok {
		return slack.MessageBlockType(t)
	}
	return ""
}
func (b rawBlock) ID() string                   { return "" }
func (b rawBlock) MarshalJSON() ([]byte, error) { return json.Marshal(b.m) }

// toSlackBlocks wraps rendered maps for MsgOptionBlocks.
func toSlackBlocks(blocks []map[string]any) []slack.Block {
	out := make([]slack.Block, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, rawBlock{m: b})
	}
	return out
}

// Line classification (mirrors block_kit.py).
var (
	hrLineRE       = regexp.MustCompile(`^\s{0,3}([-*_])(?:\s*[-*_]){2,}\s*$`)
	headerLineRE   = regexp.MustCompile(`^\s{0,3}(#{1,6})\s+(.+?)\s*#*\s*$`)
	fenceLineRE    = regexp.MustCompile("^\\s*(`{3,}|~{3,})(.*)$")
	orderedLineRE  = regexp.MustCompile(`^(\s*)(\d+)[.)]\s+(.*)$`)
	bulletLineRE   = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
	quoteLineRE    = regexp.MustCompile(`^\s{0,3}>\s?(.*)$`)
	tableSepLineRE = regexp.MustCompile(`^\s*\|?\s*:?-{1,}:?\s*(\|\s*:?-{1,}:?\s*)+\|?\s*$`)

	// Inline markdown -> rich_text elements. Code first (opaque), then links,
	// then emphasis. Italic has neighbor constraints handled in code (RE2).
	inlineCodeGrpRE = regexp.MustCompile("`([^`]+)`")
	inlineLinkRE    = regexp.MustCompile(`\[([^\]]+)\]\(([^()\s]+(?:\([^()]*\)[^()\s]*)*)\)`)
	inlineBoldRE    = regexp.MustCompile(`(?:\*\*|__)(.+?)(?:\*\*|__)`)
	inlineStrikeRE  = regexp.MustCompile(`~~(.+?)~~`)
	headerCleanRE   = regexp.MustCompile("[*_~`]")
)

// ---------------------------------------------------------------------------
// Inline markdown -> rich_text child elements
// ---------------------------------------------------------------------------

func cloneStyle(s map[string]bool) map[string]bool {
	out := make(map[string]bool, len(s)+1)
	for k, v := range s {
		out[k] = v
	}
	return out
}

// styleObj renders a style set as the JSON object Slack expects, or nil when
// empty (so an unstyled element carries no "style" key).
func styleObj(s map[string]bool) map[string]any {
	if len(s) == 0 {
		return nil
	}
	out := make(map[string]any, len(s))
	for k, v := range s {
		out[k] = v
	}
	return out
}

// maskProtected returns a copy of s with inline code spans and (non-image)
// links overwritten by an equal-length run of a neutral byte. Emphasis
// detection runs against this mask so a * or _ inside code or a link URL is
// never mistaken for a delimiter — and, crucially, so an emphasis run may pair
// *across* a code span (e.g. **foo `bar` baz**). Byte offsets are preserved, so
// match indices map straight back onto the original string.
func maskProtected(s string) string {
	b := []byte(s)
	blank := func(lo, hi int) {
		for i := lo; i < hi; i++ {
			b[i] = '\x01'
		}
	}
	for _, loc := range inlineCodeGrpRE.FindAllStringSubmatchIndex(s, -1) {
		blank(loc[0], loc[1])
	}
	for _, loc := range inlineLinkRE.FindAllStringSubmatchIndex(s, -1) {
		if loc[0] > 0 && s[loc[0]-1] == '!' {
			continue // image: not masked, handled as text downstream
		}
		blank(loc[0], loc[1])
	}
	return string(b)
}

// inlineElements parses inline markdown into rich_text section child elements
// (styled text + link elements). Unmatched markup is emitted verbatim, so it
// never loses characters. Emphasis binds outermost — resolved first against a
// masked copy that hides code spans and links — so bold/italic/strike can wrap
// those spans; code and links are then split out of each emphasis run.
func inlineElements(text string) []any {
	var elements []any

	emit := func(s string, style map[string]bool) {
		if s == "" {
			return
		}
		el := map[string]any{"type": "text", "text": s}
		if st := styleObj(style); st != nil {
			el["style"] = st
		}
		elements = append(elements, el)
	}

	var emphasis func(s string, style map[string]bool)
	var codeSpans func(s string, style map[string]bool)
	var linkSpans func(s string, style map[string]bool)

	// emphasis resolves bold/strike/italic on a masked copy (so delimiters are
	// found only outside code/links and may pair across them), recursing on the
	// original substrings; the leaf hands remaining text to codeSpans.
	emphasis = func(s string, style map[string]bool) {
		if s == "" {
			return
		}
		mask := maskProtected(s)
		if loc := inlineBoldRE.FindStringSubmatchIndex(mask); loc != nil {
			emphasis(s[:loc[0]], style)
			inner := cloneStyle(style)
			inner["bold"] = true
			emphasis(s[loc[2]:loc[3]], inner)
			emphasis(s[loc[1]:], style)
			return
		}
		if loc := inlineStrikeRE.FindStringSubmatchIndex(mask); loc != nil {
			emphasis(s[:loc[0]], style)
			inner := cloneStyle(style)
			inner["strike"] = true
			emphasis(s[loc[2]:loc[3]], inner)
			emphasis(s[loc[1]:], style)
			return
		}
		if start, end, cs, ce, ok := findItalic(mask); ok {
			emphasis(s[:start], style)
			inner := cloneStyle(style)
			inner["italic"] = true
			emphasis(s[cs:ce], inner)
			emphasis(s[end:], style)
			return
		}
		codeSpans(s, style)
	}

	// codeSpans emits inline `code` runs (opaque, carrying any active style) and
	// hands the gaps to linkSpans.
	codeSpans = func(s string, style map[string]bool) {
		pos := 0
		for _, loc := range inlineCodeGrpRE.FindAllStringSubmatchIndex(s, -1) {
			linkSpans(s[pos:loc[0]], style)
			cs := cloneStyle(style)
			cs["code"] = true
			emit(s[loc[2]:loc[3]], cs)
			pos = loc[1]
		}
		linkSpans(s[pos:], style)
	}

	linkSpans = func(s string, style map[string]bool) {
		pos := 0
		for _, loc := range inlineLinkRE.FindAllStringSubmatchIndex(s, -1) {
			if loc[0] > 0 && s[loc[0]-1] == '!' {
				continue // image: leave for the surrounding text run
			}
			emit(s[pos:loc[0]], style)
			linkEl := map[string]any{"type": "link", "url": s[loc[4]:loc[5]], "text": s[loc[2]:loc[3]]}
			if st := styleObj(style); st != nil {
				linkEl["style"] = st
			}
			elements = append(elements, linkEl)
			pos = loc[1]
		}
		emit(s[pos:], style)
	}

	emphasis(text, map[string]bool{})
	if len(elements) == 0 {
		return []any{map[string]any{"type": "text", "text": text}}
	}
	return elements
}

// findItalic emulates the lookaround-guarded single-* / single-_ italic match
// that RE2 cannot express: an opening * or _ not adjacent to another * or _
// and not followed by whitespace, a non-empty inner run (no newline), and a
// closing * or _ not preceded by whitespace/emphasis and not followed by *.
func findItalic(s string) (start, end, contentStart, contentEnd int, ok bool) {
	isDelim := func(b byte) bool { return b == '*' || b == '_' }
	isSpace := func(b byte) bool { return b == ' ' || b == '\t' || b == '\n' }
	for i := 0; i < len(s); i++ {
		if !isDelim(s[i]) {
			continue
		}
		if i > 0 && isDelim(s[i-1]) { // opener adjacent to another delim => bold/emphasis run
			continue
		}
		if i+1 >= len(s) || isDelim(s[i+1]) || isSpace(s[i+1]) {
			continue
		}
		for j := i + 1; j < len(s); j++ {
			if s[j] == '\n' {
				break // content cannot cross a line
			}
			if !isDelim(s[j]) {
				continue
			}
			if isSpace(s[j-1]) || isDelim(s[j-1]) { // no space/emphasis just before close
				continue
			}
			if j+1 < len(s) && s[j+1] == '*' {
				continue
			}
			if j == i+1 { // empty content
				continue
			}
			return i, j + 1, i + 1, j, true
		}
	}
	return 0, 0, 0, 0, false
}

// ---------------------------------------------------------------------------
// Structural block builders
// ---------------------------------------------------------------------------

// nonemptyElements makes a rich_text child list safe: Slack rejects an empty
// elements list or a zero-length text element, so drop empties and, if nothing
// remains, substitute a single space (renders blank, stays schema-valid).
func nonemptyElements(elements []any) []any {
	out := make([]any, 0, len(elements))
	for _, e := range elements {
		if m, ok := e.(map[string]any); ok && m["type"] == "text" {
			if t, _ := m["text"].(string); t == "" {
				continue
			}
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		return []any{map[string]any{"type": "text", "text": " "}}
	}
	return out
}

// headerBlock builds a plain_text header (150-char cap), or nil when the text
// reduces to nothing (Slack rejects an empty plain_text).
func headerBlock(text string) map[string]any {
	clean := strings.TrimSpace(headerCleanRE.ReplaceAllString(text, ""))
	if clean == "" {
		return nil
	}
	if len(clean) > maxHeaderText {
		clean = clean[:maxHeaderText-1] + "…"
	}
	return map[string]any{"type": "header", "text": map[string]any{"type": "plain_text", "text": clean, "emoji": true}}
}

func dividerBlock() map[string]any { return map[string]any{"type": "divider"} }

func preformattedBlock(text string) map[string]any {
	return map[string]any{
		"type": "rich_text",
		"elements": []any{map[string]any{
			"type":     "rich_text_preformatted",
			"elements": nonemptyElements([]any{map[string]any{"type": "text", "text": strings.TrimRight(text, "\n")}}),
		}},
	}
}

func quoteBlock(lines []string) map[string]any {
	var children []any
	for i, ln := range lines {
		if i > 0 {
			children = append(children, map[string]any{"type": "text", "text": "\n"})
		}
		children = append(children, inlineElements(ln)...)
	}
	return map[string]any{
		"type":     "rich_text",
		"elements": []any{map[string]any{"type": "rich_text_quote", "elements": nonemptyElements(children)}},
	}
}

type listItem struct {
	indent  int
	ordered bool
	text    string
}

// listBlock builds one rich_text block from consecutive list items. Each run
// sharing (indent, ordered) becomes a rich_text_list; a change starts a new
// one, which is how Slack renders nesting.
func listBlock(items []listItem) map[string]any {
	var elements []any
	var cur map[string]any
	haveCur := false
	var curIndent int
	var curOrdered bool
	for _, it := range items {
		if !haveCur || it.indent != curIndent || it.ordered != curOrdered {
			style := "bullet"
			if it.ordered {
				style = "ordered"
			}
			cur = map[string]any{"type": "rich_text_list", "style": style, "indent": it.indent, "elements": []any{}}
			elements = append(elements, cur)
			curIndent, curOrdered, haveCur = it.indent, it.ordered, true
		}
		cur["elements"] = append(cur["elements"].([]any),
			map[string]any{"type": "rich_text_section", "elements": nonemptyElements(inlineElements(it.text))})
	}
	return map[string]any{"type": "rich_text", "elements": elements}
}

func sectionBlock(text string) map[string]any {
	return map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": text}}
}

// ---------------------------------------------------------------------------
// Tables — native Block Kit table block, monospace fallback
// ---------------------------------------------------------------------------

func parseAlignment(sep string) []string {
	var aligns []string
	for _, cell := range strings.Split(strings.Trim(strings.TrimSpace(sep), "|"), "|") {
		c := strings.TrimSpace(cell)
		left := strings.HasPrefix(c, ":")
		right := strings.HasSuffix(c, ":")
		switch {
		case left && right:
			aligns = append(aligns, "center")
		case right:
			aligns = append(aligns, "right")
		default:
			aligns = append(aligns, "left")
		}
	}
	return aligns
}

func splitRow(row string) []string {
	protected := strings.ReplaceAll(strings.Trim(strings.TrimSpace(row), "|"), `\|`, "\x00PIPE\x00")
	cells := strings.Split(protected, "|")
	for i, c := range cells {
		cells[i] = strings.ReplaceAll(strings.TrimSpace(c), "\x00PIPE\x00", "|")
	}
	return cells
}

func richTextCell(text string) map[string]any {
	return map[string]any{
		"type":     "rich_text",
		"elements": []any{map[string]any{"type": "rich_text_section", "elements": nonemptyElements(inlineElements(text))}},
	}
}

// tableBlock builds a native table block, or nil when it exceeds Slack's
// limits or parses to nothing (caller then uses the monospace fallback).
func tableBlock(rows []string, sep string) map[string]any {
	var parsed [][]string
	for _, r := range rows {
		if strings.TrimSpace(r) != "" {
			parsed = append(parsed, splitRow(r))
		}
	}
	if len(parsed) == 0 {
		return nil
	}
	ncols := 0
	for _, r := range parsed {
		if len(r) > ncols {
			ncols = len(r)
		}
	}
	if len(parsed) > maxTableRows || ncols > maxTableCols {
		return nil
	}
	total := 0
	for i := range parsed {
		for len(parsed[i]) < ncols {
			parsed[i] = append(parsed[i], "")
		}
		for _, c := range parsed[i] {
			total += len(c)
		}
	}
	if total > maxTableChars {
		return nil
	}

	aligns := parseAlignment(sep)
	lastNonDefault := -1
	for c := 0; c < ncols && c < maxTableCols; c++ {
		a := "left"
		if c < len(aligns) {
			a = aligns[c]
		}
		if a != "left" {
			lastNonDefault = c
		}
	}
	var settings []any
	for c := 0; c <= lastNonDefault; c++ {
		a := "left"
		if c < len(aligns) {
			a = aligns[c]
		}
		settings = append(settings, map[string]any{"align": a})
	}

	var rowsOut []any
	for _, row := range parsed {
		var cells []any
		for _, cell := range row {
			cells = append(cells, richTextCell(cell))
		}
		rowsOut = append(rowsOut, cells)
	}
	block := map[string]any{"type": "table", "rows": rowsOut}
	if len(settings) > 0 {
		block["column_settings"] = settings
	}
	return block
}

// renderTable renders pipe-table rows as aligned monospace text (fallback).
func renderTable(rows []string) string {
	var parsed [][]string
	for _, r := range rows {
		parsed = append(parsed, splitRow(r))
	}
	if len(parsed) == 0 {
		return strings.Join(rows, "\n")
	}
	ncols := 0
	for _, r := range parsed {
		if len(r) > ncols {
			ncols = len(r)
		}
	}
	widths := make([]int, ncols)
	for i := range parsed {
		for len(parsed[i]) < ncols {
			parsed[i] = append(parsed[i], "")
		}
		for c := 0; c < ncols; c++ {
			if len(parsed[i][c]) > widths[c] {
				widths[c] = len(parsed[i][c])
			}
		}
	}
	var out []string
	for ri, r := range parsed {
		cells := make([]string, ncols)
		for c := 0; c < ncols; c++ {
			cells[c] = r[c] + strings.Repeat(" ", widths[c]-len(r[c]))
		}
		out = append(out, strings.TrimRight(strings.Join(cells, " | "), " "))
		if ri == 0 {
			seps := make([]string, ncols)
			for c := 0; c < ncols; c++ {
				seps[c] = strings.Repeat("-", widths[c])
			}
			out = append(out, strings.Join(seps, "-+-"))
		}
	}
	return strings.Join(out, "\n")
}

// ---------------------------------------------------------------------------
// Public entry point
// ---------------------------------------------------------------------------

func isListLine(line string) bool {
	return bulletLineRE.MatchString(line) || orderedLineRE.MatchString(line)
}

// dedent strips the longest run of leading whitespace common to every non-blank
// line, preserving relative indentation. A fenced code block nested under a list
// item would otherwise carry the item's indentation into the rendered code.
func dedent(lines []string) []string {
	min := -1
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		n := len(ln) - len(strings.TrimLeft(ln, " \t"))
		if min < 0 || n < min {
			min = n
		}
	}
	if min <= 0 {
		return lines
	}
	out := make([]string, len(lines))
	for i, ln := range lines {
		if len(ln) >= min {
			out[i] = ln[min:]
		} else {
			out[i] = strings.TrimLeft(ln, " \t")
		}
	}
	return out
}

func indentLevel(spaces string) int {
	width := 0
	for _, ch := range spaces {
		if ch == '\t' {
			width += 4
		} else {
			width++
		}
	}
	if lvl := width / 2; lvl < 5 {
		return lvl
	}
	return 5
}

// renderBlocks converts markdown to a Block Kit blocks list, or nil when the
// content is empty, exceeds Slack's structural limits, or hits an unexpected
// shape — the caller then falls back to the flat text payload. mrkdwnFn (the
// adapter's toMrkdwn) formats section paragraphs. Never panics.
func renderBlocks(markdown string, mrkdwnFn func(string) string) (blocks []map[string]any) {
	if strings.TrimSpace(markdown) == "" {
		return nil
	}
	if mrkdwnFn == nil {
		mrkdwnFn = func(s string) string { return s }
	}
	defer func() {
		if recover() != nil {
			blocks = nil // a rendering bug must never drop a message
		}
	}()

	lines := strings.Split(strings.ReplaceAll(markdown, "\r\n", "\n"), "\n")
	n := len(lines)
	var para []string

	flushPara := func() {
		if len(para) == 0 {
			return
		}
		text := strings.TrimSpace(strings.Join(para, "\n"))
		para = para[:0]
		if text == "" {
			return
		}
		for _, chunk := range chunkMessage(mrkdwnFn(text), maxSectionText) {
			blocks = append(blocks, sectionBlock(chunk))
		}
	}

	for i := 0; i < n; {
		line := lines[i]

		if strings.TrimSpace(line) == "" {
			flushPara()
			i++
			continue
		}
		if m := fenceLineRE.FindStringSubmatch(line); m != nil {
			flushPara()
			marker := m[1]
			var body []string
			i++
			for i < n && !strings.HasPrefix(strings.TrimLeft(lines[i], " \t"), marker) {
				body = append(body, lines[i])
				i++
			}
			i++ // consume closing fence
			blocks = append(blocks, preformattedBlock(strings.Join(dedent(body), "\n")))
			continue
		}
		if hrLineRE.MatchString(line) {
			flushPara()
			blocks = append(blocks, dividerBlock())
			i++
			continue
		}
		if m := headerLineRE.FindStringSubmatch(line); m != nil {
			flushPara()
			if h := headerBlock(m[2]); h != nil {
				blocks = append(blocks, h)
			}
			i++
			continue
		}
		if strings.Contains(line, "|") && i+1 < n && tableSepLineRE.MatchString(lines[i+1]) {
			flushPara()
			trows := []string{line}
			sep := lines[i+1]
			i += 2
			for i < n && strings.Contains(lines[i], "|") && strings.TrimSpace(lines[i]) != "" {
				trows = append(trows, lines[i])
				i++
			}
			if t := tableBlock(trows, sep); t != nil {
				blocks = append(blocks, t)
			} else {
				blocks = append(blocks, preformattedBlock(renderTable(trows)))
			}
			continue
		}
		if quoteLineRE.MatchString(line) {
			flushPara()
			var qlines []string
			for i < n {
				qm := quoteLineRE.FindStringSubmatch(lines[i])
				if qm == nil {
					break
				}
				qlines = append(qlines, qm[1])
				i++
			}
			blocks = append(blocks, quoteBlock(qlines))
			continue
		}
		if isListLine(line) {
			flushPara()
			var items []listItem
			for i < n {
				if bm := bulletLineRE.FindStringSubmatch(lines[i]); bm != nil {
					items = append(items, listItem{indent: indentLevel(bm[1]), ordered: false, text: bm[2]})
					i++
				} else if om := orderedLineRE.FindStringSubmatch(lines[i]); om != nil {
					items = append(items, listItem{indent: indentLevel(om[1]), ordered: true, text: om[3]})
					i++
				} else if fenceLineRE.MatchString(lines[i]) {
					// A fenced code block (even indented under an item) ends the
					// list: rich_text_list children are inline-only, so the outer
					// loop renders the fence as its own preformatted block rather
					// than flattening it into the item's text.
					break
				} else if strings.TrimSpace(lines[i]) != "" && (strings.HasPrefix(lines[i], " ") || strings.HasPrefix(lines[i], "\t")) && len(items) > 0 {
					items[len(items)-1].text += " " + strings.TrimSpace(lines[i])
					i++
				} else if strings.TrimSpace(lines[i]) == "" && len(items) > 0 {
					// Blank line inside a list: if the next non-blank line is
					// another item, treat the blank as a soft separator so the
					// run stays one list (Slack renumbers a new list from 1).
					j := i + 1
					for j < n && strings.TrimSpace(lines[j]) == "" {
						j++
					}
					if j < n && isListLine(lines[j]) {
						i = j
					} else {
						break
					}
				} else {
					break
				}
			}
			blocks = append(blocks, listBlock(items))
			continue
		}

		para = append(para, line)
		i++
	}
	flushPara()

	if len(blocks) == 0 || len(blocks) > maxBlocks {
		return nil // empty, or too complex to express safely -> plain text
	}
	return blocks
}

// clampTextObj truncates a text object's "text" so the result stays within
// limit bytes, appending an ellipsis and never splitting a rune. The ellipsis
// (U+2026) is 3 bytes, so the cut point accounts for it.
func clampTextObj(obj map[string]any, limit int) {
	t, _ := obj["text"].(string)
	if len(t) <= limit {
		return
	}
	const ellipsis = "…" // 3 bytes
	cut := runeBoundary(t, limit-len(ellipsis))
	obj["text"] = strings.TrimRight(t[:cut], " ") + ellipsis
}

// sanitizeBlocks clamps an outbound blocks payload to Slack's hard limits just
// before the API call: one oversized/empty block fails the whole call with
// invalid_blocks. Drops empties, truncates over-long text, caps at 50 blocks.
// Returns nil when nothing valid remains (caller sends the text fallback).
func sanitizeBlocks(blocks []map[string]any) (out []map[string]any) {
	if len(blocks) == 0 {
		return nil
	}
	defer func() {
		if recover() != nil {
			out = nil
		}
	}()
	for _, b := range blocks {
		t, _ := b["type"].(string)
		switch t {
		case "":
			continue
		case "section":
			textObj, hasText := b["text"].(map[string]any)
			hasBody := b["fields"] != nil || b["accessory"] != nil
			if hasText {
				if s, _ := textObj["text"].(string); strings.TrimSpace(s) == "" && !hasBody {
					continue
				}
				clampTextObj(textObj, maxSectionText)
			} else if !hasBody {
				continue
			}
		case "header":
			textObj, ok := b["text"].(map[string]any)
			if !ok {
				continue
			}
			if s, _ := textObj["text"].(string); strings.TrimSpace(s) == "" {
				continue
			}
			clampTextObj(textObj, maxHeaderText)
		case "context":
			if els, _ := b["elements"].([]any); len(els) == 0 {
				continue
			}
		case "rich_text", "actions", "context_actions":
			if els, _ := b["elements"].([]any); len(els) == 0 {
				continue
			}
		case "table":
			if rows, _ := b["rows"].([]any); len(rows) == 0 {
				continue
			}
		}
		out = append(out, b)
	}
	if len(out) == 0 {
		return nil
	}
	if len(out) > maxBlocks {
		out = out[:maxBlocks]
	}
	return out
}
