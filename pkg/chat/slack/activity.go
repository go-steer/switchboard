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
// assistant view, no agent scopes) on 2026-10-21: it posts, it edits in place
// with chat.update, and it sits beside ordinary context and section blocks. Its
// status takes in_progress, complete and error; "pending" and "not_started" are
// refused with invalid_blocks, which is why chat.StepStatus has no state for a
// step that has not begun.
const (
	taskInProgress = "in_progress"
	taskComplete   = "complete"
	taskError      = "error"

	// maxTaskTitle bounds a card's title. Slack accepted 300 characters when
	// measured; 256 is kube-agents' limit for the same block, kept here so a
	// change on Slack's side meets the more conservative of the two.
	maxTaskTitle = 256
	// maxTaskCode bounds the argument in a card's details. The router's
	// summary is already one short line; this is the belt for a future one
	// that is not.
	maxTaskCode = 2000
	// maxTaskCards is the most steps drawn as cards. Past it the reply goes
	// as text: a message is fifty blocks at most, and a notice that would
	// need to drop steps to fit is better told whole in prose than partly in
	// cards.
	maxTaskCards = 40
)

// taskStatus maps a step's state onto task_card's vocabulary.
var taskStatus = map[chat.StepStatus]string{
	chat.StepRunning: taskInProgress,
	chat.StepDone:    taskComplete,
	chat.StepFailed:  taskError,
}

// activityBlocks renders an Activity as a context line for its head and one
// task_card per step. nil when there is nothing to draw, or more than a message
// holds, which sends the reply down the text path — Text says the same thing.
func activityBlocks(a *chat.Activity, mrkdwnFn func(string) string) []map[string]any {
	if a == nil || len(a.Steps) == 0 || len(a.Steps) > maxTaskCards {
		return nil
	}
	blocks := make([]map[string]any, 0, len(a.Steps)+1)
	if head := mrkdwnFn(a.Head); head != "" {
		blocks = append(blocks, map[string]any{
			"type":     "context",
			"elements": []any{map[string]any{"type": "mrkdwn", "text": clampRunes(head, maxSectionText)}},
		})
	}
	for i, s := range a.Steps {
		status, ok := taskStatus[s.Status]
		if !ok {
			status = taskInProgress
		}
		title := clampRunes(s.Title, maxTaskTitle)
		if title == "" {
			title = "?"
		}
		card := map[string]any{
			"type":    "task_card",
			"task_id": "step-" + strconv.Itoa(i+1),
			"title":   title,
			"status":  status,
		}
		// Rich text, not mrkdwn: a text element is literal, so an argument the
		// agent chose cannot become a mention, a link or formatting.
		var details []any
		if s.Code != "" {
			details = append(details, map[string]any{
				"type":     "rich_text_preformatted",
				"elements": []any{map[string]any{"type": "text", "text": clampRunes(s.Code, maxTaskCode)}},
			})
		}
		if s.Note != "" {
			details = append(details, map[string]any{
				"type":     "rich_text_section",
				"elements": []any{map[string]any{"type": "text", "text": clampRunes(s.Note, maxTaskTitle)}},
			})
		}
		if len(details) > 0 {
			card["details"] = map[string]any{"type": "rich_text", "elements": details}
		}
		blocks = append(blocks, card)
	}
	return blocks
}

// DrawsActivity implements chat.DrawsActivity: every reply's Activity is drawn
// as task cards, RichBlocks or not — until Slack has refused them once.
func (a *Adapter) DrawsActivity() bool { return !a.noCards.Load() }

// cardBlocks is activityBlocks for a reply about to go out, or nil once Slack
// has refused task cards.
func (a *Adapter) cardBlocks(r chat.Reply) []map[string]any {
	if a.noCards.Load() {
		return nil
	}
	return sanitizeBlocks(activityBlocks(r.Activity, toMrkdwn))
}

// refuseCards stops drawing task cards for the rest of the process. Slack
// refusing a block type is a property of the workspace, not of one message —
// the cards are the same shape every time — and a status turn edits its
// message every few seconds: carrying on would cost every one of those edits a
// refused call before the text call that lands. The text says what the cards
// did; the router reads DrawsActivity and stops making the edits only cards
// show.
func (a *Adapter) refuseCards(conv string, err error) {
	if a.noCards.CompareAndSwap(false, true) {
		a.logf.Warnf("slack: task cards refused in %s (%v); drawing tool activity as text from now on", conv, err)
	}
}
