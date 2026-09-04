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

// Package logging builds the one logger switchboard writes through.
//
// Every component takes the same hook — Logf, on the adapter configs, the
// ingress and the router — so this package's job is to decide what a line
// looks like, not what goes in one.
//
// What a line carries, and what it does not (#49):
//
//   - Time, always. Deployments get an ingestion timestamp from Cloud Run or
//     a k8s collector, but that is when the line was collected, and it is
//     absent altogether for a local run, a redirect to a file, a `kubectl
//     logs` dump taken without --timestamps, or output pasted into an issue.
//     The relay's reconnect backoff and the progress clock are the things
//     most often being diagnosed from a log, and "four flaps" means nothing
//     without knowing whether it was over a minute or an hour.
//   - Severity, carried by the call site (step 2). Cloud Logging keys
//     alerting and Error Reporting off it, so until every line was one
//     undifferentiated stream nothing in a deployment could page. The level
//     is a parameter rather than something inferred here on purpose: a
//     handful of messages already open with "warning: ", and reading a level
//     back out of the text would be right for those and wrong the first time
//     someone reworded one.
//   - No structured fields. The messages bake their component and their
//     conversation key into the format string, and turning those into attrs
//     means rewriting every message — #49 step 3, deliberately not this.
package logging

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Level is the severity a call site attaches to a line.
//
// Three, and deliberately no more. The distinction that has to survive is the
// one a deployment acts on: Cloud Logging alerts and Error Reporting key off
// ERROR, an operator scanning a startup reads WARNING, and everything else is
// the running commentary. A DEBUG level would be a fourth thing to classify
// 100 call sites against and a --log-level flag to gate it, for a process
// whose whole log fits in a terminal.
type Level slog.Level

// The rubric the call sites were classified against:
//
//   - LevelError: switchboard could not do something, and a user or a turn is
//     worse off for it — a reply that never reached the thread, a relay that
//     gave up, a surface call that failed.
//   - LevelWarn: degraded but still going — a retry, a fallback, a capability
//     the daemon does not offer, a config value that was ignored, a frame that
//     could not be read. Nothing is lost that the next line will not recover.
//   - LevelInfo: lifecycle and normal operation. Listening, connected,
//     bound, relaying, shutting down.
const (
	LevelInfo  Level = Level(slog.LevelInfo)
	LevelWarn  Level = Level(slog.LevelWarn)
	LevelError Level = Level(slog.LevelError)
)

// String renders the level for the text format: fixed width, so the message
// column lines up down a terminal.
func (l Level) String() string {
	switch l {
	case LevelWarn:
		return "WARN "
	case LevelError:
		return "ERROR"
	default:
		return "INFO "
	}
}

// severity renders the level for the JSON format, using Cloud Logging's
// vocabulary — which is slog's but for WARNING, spelled out.
func (l Level) severity() string {
	switch l {
	case LevelWarn:
		return "WARNING"
	case LevelError:
		return "ERROR"
	default:
		return "INFO"
	}
}

// Logf is the hook every component logs through: the adapter configs, the
// ingress and the router all hold one, and main hands them all the same one.
//
// The methods rather than the bare call are the point of the named type. They
// keep a call site to `r.logf.Warnf("...")` — as short as the printf-style
// hook this replaced — and they are nil-safe, so a component with no logger
// wired is the zero value and every component's "if nil, substitute a
// discard" guard goes away. A test that wants the lines captures them with a
// two-line closure; t.Logf no longer fits the signature, but the tests that
// passed it were discarding switchboard's output into the test log, not
// asserting on it.
type Logf func(Level, string, ...any)

// Infof logs at LevelInfo. Does nothing if l is nil.
func (l Logf) Infof(format string, a ...any) { l.log(LevelInfo, format, a...) }

// Warnf logs at LevelWarn. Does nothing if l is nil.
func (l Logf) Warnf(format string, a ...any) { l.log(LevelWarn, format, a...) }

// Errorf logs at LevelError. Does nothing if l is nil.
func (l Logf) Errorf(format string, a ...any) { l.log(LevelError, format, a...) }

func (l Logf) log(lv Level, format string, a ...any) {
	if l != nil {
		l(lv, format, a...)
	}
}

// StdLogger adapts the hook to the *log.Logger that some standard-library
// types insist on, writing whatever they hand it at lv under prefix.
//
// This exists for one caller: http.Server.ErrorLog. Left unset, a server
// writes its runtime errors — a panic recovered in a handler, a TLS handshake
// that failed, a connection error — through the log package's default logger,
// which is os.Stderr with none of switchboard's stamping and, under
// --log-format json, unparseable lines in the middle of the stream. Those are
// the lines that say why a listener is misbehaving, and they were the only
// ones the process could not see.
func (l Logf) StdLogger(lv Level, prefix string) *log.Logger {
	return log.New(levelWriter{l: l, lv: lv}, prefix, 0)
}

// levelWriter turns each Write into one record at a fixed level.
//
// One record, not one line. log.Logger writes once per Print, and net/http's
// loudest use of ErrorLog is the recovered-panic path, which formats the
// message and a whole runtime.Stack dump into a single call. Splitting that
// into a record per line would scatter one failure across twenty of them and
// cost Error Reporting the stack trace it groups on; the text renderer marks
// the continuation lines instead.
type levelWriter struct {
	l  Logf
	lv Level
}

