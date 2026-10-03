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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-steer/switchboard/pkg/daemon"
)

// Durable routing state (#86).
//
// Every conversation → session mapping lived in one process's memory, so a
// restart — a rollout, a node drain, an OOM — reset every thread without
// telling anyone in it, and left each conversation's session on the daemon
// with nothing that could ever find it again: the session is created with an
// empty body, so nothing on the daemon ties it to the conversation that caused
// it (go-steer/core-agent#995 would let it; until then the record is ours).
//
// So the routing table is written down. Not every turn — a snapshot, debounced,
// of what it takes to pick a conversation up where it was: its session, the
// identity its relay subscribes as, whether it was adopted, and how far its
// stream had been delivered. On boot a conversation that was recently active
// re-attaches at once, so an answer produced while switchboard was down still
// arrives; an older one stays dormant and re-attaches on its next message. A
// dormant record is a session the conversation has, exactly as a live entry is,
// and the binding rules treat it so.
//
// File-backed, behind an interface that does not presume a file. One writer:
// a second replica needs a shared store, and that is a different requirement
// from surviving a restart.

// stateVersion is the on-disk format. A file with a different version is not
// guessed at; see loadState.
const stateVersion = 1

// stateFileName is the snapshot inside --state-dir.
const stateFileName = "state.json"

// persistDebounce is how long the persister waits after the first change
// before writing, so a burst — a turn's tool calls advancing the notice
// watermark one by one — is one write rather than many.
const persistDebounce = 500 * time.Millisecond

// reviveWindow is how recently a conversation must have had traffic to be
// re-attached at boot rather than on its next message. An hour covers the
// turns a restart can interrupt and the answers that land after it, without
// opening a stream for every thread a workspace has ever touched.
const reviveWindow = time.Hour

// dormantMaxAge is how long a record is kept for a conversation nobody has
// spoken in. Past it the thread starts fresh if it ever wakes, which is all
// that was possible before records existed, and the file stops growing with
// every thread ever touched.
const dormantMaxAge = 30 * 24 * time.Hour

// errStateUnreadable marks a snapshot that is there but cannot be used —
// malformed, or another format — as distinct from one that cannot be read at
// all. Only the first is safe to set aside: a permission or I/O error says
// nothing about the file, and moving a good one aside would lose it.
var errStateUnreadable = errors.New("unreadable")

// routerState is the snapshot. Field names are a format a restarted process
// reads, so they change only additively; a breaking change bumps stateVersion.
type routerState struct {
	Version   int                      `json:"version"`
	Sessions  map[string]sessionRecord `json:"sessions,omitempty"`
	Bindings  []bindingRecord          `json:"bindings,omitempty"`
	Overrides map[string]string        `json:"overrides,omitempty"`
}

// sessionRecord is what it takes to re-attach one conversation.
type sessionRecord struct {
	Channel string `json:"channel,omitempty"`
	// Session is "app/sid".
	Session string `json:"session"`
	// Owner is the identity the relay subscribes as: the creator, or empty for
	// an adopted session (sessionEntry.owner).
	Owner   string `json:"owner,omitempty"`
	Adopted bool   `json:"adopted,omitempty"`
	// Relayed and Noticed are the delivery watermarks: the last answer whose
	// post completed, and the last tool notice posted. The stream resumes from
	// Relayed, and neither watermark lets a replayed event be posted twice.
	Relayed int64 `json:"relayed"`
	Noticed int64 `json:"noticed,omitempty"`
	// Attempted is the highest seq an answer post was started for, written
	// before the post. Answers past Relayed and up to it are the ones that
	// may already be in the thread, and are verified before being posted.
	Attempted int64 `json:"attempted,omitempty"`
	// Touched is the last traffic in either direction.
	Touched time.Time `json:"touched"`
	// Asked is the questions this conversation has on screen and unanswered.
	// Without them a revived entry's prompt watcher, seeded with everything
	// still pending, would post each question again beside the one whose
	// buttons are still live.
	Asked []askedRecord `json:"asked,omitempty"`
	// Placeholder is the progress message a turn in flight had on screen.
	// Its ticker died with the process, so a revived entry takes it down
	// rather than leave "Working… 3m" frozen in the thread.
	Placeholder string `json:"placeholder,omitempty"`
}

