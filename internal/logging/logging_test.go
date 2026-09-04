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
	logf.Warnf("relay %s: stream ended (%v)", "C1:T2", "EOF")

	// The zone the clock is in must not reach the output: 21:30 IST is 16:00Z.
	const want = "2026-08-19T16:00:00.123Z WARN  switchboard: relay C1:T2: stream ended (EOF)\n"
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
	logf.Infof("connected")

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
	logf.Infof("bridging %s -> %s", "slack", "http://127.0.0.1:7777")
	logf.Infof("shutting down")

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

func TestJSONCarriesCloudLoggingSeverity(t *testing.T) {
	// The field name and the vocabulary are both Cloud Logging's. A record
	// that spelled the key "level", or the value slog's "WARN", would be
	// ingested at DEFAULT with the level sitting inert in the payload — no
	// alert policy and no Error Reporting group.
	for _, tc := range []struct {
		name string
		log  func(Logf)
		want string
	}{
		{"info", func(l Logf) { l.Infof("connected") }, "INFO"},
		{"warn", func(l Logf) { l.Warnf("relay %s: reconnecting", "C1:T2") }, "WARNING"},
		{"error", func(l Logf) { l.Errorf("handle %s: surface error: %v", "C1:T2", "500") }, "ERROR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			tc.log(newClock(&buf, JSON, "switchboard", fixed(at)))

			var rec map[string]any
			if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
				t.Fatalf("not JSON: %v", err)
			}
			if got := rec["severity"]; got != tc.want {
				t.Errorf(`"severity" = %v, want %q`, got, tc.want)
			}
			for _, k := range []string{"level", "msg"} {
				if v, ok := rec[k]; ok {
					t.Errorf("record carries slog's %q = %v; Cloud Logging reads neither", k, v)
				}
			}
			if len(rec) != 3 {
				t.Errorf("record has %d keys (%v), want time, severity and message", len(rec), rec)
			}
		})
	}
}

func TestTextRendersTheLevelInItsOwnColumn(t *testing.T) {
	// Fixed width, so the message column lines up down a terminal, and its own
	// field rather than a "warning: " the message carries — which is what lets
	// `grep ERROR` mean something.
	var buf bytes.Buffer
	var got []string
	logf := newClock(&buf, Text, "switchboard", fixed(at))
	for _, log := range []func(){
		func() { logf.Infof("connected") },
		func() { logf.Warnf("connected") },
		func() { logf.Errorf("connected") },
	} {
		buf.Reset()
		log()
		got = append(got, buf.String())
	}

	want := []string{
		"2026-08-19T16:00:00.123Z INFO  switchboard: connected\n",
		"2026-08-19T16:00:00.123Z WARN  switchboard: connected\n",
		"2026-08-19T16:00:00.123Z ERROR switchboard: connected\n",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d:\n got %q\nwant %q", i, got[i], want[i])
		}
	}
	if a, b := strings.Index(got[0], "switchboard"), strings.Index(got[2], "switchboard"); a != b {
		t.Errorf("INFO and ERROR lines put the message at columns %d and %d", a, b)
	}
}

func TestStdLoggerWritesOneRecordPerLine(t *testing.T) {
	// http.Server.ErrorLog is the caller. Left unset, its runtime errors go to
	// the log package's default logger — stderr, unstamped, and under
	// --log-format json not JSON, in the middle of a stream a collector is
	// parsing.
	var buf bytes.Buffer
	l := newClock(&buf, JSON, "switchboard", fixed(at)).StdLogger(LevelError, "ingress server: ")
	// What net/http actually hands it: a trailing newline of its own, and text
	// that is free to contain a percent sign.
	l.Print("http: superfluous response.WriteHeader call from h (100% done)\n")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not JSON: %v (%q)", err, buf.String())
	}
	want := "ingress server: http: superfluous response.WriteHeader call from h (100% done)"
	if got := rec["message"]; got != want {
		t.Errorf(`"message" = %v, want %q`, got, want)
	}
	if got := rec["severity"]; got != "ERROR" {
		t.Errorf(`"severity" = %v, want "ERROR"`, got)
	}
	if n := bytes.Count(buf.Bytes(), []byte("\n")); n != 1 {
		t.Errorf("one Print produced %d lines:\n%s", n, buf.String())
	}
}

