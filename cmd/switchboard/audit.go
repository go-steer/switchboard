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
	"io"
	"os"
	"sync"
	"time"

	"github.com/go-steer/switchboard/internal/logging"
	"github.com/go-steer/switchboard/pkg/chat"
	"github.com/go-steer/switchboard/pkg/daemon"
)

// The ingress audit record (#89).
//
// switchboard is the one component that sees the plaintext chat identity: it
// resolves who spoke and asserts it to the daemon, which trusts the assertion
// for ACLs, approvals and attribution. So the join an investigation needs —
// this chat message, from this person, caused this session's turn — exists
// here and nowhere else, and before this it existed only for the duration of
// one function call.
//
// One JSON line per turn and per press, on a sink of its own. Not the
// operational log, whose format, retention and readers are all different, and
// not metrics, where an identity label is a cardinality mistake. Never the
// message text: the chat platform is the system of record for content, with its
// own retention and legal posture, and the platform message ID is what leads
// back to it.

// auditStdout is the --audit-log value meaning standard output. The
// operational log goes to standard error, so this is a clean separate stream
// for a container's log shipper to route on its own.
const auditStdout = "stdout"

// Audit record kinds.
const (
	auditTurn  = "turn"
	auditPress = "press"
)

// Turn outcomes.
const (
	auditInjected     = "injected"      // the daemon took the message
	auditNoSession    = "no_session"    // no session could be opened or found for it
	auditInjectFailed = "inject_failed" // the daemon refused or could not be reached
)

// Press outcomes. Each is a distinct exit from HandlePress, named for what
// happened to the answer rather than for the notice the thread was shown.
const (
	auditApplied          = "applied"            // the daemon applied it
	auditDisabled         = "disabled"           // approvals are off for this channel
	auditInvalid          = "invalid"            // not a decision, or no prompt named
	auditNotApprover      = "not_approver"       // refused by --approvers
	auditNotStanding      = "not_standing"       // refused by --standing-approvers
	auditStale            = "stale"              // the thread no longer has that session
	auditSettledElsewhere = "settled_elsewhere"  // answered already, or expired
	auditRefusedOrSettled = "refused_or_settled" // not pending, or the daemon refused the presser (#106)
	auditMaybeApplied     = "maybe_applied"      // the daemon took it and did not confirm
	auditFailed           = "failed"             // it did not reach the daemon
	auditPressError       = "error"              // anything else
)

// auditRecord is one line. Field names are a format operators will parse, so
// they change only additively.
type auditRecord struct {
	Time         string `json:"time"`
	Kind         string `json:"kind"`
	Conversation string `json:"conversation"`
	Channel      string `json:"channel,omitempty"`
	// Caller is exactly what was put in X-Asserted-Caller, so a daemon audit
	// entry can be matched on it.
	Caller string `json:"caller"`
	// Session is "app/sid": for a turn, the session it was injected into; for
	// a press, the session the question belonged to.
	Session string `json:"session,omitempty"`
	// MessageID is the platform's handle on the message — the turn's, or the
	// question's for a press.
	MessageID string `json:"message_id,omitempty"`
	Outcome   string `json:"outcome"`
	// Status is the daemon's HTTP status when it refused, for a failure that
	// has one.
	Status int `json:"status,omitempty"`

	// Press only.
	PromptID string `json:"prompt_id,omitempty"`
	Decision string `json:"decision,omitempty"`
	// Approver is who the daemon says it recorded, which can differ from
	// Caller and is the one to believe.
	Approver string `json:"approver,omitempty"`
}

// auditLog writes records. A nil *auditLog records nothing, so the router can
// call it unconditionally.
type auditLog struct {
	mu     sync.Mutex
	w      io.Writer
	closer io.Closer
	dest   string
	logf   logging.Logf
	now    func() time.Time
}

// openAuditLog opens the sink --audit-log names: nothing for "", standard
// output for "stdout", and otherwise a file opened for append — created if
// missing, never truncated, and readable by its owner and group only, since
// every line names a person.
func openAuditLog(dest string, logf logging.Logf) (*auditLog, error) {
	switch dest {
	case "":
		return nil, nil
	case auditStdout:
		return &auditLog{w: os.Stdout, dest: dest, logf: logf, now: time.Now}, nil
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o640)
	if err != nil {
		return nil, fmt.Errorf("audit log: %w", err)
	}
	return &auditLog{w: f, closer: f, dest: dest, logf: logf, now: time.Now}, nil
}

// record writes one line. A write that fails is logged and dropped rather than
// failing the turn: the record describes the turn, and refusing turns because
// a disk filled would make the audit trail the outage.
func (l *auditLog) record(rec auditRecord) {
	if l == nil {
		return
	}
	rec.Time = l.now().UTC().Format(time.RFC3339Nano)
	line, err := json.Marshal(rec)
	if err != nil {
		l.logf.Errorf("audit: encode a %s record for %s: %v", rec.Kind, rec.Conversation, err)
		return
	}
	line = append(line, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.w.Write(line); err != nil {
		l.logf.Errorf("audit: write to %s: %v", l.dest, err)
	}
}

// close releases a file sink.
func (l *auditLog) close() error {
	if l == nil || l.closer == nil {
		return nil
	}
	return l.closer.Close()
}

// setAudit installs the ingress record. Called once at startup, before the
// adapter dispatches.
func (r *Router) setAudit(l *auditLog) { r.audit = l }

// auditTurn records one turn: who caused it, from which message, into which
// session, and whether the daemon took it.
func (r *Router) auditTurn(msg chat.Message, e *sessionEntry, err error) {
	rec := auditRecord{
		Kind:         auditTurn,
		Conversation: msg.Conversation,
		Channel:      msg.Channel,
		Caller:       msg.Caller,
		MessageID:    msg.MessageID,
		Outcome:      auditInjected,
	}
	// An entry whose creation failed has no session to name.
	if e != nil && e.err == nil && e.sess != (daemon.Session{}) {
		rec.Session = sessionRef(e.sess)
	}
	switch {
	case err == nil:
	case rec.Session == "":
		rec.Outcome = auditNoSession
		rec.Status = statusOf(err)
	default:
		rec.Outcome = auditInjectFailed
		rec.Status = statusOf(err)
	}
	r.audit.record(rec)
}

// statusOf returns the daemon's HTTP status for err, or 0.
func statusOf(err error) int {
	var se *daemon.StatusError
	if errors.As(err, &se) {
		return se.StatusCode
	}
	return 0
}
