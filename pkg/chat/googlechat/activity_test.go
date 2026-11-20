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

package googlechat

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/go-steer/switchboard/pkg/chat"
)

func TestActivityCardNil(t *testing.T) {
	if card := activityCard(nil); card != nil {
		t.Fatalf("activityCard(nil) = %+v, want nil", card)
	}
}

// While running in status mode (Recent empty), the card is a single uncollapsed
// DecoratedText line with the hourglass icon — even when Steps has accumulated
// calls, because an expanded Collapsible section resets to collapsed on every
// messages.patch in Google Chat.
func TestRunningStatusActivityIsOneUncollapsedLine(t *testing.T) {
	a := &chat.Activity{
		Title:  "Working… 18s · 3 calls · _fallback › grep",
		Status: chat.StepRunning,
		Steps: []chat.Step{
			{Title: "bash", Status: chat.StepDone},
			{Title: "_fallback", Status: chat.StepRunning, Calls: 1, Steps: []chat.Step{
				{Title: "grep", Status: chat.StepRunning},
			}},
		},
	}
	card := activityCard(a)
	if card == nil || len(card.Sections) != 1 {
		t.Fatalf("card = %+v, want 1 section", card)
	}
	sec := card.Sections[0]
	if sec.Collapsible || sec.UncollapsibleWidgetsCount != 0 {
		t.Errorf("running section Collapsible=%t UncollapsibleWidgetsCount=%d, want false and 0",
			sec.Collapsible, sec.UncollapsibleWidgetsCount)
	}
	if len(sec.Widgets) != 1 {
		t.Fatalf("widgets = %d, want 1 (title alone in status mode)", len(sec.Widgets))
	}
	dt := sec.Widgets[0].DecoratedText
	if dt == nil || dt.StartIcon == nil || dt.StartIcon.MaterialIcon == nil {
		t.Fatalf("widget 0 = %+v, want DecoratedText with MaterialIcon", sec.Widgets[0])
	}
	if got := dt.StartIcon.MaterialIcon.Name; got != iconProgress {
		t.Errorf("icon = %q, want %q", got, iconProgress)
	}
	if !dt.WrapText {
		t.Error("WrapText = false, want true")
	}
	if want := "<b>Working… 18s · 3 calls · _fallback › grep</b>"; dt.Text != want {
		t.Errorf("title text = %q, want %q", dt.Text, want)
	}
}

// While running in stream mode (Recent populated with up to 5 lines), the card
// stays uncollapsed and shows the rolling window in a second HTML TextParagraph
// widget, <br>-joined at top level so it is readable with zero clicks and never
// snaps shut across patches.
func TestRunningStreamActivityShowsRecentWindowUncollapsed(t *testing.T) {
	a := &chat.Activity{
		Title:  "Working… 25s · 6 calls · grep",
		Status: chat.StepRunning,
		Steps:  make([]chat.Step, 6),
		Recent: []chat.Step{
			{Title: "bash", Status: chat.StepDone, Note: "2.3s", Code: "kubectl get pods -A"},
			{Title: "_fallback", Status: chat.StepRunning, Calls: 3, Code: "check billing"},
			{Title: "read_file", Status: chat.StepDone, Note: "0.4s", Code: "deploy/billing.yaml"},
			{Title: "list_pods ×2", Status: chat.StepDone, Code: "ns=billing"},
			{Title: "grep", Status: chat.StepRunning, Code: "memory"},
		},
	}
	card := activityCard(a)
	if card == nil || len(card.Sections) != 1 {
		t.Fatalf("card = %+v, want 1 section", card)
	}
	sec := card.Sections[0]
	if sec.Collapsible || sec.UncollapsibleWidgetsCount != 0 {
		t.Errorf("running stream section Collapsible=%t UncollapsibleWidgetsCount=%d, want false and 0",
			sec.Collapsible, sec.UncollapsibleWidgetsCount)
	}
	if len(sec.Widgets) != 2 {
		t.Fatalf("widgets = %d, want 2 (DecoratedText + Recent TextParagraph)", len(sec.Widgets))
	}
	if got := sec.Widgets[0].DecoratedText.StartIcon.MaterialIcon.Name; got != iconProgress {
		t.Errorf("icon = %q, want %q", got, iconProgress)
	}
	tp := sec.Widgets[1].TextParagraph
	if tp == nil || tp.TextSyntax != "" {
		t.Fatalf("widget 1 = %+v, want default-HTML TextParagraph (empty TextSyntax)", sec.Widgets[1])
	}
	wantLines := []string{
		"• ✔\uFE0E <b>bash</b>  2.3s  <tt>kubectl get pods -A</tt>",
		"• ▸ <b>_fallback</b> · 3 calls  <tt>check billing</tt>",
		"• ✔\uFE0E <b>read_file</b>  0.4s  <tt>deploy/billing.yaml</tt>",
		"• ✔\uFE0E <b>list_pods ×2</b>  <tt>ns=billing</tt>",
		"• ▸ <b>grep</b>  <tt>memory</tt>",
	}
	if want := strings.Join(wantLines, "<br>"); tp.Text != want {
		t.Errorf("recent paragraph =\n%s\nwant\n%s", tp.Text, want)
	}
}

