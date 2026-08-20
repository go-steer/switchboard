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

package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/go-steer/switchboard/pkg/chat"
	"github.com/go-steer/switchboard/pkg/daemon"
)

// call and result are shorthand for the fixtures below, which are mostly
// three-field structs with one field that matters.
func call(name, arg string) daemon.ToolCall { return daemon.ToolCall{Name: name, Arg: arg} }

func okRes(name string) *daemon.ToolResult { return &daemon.ToolResult{Name: name} }

func failRes(name, detail string) *daemon.ToolResult {
	return &daemon.ToolResult{Name: name, Failed: true, Detail: detail}
}

// TestActivityText pins the rendered shapes the doc comment promises, in both
// disclosure modes. These are what a reader actually sees, so they are compared
// whole rather than by substring.
func TestActivityText(t *testing.T) {
	cases := []struct {
		name   string
		calls  []daemon.ToolCall
		res    []*daemon.ToolResult
		detail bool
		want   string
	}{
		{
			name:   "one call in flight",
			calls:  []daemon.ToolCall{call("bash", "kubectl get pods -A")},
			detail: true,
			want:   "🔧 Running `bash` — kubectl get pods -A",
		},
		{
			name:   "one call finished",
			calls:  []daemon.ToolCall{call("bash", "kubectl get pods -A")},
			res:    []*daemon.ToolResult{okRes("bash")},
			detail: true,
			want:   "✅ Ran `bash` — kubectl get pods -A",
		},
		{
			name:   "one call failed, with the reason",
			calls:  []daemon.ToolCall{call("bash", "kubectl get ns --context nope")},
			res:    []*daemon.ToolResult{failRes("bash", "exit 2")},
			detail: true,
			want:   "❌ Ran `bash` (exit 2) — kubectl get ns --context nope",
		},
		{
			name:   "a call with no summarisable argument still renders",
			calls:  []daemon.ToolCall{call("think", "")},
			detail: true,
			want:   "🔧 Running `think`",
		},
		{
			// Three shells in one frame: the whole point of the argument summary
			// is that these stay three legible lines rather than one count.
			name: "a parallel frame, partly resolved",
			calls: []daemon.ToolCall{
				call("bash", "kubectl get pods -A"),
				call("bash", "kubectl get ns --context nope"),
				call("bash", "sleep 30"),
			},
			res:    []*daemon.ToolResult{okRes("bash"), failRes("bash", "exit 2"), nil},
			detail: true,
			want: "🔧 Running 3 tools\n" +
				"• ✅ `bash` — kubectl get pods -A\n" +
				"• ❌ `bash` (exit 2) — kubectl get ns --context nope\n" +
				"• 🔧 `bash` — sleep 30",
		},
		{
			name: "a frame whose calls all landed says so in the header",
			calls: []daemon.ToolCall{
				call("read", "/etc/hosts"),
				call("write", "/tmp/out"),
			},
			res:    []*daemon.ToolResult{okRes("read"), okRes("write")},
			detail: true,
			want: "✅ Ran 2 tools\n" +
				"• ✅ `read` — /etc/hosts\n" +
				"• ✅ `write` — /tmp/out",
		},
		{
			name: "the header counts the failures",
			calls: []daemon.ToolCall{
				call("read", "/etc/hosts"),
				call("read", "/nope"),
			},
			res:    []*daemon.ToolResult{okRes("read"), failRes("read", "")},
			detail: true,
			want: "❌ Ran 2 tools (1 failed)\n" +
				"• ✅ `read` — /etc/hosts\n" +
				"• ❌ `read` — /nope",
		},
		{
			// Same name, same argument, same verdict: indistinguishable to a
			// reader, so collapsed. "`bash`, `bash`, `bash`" reads as a bug.
			name: "identical adjacent calls collapse to a count",
			calls: []daemon.ToolCall{
				call("bash", "make test"),
				call("bash", "make test"),
				call("bash", "make test"),
			},
			detail: true,
			want:   "🔧 Running `bash` ×3 — make test",
		},
		{
			name: "same tool, different arguments, stays separate lines",
			calls: []daemon.ToolCall{
				call("bash", "make test"),
				call("bash", "make lint"),
			},
			detail: true,
			want: "🔧 Running 2 tools\n" +
				"• 🔧 `bash` — make test\n" +
				"• 🔧 `bash` — make lint",
		},
		{
			// A resolved and an unresolved call of the same shape are different
			// states, so they must not fold into one line.
			name: "a differing verdict breaks a run",
			calls: []daemon.ToolCall{
				call("bash", "make test"),
				call("bash", "make test"),
			},
			res:    []*daemon.ToolResult{okRes("bash"), nil},
			detail: true,
			want: "🔧 Running 2 tools\n" +
				"• ✅ `bash` — make test\n" +
				"• 🔧 `bash` — make test",
		},
		{
			name:   "terse mode names the tool and nothing else",
			calls:  []daemon.ToolCall{call("bash", "kubectl get pods -A")},
			detail: false,
			want:   "🔧 Running `bash`",
		},
		{
			name: "terse mode counts a run and joins the rest",
			calls: []daemon.ToolCall{
				call("bash", "make test"),
				call("bash", "make lint"),
				call("read", "/etc/hosts"),
			},
			detail: false,
			want:   "🔧 Running `bash` ×2, `read`",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := activityText(tc.calls, tc.res, tc.detail); got != tc.want {
				t.Fatalf("activityText =\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}

// TestTerseModeNeverShowsAnArgument pins that the terse render is a function of
// tool names alone. That is structural today — the branch reads only Name — so
// the substring sweep cannot fail without someone teaching it to read Arg,
// which is exactly the regression worth catching: a reader who chose indicator
// or status did not ask to see what the agent is running things with, and a
// deployment that cannot accept argument disclosure at all is told to use those
// modes.
func TestTerseModeNeverShowsAnArgument(t *testing.T) {
	calls := []daemon.ToolCall{
		call("bash", "curl -H 'Authorization: Bearer hunter2' https://internal"),
		call("read", "/home/alice/.ssh/id_rsa"),
	}
	got := activityText(calls, nil, false)
	for _, secret := range []string{"hunter2", "Authorization", "id_rsa", "/home/alice", "curl", "—"} {
		if strings.Contains(got, secret) {
			t.Fatalf("terse activityText leaked %q: %s", secret, got)
		}
	}
	if want := "🔧 Running `bash`, `read`"; got != want {
		t.Fatalf("activityText = %q, want %q", got, want)
	}
}

// TestStripArgsIsUnconditional pins the belt to that brace: even if a future
// edit teaches the terse renderer to read Arg, there is nothing there to read.
func TestStripArgsIsUnconditional(t *testing.T) {
	for _, c := range stripArgs([]daemon.ToolCall{{ID: "c1", Name: "bash", Arg: "secret"}}) {
		if c.Arg != "" {
			t.Fatalf("stripArgs kept an argument: %+v", c)
		}
		if c.ID != "c1" || c.Name != "bash" {
			t.Fatalf("stripArgs dropped identity: %+v", c)
		}
	}
}

// fileResult files a single result and reports the edit it produced, which is
// what the relay does for a frame carrying one. Results arrive in frames, so
// applyToolResults is the real entry point; this keeps the one-at-a-time tests
// reading as one at a time.
func fileResult(e *sessionEntry, r daemon.ToolResult) (chat.MessageRef, string, bool) {
	edits := e.applyToolResults([]daemon.ToolResult{r})
	if len(edits) == 0 {
		return chat.MessageRef{}, "", false
	}
	return edits[0].ref, edits[0].text, true
}

// TestResolveToolMatchesOnCallID is the ordinary path: the daemon gives every
// call an id and echoes it on the result, so a result finds its own line even
// when several of the same tool are in flight.
func TestResolveToolMatchesOnCallID(t *testing.T) {
	e := &sessionEntry{}
	calls := []daemon.ToolCall{
		{ID: "a", Name: "bash", Arg: "make test"},
		{ID: "b", Name: "bash", Arg: "make lint"},
	}
	e.noteToolCalls(chat.MessageRef{ID: "ts1"}, calls, 0, true)

	ref, text, ok := fileResult(e, daemon.ToolResult{ID: "b", Name: "bash", Failed: true, Detail: "exit 1"})
	if !ok {
		t.Fatal("a result carrying a known call id found no notice")
	}
	if ref.ID != "ts1" {
		t.Fatalf("edited %q, want the notice that announced the call", ref.ID)
	}
	want := "🔧 Running 2 tools\n" +
		"• 🔧 `bash` — make test\n" +
		"• ❌ `bash` (exit 1) — make lint"
	if text != want {
		t.Fatalf("re-render =\n%s\nwant\n%s", text, want)
	}
}

// TestResolveToolIgnoresADuplicateResult: the SSE resume replays from the last
// seq seen, so a result can arrive twice for one call. The second must not
// re-edit the notice — the content is identical and the edit is an API call.
func TestResolveToolIgnoresADuplicateResult(t *testing.T) {
	e := &sessionEntry{}
	e.noteToolCalls(chat.MessageRef{ID: "ts1"}, []daemon.ToolCall{{ID: "a", Name: "bash", Arg: "make test"}}, 0, true)

	if _, _, ok := fileResult(e, daemon.ToolResult{ID: "a", Name: "bash"}); !ok {
		t.Fatal("the first result found no notice")
	}
	if _, _, ok := fileResult(e, daemon.ToolResult{ID: "a", Name: "bash", Failed: true, Detail: "exit 9"}); ok {
		t.Fatal("a repeat of an answered result was filed again")
	}
}

// TestResolveToolFallsBackToTheOldestUnansweredCall covers a daemon that sends
// no call ids. The fallback matches in submission order, because within a frame
// the calls are in the order the model asked for them and a run of same-named
// calls answered in that order is the ordinary case. Matching newest-first
// inverted every pair: three `bash` calls answered in order each got ticked off
// against the wrong line, so every argument shown was another call's.
func TestResolveToolFallsBackToTheOldestUnansweredCall(t *testing.T) {
	e := &sessionEntry{}
	e.noteToolCalls(chat.MessageRef{ID: "ts1"}, []daemon.ToolCall{call("bash", "one"), call("bash", "two")}, 0, true)

	if _, text, ok := fileResult(e, daemon.ToolResult{Name: "bash"}); !ok {
		t.Fatal("the first result found no notice")
	} else if !strings.Contains(text, "• ✅ `bash` — one") {
		t.Fatalf("the first result did not land on the oldest call:\n%s", text)
	}
	// The oldest is answered now, so the second result takes the next line
	// rather than overwriting the one just filled.
	if _, text, ok := fileResult(e, daemon.ToolResult{Name: "bash"}); !ok {
		t.Fatal("the second result found no notice")
	} else if !strings.Contains(text, "• ✅ `bash` — two") {
		t.Fatalf("the second result did not fall through to the next call:\n%s", text)
	}
	// Both are answered; a third has nowhere to go and must not overwrite.
	if _, _, ok := fileResult(e, daemon.ToolResult{Name: "bash"}); ok {
		t.Fatal("a third result was filed against a fully answered notice")
	}
}

// TestResolveToolIgnoresWhatItIsNotWaitingFor: a result from before switchboard
// connected, or for a tool no notice announced, is dropped in silence. That is
// the normal state of every mode but stream.
func TestResolveToolIgnoresWhatItIsNotWaitingFor(t *testing.T) {
	e := &sessionEntry{}
	if _, _, ok := fileResult(e, daemon.ToolResult{ID: "x", Name: "bash"}); ok {
		t.Fatal("a result matched against a session with no notices")
	}
	e.noteToolCalls(chat.MessageRef{ID: "ts1"}, []daemon.ToolCall{{ID: "a", Name: "bash"}}, 0, true)
	if _, _, ok := fileResult(e, daemon.ToolResult{Name: "read"}); ok {
		t.Fatal("a result matched a notice that announced a different tool")
	}
	if _, _, ok := fileResult(e, daemon.ToolResult{ID: "zzz", Name: "bash"}); ok {
		t.Fatal("a result carrying an unknown id fell back on the name")
	}
}

// TestResolveToolSearchesOlderNotices: a long turn posts one notice per frame,
// and results do not arrive in frame order. A slow call from three frames back
// must still find its line.
func TestResolveToolSearchesOlderNotices(t *testing.T) {
	e := &sessionEntry{}
	e.noteToolCalls(chat.MessageRef{ID: "ts1"}, []daemon.ToolCall{{ID: "slow", Name: "bash", Arg: "sleep 30"}}, 0, true)
	e.noteToolCalls(chat.MessageRef{ID: "ts2"}, []daemon.ToolCall{{ID: "fast", Name: "bash", Arg: "echo hi"}}, 0, true)

	if ref, _, ok := fileResult(e, daemon.ToolResult{ID: "fast", Name: "bash"}); !ok || ref.ID != "ts2" {
		t.Fatalf("newest result edited %q (ok=%v), want ts2", ref.ID, ok)
	}
	if ref, _, ok := fileResult(e, daemon.ToolResult{ID: "slow", Name: "bash"}); !ok || ref.ID != "ts1" {
		t.Fatalf("older result edited %q (ok=%v), want ts1", ref.ID, ok)
	}
}

// TestNoticeMemoryIsBounded: a turn that runs for an hour must not grow the
// session without limit. The cost of forgetting is a notice that keeps its 🔧,
// which is why the bound is deliberately small.
func TestNoticeMemoryIsBounded(t *testing.T) {
	const extra = 10
	e := &sessionEntry{}
	for i := range noticeMemory + extra {
		ref := chat.MessageRef{ID: fmt.Sprintf("ts%d", i)}
		e.noteToolCalls(ref, []daemon.ToolCall{{ID: fmt.Sprintf("c%d", i), Name: "bash"}}, 0, true)
	}
	e.amu.Lock()
	n, oldest, newest := len(e.notices), e.notices[0].ref.ID, e.notices[len(e.notices)-1].ref.ID
	e.amu.Unlock()
	if n != noticeMemory {
		t.Fatalf("session holds %d notices, want %d", n, noticeMemory)
	}
	// Which end is dropped is the part that matters: a result almost always
	// answers one of the most recent few, so trimming the newest would make the
	// bound worse than no bound.
	if want := fmt.Sprintf("ts%d", extra); oldest != want {
		t.Errorf("oldest surviving notice = %s, want %s — the wrong end was trimmed", oldest, want)
	}
	if want := fmt.Sprintf("ts%d", noticeMemory+extra-1); newest != want {
		t.Errorf("newest surviving notice = %s, want %s", newest, want)
	}
	// And the survivors are still addressable, not just present.
	if ref, _, ok := fileResult(e, daemon.ToolResult{ID: fmt.Sprintf("c%d", noticeMemory+extra-1), Name: "bash"}); !ok {
		t.Error("the newest notice survived the trim but no longer resolves")
	} else if ref.ID != newest {
		t.Errorf("resolved to %s, want %s", ref.ID, newest)
	}
}

// TestTheTurnTransitionDropsThePreviousTurnsNotices: a notice is registered
// only after the Send that posted it returns, while the turn transition runs on
// the goroutine handling the next turn. So the notice lands on either side of
// the transition, and each side needs its own guard.
//
// Registered first, it is in e.notices when the transition runs, and the clear
// is what drops it. Registered after — a Send slow enough to outlive the turn
// that made it — there is nothing to clear, and the stamp it carries is what
// drops it. Miss either and turn two's results tick lines off turn one's
// notice: the wrong message edited with another turn's verdicts.
//
// beginTurnInFlight takes both halves under one lock, so there is no third
// case: a notice cannot land midway through the transition and slip past both.
func TestTheTurnTransitionDropsThePreviousTurnsNotices(t *testing.T) {
	note := func(e *sessionEntry, turn int64) {
		e.noteToolCalls(chat.MessageRef{ID: "ts1"}, []daemon.ToolCall{{ID: "a", Name: "bash"}}, turn, true)
	}
	for _, tc := range []struct {
		name string
		run  func(e *sessionEntry, turn int64)
	}{
		{"the notice is registered before the transition", func(e *sessionEntry, turn int64) {
			note(e, turn)
			e.beginTurnInFlight()
		}},
		{"the notice is registered after it", func(e *sessionEntry, turn int64) {
			e.beginTurnInFlight()
			note(e, turn)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &sessionEntry{}
			tc.run(e, e.turnSeq.Load()) // the stamp postActivity read before its Send
			if _, _, ok := fileResult(e, daemon.ToolResult{ID: "a", Name: "bash"}); ok {
				t.Fatal("a notice from the previous turn collected this turn's results")
			}
		})
	}
}

// TestResolveToolSearchesNoticesOldestFirstOnAName: the FIFO argument that
// pending makes within one notice has to hold across notices too, or two
// same-named calls in two frames are paired up backwards — which is what
// searching the notices newest-first did.
func TestResolveToolSearchesNoticesOldestFirstOnAName(t *testing.T) {
	e := &sessionEntry{}
	e.noteToolCalls(chat.MessageRef{ID: "ts1"}, []daemon.ToolCall{call("bash", "first")}, 0, true)
	e.noteToolCalls(chat.MessageRef{ID: "ts2"}, []daemon.ToolCall{call("bash", "second")}, 0, true)

	if ref, text, ok := fileResult(e, daemon.ToolResult{Name: "bash"}); !ok || ref.ID != "ts1" {
		t.Fatalf("the first result edited %q (ok=%v), want ts1\n%s", ref.ID, ok, text)
	}
	if ref, _, ok := fileResult(e, daemon.ToolResult{Name: "bash"}); !ok || ref.ID != "ts2" {
		t.Fatalf("the second result edited %q (ok=%v), want ts2", ref.ID, ok)
	}
}

// TestApplyToolResultsCoalescesAFrame: a frame answering several calls on one
// notice is one edit, not one per result. Editing per result costs an API call
// each and writes intermediate states that were already stale — the reader sees
// the notice flicker through partial verdicts to reach the same place.
func TestApplyToolResultsCoalescesAFrame(t *testing.T) {
	e := &sessionEntry{}
	e.noteToolCalls(chat.MessageRef{ID: "ts1"}, []daemon.ToolCall{
		{ID: "a", Name: "bash", Arg: "one"},
		{ID: "b", Name: "bash", Arg: "two"},
	}, 0, true)

	edits := e.applyToolResults([]daemon.ToolResult{
		{ID: "a", Name: "bash"},
		{ID: "b", Name: "bash", Failed: true, Detail: "exit 2"},
	})
	if len(edits) != 1 {
		t.Fatalf("got %d edits for one notice, want 1", len(edits))
	}
	want := "❌ Ran 2 tools (1 failed)\n" +
		"• ✅ `bash` — one\n" +
		"• ❌ `bash` (exit 2) — two"
	if edits[0].text != want {
		t.Fatalf("the single edit is not the final state:\n%s\nwant\n%s", edits[0].text, want)
	}
}

// TestApplyToolResultsFilesIdsBeforeGuessingByName: within one frame an id is a
// claim on a specific line and a name is only a guess, so every id in the frame
// is honoured before any name is. Filing in arrival order let the guess take the
// line the claim owned: the id-carrying result then found it answered and was
// dropped, so one call wore another's verdict and the other stayed at 🔧.
func TestApplyToolResultsFilesIdsBeforeGuessingByName(t *testing.T) {
	e := &sessionEntry{}
	e.noteToolCalls(chat.MessageRef{ID: "ts1"}, []daemon.ToolCall{
		{ID: "a", Name: "bash", Arg: "one"},
		{ID: "b", Name: "bash", Arg: "two"},
	}, 0, true)

	// The id-less result arrives first, but "a" is spoken for.
	edits := e.applyToolResults([]daemon.ToolResult{
		{Name: "bash", Failed: true, Detail: "exit 7"},
		{ID: "a", Name: "bash"},
	})
	if len(edits) != 1 {
		t.Fatalf("got %d edits for one notice, want 1", len(edits))
	}
	want := "❌ Ran 2 tools (1 failed)\n" +
		"• ✅ `bash` — one\n" +
		"• ❌ `bash` (exit 7) — two"
	if edits[0].text != want {
		t.Fatalf("the frame was filed in arrival order, not ids first:\n%s\nwant\n%s", edits[0].text, want)
	}
}

// TestResolveToolPrefersTheNewestNoticeHoldingARepeatedID: ids are meant to be
// unique, and nothing switchboard can see enforces that. A daemon numbering its
// calls per frame repeats them, and the newest notice is the one still being
// answered — so a repeat resolves there rather than against a straggler two
// frames back. Where ids really are unique this changes nothing.
func TestResolveToolPrefersTheNewestNoticeHoldingARepeatedID(t *testing.T) {
	e := &sessionEntry{}
	e.noteToolCalls(chat.MessageRef{ID: "ts1"}, []daemon.ToolCall{{ID: "0", Name: "bash", Arg: "first"}}, 0, true)
	e.noteToolCalls(chat.MessageRef{ID: "ts2"}, []daemon.ToolCall{{ID: "0", Name: "bash", Arg: "second"}}, 0, true)

	if ref, text, ok := fileResult(e, daemon.ToolResult{ID: "0", Name: "bash"}); !ok || ref.ID != "ts2" {
		t.Fatalf("a repeated id resolved to %q (ok=%v), want ts2\n%s", ref.ID, ok, text)
	}
}

// TestAFailedEditHealsOnTheNextResult: a notice is re-rendered whole every
// time, so a result whose edit never reached the platform is carried in by the
// next one that does. This is why a failed edit leaves its result *filed*:
// un-filing it to allow a retry would make the next render draw that call as
// still running, and the verdict would be lost for good — and there is no
// retry to allow, because the relay's watermark has already moved past the
// frame that carried it.
func TestAFailedEditHealsOnTheNextResult(t *testing.T) {
	e := &sessionEntry{}
	e.noteToolCalls(chat.MessageRef{ID: "ts1"}, []daemon.ToolCall{
		{ID: "a", Name: "bash", Arg: "one"},
		{ID: "b", Name: "bash", Arg: "two"},
	}, 0, true)

	// The edit for this one fails to reach the platform; nothing is un-filed.
	if edits := e.applyToolResults([]daemon.ToolResult{{ID: "a", Name: "bash", Failed: true, Detail: "exit 2"}}); len(edits) != 1 {
		t.Fatalf("got %d edits, want 1", len(edits))
	}

	_, text, ok := fileResult(e, daemon.ToolResult{ID: "b", Name: "bash"})
	if !ok {
		t.Fatal("the second result found no notice")
	}
	want := "❌ Ran 2 tools (1 failed)\n" +
		"• ❌ `bash` (exit 2) — one\n" +
		"• ✅ `bash` — two"
	if text != want {
		t.Fatalf("the lost verdict was not carried in:\n%s\nwant\n%s", text, want)
	}
}

// TestResolveToolRerendersInTheModeItWasPostedIn. The notice records the mode
// it was rendered in rather than assuming stream. Only stream registers a
// notice today, so this pins the property rather than a live path: the
// no-arguments guarantee that status and indicator make should not rest on a
// single `if detail` at the registration site.
func TestResolveToolRerendersInTheModeItWasPostedIn(t *testing.T) {
	e := &sessionEntry{}
	e.noteToolCalls(chat.MessageRef{ID: "ts1"}, []daemon.ToolCall{call("bash", "kubectl get secret x -o yaml")}, 0, false)

	_, text, ok := fileResult(e, daemon.ToolResult{Name: "bash"})
	if !ok {
		t.Fatal("the result found no notice")
	}
	if strings.Contains(text, "kubectl") {
		t.Fatalf("a terse notice re-rendered with its argument:\n%s", text)
	}
}

const (
	// A parallel frame and the two results that answer it, in the wire shape
	// #36 describes: calls are model-authored, results are not.
	twoCallEvent = `{"seq":1,"event":{"Content":{"parts":[` +
		`{"functionCall":{"id":"a","name":"bash","args":{"command":"kubectl get pods -A"}}},` +
		`{"functionCall":{"id":"b","name":"bash","args":{"command":"kubectl get ns --context nope"}}}` +
		`],"role":"model"}}}`
	okResultEvent = `{"seq":2,"event":{"Content":{"parts":[` +
		`{"functionResponse":{"id":"a","name":"bash","response":{"exit_code":0}}}],"role":"user"}}}`
	failResultEvent = `{"seq":3,"event":{"Content":{"parts":[` +
		`{"functionResponse":{"id":"b","name":"bash","response":{"exit_code":2}}}],"role":"user"}}}`
	// The answer behind them, at the seq it would really carry.
	lateAnswerEvent = `{"seq":4,"event":{"Content":{"parts":[{"text":"the answer"}],"role":"model"},"Partial":false}}`
)

// TestStreamModeTicksResultsOffTheNoticeItPosted is the end-to-end shape of
// #36: one notice per frame, edited in place as results land. A turn making
// fifteen calls would otherwise put thirty messages in the thread, and the
// second fifteen carry one bit each.
func TestStreamModeTicksResultsOffTheNoticeItPosted(t *testing.T) {
	router, fake := newEventRouter(t, ProgressStream, nil, twoCallEvent, okResultEvent, failResultEvent, lateAnswerEvent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := router.Handle(ctx, chat.Message{Conversation: "C0:1", Caller: "a@b.com", Text: "hi"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	notice := recvReply(t, fake.replies)
	want := "🔧 Running 2 tools\n" +
		"• 🔧 `bash` — kubectl get pods -A\n" +
		"• 🔧 `bash` — kubectl get ns --context nope"
	if notice.Text != want {
		t.Fatalf("notice =\n%s\nwant\n%s", notice.Text, want)
	}
	if notice.Kind != chat.KindActivity {
		t.Errorf("notice kind = %v, want KindActivity", notice.Kind)
	}
	if got := recvReply(t, fake.replies); got.Text != "the answer" {
		t.Fatalf("second message = %q, want the answer — results must edit, not post", got.Text)
	}

	// Both results edited the one notice, and the last edit shows both verdicts.
	waitFor(t, func() bool { return len(fake.updatedCalls()) == 2 }, "the results did not edit the notice twice")
	edits := fake.updatedCalls()
	last := edits[len(edits)-1]
	if last.ref.ID != "ts1" {
		t.Errorf("edited %q, want the notice ts1", last.ref.ID)
	}
	wantFinal := "❌ Ran 2 tools (1 failed)\n" +
		"• ✅ `bash` — kubectl get pods -A\n" +
		"• ❌ `bash` (exit 2) — kubectl get ns --context nope"
	if last.text != wantFinal {
		t.Fatalf("final notice =\n%s\nwant\n%s", last.text, wantFinal)
	}
}

// TestToolActivityNeverShadowsTheAnswer: tool notices and answers dedupe
// against separate watermarks. Sharing one would mean a tool result at seq N
// makes an answer at seq N look like a replay, and the answer is dropped in
// silence — the worst thing this gateway can do. Seqs only go up on a healthy
// stream, so this cannot arise from a daemon behaving itself; the watermark
// exists precisely for a stream that is not.
func TestToolActivityNeverShadowsTheAnswer(t *testing.T) {
	// answerEvent is seq 2, the same seq the tool result carries.
	router, fake := newEventRouter(t, ProgressStream, nil, twoCallEvent, okResultEvent, answerEvent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := router.Handle(ctx, chat.Message{Conversation: "C0:1", Caller: "a@b.com", Text: "hi"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	recvReply(t, fake.replies) // the notice
	if got := recvReply(t, fake.replies); got.Text != "the answer" {
		t.Fatalf("second message = %q, want the answer — a tool result shadowed it", got.Text)
	}
}

// TestOffModeSurfacesNoToolActivityAtAll: the dispatch is gated on the progress
// mode, so a deployment that opted out of progress does not start receiving
// tool notices — or argument summaries — because #36 landed.
func TestOffModeSurfacesNoToolActivityAtAll(t *testing.T) {
	router, fake := newEventRouter(t, ProgressOff, nil, twoCallEvent, okResultEvent, lateAnswerEvent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := router.Handle(ctx, chat.Message{Conversation: "C0:1", Caller: "a@b.com", Text: "hi"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := recvReply(t, fake.replies); got.Text != "the answer" {
		t.Fatalf("first message = %q, want the answer and nothing before it", got.Text)
	}
	if n := len(fake.updatedCalls()); n != 0 {
		t.Errorf("off mode edited %d message(s); want 0", n)
	}
}
