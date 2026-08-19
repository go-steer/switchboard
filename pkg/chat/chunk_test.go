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
	"math/rand"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// checkChunks asserts everything ChunkText promises about one result at once,
// so a case only has to supply an input: every piece is within the limit and
// valid UTF-8, every piece's fences are balanced, a piece that continues an
// open block actually reopens it, and the pieces put back together are the
// original text.
//
// The reassembly is the load-bearing one. Balanced-fence assertions on their
// own are near-vacuous — an implementation that closes a block and then never
// reopens it also has even parity everywhere, and renders the middle of a code
// block as prose. Walking the pieces against the original catches that, along
// with a dropped, duplicated, or reordered piece.
func checkChunks(t *testing.T, original string, limit int, parts []string) {
	t.Helper()
	if len(parts) == 0 {
		t.Fatal("ChunkText returned nothing")
	}
	for i, p := range parts {
		if p == "" {
			t.Errorf("piece %d is empty, so it is a message with nothing in it", i)
		}
		if len(p) > limit {
			t.Errorf("piece %d is %d bytes, over the %d-byte limit", i, len(p), limit)
		}
		if !utf8.ValidString(p) {
			t.Errorf("piece %d is not valid UTF-8: %q", i, p)
		}
		if w := openFence(p, 0); w != 0 {
			t.Errorf("piece %d ends with a %d-backtick block still open, so it renders backticks literally:\n%s", i, w, p)
		}
	}

	steps, ok := decompose(original, parts)
	if !ok {
		t.Fatalf("the pieces do not rejoin into the text:\n text: %q\npieces: %q", original, parts)
	}
	for i := 1; i < len(steps); i++ {
		// A break must never fall through a CRLF, leaving the piece before it
		// ending on a bare '\r' and the one after opening on a bare '\n'. Asked
		// of the decomposed bodies rather than of the pieces, for two reasons:
		// the '\n' may be the line ending the break dropped rather than the next
		// piece's first byte, and a piece's closing marker sits on top of its
		// body's last byte, so a trailing '\r' does not reach the piece's end.
		// A lone '\r' is not a bisected anything, and is left alone.
		next := steps[i].eol + steps[i].body
		if strings.HasSuffix(steps[i-1].body, "\r") && strings.HasPrefix(next, "\n") {
			t.Errorf("the break before piece %d bisected a CRLF:\n %q\n %q",
				i, steps[i-1].body, next)
		}
		// Nor may it fall through a run of backticks: that leaves a stray marker
		// on each side, both halves still balance, and the block never opens.
		// Only a break that dropped nothing can do it — one taken on a line
		// ending has a newline between the two halves.
		if steps[i].eol != "" {
			continue
		}
		if strings.HasSuffix(steps[i-1].body, "`") && strings.HasPrefix(steps[i].body, "`") {
			t.Errorf("the break before piece %d bisected a fence marker:\n %q\n %q",
				i, steps[i-1].body, steps[i].body)
		}
	}
}

// step is one piece's place in the original: the line ending the break before
// it dropped, and the piece's own text with ChunkText's markers removed.
type step struct{ eol, body string }

// isFenceLine reports whether a line is a bare delimiter and nothing else,
// which is the shape of the closing marker seal appends. A line carrying an
// info string is an opener and can only be the text's own, never a marker.
func isFenceLine(line string) bool {
	w, bare := fenceLine(line)
	return w > 0 && bare
}

