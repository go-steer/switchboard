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
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Outbound thread context: what an unbound ingress post said before anyone
// replied to it.
//
// When a daemon caller opens a thread through POST /v1/messages and names a
// session, switchboard binds the thread to that session (binding.go). When the
// caller names no session — a monitoring pipeline, or an autonomous ax task
// whose conversation agent runs one task per thread and therefore has no
// shared daemon session to bind — a human replying in the thread opens a fresh
// session. Switchboard holds no chat-history scope on either platform, so
// without remembering what its own ingress just posted into that thread, the
// new session starts blind to the message at the head of it.
//
// The router therefore keeps a bounded record of unbound outbound threads —
// posts, in-place edits, appends, and continuation rollovers — until the
// thread's first inbound turn reaches the agent, which carries that thread
// context ahead of the human's reply once and then forgets it.
const (
	maxOutboundThreads       = 1024
	maxOutboundMsgsPerThread = 64
	maxOutboundSeedBytes     = 32 << 10
)

const outboundTruncatedSuffix = "\n…[truncated]"

// outboundMsg is one message switchboard's outbound ingress posted into an
// unbound thread, kept in order so later in-place PATCHes edit the right part.
type outboundMsg struct {
	ID        string `json:"id,omitempty"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated,omitempty"`
}

// outboundThread is what the outbound ingress has put into one thread that has
// not yet delivered its first inbound turn.
type outboundThread struct {
	msgs      []outboundMsg
	touched   time.Time
	truncated bool
}

// formatSeededTurn prepends remembered outbound thread context to the first
// inbound message of a freshly opened session.
func formatSeededTurn(seed, reply string) string {
	if seed == "" {
		return reply
	}
	return "[Thread context — earlier message posted in this thread]\n" +
		seed +
		"\n[End thread context]\n\n" +
		reply
}