// Once settled (StepDone, StepFailed, StepUnfinished), patches have stopped, so
// a card with Steps folds to its 1-line title (Collapsible: true,
// UncollapsibleWidgetsCount: 1) and puts the full nested call history behind
// the expander. A title-only settled card (len(Steps) == 0) stays uncollapsed.
func TestSettledActivityFoldsHistoryBehindOneLine(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   chat.StepStatus
		wantIcon string
	}{
		{"done", chat.StepDone, iconToolOK},
		{"failed", chat.StepFailed, iconToolFail},
		{"unfinished", chat.StepUnfinished, iconToolOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &chat.Activity{
				Title:  "Done · 40s · 5 calls (1 failed)",
				Status: tc.status,
				Steps: []chat.Step{
					{Title: "bash", Status: chat.StepDone, Note: "2.3s", Code: "kubectl get pods -A"},
					{Title: "_fallback", Status: chat.StepUnfinished, Calls: 1, Note: "18s", Code: "check billing", Steps: []chat.Step{
						{Title: "read_file", Status: chat.StepDone, Note: "0.4s", Code: "deploy/billing.yaml", Steps: []chat.Step{
							{Title: "grandchild_ignored", Status: chat.StepDone},
						}},
						{Title: "grep", Status: chat.StepUnfinished, Code: "memory"},
					}},
					{Title: "bash (exit 2)", Status: chat.StepFailed},
					{Title: "tail", Status: chat.StepRunning},
				},
				// Even if Recent were left populated on the struct, a settled
				// card renders Steps, not Recent.
				Recent: []chat.Step{{Title: "stale_recent", Status: chat.StepRunning}},
			}
			card := activityCard(a)
			if card == nil || len(card.Sections) != 1 {
				t.Fatalf("card = %+v, want 1 section", card)
			}
			sec := card.Sections[0]
			if !sec.Collapsible || sec.UncollapsibleWidgetsCount != 1 {
				t.Errorf("Collapsible=%t UncollapsibleWidgetsCount=%d, want true and 1",
					sec.Collapsible, sec.UncollapsibleWidgetsCount)
			}
			if got := sec.Widgets[0].DecoratedText.StartIcon.MaterialIcon.Name; got != tc.wantIcon {
				t.Errorf("icon = %q, want %q", got, tc.wantIcon)
			}
			if len(sec.Widgets) != 2 {
				t.Fatalf("widgets = %d, want 2", len(sec.Widgets))
			}
			got := sec.Widgets[1].TextParagraph.Text
			wantLines := []string{
				"• ✔\uFE0E <b>bash</b>  2.3s  <tt>kubectl get pods -A</tt>",
				"• – <b>_fallback</b> · 1 call  18s  <tt>check billing</tt>",
				"\u00a0\u00a0\u00a0\u00a0◦ ✔\uFE0E <b>read_file</b>  0.4s  <tt>deploy/billing.yaml</tt>",
				"\u00a0\u00a0\u00a0\u00a0◦ – <b>grep</b>  <tt>memory</tt>",
				"• ✖\uFE0E <b>bash (exit 2)</b>",
				"• ▸ <b>tail</b>",
			}
			if want := strings.Join(wantLines, "<br>"); got != want {
				t.Errorf("history =\n%s\nwant\n%s", got, want)
			}
			if strings.Contains(got, "grandchild_ignored") || strings.Contains(got, "stale_recent") {
				t.Errorf("unexpected lines in settled history: %q", got)
			}
		})
	}

	// Title-only settled card (len(Steps) == 0): Widget 0 alone, Collapsible false.
	titleOnly := activityCard(&chat.Activity{Title: "Done · 4s · 0 calls", Status: chat.StepDone})
	if titleOnly == nil {
		t.Fatal("title-only settled card returned nil")
	}
	sec := titleOnly.Sections[0]
	if sec.Collapsible || sec.UncollapsibleWidgetsCount != 0 || len(sec.Widgets) != 1 {
		t.Errorf("title-only settled section: Collapsible=%t Uncollapsible=%d widgets=%d, want false, 0, 1",
			sec.Collapsible, sec.UncollapsibleWidgetsCount, len(sec.Widgets))
	}
	if got := sec.Widgets[0].DecoratedText.StartIcon.MaterialIcon.Name; got != iconToolOK {
		t.Errorf("title-only settled icon = %q, want %q", got, iconToolOK)
	}

	// Empty titles fall back to "?" rather than emitting empty bold tags.
	blank := activityCard(&chat.Activity{Status: chat.StepDone, Steps: []chat.Step{{Status: chat.StepDone}}})
	if blank.Sections[0].Widgets[0].DecoratedText.Text != "<b>?</b>" {
		t.Errorf("blank card title = %q, want <b>?</b>", blank.Sections[0].Widgets[0].DecoratedText.Text)
	}
	if blank.Sections[0].Widgets[1].TextParagraph.Text != "• ✔\uFE0E <b>?</b>" {
		t.Errorf("blank step title = %q, want • ✔︎ <b>?</b>", blank.Sections[0].Widgets[1].TextParagraph.Text)
	}
}

