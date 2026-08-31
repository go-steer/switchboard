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

package sse

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func scanAll(t *testing.T, body string) []Event {
	t.Helper()
	var got []Event
	if err := Scan(strings.NewReader(body), func(e Event) error {
		got = append(got, e)
		return nil
	}); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return got
}

func TestScanDispatchesOnTheBlankLine(t *testing.T) {
	got := scanAll(t, "event: a\ndata: 1\n\nevent: b\ndata: 2\n\n")
	want := []Event{{Type: "a", Data: "1"}, {Type: "b", Data: "2"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestScanJoinsMultiLineData(t *testing.T) {
	got := scanAll(t, "event: a\ndata: {\ndata:   \"k\": 1\ndata: }\n\n")
	if len(got) != 1 {
		t.Fatalf("events = %v, want 1", got)
	}
	if want := "{\n\"k\": 1\n}"; got[0].Data != want {
		t.Fatalf("data = %q, want %q", got[0].Data, want)
	}
}

// A record with a type and no data is real — some events are the fact that
// they happened. A record with data and no type is real too; the caller
// decides what to do with an unnamed frame.
func TestScanDeliversARecordThatCarriesOnlyOneField(t *testing.T) {
	got := scanAll(t, "event: ping\n\ndata: 7\n\n")
	want := []Event{{Type: "ping"}, {Data: "7"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

// Keep-alive comments, id/retry fields and stray blank lines all reduce to an
// empty record once the two fields this decoder reads are filtered out.
// Delivering those would make every caller re-check for them.
func TestScanDropsRecordsThatAreEmptyAfterFiltering(t *testing.T) {
	got := scanAll(t, "\n\n: keep-alive\n\nid: 9\nretry: 100\n\nevent: a\ndata: 1\n\n\n")
	want := []Event{{Type: "a", Data: "1"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

// The only thing separating a truncated final record from a complete one is
// the terminator it is missing. Delivering it hands the caller half a JSON
// payload to parse; dropping it costs a frame the caller sees again when it
// resumes.
func TestScanDropsARecordTheStreamWasCutOffMidWay(t *testing.T) {
	got := scanAll(t, "event: a\ndata: 1\n\nevent: b\ndata: {\"partial\":")
	want := []Event{{Type: "a", Data: "1"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestScanReturnsTheCallbacksOwnErrorAndStops(t *testing.T) {
	stop := errors.New("enough")
	n := 0
	err := Scan(strings.NewReader("event: a\n\nevent: b\n\n"), func(Event) error {
		n++
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatalf("err = %v, want the callback's error unwrapped", err)
	}
	if n != 1 {
		t.Errorf("callback ran %d times after asking to stop", n)
	}
}

func TestScanReportsAReadFailure(t *testing.T) {
	boom := errors.New("connection reset")
	err := Scan(io.MultiReader(strings.NewReader("event: a\ndata: 1\n\n"), errReader{boom}), func(Event) error { return nil })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the read failure", err)
	}
}

// An unbounded scanner lets the far end decide how much memory switchboard
// uses. A line past the cap ends the scan with an error rather than growing
// to meet it.
func TestScanRefusesALineLargerThanItsCap(t *testing.T) {
	body := "event: a\ndata: " + strings.Repeat("x", maxLine+1) + "\n\n"
	n := 0
	err := Scan(strings.NewReader(body), func(Event) error {
		n++
		return nil
	})
	if err == nil {
		t.Fatal("Scan accepted a line past its cap")
	}
	if n != 0 {
		t.Errorf("delivered %d events from an oversized frame", n)
	}
}

// A per-line cap alone leaves the total set by how many lines the far end
// sends: data accumulates across every `data:` line until the blank one. A
// record has to be bounded in its own right, or a stream is a way to make
// switchboard allocate without limit.
func TestScanRefusesARecordLargerThanItsCapEvenInSmallLines(t *testing.T) {
	line := "data: " + strings.Repeat("x", 64*1024) + "\n"
	var b strings.Builder
	b.WriteString("event: a\n")
	for b.Len() < maxRecord+128*1024 {
		b.WriteString(line)
	}
	b.WriteString("\n")

	n := 0
	err := Scan(strings.NewReader(b.String()), func(Event) error {
		n++
		return nil
	})
	if err == nil {
		t.Fatal("Scan accepted a record past its cap, one small line at a time")
	}
	if n != 0 {
		t.Errorf("delivered %d events from an oversized record", n)
	}
}

// The bound must not cost the ordinary case: a record made of many lines that
// together stay under the cap is still one event with all of them joined.
func TestScanJoinsManyLinesUpToTheCap(t *testing.T) {
	const lines = 500
	var b strings.Builder
	b.WriteString("event: a\n")
	for range lines {
		b.WriteString("data: chunk\n")
	}
	b.WriteString("\n")

	got := scanAll(t, b.String())
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
	if want := strings.TrimSuffix(strings.Repeat("chunk\n", lines), "\n"); got[0].Data != want {
		t.Errorf("data lost lines: %d bytes, want %d", len(got[0].Data), len(want))
	}
}

// Records are read into a reused buffer; one must not leak into the next.
func TestScanDoesNotBleedOneRecordIntoTheNext(t *testing.T) {
	got := scanAll(t, "event: a\ndata: first\ndata: second\n\nevent: b\ndata: x\n\n")
	want := []Event{{Type: "a", Data: "first\nsecond"}, {Type: "b", Data: "x"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }
