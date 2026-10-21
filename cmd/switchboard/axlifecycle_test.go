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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/switchboard/internal/axapi"
	"github.com/go-steer/switchboard/pkg/ax"
)

// quietFor makes conv's record dormant and untouched for d.
func quietFor(t *testing.T, f *axFixture, conv string, d time.Duration) {
	t.Helper()
	f.r.mu.Lock()
	defer f.r.mu.Unlock()
	e, live := f.r.sessions[conv]
	var rec sessionRecord
	if live {
		rec = e.record()
		delete(f.r.sessions, conv)
		if e.stop != nil {
			e.stop()
		}
	} else {
		rec = f.r.dormant[conv]
	}
	rec.Touched = time.Now().Add(-d)
	f.r.dormant[conv] = rec
}

// A thread's task is deleted once its conversation has been quiet past
// delete_after; an active one is kept; the next message in the quiet thread
// starts over in a fresh task and the thread is told why.
func TestAQuietThreadsTaskIsDeleted(t *testing.T) {
	f := newAXFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.say(t, ctx, "C1:1", "old")
	f.say(t, ctx, "C1:2", "recent")
	quietFor(t, f, "C1:1", 15*24*time.Hour)
	quietFor(t, f, "C1:2", time.Hour)

	if n := f.r.deleteQuietTasks(ctx, time.Now()); n != 1 {
		t.Fatalf("deleted %d, want the quiet thread's task", n)
	}
	old := ax.TaskName("sb-triage", "slack", "C1:1")
	f.server.mu.Lock()
	deletes := append([]string(nil), f.server.deletes...)
	f.server.mu.Unlock()
	if len(deletes) != 1 || deletes[0] != old {
		t.Errorf("deletes = %v, want %s", deletes, old)
	}
	if n := f.r.deleteQuietTasks(ctx, time.Now()); n != 0 {
		t.Errorf("deleted %d on a second sweep, want none", n)
	}

	f.say(t, ctx, "C1:1", "back")
	got := recvReply(t, f.out.replies)
	if !strings.Contains(got.Text, "cleaned up") {
		t.Errorf("notice = %q", got.Text)
	}
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	if !f.server.tasks[old] {
		t.Error("no fresh task for the thread that started over")
	}
}

// Only thread tasks: a personal or shared task holds more than one
// conversation, and delete_after 0 keeps tasks.
func TestOnlyThreadTasksAreDeletedAutomatically(t *testing.T) {
	for _, tc := range []struct {
		scope, deleteAfter string
	}{{axScopeOwner, ""}, {axScopeShared, ""}, {axScopeThread, "0"}} {
		f := newAXFixture(t)
		axc := *f.cfg.AX
		axc.DeleteAfter = tc.deleteAfter
		f.cfg.AX = &axc
		withScope(t, f, tc.scope)
		ctx, cancel := context.WithCancel(context.Background())
		f.say(t, ctx, "C1:1", "hi")
		quietFor(t, f, "C1:1", 29*24*time.Hour)
		if n := f.r.deleteQuietTasks(ctx, time.Now()); n != 0 {
			t.Errorf("scope %s, delete_after %q: deleted %d", tc.scope, tc.deleteAfter, n)
		}
		cancel()
	}
}

// delete_after past dormantMaxAge would forget the conversation, and with it
// the task to delete, first.
func TestDeleteAfterIsBounded(t *testing.T) {
	f := newAXFixture(t)
	for _, d := range []string{"697h", "720h", "1000h", "-1h", "soon"} {
		c := f.cfg
		axc := *c.AX
		axc.DeleteAfter = d
		c.AX = &axc
		if _, err := buildAgent(c, agentBuilder{ax: &axClients{}}); err == nil {
			t.Errorf("delete_after %s: accepted", d)
		}
	}
}

// A thread task made from an earlier template — an old token, say — is
// replaced; with replace_drifted false it is kept.
func TestADriftedThreadTaskIsReplaced(t *testing.T) {
	for _, replace := range []bool{true, false} {
		f := newAXFixture(t)
		axc := *f.cfg.AX
		axc.ReplaceDrifted = &replace
		f.cfg.AX = &axc
		withScope(t, f, axScopeThread)
		task := ax.TaskName("sb-triage", "slack", "C1:1")
		f.server.mu.Lock()
		f.server.tasks[task] = true
		f.server.specs = map[string]*axapi.TaskSpec{task: {Image: "an-older-image", Http: &axapi.TaskHTTP{Port: 8484}}}
		f.server.mu.Unlock()

		ctx, cancel := context.WithCancel(context.Background())
		f.say(t, ctx, "C1:1", "hi")
		cancel()
		f.server.mu.Lock()
		replaced := len(f.server.deletes) == 1 && f.server.specs[task].GetImage() == "agent-mast"
		f.server.mu.Unlock()
		if replaced != replace {
			t.Errorf("replace_drifted %v: replaced %v", replace, replaced)
		}
	}
}

