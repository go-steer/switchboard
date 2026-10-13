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
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"

	"github.com/go-steer/switchboard/internal/logging"
)

// The runtime registry (#156, docs/agent-identity.md).
//
// The agents a conversation can go to, the global default, and which agents a
// channel offers used to be fixed at startup. The admin API (#157) changes
// them while turns are running, so the router reads them from one snapshot
// that is replaced whole on each change: a turn never sees an agent added but
// not yet allowed, or a default that names an agent not yet registered.
//
// What the config file declares is pinned. Its agents, its default and the
// agent settings of a channel it names cannot be changed or removed at
// runtime; the API manages only what it added. A deployment's declared agents
// are therefore always there, whatever has been done through the API.

// channelAgents is what the admin API says about one channel's agents. Each
// field it sets replaces, for that channel, what the config would have given
// it; a field it leaves empty is inherited, as a config block inherits from
// defaults (caught in review). So the API can narrow a channel or name its
// default, but cannot widen one past an allow list the config set in defaults.
type channelAgents struct {
	DefaultAgent string   `json:"default_agent,omitempty"`
	Agents       []string `json:"agents,omitempty"`
}

// registry is the routing snapshot. Never modified once published.
type registry struct {
	agents   *agentSet
	channels map[string]channelAgents
}

// agents is the registry every conversation's daemon is found in.
func (r *Router) agents() *agentSet { return r.reg.Load().agents }

// setAgents installs a registry with no runtime channel settings: the startup
// path, and tests.
func (r *Router) setAgents(s *agentSet) { r.reg.Store(&registry{agents: s}) }

// routing reads the agents and a channel's settings from one snapshot, for a
// decision that needs both to agree.
func (r *Router) routing(channel string) (*agentSet, channelSettings) {
	reg := r.reg.Load()
	return reg.agents, r.settingsIn(reg, channel)
}

// overlay applies the API's agent settings for a channel, if it has any.
func (reg *registry) overlay(channel string, s channelSettings) channelSettings {
	if ca, ok := reg.channels[channel]; ok {
		if ca.DefaultAgent != "" {
			s.defaultAgent = ca.DefaultAgent
		}
		if len(ca.Agents) > 0 {
			s.agents = ca.Agents
		}
	}
	return s
}

// Errors the admin operations return, for the API to map onto statuses.
var (
	// errPinned is a change to something the config file declares.
	errPinned = errors.New("declared in the config file, so it cannot be changed at runtime")
	// errUnknown is a change to something that does not exist.
	errUnknown = errors.New("not found")
	// errRejected is a change that would leave the registry inconsistent.
	errRejected = errors.New("rejected")
)

// registryState is what the API has said: the store's contents. Entries that
// do not currently work — an agent whose token variable has gone, a channel
// naming an agent that has — are kept, reported, and left out of the snapshot,
// so a gateway restarted into a changed environment loses nothing an operator
// put there.
type registryState struct {
	Version      int                      `json:"version"`
	Agents       []AgentConfig            `json:"agents,omitempty"`
	DefaultAgent string                   `json:"default_agent,omitempty"`
	Channels     map[string]channelAgents `json:"channels,omitempty"`
}

const (
	registryVersion  = 1
	registryFileName = "registry.json"
)

// registryStore persists registryState, as stateStore does the routing table.
type registryStore interface {
	load() (registryState, error)
	save(registryState) error
	where() string
}

// registryFile keeps the registry as one JSON file beside the state snapshot.
type registryFile struct {
	mu   sync.Mutex
	path string
}

func openRegistryFile(dir string) (*registryFile, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("state dir: %w", err)
	}
	return &registryFile{path: filepath.Join(dir, registryFileName)}, nil
}

func (f *registryFile) where() string { return f.path }

// load reads the registry. A missing file is an empty one. An unreadable file
// is an error, not a fresh start: unlike the routing table, what it holds was
// put there by hand, and starting without it would quietly drop agents.
func (f *registryFile) load() (registryState, error) {
	raw, err := os.ReadFile(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		return registryState{Version: registryVersion}, nil
	}
	if err != nil {
		return registryState{}, err
	}
	var st registryState
	if err := json.Unmarshal(raw, &st); err != nil {
		return registryState{}, fmt.Errorf("%s: %w", f.path, err)
	}
	if st.Version == 0 {
		st.Version = registryVersion // written by hand, without one
	}
	if st.Version != registryVersion {
		return registryState{}, fmt.Errorf("%s: version %d, this build reads %d", f.path, st.Version, registryVersion)
	}
	return st, nil
}

