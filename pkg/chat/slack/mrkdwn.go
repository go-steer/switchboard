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
// Slack mrkdwn — the flat text payload that is the always-on baseline (and
// the fallback for any richer Block Kit path added later). It is a faithful
// port of hermes-agent's slack format_message so fixes port between the
// repos; the notable divergence is that Go's RE2 has no lookaround, so the
// two passes that must inspect the characters bordering a match (skip image
// links, single-* italic) do it with index-aware neighbor checks instead of
// inline (?<!x)/(?!x).
package slack

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/go-steer/switchboard/pkg/chat"
)

// slackTextLimit is the per-message character budget. Slack accepts far more
// in a single chat.postMessage text field, but clients collapse long posts
// behind a "show more"; chunking near this size keeps each thread reply
// readable. Long turns are split across multiple in-thread posts.
const slackTextLimit = 3900

// Precompiled patterns. Order and intent mirror the hermes port; see toMrkdwn.
var (
	// Broadcast mentions Slack executes even from a bot: <!channel>,
	// <!everyone>, <!here> (optionally |labelled). Escaped so model output
	// cannot page a whole workspace/channel.
	broadcastRE = regexp.MustCompile(`<!(?:everyone|channel|here)(?:\|[^>]*)?>`)
	// A fenced code block. (?s) so . spans newlines; non-greedy to stop at
	// the first closing fence.
	fenceRE = regexp.MustCompile("(?s)```(?:[^\n]*\n)?.*?```")
	// The optional language tag on a genuine opening fence (```go\n), dropped
	// so Slack does not render it as the code's literal first line.
	openFenceLangRE = regexp.MustCompile("(?s)\\A```[^\\s`]+[ \t]*(\r?\n)")
	inlineCodeRE    = regexp.MustCompile("`[^`]+`")
	// [label](url), tolerating one level of balanced parens in the url.
	linkRE = regexp.MustCompile(`\[([^\]]+)\]\(([^()]*(?:\([^()]*\)[^()]*)*)\)`)
	// Already-formed Slack entities/links to protect from escaping.
	entityRE       = regexp.MustCompile(`<(?:[@#!]|(?:https?|mailto|tel):)[^>\n]+>`)
	blockquoteRE   = regexp.MustCompile(`(?m)^>+\s`)
	entityUnescRE  = regexp.MustCompile(`&(amp|lt|gt);`)
	headerRE       = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
	headerBoldRE   = regexp.MustCompile(`\*\*(.+?)\*\*`)
	boldItalicRE   = regexp.MustCompile(`\*\*\*(.+?)\*\*\*`)
	boldRE         = regexp.MustCompile(`\*\*(.+?)\*\*`)
	italicRE       = regexp.MustCompile(`\*(\S(?:[^*\n]*?\S)?)\*`)
	strikethruRE   = regexp.MustCompile(`~~(.+?)~~`)
	controlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
)

// phStash holds substrings behind opaque placeholders so later formatting
// passes cannot touch them. Restoration is in reverse creation order so a
// placeholder nested inside a later one (e.g. a link inside bold) resolves.
type phStash struct {
	keys []string
	vals map[string]string
	n    int
}

func newStash() *phStash { return &phStash{vals: make(map[string]string)} }

func (p *phStash) add(v string) string {
	// NUL-delimited so it cannot collide with model text or markdown markers.
	key := "\x00SL" + strconv.Itoa(p.n) + "\x00"
	p.n++
	p.keys = append(p.keys, key)
	p.vals[key] = v
	return key
}

func (p *phStash) restore(s string) string {
	for i := len(p.keys) - 1; i >= 0; i-- {
		s = strings.ReplaceAll(s, p.keys[i], p.vals[p.keys[i]])
	}
	return s
}

