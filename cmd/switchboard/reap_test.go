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

func TestResolveIdleTTL(t *testing.T) {
	for _, tc := range []struct {
		flag, stateDir string
		want           time.Duration
		wantErr        string
	}{
		{"", "", 0, ""},
		{"", "/var/lib/switchboard", defaultIdleTTL, ""},
		{"30m", "/var/lib/switchboard", 30 * time.Minute, ""},
		{"0", "/var/lib/switchboard", 0, ""},
		{"0", "", 0, ""},
		// Asked for, and not safe to do: refused, not ignored.
		{"1h", "", 0, "needs --state-dir"},
		{"soon", "/x", 0, "invalid --session-idle-ttl"},
		{"-1h", "/x", 0, "invalid --session-idle-ttl"},
	} {
		got, err := resolveIdleTTL(tc.flag, tc.stateDir)
		switch {
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("resolveIdleTTL(%q, %q) err = %v, want %q", tc.flag, tc.stateDir, err, tc.wantErr)
		case tc.wantErr == "" && (err != nil || got != tc.want):
			t.Errorf("resolveIdleTTL(%q, %q) = %v, %v; want %v", tc.flag, tc.stateDir, got, err, tc.want)
		}
	}
}

func TestReapEveryIsBounded(t *testing.T) {
	if got := reapEvery(12 * time.Hour); got != 5*time.Minute {
		t.Errorf("reapEvery(12h) = %v, want capped at 5m", got)
	}
	if got := reapEvery(time.Minute); got != 15*time.Second {
		t.Errorf("reapEvery(1m) = %v, want a quarter", got)
	}
	if got := reapEvery(time.Millisecond); got != time.Second {
		t.Errorf("reapEvery(1ms) = %v, want floored at 1s", got)
	}
}

