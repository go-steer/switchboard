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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-steer/switchboard/internal/logging"
	"github.com/go-steer/switchboard/pkg/approval"
	"github.com/go-steer/switchboard/pkg/ax"
	"github.com/go-steer/switchboard/pkg/chat"
	"github.com/go-steer/switchboard/pkg/daemon"
)

// ax agents (#170, docs/ax-backend.md).
//
// An agent of kind "ax" is not a daemon somebody runs: it is a task template,
// and each conversation gets a task of its own, created from it on the first
// message, woken by Agent Substrate's router on every one after, and reached
// through that router. Inside the task the agent serves the same session
// contract a daemon does, so past the task's start nothing in the router
// knows the difference: daemonOf and approvalsOf hand back the task's
// clients.

const agentKindAX = "ax"

// AXConfig is an ax agent's ax block.
type AXConfig struct {
	// Server is ax-server's gRPC address, host:port.
	Server string `json:"server"`
	// Router is the atenet router's base URL; empty is ax.DefaultRouter.
	// An https router is reached with mTLS (#181): the pod's podidentity
	// certificate, verified against the servicedns trust bundle. An http
	// router is reached in plaintext, for a development router or a test.
	Router string `json:"router,omitempty"`
	// RouterCredentialBundleFile is the client's key and certificate
	// chain for an https router, from a podCertificate projection's
	// credentialBundlePath; empty is ax.DefaultCredentialBundleFile.
	RouterCredentialBundleFile string `json:"router_credential_bundle_file,omitempty"`
	// RouterTrustBundleFile is the roots the router's certificate must
	// chain to, from a clusterTrustBundle projection of the servicedns
	// signer; empty is ax.DefaultTrustBundleFile.
	RouterTrustBundleFile string `json:"router_trust_bundle_file,omitempty"`
	// RouterServerName is the name verified on the router's certificate;
	// empty is ax.DefaultRouterServerName.
	RouterServerName string `json:"router_server_name,omitempty"`
	// Atespace the tasks live in; empty is ax's default.
	Atespace string `json:"atespace,omitempty"`
	// TaskTemplateFile is a full ax Task manifest every task is created
	// from. A file, because it carries the agent's token in its env.
	TaskTemplateFile string `json:"task_template_file"`
	// Scope is which conversations share a task (#172): "thread" (the
	// default), a task per conversation; "owner", a task per person — their
	// personal agent, every thread they start with it a session in it; or
	// "shared", one task for everyone.
	Scope string `json:"scope,omitempty"`
	// ReadyTimeout bounds a task's start; empty is two minutes.
	ReadyTimeout string `json:"ready_timeout,omitempty"`
	// StreamLinger is how long after its last traffic a conversation keeps
	// its streams to the task open; empty is one minute, and at least ten
	// seconds. An open stream is traffic to ax, whose idle clock starts when
	// the stream closes, so a task suspends about linger + suspendAfter after
	// its last turn (#171).
	StreamLinger string `json:"stream_linger,omitempty"`
	// DeleteAfter deletes a thread-scope task once its conversation has
	// been quiet this long (#173); empty is 14 days, "0" keeps tasks. At
	// most dormantMaxAge: past it the conversation's record is forgotten,
	// and with it which task to delete.
	DeleteAfter string `json:"delete_after,omitempty"`
	// ReplaceDrifted replaces a thread-scope task created from an earlier
	// version of the template — a rotated token, say, which would fail every
	// request — with a fresh one; nil is true. Its conversation starts over.
	ReplaceDrifted *bool `json:"replace_drifted,omitempty"`
}

// defaultDeleteAfter is how long a thread-scope task outlives its
// conversation's last traffic. maxDeleteAfter leaves a day under the record's
// own lifetime: at the cap the record was pruned before the deleting sweep
// could see it, orphaning the task (caught in review).
const (
	defaultDeleteAfter = 14 * 24 * time.Hour
	maxDeleteAfter     = dormantMaxAge - 24*time.Hour
)

// minStreamLinger bounds stream_linger from below: shorter, a conversation
// would let go between a message and its reply.
const minStreamLinger = 10 * time.Second

// defaultStreamLinger is how long an ax conversation keeps its streams open
// after its last traffic.
const defaultStreamLinger = time.Minute

