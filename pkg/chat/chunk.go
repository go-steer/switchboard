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
	"strings"
	"unicode/utf8"
)

// fence is the code-block delimiter every platform switchboard speaks to
// renders. Both adapters strip a fence's language tag before chunking — Chat
// and Slack show the tag as the code's literal first line rather than
// highlighting it — so a reopened block needs nothing but the marker.
const fence = "```"

// ChunkText splits text into pieces of at most limit bytes for a platform that
// caps message length, so a long model turn becomes several ordered posts
// rather than a truncated one.
//
// Four properties, in the order they matter:
//
//   - A fenced code block is never left unbalanced. Every platform switchboard
//     speaks to renders an odd ``` literally, so a split landing inside a block
//     puts raw backticks on screen in both halves and loses the monospace in
//     one — the failure this exists to prevent (#31). A piece left open is
//     closed, and the next reopened, so each renders correctly alone. A break
//     is never taken inside the marker itself either, which would leave a
//     stray backtick that no amount of balancing can see.
//   - Nothing is dropped but the line ending the break is taken on. The pieces
//     rejoin into the original text, which matters most inside a code block: a
//     swallowed blank line changes what the model actually wrote.
//   - The break prefers the last line ending in the window, so a reply's block
//     structure survives it.
//   - A multi-byte rune is never split. Half a rune renders as a replacement
//     character, which reads as corruption rather than continuation.
//
// Text that already fits is returned whole, as a single-element slice, so a
// caller can treat the result uniformly.
//
// The size bound holds for any limit a real platform sets. Below about 16 the
// headroom reserved for the markers collapses (see the floor in the window
// calculation) and a balanced piece can exceed limit by the marker's cost;
// that is the better of the two failures, since the alternative shreds the
// text into fragments.
func ChunkText(text string, limit int) []string {
	if len(text) <= limit {
		return []string{text}
	}
	if limit < 1 {
		return []string{text} // nothing sensible to split on
	}

	// Reserve headroom for the closing marker a piece may gain, so closing it
	// cannot push it back over the platform's ceiling. The reopening marker on
	// the *next* piece is subtracted separately, below, since it is only paid
	// when a block is actually open. Floored at half the limit, because a tiny
	// limit would otherwise collapse the window to nothing.
	window := limit
	if countFences(text) > 0 {
		window = max(limit-len(fence)-1, limit/2, 1)
	}

	var out []string
	open := false // a fenced block is still open at the end of the last piece
	remaining := text
	for remaining != "" {
		prefix := ""
		if open {
			prefix = fence + "\n"
		}
		if whole, _ := seal(prefix + remaining); len(whole) <= limit {
			out = append(out, whole)
			break
		}
		cut, drop := chooseCut(remaining, window-len(prefix), open)
		piece, reopen := seal(prefix + remaining[:cut])
		out = append(out, piece)
		open = reopen
		remaining = remaining[cut+drop:]
	}
	return out
}

// chooseCut picks how many bytes of s belong to this piece, and how many bytes
// immediately after them are the break itself and are dropped rather than
// carried over. Only a line ending is ever dropped, and only when the break is
// taken on one: a piece that opens with the tail of the previous line's ending
// reads as a gap in the conversation. Everything else survives, so the pieces
// rejoin into s.
//
// cut is always at least 1, so a caller looping on the remainder always makes
// progress. open says whether a fenced block is already open entering this
// piece, which is what decides whether a trailing fence line opens or closes.
func chooseCut(s string, window int, open bool) (cut, drop int) {
	// Never offer the whole of s: the caller only asks when s does not fit, so
	// a cut at len(s) would loop without shrinking anything.
	if window >= len(s) {
		window = len(s) - 1
	}
	if window < 1 {
		window = 1
	}

	if nl := strings.LastIndexByte(s[:window], '\n'); nl > 0 {
		return splitAtEOL(s, backOffEmptyOpener(s, nl, open))
	}

	cut = RuneBoundary(s, window)
	if cut == 0 {
		// window lands inside the first rune: emit that whole rune so the loop
		// always makes progress (never a zero-length cut).
		_, sz := utf8.DecodeRuneInString(s)
		return sz, 0
	}
	// A hard cut must not land inside a run of backticks. Half a marker on each
	// side of the break opens no block and puts a stray backtick on screen in
	// both pieces — #31's symptom reached by a different route, and invisible
	// to the balancing pass, which would count both halves as even.
	if cut < len(s) && s[cut] == '`' && s[cut-1] == '`' {
		n := cut
		for n > 0 && s[n-1] == '`' {
			n--
		}
		if n > 0 {
			cut = n
		}
		// n == 0 means the run starts the piece and is longer than the whole
		// window: there is nothing to break on before it. Left bisected, and
		// no better answer available — a backtick run wider than a platform's
		// message limit is not text anyone meant to send.
	}
	return cut, 0
}

// splitAtEOL turns the index of the '\n' a break was taken on into the piece
// length and the size of the line ending to drop, keeping a CRLF together so a
// piece cannot end on a dangling '\r'.
func splitAtEOL(s string, nl int) (cut, drop int) {
	if nl > 1 && s[nl-1] == '\r' {
		return nl - 1, 2
	}
	return nl, 1
}

// backOffEmptyOpener moves a break that lands directly after an opening fence
// back by one line. Left where it is, the piece ends "```\n```" — an empty code
// block on screen — and the block's actual content opens the next piece anyway.
// Backing up sends the opener along with the code it opens.
func backOffEmptyOpener(s string, nl int, open bool) int {
	if open == (countFences(s[:nl])%2 == 1) {
		return nl // the piece already ends with its block closed
	}
	start := strings.LastIndexByte(s[:nl], '\n') + 1
	if start <= 1 || !isFenceLine(s[start:nl]) {
		return nl
	}
	return start - 1
}

// seal closes a piece whose fenced block the break left open, and reports
// whether the piece after it has to reopen that block.
func seal(piece string) (string, bool) {
	if countFences(piece)%2 == 0 {
		return piece, false
	}
	return piece + "\n" + fence, true
}

// countFences counts code-block delimiters: each run of three or more backticks
// is one. strings.Count would read "````" as one delimiter plus a loose
// backtick and get the parity wrong — a four-backtick fence is what a model
// reaches for when the answer is itself about markdown.
func countFences(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if s[i] != '`' {
			i++
			continue
		}
		j := i
		for j < len(s) && s[j] == '`' {
			j++
		}
		if j-i >= len(fence) {
			n++
		}
		i = j
	}
	return n
}

// isFenceLine reports whether a line is a bare delimiter and nothing else,
// which is the shape of an opener a break can usefully be moved past. A line
// with a ```span``` in it is not one, and must not be moved.
func isFenceLine(line string) bool {
	t := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
	if len(t) < len(fence) {
		return false
	}
	return strings.Trim(t, "`") == ""
}

// RuneBoundary returns the largest index <= n that starts a rune, so a hard cut
// never splits a multi-byte character. Shared by everything in the adapters
// that cuts a string to a byte budget — the chunker here, and the clamps that
// fit text into a platform's fixed-size fields.
func RuneBoundary(s string, n int) int {
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
