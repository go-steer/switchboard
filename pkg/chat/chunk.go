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

// fence is the narrowest code-block delimiter every platform switchboard
// speaks to renders. A block may be opened by any wider run and is then closed
// only by a bare run at least that wide, so the width is carried rather than
// assumed — see openFence.
//
// Both adapters' text paths strip the language tag off an opening fence before
// chunking, since Chat and Slack show the tag as the code's literal first line
// rather than highlighting it. That is not relied on here, and could not be:
// their pattern matches exactly three backticks, so a ````markdown opener keeps
// its tag, and Chat's card path chunks the markdown unstripped. A fence
// carrying an info string can only be an opener, and a marker written by seal
// is always bare, which is what tells the two apart.
const fence = "```"

// ChunkText splits text into pieces of at most limit bytes for a platform that
// caps message length, so a long model turn becomes several ordered posts
// rather than a truncated one.
//
// Four properties, in the order they matter:
//
//   - A fenced code block is never left unbalanced. Every platform switchboard
//     speaks to renders an unclosed ``` literally, so a split landing inside a
//     block puts raw backticks on screen in both halves and loses the monospace
//     in one — the failure this exists to prevent (#31). A piece left open is
//     closed, and the next reopened with a marker of the same width, so each
//     renders correctly alone. A break is never taken inside the marker itself
//     either, which would leave a stray backtick that no amount of balancing
//     can see.
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
// The size bound holds while the limit leaves room for both markers and a rune
// between them: 2w+6 <= limit, where w is the widest run of backticks in the
// text. A piece continuing a block pays w+1 to reopen it and w+1 to close it
// again, and has to carry at least one rune — up to four bytes — or the loop
// stops making progress. Past that point a piece can exceed limit by the
// markers' cost, which is the better of the two failures: the alternative is a
// text no piece of which can be balanced. The governing quantity is the widest
// run rather than the limit alone — a wall of backticks defeats a 4096-byte
// ceiling as surely as a tiny one, though it takes two thousand of them.
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
	// when a block is actually open.
	// Sized to the widest run in the text, not to len(fence): the marker that
	// closes a ```` block has to be four backticks too, or it is content rather
	// than a close.
	window := limit
	if w := widestRun(text); w > 0 {
		window = max(limit-w-1, 1)
	}

	var out []string
	// Width of the fenced block still open at the end of the last piece, or 0
	// if none is. A width rather than a flag because reopening a block needs a
	// marker as wide as the one that opened it.
	open := 0
	remaining := text
	for remaining != "" {
		prefix := ""
		if open > 0 {
			prefix = strings.Repeat("`", open) + "\n"
		}
		if whole, _ := seal(prefix + remaining); len(whole) <= limit {
			out = append(out, whole)
			break
		}
		cut, drop := chooseCut(remaining, window-len(prefix), open)
		piece, reopen := seal(prefix + remaining[:cut])
		// If the break stranded the block's own closing fence at the head of
		// what is left, the next piece opens with a block that shuts at once —
		// an empty one on screen. Pull the closer into this piece when it still
		// fits: the piece then ends on the text's fence and needs no marker at
		// all. backOffEmptySeam moves the break back a line for the same reason,
		// but cannot when the break is already at the head of the remainder.
		if reopen > 0 {
			if end := closerLine(remaining, cut+drop, reopen); end > 0 {
				if whole, r := seal(prefix + remaining[:end]); r == 0 && len(whole) <= limit {
					piece, reopen = whole, 0
					cut, drop = end, 0
				}
			}
		}
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
// progress. open is the width of the fenced block already open entering this
// piece, which is what decides whether a trailing fence line opens or closes.
func chooseCut(s string, window int, open int) (cut, drop int) {
	// Never offer the whole of s: the caller only asks when s does not fit, so
	// a cut at len(s) would loop without shrinking anything.
	if window >= len(s) {
		window = len(s) - 1
	}
	if window < 1 {
		window = 1
	}

	// A break on a line ending keeps the reply's block structure, so it wins
	// whenever there is one in the window — except when the only one there is
	// the opening fence's own and there is no earlier line to back off to, which
	// is the shape of a block whose first line is longer than the window. Taking
	// it would post the opener by itself, an empty code block, and open the real
	// one on the next message. Falling through to a hard cut sends the opener
	// with as much of its first line as fits instead.
	if nl := strings.LastIndexByte(s[:window], '\n'); nl > 0 {
		if at := backOffEmptySeam(s, nl, open); !endsOnEmptyOpener(s, at, open) {
			return splitAtEOL(s, at)
		}
	}

	cut = RuneBoundary(s, window)
	if cut == 0 {
		// window lands inside the first rune: emit that whole rune so the loop
		// always makes progress (never a zero-length cut).
		_, sz := utf8.DecodeRuneInString(s)
		return sz, 0
	}
	// Nor may it land between the two bytes of a CRLF. The piece would end on a
	// bare '\r' and the next open on a bare '\n' — the break is a line ending
	// after all, so take it as one.
	if cut < len(s) && s[cut] == '\n' && s[cut-1] == '\r' {
		switch {
		case cut == 1:
			return cut + 1, 0 // the CRLF starts s: keep both bytes rather than emit nothing
		case cut+1 == len(s):
			return cut - 1, 0 // it ends s: dropping it would lose the text's last line
		default:
			return cut - 1, 2
		}
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
	if s[nl-1] != '\r' {
		return nl, 1
	}
	if nl == 1 {
		// The CRLF starts s, so dropping it would leave an empty piece. Keep
		// both bytes: a piece that is one blank line reads as a gap in the
		// conversation, where one ending on a bare '\r' is a broken line ending
		// in whatever renders it next.
		return 2, 0
	}
	return nl - 1, 2
}

// backOffEmptySeam moves a break that would put an empty code block on screen
// back by one line, and returns where the break should go instead.
//
// There are two ways to make one, either side of the block:
//
//   - The break lands directly after an opening fence. The piece ends
//     "```\n```" and the block's actual content opens the next piece anyway.
//     An opener carrying an info string ("```go") counts, which is the form a
//     model actually writes.
//   - The break lands directly before the closing fence. The piece is sealed,
//     the next reopens, and the text's own closer arrives immediately after —
//     so the continuation *starts* "```\n```".
//
// Backing up a line sends the opener along with its code in the first case,
// and keeps a line of code in front of the closer in the second. If backing up
// only moves the seam from one of these onto the other — a block one line long
// has nowhere good to break — the original break stands, since an empty block
// is cosmetic and looping to find a better one is not worth an unbounded walk.
func backOffEmptySeam(s string, nl int, open int) int {
	back := func(at int) int {
		start := strings.LastIndexByte(s[:at], '\n') + 1
		if start <= 1 {
			return at // backing off would leave nothing in front of the break
		}
		return start - 1
	}

	switch {
	case endsOnEmptyOpener(s, nl, open):
	case startsOnCloser(s, nl, open):
	default:
		return nl
	}

	moved := back(nl)
	if moved == nl || endsOnEmptyOpener(s, moved, open) || startsOnCloser(s, moved, open) {
		return nl
	}
	return moved
}

// endsOnEmptyOpener reports whether the line ending at nl opens a block, with
// nothing of that block in front of the break.
//
// Whether there is an earlier line to move the break to is not its business:
// backOffEmptySeam asks this of the moved break as well as of the original, and
// answering "no" for a seam that sits on the text's first line would let the
// closer-side move relocate the break onto exactly the seam this reports.
func endsOnEmptyOpener(s string, nl int, open int) bool {
	start := strings.LastIndexByte(s[:nl], '\n') + 1
	if w, _ := fenceLine(s[start:nl]); w == 0 {
		return false
	}
	// A block already open before that line means the line closes it — or is
	// narrower than the opener, or carries an info string, and is code inside
	// it. Either way it is not an opener with nothing under it.
	return openFence(s[:start], open) == 0
}

// startsOnCloser reports whether the line after nl is the bare fence closing
// the block that is still open at nl, so the piece after the break would open
// with a block that shuts immediately.
func startsOnCloser(s string, nl int, open int) bool {
	w := openFence(s[:nl], open)
	return w != 0 && closerLine(s, nl+1, w) > 0
}

// closerLine reports the offset just past the line starting at off, if that
// line is the bare fence that closes a block of width open. It returns 0 when
// the line is anything else — content, an opener with an info string, or a
// fence too narrow to close this block.
func closerLine(s string, off, open int) int {
	if off >= len(s) {
		return 0
	}
	line, end := s[off:], len(s)
	if nl := strings.IndexByte(line, '\n'); nl >= 0 {
		line, end = line[:nl], off+nl+1
	}
	if w, bare := fenceLine(line); !bare || w < open {
		return 0
	}
	return end
}

// seal closes a piece whose fenced block the break left open, and reports the
// marker width the piece after it has to reopen that block with — 0 when
// nothing was left open.
func seal(piece string) (string, int) {
	open := openFence(piece, 0)
	if open == 0 {
		return piece, 0
	}
	return piece + "\n" + strings.Repeat("`", open), open
}

// widestRun reports the longest run of three or more backticks anywhere in s,
// or 0 if it has none — the widest marker a piece of s could ever have to gain.
//
// Deliberately blind to where the run sits on its line, unlike openFence. A
// hard cut taken with no line ending in the window can land immediately before
// a run that was mid-line, which puts it at the start of the next piece where
// it does open a block. Headroom has to cover that.
func widestRun(s string) int {
	widest := 0
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
			widest = max(widest, j-i)
		}
		i = j
	}
	return widest
}

