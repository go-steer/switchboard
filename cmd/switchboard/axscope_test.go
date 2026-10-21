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

	"github.com/go-steer/switchboard/pkg/ax"
	"github.com/go-steer/switchboard/pkg/chat"
)

// withScope rebuilds the fixture's agent with scope.
func withScope(t *testing.T, f *axFixture, scope string) {
	t.Helper()
	axc := *f.cfg.AX
	axc.Scope = scope
	f.cfg.AX = &axc
	set, err := buildAgents([]AgentConfig{f.cfg}, "triage", agentBuilder{ax: &axClients{}})
	if err != nil {
		t.Fatal(err)
	}
	f.r.setAgents(set)
}

func (f *axFixture) sayAs(t *testing.T, ctx context.Context, conv, caller, text string) {
	t.Helper()
	if err := f.r.Handle(ctx, chat.Message{Conversation: conv, Channel: "C1", Caller: caller, Text: text}); err != nil {
		t.Fatalf("Handle %s as %s: %v", conv, caller, err)
	}
}

func distinct(names []string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

// A personal agent is one task per person: every thread they start with it
// is a session in that task, and somebody else gets a task of their own.
func TestAPersonalAgentIsATaskPerPerson(t *testing.T) {
	f := newAXFixture(t)
	withScope(t, f, axScopeOwner)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	f.sayAs(t, ctx, "C1:1", "alice@x.com", "one")
	f.sayAs(t, ctx, "C1:2", "alice@x.com", "two")
	f.sayAs(t, ctx, "C1:3", "bob@x.com", "three")

	f.server.mu.Lock()
	tasks := distinct(f.server.creates)
	f.server.mu.Unlock()
	alice := ax.TaskName("sb-triage-u", "slack", "alice@x.com")
	bob := ax.TaskName("sb-triage-u", "slack", "bob@x.com")
	if len(tasks) != 2 || !tasks[alice] || !tasks[bob] {
		t.Errorf("tasks = %v, want alice's and bob's", tasks)
	}
	if strings.Contains(alice, "alice") {
		t.Errorf("the task name %q carries the caller", alice)
	}
	// Alice's two threads are two sessions in her one task.
	if posts := f.router.callsMatching("agents/" + alice + " POST /sessions"); len(posts) < 4 {
		t.Errorf("calls to alice's task = %v, want two sessions and two injects", posts)
	}
}

// A personal agent's thread answers its owner alone: anyone else driving it
// would act with the owner's credentials and history.
func TestAPersonalAgentAnswersItsOwnerOnly(t *testing.T) {
	f := newAXFixture(t)
	withScope(t, f, axScopeOwner)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	f.sayAs(t, ctx, "C1:1", "alice@x.com", "mine")
	alice := "agents/" + ax.TaskName("sb-triage-u", "slack", "alice@x.com")
	waitFor(t, func() bool { return len(f.router.callsMatching(alice+" POST /sessions/")) == 1 }, "alice's inject")

	f.sayAs(t, ctx, "C1:1", "mallory@x.com", "let me in")
	got := recvReply(t, f.out.replies)
	if !strings.Contains(got.Text, "personal agent") {
		t.Errorf("notice = %q", got.Text)
	}
	if n := len(f.router.callsMatching(alice + " POST /sessions/")); n != 1 {
		t.Errorf("injects into alice's task = %d, want hers alone", n)
	}
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	if len(f.server.creates) != 1 {
		t.Errorf("creates = %v, want no task for mallory", f.server.creates)
	}
}

// A shared agent is one task for everyone, whatever thread or caller.
func TestASharedAgentIsOneTask(t *testing.T) {
	f := newAXFixture(t)
	withScope(t, f, axScopeShared)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.sayAs(t, ctx, "C1:1", "alice@x.com", "one")
	f.sayAs(t, ctx, "C1:2", "bob@x.com", "two")
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	want := ax.TaskName("sb-triage-shared", "slack")
	if tasks := distinct(f.server.creates); len(tasks) != 1 || !tasks[want] {
		t.Errorf("tasks = %v, want only %s", tasks, want)
	}
}

// Each scope names its tasks apart: a thread task, a person's and the shared
// one never coincide.
func TestScopesNameTasksApart(t *testing.T) {
	x := func(scope string) *axAgent { return &axAgent{scope: scope} }
	names := []string{
		x(axScopeThread).taskName("triage", "slack", "C1:1", "alice"),
		x(axScopeOwner).taskName("triage", "slack", "C1:1", "alice"),
		x(axScopeShared).taskName("triage", "slack", "C1:1", "alice"),
	}
	if len(distinct(names)) != 3 {
		t.Errorf("names %v collide", names)
	}
	for _, n := range names {
		if err := ax.ValidName(n); err != nil {
			t.Error(err)
		}
	}
}