// The scopes.
const (
	axScopeThread = "thread"
	axScopeOwner  = "owner"
	axScopeShared = "shared"
)

// axAgent is what an ax agent holds instead of a daemon client: the ax-server
// client, and each task's clients, made on first use.
type axAgent struct {
	client    *ax.Client
	token     string
	scope     string
	approvals bool
	idle      bool          // the template lets tasks suspend
	idleAfter time.Duration // the template's suspendAfter
	linger    time.Duration // streams close this long after the last traffic

	deleteAfter    time.Duration // thread tasks quiet this long are deleted; 0 keeps
	replaceDrifted bool          // replace a thread task made from an older template

	omitCaller bool // send no X-Asserted-Caller (assert_caller, false by default here)
}

// ownsTask reports whether name is one of this agent's tasks, in any scope:
// what the admin API may list and delete for it. By pattern, since ax keeps
// no labels; an agent named like another's scope suffix ("triage-u") can
// match too, which the admin can see from the listing's conversation column.
func (x *axAgent) ownsTask(agentName, name string) bool {
	for _, p := range []string{"sb-" + agentName, "sb-" + agentName + "-u", "sb-" + agentName + "-shared"} {
		if ax.NameMatcher(p)(name) {
			return true
		}
	}
	return false
}

// taskClients are one task's contract clients.
type taskClients struct {
	daemon    *daemon.Client
	approvals *approval.Client
}

// forTask is task's clients: the contract through the router, addressed to
// the task by the actor header. Built per call — they hold no connections of
// their own (the transport is shared), and a cache would grow with every
// conversation ever seen (caught in review).
func (x *axAgent) forTask(task string) (*taskClients, error) {
	cfg := daemon.Config{BaseURL: x.client.Router(), BearerToken: x.token, HTTPClient: x.client.HTTPClient(task), OmitAssertedCaller: x.omitCaller}
	dc, err := daemon.New(cfg)
	if err != nil {
		return nil, err
	}
	tc := &taskClients{daemon: dc}
	if x.approvals {
		if tc.approvals, err = approval.New(cfg); err != nil {
			return nil, err
		}
	}
	return tc, nil
}

// taskName is the task a conversation of this agent lives in: deterministic,
// so a restart or another gateway pod arrives at the same one. caller is who
// starts the conversation, which names an owner-scope task.
func (x *axAgent) taskName(agentName, gateway, conv, caller string) string {
	switch x.scope {
	case axScopeOwner:
		// The scope is hashed too, so agent "foo" here and agent "foo-u" in
		// thread scope never share a name (caught in review); the owner is
		// folded, as their identity compares (sameOwner).
		return ax.TaskName("sb-"+agentName+"-u", axScopeOwner, gateway, strings.ToLower(caller))
	case axScopeShared:
		return ax.TaskName("sb-"+agentName+"-shared", axScopeShared, gateway)
	}
	return ax.TaskName("sb-"+agentName, gateway, conv)
}

// axClients caches ax-server clients by everything that builds one, so the
// registry, which rebuilds every runtime agent on each change, does not open
// a connection to ax-server every time and leak the last one. It caches the
// router's mTLS clients the same way, by their files and server name: each
// holds two file watchers, and agents that share one share its connections.
type axClients struct {
	logf logging.Logf // hears a router certificate reload that failed; nil is silent

	mu      sync.Mutex
	clients map[string]axClient
	routers map[routerKey]*ax.RouterClient
}

// routerKey is what builds a router client. A struct, so no path can be
// spelled to collide with another pair (caught in review).
type routerKey struct{ bundle, trust, serverName string }

// router is the mTLS client for t, made on first use.
func (c *axClients) router(t ax.RouterTLS) (*ax.RouterClient, error) {
	t = t.WithDefaults()
	key := routerKey{t.CredentialBundleFile, t.TrustBundleFile, t.ServerName}
	c.mu.Lock()
	defer c.mu.Unlock()
	if rc, ok := c.routers[key]; ok {
		return rc, nil
	}
	if c.logf != nil {
		logf, files := c.logf, t.CredentialBundleFile+", "+t.TrustBundleFile
		t.OnError = func(err error) {
			logf.Warnf("ax router TLS (%s): reload failed, still using the last good files: %v", files, err)
		}
	}
	rc, err := ax.NewRouterClient(t)
	if err != nil {
		return nil, err
	}
	if c.routers == nil {
		c.routers = make(map[routerKey]*ax.RouterClient)
	}
	c.routers[key] = rc
	return rc, nil
}

