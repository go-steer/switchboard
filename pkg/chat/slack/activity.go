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

package slack

import (
	"strconv"

	"github.com/go-steer/switchboard/pkg/chat"
)

// Slack's task_card block, as measured against the API from a plain bot (no
// assistant view, no agent scopes) on 2026-10-21 and again on 2026-11-19: it
// posts, it edits in place with chat.update, and it draws folded to one line
// by default — a status icon, the title, and a chevron that opens its details.
// Its status takes in_progress (a spinner), complete (a check) and error (a
// warning triangle); "pending" and "not_started" are refused with
// invalid_blocks, which is why chat.StepStatus has no state for a step that
// has not begun. A card left in_progress spins for good, so the router never
// leaves one in that state.
//
// details is rich text. It took 800 list lines and, separately, 32,000
// characters with no ceiling found; a rich_text_list at indent 1 draws nested
// under one at indent 0; bold and code styles on text elements draw; and a
// text element is literal — no mention, link or markup is read out of it.
const (
	taskInProgress = "in_progress"
	taskComplete   = "complete"
	taskError      = "error"

	// taskID is the card's task_id. A message holds one card, so one name
	// does for every message.
	taskID = "turn"

	// taskBlockID is the card's block_id, the same on every render. Measured
	// on 2026-11-19: a card a reader has opened snaps shut on every
	// chat.update unless the block carries the block_id it had before, and
	// with one it stays open while it updates. It has to be unique within a
	// message and identical across that message's edits, and a message holds
	// one card, so one constant does for every message — a card that has
	// rolled over or been split by narration included.
	taskBlockID = "sb-activity"

	// recentBlockID is the block_id of the lines drawn under the card
	// (Activity.Recent), fixed for the same reason: in the same test the card
	// above stayed open across edits when both blocks carried fixed ids.
	recentBlockID = "sb-activity-recent"

	// maxTaskTitle bounds the card's title and each line's. Slack accepted
	// 300 characters when measured; 256 is kube-agents' limit for the same
	// block, kept here so a change on Slack's side meets the more
	// conservative of the two.
	maxTaskTitle = 256
	// maxTaskCode bounds the argument on a line. The router's summary is
	// already one short line; this is the belt for a future one that is not.
	maxTaskCode = 2000
	// maxTaskLines is the most lines a card's details draw: what was measured
	// to post. Past it the reply goes as text, whole, rather than as a card
	// with some lines quietly missing. A belt and not the mechanism — the
	// router starts a new message long before a card gets here.
	maxTaskLines = 800
)

// taskStatus maps an activity's state onto task_card's vocabulary. Unfinished
// is not a state the router gives an activity; it is here so that one never
// falls through to in_progress, the one status that draws something moving.
var taskStatus = map[chat.StepStatus]string{
	chat.StepRunning:    taskInProgress,
	chat.StepDone:       taskComplete,
	chat.StepFailed:     taskError,
	chat.StepUnfinished: taskComplete,
}

// The marks a line leads with, by its step's state. The check and the cross
// carry the text-style variation selector (U+FE0E): without it Slack printed
// a bare U+2714 as the literal ":heavy_check_mark:" (measured). A step no
// result was seen for (chat.StepUnfinished) gets an en dash: neither of
// those, and not the triangle of something still running.
const (
	lineRunning    = "▸"
	lineDone       = "✔\uFE0E"
	lineFailed     = "✖\uFE0E"
	lineUnfinished = "–"
)

// activityBlocks renders an Activity as one task_card: its title and status on
// the card, its steps as the lines of the card's details. A card draws folded
// to its title, so the lines an Activity wants seen without a click (Recent)
// go in a second block directly under it: one flat bullet list, each line in
// the order a card's line is. With no Recent the message is the card alone.
//
// nil when there is nothing to draw, or more than a card is known to hold,
// which sends the reply down the text path — Text says the same thing.
func activityBlocks(a *chat.Activity) []map[string]any {
	if a == nil || activityLines(a) > maxTaskLines {
		return nil
	}
	status, ok := taskStatus[a.Status]
	if !ok {
		status = taskInProgress
	}
	title := clampRunes(a.Title, maxTaskTitle)
	if title == "" {
		title = "?"
	}
	card := map[string]any{
		"type":     "task_card",
		"block_id": taskBlockID,
		"task_id":  taskID,
		"title":    title,
		"status":   status,
	}
	if lists := stepLists(a.Steps); len(lists) > 0 {
		card["details"] = map[string]any{"type": "rich_text", "elements": lists}
	}
	blocks := []map[string]any{card}
	if len(a.Recent) > 0 {
		lines := make([]any, len(a.Recent))
		for i, s := range a.Recent {
			lines[i] = stepLine(s)
		}
		blocks = append(blocks, map[string]any{
			"type":     "rich_text",
			"block_id": recentBlockID,
			"elements": []any{map[string]any{
				"type":     "rich_text_list",
				"style":    "bullet",
				"indent":   0,
				"elements": lines,
			}},
		})
	}
	return blocks
}

