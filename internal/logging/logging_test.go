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

package logging

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// at is the instant every test that pins a stamp uses. Deliberately not UTC:
// a clock that hands back a local time is the normal case for time.Now, and
// the rendered stamp has to be the UTC one either way.
var at = time.Date(2026, 8, 19, 21, 30, 0, 123456789, time.FixedZone("IST", 5*3600+1800))

func fixed(t time.Time) func() time.Time { return func() time.Time { return t } }

func TestParseFormat(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Format
		ok   bool
	}{
		{in: "text", want: Text, ok: true},
		{in: "json", want: JSON, ok: true},
		{in: "", ok: false},
		{in: "TEXT", ok: false},
		{in: "logfmt", ok: false},
	} {
		got, ok := ParseFormat(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ParseFormat(%q) = %q, %t; want %q, %t", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestTextRendersAStampedLine(t *testing.T) {
	var buf bytes.Buffer
	logf := newClock(&buf, Text, "switchboard", fixed(at))
	logf("relay %s: stream ended (%v)", "C1:T2", "EOF")

	// The zone the clock is in must not reach the output: 21:30 IST is 16:00Z.
	const want = "2026-08-19T16:00:00.123Z switchboard: relay C1:T2: stream ended (EOF)\n"
	if got := buf.String(); got != want {
		t.Errorf("text line:\n got %q\nwant %q", got, want)
	}
}

func TestTextStampIsFixedWidth(t *testing.T) {
	// RFC3339Nano trims trailing zeros, which would render a different width
	// per line and make the left-hand column unreadable.
	var buf bytes.Buffer
	whole := time.Date(2026, 8, 19, 16, 0, 0, 0, time.UTC)
	logf := newClock(&buf, Text, "switchboard", fixed(whole))
	logf("connected")

	stampOf := func(line string) string { return strings.SplitN(line, " ", 2)[0] }
	got, other := stampOf(buf.String()), stampOf("2026-08-19T16:00:00.123Z ")
	if len(got) != len(other) {
		t.Errorf("stamp %q is %d wide; a stamp with a fractional part is %d", got, len(got), len(other))
	}
	if !strings.HasSuffix(got, ".000Z") {
		t.Errorf("stamp %q dropped the fractional part of a whole second", got)
	}
}

func TestJSONRendersOneObjectPerLine(t *testing.T) {
	var buf bytes.Buffer
	logf := newClock(&buf, JSON, "switchboard", fixed(at))
	logf("bridging %s -> %s", "slack", "http://127.0.0.1:7777")
	logf("shutting down")

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2:\n%s", len(lines), buf.String())
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("line 0 is not JSON: %v (%q)", err, lines[0])
	}
	if got, want := first["time"], "2026-08-19T16:00:00.123Z"; got != want {
		t.Errorf(`"time" = %v, want %q`, got, want)
	}
	if got, want := first["message"], "bridging slack -> http://127.0.0.1:7777"; got != want {
		t.Errorf(`"message" = %v, want %q`, got, want)
	}
}

func TestJSONCarriesNoSeverity(t *testing.T) {
	// Not an oversight: no call site distinguishes a connect notice from a
	// send failure, so every record arrives at the same level, and labelling
	// them all INFO would mislabel the failures. Cloud Logging assigns
	// DEFAULT to a record with no severity, which is the honest reading
	// until #49 step 2 gives the call sites a level to carry.
	var buf bytes.Buffer
	logf := newClock(&buf, JSON, "switchboard", fixed(at))
	logf("handle %s: surface error: %v", "C1:T2", "500")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	for _, k := range []string{"level", "severity", "msg"} {
		if v, ok := rec[k]; ok {
			t.Errorf("record carries %q = %v; want it absent", k, v)
		}
	}
	if len(rec) != 2 {
		t.Errorf("record has %d keys (%v), want just time and message", len(rec), rec)
	}
}

func TestJSONOmitsTheProgramPrefix(t *testing.T) {
	// The prefix earns its place on a terminal, where switchboard shares the
	// stream. In a collector the stream is switchboard's alone, so a constant
	// on every record is noise — and baking it into the message would put it
	// somewhere no query can strip it back off.
	var buf bytes.Buffer
	logf := newClock(&buf, JSON, "switchboard", fixed(at))
	logf("connected")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if got := rec["message"]; got != "connected" {
		t.Errorf(`"message" = %v, want %q`, got, "connected")
	}
}

