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
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/go-steer/switchboard/pkg/approval"
	"github.com/go-steer/switchboard/pkg/daemon"
)

// Multi-agent routing (#140, docs/multi-agent.md).
//
// A gateway used to speak to exactly one daemon. It now holds a registry of
// agents — each a daemon speaking the core-agent contract (core-agent, mast) —
// and every conversation belongs to one of them: the thread's agent, chosen
// when the conversation began and kept for its life. One agent per thread is
// the rule the rest of this rests on; nothing here moves a conversation
// between agents.
//
// A deployment that registers no agents gets exactly one, built from
// daemon_url and token_env as before, and stores *no* name for it: its records
// and buttons say nothing about an agent, exactly as those written before the
// registry existed. A record or a decision id that names no agent belongs to
// whatever the default is — so a gateway that later registers a list keeps
// every such thread, as long as the old daemon is the default. Writing a name
// here (the literal "default") would pin those threads to an agent no list
// will ever have (caught in review).

// defaultAgentName names the agent a deployment without an agents list gets.
const defaultAgentName = "default"

// agentNameRE is what an agent name may be: it is typed in commands, written
// into records and carried in button payloads, so it is kept short, lowercase
// and free of the separators those formats use ('@', '#', '/', ':').
var agentNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

// AgentConfig is one agent in the config file's agents list. The credential is
// named, never given: TokenEnv is the variable holding it (see checkNoSecrets).
type AgentConfig struct {
	Name        string            `json:"name"`
	DisplayName string            `json:"display_name,omitempty"`
	Description string            `json:"description,omitempty"`
	DaemonURL   string            `json:"daemon_url"`
	TokenEnv    string            `json:"token_env"`
	Kind        string            `json:"kind,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

// agentKinds are the daemon flavours an agent may declare. Informational today
// — both speak the same contract — and the place a difference would attach.
var agentKinds = []string{"core-agent", "mast"}

// agent is one routable daemon and the clients switchboard holds for it.
type agent struct {
	name    string
	display string
	desc    string
	daemon  *daemon.Client
	// approvals is nil when no channel relays permission prompts: a run with
	// approvals off everywhere holds no client for a surface it does not offer.
	approvals *approval.Client
}

// label is how the agent is named to a person.
func (a *agent) label() string {
	if a.display != "" {
		return a.display
	}
	return a.name
}

// agentSet is the registry: every agent by name, and the global default.
type agentSet struct {
	byName map[string]*agent
	order  []string // registration order, for listings
	def    string
	// implicit marks the registry of a deployment with no agents list: one
	// agent, never named in a record or a button (see stored).
	implicit bool
}

// newAgentSet builds a registry. def names the global default and must be one
// of the agents.
func newAgentSet(def string, agents ...*agent) (*agentSet, error) {
	s := &agentSet{byName: make(map[string]*agent, len(agents)), def: def}
	for _, a := range agents {
		if _, dup := s.byName[a.name]; dup {
			return nil, fmt.Errorf("agent %q is registered twice", a.name)
		}
		s.byName[a.name] = a
		s.order = append(s.order, a.name)
	}
	if _, ok := s.byName[def]; !ok {
		return nil, fmt.Errorf("default agent %q is not a registered agent", def)
	}
	return s, nil
}

// singleAgent is the registry of a deployment with one daemon.
func singleAgent(c *daemon.Client, ac *approval.Client) *agentSet {
	s, _ := newAgentSet(defaultAgentName, &agent{name: defaultAgentName, daemon: c, approvals: ac})
	s.implicit = true
	return s
}

// stored is the name a record or button carries for an agent: the name
// itself in a registry built from a list, and nothing for the implicit one.
func (s *agentSet) stored(name string) string {
	if s.implicit {
		return ""
	}
	return s.resolve(name)
}

// lookup finds an agent by name; the empty name is the default, which is what
// a record or decision id written before the registry existed names.
func (s *agentSet) lookup(name string) (*agent, bool) {
	if name == "" {
		name = s.def
	}
	a, ok := s.byName[name]
	return a, ok
}

// resolve is the name lookup would use, so a record can store the agent it
// actually got rather than the empty default it was asked for.
func (s *agentSet) resolve(name string) string {
	if name == "" {
		return s.def
	}
	return name
}

// anyApprovals reports whether any agent can relay permission prompts.
func (s *agentSet) anyApprovals() bool {
	for _, a := range s.byName {
		if a.approvals != nil {
			return true
		}
	}
	return false
}

// daemonAPI is the part of the daemon client the router calls: the contract's
// verbs, plus the head probe. An interface so a conversation whose agent is
// gone can be handed something that refuses rather than nil.
type daemonAPI interface {
	CreateSession(ctx context.Context, assertedCaller string) (daemon.Session, error)
	Inject(ctx context.Context, sess daemon.Session, assertedCaller, text string) error
	Subscribe(ctx context.Context, sess daemon.Session, assertedCaller string, since int64, fn func(daemon.Event) error) error
	HeadSeq(ctx context.Context, sess daemon.Session, assertedCaller string) (int64, error)
}

// goneDaemon stands in for an agent the registry no longer has.
type goneDaemon struct{}

func (goneDaemon) CreateSession(context.Context, string) (daemon.Session, error) {
	return daemon.Session{}, errAgentGone
}
func (goneDaemon) Inject(context.Context, daemon.Session, string, string) error { return errAgentGone }
func (goneDaemon) Subscribe(context.Context, daemon.Session, string, int64, func(daemon.Event) error) error {
	return errAgentGone
}
func (goneDaemon) HeadSeq(context.Context, daemon.Session, string) (int64, error) {
	return 0, errAgentGone
}

// setAgents installs the registry. Called once at startup, before dispatch.
func (r *Router) setAgents(s *agentSet) { r.agents = s }

// daemonFor is the daemon client of a named agent — the default for "". A name
// the registry no longer has gets a client that refuses every call with
// errAgentGone, so a caller handles it like any other daemon failure.
func (r *Router) daemonFor(name string) daemonAPI {
	if a, ok := r.agents.lookup(name); ok {
		return a.daemon
	}
	return goneDaemon{}
}

// daemonOf is daemonFor for a conversation.
func (r *Router) daemonOf(e *sessionEntry) daemonAPI { return r.daemonFor(e.agent) }

// approvalsOf is the approval client of a conversation's agent, nil when it
// relays no prompts or the agent is gone.
func (r *Router) approvalsOf(e *sessionEntry) *approval.Client {
	if a, ok := r.agents.lookup(e.agent); ok {
		return a.approvals
	}
	return nil
}

// errAgentGone is a conversation whose agent is no longer registered — removed
// from the config since the thread began. Nothing is rerouted: the thread is
// told, and a new one starts with an agent that exists.
var errAgentGone = errors.New("this conversation's agent is no longer available")

// agentGoneNotice tells a thread its agent has been removed.
func agentGoneNotice(name string) string {
	return fmt.Sprintf("This conversation's agent (%s) is no longer available. Start a new thread to talk to another agent.", name)
}

// channelAgent is the agent a new conversation in a channel goes to: the
// channel's default, else the global one — provided the channel allows it.
//
// The name returned is the one to store (see stored): empty for the implicit
// single agent.
func (s *agentSet) channelAgent(cs channelSettings) (string, error) {
	name := cs.defaultAgent
	if name == "" {
		name = s.def
	}
	if len(cs.agents) > 0 && !slices.Contains(cs.agents, name) {
		return "", fmt.Errorf("%w: the default agent %q is not in this channel's allowed agents", errNoAgentHere, name)
	}
	return s.stored(name), nil
}

// chosenAgent validates an explicitly chosen agent for a channel and returns
// the name to store.
func (s *agentSet) chosenAgent(cs channelSettings, name string) (string, error) {
	if _, ok := s.byName[name]; !ok || s.implicit {
		return "", fmt.Errorf("%w: %q is not a registered agent", errNoAgentHere, name)
	}
	if len(cs.agents) > 0 && !slices.Contains(cs.agents, name) {
		return "", fmt.Errorf("%w: %q is not among this channel's allowed agents", errNoAgentHere, name)
	}
	return s.stored(name), nil
}

// allowedIn lists the agents a channel may use, in registration order.
func (s *agentSet) allowedIn(cs channelSettings) []*agent {
	var out []*agent
	for _, n := range s.order {
		if len(cs.agents) == 0 || slices.Contains(cs.agents, n) {
			out = append(out, s.byName[n])
		}
	}
	return out
}

// errNoAgentHere is a channel whose allow list excludes the agent it would
// default to. Startup validation refuses that configuration; this is what a
// conversation is told if one gets through anyway.
var errNoAgentHere = errors.New("no agent is available here")

// buildAgents turns the config file's agents list into a registry, reading each
// agent's credential from the variable it names. wantApprovals builds the
// approval client alongside each daemon client.
func buildAgents(list []AgentConfig, def string, wantApprovals bool) (*agentSet, error) {
	agents := make([]*agent, 0, len(list))
	for i, ac := range list {
		where := fmt.Sprintf("agents[%d]", i)
		if !agentNameRE.MatchString(ac.Name) {
			return nil, fmt.Errorf("%s.name %q: want a short lowercase name (letters, digits, '-'), like %q", where, ac.Name, "platform")
		}
		where = fmt.Sprintf("agents[%q]", ac.Name)
		if ac.Kind != "" && !slices.Contains(agentKinds, ac.Kind) {
			return nil, fmt.Errorf("%s.kind %q: want one of %s", where, ac.Kind, strings.Join(agentKinds, ", "))
		}
		if strings.TrimSpace(ac.DaemonURL) == "" {
			return nil, fmt.Errorf("%s.daemon_url is required", where)
		}
		if strings.TrimSpace(ac.TokenEnv) == "" {
			return nil, fmt.Errorf("%s.token_env is required: name the variable holding the daemon token", where)
		}
		token := os.Getenv(ac.TokenEnv)
		if token == "" {
			return nil, fmt.Errorf("%s: no daemon token in $%s", where, ac.TokenEnv)
		}
		cfg := daemon.Config{BaseURL: ac.DaemonURL, BearerToken: token}
		dc, err := daemon.New(cfg)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		a := &agent{name: ac.Name, display: ac.DisplayName, desc: strings.TrimSpace(ac.Description), daemon: dc}
		if wantApprovals {
			if a.approvals, err = approval.New(cfg); err != nil {
				return nil, fmt.Errorf("%s: %w", where, err)
			}
		}
		agents = append(agents, a)
	}
	if def == "" && len(agents) > 0 {
		def = agents[0].name
	}
	return newAgentSet(def, agents...)
}

// agentsFrom builds the registry a bridged run routes through: the config
// file's agents list when there is one, and otherwise the single agent
// daemon_url and token_env have always described.
func agentsFrom(cfg *Config, daemonURL, tokenEnv string, wantApprovals bool) (*agentSet, error) {
	def := ""
	if cfg.DefaultAgent != nil {
		def = strings.TrimSpace(*cfg.DefaultAgent)
	}
	if len(cfg.Agents) > 0 {
		return buildAgents(cfg.Agents, def, wantApprovals)
	}
	if def != "" && def != defaultAgentName {
		return nil, fmt.Errorf("default_agent %q names an agent, but the config file has no agents list", def)
	}
	token := os.Getenv(tokenEnv)
	if token == "" {
		return nil, fmt.Errorf("no daemon token in $%s (set --token-env to the right var)", tokenEnv)
	}
	dcfg := daemon.Config{BaseURL: daemonURL, BearerToken: token}
	dc, err := daemon.New(dcfg)
	if err != nil {
		return nil, err
	}
	// Same daemon, same credential, different routes — and built here rather
	// than inside the router so that a run with approvals off everywhere holds
	// no client for a surface it does not offer.
	var ac *approval.Client
	if wantApprovals {
		if ac, err = approval.New(dcfg); err != nil {
			return nil, err
		}
	}
	return singleAgent(dc, ac), nil
}

// checkChannelAgents refuses channel settings naming agents the registry does
// not have, or a default the channel's own allow list excludes. At startup,
// because both fail as a thread that cannot start, discovered by whoever posts
// in it first.
func checkChannelAgents(s *agentSet, scope string, cs channelSettings) error {
	none := ""
	if s.implicit {
		none = " (the config file has no agents list)"
	}
	for _, n := range cs.agents {
		if _, ok := s.byName[n]; !ok || s.implicit {
			return fmt.Errorf("%s.agents names %q, which is not a registered agent%s", scope, n, none)
		}
	}
	if cs.defaultAgent != "" {
		if _, ok := s.byName[cs.defaultAgent]; !ok || s.implicit {
			return fmt.Errorf("%s.default_agent %q is not a registered agent%s", scope, cs.defaultAgent, none)
		}
	}
	if _, err := s.channelAgent(cs); err != nil {
		return fmt.Errorf("%s: %w", scope, err)
	}
	return nil
}