func (w levelWriter) Write(p []byte) (int, error) {
	// log.Logger terminates what it writes; both renderings add their own.
	// Through %s rather than as a format string: this text is the standard
	// library's, and it is free to contain a percent sign.
	w.l.log(w.lv, "%s", strings.TrimSuffix(string(p), "\n"))
	return len(p), nil
}

// Format selects how a line is rendered.
type Format string

const (
	// Text is one line per record, for a human reading a terminal.
	Text Format = "text"
	// JSON is one object per record, for a log collector.
	JSON Format = "json"
)

// stamp is the time layout both formats share: RFC 3339, fixed width, in UTC.
//
// Milliseconds rather than RFC3339Nano's variable precision, which trims
// trailing zeros and so renders a different width per line — unreadable in a
// terminal where the time is the left-hand column. Milliseconds are enough to
// order the events that arrive in bursts (a stream reconnect, a run of daemon
// frames); the 15-second progress tick was never the hard case.
const stamp = "2006-01-02T15:04:05.000Z07:00"

// ParseFormat maps a --log-format value onto a Format.
func ParseFormat(s string) (Format, bool) {
	switch f := Format(s); f {
	case Text, JSON:
		return f, true
	default:
		return "", false
	}
}

// New returns the logger the whole process writes through. Lines go to w —
// os.Stderr in the binary — each stamped with the time it was written and the
// level the call site asked for.
//
// prog prefixes every text line and appears nowhere in the JSON, which is not
// an oversight: on a terminal the prefix is what distinguishes switchboard's
// output from whatever else shares the stream, and in a collector the stream
// is switchboard's alone and a constant field would be noise on every record.
//
// Nothing is filtered. Every level reaches w, because there are only three and
// the quietest of them is already the interesting one.
func New(w io.Writer, f Format, prog string) Logf {
	return newClock(w, f, prog, time.Now)
}

// newClock is New with the clock injected, so a test can assert on the stamp
// rather than on a regexp that would pass for any time at all.
func newClock(w io.Writer, f Format, prog string, now func() time.Time) Logf {
	if f == JSON {
		return jsonLogger(w, now)
	}
	return textLogger(w, prog, now)
}

// textLogger renders "<time> <LEVEL> <prog>: <message>".
//
// The level sits between the two fixed-width columns rather than in the
// message, which is what lets `grep ERROR` mean something now that eight
// messages no longer open with their own "warning: ".
//
// One Write per record, holding the mutex: the router relays several
// conversations at once and both adapters log from their own goroutines, so
// two lines built concurrently would otherwise be free to interleave. This
// does not make a line atomic against the OS — a payload logged under
// --googlechat-log-events can exceed a pipe's atomic write size — but it does
// mean the process never hands the kernel a half-built one.
func textLogger(w io.Writer, prog string, now func() time.Time) Logf {
	var mu sync.Mutex
	return func(lv Level, format string, a ...any) {
		head := now().UTC().Format(stamp) + " " + lv.String() + " " + prog + ": "
		// A record can be several lines: a Chat payload under
		// --googlechat-log-events, a daemon frame quoted back verbatim, a panic's
		// stack arriving through StdLogger. Left flush those lines would be
		// indistinguishable from records of their own — unstamped, unlevelled,
		// and counted by any grep that tallies levels — so they are marked as
		// belonging to the line above instead.
		msg := strings.ReplaceAll(fmt.Sprintf(format, a...), "\n", "\n"+continued)
		mu.Lock()
		defer mu.Unlock()
		_, _ = io.WriteString(w, head+msg+"\n")
	}
}

// continued opens a line that belongs to the record above it. Chosen to be
// something no message starts with and a human reads as a gutter.
const continued = "    | "

// jsonLogger renders one object per line through slog's JSON handler.
//
// The handler is driven a record at a time rather than through an
// slog.Logger, which would stamp its own time.Now and leave nothing for a
// test to pin. What it is here for is the encoding: messages carry raw daemon
// frames (`unreadable status-update: %s`) and, under
// --googlechat-log-events, whole Chat payloads, so the quotes, braces and
// newlines in a line are the normal case and not the edge.
func jsonLogger(w io.Writer, now func() time.Time) Logf {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			switch a.Key {
			case slog.TimeKey:
				// Fixed-width UTC, matching the text format. slog would
				// otherwise write RFC3339 with variable-width nanoseconds.
				return slog.String("time", a.Value.Time().Format(stamp))
			case slog.LevelKey:
				// Renamed to "severity", which is the field Cloud Logging
				// reads a record's level off; a record that spelled it "level"
				// would be ingested at DEFAULT severity with the level sitting
				// inert in the payload. The value is spelled Cloud Logging's
				// way too — WARNING, where slog writes WARN.
				lv, _ := a.Value.Any().(slog.Level)
				return slog.String("severity", Level(lv).severity())
			case slog.MessageKey:
				// Cloud Logging reads the log entry's text off "message";
				// slog's own key is "msg".
				return slog.String("message", a.Value.String())
			}
			return a
		},
	})
	return func(lv Level, format string, a ...any) {
		rec := slog.NewRecord(now().UTC(), slog.Level(lv), fmt.Sprintf(format, a...), 0)
		// A logger that fails the caller is worse than one that loses a line,
		// and there is nowhere to report a failure to write to the log.
		_ = h.Handle(context.Background(), rec)
	}
}
