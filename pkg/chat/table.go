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

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A table is laid out only if that does not grow it past maxTableGrowth times
// its source plus tableGrowthSlack bytes. Every row is padded to each column's
// widest cell, so one long cell in an early column multiplies across every
// row — a reply of a couple of kilobytes measured at ninety, split over thirty
// messages. A table like that is not tabular data anyone reads in columns, and
// is left as the source it arrived as. Measured on the output rather than as
// a cap on column width, because a cap also refused ordinary tables: a pod
// list whose longest name ran to 56 characters stayed raw (seen live).
//
// The budget is the reply's, not each table's: many small tables would
// otherwise each take the slack and add up past it. maxTableAdded caps the
// bytes laying out may add to one reply outright, so a large table under the
// ratio still cannot turn ten messages into two dozen. Because the budget is
// the reply's, a table declined for it could be laid out by a second pass over
// the result; the adapters apply this once per reply, so that never arises.
const (
	maxTableGrowth   = 3
	tableGrowthSlack = 512
	maxTableAdded    = 16 << 10
)

// TablesToCode rewrites every GitHub-flavoured markdown table in md as a
// fenced block of space-aligned columns, and leaves everything else as it was.
//
// A platform with no table rendering shows a pipe table as its raw source —
// Google Chat renders none, in text or in a card's MARKDOWN — and the part
// that reads worst is the delimiter row, `| :--- | :--- |`, which looks like a
// blank first row of the table. Aligned columns in a monospace block are the
// rendering every client has: the header, a rule under it, the rows lined up.
//
// Cells are flattened to plain text first, because inside a code block markup
// is shown rather than rendered: paired emphasis loses its markers, a code
// span loses its backticks but nothing inside them, and a link becomes its
// label followed by the URL. A column whose delimiter is right-aligned (`---:`)
// is right-aligned. Width is counted in runes, so wide CJK characters and
// emoji misalign; that is cosmetic.
//
// Deliberately narrower than GFM, because a false positive rewrites text that
// was not a table: the header and delimiter rows must both carry a pipe and
// start at the margin (an indented table may be inside a list item or be
// indented code, and a fence at the margin would break either), the body ends
// at a blank line or at a line that starts another block, and a table that
// laying out would balloon is left alone (maxTableGrowth). Tables inside an
// existing fenced block are code already and are left alone.
//
// It never panics: a rendering bug returns md unchanged, because this runs on
// every reply and a reply must never be lost to how it is laid out.
func TablesToCode(md string) (out string) {
	if !strings.Contains(md, "|") {
		return md
	}
	defer func() {
		if recover() != nil {
			out = md
		}
	}()
	lines := strings.Split(md, "\n")
	res := make([]string, 0, len(lines))
	var fence string // the open fence's marker, "" when outside one
	changed := false
	budget := min(maxTableGrowth*len(md)+tableGrowthSlack-len(md), maxTableAdded)
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if fence != "" {
			res = append(res, line)
			if t := strings.TrimSpace(line); strings.HasPrefix(t, fence) && strings.Trim(t, fence[:1]) == "" {
				fence = ""
			}
			continue
		}
		if m := tableFenceRE.FindStringSubmatch(line); m != nil {
			fence = m[1]
			res = append(res, line)
			continue
		}
		rows, align, next, ok := parseTable(lines, i)
		if ok {
			out := renderTable(rows, align)
			if add := addedBytes(lines[i:next], out); add <= budget {
				budget -= add
				res = append(res, out...)
				changed = true
				i = next - 1
				continue
			}
		}
		if next > 0 {
			// A table this declines to lay out is left whole, body included:
			// scanning on from its second line would find a "table" made of
			// its own rows.
			res = append(res, lines[i:next]...)
			i = next - 1
			continue
		}
		res = append(res, line)
	}
	if !changed {
		return md
	}
	return strings.Join(res, "\n")
}