func (f *registryFile) save(st registryState) error {
	st.Version = registryVersion
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return writeFileAtomic(f.path, raw)
}

// registryAdmin owns every change to the registry: it validates a change
// against everything else, persists it, and only then publishes the new
// snapshot. Changes are serialized; reads never wait on it.
type registryAdmin struct {
	mu    sync.Mutex
	r     *Router
	store registryStore // nil: changes last until the process exits
	logf  logging.Logf

	wantApprovals bool
	// What the config file declared: pinned.
	pinned         []*agent
	pinnedDefault  bool   // the file names default_agent
	configDefault  string // the default the file gives, or the first agent
	defaults       channelSettings
	byChannel      map[string]channelSettings
	pinnedChannels map[string]bool // channels whose block names agent settings

	state registryState
}

// setRegistryAdmin installs what changes the registry at runtime: the admin
// API's handle on it (#157). Called once at startup, before dispatch.
func (r *Router) setRegistryAdmin(a *registryAdmin) { r.regAdmin = a }

// agentChannels lists the channels whose config block names a default agent or
// an allow list: their agent settings are pinned.
func agentChannels(cfg *Config) map[string]bool {
	out := map[string]bool{}
	for id, c := range cfg.Channels {
		if c.DefaultAgent != nil || c.Agents != nil {
			out[id] = true
		}
	}
	return out
}

// registryConfig is what newRegistryAdmin needs from startup.
type registryConfig struct {
	Base          *agentSet // the registry the config file built
	PinnedDefault bool
	Defaults      channelSettings
	ByChannel     map[string]channelSettings
	// AgentChannels are the channel IDs whose config block names a default
	// agent or an allow list.
	AgentChannels map[string]bool
	WantApprovals bool
	Store         registryStore
	Logf          logging.Logf
}

func newRegistryAdmin(r *Router, c registryConfig) *registryAdmin {
	a := &registryAdmin{
		r:              r,
		store:          c.Store,
		logf:           c.Logf,
		wantApprovals:  c.WantApprovals,
		pinnedDefault:  c.PinnedDefault,
		configDefault:  c.Base.def,
		defaults:       c.Defaults,
		byChannel:      c.ByChannel,
		pinnedChannels: c.AgentChannels,
		state:          registryState{Version: registryVersion},
	}
	if !c.Base.implicit {
		for _, n := range c.Base.order {
			a.pinned = append(a.pinned, c.Base.byName[n])
		}
	}
	return a
}

// restore installs what the store holds, at startup, before anything is
// dispatched. What cannot be used now is reported and kept.
func (a *registryAdmin) restore(st registryState) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.r.agents().implicit && (len(st.Agents) > 0 || st.DefaultAgent != "" || len(st.Channels) > 0) {
		a.logf.Warnf("registry: %s holds runtime agents, but the config file has no agents list, so they are ignored", a.where())
	}
	a.state = st
	reg, problems := a.compile(st)
	for _, p := range problems {
		a.logf.Warnf("registry: %v; left out until it is fixed", p)
	}
	a.r.reg.Store(reg)
	if n := len(st.Agents); n > 0 {
		a.logf.Infof("registry: %d runtime agent(s) restored from %s", n, a.where())
	}
	switch {
	case st.DefaultAgent != "" && a.pinnedDefault:
		a.logf.Warnf("registry: %s names default agent %q, but the config file sets the default, so it is ignored", a.where(), st.DefaultAgent)
	case reg.agents.def != a.configDefault:
		a.logf.Infof("registry: the default agent is %q (set at runtime)", reg.agents.def)
	}
}

func (a *registryAdmin) where() string {
	if a.store == nil {
		return "memory"
	}
	return a.store.where()
}