// openFence reports the width of the fenced block still open at the end of s,
// given that a block of width open (0 for none) was already open entering it.
//
// A scanner rather than a delimiter count, because a fenced block is closed
// only by a fence at least as wide as the one that opened it: the ``` lines
// inside a ```` block — what a model writes when the answer is itself about
// markdown — are content. Counting delimiters and taking the parity reads the
// first of them as a close, and then every cut in the block is off by one
// block: the piece is not sealed, the next is not reopened, and the code lands
// in the thread as prose.
//
// Nor is a run of backticks a fence wherever it appears. One with text before
// it on the line is part of the prose — an inline span, or a sentence naming
// the syntax — and counting it flips the parity of everything after it.
func openFence(s string, open int) int {
	for len(s) > 0 {
		line := s
		if nl := strings.IndexByte(s, '\n'); nl >= 0 {
			line, s = s[:nl], s[nl+1:]
		} else {
			s = ""
		}
		w, bare := fenceLine(line)
		if w == 0 {
			continue
		}
		switch {
		case open == 0:
			open = w
		case bare && w >= open:
			open = 0
		}
	}
	return open
}

// fenceLine reports the width of the fence a line is, or 0 if it is not one,
// along with whether it is the marker alone. A fence is a run of three or more
// backticks starting the line; anything after it is the info string ("```go"),
// which may not contain a backtick and which only ever opens a block — a
// closing fence is bare.
//
// Leading and trailing whitespace is ignored rather than bounded at
// CommonMark's three columns: a fence indented under a list item is still a
// fence to every renderer switchboard posts to. That also takes care of the
// '\r' on a CRLF-terminated line, which is whitespace like any other here.
func fenceLine(line string) (width int, bare bool) {
	t := strings.TrimSpace(line)
	n := 0
	for n < len(t) && t[n] == '`' {
		n++
	}
	if n < len(fence) || strings.ContainsRune(t[n:], '`') {
		return 0, false
	}
	return n, n == len(t)
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