// askedRecord is one unanswered question, as the thread was shown it.
type askedRecord struct {
	ID   string `json:"id"`
	Text string `json:"text,omitempty"`
	Kind string `json:"kind,omitempty"`
}

// bindingRecord is one outbound-ingress binding (#38), in bindOrder.
type bindingRecord struct {
	Conversation string `json:"conversation"`
	Session      string `json:"session"`
	Since        int64  `json:"since"`
}

// stateStore persists the snapshot. The file is the only implementation; the
// interface is the seam a shared store would replace it at.
type stateStore interface {
	load() (routerState, error)
	save(routerState) error
	where() string
}

// fileStore keeps the snapshot as one JSON file, replaced atomically.
type fileStore struct {
	mu   sync.Mutex // one write at a time: the temp file name is fixed
	path string
}

// openFileStore prepares dir to hold the snapshot. The directory is created if
// missing, readable by its owner only, since the snapshot names who spoke in
// every conversation it holds.
func openFileStore(dir string) (*fileStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("state dir: %w", err)
	}
	return &fileStore{path: filepath.Join(dir, stateFileName)}, nil
}

func (s *fileStore) where() string { return s.path }

// load reads the snapshot. A missing file is an empty state — a first run.
func (s *fileStore) load() (routerState, error) {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return routerState{Version: stateVersion}, nil
	}
	if err != nil {
		return routerState{}, err
	}
	var st routerState
	if err := json.Unmarshal(raw, &st); err != nil {
		return routerState{}, fmt.Errorf("%s: %w: %v", s.path, errStateUnreadable, err)
	}
	if st.Version != stateVersion {
		return routerState{}, fmt.Errorf("%s: %w: version %d, this build reads %d", s.path, errStateUnreadable, st.Version, stateVersion)
	}
	return st, nil
}

// save replaces the snapshot: written to a temporary file, synced, and renamed
// over the old one, so a crash mid-write leaves the previous snapshot rather
// than half of a new one.
func (s *fileStore) save(st routerState) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	// The rename is durable once the directory is; best effort, since a
	// filesystem that cannot sync a directory still renamed the file.
	if d, err := os.Open(filepath.Dir(s.path)); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// setAside moves an unreadable snapshot out of the way so the run can start
// from nothing, keeping it for whoever wants to know what was in it. Starting
// empty loses the mapping; refusing to start would turn a corrupt file into a
// crash loop that loses everything else too.
func (s *fileStore) setAside(now time.Time) (string, error) {
	dest := fmt.Sprintf("%s.unreadable-%s", s.path, now.UTC().Format("20060102T150405Z"))
	return dest, os.Rename(s.path, dest)
}

// setStore installs the state store and its persister's signal. Called once at
// startup, before restore.
func (r *Router) setStore(s stateStore) {
	r.store = s
	r.dirty = make(chan struct{}, 1)
}

// markDirty schedules a write. Cheap and non-blocking: a change made while a
// write is already pending is covered by that write's snapshot.
func (r *Router) markDirty() {
	if r.dirty == nil {
		return
	}
	select {
	case r.dirty <- struct{}{}:
	default:
	}
}

// runPersister writes the snapshot after each burst of changes until ctx ends.
// The final write is the caller's (saveState after the adapter returns), so a
// change made during shutdown is not lost to a cancelled debounce.
func (r *Router) runPersister(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.dirty:
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(persistDebounce):
		}
		r.saveState()
	}
}