// axClient is a cached client and what its template said about idling.
type axClient struct {
	client    *ax.Client
	idle      bool
	idleAfter time.Duration
}

func (c *axClients) get(key string, mk func() (axClient, error)) (axClient, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cl, ok := c.clients[key]; ok {
		return cl, nil
	}
	cl, err := mk()
	if err != nil {
		return axClient{}, err
	}
	if c.clients == nil {
		c.clients = make(map[string]axClient)
	}
	c.clients[key] = cl
	return cl, nil
}

// buildAXAgent validates an ax agent's entry and builds its clients.
func buildAXAgent(ac AgentConfig, b agentBuilder) (*axAgent, error) {
	c := ac.AX
	switch {
	case c == nil:
		return nil, errors.New(`.ax is required for kind "ax": server and task_template_file`)
	case ac.DaemonURL != "":
		return nil, errors.New(`.daemon_url is not for kind "ax": its tasks are reached through .ax.router`)
	case ac.Auth != nil:
		// The router's certificate is the pod's own, set by .ax.router_*;
		// ax-server takes no credentials (docs/ax-backend.md).
		return nil, errors.New(`.auth is not for kind "ax": the router is reached with the pod's certificate, set by .ax.router_credential_bundle_file and friends`)
	case strings.TrimSpace(ac.TokenEnv) == "":
		return nil, errors.New(".token_env is required: the variable holding the token the task's agent expects (its MAST_ATTACH_TOKEN)")
	case c.Server == "" || c.TaskTemplateFile == "":
		return nil, errors.New(".ax needs server and task_template_file")
	}
	router := c.Router
	if router == "" {
		router = ax.DefaultRouter
	}
	routerTLS := ax.RouterTLS{CredentialBundleFile: c.RouterCredentialBundleFile, TrustBundleFile: c.RouterTrustBundleFile, ServerName: c.RouterServerName}
	tlsRouter := ax.RouterIsTLS(router)
	if !tlsRouter && (c.RouterCredentialBundleFile != "" || c.RouterTrustBundleFile != "" || c.RouterServerName != "") {
		// They would be ignored, and a router meant to be authenticated
		// reached in plaintext without a word.
		return nil, fmt.Errorf(".ax.router %q: router_credential_bundle_file, router_trust_bundle_file and router_server_name are for an https router", router)
	}
	scope := c.Scope
	if scope == "" {
		scope = axScopeThread
	}
	switch scope {
	case axScopeThread, axScopeOwner, axScopeShared:
	default:
		return nil, fmt.Errorf(".ax.scope %q: want %q, %q or %q", c.Scope, axScopeThread, axScopeOwner, axScopeShared)
	}
	var ready time.Duration
	if c.ReadyTimeout != "" {
		d, err := time.ParseDuration(c.ReadyTimeout)
		if err != nil || d <= 0 || d > maxReadyTimeout {
			// Capped: every other message in the thread waits on a start.
			return nil, fmt.Errorf(".ax.ready_timeout %q: want a duration up to %s, like 2m", c.ReadyTimeout, maxReadyTimeout)
		}
		ready = d
	}
	token := os.Getenv(ac.TokenEnv)
	if token == "" {
		return nil, fmt.Errorf(": no agent token in $%s", ac.TokenEnv)
	}
	raw, err := os.ReadFile(c.TaskTemplateFile)
	if err != nil {
		return nil, fmt.Errorf(".ax.task_template_file: %w", err)
	}
	sum := sha256.Sum256(raw)
	cache := b.axCache()
	// No redirects through the router either (daemon.NoRedirects).
	hc := &http.Client{CheckRedirect: daemon.NoRedirects}
	if tlsRouter {
		routerTLS = routerTLS.WithDefaults()
		rc, err := cache.router(routerTLS)
		if err != nil {
			return nil, fmt.Errorf(".ax.router %s: %w", router, err)
		}
		hc = rc.HTTP
	}
	key := strings.Join([]string{c.Server, router, routerTLS.CredentialBundleFile, routerTLS.TrustBundleFile, routerTLS.ServerName,
		c.Atespace, c.TaskTemplateFile, hex.EncodeToString(sum[:]), ready.String()}, "|")
	cl, err := cache.get(key, func() (axClient, error) {
		// From the bytes the key hashes, not a second read (caught in
		// review: a file changed between the two was cached under the old
		// hash).
		t, err := ax.ParseTemplate(raw, c.TaskTemplateFile)
		if err != nil {
			return axClient{}, err
		}
		client, err := ax.New(ax.Config{
			Server: c.Server, Router: router, Atespace: c.Atespace, Template: t, ReadyTimeout: ready,
			HTTPClient: hc,
		})
		return axClient{client: client, idle: t.Idle, idleAfter: t.IdleAfter}, err
	})
	if err != nil {
		return nil, fmt.Errorf(".ax: %w", err)
	}
	linger := defaultStreamLinger
	if c.StreamLinger != "" {
		d, err := time.ParseDuration(c.StreamLinger)
		if err != nil || d < minStreamLinger {
			return nil, fmt.Errorf(".ax.stream_linger %q: want a duration of at least %s, like 1m", c.StreamLinger, minStreamLinger)
		}
		linger = d
	}
	deleteAfter := defaultDeleteAfter
	if c.DeleteAfter != "" {
		d, err := time.ParseDuration(c.DeleteAfter)
		if err != nil || d < 0 || d > maxDeleteAfter {
			return nil, fmt.Errorf(".ax.delete_after %q: want 0 (keep tasks) or a duration up to %s, like 336h", c.DeleteAfter, maxDeleteAfter)
		}
		deleteAfter = d
	}
	replace := c.ReplaceDrifted == nil || *c.ReplaceDrifted
	// A linger past suspendAfter only delays suspension (ax's idle clock
	// starts when the stream closes), so it is a startup warning, not an
	// error (caught in review: the check refused valid templates).
	return &axAgent{client: cl.client, token: token, scope: scope, approvals: b.approvals, idle: cl.idle, idleAfter: cl.idleAfter, linger: linger,
		deleteAfter: deleteAfter, replaceDrifted: replace, omitCaller: !assertsCaller(ac)}, nil
}