var (
	tableFenceRE     = regexp.MustCompile("^\\s{0,3}(`{3,}|~{3,})")
	tableDelimCellRE = regexp.MustCompile(`^:?-+:?$`)
	// A line that starts another block ends a table's body, and cannot be its
	// header: a heading, a quote, a list item, a fence or a rule.
	tableBlockStartRE = regexp.MustCompile("^(#{1,6}\\s|>|[-*+]\\s|\\d+[.)]\\s|`{3,}|~{3,}|([-*_])(\\s*[-*_]){2,}\\s*$)")
	tableLinkRE       = regexp.MustCompile(`!?\[([^\]]*)\]\(([^)\s]*)[^)]*\)`)
	tableCodeSpanRE   = regexp.MustCompile("`([^`]+)`")
	tableBoldRE       = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	tableUnderBoldRE  = regexp.MustCompile(`__([^_]+)__`)
	tableStrikeRE     = regexp.MustCompile(`~~([^~]+)~~`)
	tableHeldRE       = regexp.MustCompile("\x00[0-9]+\x00")
)

// parseTable reports whether a table starts at lines[i], and if so its rows,
// which columns are right-aligned, and the index of the first line after it.
// next is also set when a table matched but is declined, so the caller can
// leave its whole span as written.
func parseTable(lines []string, i int) (rows [][]string, right []bool, next int, ok bool) {
	if i+1 >= len(lines) || !tableRowCandidate(lines[i]) || !tableRowCandidate(lines[i+1]) {
		return nil, nil, 0, false
	}
	header := tableCells(lines[i])
	right, ok = tableDelimiter(lines[i+1], len(header))
	if !ok {
		return nil, nil, 0, false
	}
	rows = [][]string{header}
	next = i + 2
	for ; next < len(lines); next++ {
		l := lines[next]
		if strings.TrimSpace(l) == "" || !strings.Contains(l, "|") || tableBlockStartRE.MatchString(l) {
			break
		}
		rows = append(rows, tableCells(l))
	}
	// A cell holding ``` would end the generated block: Chat's text path closes
	// a fence on the first run anywhere, whatever its width. Left as written.
	for _, l := range lines[i:next] {
		if strings.Contains(l, "```") {
			return nil, nil, next, false
		}
	}
	return rows, right, next, true
}

// addedBytes is how many bytes laying a table out adds to the reply (negative
// when it shrinks).
func addedBytes(src, out []string) int {
	n, m := 0, 0
	for _, l := range src {
		n += len(l) + 1
	}
	for _, l := range out {
		m += len(l) + 1
	}
	return m - n
}

// tableRowCandidate is a line that may be a header or delimiter row: it has a
// pipe, starts at the margin, and does not start some other block.
func tableRowCandidate(line string) bool {
	return strings.Contains(line, "|") && line != "" && line[0] != ' ' && line[0] != '\t' &&
		!tableBlockStartRE.MatchString(line)
}

// tableCells splits one table row into trimmed, flattened cells. The outer
// pipes are optional in GFM, and an escaped \| is a literal pipe inside a cell.
func tableCells(line string) []string {
	s := strings.TrimSpace(line)
	s = strings.TrimPrefix(s, "|")
	if strings.HasSuffix(s, "|") && !strings.HasSuffix(s, `\|`) {
		s = s[:len(s)-1]
	}
	var cells []string
	var cur strings.Builder
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\\' && i+1 < len(s) && s[i+1] == '|':
			cur.WriteByte('|')
			i++
		case s[i] == '|':
			cells = append(cells, flattenCell(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(s[i])
		}
	}
	return append(cells, flattenCell(cur.String()))
}

