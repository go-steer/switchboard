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

package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// agentFrameAt renders an agent event carrying one line of model text at seq.
func agentFrameAt(seq int64, text string) string {
	return fmt.Sprintf(`{"seq":%d,"event":{"Content":{"parts":[{"text":%q}],"role":"model"},"Partial":false}}`, seq, text)
}

// withHeadProbeCap shortens the probe's outer bound for one test and returns
// the restore. These tests do not run in parallel, which is what makes writing
// a package var here safe.
func withHeadProbeCap(d time.Duration) func() {
	prev := headProbeCap
	headProbeCap = d
	return func() { headProbeCap = prev }
}

// streamThenHold writes the given SSE frames and keeps the connection open the
// way a live daemon does, so a reader has to decide for itself when the backlog
// has run out.
func streamThenHold(t *testing.T, frames []string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		for _, f := range frames {
			fmt.Fprint(w, f)
			w.(http.Flusher).Flush()
		}
		<-r.Context().Done()
	}
}

// TestHeadSeqReportsTheEndOfTheBacklog is the property adoption rests on: the
// seq it comes back with is the last one the daemon already holds, so a
// subscriber starting there is handed the next thing to happen and none of what
// already did.
func TestHeadSeqReportsTheEndOfTheBacklog(t *testing.T) {
	frames := []string{
		fmt.Sprintf("event: %s\ndata: {\"protocol_version\":\"1.5.0\"}\n\n", EventCapabilities),
		fmt.Sprintf("event: %s\ndata: %s\n\n", EventAgent, agentFrameAt(1, "looking at the pods")),
		fmt.Sprintf("event: %s\ndata: %s\n\n", EventAgent, agentFrameAt(7, "the deployment is wedged")),
		fmt.Sprintf("event: %s\ndata: {\"turn_state\":\"idle\"}\n\n", EventStatusUpdate),
	}
	c := newTestClient(t, streamThenHold(t, frames))

	start := time.Now()
	head, err := c.HeadSeq(context.Background(), Session{App: "core-agent", ID: "s1"}, "")
	if err != nil {
		t.Fatalf("HeadSeq: %v", err)
	}
	if head != 7 {
		t.Errorf("head = %d, want 7 (the last agent frame in the backlog)", head)
	}
	// The stream never ends, so returning at all means the quiet detector did
	// its job — and it has to do it quickly, because a caller is holding an
	// HTTP request open behind this.
	if elapsed := time.Since(start); elapsed > headProbeCap {
		t.Errorf("HeadSeq took %s; the probe cap is %s", elapsed, headProbeCap)
	}
}

// TestHeadSeqReportsANonexistentSession is the loud half of a bind: a thread
// tied to a session the daemon does not have would take a human's reply and
// drop it, so the bind has to fail instead.
func TestHeadSeqReportsANonexistentSession(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such session", http.StatusNotFound)
	})

	_, err := c.HeadSeq(context.Background(), Session{App: "core-agent", ID: "gone"}, "")
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("HeadSeq err = %v, want a *StatusError", err)
	}
	if se.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", se.StatusCode)
	}
}

// TestHeadSeqOnASessionWithNoBacklog reports 0, which is also what a caller
// asking for everything would pass: a session that has said nothing yet has
// nothing to keep out of the thread.
func TestHeadSeqOnASessionWithNoBacklog(t *testing.T) {
	c := newTestClient(t, streamThenHold(t, nil))

	head, err := c.HeadSeq(context.Background(), Session{App: "core-agent", ID: "fresh"}, "")
	if err != nil {
		t.Fatalf("HeadSeq: %v", err)
	}
	if head != 0 {
		t.Errorf("head = %d, want 0", head)
	}
}

