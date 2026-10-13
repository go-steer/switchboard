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
	"sync"
	"testing"

	"github.com/go-steer/switchboard/pkg/chat"
)

// registryFixture is a gateway whose config declares agents "a" (the default)
// and "b", and channel C3 restricted to both with "a" its own default, with
// daemon "c" standing by to be added at runtime.
type registryFixture struct {
	r       *Router
	admin   *registryAdmin
	a, b, c *recordingDaemon
	store   *registryFile
}

func newRegistryFixture(t *testing.T, store *registryFile) *registryFixture {
	t.Helper()
	t.Setenv("TOK_REG", "tok")
	f := &registryFixture{a: newRecordingDaemon(t, "sa"), b: newRecordingDaemon(t, "sb"), c: newRecordingDaemon(t, "sc"), store: store}
	base, err := buildAgents([]AgentConfig{
		{Name: "a", DaemonURL: f.a.url, TokenEnv: "TOK_REG"},
		{Name: "b", DaemonURL: f.b.url, TokenEnv: "TOK_REG"},
	}, "a", false)
	if err != nil {
		t.Fatal(err)
	}
	byChannel := map[string]channelSettings{"C3": {defaultAgent: "a", agents: []string{"a", "b"}}}
	f.r = NewRouter(nil, &fakeSender{replies: make(chan chat.Reply, 16)}, ProgressOff, nil, nil)
	f.r.setAgents(base)
	f.r.setChannels(byChannel)
	cfg := registryConfig{Base: base, ByChannel: byChannel, AgentChannels: map[string]bool{"C3": true}}
	if store != nil {
		cfg.Store = store
	}
	f.admin = newRegistryAdmin(f.r, cfg)
	f.r.setRegistryAdmin(f.admin)
	return f
}

func (f *registryFixture) agentC() AgentConfig {
	return AgentConfig{Name: "c", DisplayName: "Agent C", DaemonURL: f.c.url, TokenEnv: "TOK_REG"}
}

func choiceNames(cs []chat.AgentChoice) []string {
	var out []string
	for _, c := range cs {
		n := c.Name
		if c.Default {
			n += "*"
		}
		out = append(out, n)
	}
	return out
}