// saveState writes the snapshot now. A failure is logged and the next change
// tries again: the gateway keeps routing either way, and what is lost is only
// the ability to pick up where it was if it restarts before a write succeeds.
func (r *Router) saveState() {
	if err := r.trySaveState(); err != nil {
		r.logf.Errorf("state: save to %s: %v", r.store.where(), err)
	}
}

// trySaveState is saveState reporting its error, for the write at startup that
// proves the directory is writable.
//
// The snapshot and the write are one step under saveMu. Otherwise a persister
// write already in progress at shutdown could take its snapshot first and land
// its file last, over the newer final one.
func (r *Router) trySaveState() error {
	if r.store == nil {
		return nil
	}
	r.saveMu.Lock()
	defer r.saveMu.Unlock()
	return r.store.save(r.snapshot())
}

// snapshot captures the routing table. Entries still opening, or whose opening
// failed, are left out: there is no session to record yet, or ever.
func (r *Router) snapshot() routerState {
	st := routerState{Version: stateVersion}
	r.mu.Lock()
	if n := len(r.sessions) + len(r.dormant); n > 0 {
		st.Sessions = make(map[string]sessionRecord, n)
	}
	for conv, rec := range r.dormant {
		st.Sessions[conv] = rec
	}
	for conv, e := range r.sessions {
		select {
		case <-e.ready:
		default:
			continue
		}
		if e.err != nil {
			continue
		}
		st.Sessions[conv] = e.record()
	}
	for _, conv := range r.bindOrder {
		b, ok := r.bindings[conv]
		if !ok {
			continue
		}
		st.Bindings = append(st.Bindings, bindingRecord{Conversation: conv, Session: sessionRef(b.sess), Since: b.since})
	}
	r.mu.Unlock()

	r.omu.Lock()
	if len(r.overrides) > 0 {
		st.Overrides = make(map[string]string, len(r.overrides))
		for ch, m := range r.overrides {
			st.Overrides[ch] = string(m)
		}
	}
	r.omu.Unlock()
	return st
}

// record is the entry's durable form.
func (e *sessionEntry) record() sessionRecord {
	rec := sessionRecord{
		Channel: e.channel,
		Session: sessionRef(e.sess),
		Owner:   e.owner,
		Adopted: e.adopted,
		// delivered, not relayed: relayed moves before the post is made, so a
		// post cut off by shutdown would otherwise be recorded as made.
		Relayed:   e.delivered.Load(),
		Noticed:   e.noticed.Load(),
		Attempted: e.attempted.Load(),
		Touched:   time.Unix(0, e.touched.Load()).UTC(),
	}
	e.qmu.Lock()
	for id, a := range e.asked {
		if a.settled == unsettled && a.posted {
			rec.Asked = append(rec.Asked, askedRecord{ID: id, Text: a.text, Kind: a.kind})
		}
	}
	e.qmu.Unlock()
	// Only a live placeholder: one a ticker is still rendering, or one for a
	// turn still in flight. A placeholder left standing on purpose — the
	// frozen "Working… 2m30s" that is the only trace of a turn that ended
	// without an answer, or a status trail — is part of the thread, and a
	// revive must not take it down.
	e.pmu.Lock()
	if e.tickStop != nil || e.turnInFlight() {
		rec.Placeholder = e.progressMsg.ID
	}
	e.pmu.Unlock()
	return rec
}

// touch records traffic on the entry, for the snapshot and the revive window.
func (e *sessionEntry) touch() { e.touched.Store(time.Now().UnixNano()) }