// decompose walks the pieces back onto the original text, accounting for
// exactly three edits: a reopening marker at the front of a piece continuing an
// open block, a closing marker at the back of one the break left open, and the
// single line ending each break consumed. Anything else — a dropped byte, a
// duplicated or reordered piece, a marker that should have been there and is
// not — has no walk and fails.
//
// It is a search because a piece ending "\n```" is genuinely ambiguous: that is
// either the marker or the text's own closing fence, and only the rest of the
// walk says which. The seen set keeps the backtracking linear.
//
// open is carried as a width, exactly as ChunkText carries it: a walk that
// assumed every marker was three backticks would accept a ```` block reopened
// with ```, which is the bug this all exists to catch.
func decompose(original string, parts []string) ([]step, bool) {
	type spot struct {
		i, off, open int
	}
	seen := map[spot]bool{}
	steps := make([]step, len(parts))

	var walk func(spot) bool
	walk = func(s spot) bool {
		if s.i == len(parts) {
			// Whether the last piece asked for a reopen says nothing: there is
			// no piece after it to carry one. It cannot be checked against the
			// whole text's own trailing state either, because a hard cut can
			// land in front of a run that was mid-line and start a piece with
			// it, opening a block the text as one string never had.
			return s.off == len(original)
		}
		if seen[s] {
			return false
		}
		seen[s] = true

		want := ""
		if s.open > 0 {
			want = strings.Repeat("`", s.open) + "\n"
		}
		if !strings.HasPrefix(parts[s.i], want) {
			return false
		}
		piece := parts[s.i][len(want):]
		for _, eol := range []string{"", "\n", "\r\n"} {
			off := s.off + len(eol)
			if off > len(original) || original[s.off:off] != eol {
				continue
			}
			for _, closed := range []bool{false, true} {
				body, marker := piece, 0
				if closed {
					// The marker is the piece's last line, and has to be the
					// bare run that closes whatever the body left open.
					nl := strings.LastIndexByte(piece, '\n')
					if nl < 0 || !isFenceLine(piece[nl+1:]) {
						continue
					}
					body, marker = piece[:nl], len(piece[nl+1:])
				}
				if openFence(want+body, 0) != marker {
					continue // ChunkText would not have made that choice
				}
				if !strings.HasPrefix(original[off:], body) {
					continue
				}
				if walk(spot{s.i + 1, off + len(body), marker}) {
					steps[s.i] = step{eol, body}
					return true
				}
			}
		}
		return false
	}
	if !walk(spot{}) {
		return nil, false
	}
	return steps, true
}

func TestChunkTextShortPassesThrough(t *testing.T) {
	got := ChunkText("hello world", 100)
	if len(got) != 1 || got[0] != "hello world" {
		t.Fatalf("ChunkText = %q, want the text whole in one piece", got)
	}
	// Exactly at the limit is still one piece — the limit is inclusive.
	if got := ChunkText("12345", 5); len(got) != 1 {
		t.Errorf("text exactly at the limit split into %d pieces", len(got))
	}
}

func TestChunkTextPrefersNewlines(t *testing.T) {
	const body = "line one\nline two\nline three"
	got := ChunkText(body, 12)
	if len(got) != 3 {
		t.Fatalf("ChunkText = %q, want three pieces", got)
	}
	if got[0] != "line one" {
		t.Errorf("first piece = %q, want the break taken on the newline", got[0])
	}
	checkChunks(t, body, 12, got)
}

func TestChunkTextNeverSplitsARune(t *testing.T) {
	// Three bytes each, against a limit that is not a multiple of three.
	const body = "世世世世世世世世世世"
	checkChunks(t, body, 7, ChunkText(body, 7))
}

func TestChunkTextAlwaysMakesProgress(t *testing.T) {
	// A limit below the first rune's width must still terminate, one rune at a
	// time, rather than loop forever on a zero-length cut.
	got := ChunkText("αβγ", 1)
	if len(got) != 3 {
		t.Fatalf("ChunkText = %q, want one piece per rune", got)
	}
	// A limit with nothing to split on at all is returned whole rather than
	// shredded — there is no sensible answer, and losing the text is worse.
	if got := ChunkText("αβγ", 0); len(got) != 1 || got[0] != "αβγ" {
		t.Errorf("ChunkText with limit 0 = %q, want the text whole", got)
	}
	if got := ChunkText("αβγ", -5); len(got) != 1 || got[0] != "αβγ" {
		t.Errorf("ChunkText with a negative limit = %q, want the text whole", got)
	}
}

