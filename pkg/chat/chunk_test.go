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
		if len(p) > limit {
			t.Errorf("piece %d is %d bytes, over the %d-byte limit", i, len(p), limit)
		}
		if !utf8.ValidString(p) {
			t.Errorf("piece %d is not valid UTF-8: %q", i, p)
		}
		if n := countFences(p); n%2 != 0 {
			t.Errorf("piece %d has %d fences (odd), so it renders backticks literally:\n%s", i, n, p)
		}
	}

	steps, ok := decompose(original, parts)
	if !ok {
		t.Fatalf("the pieces do not rejoin into the text:\n text: %q\npieces: %q", original, parts)
	}
	// A break must never fall through a run of backticks: that leaves a stray
	// marker on each side, both pieces still count even, and the block never
	// opens. Only a break that dropped nothing can do it — one taken on a line
	// ending has a newline between the two halves.
	for i := 1; i < len(steps); i++ {
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
func decompose(original string, parts []string) ([]step, bool) {
	type spot struct {
		i, off int
		open   bool
	}
	seen := map[spot]bool{}
	steps := make([]step, len(parts))

	var walk func(spot) bool
	walk = func(s spot) bool {
		if s.i == len(parts) {
			// A block still open at the end is only honest if the text itself
			// never closed it.
			return s.off == len(original) && (!s.open || countFences(original)%2 == 1)
		}
		if seen[s] {
			return false
		}
		seen[s] = true

		want := ""
		if s.open {
			want = "```\n"
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
				body := piece
				if closed {
					var cut bool
					if body, cut = strings.CutSuffix(piece, "\n```"); !cut {
						continue
					}
				}
				if (countFences(want+body)%2 == 1) != closed {
					continue // ChunkText would not have made that choice
				}
				if !strings.HasPrefix(original[off:], body) {
					continue
				}
				if walk(spot{s.i + 1, off + len(body), closed}) {
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
		if countFences(c)%2 != 0 {
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

// TestChunkTextCountsFencesByRun pins the parity of a four-backtick fence,
// which a model reaches for when the answer is itself about markdown.
// strings.Count reads "````" as one ``` plus a loose backtick and gets the
// parity backwards, so a split inside the outer block looks balanced.
// Pinned directly, and with cases the two obvious wrong implementations get
// wrong, because countFences is what the test helper counts with too — a
// shared mistake there would be invisible everywhere else.
func TestChunkTextCountsFencesByRun(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"", 0},
		{"`", 0},
		{"``", 0}, // two backticks are not a fence
		{"```", 1},
		{"````", 1},   // strings.Count says 1 too, but for the wrong reason
		{"``````", 1}, // strings.Count says 2; one run is one delimiter
		{"```a```", 2},
		{"`a`b`c`", 0}, // inline code, not a fence in sight
	} {
		if got := countFences(tc.in); got != tc.want {
			t.Errorf("countFences(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}

	const limit = 40
	body := "````\n" + strings.Repeat("md\n", 20) + "````\n"
	checkChunks(t, body, limit, ChunkText(body, limit))
}

// TestChunkTextNoEmptyCodeBlockAtTheSeam covers the break that lands right
// after an opening fence: closing there posts "```\n```", an empty code block,
// and the content opens the next piece anyway. The break moves back a line so
// the opener travels with its code.
// Swept across limits rather than pinned to one, because whether the break
// lands on the opening fence's own line is pure arithmetic between the limit
// and where the block starts — a single limit tests whichever case it happens
// to hit.
func TestChunkTextNoEmptyCodeBlockAtTheSeam(t *testing.T) {
	body := strings.Repeat("a\n", 30) + "```\ncode\nmore code\n```\n"
	for limit := 20; limit < len(body); limit++ {
		got := ChunkText(body, limit)
		checkChunks(t, body, limit, got)
		for i, p := range got {
			if strings.Contains(p, "```\n```") {
				t.Errorf("limit %d, piece %d posts an empty code block: %q", limit, i, p)
			}
		}
	}
}

// TestChunkTextKeepsCRLFTogether: a break taken on a CRLF must take both bytes,
// or every piece ends on a dangling carriage return. Chat does not normalise
// line endings on the way in, so model output with them reaches the chunker.
func TestChunkTextKeepsCRLFTogether(t *testing.T) {
	const limit = 40
	body := strings.Repeat("hello there\r\n", 12)
	got := ChunkText(body, limit)
	checkChunks(t, body, limit, got)
	for i, p := range got {
		if strings.HasSuffix(p, "\r") {
			t.Errorf("piece %d ends on a dangling carriage return: %q", i, p)
		}
	}
}

// TestChunkTextHoldsUnderFuzz is the property check the individual cases are
// examples of. The alphabet is deliberately hostile: backticks in runs of one
// to four, both line endings, and multi-byte runes, at limits small enough that
// every piece is a boundary case.
func TestChunkTextHoldsUnderFuzz(t *testing.T) {
	// Backtick tokens never land next to each other: a run wider than the
	// window has no break point inside or outside it and is bisected by
	// necessity (see TestChunkTextSurvivesAWallOfBackticks). Real fences are
	// three or four.
	pieces := []string{"a", " ", "\n", "\r\n", "`", "``", "```", "````", "世", "αβ", "code", "\n\n"}
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
