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
// Every component takes the same printf-style hook — `Logf func(string,
// ...any)` on the adapter configs, the ingress and the router — so this
// package's job is to decide what a line looks like, not what goes in one.
// The signature is kept deliberately: it is what lets a test pass `t.Logf`
// straight in, and changing it would touch all 54 call sites for a change
// that is really about the two ends of the line.
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
//   - No severity. No call site carries one yet, so there is nothing to
//     render: see the JSON handler's ReplaceAttr for why this package
//     declines to invent one.
//   - No structured fields. The messages bake their component and their
//     conversation key into the format string, and turning those into attrs
//     means rewriting every message — #49 step 3, deliberately not this.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
)

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

// New returns the printf-style logger the whole process writes through.
// Lines go to w — os.Stderr in the binary — each stamped with the time it was
// written.
//
// prog prefixes every text line and appears nowhere in the JSON, which is not
// an oversight: on a terminal the prefix is what distinguishes switchboard's
// output from whatever else shares the stream, and in a collector the stream
// is switchboard's alone and a constant field would be noise on every record.
func New(w io.Writer, f Format, prog string) func(string, ...any) {
	return newClock(w, f, prog, time.Now)
}

// newClock is New with the clock injected, so a test can assert on the stamp
// rather than on a regexp that would pass for any time at all.
func newClock(w io.Writer, f Format, prog string, now func() time.Time) func(string, ...any) {
	if f == JSON {
		return jsonLogger(w, now)
	}
	return textLogger(w, prog, now)
}

// textLogger renders "<time> <prog>: <message>".
//
// One Write per line, holding the mutex: the router relays several
// conversations at once and both adapters log from their own goroutines, so
// two lines built concurrently would otherwise be free to interleave. This
// does not make a line atomic against the OS — a payload logged under
// --googlechat-log-events can exceed a pipe's atomic write size — but it does
// mean the process never hands the kernel a half-built one.
func textLogger(w io.Writer, prog string, now func() time.Time) func(string, ...any) {
	var mu sync.Mutex
	return func(format string, a ...any) {
		line := now().UTC().Format(stamp) + " " + prog + ": " + fmt.Sprintf(format, a...) + "\n"
		mu.Lock()
		defer mu.Unlock()
		_, _ = io.WriteString(w, line)
	}
}

// jsonLogger renders one object per line through slog's JSON handler.
//
// The handler is driven a record at a time rather than through an
// slog.Logger, which would stamp its own time.Now and leave nothing for a
// test to pin. What it is here for is the encoding: messages carry raw daemon
// frames (`unreadable status-update: %s`) and, under
// --googlechat-log-events, whole Chat payloads, so the quotes, braces and
// newlines in a line are the normal case and not the edge.
func jsonLogger(w io.Writer, now func() time.Time) func(string, ...any) {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			switch a.Key {
			case slog.TimeKey:
				// Fixed-width UTC, matching the text format. slog would
				// otherwise write RFC3339 with variable-width nanoseconds.
				return slog.String("time", a.Value.Time().Format(stamp))
			case slog.LevelKey:
				// Dropped, not renamed to Cloud Logging's "severity".
				// Nothing upstream distinguishes "slack: connected as %s"
				// from "handle %s: surface error: %v", so every record
				// reaches here at the same level, and stamping INFO on the
				// error line would be a claim this package cannot support —
				// worse than the DEFAULT severity that Cloud Logging assigns
				// to a record with no severity at all. Giving the call sites
				// a level to carry is #49 step 2.
				return slog.Attr{}
			case slog.MessageKey:
				// Cloud Logging reads the log entry's text off "message";
				// slog's own key is "msg".
				return slog.String("message", a.Value.String())
			}
			return a
		},
	})
	return func(format string, a ...any) {
		rec := slog.NewRecord(now().UTC(), slog.LevelInfo, fmt.Sprintf(format, a...), 0)
		// A logger that fails the caller is worse than one that loses a line,
		// and there is nowhere to report a failure to write to the log.
		_ = h.Handle(context.Background(), rec)
	}
}