// reReplaceFunc is the index-aware analogue of Regexp.ReplaceAllStringFunc:
// repl receives the full submatch index slice (as from FindStringSubmatchIndex)
// into s, so it can inspect the characters bordering the match — the stand-in
// for RE2's missing lookaround.
func reReplaceFunc(re *regexp.Regexp, s string, repl func(s string, loc []int) string) string {
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

// toMrkdwn converts standard markdown to Slack mrkdwn. The strategy: protect
// spans that must survive verbatim (code, existing entities) behind
// placeholders, escape Slack control characters in what remains, translate
// the markdown constructs, then restore the placeholders. Each translated
// span is itself stashed so a later pass cannot re-interpret it.
func toMrkdwn(content string) string {
	if content == "" {
		return content
	}
	ph := newStash()
	text := content

	// Escape broadcast mentions before anything else so protected spans below
	// can never shelter a live <!channel>.
	text = broadcastRE.ReplaceAllStringFunc(text, func(m string) string {
		return strings.Replace(m, "<", "&lt;", 1)
	})

	// 1) Protect fenced code blocks; strip the language tag on a real opening
	//    fence (one at line start), but never on a mid-line ```span```.
	text = reReplaceFunc(fenceRE, text, func(s string, loc []int) string {
		block := s[loc[0]:loc[1]]
		if loc[0] == 0 || s[loc[0]-1] == '\n' {
			block = openFenceLangRE.ReplaceAllString(block, "```$1")
		}
		return ph.add(block)
	})

	// 2) Protect inline code.
	text = inlineCodeRE.ReplaceAllStringFunc(text, ph.add)

	// 3) Convert links [label](url) -> <url|label>, skipping images (![...]).
	text = reReplaceFunc(linkRE, text, func(s string, loc []int) string {
		if loc[0] > 0 && s[loc[0]-1] == '!' { // image; leave the [..](..) literal
			return s[loc[0]:loc[1]]
		}
		label := s[loc[2]:loc[3]]
		url := strings.TrimSpace(s[loc[4]:loc[5]])
		url = strings.TrimSuffix(strings.TrimPrefix(url, "<"), ">")
		return ph.add("<" + url + "|" + label + ">")
	})

	// 4) Protect existing Slack entities/manual links.
	text = entityRE.ReplaceAllStringFunc(text, ph.add)

	// 5) Protect blockquote markers so their > is not escaped below.
	text = blockquoteRE.ReplaceAllStringFunc(text, ph.add)

	// 6) Escape Slack control chars in the remaining plain text. Unescape
	//    first (single pass) so already-escaped input is not double-escaped.
	text = entityUnescRE.ReplaceAllStringFunc(text, func(m string) string {
		switch m {
		case "&amp;":
			return "&"
		case "&lt;":
			return "<"
		case "&gt;":
			return ">"
		}
		return m
	})
	text = controlEscaper.Replace(text)

	// 7) Headers (## Title) -> *Title* (bold), dropping redundant inner bold.
	text = reReplaceFunc(headerRE, text, func(s string, loc []int) string {
		inner := strings.TrimSpace(s[loc[2]:loc[3]])
		inner = headerBoldRE.ReplaceAllString(inner, "$1")
		return ph.add("*" + inner + "*")
	})

	// 8) ***bold italic*** -> *_text_* (Slack bold wrapping italic).
	text = reReplaceFunc(boldItalicRE, text, func(s string, loc []int) string {
		return ph.add("*_" + s[loc[2]:loc[3]] + "_*")
	})

	// 9) **bold** -> *bold*. Slack drops the rest of a message when a closing
	//    * is preceded by a non-word char, so insert a zero-width space there.
	text = reReplaceFunc(boldRE, text, func(s string, loc []int) string {
		inner := s[loc[2]:loc[3]]
		if r, _ := utf8.DecodeLastRuneInString(inner); r != utf8.RuneError &&
			!(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_') {
			return ph.add("*" + inner + "\u200b*")
		}
		return ph.add("*" + inner + "*")
	})

	// 10) Single *italic* -> _italic_, but only when not adjacent to another *
	//     (RE2 has no lookaround, so check the bordering bytes here). Existing
	//     _italic_ is already valid Slack and left untouched.
	text = reReplaceFunc(italicRE, text, func(s string, loc []int) string {
		if loc[0] > 0 && s[loc[0]-1] == '*' { // preceded by * -> part of **
			return s[loc[0]:loc[1]]
		}
		if loc[1] < len(s) && s[loc[1]] == '*' { // followed by * -> part of **
			return s[loc[0]:loc[1]]
		}
		return ph.add("_" + s[loc[2]:loc[3]] + "_")
	})

	// 11) ~~strike~~ -> ~strike~.
	text = reReplaceFunc(strikethruRE, text, func(s string, loc []int) string {
		return ph.add("~" + s[loc[2]:loc[3]] + "~")
	})

	return ph.restore(text)
}

// chunkMessage splits rendered mrkdwn into <= limit-byte pieces for posting as
// several ordered in-thread messages. The splitting itself — newline
// preference, rune safety, and closing/reopening a ``` block that a cut lands
// inside — is shared with the other adapters in pkg/chat, since every platform
// switchboard speaks to renders an unbalanced fence literally.
func chunkMessage(text string, limit int) []string {
	return chat.ChunkText(text, limit)
}

// runeBoundary returns the largest index <= n that starts a rune, so a hard
// cut never splits a multi-byte character. Shared with the other adapters,
// which cut to a byte budget in the same places for the same reason.
func runeBoundary(s string, n int) int { return chat.RuneBoundary(s, n) }