// TestChunkTextBalancesFences is #31: every platform switchboard speaks to
// renders an odd ``` literally, so a split landing inside a fenced block put
// raw backticks on screen in both halves and lost the monospace in one.
func TestChunkTextBalancesFences(t *testing.T) {
	const limit = 200
	body := "Here is the config:\n\n```\n" +
		strings.Repeat("resource \"google_project_service\" \"svc\" {}\n", 12) +
		"```\n\nAnd the second one:\n\n```\n" +
		strings.Repeat("variable \"region\" { default = \"us-central1\" }\n", 12) +
		"```\n"

	got := ChunkText(body, limit)
	if len(got) < 4 {
		t.Fatalf("got %d pieces, want the fenced blocks to actually split", len(got))
	}
	checkChunks(t, body, limit, got)

	// The reopen is the half of the fix that parity alone cannot see: without
	// it every piece is still even, and the middle of the block renders as
	// prose. At least one continuation piece must carry the marker.
	reopens := 0
	for _, p := range got {
		if strings.HasPrefix(p, "```\n") && strings.HasSuffix(p, "\n```") {
			reopens++
		}
	}
	if reopens == 0 {
		t.Error("no piece both reopened and reclosed a block; the split cannot have landed inside one")
	}
}

// TestChunkTextUnfencedTextGetsNoMarkers checks the headroom and the balancing
// pass are both inert on text with no code in it, which is most replies.
func TestChunkTextUnfencedTextGetsNoMarkers(t *testing.T) {
	body := strings.Repeat("a plain sentence with no code in it at all.\n", 20)
	got := ChunkText(body, 100)
	if len(got) < 2 {
		t.Fatalf("got %d pieces, want a split", len(got))
	}
	for i, c := range got {
		if strings.Contains(c, "```") {
			t.Errorf("piece %d gained a fence marker it did not need: %q", i, c)
		}
	}
	checkChunks(t, body, 100, got)
}

// TestChunkTextAnUnclosedFenceIsStillClosed covers model output that is itself
// malformed. Balancing the pieces of an already-unbalanced text cannot restore
// what the model meant, but it can stop one bad block from leaking monospace
// into every message after it.
func TestChunkTextAnUnclosedFenceIsStillClosed(t *testing.T) {
	body := "```\n" + strings.Repeat("never closed\n", 30)
	for i, c := range ChunkText(body, 120) {
		if openFence(c, 0) != 0 {
			t.Errorf("piece %d is unbalanced: %q", i, c)
		}
	}
}

// TestChunkTextKeepsBlankLinesInCode is the difference between splitting a
// reply and editing it. The break consumes the one line ending it is taken on
// and nothing else; swallowing the run would delete blank lines from a YAML or
// Python block, and the reader has no way to know it happened.
func TestChunkTextKeepsBlankLinesInCode(t *testing.T) {
	const limit = 48
	body := "```\n" + strings.Repeat("aaaaaaaa\n", 4) + "\n" + strings.Repeat("bbbbbbbb\n", 4) + "```\n"
	got := ChunkText(body, limit)
	if len(got) < 2 {
		t.Fatalf("got %d pieces, want a split", len(got))
	}
	checkChunks(t, body, limit, got)

	// Said the other way round, since this is the whole point: strip the
	// markers and the pieces are the original's own lines, so putting one line
	// ending back between them is the original text. The old splitter trimmed
	// the whole run of newlines at the break and this join came up short.
	steps, ok := decompose(body, got)
	if !ok {
		t.Fatalf("the pieces do not rejoin into the text: %q", got)
	}
	lines := make([]string, len(steps))
	for i, s := range steps {
		lines[i] = s.body
	}
	if rejoined := strings.Join(lines, "\n"); rejoined != body {
		t.Errorf("rejoined:\n%q\nwant:\n%q", rejoined, body)
	}
}

// TestChunkTextNeverBisectsAFence covers the break the balancing pass cannot
// see. With no newline in the window the cut is at an arbitrary rune boundary,
// and a backtick is one byte — a cut through the middle of a ``` leaves a
// stray backtick on each side, both pieces count even, and the block never
// opens. #31's symptom by another route.
func TestChunkTextNeverBisectsAFence(t *testing.T) {
	// limit 39 leaves a 35-byte window, so the marker straddles the cut.
	const limit = 39
	body := strings.Repeat("x", 34) + "```" + strings.Repeat("y", 40) + "```" + strings.Repeat("z", 10)
	// checkChunks does the work: it asserts no break falls through a backtick
	// run, and that the pieces still rejoin into the original.
	checkChunks(t, body, limit, ChunkText(body, limit))
}