// axCache is the builder's client cache, created on first use.
func (b agentBuilder) axCache() *axClients {
	if b.ax != nil {
		return b.ax
	}
	return &axClients{} // a builder without one caches nothing
}

// --- in the router -------------------------------------------------------------

// maxReadyTimeout bounds ax.ready_timeout.
const maxReadyTimeout = 10 * time.Minute

// errTaskRestarted is a conversation whose task was gone and was started
// afresh, so its session is gone: what a turn waiting on the revive gets.
var errTaskRestarted = errors.New("this conversation's task was restarted; send the message again")

// errNeedsTask is an ax agent asked for a daemon with no task named: a path
// that addresses an agent rather than a conversation, such as an ingress
// adoption, which ax agents do not support.
var errNeedsTask = errors.New("this agent runs a task per conversation, and this operation names none")

// needsTask refuses every call, like goneDaemon.
type needsTask struct{}

func (needsTask) CreateSession(context.Context, string) (daemon.Session, error) {
	return daemon.Session{}, errNeedsTask
}
func (needsTask) Inject(context.Context, daemon.Session, string, string) error { return errNeedsTask }
func (needsTask) Subscribe(context.Context, daemon.Session, string, int64, func(daemon.Event) error) error {
	return errNeedsTask
}
func (needsTask) HeadSeq(context.Context, daemon.Session, string) (int64, error) {
	return 0, errNeedsTask
}

// setGateway names this gateway in its tasks' names, so two gateways in one
// cluster never share a conversation's task. Called once at startup.
func (r *Router) setGateway(id string) { r.gatewayID = id }

