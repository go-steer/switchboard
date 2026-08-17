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

// This file renders a model turn's standard (CommonMark-ish) markdown into
// Google Chat's text markup — the flat payload that is the always-on baseline
// and the fallback behind every card. Chat's dialect is close to Slack's and
// nothing like CommonMark: emphasis is single-delimiter (*bold*, _italic_,
// ~strike~), links are <url|label>, and there are no headers at all. Posting
// raw markdown into Chat renders the delimiters literally, so this pass is not
// cosmetic.
//
// The strategy mirrors pkg/chat/slack/mrkdwn.go — protect spans that must
// survive verbatim behind placeholders, translate, restore — so a fix in either
// dialect is easy to carry across. Go's RE2 has no lookaround, so the passes
// that must inspect the characters bordering a match do it with index-aware
// neighbor checks.
package googlechat

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Precompiled patterns. Order and intent mirror toChatText below.
var (
	// A Chat mention that would notify a human or a whole space. Model output
	// must not be able to page @all, so the opening bracket is stripped and the
	// mention degrades to inert text.
	mentionRE = regexp.MustCompile(`<users/[^>\n]+>`)
	// A fenced code block. (?s) so . spans newlines; non-greedy to stop at the
	// first closing fence.
	fenceRE = regexp.MustCompile("(?s)```(?:[^\n]*\n)?.*?```")
	// The optional language tag on a genuine opening fence (```go\n). Chat has
	// no syntax highlighting and would render the tag as the code's first line.
	openFenceLangRE = regexp.MustCompile("(?s)\\A```[^\\s`]+[ \t]*(\r?\n)")
	inlineCodeRE    = regexp.MustCompile("`[^`]+`")
	// [label](url), tolerating one level of balanced parens in the url.
	mdLinkRE = regexp.MustCompile(`\[([^\]]+)\]\(([^()]*(?:\([^()]*\)[^()]*)*)\)`)
	// An already-formed Chat link (<url|label> or bare <url>), protected so the
	// emphasis passes cannot chew on a url.
	chatLinkRE = regexp.MustCompile(`<(?:https?|mailto|tel):[^>\n]+>`)
	// Setext-style and ATX headers. Chat has no header syntax; both become bold.
	mdHeaderRE   = regexp.MustCompile(`(?m)^\s{0,3}#{1,6}\s+(.+?)\s*#*\s*$`)
	headerBoldRE = regexp.MustCompile(`\*\*(.+?)\*\*`)
	boldItalicRE = regexp.MustCompile(`\*\*\*(.+?)\*\*\*`)
	mdBoldRE     = regexp.MustCompile(`\*\*(.+?)\*\*`)
	mdItalicRE   = regexp.MustCompile(`\*(\S(?:[^*\n]*?\S)?)\*`)
	mdStrikeRE   = regexp.MustCompile(`~~(.+?)~~`)
	// A horizontal rule. Chat draws no rule in plain text, so it becomes a run
	// of box-drawing characters that reads as one.
	hrRE = regexp.MustCompile(`(?m)^\s{0,3}([-*_])(?:\s*[-*_]){2,}\s*$`)
)

// hrText is what a markdown horizontal rule degrades to in plain Chat text.
const hrText = "──────────"

// stash holds substrings behind opaque placeholders so later formatting passes
// cannot touch them. Restoration is in reverse creation order so a placeholder
// nested inside a later one (a link inside bold, say) resolves.
type stash struct {
	keys []string
	vals map[string]string
	n    int
}

func newStash() *stash { return &stash{vals: make(map[string]string)} }

func (p *stash) add(v string) string {
	// NUL-delimited so it cannot collide with model text or markdown markers.
	key := "\x00GC" + strconv.Itoa(p.n) + "\x00"
	p.n++
	p.keys = append(p.keys, key)
	p.vals[key] = v
	return key
}

func (p *stash) restore(s string) string {
	for i := len(p.keys) - 1; i >= 0; i-- {
		s = strings.ReplaceAll(s, p.keys[i], p.vals[p.keys[i]])
	}
	return s
}