// TestChunkTextSurvivesAWallOfBackticks is the case the no-bisect rule has to
// give up on: a run wider than the whole window has no break point inside it
// and none before it, so it is bisected by necessity. What still has to hold is
// that ChunkText returns at all — backing the cut off to the start of the run
// when the run starts the piece would set the cut to zero and loop forever, and
// the fuzz alphabet deliberately keeps runs narrow enough never to reach here.
func TestChunkTextSurvivesAWallOfBackticks(t *testing.T) {
	done := make(chan []string, 1)
	body := strings.Repeat("`", 40) + strings.Repeat("x", 40)
	go func() { done <- ChunkText(body, 20) }()
	select {
	case got := <-done:
		if rejoined := strings.Join(got, ""); !strings.Contains(rejoined, strings.Repeat("`", 40)) {
			t.Errorf("the wall did not survive the split: %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ChunkText did not return: the cut stopped making progress")
	}
}

// TestChunkTextHoldsTheLimitWithAWideMarker is the size bound at the limit a
// platform actually imposes. The headroom reserved for the closing marker used
// to be three backticks and a newline, which is a byte short for the ```` block
// a model writes when the answer is about markdown: the piece came back at
// limit+1 and Chat rejected the post. Every other case here runs at a limit
// small enough that the arithmetic is easy to get right by accident.
func TestChunkTextHoldsTheLimitWithAWideMarker(t *testing.T) {
	for _, width := range []int{3, 4, 6} {
		f := strings.Repeat("`", width)
		// One unbroken line, longer than the limit, so every break inside the
		// block is a hard cut taken at the far edge of the window — the only
		// place where being a byte short of headroom actually shows.
		body := f + "json\n" + strings.Repeat("x", 9000) + "\n" + f + "\n"
		checkChunks(t, body, 4096, ChunkText(body, 4096))
	}
}

// TestChunkTextHoldsTheLimitAtTheEdgeOfTheBound sweeps the smallest limits the
// size bound still covers, where every term of it is the same order of
// magnitude: the reopening marker, the closing marker, and the rune that has to
// get through between them. No platform sets a limit anywhere near here, but
// this is where an off-by-one in the headroom shows as an over-limit piece
// rather than being swallowed by four kilobytes of slack.
//
// The alphabet is the fuzz's, plus a four-byte rune, since four bytes is the
// widest single thing a piece is obliged to carry. Cases below the bound are
// skipped rather than asserted on: there the markers genuinely do not fit, and
// an oversized piece is the documented behaviour.
func TestChunkTextHoldsTheLimitAtTheEdgeOfTheBound(t *testing.T) {
	pieces := []string{"a", " ", "\n", "\r\n", "\r", "`", "``", "```", "````", "code", "世", "😀"}
	rng := rand.New(rand.NewSource(99))
	for range 20000 {
		var b strings.Builder
		for range rng.Intn(14) + 1 {
			b.WriteString(pieces[rng.Intn(len(pieces))])
		}
		body, limit := b.String(), rng.Intn(30)+3
		if len(body) <= limit || limit < 2*widestRun(body)+6 {
			continue
		}
		for i, p := range ChunkText(body, limit) {
			if len(p) > limit {
				t.Fatalf("piece %d is %d bytes against a limit of %d: %q\nbody %q",
					i, len(p), limit, p, body)
			}
		}
	}
}

// TestOpenFenceTracksWidth pins the scanner the whole chunker turns on, with
// the cases the obvious wrong implementations get wrong. Pinned directly
// because openFence is what the test helpers decide with too — a shared mistake
// there would be invisible everywhere else.
func TestOpenFenceTracksWidth(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want int
	}{
		{"nothing", "", 0},
		{"two backticks are not a fence", "``\ncode\n", 0},
		{"an opener with no closer", "```\ncode\n", 3},
		{"opened and closed", "```\ncode\n```\n", 0},
		{"an info string opens", "```go\ncode\n```\n", 0},
		{"a wide opener needs a wide closer", "````\ncode\n````\n", 0},
		{
			// The bug. A ``` inside a ```` block is content: closing on it
			// leaves the rest of the answer reopened as code.
			name: "a narrow fence inside a wide block is content",
			in:   "````markdown\n```go\ncode\n```\n",
			want: 4,
		},
		{"and the wide one still closes it", "````markdown\n```go\ncode\n```\n````\n", 0},
		{"a wider fence closes a narrow block", "```\ncode\n`````\n", 0},
		{"an inline span is not a fence", "use ``` to open a block\n", 0},
		{"nor is one with a span on the line", "```a```\n", 0},
		{"an indented fence is still a fence", "  ```\ncode\n", 3},
		{"a closer may not carry an info string", "```\ncode\n```go\n", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := openFence(tc.in, 0); got != tc.want {
				t.Errorf("openFence(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}

	// The width a piece is entered with carries through.
	if got := openFence("code\n", 4); got != 4 {
		t.Errorf("openFence of a plain line inside a ```` block = %d, want 4", got)
	}
	if got := openFence("```\ncode\n", 4); got != 4 {
		t.Errorf("a narrow fence closed a ```` block entered from the previous piece: got %d", got)
	}
	if got := openFence("````\n", 4); got != 0 {
		t.Errorf("a matching fence did not close the block entered from the previous piece: got %d", got)
	}

	if got := widestRun("```\ncode\n```\n`````\n"); got != 5 {
		t.Errorf("widestRun = %d, want 5 — the headroom has to cover the widest marker", got)
	}
}

// TestFenceLine pins the line classifier openFence and every seam check are
// built on. Bare-ness is the half the callers turn on and the half ChunkText's
// output cannot show: an info string only ever opens a block, so a fence
// carrying one can never be the marker that closes one.
func TestFenceLine(t *testing.T) {
	for _, tc := range []struct {
		in    string
		width int
		bare  bool
	}{
		{"", 0, false},
		{"code", 0, false},
		{"``", 0, false},
		{"```", 3, true},
		{"````", 4, true},
		{"```go", 3, false},
		{"  ```", 3, true},         // indented under a list item
		{"```   ", 3, true},        // trailing spaces do not make it an info string
		{"```go  ", 3, false},      // nor do they hide one
		{"```\r", 3, true},         // a CRLF line ending is whitespace too
		{"```go\r", 3, false},      //
		{"```a```", 0, false},      // a span on the line, not a fence
		{"use ``` here", 0, false}, /* nor is one with text in front */
	} {
		t.Run(tc.in, func(t *testing.T) {
			w, bare := fenceLine(tc.in)
			if w != tc.width || bare != tc.bare {
				t.Errorf("fenceLine(%q) = (%d, %v), want (%d, %v)", tc.in, w, bare, tc.width, tc.bare)
			}
		})
	}
}

// TestBackOffEmptySeam covers the decisions the sweep in
// TestChunkTextNoEmptyCodeBlockAtTheSeam reaches only for the limits that
// happen to land on them: whether a break moves at all, and the two ways moving
// it would be wrong. The last case is the one that matters most — backing off
// to a break at index 0 would leave nothing in front of it, and splitAtEOL
// reads the byte before the break.
func TestBackOffEmptySeam(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    string
		nl   int
		open int
		want int
	}{
		{
			name: "plain text stays where it is",
			s:    "one\ntwo\nthree\n",
			nl:   7,
			want: 7,
		},
		{
			// The seam right after an opener: "a" is sent alone and the opener
			// travels with its code.
			name: "a break after an opener moves back a line",
			s:    "a\n```\ncode\nmore\n```\n",
			nl:   5,
			want: 1,
		},
		{
			// A block was already open entering s, so this fence line is inside
			// it — an info string means it cannot even be the close — and there
			// is no empty block to avoid. Reading the incoming width as 0 makes
			// it an opener and moves a break that was fine.
			name: "a fence inside the incoming block is not an opener",
			s:    "aa\ncode\n```go\nmore\n",
			nl:   13,
			open: 3,
			want: 13,
		},
		{
			name: "and with no incoming block the same fence does open one",
			s:    "aa\ncode\n```go\nmore\n",
			nl:   13,
			want: 7,
		},
		{
			// A ``` inside a ```` block is content, not an opener. Treating
			// every fence line as one moves a break that had nothing wrong with
			// it, splitting the block a line earlier than asked.
			name: "a narrow fence inside a wide block is not an opener",
			s:    "````md\ncode\n```\nmore\n````\n",
			nl:   15,
			want: 15,
		},
		{
			// A fence closing a block opened earlier in s.
			name: "a break after a closer stays where it is",
			s:    "```\ncode\n```\ntail\n",
			nl:   12,
			want: 12,
		},
		{
			// The break is before a closer, so it would strand it — but the
			// block is one line long and moving back only lands on the opener.
			name: "a one-line block has nowhere better to break",
			s:    "a\n```\ncode\n```\n",
			nl:   10,
			want: 10,
		},
		{
			// Two lines of code, so backing off keeps one of them in front of
			// the closer and the continuation is not an empty block.
			name: "a break before a closer moves back a line",
			s:    "a\n```\nc1\nc2\n```\n",
			nl:   11,
			want: 8,
		},
		{
			// Backing off would put the break at index 0, and there is no line
			// in front of it to keep. It has to stand.
			name: "a seam on the first line does not move off the front",
			s:    "\n```\n```\n",
			nl:   4,
			want: 4,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := backOffEmptySeam(tc.s, tc.nl, tc.open); got != tc.want {
				t.Errorf("backOffEmptySeam(%q, %d, %d) = %d, want %d",
					tc.s, tc.nl, tc.open, got, tc.want)
			}
		})
	}
}

// TestCloserLine pins what counts as the fence that closes a block, which both
// seam checks and the pull-the-closer-forward step in ChunkText turn on. A
// closer may be wider than the opener and may not carry an info string, and
// getting either wrong moves breaks that were fine.
func TestCloserLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    string
		off  int
		open int
		want int
	}{
		{"the block's own closer", "code\n```\ntail\n", 5, 3, 9},
		{"a wider fence closes it too", "code\n`````\ntail\n", 5, 3, 11},
		{"a narrower one does not", "code\n```\ntail\n", 5, 4, 0},
		{"nor does one with an info string", "code\n```go\ntail\n", 5, 3, 0},
		{"nor does a span on the line", "code\n```x```\ntail\n", 5, 3, 0},
		{"nor does ordinary content", "code\nmore\n", 5, 3, 0},
		{"a closer ending the text needs no newline", "code\n```", 5, 3, 8},
		{"there is no line past the end", "code\n", 5, 3, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := closerLine(tc.s, tc.off, tc.open); got != tc.want {
				t.Errorf("closerLine(%q, %d, %d) = %d, want %d",
					tc.s, tc.off, tc.open, got, tc.want)
			}
		})
	}
}