// Hostile or markup-like text in titles, notes, and arguments is HTML-escaped
// literally (never fed through toCardHTML, which translates *bold* and `code`
// as Chat markup), and multi-byte UTF-8 runes are clamped without splitting a
// character or an HTML entity.
func TestActivityCardEscapesHTMLAndClampsRunesSafely(t *testing.T) {
	a := &chat.Activity{
		Title:  "Working <script> & *notbold* \"quoted\" 'apos'",
		Status: chat.StepDone,
		Steps: []chat.Step{
			{
				Title:  "<b>tool</b> & _snake_case_ *raw*",
				Status: chat.StepDone,
				Calls:  2,
				Note:   "<1s & fast",
				Code:   "<script>alert(1)</script> & <users/all> *literal* 'single' `ticks`",
			},
		},
	}
	card := activityCard(a)
	if card == nil {
		t.Fatal("no card")
	}
	headText := card.Sections[0].Widgets[0].DecoratedText.Text
	wantHead := "<b>Working &lt;script&gt; &amp; *notbold* &quot;quoted&quot; 'apos'</b>"
	if headText != wantHead {
		t.Errorf("title = %q, want %q", headText, wantHead)
	}
	bodyText := card.Sections[0].Widgets[1].TextParagraph.Text
	wantBody := "• ✔\uFE0E <b>&lt;b&gt;tool&lt;/b&gt; &amp; _snake_case_ *raw*</b> · 2 calls  &lt;1s &amp; fast  " +
		"<tt>&lt;script&gt;alert(1)&lt;/script&gt; &amp; &lt;users/all&gt; *literal* 'single' `ticks`</tt>"
	if bodyText != wantBody {
		t.Errorf("step =\n%s\nwant\n%s", bodyText, wantBody)
	}

	// Multi-byte UTF-8 runes exceeding maxActivityTitle and maxWidgetText clamp
	// on rune boundaries without invalid UTF-8 or broken entities.
	longTitle := strings.Repeat("é", maxActivityTitle+20)
	longNote := strings.Repeat("命", maxActivityTitle+20)
	longEntities := strings.Repeat("&", 1000) // 1000 * 5 = 5000 bytes escaped > maxWidgetText
	longUTF8Code := strings.Repeat("ü", 2100) // 4200 bytes > maxWidgetText
	clamped := activityCard(&chat.Activity{
		Title:  longTitle,
		Status: chat.StepDone,
		Steps: []chat.Step{
			{Title: longTitle, Status: chat.StepDone, Note: longNote, Code: longEntities},
			{Title: "utf8", Status: chat.StepDone, Code: longUTF8Code},
		},
	})
	if clamped == nil {
		t.Fatal("clamped card returned nil")
	}
	dt := clamped.Sections[0].Widgets[0].DecoratedText.Text
	if !utf8.ValidString(dt) {
		t.Errorf("clamped DecoratedText is invalid UTF-8")
	}
	innerTitle := strings.TrimSuffix(strings.TrimPrefix(dt, "<b>"), "</b>")
	if n := utf8.RuneCountInString(innerTitle); n != maxActivityTitle {
		t.Errorf("clamped title rune count = %d, want %d", n, maxActivityTitle)
	}
	for i, w := range clamped.Sections[0].Widgets[1:] {
		txt := w.TextParagraph.Text
		if !utf8.ValidString(txt) {
			t.Errorf("widget %d invalid UTF-8", i+1)
		}
		if len(txt) > maxWidgetText {
			t.Errorf("widget %d len = %d, exceeds maxWidgetText %d", i+1, len(txt), maxWidgetText)
		}
		if strings.Count(txt, "<tt>") != strings.Count(txt, "</tt>") ||
			strings.Count(txt, "<b>") != strings.Count(txt, "</b>") {
			t.Errorf("widget %d has unbalanced HTML tags: %q", i+1, txt)
		}
		// Every '&' must begin a complete entity (&amp;, &nbsp;, etc.).
		for rest := txt; ; {
			idx := strings.IndexByte(rest, '&')
			if idx < 0 {
				break
			}
			semi := strings.IndexByte(rest[idx:], ';')
			if semi <= 1 || semi > 7 {
				t.Fatalf("widget %d has broken HTML entity near %q", i+1, rest[idx:min(len(rest), idx+12)])
			}
			rest = rest[idx+semi+1:]
		}
	}
}