// An agent added at runtime is offered and routed to at once, with no restart.
func TestARuntimeAgentIsOfferedAndRoutedTo(t *testing.T) {
	f := newRegistryFixture(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := f.admin.putAgent(f.agentC()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(choiceNames(f.r.AgentChoices("C1")), ","); got != "a*,b,c" {
		t.Errorf("choices = %s, want c offered beside the config's agents", got)
	}
	if _, err := f.r.sessionAs(ctx, "C1:1", "C1", "u@x.com", "c"); err != nil {
		t.Fatal(err)
	}
	waitCount(t, &f.c.creates, 1, "agent c sessions")
	f.r.mu.Lock()
	e := f.r.sessions["C1:1"]
	f.r.mu.Unlock()
	if id := f.r.identityOf(e); id == nil || id.Label != "Agent C" {
		t.Errorf("identity = %+v, want the runtime agent's", id)
	}
}

// What the config declares is pinned: its agents, its default when it names
// one, and a channel whose block names agent settings.
func TestTheConfigsDeclarationsArePinned(t *testing.T) {
	f := newRegistryFixture(t, nil)
	for name, err := range map[string]error{
		"replace a":   f.admin.putAgent(AgentConfig{Name: "a", DaemonURL: f.c.url, TokenEnv: "TOK_REG"}),
		"remove b":    f.admin.removeAgent("b"),
		"set C3":      f.admin.setChannel("C3", channelAgents{DefaultAgent: "b"}),
		"clear C3":    f.admin.clearChannel("C3"),
		"remove none": f.admin.removeAgent("nope"),
	} {
		want := errPinned
		if name == "remove none" {
			want = errUnknown
		}
		if !errors.Is(err, want) {
			t.Errorf("%s: err = %v, want %v", name, err, want)
		}
	}

	f.admin.pinnedDefault = true
	if err := f.admin.setDefault("b"); !errors.Is(err, errPinned) {
		t.Errorf("set a pinned default: err = %v", err)
	}
}

// A runtime agent can't be removed while the default or a channel names it:
// a channel defaulting to an agent that isn't there is a thread that can't
// start. Once nothing names it, it goes, and its threads are told.
func TestARuntimeAgentInUseCannotBeRemoved(t *testing.T) {
	f := newRegistryFixture(t, nil)
	if err := f.admin.putAgent(f.agentC()); err != nil {
		t.Fatal(err)
	}
	if err := f.admin.setChannel("C9", channelAgents{DefaultAgent: "c", Agents: []string{"c"}}); err != nil {
		t.Fatal(err)
	}
	if err := f.admin.removeAgent("c"); !errors.Is(err, errRejected) {
		t.Fatalf("remove while C9 uses it: err = %v, want rejected", err)
	}
	if err := f.admin.clearChannel("C9"); err != nil {
		t.Fatal(err)
	}
	if err := f.admin.setDefault("c"); err != nil {
		t.Fatal(err)
	}
	if err := f.admin.removeAgent("c"); !errors.Is(err, errRejected) {
		t.Fatalf("remove while the default: err = %v, want rejected", err)
	}
	if err := f.admin.setDefault(""); err != nil {
		t.Fatal(err)
	}
	if err := f.admin.removeAgent("c"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.r.daemonFor("c").(goneDaemon); !ok {
		t.Error("a removed agent's threads still reach a daemon")
	}
}

// Moving the default moves where new conversations go, but not the threads
// recorded before the agents list existed: those name no agent and stay on
// the default the config gave them, whose daemon holds their sessions.
func TestMovingTheDefaultLeavesLegacyThreadsWhereTheyAre(t *testing.T) {
	f := newRegistryFixture(t, nil)
	if err := f.admin.putAgent(f.agentC()); err != nil {
		t.Fatal(err)
	}
	if err := f.admin.setDefault("c"); err != nil {
		t.Fatal(err)
	}
	set, cs := f.r.routing("C1")
	if got, _ := set.channelAgent(cs); got != "c" {
		t.Errorf("new conversations go to %q, want c", got)
	}
	if f.r.daemonFor("") != daemonAPI(f.r.agents().byName["a"].daemon) {
		t.Error("a record naming no agent moved with the default")
	}
	if got := f.r.agents().stored(""); got != "a" {
		t.Errorf("stored(\"\") = %q, want the legacy default", got)
	}
}

// A default a channel's allow list excludes would leave that channel unable
// to start a thread, so it is refused.
func TestADefaultAChannelExcludesIsRefused(t *testing.T) {
	f := newRegistryFixture(t, nil)
	if err := f.admin.putAgent(f.agentC()); err != nil {
		t.Fatal(err)
	}
	// C4 allows a and b and names no default of its own, so it follows the
	// global one.
	c4 := channelSettings{agents: []string{"a", "b"}}
	f.admin.byChannel = map[string]channelSettings{"C3": f.admin.byChannel["C3"], "C4": c4}
	f.r.setChannels(f.admin.byChannel)
	if err := f.admin.setDefault("c"); !errors.Is(err, errRejected) {
		t.Fatalf("err = %v, want rejected: C3 would have no usable default", err)
	}
	if set, _ := f.r.routing("C1"); set.def != "a" {
		t.Errorf("default = %q after a refused change, want it unchanged", set.def)
	}
}

func TestRuntimeChannelSettingsReplaceTheConfigs(t *testing.T) {
	f := newRegistryFixture(t, nil)
	if err := f.admin.setChannel("C1", channelAgents{DefaultAgent: "b", Agents: []string{"b"}}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(choiceNames(f.r.AgentChoices("C1")), ","); got != "b*" {
		t.Errorf("C1 choices = %s, want only b, the default", got)
	}
	if err := f.admin.setChannel("C1", channelAgents{DefaultAgent: "nope"}); !errors.Is(err, errRejected) {
		t.Errorf("an unknown default: err = %v, want rejected", err)
	}
}

// What the API adds survives a restart, and an entry the environment has
// broken in the meantime is kept, reported and left out rather than lost or
// allowed to block an unrelated change.
func TestTheRegistryOutlivesARestart(t *testing.T) {
	dir := t.TempDir()
	store, err := openRegistryFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := newRegistryFixture(t, store)
	t.Setenv("TOK_GONE", "tok")
	if err := f.admin.putAgent(f.agentC()); err != nil {
		t.Fatal(err)
	}
	if err := f.admin.putAgent(AgentConfig{Name: "d", DaemonURL: f.c.url, TokenEnv: "TOK_GONE"}); err != nil {
		t.Fatal(err)
	}
	os.Unsetenv("TOK_GONE")

	st, err := store.load()
	if err != nil {
		t.Fatal(err)
	}
	g := newRegistryFixture(t, store)
	g.admin.restore(st)
	if _, ok := g.r.agents().byName["c"]; !ok {
		t.Error("c was not restored")
	}
	if _, ok := g.r.agents().byName["d"]; ok {
		t.Error("d, whose token variable is gone, was registered")
	}
	if err := g.admin.setChannel("C1", channelAgents{DefaultAgent: "c"}); err != nil {
		t.Fatalf("an unrelated change was blocked by the broken entry: %v", err)
	}
	after, err := store.load()
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Agents) != 2 {
		t.Errorf("stored agents = %+v, want the broken one kept", after.Agents)
	}
}

func TestAnUnreadableRegistryIsAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, registryFileName), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := openRegistryFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.load(); err == nil {
		t.Error("an unreadable registry loaded")
	}
}

// Without an agents list there is one unnamed agent and nothing to manage.
func TestAGatewayWithoutAnAgentsListTakesNoRuntimeAgents(t *testing.T) {
	d := newRecordingDaemon(t, "s1")
	t.Setenv("TOK_REG", "tok")
	r := NewRouter(d.client, &fakeSender{replies: make(chan chat.Reply, 1)}, ProgressOff, nil, nil)
	admin := newRegistryAdmin(r, registryConfig{Base: r.agents()})
	if err := admin.putAgent(AgentConfig{Name: "c", DaemonURL: d.url, TokenEnv: "TOK_REG"}); !errors.Is(err, errRejected) {
		t.Errorf("err = %v, want rejected", err)
	}
}

// Turns keep routing while the registry changes under them: every read sees a
// whole snapshot (run under -race).
func TestTurnsRouteWhileTheRegistryChanges(t *testing.T) {
	f := newRegistryFixture(t, nil)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, c := range f.r.AgentChoices("C1") {
					if c.Name == "" {
						t.Error("an empty choice")
					}
				}
				set, cs := f.r.routing("C1")
				if _, err := set.channelAgent(cs); err != nil {
					t.Errorf("a torn snapshot: %v", err)
				}
			}
		}()
	}
	for range 50 {
		if err := f.admin.putAgent(f.agentC()); err != nil {
			t.Fatal(err)
		}
		if err := f.admin.setChannel("C1", channelAgents{DefaultAgent: "c", Agents: []string{"c"}}); err != nil {
			t.Fatal(err)
		}
		if err := f.admin.clearChannel("C1"); err != nil {
			t.Fatal(err)
		}
		if err := f.admin.removeAgent("c"); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
}