// TestChooseCutHardBreaks covers the hard-cut guards at window widths ChunkText
// only reaches for limits smaller than any platform's. They are asked of
// chooseCut directly because the interesting part is which bytes it drops,
// which the pieces alone do not say.
func TestChooseCutHardBreaks(t *testing.T) {
	for _, tc := range []struct {
		name      string
		s         string
		window    int
		cut, drop int
	}{
		{
			name: "a cut inside a run backs off to the front of it",
			s:    "xx```yy", window: 4, cut: 2,
		},
		{
			// The run starts s and is wider than the window, so backing off
			// would cut nothing at all and the caller would loop forever. The
			// run is bisected instead — see TestChunkTextSurvivesAWallOfBackticks.
			name: "unless the run starts the piece",
			s:    "`````yy", window: 3, cut: 3,
		},
		{
			name: "a cut between the bytes of a CRLF takes the line ending",
			s:    "abcd\r\nefgh", window: 5, cut: 4, drop: 2,
		},
		{
			// Dropping it would leave an empty piece, so both bytes are kept.
			name: "a CRLF at the very front is kept whole",
			s:    "\r\nxxxx", window: 1, cut: 2,
		},
		{
			// Dropping it would lose the text's last line ending, and nothing
			// follows to carry it.
			name: "a CRLF at the very end is kept whole",
			s:    "xxxx\r\n", window: 5, cut: 4,
		},
		{
			name: "a window inside the first rune emits the whole rune",
			s:    "世界", window: 1, cut: 3,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cut, drop := chooseCut(tc.s, tc.window, 0)
			if cut != tc.cut || drop != tc.drop {
				t.Errorf("chooseCut(%q, %d) = (%d, %d), want (%d, %d)",
					tc.s, tc.window, cut, drop, tc.cut, tc.drop)
			}
		})
	}
}

