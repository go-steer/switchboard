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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/switchboard/internal/logging"
	"github.com/go-steer/switchboard/pkg/approval"
	"github.com/go-steer/switchboard/pkg/chat"
)

// syncBuffer is a bytes.Buffer safe to read while the router writes to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// testAudit is an audit log into a buffer, stamped with a fixed time.
func testAudit() (*auditLog, *syncBuffer) {
	buf := &syncBuffer{}
	return &auditLog{
		w:    buf,
		dest: "test",
		logf: func(logging.Level, string, ...any) {},
		now:  func() time.Time { return time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC) },
	}, buf
}

// records parses every line written so far.
func records(t *testing.T, buf *syncBuffer) []auditRecord {
	t.Helper()
	var out []auditRecord
	sc := bufio.NewScanner(strings.NewReader(buf.String()))
	for sc.Scan() {
		var rec auditRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("audit line %q is not JSON: %v", sc.Text(), err)
		}
		out = append(out, rec)
	}
	return out
}

// TestATurnLeavesOneRecordAndNoText is #89's done-when: one record per turn,
// correlating the chat message, the asserted caller and the session — and the
// message text nowhere in it.
func TestATurnLeavesOneRecordAndNoText(t *testing.T) {
	router, _ := boundRouter(t, &boundDaemon{})
	audit, buf := testAudit()
	router.setAudit(audit)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	msg := chat.Message{
		Conversation: "C0:1", Channel: "C0", Caller: "alice@example.com",
		Text: "restart the payments deployment", MessageID: "1700000000.000100",
	}
	if err := router.Handle(ctx, msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	got := records(t, buf)
	if len(got) != 1 {
		t.Fatalf("records = %d, want 1", len(got))
	}
	want := auditRecord{
		Time: "2026-10-03T09:00:00Z", Kind: auditTurn, Conversation: "C0:1", Channel: "C0",
		Caller: "alice@example.com", Session: "core-agent/fresh", MessageID: "1700000000.000100",
		Outcome: auditInjected,
	}
	if got[0] != want {
		t.Errorf("record = %+v\nwant     %+v", got[0], want)
	}
	if strings.Contains(buf.String(), "payments") {
		t.Errorf("the message text reached the audit record: %s", buf)
	}
}

// TestAFailedTurnIsRecordedToo: the turns that did not go through are part of
// the trail — somebody still caused them.
func TestAFailedTurnIsRecordedToo(t *testing.T) {
	router, _ := boundRouter(t, &boundDaemon{injectStatus: http.StatusInternalServerError})
	audit, buf := testAudit()
	router.setAudit(audit)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = router.Handle(ctx, chat.Message{Conversation: "C0:1", Channel: "C0", Caller: "alice@example.com", Text: "hi"})

	got := records(t, buf)
	if len(got) != 1 || got[0].Outcome != auditInjectFailed || got[0].Status != http.StatusInternalServerError ||
		got[0].Session != "core-agent/fresh" {
		t.Fatalf("records = %+v, want one inject_failed with status 500 naming the session", got)
	}
}

// TestPressesAreRecordedWithTheirOutcome: an applied answer names the approver
// the daemon recorded; a refused one names what it was trying to answer.
func TestPressesAreRecordedWithTheirOutcome(t *testing.T) {
	d := newPermsDaemon(t)
	r, _, _ := permsRouter(t, d)
	r.setApprovers(mustApprovers(t, "presser@example.com"))
	audit, buf := testAudit()
	r.setAudit(audit)
	e := liveEntryIn(r, "C1:1", "C1")

	if err := r.HandlePress(context.Background(), pressIn(e, "C1", "pr1")); err != nil {
		t.Fatalf("HandlePress: %v", err)
	}
	refused := pressIn(e, "C1", "pr2")
	refused.Caller = "mallory@example.com"
	_ = r.HandlePress(context.Background(), refused)

	got := records(t, buf)
	if len(got) != 2 {
		t.Fatalf("records = %d, want 2", len(got))
	}
	if g := got[0]; g.Kind != auditPress || g.Outcome != auditApplied || g.Approver != "presser@example.com" ||
		g.Decision != string(approval.AllowOnce) || g.PromptID != "pr1" || g.MessageID != "ts1" || g.Session == "" {
		t.Errorf("applied press = %+v", g)
	}
	if g := got[1]; g.Outcome != auditNotApprover || g.Caller != "mallory@example.com" || g.PromptID != "pr2" || g.Approver != "" {
		t.Errorf("refused press = %+v", g)
	}
}

// TestOpenAuditLogAppends: a file sink is created owner-and-group readable and
// appended to, never truncated — a restart must not erase the trail.
func TestOpenAuditLogAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	if err := os.WriteFile(path, []byte(`{"earlier":true}`+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	l, err := openAuditLog(path, func(logging.Level, string, ...any) {})
	if err != nil {
		t.Fatalf("openAuditLog: %v", err)
	}
	l.record(auditRecord{Kind: auditTurn, Conversation: "C0:1", Outcome: auditInjected})
	if err := l.close(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if lines := strings.Split(strings.TrimSpace(string(raw)), "\n"); len(lines) != 2 || !strings.Contains(lines[0], "earlier") {
		t.Errorf("file = %q, want the earlier line kept and one appended", raw)
	}

	fresh := filepath.Join(t.TempDir(), "new.jsonl")
	l2, err := openAuditLog(fresh, func(logging.Level, string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	defer l2.close()
	if fi, err := os.Stat(fresh); err != nil || fi.Mode().Perm()&0o007 != 0 {
		t.Errorf("mode = %v, %v; every line names a person, so no world access", fi.Mode(), err)
	}

	if l, err := openAuditLog("", nil); l != nil || err != nil {
		t.Errorf(`openAuditLog("") = %v, %v; want off`, l, err)
	}
	var off *auditLog
	off.record(auditRecord{}) // a nil log records nothing and does not panic
}