// compile builds the snapshot st describes, on top of what the config pins.
// Entries that do not work are left out and returned as problems; the result
// is always a usable registry.
func (a *registryAdmin) compile(st registryState) (*registry, []error) {
	base := a.r.agents()
	if base.implicit {
		// One unnamed agent: nothing to add to, and nothing the API may change.
		return &registry{agents: base}, nil
	}
	var problems []error
	agents := slices.Clone(a.pinned)
	taken := make(map[string]bool, len(agents)+len(st.Agents))
	for _, p := range a.pinned {
		taken[p.name] = true
	}
	for _, ac := range st.Agents {
		if a.isPinned(ac.Name) {
			problems = append(problems, fmt.Errorf("runtime agent %q: %w (the config file declares it)", ac.Name, errPinned))
			continue
		}
		if taken[ac.Name] {
			// Only a hand-edited file can say this; the first one stands
			// (caught in review: it crashed startup).
			problems = append(problems, fmt.Errorf("runtime agent %q is listed twice; the first entry is used", ac.Name))
			continue
		}
		built, err := buildAgent(ac, a.wantApprovals)
		if err != nil {
			problems = append(problems, fmt.Errorf("runtime agent %q%w", ac.Name, err))
			continue
		}
		taken[ac.Name] = true
		agents = append(agents, built)
	}
	set, err := newAgentSet(a.configDefault, agents...)
	if err != nil {
		// Unreachable: names are unique and the config default is pinned.
		return &registry{agents: base}, append(problems, err)
	}
	set.legacy = a.configDefault
	reg := &registry{agents: set}

	// Channels first, and always kept. One that does not check out is kept
	// with its broken names in place — an allow-list name that is not
	// registered matches nothing — rather than dropped, which would hand the
	// channel back to settings that may offer every agent: a restriction must
	// not lapse because the environment broke an agent (caught in review). A
	// default that names no registered agent is left to inherit.
	for _, id := range sortedKeys(st.Channels) {
		if a.pinnedChannels[id] {
			problems = append(problems, fmt.Errorf("runtime settings for channel %q: %w", id, errPinned))
			continue
		}
		ca := st.Channels[id]
		trial := &registry{agents: set, channels: map[string]channelAgents{id: ca}}
		errs := channelProblems(set, fmt.Sprintf("channels[%q]", id), trial.overlay(id, a.configSettings(id)))
		for _, e := range errs {
			problems = append(problems, fmt.Errorf("runtime settings for %w", e))
		}
		if _, ok := set.byName[ca.DefaultAgent]; ca.DefaultAgent != "" && !ok {
			ca.DefaultAgent = ""
		}
		if reg.channels == nil {
			reg.channels = make(map[string]channelAgents)
		}
		reg.channels[id] = ca
	}

	// Then the default, which has to work in every channel that relies on it;
	// one that does not falls back to the config's.
	if st.DefaultAgent != "" && !a.pinnedDefault && st.DefaultAgent != a.configDefault {
		if _, ok := set.byName[st.DefaultAgent]; !ok {
			problems = append(problems, fmt.Errorf("runtime default_agent %q is not a registered agent", st.DefaultAgent))
		} else {
			set.def = st.DefaultAgent
			if err := a.checkChannels(reg); err != nil {
				problems = append(problems, fmt.Errorf("runtime default_agent %q: %w", st.DefaultAgent, err))
				set.def = a.configDefault
			}
		}
	}
	return reg, problems
}

// channelProblems is every way a channel's agent settings fail against set —
// each unknown name, an unknown default, a default the allow list excludes —
// where checkChannelAgents stops at the first. All of them, so that a change
// adding a second breakage to an already broken channel is a new problem, not
// the same one again (caught in review).
func channelProblems(set *agentSet, scope string, cs channelSettings) []error {
	var errs []error
	for _, n := range cs.agents {
		if _, ok := set.byName[n]; !ok {
			errs = append(errs, fmt.Errorf("%s.agents names %q, which is not a registered agent", scope, n))
		}
	}
	if cs.defaultAgent != "" {
		if _, ok := set.byName[cs.defaultAgent]; !ok {
			errs = append(errs, fmt.Errorf("%s.default_agent %q is not a registered agent", scope, cs.defaultAgent))
		}
	}
	if _, err := set.channelAgent(cs); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", scope, err))
	}
	return errs
}

// checkChannels checks every channel's effective agent settings — the
// defaults, each config block, each runtime entry — against reg.
func (a *registryAdmin) checkChannels(reg *registry) error {
	if err := checkChannelAgents(reg.agents, "defaults", a.defaults); err != nil {
		return err
	}
	ids := map[string]bool{}
	for id := range a.byChannel {
		ids[id] = true
	}
	for id := range reg.channels {
		ids[id] = true
	}
	for _, id := range sortedKeys(ids) {
		if err := checkChannelAgents(reg.agents, fmt.Sprintf("channels[%q]", id), reg.overlay(id, a.configSettings(id))); err != nil {
			return err
		}
	}
	return nil
}