// TestChunkTextSplitsANestedBlock is the case flat parity gets wrong: an answer
// *about* markdown, where a ```` block contains an ordinary ``` one. Counting
// delimiters reads the inner opener as closing the outer block, so a cut inside
// it seals nothing and reopens nothing, and the rest of the code lands in the
// thread as prose with stray backticks around it.
func TestChunkTextSplitsANestedBlock(t *testing.T) {
	const limit = 220
	inner := "```go\n" + strings.Repeat("fmt.Println(\"hello world\")\n", 12) + "```\n"
	body := "Here is how you write a fenced block:\n\n" +
		"````markdown\n" + inner + "````\n\n" +
		"That trailing paragraph is plain prose, not code.\n"

	got := ChunkText(body, limit)
	if len(got) < 3 {
		t.Fatalf("got %d pieces, want the nested block to actually split", len(got))
	}
	checkChunks(t, body, limit, got)

	// checkChunks proves each piece is balanced and that they rejoin. This is
	// the half it cannot see: the continuation has to reopen with the *outer*
	// block's width, or the inner ``` closes it and the rest renders as prose.
	reopens := 0
	for _, p := range got {
		if strings.HasPrefix(p, "````\n") {
			reopens++
		}
		if strings.HasPrefix(p, "```\n") {
			t.Errorf("a piece reopened a ```` block with a three-backtick marker:\n%s", p)
		}
	}
	if reopens == 0 {
		t.Error("no piece reopened the outer block; the split cannot have landed inside it")
	}
}

