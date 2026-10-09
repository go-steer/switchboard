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
	"strings"
	"testing"
	"time"

	"github.com/go-steer/switchboard/pkg/chat"
)

// traceSender is a platform where deleting leaves a trace, as Google Chat's
// "Message deleted by its author" does.
type traceSender struct{ *fakeSender }

func (traceSender) DeleteLeavesTrace() bool { return true }

// Where a delete leaves a trace, the placeholder is not deleted when the
// answer lands: it is edited into a final "Done" line, so the thread shows
// what happened instead of a tombstone (reported from the GKE deployment).
// And narration does not move the clock, since a move is a post and a delete.
func TestThePlaceholderIsFinalisedNotDeletedWhereDeletesShow(t *testing.T) {
	narrate, answer := make(chan struct{}), make(chan struct{})
	dc := narrationDaemon(t, capsWithBoundary, narrate, answer)
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	r, ctx := narrationRouter(t, dc, traceSender{fake}, time.Hour)

	if got := handleOne(t, r, ctx, fake).Text; got != workingText {
		t.Fatalf("first post = %q, want the placeholder", got)
	}
	e := entryFor(t, r)
	rewindTurn(e, 12*time.Second)

	close(narrate)
	if got := recvReply(t, fake.replies).Text; got != "let me check the logs…" {
		t.Fatalf("second post = %q, want the narration", got)
	}
	close(answer)
	if got := recvReply(t, fake.replies).Text; got != "the answer" {
		t.Fatalf("third post = %q, want the answer — no re-anchored placeholder before it", got)
	}
	waitFor(t, func() bool {
		for _, u := range fake.updatedCalls() {
			if u.ref.ID == "ts1" && strings.HasPrefix(u.text, markOK+" Done · 12s") {
				return true
			}
		}
		return false
	}, "the placeholder was not finalised as done")
	if got := fake.deletedRefs(); len(got) != 0 {
		t.Errorf("deleted %v where a delete leaves a trace", got)
	}
	for _, u := range fake.updatedCalls() {
		if strings.HasPrefix(u.text, markOK) && u.reply.Kind != chat.KindActivity {
			t.Errorf("final line Kind = %v, want KindActivity (its mark picks the verdict icon)", u.reply.Kind)
		}
	}
}

// The final line says what happened: a turn that did not finish is stopped,
// not done.
func TestAFailedTurnsPlaceholderSaysStopped(t *testing.T) {
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	r := NewRouter(nil, traceSender{fake}, ProgressIndicator, nil, nil)
	e := liveEntryIn(r, "C1:1", "C1")
	ref, _ := fake.Send(context.Background(), chat.Reply{Conversation: "C1:1", Text: workingText, Kind: chat.KindProgress})
	recvReply(t, fake.replies)
	e.beginTurn(ref, time.Now().Add(-5*time.Second))

	r.failProgress(context.Background(), e, "C1:1")
	edits := fake.updatedCalls()
	if len(edits) != 1 || !strings.HasPrefix(edits[0].text, markFailed+" Stopped · 5s") {
		t.Errorf("edits = %+v, want the placeholder finalised as stopped", edits)
	}
	if len(fake.deletedRefs()) != 0 {
		t.Error("the placeholder was deleted")
	}
}

// A tick already inside its edit when the turn finishes must not land after
// the final line and put the clock back: before, the late tick hit a deleted
// message; finalised instead, it would overwrite "Done" (caught in review).
func TestALateTickCannotUndoTheFinalLine(t *testing.T) {
	fake := &fakeSender{replies: make(chan chat.Reply, 8), stallText: "Working", stallFor: 300 * time.Millisecond}
	r := NewRouter(nil, traceSender{fake}, ProgressIndicator, nil, nil)
	r.tickInterval = 20 * time.Millisecond
	e := liveEntryIn(r, "C1:1", "C1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r.startProgress(ctx, e, "C1:1")
	recvReply(t, fake.replies)        // the placeholder
	time.Sleep(60 * time.Millisecond) // a tick is now stalled inside its edit
	r.clearProgress(ctx, e, "C1:1")
	time.Sleep(400 * time.Millisecond) // past the stalled tick

	edits := fake.updatedCalls()
	if len(edits) == 0 {
		t.Fatal("no edits")
	}
	if last := edits[len(edits)-1].text; !strings.HasPrefix(last, markOK+" Done") {
		t.Errorf("last edit = %q, want the final line; edits: %d", last, len(edits))
	}
}