// The admin API lists an agent's tasks with what switchboard knows of them,
// and deletes one — only one of the agent's own: the atespace holds other
// services' tasks too.
func TestTheAdminAPIListsAndDeletesTasks(t *testing.T) {
	f := newAXFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.say(t, ctx, "C1:1", "hi")
	task := ax.TaskName("sb-triage", "slack", "C1:1")
	f.server.mu.Lock()
	f.server.tasks["lookout-0123456789abcdef"] = true // someone else's
	f.server.mu.Unlock()

	admin := newRegistryAdmin(f.r, registryConfig{Base: f.r.agents()})
	al, buf := testAudit()
	s, err := newAdminServer(adminConfig{Addr: "127.0.0.1:0", Auth: adminAuthToken, Token: "t", Platform: "slack", Audit: al}, f.r, admin)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.h)
	defer srv.Close()
	do := func(method, path string) (int, string) {
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer t")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	code, body := do("GET", "/v1/agents/triage/tasks")
	var list struct{ Tasks []taskView }
	if err := json.Unmarshal([]byte(body), &list); code != 200 || err != nil {
		t.Fatalf("list: %d %s", code, body)
	}
	if len(list.Tasks) != 1 || list.Tasks[0].Task != task || len(list.Tasks[0].Conversations) != 1 || list.Tasks[0].Conversations[0] != "C1:1" {
		t.Errorf("tasks = %+v, want the thread's alone", list.Tasks)
	}
	if code, _ := do("DELETE", "/v1/agents/triage/tasks/lookout-0123456789abcdef"); code != http.StatusNotFound {
		t.Errorf("deleting another service's task: %d, want 404", code)
	}
	if code, body := do("DELETE", "/v1/agents/triage/tasks/"+task); code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", code, body)
	}
	f.r.mu.Lock()
	rec, dormant := f.r.dormant["C1:1"]
	_, live := f.r.sessions["C1:1"]
	f.r.mu.Unlock()
	if live || !dormant || !rec.TaskDeleted {
		t.Errorf("after the delete: live %v, record %+v; want the conversation released and marked", live, rec)
	}
	var audited bool
	for _, r := range records(t, buf) {
		if r.Kind == auditAdmin && r.Admin != nil && strings.HasPrefix(r.Admin.Change, "delete task ") {
			audited = true
		}
	}
	if !audited {
		t.Error("the task delete was not audited")
	}
}

// A conversation waking its task is not swept: the sweep deleted a task its
// conversation had just resumed, leaving it live on a deleted task (caught in
// review).
func TestAWakingConversationsTaskIsNotDeleted(t *testing.T) {
	f := newAXFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.say(t, ctx, "C1:1", "old")
	quietFor(t, f, "C1:1", 15*24*time.Hour)
	f.r.mu.Lock()
	f.r.sessions["C1:1"] = &sessionEntry{ready: make(chan struct{})} // reviving
	f.r.mu.Unlock()
	if n := f.r.deleteQuietTasks(ctx, time.Now()); n != 0 {
		t.Errorf("deleted %d under a waking conversation", n)
	}
}

// A task start waits out a deletion of its task in flight, rather than reuse
// a task about to vanish under it.
func TestATaskStartWaitsOutItsDeletion(t *testing.T) {
	f := newAXFixture(t)
	task := ax.TaskName("sb-triage", "slack", "C1:1")
	f.r.mu.Lock()
	done, ok := f.r.beginTaskDeleteLocked(task)
	f.r.mu.Unlock()
	if !ok {
		t.Fatal("no gate")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		f.say(t, ctx, "C1:1", "hi")
	}()
	time.Sleep(200 * time.Millisecond)
	if f.server.createCount() != 0 {
		t.Fatal("the task was started while its deletion was in flight")
	}
	done()
	<-finished
	if f.server.createCount() != 1 {
		t.Errorf("creates = %d after the deletion, want 1", f.server.createCount())
	}
}

// delete_after at its cap still deletes before the record is pruned (caught
// in review: at dormantMaxAge the prune always won).
func TestDeleteAfterAtItsCapStillDeletes(t *testing.T) {
	f := newAXFixture(t)
	axc := *f.cfg.AX
	axc.DeleteAfter = maxDeleteAfter.String()
	f.cfg.AX = &axc
	withScope(t, f, axScopeThread)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.say(t, ctx, "C1:1", "hi")
	quietFor(t, f, "C1:1", maxDeleteAfter+time.Hour)
	f.r.releaseIdleAX(time.Now()) // the 10s tick that prunes first
	if n := f.r.deleteQuietTasks(ctx, time.Now()); n != 1 {
		t.Errorf("deleted %d, want the task deleted before its record is pruned", n)
	}
}