// TestChunkTextNoEmptyCodeBlockAtTheSeam covers the two breaks either side of a
// block that put an empty code block on screen: one landing right after the
// opening fence, so the piece ends "```\n```" and the content opens the next
// piece anyway, and one landing right before the closing fence, so the piece
// after it *starts* "```\n```". The break moves back a line either way.
//
// Swept across limits rather than pinned to one, because which of the two a
// break lands on is pure arithmetic between the limit and where the block sits
// — a single limit tests whichever case it happens to hit.
//
// The four dimensions are each a way an earlier version got it wrong:
//
//   - Both spellings of the opener, because the one a model actually writes
//     carries an info string, and the check that recognises an opener used to
//     want a bare delimiter — so the common case was the one that slipped
//     through.
//   - A tail after the block, because with the closing fence last in the text
//     there is nothing for a break in front of it to strand.
//   - The block at the very top of the answer as well as further down. Backing
//     a break off the closing fence used to be allowed to land it on the
//     opening one, because "there is no earlier line to move to" and "this is
//     not an empty opener" were the same answer — so a block starting the
//     answer got the empty block moved onto it rather than removed.
//   - A block whose first line is longer than the window, so the opener's own
//     newline is the only one in reach and there is nothing to back off to. The
//     break has to stop preferring a line ending and cut inside the long line.
func TestChunkTextNoEmptyCodeBlockAtTheSeam(t *testing.T) {
	for _, opener := range []string{"```", "```go"} {
		for _, head := range []string{"", strings.Repeat("a\n", 30)} {
			for _, code := range []string{"code\nmore code\n", strings.Repeat("x", 200) + "\n"} {
				for _, tail := range []string{"", "tail\n", strings.Repeat("tail line\n", 4)} {
					body := head + opener + "\n" + code + "```\n" + tail
					for limit := 20; limit < len(body); limit++ {
						got := ChunkText(body, limit)
						checkChunks(t, body, limit, got)
						for i, p := range got {
							if strings.Contains(p, opener+"\n```") || strings.Contains(p, "```\n```") {
								t.Errorf("opener %q, head %q, code %q, tail %q, limit %d, piece %d posts an empty code block: %q",
									opener, head, code, tail, limit, i, p)
							}
						}
					}
				}
			}
		}
	}
}