// entryFromRecord rebuilds an entry from its record, not yet ready and with no
// relay. The stream resumes from the last answer delivered; the notice
// watermark is raised to it, so a notice older than the last answer — one a
// mode that posts no notices never advanced past — is not posted late.
func entryFromRecord(rec sessionRecord, channel string) (*sessionEntry, error) {
	sess, err := parseSessionRef(rec.Session)
	if err != nil {
		return nil, err
	}
	if channel == "" {
		channel = rec.Channel
	}
	e := &sessionEntry{
		ready:   make(chan struct{}),
		sess:    sess,
		channel: channel,
		adopted: rec.Adopted,
		owner:   rec.Owner,
	}
	e.seq.Store(rec.Relayed)
	e.relayed.Store(rec.Relayed)
	e.delivered.Store(rec.Relayed)
	e.attempted.Store(rec.Attempted)
	// Anything the dead process started posting and did not see finish.
	if rec.Attempted > rec.Relayed {
		e.verifyThrough.Store(rec.Attempted)
	}
	e.noticed.Store(max(rec.Noticed, rec.Relayed))
	e.touched.Store(rec.Touched.UnixNano())
	// A new process is a new connection: whatever turn was in flight was not
	// watched whole by this one, so an answer replayed into it is taken as
	// the answer rather than as narration awaiting a turn-complete that will
	// never be replayed (lifecycle frames carry no seq).
	e.streamGen.Store(1)
	if len(rec.Asked) > 0 {
		e.asked = make(map[string]*askRecord, len(rec.Asked))
		for _, a := range rec.Asked {
			e.asked[a.ID] = &askRecord{text: a.Text, kind: a.Kind, posted: true}
		}
	}
	return e, nil
}

// restore loads a snapshot into a router that has not started dispatching.
// Every conversation becomes dormant; those with traffic inside reviveWindow
// are re-attached straight away, on ctx, which is the serve context their
// relays will run on. It returns how many of each.
func (r *Router) restore(ctx context.Context, st routerState, now time.Time) (revived, dormant int) {
	r.mu.Lock()
	for _, b := range st.Bindings {
		sess, err := parseSessionRef(b.Session)
		if err != nil {
			r.logf.Warnf("state: dropping the binding of %s: %v", b.Conversation, err)
			continue
		}
		r.bindings[b.Conversation] = binding{sess: sess, since: b.Since}
		r.boundTo[b.Session] = b.Conversation
		r.bindOrder = append(r.bindOrder, b.Conversation)
		r.metrics.bindRecorded()
	}
	r.evictBindings()
	var recent []string
	for conv, rec := range st.Sessions {
		if _, err := parseSessionRef(rec.Session); err != nil {
			r.logf.Warnf("state: dropping the session of %s: %v", conv, err)
			continue
		}
		if now.Sub(rec.Touched) > dormantMaxAge {
			continue
		}
		r.dormant[conv] = rec
		if now.Sub(rec.Touched) <= r.reviveWindow() {
			recent = append(recent, conv)
		}
	}
	dormant = len(r.dormant) - len(recent)
	r.mu.Unlock()

	r.omu.Lock()
	for ch, m := range st.Overrides {
		if mode, err := parseProgressMode(m); err == nil {
			r.overrides[ch] = mode
		}
	}
	r.omu.Unlock()

	for _, conv := range recent {
		r.mu.Lock()
		rec, ok := r.dormant[conv]
		r.mu.Unlock()
		if !ok {
			continue
		}
		if _, err := r.session(ctx, conv, rec.Channel, rec.Owner); err != nil {
			r.logf.Warnf("state: re-attaching %s: %v", conv, err)
			continue
		}
		revived++
	}
	return revived, dormant
}

// reviveWindow is how recent a restored conversation's traffic must be for it
// to re-attach at boot: the idle TTL when reaping is on, since anything inside
// it would still have been live had the process not restarted, and the
// default window otherwise.
func (r *Router) reviveWindow() time.Duration {
	if r.idleTTL > 0 {
		return r.idleTTL
	}
	return reviveWindow
}

// otherSession reports whether conv already has a session other than sess —
// live or dormant — the test both halves of a bind make. The caller holds r.mu.
func (r *Router) otherSession(conv string, sess daemon.Session) bool {
	if e, ok := r.sessions[conv]; ok && !runningSession(e, sess) {
		return true
	}
	if rec, ok := r.dormant[conv]; ok && !(rec.Adopted && rec.Session == sessionRef(sess)) {
		return true
	}
	return false
}