// Long settled call histories batch across multiple TextParagraph widgets on
// line boundaries (each <= maxWidgetText bytes). At MaxActivityLines (60) even
// dense subagent shell lines fit intact; past maxCardBytes (e.g. a burst inside
// the 3s redraw window right before turn-complete), excess lines are elided
// inside the folded card so Update never overwrites it with a 4,096-byte
// truncated wall of plain text.
func TestActivityCardBatchesWidgetsAndElidesOverByteCeiling(t *testing.T) {
	const shellArg = `kubectl get pods -n production -o jsonpath='{.items[?(@.status.phase=="Running")].metadata.name}' 2>&1 | head -n 20`
	makeSubagentBurst := func(children int) *chat.Activity {
		subs := make([]chat.Step, children)
		for i := range subs {
			subs[i] = chat.Step{
				Title:  fmt.Sprintf("bash_%03d", i),
				Status: chat.StepDone,
				Note:   "<0.1s",
				Code:   shellArg,
			}
		}
		return &chat.Activity{
			Title:  fmt.Sprintf("Done · 2m10s · %d calls", children+1),
			Status: chat.StepDone,
			Steps: []chat.Step{
				{
					Title:  "_fallback",
					Status: chat.StepDone,
					Calls:  children,
					Note:   "2m10s",
					Code:   "inspect running production pods",
					Steps:  subs,
				},
			},
		}
	}

	// MaxActivityLines (1 parent + 59 dense subagent shell lines = 60 lines):
	// fits completely under maxCardBytes with zero elision, batched across
	// several <= maxWidgetText widgets.
	card := activityCard(makeSubagentBurst(maxActivityCardLines - 1))
	if card == nil {
		t.Fatalf("%d-line subagent activity card returned nil; must fit under maxCardBytes", maxActivityCardLines)
	}
	if b := cardBytes(card); b > maxCardBytes {
		t.Fatalf("cardBytes = %d, exceeds maxCardBytes %d", b, maxCardBytes)
	}
	sec := card.Sections[0]
	if !sec.Collapsible || sec.UncollapsibleWidgetsCount != 1 {
		t.Errorf("Collapsible=%t Uncollapsible=%d, want true and 1", sec.Collapsible, sec.UncollapsibleWidgetsCount)
	}
	if len(sec.Widgets) < 4 {
		t.Fatalf("widgets = %d, want %d stream lines batched across multiple TextParagraphs", len(sec.Widgets), maxActivityCardLines)
	}
	var all []string
	for i, w := range sec.Widgets[1:] {
		if w.TextParagraph == nil {
			t.Fatalf("widget %d is not a TextParagraph", i+1)
		}
		txt := w.TextParagraph.Text
		if len(txt) > maxWidgetText {
			t.Errorf("widget %d is %d bytes, over maxWidgetText %d", i+1, len(txt), maxWidgetText)
		}
		if strings.HasPrefix(txt, "<br>") || strings.HasSuffix(txt, "<br>") {
			t.Errorf("widget %d has stray boundary <br>: %q", i+1, txt)
		}
		all = append(all, strings.Split(txt, "<br>")...)
	}
	if len(all) != maxActivityCardLines {
		t.Errorf("reconstructed %d lines across widgets, want %d", len(all), maxActivityCardLines)
	}
	joined := strings.Join(all, "\n")
	for _, want := range []string{"_fallback", "bash_000", "bash_030", fmt.Sprintf("bash_%03d", maxActivityCardLines-2)} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing step %q in batched widgets", want)
		}
	}
	if strings.Contains(joined, "<i>… and ") {
		t.Errorf("%d-line card should not elide any line: %q", maxActivityCardLines, all[len(all)-1])
	}

	// Past maxCardBytes (e.g. a 200-line pre-settle burst): keeps the folded
	// cardsV2 card on Update (preserving ~25 KB of call history + an elision
	// footer) instead of returning nil and clamping to 4 KB of plain text.
	f := &fakeMessenger{}
	ad := &Adapter{msg: f, cards: CardsRich}
	over := makeSubagentBurst(200)
	overCard := activityCard(over)
	if overCard == nil {
		t.Fatal("200-line burst returned nil; want folded card with in-card elision")
	}
	if b := cardBytes(overCard); b > maxCardBytes {
		t.Fatalf("200-line elided cardBytes = %d, exceeds maxCardBytes %d", b, maxCardBytes)
	}
	overSec := overCard.Sections[0]
	if !overSec.Collapsible || overSec.UncollapsibleWidgetsCount != 1 {
		t.Errorf("elided card Collapsible=%t Uncollapsible=%d, want true and 1",
			overSec.Collapsible, overSec.UncollapsibleWidgetsCount)
	}
	lastWidget := overSec.Widgets[len(overSec.Widgets)-1].TextParagraph.Text
	if !strings.Contains(lastWidget, "• <i>… and ") || !strings.HasSuffix(lastWidget, " more</i>") {
		t.Errorf("last widget = %q, want trailing elision line", lastWidget)
	}

	ref := chat.MessageRef{Conversation: "spaces/AAA:spaces/AAA/threads/T1", ID: "spaces/AAA/messages/M1"}
	longFallback := strings.Repeat("✔\uFE0E `bash`  <0.1s  `"+shellArg+"`\n", 200)
	if err := ad.Update(context.Background(), ref, chat.Reply{
		Conversation: ref.Conversation,
		Text:         longFallback,
		Kind:         chat.KindActivity,
		Activity:     over,
	}); err != nil {
		t.Fatalf("Update over-budget activity: %v", err)
	}
	if len(f.patches) != 1 || f.patches[0].card == nil || f.patches[0].text != "" {
		t.Errorf("patches = %+v, want 1 folded cardsV2 patch (not 4 KB plain-text fallback)", f.patches)
	}
	if !ad.DrawsActivity() {
		t.Error("in-card elision must not trip noActivityCards")
	}

	// Synthetic huge running Recent slice also stays within maxCardBytes.
	recentSteps := make([]chat.Step, 200)
	for i := range recentSteps {
		recentSteps[i] = chat.Step{Title: fmt.Sprintf("bash_%03d", i), Status: chat.StepRunning, Code: shellArg}
	}
	runningOver := activityCard(&chat.Activity{
		Title:  "Working… 10s · 200 calls",
		Status: chat.StepRunning,
		Recent: recentSteps,
	})
	if runningOver == nil || cardBytes(runningOver) > maxCardBytes {
		t.Fatalf("running over-budget Recent card = %+v, want non-nil <= %d bytes", runningOver, maxCardBytes)
	}
	lastRecent := runningOver.Sections[0].Widgets[len(runningOver.Sections[0].Widgets)-1].TextParagraph.Text
	if !strings.Contains(lastRecent, "bash_199") {
		t.Errorf("trimmed running Recent window lost latest call bash_199: %q", lastRecent)
	}
}