// TestChunkTextKeepsCRLFTogether: a break taken on a CRLF must take both bytes,
// or the piece before it ends on a bare '\r' and the one after opens on a bare
// '\n'. Chat does not normalise line endings on the way in, so model output
// with them reaches the chunker.
//
// checkChunks makes the assertion, on every case in the file. These three are
// the shapes that got it wrong: a break found by scanning for a line ending, a
// hard cut with no line ending in the window landing between the two bytes, and
// a CRLF with nothing in front of it.
func TestChunkTextKeepsCRLFTogether(t *testing.T) {
	const limit = 40
	body := strings.Repeat("hello there\r\n", 12)
	checkChunks(t, body, limit, ChunkText(body, limit))

	// No line ending inside any window, so every break is a hard cut, and the
	// odd run length walks one across the CRLF.
	unbroken := strings.Repeat("hello there is no newline in here\r\n", 4)
	checkChunks(t, unbroken, 17, ChunkText(unbroken, 17))

	// A CRLF the text opens on. It is the only line ending in the first window,
	// so it is the break, and dropping it would leave nothing in front of it —
	// the case the guard against an empty piece used to answer by keeping the
	// '\r' and dropping the '\n'.
	leading := "\r\n" + strings.Repeat("x", 60)
	checkChunks(t, leading, 20, ChunkText(leading, 20))

	// A lone '\r' is an old-Mac line ending, not half of a CRLF. It is content:
	// a piece may end on one, and nothing may be dropped around it.
	lone := strings.Repeat("a", 20) + "\r" + strings.Repeat("b", 20)
	checkChunks(t, lone, 21, ChunkText(lone, 21))
}

// TestChunkTextHoldsUnderFuzz is the property check the individual cases are
// examples of. The alphabet is deliberately hostile: backticks in runs of one
// to four, all three line endings — including a lone '\r', which is not half of
// anything and must not be treated as such — and multi-byte runes, at limits
// small enough that every piece is a boundary case.
func TestChunkTextHoldsUnderFuzz(t *testing.T) {
	// Backtick tokens never land next to each other: a run wider than the
	// window has no break point inside or outside it and is bisected by
	// necessity, which TestChunkTextSurvivesAWallOfBackticks covers on its own.
	// Real fences are three or four.
	pieces := []string{"a", " ", "\n", "\r\n", "`", "``", "```", "````", "世", "αβ", "code", "\n\n", "\r"}
	const firstTick, lastTick = 4, 7
	rng := rand.New(rand.NewSource(1))
	for i := range 4000 {
		var b strings.Builder
		last := -1
		for range rng.Intn(40) + 1 {
			n := rng.Intn(len(pieces))
			if n >= firstTick && n <= lastTick && last >= firstTick && last <= lastTick {
				continue
			}
			b.WriteString(pieces[n])
			last = n
		}
		body, limit := b.String(), rng.Intn(48)+16
		if len(body) <= limit {
			continue
		}
		t.Run("", func(t *testing.T) {
			if t.Failed() {
				t.Logf("case %d: limit %d, body %q", i, limit, body)
			}
			checkChunks(t, body, limit, ChunkText(body, limit))
		})
	}
}

func TestRuneBoundary(t *testing.T) {
	const s = "a世b" // 1 + 3 + 1 bytes
	for _, tc := range []struct{ n, want int }{
		{-1, 0}, {0, 0}, {1, 1}, {2, 1}, {3, 1}, {4, 4}, {5, 5}, {99, 5},
	} {
		if got := RuneBoundary(s, tc.n); got != tc.want {
			t.Errorf("RuneBoundary(%q, %d) = %d, want %d", s, tc.n, got, tc.want)
		}
	}
}
