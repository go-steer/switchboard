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
	"fmt"
	"slices"
	"strconv"
	"strings"

	chatv1 "google.golang.org/api/chat/v1"

	"github.com/go-steer/switchboard/pkg/chat"
)

// Google Chat activity cards (R2-B layout), measured against the live Chat API
// on 2026-11-20:
//
// GoogleAppsCardV1Section's Collapsible: true with UncollapsibleWidgetsCount: 1
// renders a native client-side chevron / "Show more" expander with zero
// callback events, so it works identically over Pub/Sub and HTTP and adds no
// interactive controls. However, unlike Slack's task_card (which stays open
// across chat.update when given a fixed block_id), Google Chat's web client
// resets an expanded Collapsible section back to collapsed on every
// messages.patch — even when card, section, and widget identifiers are fixed.
//
// Activity cards therefore take two shapes across a turn's lifetime:
//
//   - Running (StepRunning): Collapsible is false so no chevron snaps shut
//     under a reader's cursor. Status mode draws the title line alone; stream
//     mode draws the rolling Recent window directly below it, visible with no
//     click.
//   - Settled (StepDone, StepFailed, StepUnfinished): edits to this message
//     have ended, so when Steps is non-empty the section folds to its 1-line
//     title (Collapsible: true, UncollapsibleWidgetsCount: 1) and holds the
//     complete nested call history behind the chevron, staying open once
//     expanded.

var (
	_ chat.DrawsActivity     = (*Adapter)(nil)
	_ chat.ActivityLineLimit = (*Adapter)(nil)
)

const (
	// maxActivityCardLines is the rollover cap reported via
	// chat.ActivityLineLimit. encoding/json escapes <, >, and & as 6-byte \u00xx
	// sequences on the wire, so 60 dense subagent stream lines with 120-byte
	// shell arguments serialize to ~15–17 KB — leaving ~9 KB of headroom below
	// maxCardBytes (26 KB) for bursts between coalesced redraws.
	maxActivityCardLines = 60

	// maxActivityTitle bounds the card's title, each step's title, and each
	// step's note in runes.
	maxActivityTitle = 256

	// maxActivityCode bounds a step's argument in runes before escaping (over
	// 4x daemon's 120-byte argSummaryCap); stepLineHTML additionally shrinks it
	// if HTML entity expansion would push the line past maxWidgetText bytes.
	maxActivityCode = 500

	// Step marks mirror pkg/chat/slack/activity.go: check and cross carry the
	// text-style variation selector (U+FE0E), running is a triangle, and an
	// unfinished step (left behind on a rollover or restart) gets an en dash.
	lineRunning    = "▸"
	lineDone       = "✔\uFE0E"
	lineFailed     = "✖\uFE0E"
	lineUnfinished = "–"

	// Literal UTF-8 non-breaking spaces (U+00A0) indent child lines in 8 wire
	// bytes rather than 40 for four JSON-escaped &nbsp; entities.
	topStepBullet   = "• "
	childStepBullet = "\u00a0\u00a0\u00a0\u00a0◦ "
)

// DrawsActivity implements chat.DrawsActivity: tool activity is drawn as one
// in-place card per turn whenever cards are enabled (CardsStatus and
// CardsRich), unless Chat has refused an activity card.
func (a *Adapter) DrawsActivity() bool {
	return a.cards != CardsOff && !a.noActivityCards.Load()
}

// MaxActivityLines implements chat.ActivityLineLimit.
func (a *Adapter) MaxActivityLines() int { return maxActivityCardLines }

// refuseActivityCards stops drawing activity cards for the rest of the process
// after Chat rejects one (400/413), so a turn ticking every few seconds does
// not spend a rejected write before every text fallback.
func (a *Adapter) refuseActivityCards(where string, err error) {
	if a.noActivityCards.CompareAndSwap(false, true) {
		a.logf.Warnf("googlechat: activity cards refused in %s (%v); drawing tool activity as text from now on", where, err)
	}
}

