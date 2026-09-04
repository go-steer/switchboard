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
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/switchboard/internal/logging"
)

// TestListenerRuntimeErrorsGoThroughTheLogger is the end of the wire #49's
// ErrorLog follow-up runs: a panic recovered inside net/http has to come out of
// switchboard's logger, at ERROR, saying which listener it was. With
// http.Server.ErrorLog unset — its state before this — the same panic went to
// stderr through the log package's default logger: unstamped, and under
// --log-format json unparseable lines in the middle of the stream.
//
// Driven through serveHTTP rather than StdLogger directly, because the field
// being dropped from the http.Server literal is the failure this has to catch.
func TestListenerRuntimeErrorsGoThroughTheLogger(t *testing.T) {
	sink := &logSink{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addr := freeAddr(t)
	panicking := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })
	done := make(chan error, 1)
	go func() { done <- serveHTTP(ctx, "metrics", addr, panicking, sink.logf) }()
	waitFor(t, func() bool {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}, "serveHTTP never bound its port")

	// The panic is recovered by net/http, which closes the connection without a
	// response — so the client error here is the expected outcome, not a
	// failure of the test.
	if resp, err := http.Get("http://" + addr + "/metrics"); err == nil {
		resp.Body.Close()
		t.Fatal("a panicking handler answered; net/http did not recover it")
	}

	var got []logLine
	waitFor(t, func() bool {
		got = sink.at("panic serving")
		return len(got) > 0
	}, "the recovered panic never reached switchboard's logger")
	if len(got) != 1 {
		t.Fatalf("one panic logged %d records, want 1: %v", len(got), got)
	}
	if got[0].level != logging.LevelError {
		t.Errorf("the panic logged at %v, want ERROR", got[0].level)
	}
	if !strings.HasPrefix(got[0].text, "metrics server: ") {
		t.Errorf("record = %q, want it named for the listener it came from", got[0].text)
	}
	// The stack is what Error Reporting groups on, so it has to survive the trip
	// whole rather than being trimmed to the first line.
	if !strings.Contains(got[0].text, "goroutine ") {
		t.Errorf("record dropped the stack dump: %q", got[0].text)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serveHTTP = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("serveHTTP did not shut down after cancel")
	}
}