// TestAnIdleConversationIsReleasedAndReattaches is #87's done-when: an idle
// conversation's stream is released without a restart, and its next message
// continues the same session rather than opening a new one.
func TestAnIdleConversationIsReleasedAndReattaches(t *testing.T) {
	d := &boundDaemon{resumed: make(chan string, 16)}
	r, _ := boundRouter(t, d)
	r.setIdleTTL(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	msg := chat.Message{Conversation: "C0:1", Channel: "C0", Caller: "alice@example.com", Text: "hi"}
	if err := r.Handle(ctx, msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	<-d.resumed
	r.mu.Lock()
	e := r.sessions["C0:1"]
	r.mu.Unlock()
	e.endTurn() // the turn is over; only the clock keeps it live now

	if n := r.reapIdle(time.Now()); n != 0 {
		t.Fatalf("reaped %d conversation(s) the moment they spoke", n)
	}
	if n := r.reapIdle(time.Now().Add(2 * time.Hour)); n != 1 {
		t.Fatalf("reaped %d, want the idle conversation released", n)
	}
	r.mu.Lock()
	_, live := r.sessions["C0:1"]
	rec, dormant := r.dormant["C0:1"]
	r.mu.Unlock()
	if live || !dormant || rec.Session != "core-agent/fresh" || rec.Owner != "alice@example.com" {
		t.Fatalf("after reaping: live=%v dormant=%v rec=%+v; want it dormant with its session and owner", live, dormant, rec)
	}
	select {
	case <-e.ready:
	default:
	}
	// The relay's context is what the reaper cancels; the stream it held
	// goes with it.
	if e.stop == nil {
		t.Fatal("entry has no stop")
	}

	if err := r.Handle(ctx, msg); err != nil {
		t.Fatalf("Handle after reaping: %v", err)
	}
	if got := d.creates.Load(); got != 1 {
		t.Errorf("creates = %d, want 1: the reaped conversation continued its session", got)
	}
	select {
	case <-d.resumed:
	case <-time.After(2 * time.Second):
		t.Fatal("the re-attached conversation never resubscribed")
	}
}

// TestABusyConversationIsNeverReaped: a turn in flight is not idle whatever
// the clock says — the agent may be thinking, and releasing the stream would
// drop its answer.
func TestABusyConversationIsNeverReaped(t *testing.T) {
	d := &boundDaemon{resumed: make(chan string, 16)}
	r, _ := boundRouter(t, d)
	r.setIdleTTL(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Handle(ctx, chat.Message{Conversation: "C0:1", Caller: "alice@example.com", Text: "long one"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// Handle leaves the turn in flight until the daemon says it is over.
	if n := r.reapIdle(time.Now().Add(48 * time.Hour)); n != 0 {
		t.Errorf("reaped %d conversation(s) with a turn in flight", n)
	}
}

// TestAPressRevivesAnIdleConversation: a question asked before its
// conversation went idle is still that conversation's question, so a press
// re-attaches it rather than being told the session is gone.
func TestAPressRevivesAnIdleConversation(t *testing.T) {
	d := newPermsDaemon(t)
	r, fake, _ := permsRouter(t, d)
	r.mu.Lock()
	r.dormant["C1:1"] = sessionRecord{Channel: "C1", Session: sessionRef(testSession), Owner: "alice@example.com",
		Touched: time.Now().Add(-48 * time.Hour)}
	r.mu.Unlock()

	p := chat.Press{
		Conversation: "C1:1", Channel: "C1", Caller: "presser@example.com",
		DecisionID: ref("pr1"), Option: "allow-once",
		Message: chat.MessageRef{Conversation: "C1:1", ID: "ts1"},
	}
	// The revived relay runs on the press's context, as every relay runs on
	// the context it was opened under; cancelled before the server closes.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.HandlePress(ctx, p); err != nil {
		t.Fatalf("HandlePress: %v", err)
	}
	if n := len(d.posts()); n != 1 {
		t.Fatalf("posts = %d, want the press answered on the revived session", n)
	}
	select {
	case rep := <-fake.replies:
		if rep.Text == noticeStalePress {
			t.Errorf("the press was called stale")
		}
	default:
	}
	r.mu.Lock()
	_, live := r.sessions["C1:1"]
	r.mu.Unlock()
	if !live {
		t.Error("the conversation was not re-attached")
	}
}

// TestAPressNeverOpensASession: with nothing live or dormant, a press is
// stale — it must not create a session to answer the question as a stranger.
func TestAPressNeverOpensASession(t *testing.T) {
	d := newPermsDaemon(t)
	r, fake, _ := permsRouter(t, d)
	p := chat.Press{Conversation: "C1:1", Channel: "C1", Caller: "presser@example.com",
		DecisionID: ref("pr1"), Option: "allow-once"}
	if err := r.HandlePress(context.Background(), p); err != nil {
		t.Fatalf("HandlePress: %v", err)
	}
	if got := drainNotice(t, fake); got != noticeStalePress {
		t.Errorf("the thread was told %q, want %q", got, noticeStalePress)
	}
	r.mu.Lock()
	n := len(r.sessions)
	r.mu.Unlock()
	if n != 0 {
		t.Errorf("sessions = %d; a press opened one", n)
	}
}

// TestAnAdoptedSessionIsNeverReaped: an unattended agent posting into its
// thread has no human turn to keep the entry live, and releasing its relay
// would silently stop its output reaching the thread.
func TestAnAdoptedSessionIsNeverReaped(t *testing.T) {
	d := &boundDaemon{resumed: make(chan string, 16)}
	r, _ := boundRouter(t, d)
	r.setIdleTTL(time.Hour)
	r.CommitBind("C0:1", mustSession(t, boundSession), 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Handle(ctx, chat.Message{Conversation: "C0:1", Caller: "alice@example.com", Text: "hi"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	r.mu.Lock()
	e := r.sessions["C0:1"]
	r.mu.Unlock()
	e.endTurn()
	if n := r.reapIdle(time.Now().Add(48 * time.Hour)); n != 0 {
		t.Errorf("reaped %d adopted conversation(s)", n)
	}
}

// TestReapingForgetsMonthOldDormantRecords: the record does not grow with
// every thread ever touched.
func TestReapingForgetsMonthOldDormantRecords(t *testing.T) {
	r, _ := boundRouter(t, &boundDaemon{})
	r.setIdleTTL(time.Hour)
	now := time.Now()
	r.mu.Lock()
	r.dormant["old"] = sessionRecord{Session: "core-agent/old", Touched: now.Add(-dormantMaxAge - time.Hour)}
	r.dormant["recent"] = sessionRecord{Session: "core-agent/recent", Touched: now.Add(-24 * time.Hour)}
	r.mu.Unlock()
	r.reapIdle(now)
	r.mu.Lock()
	_, old := r.dormant["old"]
	_, recent := r.dormant["recent"]
	r.mu.Unlock()
	if old || !recent {
		t.Errorf("dormant old=%v recent=%v; want the month-old record forgotten and the recent one kept", old, recent)
	}
}