// activityStatusIcon maps an activity's status onto its Material icon and
// reports whether the card has settled (no further progress patches expected).
func activityStatusIcon(s chat.StepStatus) (icon string, settled bool) {
	switch s {
	case chat.StepDone, chat.StepUnfinished:
		return iconToolOK, true
	case chat.StepFailed:
		return iconToolFail, true
	default:
		return iconProgress, false
	}
}

// activityCard renders an Activity as a single-section Google Chat card, or nil
// when a is nil. If an unsplit burst pushes a card past maxCardWidgets or
// maxCardBytes, excess lines are elided inside the card rather than returning
// nil — activity cards are finalised via Update (messages.patch), where a nil
// card would overwrite the folded 1-line card with a 4,096-byte truncated wall
// of plain text.
//
// Every string on Activity is plain text — often agent-supplied tool names and
// arguments — so every field goes through htmlEscaper, never toCardHTML.
func activityCard(a *chat.Activity) *chatv1.GoogleAppsCardV1Card {
	if a == nil {
		return nil
	}
	icon, settled := activityStatusIcon(a.Status)
	title := clampRunes(a.Title, maxActivityTitle)
	if title == "" {
		title = "?"
	}
	head := &chatv1.GoogleAppsCardV1Widget{
		DecoratedText: &chatv1.GoogleAppsCardV1DecoratedText{
			StartIcon: &chatv1.GoogleAppsCardV1Icon{
				MaterialIcon: &chatv1.GoogleAppsCardV1MaterialIcon{Name: icon},
			},
			Text:     "<b>" + htmlEscaper.Replace(title) + "</b>",
			WrapText: true,
		},
	}

	var lines []string
	if !settled {
		if len(a.Recent) > 0 {
			lines = make([]string, len(a.Recent))
			for i, s := range a.Recent {
				lines[i] = stepLineHTML(s, false)
			}
		}
	} else if len(a.Steps) > 0 {
		for _, s := range a.Steps {
			lines = append(lines, stepLineHTML(s, false))
			for _, sub := range s.Steps {
				lines = append(lines, stepLineHTML(sub, true))
			}
		}
	}
	if card := buildActivityCard(head, settled, lines); card != nil {
		return card
	}
	if len(lines) == 0 {
		return nil
	}
	if !settled {
		lo, hi := 1, len(lines)-1
		var best *chatv1.GoogleAppsCardV1Card
		for lo <= hi {
			mid := (lo + hi) / 2
			if cand := buildActivityCard(head, false, lines[len(lines)-mid:]); cand != nil {
				best = cand
				lo = mid + 1
			} else {
				hi = mid - 1
			}
		}
		if best != nil {
			return best
		}
		return buildActivityCard(head, false, nil)
	}
	lo, hi := 0, len(lines)-1
	var best *chatv1.GoogleAppsCardV1Card
	for lo <= hi {
		mid := (lo + hi) / 2
		trimmed := append(slices.Clone(lines[:mid]), elidedCallsLine(len(lines)-mid))
		if cand := buildActivityCard(head, true, trimmed); cand != nil {
			best = cand
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	if best != nil {
		return best
	}
	return buildActivityCard(head, false, nil)
}

func elidedCallsLine(omitted int) string {
	if omitted == 1 {
		return topStepBullet + "<i>… and 1 more</i>"
	}
	return fmt.Sprintf("%s<i>… and %d more</i>", topStepBullet, omitted)
}

func buildActivityCard(head *chatv1.GoogleAppsCardV1Widget, collapsible bool, lines []string) *chatv1.GoogleAppsCardV1Card {
	sec := &chatv1.GoogleAppsCardV1Section{
		Widgets: []*chatv1.GoogleAppsCardV1Widget{head},
	}
	if len(lines) > 0 {
		if collapsible {
			sec.Collapsible = true
			sec.UncollapsibleWidgetsCount = 1
		}
		sec.Widgets = append(sec.Widgets, batchLineWidgets(lines)...)
	}
	if len(sec.Widgets) > maxCardWidgets {
		return nil
	}
	card := &chatv1.GoogleAppsCardV1Card{
		Sections: []*chatv1.GoogleAppsCardV1Section{sec},
	}
	if cardBytes(card) > maxCardBytes {
		return nil
	}
	return card
}

// stepLineHTML renders one step as one HTML line for a TextParagraph widget:
// bullet prefix, verdict mark, bold escaped tool title, optional delegation
// call count, optional duration/note, and optional monospace <tt> argument.
// The returned string never exceeds maxWidgetText bytes.
func stepLineHTML(s chat.Step, child bool) string {
	prefix := topStepBullet
	if child {
		prefix = childStepBullet
	}
	mark := lineRunning
	switch s.Status {
	case chat.StepDone:
		mark = lineDone
	case chat.StepFailed:
		mark = lineFailed
	case chat.StepUnfinished:
		mark = lineUnfinished
	}
	title := clampRunes(s.Title, maxActivityTitle)
	if title == "" {
		title = "?"
	}

	var b strings.Builder
	b.WriteString(prefix)
	b.WriteString(mark)
	b.WriteString(" <b>")
	b.WriteString(htmlEscaper.Replace(title))
	b.WriteString("</b>")
	switch {
	case s.Calls == 1:
		b.WriteString(" · 1 call")
	case s.Calls > 1:
		b.WriteString(" · ")
		b.WriteString(strconv.Itoa(s.Calls))
		b.WriteString(" calls")
	}
	if s.Note != "" {
		b.WriteString("  ")
		b.WriteString(htmlEscaper.Replace(clampRunes(s.Note, maxActivityTitle)))
	}
	if s.Code != "" {
		const openTag, closeTag = "  <tt>", "</tt>"
		budget := maxWidgetText - b.Len() - len(openTag) - len(closeTag)
		if escaped := escapedCodeWithin(s.Code, maxActivityCode, budget); escaped != "" {
			b.WriteString(openTag)
			b.WriteString(escaped)
			b.WriteString(closeTag)
		}
	}
	return b.String()
}

// escapedCodeWithin rune-clamps code to at most maxRunes and HTML-escapes it,
// shrinking the rune count if entity expansion pushes the escaped form past
// maxBytes so a single rendered line always fits one widget without cutting
// inside a rune or an HTML entity.
func escapedCodeWithin(code string, maxRunes, maxBytes int) string {
	if maxBytes <= 0 || maxRunes <= 0 {
		return ""
	}
	clamped := clampRunes(code, maxRunes)
	escaped := htmlEscaper.Replace(clamped)
	if len(escaped) <= maxBytes {
		return escaped
	}
	runes := []rune(code)
	clampSlice := func(n int) string {
		if len(runes) <= n {
			return code
		}
		if n == 1 {
			return string(runes[:1])
		}
		return string(runes[:n-1]) + "…"
	}
	lo, hi := 1, min(len(runes), maxRunes)
	best := ""
	for lo <= hi {
		mid := (lo + hi) / 2
		cand := htmlEscaper.Replace(clampSlice(mid))
		if len(cand) <= maxBytes {
			best = cand
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return best
}

// batchLineWidgets packs HTML lines into <br>-joined TextParagraph widgets of
// at most maxWidgetText bytes each, splitting only between lines so a tag or
// an entity is never cut in half.
func batchLineWidgets(lines []string) []*chatv1.GoogleAppsCardV1Widget {
	if len(lines) == 0 {
		return nil
	}
	var (
		widgets []*chatv1.GoogleAppsCardV1Widget
		cur     strings.Builder
	)
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		widgets = append(widgets, &chatv1.GoogleAppsCardV1Widget{
			TextParagraph: &chatv1.GoogleAppsCardV1TextParagraph{Text: cur.String()},
		})
		cur.Reset()
	}
	for _, line := range lines {
		if cur.Len() > 0 && cur.Len()+len("<br>")+len(line) > maxWidgetText {
			flush()
		}
		if cur.Len() > 0 {
			cur.WriteString("<br>")
		}
		cur.WriteString(line)
	}
	flush()
	return widgets
}