// configSettings is a channel's settings as the config file alone gives them.
func (a *registryAdmin) configSettings(id string) channelSettings {
	if cs, ok := a.byChannel[id]; ok {
		return cs
	}
	return a.defaults
}

// apply validates a change — st is the state after it — persists it, and
// publishes the snapshot. A change that would add a problem the current state
// does not have is refused, so an entry broken by the environment never blocks
// an unrelated change, while a change cannot break anything itself.
func (a *registryAdmin) apply(st registryState) error {
	if a.r.agents().implicit {
		return fmt.Errorf("%w: runtime agents need an agents list in the config file", errRejected)
	}
	_, before := a.compile(a.state)
	reg, after := a.compile(st)
	seen := make(map[string]bool, len(before))
	for _, p := range before {
		seen[p.Error()] = true
	}
	for _, p := range after {
		if !seen[p.Error()] {
			if errors.Is(p, errPinned) {
				return p
			}
			return fmt.Errorf("%w: %v", errRejected, p)
		}
	}
	if a.store != nil {
		if err := a.store.save(st); err != nil {
			return fmt.Errorf("registry: save %s: %w", a.store.where(), err)
		}
	}
	a.state = st
	a.r.reg.Store(reg)
	return nil
}

// snapshotState is a deep enough copy of the current state to change.
func (a *registryAdmin) snapshotState() registryState {
	st := a.state
	st.Agents = slices.Clone(a.state.Agents)
	st.Channels = make(map[string]channelAgents, len(a.state.Channels))
	for id, ca := range a.state.Channels {
		st.Channels[id] = ca
	}
	return st
}

func (a *registryAdmin) isPinned(name string) bool {
	return slices.ContainsFunc(a.pinned, func(p *agent) bool { return p.name == name })
}

// putAgent adds a runtime agent or replaces one. Replacing an agent's daemon
// leaves its threads' sessions on the old one; that is the caller's call.
func (a *registryAdmin) putAgent(ac AgentConfig) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.isPinned(ac.Name) {
		return fmt.Errorf("agent %q is %w", ac.Name, errPinned)
	}
	st := a.snapshotState()
	if i := slices.IndexFunc(st.Agents, func(x AgentConfig) bool { return x.Name == ac.Name }); i >= 0 {
		st.Agents[i] = ac
	} else {
		st.Agents = append(st.Agents, ac)
	}
	return a.apply(st)
}

// removeAgent removes a runtime agent. Refused while the default or a channel
// still names it; its threads get the "agent is gone" notice, and nothing is
// rerouted.
func (a *registryAdmin) removeAgent(name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.isPinned(name) {
		return fmt.Errorf("agent %q is %w", name, errPinned)
	}
	st := a.snapshotState()
	i := slices.IndexFunc(st.Agents, func(x AgentConfig) bool { return x.Name == name })
	if i < 0 {
		return fmt.Errorf("agent %q: %w", name, errUnknown)
	}
	st.Agents = slices.Delete(st.Agents, i, i+1)
	return a.apply(st)
}

// setDefault moves the global default; "" returns it to the config's.
func (a *registryAdmin) setDefault(name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pinnedDefault {
		return fmt.Errorf("the default agent is %w", errPinned)
	}
	st := a.snapshotState()
	st.DefaultAgent = name
	return a.apply(st)
}

// setChannel gives a channel its own default and allow list.
func (a *registryAdmin) setChannel(id string, ca channelAgents) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pinnedChannels[id] {
		return fmt.Errorf("channel %q's agents are %w", id, errPinned)
	}
	if ca.DefaultAgent == "" && len(ca.Agents) == 0 {
		return fmt.Errorf("%w: name a default_agent, agents, or both; to drop a channel's settings, clear it", errRejected)
	}
	st := a.snapshotState()
	st.Channels[id] = ca
	return a.apply(st)
}

// clearChannel drops a channel's runtime agent settings.
func (a *registryAdmin) clearChannel(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pinnedChannels[id] {
		return fmt.Errorf("channel %q's agents are %w", id, errPinned)
	}
	st := a.snapshotState()
	if _, ok := st.Channels[id]; !ok {
		return fmt.Errorf("channel %q: %w", id, errUnknown)
	}
	delete(st.Channels, id)
	return a.apply(st)
}

// current is what the API has said, including entries left out for now.
func (a *registryAdmin) current() registryState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.snapshotState()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
