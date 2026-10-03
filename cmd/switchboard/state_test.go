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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/switchboard/pkg/chat"
)

func TestFileStoreRoundTrips(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s, err := openFileStore(dir)
	if err != nil {
		t.Fatalf("openFileStore: %v", err)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("state dir mode = %v, %v; want 0700 — it names who spoke in every thread", fi.Mode(), err)
	}
	empty, err := s.load()
	if err != nil || len(empty.Sessions) != 0 || empty.Version != stateVersion {
		t.Fatalf("load before any save = %+v, %v; want an empty first-run state", empty, err)
	}

	touched := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	want := routerState{
		Version: stateVersion,
		Sessions: map[string]sessionRecord{"C0:1": {
			Channel: "C0", Session: "core-agent/s1", Owner: "alice@example.com",
			Relayed: 7, Noticed: 9, Touched: touched,
		}},
		Bindings:  []bindingRecord{{Conversation: "C0:2", Session: "core-agent/incident-7", Since: 5}},
		Overrides: map[string]string{"C0": "stream"},
	}
	if err := s.save(want); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(s.path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the temporary file survived the rename: %v", err)
	}
	got, err := s.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Sessions["C0:1"] != want.Sessions["C0:1"] || len(got.Bindings) != 1 || got.Bindings[0] != want.Bindings[0] ||
		got.Overrides["C0"] != "stream" {
		t.Errorf("load = %+v\nwant   %+v", got, want)
	}
}

