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
	"testing"
	"time"

	"github.com/go-steer/switchboard/pkg/chat"
	"github.com/go-steer/switchboard/pkg/daemon"
)

// TestBacklogReanchorSurvivesTheDelay is #42's second case under the delayed
// re-anchor and core-agent 2.10's order: the answer arrives ahead of its
// boundary (so it may still be narration, and its re-anchor waits), and the
// queued message is taken up almost at once, starting the next turn on the
// same placeholder. The wait must not drop the move: the next turn's clock
// still ends up below the answer (caught in review).
func TestBacklogReanchorSurvivesTheDelay(t *testing.T) {
	feed := make(chan frame)
	dc := fedDaemon(t, feed)
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	r, ctx := narrationRouter(t, dc, fake, 5*time.Millisecond)
	r.reanchorDelay = 200 * time.Millisecond

	handleOne(t, r, ctx, fake)
	e := entryFor(t, r)
	feed <- frame{daemon.EventInbox, backlogQueued}
	waitFor(t, e.backlogged, "the queued inbox event did not reach the backlog")

	feed <- frame{daemon.EventAgent, backlogAnswer1} // answer first…
	if got := recvReply(t, fake.replies).Text; got != "first answer" {
		t.Fatalf("second post = %q, want the first answer", got)
	}
	time.Sleep(20 * time.Millisecond)
	feed <- frame{daemon.EventInbox, backlogDequeued} // …the next turn taken up at once…
	feed <- frame{daemon.EventTurnComplete, backlogComplete}

	moved := recvReply(t, fake.replies) // …and the clock still moves below the answer
	if moved.Kind != chat.KindProgress {
		t.Fatalf("third post = %q (%v), want the re-anchored placeholder", moved.Text, moved.Kind)
	}
	if got := progressID(e); got == "ts1" {
		t.Errorf("progress still on ts1, above the answer")
	}
}