// startAXTask makes sure e's task is running and answers before anything is
// sent to it. A start that takes more than a moment says so in the thread:
// waking a task suspended for days can take a while, and silence would read
// as the app ignoring the message.
func (r *Router) startAXTask(ctx context.Context, conv string, a *agent, e *sessionEntry) (created bool, err error) {
	done := make(chan struct{})
	var notice chat.MessageRef
	var noticed sync.WaitGroup
	noticed.Add(1)
	go func() {
		defer noticed.Done()
		select {
		case <-done:
			return
		case <-time.After(axStartNoticeAfter):
		}
		pctx, cancel := platformContext(ctx)
		defer cancel()
		ref, err := r.out.Send(pctx, chat.Reply{Conversation: conv, Text: "⏳ Starting " + a.label() + "…", Kind: chat.KindProgress})
		if err == nil {
			notice = ref
		}
	}()
	start := time.Now()
	// A deletion of this task in flight finishes first: starting it now would
	// reuse a task about to vanish under the conversation (caught in review).
	err = r.waitTaskDelete(ctx, e.task)
	replaced := false
	if err == nil && a.ax.scope == axScopeThread && a.ax.replaceDrifted {
		// Made from an earlier template — an old token fails every request —
		// and a thread's task holds one conversation: replace it, before
		// waking it, which it may not even manage. The conversation starts
		// over in the new one (#173).
		var in ax.Inspection
		if in, err = a.ax.client.Inspect(ctx, e.task); err == nil && in.Exists && in.Drifted {
			r.logf.Warnf("session %s: replacing task %s, made from an earlier version of agent %s's template", conv, e.task, a.name)
			err = a.ax.client.Delete(ctx, e.task)
			replaced = err == nil
		}
	}
	var got ax.Ensured
	if err == nil {
		got, err = a.ax.client.Ensure(ctx, e.task)
		got.Created = got.Created || replaced
	}
	close(done)
	created = got.Created
	noticed.Wait()
	if notice.ID != "" {
		pctx, cancel := platformContext(ctx)
		r.retireMessage(pctx, conv, notice, start, err == nil)
		cancel()
	}
	if err != nil {
		r.logf.Warnf("session %s: start task %s for agent %s: %v", conv, e.task, a.name, err)
		return created, fmt.Errorf("%s could not be started: %w", a.label(), err)
	}
	if got.Drifted && !got.Created {
		// Tasks are immutable: one created before the template changed keeps
		// the old spec — an old token, say, which fails every request. Said,
		// so an operator can delete the task; replacing it is #173's.
		r.logf.Warnf("session %s: task %s was created from an earlier version of agent %s's template; it keeps the old spec until it is deleted", conv, e.task, a.name)
	}
	r.logf.Infof("session %s: task %s ready for agent %s in %s", conv, e.task, a.name, time.Since(start).Round(time.Millisecond))
	return created, nil
}

// Personal agents' threads (#172).
//
// An owner-scope thread belongs to the person who started it, recorded once
// — owners, persisted with the routing table — and never reassigned. It is
// the record, not the session or the agent's current scope, that decides:
// a session is rebuilt for many reasons (a task re-created, a lost session,
// a restart) and whoever spoke at that moment must not become the owner, and
// a scope changed in the config must not open a personal task to everyone
// (both caught in review).

// errNotOwner is a message or press in a personal agent's thread from
// someone other than its owner.
var errNotOwner = errors.New("this thread talks to its starter's personal agent")

// threadOwner is who a personal agent's thread belongs to.
type threadOwner struct {
	Owner string    `json:"owner"`
	Since time.Time `json:"since"`
}

// ownerMaxAge is how long a thread's owner is remembered: long after its
// session is forgotten, since forgetting it lets anyone take the thread.
const ownerMaxAge = 365 * 24 * time.Hour

// sameOwner compares identities as switchboard's approver lists do, folding
// case: an email's case does not make another person.
func sameOwner(a, b string) bool { return strings.EqualFold(a, b) }

// ownerOf is the owner of conv's thread, if it belongs to a personal agent.
func (r *Router) ownerOf(conv string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.owners[conv]
	return o.Owner, ok
}

// claimOwnerLocked records caller as conv's owner unless it has one, and
// reports whether caller owns it. The caller holds r.mu.
func (r *Router) claimOwnerLocked(conv, caller string) bool {
	if o, ok := r.owners[conv]; ok {
		return sameOwner(o.Owner, caller)
	}
	if r.owners == nil {
		r.owners = make(map[string]threadOwner)
	}
	r.owners[conv] = threadOwner{Owner: caller, Since: time.Now().UTC()}
	r.markDirty() // a non-blocking send: safe under mu
	return true
}

