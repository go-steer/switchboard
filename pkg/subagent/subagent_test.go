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

package subagent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-steer/switchboard/pkg/daemon"
)

func TestValidName(t *testing.T) {
	valid := []string{"_fallback", "propose-change", "cluster_1", "k8s-triage"}
	for _, n := range valid {
		if !ValidName(n) {
			t.Errorf("ValidName(%q) = false, want true", n)
		}
	}
	invalid := []string{"", " ", " a", "a ", "mcp.tool", "a/b", "../x", "two words", "bad\x00name", "bad\nname"}
	for _, n := range invalid {
		if ValidName(n) {
			t.Errorf("ValidName(%q) = true, want false", n)
		}
	}
}

func TestNewValidatesConfig(t *testing.T) {
	if _, err := New(daemon.Config{}); err == nil {
		t.Fatal("New(empty) succeeded, want validation error")
	}
	hc := &http.Client{Timeout: 5 * time.Second}
	c, err := New(daemon.Config{BaseURL: "https://127.0.0.1:7777", MutualTLS: true, HTTPClient: hc})
	if err != nil {
		t.Fatalf("New(mTLS): %v", err)
	}
	if c.http.Timeout != 0 {
		t.Fatalf("client timeout = %s, want 0 (per-request context deadline only)", c.http.Timeout)
	}
	if hc.Timeout != 5*time.Second {
		t.Fatal("New mutated caller's http.Client")
	}
}