func TestAdapterDrawsActivityAndMaxLines(t *testing.T) {
	for _, tc := range []struct {
		mode CardMode
		want bool
	}{
		{CardsOff, false},
		{CardsStatus, true},
		{CardsRich, true},
	} {
		a := &Adapter{msg: &fakeMessenger{}, cards: tc.mode}
		if got := a.DrawsActivity(); got != tc.want {
			t.Errorf("Cards=%q: DrawsActivity() = %t, want %t", tc.mode, got, tc.want)
		}
		if got := a.MaxActivityLines(); got != maxActivityCardLines {
			t.Errorf("MaxActivityLines() = %d, want %d", got, maxActivityCardLines)
		}
	}

	// CardsOff sends and edits activity replies as plain text.
	f := &fakeMessenger{}
	off := &Adapter{msg: f, cards: CardsOff}
	rep := chat.Reply{
		Conversation: "spaces/AAA:spaces/AAA/threads/T1",
		Text:         "⏳ Working… 5s · running `bash`",
		Kind:         chat.KindProgress,
		Activity: &chat.Activity{
			Title:  "Working… 5s · 1 call · bash",
			Status: chat.StepRunning,
			Steps:  []chat.Step{{Title: "bash", Status: chat.StepRunning}},
		},
	}
	ref, err := off.Send(context.Background(), rep)
	if err != nil {
		t.Fatalf("Send (CardsOff): %v", err)
	}
	if len(f.creates) != 1 || f.creates[0].card != nil || f.creates[0].text == "" {
		t.Fatalf("CardsOff create = %+v, want plain text", f.creates)
	}
	if err := off.Update(context.Background(), ref, rep); err != nil {
		t.Fatalf("Update (CardsOff): %v", err)
	}
	if len(f.patches) != 1 || f.patches[0].card != nil || f.patches[0].text == "" {
		t.Fatalf("CardsOff patch = %+v, want plain text", f.patches)
	}

	// Replies without Activity (plain clock before first call, Done · 12s on a
	// no-tool turn) keep their gatewayCard rendering in CardsStatus / CardsRich.
	f2 := &fakeMessenger{}
	statusAd := &Adapter{msg: f2, cards: CardsStatus}
	clockRef, err := statusAd.Send(context.Background(), chat.Reply{
		Conversation: "spaces/AAA:spaces/AAA/threads/T1",
		Text:         "⏳ Working…",
		Kind:         chat.KindProgress,
	})
	if err != nil {
		t.Fatalf("Send clock: %v", err)
	}
	if f2.creates[0].card == nil || f2.creates[0].card.Sections[0].Widgets[0].DecoratedText.Text != "Working…" {
		t.Errorf("plain clock card = %+v, want gatewayCard", f2.creates[0].card)
	}
	if err := statusAd.Update(context.Background(), clockRef, chat.Reply{
		Conversation: "spaces/AAA:spaces/AAA/threads/T1",
		Text:         "✔\uFE0E Done · 12s",
		Kind:         chat.KindActivity,
	}); err != nil {
		t.Fatalf("Update Done line: %v", err)
	}
	if f2.patches[0].card == nil || f2.patches[0].card.Sections[0].Widgets[0].DecoratedText.StartIcon.MaterialIcon.Name != iconToolOK {
		t.Errorf("no-tool final card = %+v, want check_circle gatewayCard", f2.patches[0].card)
	}
}