// refuseNonOwner tells conv's thread, at most once per caller every
// ownerNoticeEvery, that it answers its owner alone: posted in the thread,
// so a stranger repeating themselves must not turn it into a stream.
func (r *Router) refuseNonOwner(ctx context.Context, conv, caller string) {
	key := conv + "\x00" + strings.ToLower(caller)
	r.mu.Lock()
	last, seen := r.ownerNoticed[key]
	if seen && time.Since(last) < ownerNoticeEvery {
		r.mu.Unlock()
		return
	}
	if r.ownerNoticed == nil {
		r.ownerNoticed = make(map[string]time.Time)
	}
	r.ownerNoticed[key] = time.Now()
	r.mu.Unlock()
	if err := r.surfaceNotice(ctx, conv, noticeNotOwner); err != nil {
		r.logf.Errorf("handle %s: surface personal agent: %v", conv, err)
	}
}

// ownerNoticeEvery spaces the not-the-owner notice per caller and thread.
var ownerNoticeEvery = 10 * time.Minute

const noticeNotOwnerPress = "This thread talks to its starter's personal agent: its questions are theirs to answer."

const noticeNotOwner = "This thread talks to its starter's personal agent, which answers them alone. Start a thread of your own to talk to yours."

// The delete gate (caught in review). A task is deleted outside the router's
// lock, and a conversation can wake its task meanwhile: without a gate, the
// sweep deleted a task its conversation had just resumed, leaving a live
// conversation on a deleted task. A deleter takes the gate after checking,
// under the lock, that nothing is using the task; a start waits on it.

// beginTaskDeleteLocked takes task's delete gate, or reports it taken. The
// caller holds r.mu and calls done when the delete is over.
func (r *Router) beginTaskDeleteLocked(task string) (done func(), ok bool) {
	if _, busy := r.axDeleting[task]; busy {
		return nil, false
	}
	if r.axDeleting == nil {
		r.axDeleting = make(map[string]chan struct{})
	}
	ch := make(chan struct{})
	r.axDeleting[task] = ch
	return func() {
		r.mu.Lock()
		delete(r.axDeleting, task)
		r.mu.Unlock()
		close(ch)
	}, true
}

// waitTaskDelete waits out a deletion of task in flight.
func (r *Router) waitTaskDelete(ctx context.Context, task string) error {
	r.mu.Lock()
	ch, busy := r.axDeleting[task]
	r.mu.Unlock()
	if !busy {
		return nil
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Why a task was deleted, for what its conversation is told.
const (
	taskDeletedQuiet = "quiet"
	taskDeletedAdmin = "admin"
)

// taskDeletedNotice is what a conversation whose task was deleted is told
// when it next speaks.
func taskDeletedNotice(label, reason string) string {
	if reason == taskDeletedAdmin {
		return label + "'s task for this conversation was deleted by an administrator, so this conversation starts over."
	}
	return label + "'s task for this conversation was cleaned up after it went quiet, so this conversation starts over."
}

// taskNote is what switchboard's records say about one task.
type taskNote struct {
	convs []string
	owner string
	last  time.Time
}

// taskNotes gathers, per task, the conversations recorded in it, its owner
// and its last traffic, from live entries and dormant records alike.
func (r *Router) taskNotes() map[string]taskNote {
	notes := map[string]taskNote{}
	add := func(task, conv string, touched time.Time) {
		if task == "" {
			return
		}
		n := notes[task]
		n.convs = append(n.convs, conv)
		if o, ok := r.owners[conv]; ok {
			n.owner = o.Owner
		}
		if touched.After(n.last) {
			n.last = touched
		}
		notes[task] = n
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for conv, e := range r.sessions {
		select {
		case <-e.ready:
			add(e.task, conv, time.Unix(0, e.touched.Load()).UTC())
		default:
		}
	}
	for conv, rec := range r.dormant {
		if !rec.TaskDeleted {
			add(rec.Task, conv, rec.Touched)
		}
	}
	for _, n := range notes {
		sort.Strings(n.convs)
	}
	return notes
}

// axStartNoticeAfter is how long a task start runs before the thread is told.
var axStartNoticeAfter = 2 * time.Second