func TestEventsWireContractAndFiltering(t *testing.T) {
	var gotPath, gotSince, gotAuth, gotCaller string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotSince = r.URL.Query().Get("since")
		gotAuth = r.Header.Get("Authorization")
		gotCaller = r.Header.Get("X-Asserted-Caller")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"agent": "propose-change",
			"parent_session_id": "s-1",
			"branches": ["propose-change", "bg.propose-change"],
			"events": [
				{
					"seq": 41,
					"event": {
						"Content": {"parts": [{"text": "cloning repo"}], "role": "model"},
						"Partial": false,
						"Timestamp": "2026-11-18T18:00:01Z"
					}
				},
				{
					"seq": 42,
					"event": {
						"Content": {
							"parts": [{
								"functionCall": {
									"id": "call-1",
									"name": "vcs_clone",
									"args": {"repo": "go-steer/demo", "token": "ghp_secret1234567890"}
								}
							}],
							"role": "model"
						},
						"Partial": false,
						"Timestamp": "2026-11-18T18:00:02Z"
					}
				},
				{
					"seq": 43,
					"event": {
						"Content": {
							"parts": [{
								"functionResponse": {
									"id": "call-1",
									"name": "vcs_clone",
									"response": {"exit_code": 0}
								}
							}],
							"role": "user"
						},
						"Timestamp": "2026-11-18T18:00:04Z"
					}
				},
				{
					"seq": 44,
					"event": {
						"Content": {
							"parts": [{
								"functionCall": {
									"id": "ft-1",
									"name": "finish_task",
									"args": {"result": "Opened PR #8"}
								}
							}],
							"role": "model"
						},
						"Partial": false,
						"Timestamp": "2026-11-18T18:00:05Z"
					}
				},
				{
					"seq": 45,
					"event": {
						"Content": {
							"parts": [{
								"functionResponse": {
									"id": "ft-1",
									"name": "finish_task",
									"response": {"result": "ok"}
								}
							}],
							"role": "user"
						},
						"Timestamp": "2026-11-18T18:00:05Z"
					}
				}
			],
			"next_since": 45,
			"truncated": true
		}`)
	}))
	defer srv.Close()

	c, err := New(daemon.Config{BaseURL: srv.URL, BearerToken: "secret", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	page, err := c.Events(context.Background(), daemon.Session{App: "core-agent", ID: "s-1"}, "ana@example.com", "propose-change", 40)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if gotPath != "/sessions/core-agent/s-1/agents/propose-change/events" {
		t.Errorf("path = %q", gotPath)
	}
	if gotSince != "40" {
		t.Errorf("since = %q, want 40", gotSince)
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotCaller != "ana@example.com" {
		t.Errorf("X-Asserted-Caller = %q", gotCaller)
	}
	if page.NextSince != 45 || !page.Truncated {
		t.Errorf("cursor = (NextSince=%d, Truncated=%v), want (45, true)", page.NextSince, page.Truncated)
	}
	// Plain text (41) and finish_task call/response (44, 45) are filtered out;
	// vcs_clone call (42) and response (43) come through, with secret arg redacted.
	if len(page.Events) != 2 {
		t.Fatalf("len(Events) = %d, want 2: %+v", len(page.Events), page.Events)
	}
	callFrame := page.Events[0]
	if callFrame.Seq != 42 || len(callFrame.Calls) != 1 {
		t.Fatalf("frame[0] = %+v", callFrame)
	}
	if callFrame.Calls[0].ID != "call-1" || callFrame.Calls[0].Name != "vcs_clone" || callFrame.Calls[0].Arg != "go-steer/demo" {
		t.Errorf("call = %+v", callFrame.Calls[0])
	}
	if wantAt := time.Date(2026, 11, 18, 18, 0, 2, 0, time.UTC); !callFrame.At.Equal(wantAt) {
		t.Errorf("call At = %v, want %v", callFrame.At, wantAt)
	}
	resFrame := page.Events[1]
	if resFrame.Seq != 43 || len(resFrame.Results) != 1 || resFrame.Results[0].Failed {
		t.Fatalf("frame[1] = %+v", resFrame)
	}
	if resFrame.At.Sub(callFrame.At) != 2*time.Second {
		t.Errorf("duration = %s, want 2s", resFrame.At.Sub(callFrame.At))
	}
}

func TestEventsOmitAssertedCallerAndNoRedirects(t *testing.T) {
	var gotCaller string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCaller = r.Header.Get("X-Asserted-Caller")
		if r.URL.Query().Get("since") == "99" {
			http.Redirect(w, r, "http://example.com/leak", http.StatusFound)
			return
		}
		fmt.Fprint(w, `{"events":[],"next_since":0,"truncated":false}`)
	}))
	defer srv.Close()

	c, err := New(daemon.Config{BaseURL: srv.URL, BearerToken: "tok", OmitAssertedCaller: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sess := daemon.Session{App: "mast", ID: "s1"}
	if _, err := c.Events(context.Background(), sess, "ana@example.com", "_fallback", 0); err != nil {
		t.Fatalf("Events: %v", err)
	}
	if gotCaller != "" {
		t.Errorf("X-Asserted-Caller = %q, want omitted", gotCaller)
	}

	_, err = c.Events(context.Background(), sess, "ana@example.com", "_fallback", 99)
	var se *daemon.StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusFound {
		t.Fatalf("redirect err = %v, want 302 StatusError", err)
	}
}

func TestEventsStatusSentinelsAndTransientClassification(t *testing.T) {
	cases := []struct {
		code      int
		sentinel  error
		transient bool
	}{
		{http.StatusNotFound, ErrNotSubagent, false},
		{http.StatusBadRequest, ErrNotSubagent, false},
		{http.StatusPreconditionFailed, ErrNotSupported, false},
		{http.StatusNotImplemented, ErrNotSupported, false},
		{http.StatusServiceUnavailable, nil, true},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("HTTP_%d", tc.code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "nope", tc.code)
			}))
			defer srv.Close()

			c, err := New(daemon.Config{BaseURL: srv.URL, BearerToken: "tok", HTTPClient: srv.Client()})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = c.Events(context.Background(), daemon.Session{App: "a", ID: "s"}, "", "_fallback", 0)
			if err == nil {
				t.Fatal("Events succeeded, want error")
			}
			if tc.sentinel != nil && !errors.Is(err, tc.sentinel) {
				t.Errorf("errors.Is(%v, %v) = false", err, tc.sentinel)
			}
			var se *daemon.StatusError
			if !errors.As(err, &se) || se.StatusCode != tc.code {
				t.Errorf("StatusError = %+v, want status %d", se, tc.code)
			}
			if got := daemon.IsTransient(err); got != tc.transient {
				t.Errorf("IsTransient = %v, want %v", got, tc.transient)
			}
		})
	}
}

func TestEventsLocalValidationAvoidsNetwork(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, err := New(daemon.Config{BaseURL: srv.URL, BearerToken: "tok", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Events(context.Background(), daemon.Session{App: "a"}, "", "_fallback", 0); err == nil {
		t.Fatal("incomplete session succeeded")
	}
	if _, err := c.Events(context.Background(), daemon.Session{App: "a", ID: "s"}, "", "../escape", 0); !errors.Is(err, ErrNotSubagent) {
		t.Fatalf("invalid name err = %v, want ErrNotSubagent", err)
	}
	if hits != 0 {
		t.Fatalf("server hit %d times, want 0", hits)
	}
}

func TestEventsFiltersAllDriverControlToolsAndPartials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"events": [
				{"seq": 1, "event": {"Content": {"parts": [{"functionCall": {"id": "p1", "name": "vcs_clone", "args": {"repo": "x"}}}], "role": "model"}, "Partial": true}},
				{"seq": 2, "event": {"Content": {"parts": [{"functionCall": {"id": "d1", "name": "return_result", "args": {"result": "done"}}}], "role": "model"}, "Partial": false}},
				{"seq": 3, "event": {"Content": {"parts": [{"functionCall": {"id": "d2", "name": "report_done", "args": {"result": "done"}}}], "role": "model"}, "Partial": false}},
				{"seq": 4, "event": {"Content": {"parts": [{"functionCall": {"id": "d3", "name": "report_completed", "args": {"result": "done"}}}], "role": "model"}, "Partial": false}},
				{"seq": 5, "event": {"Content": {"parts": [{"functionCall": {"id": "d4", "name": "mark_task_done", "args": {"result": "done"}}}], "role": "model"}, "Partial": false}},
				{"seq": 6, "event": {"Content": {"parts": [{"functionCall": {"id": "d5", "name": "schedule_next_turn", "args": {"delay": "10s"}}}], "role": "model"}, "Partial": false}},
				{"seq": 7, "event": {"Content": {"parts": [{"functionResponse": {"id": "d1", "name": "return_result", "response": {"ok": true}}}], "role": "user"}}},
				{"seq": 8, "event": {"Content": {"parts": [
					{"functionCall": {"id": "real-1", "name": "get_k8s_logs", "args": {"pod": "api-0"}}},
					{"functionCall": {"id": "d6", "name": "finish_task", "args": {"result": "all healthy"}}}
				], "role": "model"}, "Partial": false}}
			],
			"next_since": 8,
			"truncated": false
		}`)
	}))
	defer srv.Close()

	c, err := New(daemon.Config{BaseURL: srv.URL, BearerToken: "tok", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	page, err := c.Events(context.Background(), daemon.Session{App: "core-agent", ID: "s1"}, "", "cluster", 0)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("len(Events) = %d, want 1 (only get_k8s_logs): %+v", len(page.Events), page.Events)
	}
	if len(page.Events[0].Calls) != 1 || page.Events[0].Calls[0].Name != "get_k8s_logs" {
		t.Fatalf("Calls = %+v, want [get_k8s_logs]", page.Events[0].Calls)
	}
}