// If Chat rejects an activity card with HTTP 400 on Send or Update, the adapter
// latches noActivityCards (DrawsActivity turns false) and falls back to plain
// text — even on Update when cardText exceeds chatTextLimit (clamping on a
// valid UTF-8 rune boundary rather than leaving a running card stuck). Non-400
// errors and non-activity card rejections do not trip the latch.
func TestActivityCardRejectionLatchesAndFallsBackToText(t *testing.T) {
	actReply := func(text string) chat.Reply {
		return chat.Reply{
			Conversation: "spaces/AAA:spaces/AAA/threads/T1",
			Text:         text,
			Kind:         chat.KindActivity,
			Activity: &chat.Activity{
				Title:  "Done · 40s · 80 calls",
				Status: chat.StepDone,
				Steps:  []chat.Step{{Title: "bash ×80", Status: chat.StepDone}},
			},
		}
	}

	t.Run("Send rejection latches and falls back to text", func(t *testing.T) {
		f := &fakeMessenger{cardErr: apiErr(http.StatusBadRequest)}
		a := &Adapter{msg: f, cards: CardsStatus}
		if !a.DrawsActivity() {
			t.Fatal("fresh CardsStatus adapter should draw activity")
		}
		ref, err := a.Send(context.Background(), actReply("✔\uFE0E Done · 40s · 1 call\n✔\uFE0E `bash`"))
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if ref.ID == "" || len(f.creates) != 1 || f.creates[0].card != nil || !strings.Contains(f.creates[0].text, "`bash`") {
			t.Fatalf("creates = %+v, want 1 plain-text fallback post", f.creates)
		}
		if a.DrawsActivity() {
			t.Error("DrawsActivity() still true after HTTP 400 on activity card Send")
		}

		// Subsequent activity Send and Update go straight to text without
		// attempting a card (even if Chat would accept cards again).
		var cardAttempts int
		f.cardErr = apiErr(http.StatusBadRequest)
		if _, err := a.Send(context.Background(), actReply("✔\uFE0E second")); err != nil {
			t.Fatalf("second Send: %v", err)
		}
		if err := a.Update(context.Background(), ref, actReply("✔\uFE0E edited")); err != nil {
			t.Fatalf("Update after latch: %v", err)
		}
		for _, c := range f.creates {
			if c.card != nil {
				cardAttempts++
			}
		}
		for _, p := range f.patches {
			if p.card != nil {
				cardAttempts++
			}
		}
		if cardAttempts != 0 {
			t.Errorf("sent %d cards after latch tripped, want 0", cardAttempts)
		}

		// Standalone notices (Activity == nil) on the next turn still use
		// gatewayCard when Chat accepts them.
		f.cardErr = nil
		if _, err := a.Send(context.Background(), chat.Reply{
			Conversation: "spaces/AAA:spaces/AAA/threads/T1",
			Text:         "✔\uFE0E Ran `bash`",
			Kind:         chat.KindActivity,
		}); err != nil {
			t.Fatalf("Send standalone notice: %v", err)
		}
		if last := f.creates[len(f.creates)-1]; last.card == nil {
			t.Errorf("standalone notice after activity latch should still render gatewayCard, got %+v", last)
		}
	})

	t.Run("Update rejection with over-limit multi-byte cardText clamps and latches", func(t *testing.T) {
		// 8,000 bytes with 3-byte UTF-8 marks at varying offsets past chatTextLimit (4096).
		longBase := strings.Repeat("✔\uFE0E `bash`  1.2s  `kubectl get pods`\n", 220)
		if len(longBase) <= chatTextLimit {
			t.Fatalf("fixture len = %d, want > %d", len(longBase), chatTextLimit)
		}
		for offset := 0; offset < 4; offset++ {
			f := &fakeMessenger{cardErr: apiErr(http.StatusBadRequest)}
			a := &Adapter{msg: f, cards: CardsRich}
			ref := chat.MessageRef{Conversation: "spaces/AAA:spaces/AAA/threads/T1", ID: "spaces/AAA/messages/M1"}
			err := a.Update(context.Background(), ref, actReply(strings.Repeat("x", offset)+longBase))
			if err != nil {
				t.Fatalf("offset %d: Update returned error %v, want clamped text fallback", offset, err)
			}
			if a.DrawsActivity() {
				t.Errorf("offset %d: DrawsActivity() still true after HTTP 400 on Update", offset)
			}
			if len(f.patches) != 1 || f.patches[0].card != nil {
				t.Fatalf("offset %d: patches = %+v, want 1 text patch", offset, f.patches)
			}
			got := f.patches[0].text
			if !utf8.ValidString(got) {
				t.Errorf("offset %d: clamped text is not valid UTF-8", offset)
			}
			if len(got) > chatTextLimit || len(got) < chatTextLimit-6 {
				t.Errorf("offset %d: clamped text len = %d, want just under %d", offset, len(got), chatTextLimit)
			}
			if f.patches[0].mask != patchMask {
				t.Errorf("offset %d: mask = %q, want %q", offset, f.patches[0].mask, patchMask)
			}
		}
	})

	t.Run("transient 429/500 and non-activity 400 do not trip activity latch", func(t *testing.T) {
		for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError} {
			f := &fakeMessenger{cardErr: apiErr(status)}
			a := &Adapter{msg: f, cards: CardsRich}
			if _, err := a.Send(context.Background(), actReply("Working")); err == nil {
				t.Fatalf("status %d: want error propagated", status)
			}
			if !a.DrawsActivity() {
				t.Errorf("HTTP %d must not trip noActivityCards", status)
			}
			ref := chat.MessageRef{Conversation: "spaces/AAA:spaces/AAA/threads/T1", ID: "spaces/AAA/messages/M1"}
			if err := a.Update(context.Background(), ref, actReply("Done")); err == nil {
				t.Fatalf("status %d on Update: want error propagated", status)
			}
			if !a.DrawsActivity() {
				t.Errorf("HTTP %d on Update must not trip noActivityCards", status)
			}
		}

		// Non-activity answer card rejection on Update with over-limit text
		// still returns an error (protecting chunked answers) and leaves
		// noActivityCards false.
		f := &fakeMessenger{cardErr: apiErr(http.StatusBadRequest)}
		a := &Adapter{msg: f, cards: CardsRich}
		longAnswer := "# Header\n\n" + strings.Repeat("word ", chatTextLimit)
		ref := chat.MessageRef{Conversation: "spaces/AAA:spaces/AAA/threads/T1", ID: "spaces/AAA/messages/M1"}
		if err := a.Update(context.Background(), ref, chat.Reply{
			Conversation: ref.Conversation,
			Text:         longAnswer,
			Kind:         chat.KindAnswer,
		}); err == nil {
			t.Fatal("over-limit answer card rejection on Update must still refuse truncation")
		}
		if !a.DrawsActivity() {
			t.Error("answer card rejection must not trip noActivityCards")
		}
	})
}