func TestJSONEscapesTheMessage(t *testing.T) {
	// The reason this path goes through slog rather than a hand-rolled
	// Fprintf: a Chat payload under --googlechat-log-events, and the raw
	// daemon frames the relay logs when it cannot decode one, are quotes and
	// braces and the occasional newline by construction.
	payload := `{"message":{"text":"say \"hi\"\nthen stop"},"space":{"name":"spaces/AAA"}}`
	var buf bytes.Buffer
	logf := newClock(&buf, JSON, "switchboard", fixed(at))
	logf("googlechat: event %s", payload)

	if n := bytes.Count(bytes.TrimSuffix(buf.Bytes(), []byte("\n")), []byte("\n")); n != 0 {
		t.Errorf("a newline in the message broke the record across %d lines:\n%s", n+1, buf.String())
	}
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, buf.String())
	}
	if got, want := rec["message"], "googlechat: event "+payload; got != want {
		t.Errorf("message did not round-trip:\n got %v\nwant %q", got, want)
	}
}

// writeLog records each Write as the caller made it, so a test can tell one
// write per line from a line assembled out of several — and notices when two
// Writes are in flight at once.
//
// The overlap flag is raised before the lock that protects the slice, so this
// writer's own serialising does not hide the logger's failure to serialise.
// Checking with the race detector instead would work, but only under -race,
// and the unit presubmit does not run there.
type writeLog struct {
	inFlight atomic.Int32
	overlap  atomic.Bool

	mu     sync.Mutex
	writes []string
}

func (w *writeLog) Write(p []byte) (int, error) {
	if w.inFlight.Add(1) > 1 {
		w.overlap.Store(true)
	}
	// Wide enough that concurrent callers actually meet here: without it the
	// window between the two atomics is short enough to slip through.
	time.Sleep(time.Millisecond)
	w.mu.Lock()
	w.writes = append(w.writes, string(p))
	w.mu.Unlock()
	w.inFlight.Add(-1)
	return len(p), nil
}

func TestEachLineIsOneWrite(t *testing.T) {
	// The router relays several conversations at once and both adapters log
	// from their own goroutines. A line built out of two writes is a line
	// another goroutine is free to land inside, and two Writes in flight at
	// once is the same hazard one step earlier — the text path holds a mutex
	// for exactly this, and slog's handler does its own locking.
	for _, f := range []Format{Text, JSON} {
		t.Run(string(f), func(t *testing.T) {
			var w writeLog
			logf := newClock(&w, f, "switchboard", fixed(at))

			var wg sync.WaitGroup
			for i := range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					logf("relay C%d: send: %v", i, "timeout")
				}()
			}
			wg.Wait()

			if w.overlap.Load() {
				t.Error("two Writes were in flight at once; the lines are free to interleave")
			}
			if len(w.writes) != 8 {
				t.Errorf("got %d writes for 8 lines; want one each", len(w.writes))
			}
			for i, got := range w.writes {
				if strings.Count(got, "\n") != 1 || !strings.HasSuffix(got, "\n") {
					t.Errorf("write %d is not exactly one line: %q", i, got)
				}
			}
		})
	}
}

func TestNewWritesToTheGivenWriter(t *testing.T) {
	// New is the exported door; everything above drives newClock.
	var buf bytes.Buffer
	New(&buf, Text, "switchboard")("connected")
	if got := buf.String(); !strings.HasSuffix(got, " switchboard: connected\n") {
		t.Errorf("New wrote %q", got)
	}
}

func TestUnknownFormatRendersAsText(t *testing.T) {
	// ParseFormat is what rejects a bad --log-format; if one ever reaches the
	// constructor anyway, the readable rendering is the safer default.
	var buf bytes.Buffer
	newClock(&buf, Format("logfmt"), "switchboard", fixed(at))("connected")
	if got, want := buf.String(), "2026-08-19T16:00:00.123Z switchboard: connected\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