// The loudest thing net/http writes to ErrorLog is the recovered-panic path,
// which formats the message and a whole runtime.Stack dump into one Print. It
// has to stay one record — Error Reporting groups on the stack, and twenty
// records would be twenty groups — and in text the dump's lines have to be
// marked as continuations, or they read as records with no stamp and no level
// and every grep that counts levels counts them wrong.
func TestAStackDumpStaysOneRecord(t *testing.T) {
	const panicPrint = "http: panic serving 127.0.0.1:39944: boom\n" +
		"goroutine 18 [running]:\n" +
		"net/http.(*conn).serve.func1()\n" +
		"\t/usr/local/go/src/net/http/server.go:1898 +0xbe\n"

	t.Run("json", func(t *testing.T) {
		var buf bytes.Buffer
		newClock(&buf, JSON, "switchboard", fixed(at)).
			StdLogger(LevelError, "metrics server: ").Print(panicPrint)
		if n := bytes.Count(buf.Bytes(), []byte("\n")); n != 1 {
			t.Fatalf("a stack dump produced %d JSON records, want 1:\n%s", n, buf.String())
		}
		var rec map[string]any
		if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
			t.Fatalf("not JSON: %v (%q)", err, buf.String())
		}
		msg, _ := rec["message"].(string)
		if !strings.Contains(msg, "server.go:1898") {
			t.Errorf(`"message" lost the stack: %q`, msg)
		}
	})

	t.Run("text", func(t *testing.T) {
		var buf bytes.Buffer
		newClock(&buf, Text, "switchboard", fixed(at)).
			StdLogger(LevelError, "metrics server: ").Print(panicPrint)
		lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
		if len(lines) != 4 {
			t.Fatalf("got %d lines, want 4:\n%s", len(lines), buf.String())
		}
		if !strings.HasPrefix(lines[0], at.UTC().Format(stamp)+" ERROR switchboard: metrics server: ") {
			t.Errorf("head line = %q, want the stamped record", lines[0])
		}
		for i, l := range lines[1:] {
			if !strings.HasPrefix(l, continued) {
				t.Errorf("continuation %d = %q, want it marked with %q — unmarked it reads as a record of its own", i, l, continued)
			}
			if strings.Contains(l, "ERROR") {
				t.Errorf("continuation %d = %q carries a level, so a grep for ERROR counts it twice", i, l)
			}
		}
	})
}

func TestTheZeroLogfDiscards(t *testing.T) {
	// A component with no logger wired holds the zero value, which is what
	// retired the four copies of "if cfg.Logf == nil, substitute a discard".
	var l Logf
	l.Infof("connected")
	l.Warnf("relay %s: reconnecting", "C1:T2")
	l.Errorf("handle %s: %v", "C1:T2", "500")
	// Including through the shim, which is handed to an http.Server whether or
	// not the process was given a logger.
	l.StdLogger(LevelError, "ingress server: ").Print("accept tcp: too many open files")
}

func TestJSONOmitsTheProgramPrefix(t *testing.T) {
	// The prefix earns its place on a terminal, where switchboard shares the
	// stream. In a collector the stream is switchboard's alone, so a constant
	// on every record is noise — and baking it into the message would put it
	// somewhere no query can strip it back off.
	var buf bytes.Buffer
	logf := newClock(&buf, JSON, "switchboard", fixed(at))
	logf.Infof("connected")

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
	logf.Infof("googlechat: event %s", payload)

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
					logf.Errorf("relay C%d: send: %v", i, "timeout")
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
	New(&buf, Text, "switchboard").Infof("connected")
	if got := buf.String(); !strings.HasSuffix(got, "INFO  switchboard: connected\n") {
		t.Errorf("New wrote %q", got)
	}
}

func TestUnknownFormatRendersAsText(t *testing.T) {
	// ParseFormat is what rejects a bad --log-format; if one ever reaches the
	// constructor anyway, the readable rendering is the safer default.
	var buf bytes.Buffer
	newClock(&buf, Format("logfmt"), "switchboard", fixed(at)).Infof("connected")
	if got, want := buf.String(), "2026-08-19T16:00:00.123Z INFO  switchboard: connected\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