// A drifted thread task is deleted before it is woken: one whose old spec
// cannot start would otherwise never be replaced (caught in review).
func TestADriftedTaskIsReplacedBeforeItIsWoken(t *testing.T) {
	f := newAXFixture(t)
	task := ax.TaskName("sb-triage", "slack", "C1:1")
	f.server.mu.Lock()
	f.server.tasks[task] = true
	f.server.specs = map[string]*axapi.TaskSpec{task: {Image: "a-removed-image", Http: &axapi.TaskHTTP{Port: 8484}}}
	f.server.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.say(t, ctx, "C1:1", "hi")
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	if len(f.server.ops) < 2 || f.server.ops[0] != "delete "+task {
		t.Errorf("ops = %v, want the delete before anything else", f.server.ops)
	}
}

// The admin listing says which tasks this gateway has no record of — an
// orphan or another gateway's — and deleting one takes force.
func TestAnUnrecordedTaskNeedsForce(t *testing.T) {
	f := newAXFixture(t)
	other := ax.TaskName("sb-triage", "googlechat", "spaces/A:spaces/A/threads/T")
	f.server.mu.Lock()
	f.server.tasks[other] = true
	f.server.mu.Unlock()
	admin := newRegistryAdmin(f.r, registryConfig{Base: f.r.agents()})
	s, err := newAdminServer(adminConfig{Addr: "127.0.0.1:0", Auth: adminAuthToken, Token: "t", Platform: "slack"}, f.r, admin)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.h)
	defer srv.Close()
	do := func(method, path string) (int, string) {
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer t")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if _, body := do("GET", "/v1/agents/triage/tasks"); !strings.Contains(body, `"recorded":false`) {
		t.Errorf("listing = %s, want the task marked unrecorded", body)
	}
	if code, _ := do("DELETE", "/v1/agents/triage/tasks/"+other); code != http.StatusConflict {
		t.Errorf("unforced delete: %d, want 409", code)
	}
	if code, _ := do("DELETE", "/v1/agents/triage/tasks/"+other+"?force=true"); code != http.StatusNoContent {
		t.Errorf("forced delete: %d, want 204", code)
	}
}

// After an admin deletes a conversation's task, it is told so in those words,
// and a press there neither answers nor uses up the notice (caught in review).
func TestAnAdminDeleteIsExplainedAndPressesDoNotSpendIt(t *testing.T) {
	f := newAXFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.say(t, ctx, "C1:1", "hi")
	task := ax.TaskName("sb-triage", "slack", "C1:1")
	f.r.markTaskDeleted(ctx, task, taskDeletedAdmin)
	if _, ok := f.r.liveOrRevived(ctx, "C1:1", "C1"); ok {
		t.Error("a press found a session in a conversation whose task was deleted")
	}
	f.r.mu.Lock()
	rec, kept := f.r.dormant["C1:1"]
	f.r.mu.Unlock()
	if !kept || !rec.TaskDeleted {
		t.Fatalf("the press used up the record (%+v, kept %v): the next message would start over unexplained", rec, kept)
	}
	f.say(t, ctx, "C1:1", "back")
	var found bool
	for drained := false; !drained; {
		select {
		case r := <-f.out.replies:
			if strings.Contains(r.Text, "deleted by an administrator") {
				found = true
			}
		default:
			drained = true
		}
	}
	if !found {
		t.Error("no notice that an administrator deleted the task")
	}
}

// Starting over keeps the agent the conversation had, not the channel's
// default (caught in review).
func TestStartingOverKeepsTheConversationsAgent(t *testing.T) {
	f := newAXFixture(t)
	d := newRecordingDaemon(t, "sd")
	t.Setenv("TOK_DAEMON", "tok")
	set, err := buildAgents([]AgentConfig{
		{Name: "plain", DaemonURL: d.url, TokenEnv: "TOK_DAEMON"},
		f.cfg,
	}, "plain", agentBuilder{ax: &axClients{}})
	if err != nil {
		t.Fatal(err)
	}
	f.r.setAgents(set)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.r.mu.Lock()
	f.r.dormant["C1:1"] = sessionRecord{Channel: "C1", Session: "mast/old", Agent: "triage", Task: ax.TaskName("sb-triage", "slack", "C1:1"),
		Touched: time.Now(), TaskDeleted: true, TaskDeletedReason: taskDeletedQuiet}
	f.r.mu.Unlock()
	f.say(t, ctx, "C1:1", "back")
	if f.server.createCount() != 1 || d.creates.Load() != 0 {
		t.Errorf("ax creates %d, daemon creates %d; want the conversation back on its ax agent", f.server.createCount(), d.creates.Load())
	}
}