// NoteOutbound records a message posted into conv by an unbound outbound
// ingress call (an initial POST without a session, or an append rollover
// continuation). No-op when conv names no thread, belongs to a user-opened or
// bound conversation, or has already run a turn.
func (r *Router) NoteOutbound(conv, id, text string) {
	text = sanitizeUTF8(text)
	if !namesThread(conv) || strings.TrimSpace(text) == "" {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.canRecordOutboundLocked(conv) {
		return
	}
	t, ok := r.outbound[conv]
	if !ok {
		t = &outboundThread{}
		r.outbound[conv] = t
		r.outboundOrder = append(r.outboundOrder, conv)
	}
	t.touched = time.Now().UTC()
	updated := false
	if id != "" {
		for i := range t.msgs {
			if t.msgs[i].ID == id {
				t.msgs[i].Text = text
				t.msgs[i].Truncated = false
				updated = true
				break
			}
		}
	}
	if !updated {
		t.msgs = append(t.msgs, outboundMsg{ID: id, Text: text})
	}
	t.capLimits()
	r.evictOutboundLocked()
	r.markDirty()
}

// EditOutbound updates a message's remembered text in conv after an in-place
// PATCH (replace or non-rolling append). If conv is a bare channel or space,
// it resolves the thread against any remembered message with id in that
// channel (or Slack's channel:id root-thread identity).
func (r *Router) EditOutbound(conv, id, text string) {
	text = sanitizeUTF8(text)
	if id == "" || strings.TrimSpace(text) == "" {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	target := conv
	if !namesThread(target) {
		target = r.resolveOutboundThreadLocked(conv, id)
	}
	if !namesThread(target) || !r.canRecordOutboundLocked(target) {
		return
	}
	t, ok := r.outbound[target]
	if !ok {
		t = &outboundThread{
			msgs:    []outboundMsg{{ID: id, Text: text}},
			touched: time.Now().UTC(),
		}
		t.capLimits()
		r.outbound[target] = t
		r.outboundOrder = append(r.outboundOrder, target)
		r.evictOutboundLocked()
		r.markDirty()
		return
	}
	t.touched = time.Now().UTC()
	for i := range t.msgs {
		if t.msgs[i].ID == id {
			t.msgs[i].Text = text
			t.msgs[i].Truncated = false
			t.capLimits()
			r.markDirty()
			return
		}
	}
	t.msgs = append(t.msgs, outboundMsg{ID: id, Text: text})
	t.capLimits()
	r.markDirty()
}

// resolveOutboundThreadLocked finds the threaded conversation key for a PATCH
// addressed by bare channel/space and message id. The caller holds r.mu.
func (r *Router) resolveOutboundThreadLocked(bareConv, id string) string {
	ch := channelPart(bareConv)
	if ch == "" {
		return ""
	}
	if _, ok := r.outbound[ch+":"+id]; ok {
		return ch + ":" + id
	}
	for _, conv := range r.outboundOrder {
		if channelPart(conv) != ch {
			continue
		}
		t, ok := r.outbound[conv]
		if !ok {
			continue
		}
		for _, m := range t.msgs {
			if m.ID == id {
				return conv
			}
		}
	}
	return ""
}

func channelPart(conv string) string {
	ch, _, _ := strings.Cut(conv, ":")
	return ch
}

// canRecordOutboundLocked reports whether conv may record or revise outbound
// thread context. Starting a new record requires conv to have no session at
// all; an already-tracked thread may keep taking progressive PATCH edits and
// continuations while its session is opening (or retrying a failed first
// Inject), right up until the session accepts or runs a turn. The caller holds
// r.mu.
func (r *Router) canRecordOutboundLocked(conv string) bool {
	if _, bound := r.bindings[conv]; bound {
		return false
	}
	_, tracked := r.outbound[conv]
	if e, ok := r.sessions[conv]; ok {
		return tracked && !e.sessionProgressed()
	}
	if rec, ok := r.dormant[conv]; ok {
		return tracked && !rec.recordProgressed()
	}
	return true
}

// sessionProgressed reports whether e belongs to an adopted thread or has
// already accepted or run a turn, in which case it must never receive an
// initial outbound seed.
func (e *sessionEntry) sessionProgressed() bool {
	if e.adopted || e.seeded.Load() ||
		e.seq.Load() > 0 || e.relayed.Load() > 0 || e.delivered.Load() > 0 ||
		e.attempted.Load() > 0 || e.noticed.Load() > 0 {
		return true
	}
	e.qmu.Lock()
	asked := len(e.asked) > 0
	e.qmu.Unlock()
	return asked
}

// recordProgressed is sessionProgressed on a durable dormant record.
func (rec sessionRecord) recordProgressed() bool {
	return rec.Adopted || rec.Seeded ||
		rec.Relayed > 0 || rec.Attempted > 0 || rec.Noticed > 0 ||
		len(rec.Asked) > 0
}

// outboundSeed renders the current unconsumed outbound thread context for conv
// when entry is eligible for first-turn seeding.
func (r *Router) outboundSeed(conv string, entry *sessionEntry) string {
	if entry.sessionProgressed() {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, bound := r.bindings[conv]; bound {
		return ""
	}
	t, ok := r.outbound[conv]
	if !ok {
		return ""
	}
	return t.render()
}

// markSeeded records that e has accepted or run a turn and forgets any
// unconsumed outbound seed for conv.
func (r *Router) markSeeded(conv string, e *sessionEntry) {
	e.seeded.Store(true)
	r.clearOutbound(conv)
}

// injectTurn hands one inbound message to entry's daemon, carrying any
// remembered outbound thread context on the session's first accepted turn.
// Concurrent turns waking on a freshly opened session serialize on entry.seedMu
// until the first turn succeeds (consuming the seed in order before later turns
// proceed bare) or fails (preserving the seed for the next attempt).
func (r *Router) injectTurn(ctx context.Context, conv string, entry *sessionEntry, caller, text string) error {
	locked := false
	if !entry.seeded.Load() {
		entry.seedMu.Lock()
		locked = true
		defer entry.seedMu.Unlock()
	}
	seed := ""
	if locked && !entry.seeded.Load() {
		seed = r.outboundSeed(conv, entry)
	}
	start := time.Now()
	err := r.daemonOf(entry).Inject(ctx, entry.sess, caller, formatSeededTurn(seed, text))
	r.metrics.recordDaemon("inject", time.Since(start), err)
	if err == nil && (locked || seed != "") {
		r.markSeeded(conv, entry)
	}
	return err
}

// clearOutbound forgets conv's outbound seed once the thread's first inbound
// turn has been accepted by the agent backend.
func (r *Router) clearOutbound(conv string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clearOutboundLocked(conv)
}

// clearOutboundLocked forgets conv's outbound seed. The caller holds r.mu.
func (r *Router) clearOutboundLocked(conv string) {
	if _, ok := r.outbound[conv]; !ok {
		return
	}
	delete(r.outbound, conv)
	r.outboundOrder = slices.DeleteFunc(r.outboundOrder, func(c string) bool { return c == conv })
	r.markDirty()
}

// evictOutboundLocked bounds r.outbound to maxOutboundThreads, oldest first.
// The caller holds r.mu.
func (r *Router) evictOutboundLocked() {
	for len(r.outboundOrder) > maxOutboundThreads {
		oldest := r.outboundOrder[0]
		r.outboundOrder = r.outboundOrder[1:]
		if _, ok := r.outbound[oldest]; !ok {
			continue
		}
		delete(r.outbound, oldest)
		r.markDirty()
	}
}

// render joins the thread's outbound messages in order, bounded to
// maxOutboundSeedBytes and ending with outboundTruncatedSuffix whenever any
// message or byte was dropped.
func (t *outboundThread) render() string {
	if t == nil || len(t.msgs) == 0 {
		return ""
	}
	anyTrunc := t.truncated
	var parts []string
	for _, m := range t.msgs {
		if m.Truncated {
			anyTrunc = true
		}
		if strings.TrimSpace(m.Text) != "" {
			parts = append(parts, m.Text)
		}
	}
	if len(parts) == 0 {
		if anyTrunc {
			return strings.TrimPrefix(outboundTruncatedSuffix, "\n")
		}
		return ""
	}
	joined := strings.Join(parts, "\n\n")
	if !anyTrunc {
		return clampUTF8Bytes(joined, maxOutboundSeedBytes)
	}
	head := cutValidUTF8(joined, maxOutboundSeedBytes-len(outboundTruncatedSuffix))
	return head + outboundTruncatedSuffix
}

// capLimits bounds both message count (maxOutboundMsgsPerThread) and stored
// text bytes (maxOutboundSeedBytes, reserving space for outboundTruncatedSuffix
// whenever truncation occurs) while retaining message IDs within the 64-message
// window so bare-space PATCHes continue to resolve their thread.
func (t *outboundThread) capLimits() {
	if len(t.msgs) > maxOutboundMsgsPerThread {
		t.msgs = t.msgs[:maxOutboundMsgsPerThread]
		t.truncated = true
	}
	anyTrunc := t.truncated
	total := 0
	nonEmpty := 0
	for _, m := range t.msgs {
		if m.Truncated {
			anyTrunc = true
		}
		if strings.TrimSpace(m.Text) == "" {
			continue
		}
		if nonEmpty > 0 {
			total += 2 // "\n\n"
		}
		total += len(m.Text)
		nonEmpty++
	}
	if !anyTrunc && total <= maxOutboundSeedBytes {
		return
	}
	budget := maxOutboundSeedBytes - len(outboundTruncatedSuffix)
	kept := 0
	for i := range t.msgs {
		if strings.TrimSpace(t.msgs[i].Text) == "" {
			t.msgs[i].Text = ""
			continue
		}
		sep := 0
		if kept > 0 {
			sep = 2
		}
		if budget <= sep {
			t.msgs[i].Text = ""
			t.msgs[i].Truncated = true
			continue
		}
		avail := budget - sep
		if len(t.msgs[i].Text) > avail {
			t.msgs[i].Text = cutValidUTF8(t.msgs[i].Text, avail)
			t.msgs[i].Truncated = true
			budget = 0
		} else {
			budget -= sep + len(t.msgs[i].Text)
		}
		if strings.TrimSpace(t.msgs[i].Text) != "" {
			kept++
		}
	}
}

// sanitizeUTF8 replaces any invalid UTF-8 byte sequence in s with U+FFFD in a
// single pass, returning valid inputs without allocating.
func sanitizeUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "\uFFFD")
}

// cutValidUTF8 returns a valid UTF-8 prefix of s of at most limit bytes. Once
// sanitized, stepping back past any split continuation bytes at the cut takes
// at most utf8.UTFMax-1 checks.
func cutValidUTF8(s string, limit int) string {
	s = sanitizeUTF8(s)
	if limit <= 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// clampUTF8Bytes truncates s to at most limit bytes on a valid UTF-8 rune
// boundary, appending outboundTruncatedSuffix when limit allows.
func clampUTF8Bytes(s string, limit int) string {
	s = sanitizeUTF8(s)
	if len(s) <= limit {
		return s
	}
	if limit <= 0 {
		return ""
	}
	if limit <= len(outboundTruncatedSuffix) {
		return cutValidUTF8(s, limit)
	}
	return cutValidUTF8(s, limit-len(outboundTruncatedSuffix)) + outboundTruncatedSuffix
}