// TestFileStoreRefusesWhatItCannotRead: a corrupt file or one from another
// format is an error rather than a guess, and setAside moves it out of the way
// so the run can start without destroying it.
func TestFileStoreRefusesWhatItCannotRead(t *testing.T) {
	for name, body := range map[string]string{
		"corrupt":       `{"version": 1, "sessions": {`,
		"other version": `{"version": 99}`,
	} {
		t.Run(name, func(t *testing.T) {
			s, err := openFileStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(s.path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := s.load(); err == nil {
				t.Fatal("load accepted a file it cannot read")
			}
			moved, err := s.setAside(time.Now())
			if err != nil {
				t.Fatalf("setAside: %v", err)
			}
			if raw, err := os.ReadFile(moved); err != nil || string(raw) != body {
				t.Errorf("set-aside file = %q, %v; want the original kept", raw, err)
			}
			if st, err := s.load(); err != nil || len(st.Sessions) != 0 {
				t.Errorf("load after setAside = %+v, %v; want a clean start", st, err)
			}
		})
	}
}

// TestARestartReattachesTheThreadToItsSession is #86's done-when: after a
// restart, the next message in a thread goes to the session it had — no new
// session — and the relay picks up after the last answer it delivered,
// neither replaying it nor skipping what came after.
func TestARestartReattachesTheThreadToItsSession(t *testing.T) {
	d := &boundDaemon{resumed: make(chan string, 16)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()

	// The first process: one turn, one answer delivered at seq 3.
	first, firstOut := boundRouter(t, d)
	s1, err := openFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	first.setStore(s1)
	msg := chat.Message{Conversation: "C0:1", Channel: "C0", Caller: "alice@example.com", Text: "status?"}
	if err := first.Handle(ctx, msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	<-d.resumed
	d.publish(agentFrame(3, "all green"))
	select {
	case <-firstOut.replies:
	case <-time.After(2 * time.Second):
		t.Fatal("the first process never delivered its answer")
	}
	waitForState(t, first, "C0:1", 3)
	first.saveState()

	// While it is down, the agent says one more thing.
	d.publish(agentFrame(5, "and the canary passed"))

	// The second process restores and re-attaches the recent thread at once.
	second, secondOut := boundRouter(t, d)
	s2, err := openFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := s2.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	second.setStore(s2)
	revived, dormant := second.restore(ctx, st, time.Now())
	if revived != 1 || dormant != 0 {
		t.Fatalf("restore = %d revived, %d dormant; want 1 and 0", revived, dormant)
	}
	select {
	case since := <-d.resumed:
		if since != "3" {
			t.Errorf("the re-attached relay resumed from %s, want 3 — after the last answer delivered", since)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the restored thread never re-attached")
	}
	select {
	case rep := <-secondOut.replies:
		if !strings.Contains(rep.Text, "canary") {
			t.Errorf("first reply after restart = %q, want the answer produced while down", rep.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the answer produced while switchboard was down was never delivered")
	}
	select {
	case rep := <-secondOut.replies:
		t.Errorf("a second reply arrived: %q; the answer delivered before the restart was replayed", rep.Text)
	case <-time.After(100 * time.Millisecond):
	}

	if err := second.Handle(ctx, msg); err != nil {
		t.Fatalf("Handle after restart: %v", err)
	}
	if got := d.creates.Load(); got != 1 {
		t.Errorf("creates = %d, want 1: the thread kept its session across the restart", got)
	}
}

// waitForState polls until the router's snapshot shows conv delivered to seq.
func waitForState(t *testing.T, r *Router, conv string, seq int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for r.snapshot().Sessions[conv].Relayed < seq {
		if time.Now().After(deadline) {
			t.Fatalf("snapshot never showed %s delivered to %d: %+v", conv, seq, r.snapshot().Sessions[conv])
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestAnOldConversationWaitsForItsNextMessage: a thread quiet for longer than
// the revive window is not reconnected at boot — no stream for every thread a
// workspace ever touched — but its next message still finds its session.
func TestAnOldConversationWaitsForItsNextMessage(t *testing.T) {
	d := &boundDaemon{resumed: make(chan string, 16)}
	r, _ := boundRouter(t, d)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st := routerState{Version: stateVersion, Sessions: map[string]sessionRecord{"C0:1": {
		Channel: "C0", Session: "core-agent/old", Owner: "alice@example.com", Relayed: 4,
		Touched: time.Now().Add(-2 * reviveWindow),
	}}}
	if revived, dormant := r.restore(ctx, st, time.Now()); revived != 0 || dormant != 1 {
		t.Fatalf("restore = %d revived, %d dormant; want 0 and 1", revived, dormant)
	}
	select {
	case since := <-d.resumed:
		t.Fatalf("an old thread was reconnected at boot (since %s)", since)
	case <-time.After(100 * time.Millisecond):
	}

	if err := r.Handle(ctx, chat.Message{Conversation: "C0:1", Channel: "C0", Caller: "alice@example.com", Text: "back"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := d.creates.Load(); got != 0 {
		t.Errorf("creates = %d, want 0: the dormant session was revived, not replaced", got)
	}
	r.mu.Lock()
	e := r.sessions["C0:1"]
	r.mu.Unlock()
	if e == nil || sessionRef(e.sess) != "core-agent/old" || e.owner != "alice@example.com" {
		t.Fatalf("entry = %+v, want the recorded session and owner", e)
	}
}

// TestADormantConversationCannotBeBoundAway: a dormant record is the
// conversation's session as surely as a live entry is, so the outbound
// ingress may not bind the thread to some other session.
func TestADormantConversationCannotBeBoundAway(t *testing.T) {
	r, _ := boundRouter(t, &boundDaemon{})
	r.restore(context.Background(), routerState{Version: stateVersion, Sessions: map[string]sessionRecord{
		"C0:1": {Session: "core-agent/mine", Touched: time.Now().Add(-2 * reviveWindow)},
	}}, time.Now())

	if _, err := r.PrepareBind(context.Background(), "C0:1", mustSession(t, boundSession)); !errors.Is(err, errConversationBound) {
		t.Errorf("PrepareBind into a dormant conversation = %v, want errConversationBound", err)
	}
}

// TestRestoreBringsBackBindingsAndOverrides: the rest of the table — an
// adopted thread not yet replied to, a channel's runtime progress mode.
func TestRestoreBringsBackBindingsAndOverrides(t *testing.T) {
	r, _ := boundRouter(t, &boundDaemon{})
	r.restore(context.Background(), routerState{
		Version:   stateVersion,
		Bindings:  []bindingRecord{{Conversation: "C0:2", Session: boundSession, Since: 5}},
		Overrides: map[string]string{"C0": "stream", "C1": "not-a-mode"},
	}, time.Now())

	r.mu.Lock()
	b, ok := r.bindings["C0:2"]
	inverse := r.boundTo[boundSession]
	r.mu.Unlock()
	if !ok || sessionRef(b.sess) != boundSession || b.since != 5 || inverse != "C0:2" {
		t.Errorf("binding = %+v (%v), inverse %q; want it restored both ways", b, ok, inverse)
	}
	if got := r.progressFor("C0"); got != ProgressStream {
		t.Errorf("progressFor(C0) = %q, want the restored override", got)
	}
	if got := r.progressFor("C1"); got != ProgressOff {
		t.Errorf("progressFor(C1) = %q; an unreadable override must be dropped, not applied", got)
	}
}

// TestThePersisterWritesAfterAChange: a change is on disk within the debounce,
// without anyone calling saveState.
func TestThePersisterWritesAfterAChange(t *testing.T) {
	r, _ := boundRouter(t, &boundDaemon{})
	s, err := openFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r.setStore(s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.runPersister(ctx)

	r.setProgress("C0", ProgressStatus)
	deadline := time.Now().Add(3 * time.Second)
	for {
		st, err := s.load()
		if err == nil && st.Overrides["C0"] == string(ProgressStatus) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the change never reached disk: %+v, %v", st, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