// flattenCell turns a cell's markup into the text it renders as. Only markup
// that is actually there is removed — a paired ** or a whole code span — so a
// file name like __init__.py or an expression like 2**10 survives as written.
// A code span's contents are taken verbatim and kept away from the emphasis
// passes.
func flattenCell(s string) string {
	// Text that must come through verbatim — an escaped backtick, a code
	// span's contents, a URL — is held out under an indexed placeholder while
	// the emphasis passes run, so a __dunder__ in a path or an anchor is not
	// read as bold. NUL cannot appear in the input, so the placeholders cannot
	// be forged, and one a pass drops simply vanishes rather than shifting the
	// rest.
	s = strings.ReplaceAll(s, "\x00", "")
	var held []string
	hold := func(v string) string {
		held = append(held, v)
		return "\x00" + strconv.Itoa(len(held)-1) + "\x00"
	}
	s = strings.ReplaceAll(s, "\\`", hold("`"))
	s = tableCodeSpanRE.ReplaceAllStringFunc(s, func(m string) string { return hold(m[1 : len(m)-1]) })
	s = tableLinkRE.ReplaceAllStringFunc(s, func(m string) string {
		sub := tableLinkRE.FindStringSubmatch(m)
		label, url := strings.TrimSpace(sub[1]), sub[2]
		switch {
		case url == "" || label == url:
			return label
		case label == "":
			return hold(url)
		}
		return label + " (" + hold(url) + ")"
	})
	s = tableBoldRE.ReplaceAllString(s, "$1")
	s = replaceEmphasis(tableUnderBoldRE, s)
	s = tableStrikeRE.ReplaceAllString(s, "$1")
	// Held text can itself hold a placeholder (an escaped backtick inside a
	// code span), so restore until none is left; each pass resolves one level,
	// and nesting is no deeper than the number of things held.
	for pass := 0; pass <= len(held) && strings.Contains(s, "\x00"); pass++ {
		s = tableHeldRE.ReplaceAllStringFunc(s, func(m string) string {
			if i, err := strconv.Atoi(m[1 : len(m)-1]); err == nil && i < len(held) {
				return held[i]
			}
			return ""
		})
	}
	return strings.Join(strings.Fields(s), " ")
}

// replaceEmphasis unwraps an underscore pair only where it is emphasis: not
// with a letter or digit on the outside, so my__init__.py stays as written.
func replaceEmphasis(re *regexp.Regexp, s string) string {
	var b strings.Builder
	last := 0
	for _, loc := range re.FindAllStringSubmatchIndex(s, -1) {
		if loc[0] < last {
			continue
		}
		if wordRuneBefore(s, loc[0]) || wordRuneAfter(s, loc[1]) {
			continue
		}
		b.WriteString(s[last:loc[0]])
		b.WriteString(s[loc[2]:loc[3]])
		last = loc[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

func wordRuneBefore(s string, i int) bool {
	r, _ := utf8.DecodeLastRuneInString(s[:i])
	return i > 0 && (unicode.IsLetter(r) || unicode.IsDigit(r))
}

func wordRuneAfter(s string, i int) bool {
	r, _ := utf8.DecodeRuneInString(s[i:])
	return i < len(s) && (unicode.IsLetter(r) || unicode.IsDigit(r))
}

// tableDelimiter reports whether line is the delimiter row of a table whose
// header has n cells, and which columns it right-aligns. GFM requires the two
// to have the same number of cells; a row that does not match is ordinary text.
func tableDelimiter(line string, n int) (right []bool, ok bool) {
	if !strings.Contains(line, "-") {
		return nil, false
	}
	cells := tableCells(line)
	if len(cells) != n {
		return nil, false
	}
	right = make([]bool, n)
	for i, c := range cells {
		if !tableDelimCellRE.MatchString(c) {
			return nil, false
		}
		right[i] = strings.HasSuffix(c, ":") && !strings.HasPrefix(c, ":")
	}
	return right, true
}

// renderTable lays rows out as a fenced block: header, a dashed rule, body.
// Rows with too few cells are padded and extra cells are dropped, as GFM does.
func renderTable(rows [][]string, right []bool) []string {
	n := len(right)
	width := make([]int, n)
	for c := range width {
		width[c] = 1 // an all-empty column still gets a one-dash rule
	}
	for _, r := range rows {
		for c := 0; c < n && c < len(r); c++ {
			if w := utf8.RuneCountInString(r[c]); w > width[c] {
				width[c] = w
			}
		}
	}
	line := func(cells []string) string {
		var b strings.Builder
		for c := 0; c < n; c++ {
			v := ""
			if c < len(cells) {
				v = cells[c]
			}
			pad := strings.Repeat(" ", max(0, width[c]-utf8.RuneCountInString(v)))
			if c > 0 {
				b.WriteString("  ")
			}
			if right[c] {
				b.WriteString(pad + v)
			} else {
				b.WriteString(v + pad)
			}
		}
		return strings.TrimRight(b.String(), " ")
	}
	rule := make([]string, n)
	for c := range rule {
		rule[c] = strings.Repeat("-", width[c])
	}
	out := []string{"```", line(rows[0]), line(rule)}
	for _, r := range rows[1:] {
		out = append(out, line(r))
	}
	return append(out, "```")
}