// replaceFunc is the index-aware analogue of Regexp.ReplaceAllStringFunc: repl
// receives the full submatch index slice (as from FindStringSubmatchIndex) into
// s, so it can inspect the characters bordering the match — the stand-in for
// RE2's missing lookaround.
func replaceFunc(re *regexp.Regexp, s string, repl func(s string, loc []int) string) string {
	locs := re.FindAllStringSubmatchIndex(s, -1)
	if locs == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, loc := range locs {
		b.WriteString(s[last:loc[0]])
		b.WriteString(repl(s, loc))
		last = loc[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

// toChatText converts standard markdown to Google Chat's text markup. Content
// that Chat has no equivalent for (headers, rules) degrades to something that
// still reads correctly rather than being dropped.
func toChatText(content string) string {
	if content == "" {
		return content
	}
	ph := newStash()
	text := content

	// Defuse mentions before anything else, so a protected span below can never
	// shelter a live <users/all>.
	text = mentionRE.ReplaceAllStringFunc(text, func(m string) string {
		return strings.TrimSuffix(strings.TrimPrefix(m, "<"), ">")
	})

	// 1) Protect fenced code blocks; strip the language tag on a real opening
	//    fence (one at line start), but never on a mid-line ```span```.
	text = replaceFunc(fenceRE, text, func(s string, loc []int) string {
		block := s[loc[0]:loc[1]]
		if loc[0] == 0 || s[loc[0]-1] == '\n' {
			block = openFenceLangRE.ReplaceAllString(block, "```$1")
		}
		return ph.add(block)
	})

	// 2) Protect inline code.
	text = inlineCodeRE.ReplaceAllStringFunc(text, ph.add)

	// 3) Convert links [label](url) -> <url|label>, skipping images (![...]),
	//    which Chat cannot render inline and which read better left literal.
	text = replaceFunc(mdLinkRE, text, func(s string, loc []int) string {
		if loc[0] > 0 && s[loc[0]-1] == '!' {
			return s[loc[0]:loc[1]]
		}
		label := s[loc[2]:loc[3]]
		url := strings.TrimSpace(s[loc[4]:loc[5]])
		url = strings.TrimSuffix(strings.TrimPrefix(url, "<"), ">")
		if url == "" {
			return label
		}
		return ph.add("<" + url + "|" + label + ">")
	})

	// 4) Protect links already written in Chat's own syntax.
	text = chatLinkRE.ReplaceAllStringFunc(text, ph.add)

	// 5) Horizontal rules, before the emphasis passes: a "***" rule would
	//    otherwise look like an unterminated bold-italic run.
	text = hrRE.ReplaceAllStringFunc(text, func(string) string { return ph.add(hrText) })

	// 6) Headers (## Title) -> *Title*, dropping redundant inner bold.
	text = replaceFunc(mdHeaderRE, text, func(s string, loc []int) string {
		inner := strings.TrimSpace(headerBoldRE.ReplaceAllString(s[loc[2]:loc[3]], "$1"))
		if inner == "" {
			return ""
		}
		return ph.add("*" + inner + "*")
	})

	// 7) ***bold italic*** -> *_text_* (Chat bold wrapping italic).
	text = replaceFunc(boldItalicRE, text, func(s string, loc []int) string {
		return ph.add("*_" + s[loc[2]:loc[3]] + "_*")
	})

	// 8) **bold** -> *bold*.
	text = replaceFunc(mdBoldRE, text, func(s string, loc []int) string {
		return ph.add("*" + s[loc[2]:loc[3]] + "*")
	})

	// 9) Single *italic* -> _italic_, but only when not adjacent to another *
	//    (RE2 has no lookaround, so check the bordering bytes here). Existing
	//    _italic_ is already valid Chat and left untouched.
	text = replaceFunc(mdItalicRE, text, func(s string, loc []int) string {
		if loc[0] > 0 && s[loc[0]-1] == '*' {
			return s[loc[0]:loc[1]]
		}
		if loc[1] < len(s) && s[loc[1]] == '*' {
			return s[loc[0]:loc[1]]
		}
		return ph.add("_" + s[loc[2]:loc[3]] + "_")
	})

	// 10) ~~strike~~ -> ~strike~.
	text = replaceFunc(mdStrikeRE, text, func(s string, loc []int) string {
		return ph.add("~" + s[loc[2]:loc[3]] + "~")
	})

	return ph.restore(text)
}

// Patterns for the card dialect. They run over text that is already Chat
// markup (the output of toChatText), so the delimiters are unambiguous.
var (
	chatBoldRE   = regexp.MustCompile(`\*([^*\n]+)\*`)
	chatItalicRE = regexp.MustCompile(`_([^_\n]+)_`)
	chatStrikeRE = regexp.MustCompile(`~([^~\n]+)~`)
	// <url|label>, the Chat text link toChatText emits.
	chatLabelledLinkRE = regexp.MustCompile(`<((?:https?|mailto|tel):[^>|\n]+)\|([^>\n]*)>`)
	htmlEscaper        = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	leadEmojiRE        = regexp.MustCompile(`^[\x{1F000}-\x{1FAFF}\x{2190}-\x{2BFF}\x{FE0F}\x{200D}]+\s*`)
)

// toCardHTML converts Chat text markup into the small HTML subset a card's
// DecoratedText accepts (<b>, <i>, <s>, <a>, <br>). It is deliberately a second
// pass over toChatText's output rather than a second markdown renderer: with
// the dialect already normalized the delimiters are unambiguous, and — the
// reason this exists at all — everything that is not markup gets escaped, so a
// gateway message containing angle brackets (`progress <off|stream>`, say)
// renders as written instead of as broken HTML.
func toCardHTML(text string) string {
	if text == "" {
		return text
	}
	ph := newStash()

	// Protect inline code first: its contents are escaped but keep their
	// backticks, since a card has no monospace style to map them onto.
	text = inlineCodeRE.ReplaceAllStringFunc(text, func(m string) string {
		return ph.add("`" + htmlEscaper.Replace(strings.Trim(m, "`")) + "`")
	})
	// Links become anchors; both halves are escaped inside the tag.
	text = replaceFunc(chatLabelledLinkRE, text, func(s string, loc []int) string {
		url := htmlEscaper.Replace(s[loc[2]:loc[3]])
		label := htmlEscaper.Replace(s[loc[4]:loc[5]])
		if label == "" {
			label = url
		}
		return ph.add(`<a href="` + url + `">` + label + `</a>`)
	})

	text = htmlEscaper.Replace(text)

	text = replaceFunc(chatBoldRE, text, func(s string, loc []int) string {
		return ph.add("<b>" + s[loc[2]:loc[3]] + "</b>")
	})
	text = replaceFunc(chatItalicRE, text, func(s string, loc []int) string {
		return ph.add("<i>" + s[loc[2]:loc[3]] + "</i>")
	})
	text = replaceFunc(chatStrikeRE, text, func(s string, loc []int) string {
		return ph.add("<s>" + s[loc[2]:loc[3]] + "</s>")
	})

	// A card renders no literal newlines.
	text = strings.ReplaceAll(text, "\n", "<br>")
	return ph.restore(text)
}

// stripLeadEmoji removes a leading emoji (and the space after it) from a
// gateway message. The router prefixes its notices with one — ⏳, 🔧, ⚠️ —
// because in plain text that emoji *is* the icon; on a card the widget's own
// icon says the same thing, and showing both reads as a stutter.
func stripLeadEmoji(s string) string {
	return strings.TrimSpace(leadEmojiRE.ReplaceAllString(s, ""))
}

// clamp truncates s to at most limit bytes, appending an ellipsis and never
// splitting a rune — for the places Chat takes one field and cannot be given a
// second message (a card's text widget, a fallback string).
func clamp(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	const ellipsis = "…" // 3 bytes
	cut := runeBoundary(s, limit-len(ellipsis))
	return strings.TrimRight(s[:cut], " ") + ellipsis
}

// runeBoundary returns the largest index <= n that starts a rune, so a hard cut
// never splits a multi-byte character.
func runeBoundary(s string, n int) int {
	if n >= len(s) {
		return len(s)
	}
	if n < 0 {
		return 0
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return n
}