// TestHeadSeqFailsOnItsCallersContext keeps the probe's own deadline from
// swallowing the caller's. A head read from half a backlog would have the
// thread resume in the middle of a turn it never saw the start of.
func TestHeadSeqFailsOnItsCallersContext(t *testing.T) {
	c := newTestClient(t, streamThenHold(t, []string{
		fmt.Sprintf("event: %s\ndata: %s\n\n", EventAgent, agentFrameAt(3, "partway")),
	}))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	head, err := c.HeadSeq(ctx, Session{App: "core-agent", ID: "s1"}, "")
	if err == nil {
		t.Fatalf("HeadSeq = %d, nil; want the caller's cancellation reported", head)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if head != 0 {
		t.Errorf("head = %d, want 0 on failure", head)
	}
}

// TestHeadSeqWaitsForADaemonSlowerThanTheQuietWindow. The quiet detector times
// silence on an open stream; if it were started at call time it would be timing
// the dial, and a daemon that took longer than headProbeQuiet to answer would
// have its request cancelled before it said anything. The result of that is a
// head of 0 and no error — which is "replay the whole window into the chat
// thread", the one outcome this function exists to prevent.
func TestHeadSeqWaitsForADaemonSlowerThanTheQuietWindow(t *testing.T) {
	slow := streamThenHold(t, []string{
		fmt.Sprintf("event: %s\ndata: %s\n\n", EventAgent, agentFrameAt(42, "still here")),
	})
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * headProbeQuiet)
		slow(w, r)
	})

	head, err := c.HeadSeq(context.Background(), Session{App: "core-agent", ID: "s1"}, "")
	if err != nil {
		t.Fatalf("HeadSeq: %v", err)
	}
	if head != 42 {
		t.Errorf("head = %d, want 42", head)
	}
}

// TestHeadSeqReportsASlowNonexistentSession is the same hazard on the other
// half of the call: a 404 that arrives after the quiet window has to still be a
// 404, or a thread gets bound to a session that is not there.
func TestHeadSeqReportsASlowNonexistentSession(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * headProbeQuiet)
		http.Error(w, "no such session", http.StatusNotFound)
	})

	_, err := c.HeadSeq(context.Background(), Session{App: "core-agent", ID: "gone"}, "")
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("HeadSeq err = %v, want a *StatusError", err)
	}
	if se.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", se.StatusCode)
	}
}

// TestHeadSeqFailsWhenTheStreamNeverOpens. A daemon that accepts the connection
// and then says nothing at all — no status, no frames — has told us nothing
// about the session, and 0 is not a measurement of it.
//
// The cap has to be what runs out, not the caller's deadline: a caller that
// gave up has its own error to report, and the branch under test is the one
// where switchboard was given all the time it asked for and still learned
// nothing.
func TestHeadSeqFailsWhenTheStreamNeverOpens(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	defer withHeadProbeCap(2 * headProbeQuiet)()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	head, err := c.HeadSeq(ctx, Session{App: "core-agent", ID: "s1"}, "")
	if err == nil {
		t.Fatalf("HeadSeq = %d, nil; want a failure rather than a head nobody measured", head)
	}
	if head != 0 {
		t.Errorf("head = %d, want 0 on failure", head)
	}
}

// TestHeadSeqTakesTheSeqOfAFrameItWouldNotRelay: a tool call is a position in
// the stream like any other, and resuming behind one replays the whole turn it
// belongs to.
func TestHeadSeqTakesTheSeqOfAFrameItWouldNotRelay(t *testing.T) {
	toolCall := `{"seq":9,"event":{"Content":{"parts":[{"functionCall":{"id":"c1","name":"kubectl","args":{}}}],"role":"model"},"Partial":false}}`
	c := newTestClient(t, streamThenHold(t, []string{
		fmt.Sprintf("event: %s\ndata: %s\n\n", EventAgent, agentFrameAt(2, "checking")),
		fmt.Sprintf("event: %s\ndata: %s\n\n", EventAgent, toolCall),
	}))

	head, err := c.HeadSeq(context.Background(), Session{App: "core-agent", ID: "s1"}, "")
	if err != nil {
		t.Fatalf("HeadSeq: %v", err)
	}
	if head != 9 {
		t.Errorf("head = %d, want 9 (the tool call is the last frame)", head)
	}
}