// activityLines is how many lines an Activity draws: one per step and one per
// step under it.
func activityLines(a *chat.Activity) int {
	n := len(a.Steps)
	for _, s := range a.Steps {
		n += len(s.Steps)
	}
	return n
}

// stepLists lays the steps out as bullet lists: the steps themselves at indent
// 0, and a step's own steps as a list at indent 1 directly after it, with the
// steps that follow resuming in a new list at indent 0. One level, as
// chat.Step says: a nested step's own steps are not drawn.
func stepLists(steps []chat.Step) []any {
	var lists []any
	var run []any
	flush := func(indent int) {
		if len(run) == 0 {
			return
		}
		lists = append(lists, map[string]any{
			"type":     "rich_text_list",
			"style":    "bullet",
			"indent":   indent,
			"elements": run,
		})
		run = nil
	}
	for _, s := range steps {
		run = append(run, stepLine(s))
		if len(s.Steps) == 0 {
			continue
		}
		flush(0)
		for _, sub := range s.Steps {
			run = append(run, stepLine(sub))
		}
		flush(1)
	}
	flush(0)
	return lists
}

// stepLine is one step as one line: its mark, its title in bold, then — each
// only when there is one — how many calls were made under it, its note, and
// its argument as code.
//
// Every element is a rich-text text element, which Slack shows literally: an
// argument the agent chose, or a tool name it supplied, cannot become a
// mention, a link or formatting.
func stepLine(s chat.Step) map[string]any {
	mark := lineRunning
	switch s.Status {
	case chat.StepDone:
		mark = lineDone
	case chat.StepFailed:
		mark = lineFailed
	case chat.StepUnfinished:
		mark = lineUnfinished
	}
	title := clampRunes(s.Title, maxTaskTitle)
	if title == "" {
		title = "?"
	}
	els := []any{
		map[string]any{"type": "text", "text": mark + " "},
		map[string]any{"type": "text", "text": title, "style": map[string]any{"bold": true}},
	}
	var plain string
	switch {
	case s.Calls == 1:
		plain = " · 1 call"
	case s.Calls > 1:
		plain = " · " + strconv.Itoa(s.Calls) + " calls"
	}
	if s.Note != "" {
		plain += "  " + clampRunes(s.Note, maxTaskTitle)
	}
	if s.Code != "" {
		plain += "  "
	}
	if plain != "" {
		els = append(els, map[string]any{"type": "text", "text": plain})
	}
	if s.Code != "" {
		els = append(els, map[string]any{"type": "text", "text": clampRunes(s.Code, maxTaskCode), "style": map[string]any{"code": true}})
	}
	return map[string]any{"type": "rich_text_section", "elements": els}
}

// DrawsActivity implements chat.DrawsActivity: every reply's Activity is drawn
// as a task card, RichBlocks or not — until Slack has refused one.
func (a *Adapter) DrawsActivity() bool { return !a.noCards.Load() }

// cardBlocks is activityBlocks for a reply about to go out, or nil once Slack
// has refused task cards.
func (a *Adapter) cardBlocks(r chat.Reply) []map[string]any {
	if a.noCards.Load() {
		return nil
	}
	return sanitizeBlocks(activityBlocks(r.Activity))
}

// refuseCards stops drawing task cards for the rest of the process. Slack
// refusing a block type is a property of the workspace, not of one message —
// the card is the same shape every time — and a turn edits its card every few
// seconds: carrying on would cost every one of those edits a refused call
// before the text call that lands. The text says what the card did; the
// router reads DrawsActivity and goes back to what it does for an adapter
// that draws none.
func (a *Adapter) refuseCards(conv string, err error) {
	if a.noCards.CompareAndSwap(false, true) {
		a.logf.Warnf("slack: task cards refused in %s (%v); drawing tool activity as text from now on", conv, err)
	}
}
