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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(Config{BaseURL: srv.URL, BearerToken: "tok", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestNewValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"no base url", Config{BearerToken: "t"}},
		{"trailing slash", Config{BaseURL: "http://x/", BearerToken: "t"}},
		{"no token", Config{BaseURL: "http://x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

func TestCreateSessionSendsAuthAndCaller(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/sessions" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("X-Asserted-Caller"); got != "alice@example.com" {
			t.Errorf("X-Asserted-Caller = %q", got)
		}
		// The daemon requires Content-Type: application/json on every
		// write, even this body-less create.
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		w.WriteHeader(http.StatusCreated)
		// Daemon returns app + sessionID (camelCase), not session_id.
		fmt.Fprint(w, `{"app":"core-agent","sessionID":"sess-123","user":"alice@example.com"}`)
	})
	sess, err := c.CreateSession(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if sess.App != "core-agent" || sess.ID != "sess-123" {
		t.Fatalf("sess = %+v, want {core-agent sess-123}", sess)
	}
}

func TestCreateSessionEmptyIDErrors(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"app":"core-agent","sessionID":""}`)
	})
	if _, err := c.CreateSession(context.Background(), ""); err == nil {
		t.Fatal("expected error on empty sessionID")
	}
}

func TestCreateSessionEmptyAppErrors(t *testing.T) {
	// A missing app would make every app-qualified route malformed, so it
	// must fail at create rather than later.
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"app":"","sessionID":"s1"}`)
	})
	if _, err := c.CreateSession(context.Background(), ""); err == nil {
		t.Fatal("expected error on empty app")
	}
}

// TestDefaultClientHasNoWholeRequestTimeout guards the SSE blocker: a
// client-wide http.Client.Timeout also bounds response-body reads, which
// would force-close the long-lived Subscribe stream. Unary calls get
// their deadline from context instead.
func TestDefaultClientHasNoWholeRequestTimeout(t *testing.T) {
	c, err := New(Config{BaseURL: "http://x", BearerToken: "t"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.http.Timeout != 0 {
		t.Fatalf("default http client Timeout = %v, want 0 (would kill SSE)", c.http.Timeout)
	}
}

func TestInjectBody(t *testing.T) {
	sess := Session{App: "core-agent", ID: "sess-1"}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sessions/core-agent/sess-1/inject" {
			t.Errorf("path = %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), `"message":"hello"`) {
			t.Errorf("body = %s", b)
		}
		w.WriteHeader(http.StatusOK)
	})
	if err := c.Inject(context.Background(), sess, "bob@example.com", "hello"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
}

func TestNon2xxErrors(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	if err := c.Wake(context.Background(), Session{App: "a", ID: "s"}, ""); err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestSubscribeParsesSSE(t *testing.T) {
	sess := Session{App: "core-agent", ID: "s"}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sessions/core-agent/s/events" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("since"); got != "7" {
			t.Errorf("since = %q, want 7", got)
		}
		if got := r.URL.Query().Get("protocol"); got != protocolVersion {
			t.Errorf("protocol = %q, want %q", got, protocolVersion)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: turn\ndata: line one\ndata: line two\n\nevent: done\ndata: {}\n\n")
	})

	var got []Event
	err := c.Subscribe(context.Background(), sess, "", 7, func(e Event) error {
		got = append(got, e)
		return nil
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(got), got)
	}
	if got[0].Type != "turn" || got[0].Data != "line one\nline two" {
		t.Errorf("event0 = %+v", got[0])
	}
	if got[1].Type != "done" || got[1].Data != "{}" {
		t.Errorf("event1 = %+v", got[1])
	}
}

func TestAgentText(t *testing.T) {
	cases := []struct {
		name     string
		data     string
		wantOK   bool
		wantText string
		wantSeq  int64
		partial  bool
	}{
		{
			name:     "final model text",
			data:     `{"seq":12,"event":{"Content":{"parts":[{"text":"hello "},{"text":"world"}],"role":"model"},"Partial":false,"Author":"agent"}}`,
			wantOK:   true,
			wantText: "hello world",
			wantSeq:  12,
		},
		{
			name:     "partial chunk still parses but is flagged",
			data:     `{"seq":11,"event":{"Content":{"parts":[{"text":"hel"}],"role":"model"},"Partial":true}}`,
			wantOK:   true,
			wantText: "hel",
			partial:  true,
			wantSeq:  11,
		},
		{
			name:    "user-authored turn is not relayed",
			data:    `{"seq":9,"event":{"Content":{"parts":[{"text":"my question"}],"role":"user"}}}`,
			wantOK:  false,
			wantSeq: 9,
		},
		{
			name:    "tool-call event has no text",
			data:    `{"seq":13,"event":{"Content":{"parts":[{"functionCall":{"name":"x"}}],"role":"model"}}}`,
			wantOK:  false,
			wantSeq: 13,
		},
		{
			name:    "no content",
			data:    `{"seq":5,"event":{"Partial":false}}`,
			wantOK:  false,
			wantSeq: 5,
		},
		{
			name:   "malformed json",
			data:   `not json`,
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, ok := AgentText(tc.data)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (r = %+v)", ok, tc.wantOK, r)
			}
			if r.Seq != tc.wantSeq {
				t.Errorf("seq = %d, want %d", r.Seq, tc.wantSeq)
			}
			if tc.wantOK {
				if r.Text != tc.wantText {
					t.Errorf("text = %q, want %q", r.Text, tc.wantText)
				}
				if r.Partial != tc.partial {
					t.Errorf("partial = %v, want %v", r.Partial, tc.partial)
				}
			}
		})
	}
}

func TestToolCalls(t *testing.T) {
	cases := []struct {
		name string
		data string
		want []string
	}{
		{
			name: "single tool call",
			data: `{"seq":13,"event":{"Content":{"parts":[{"functionCall":{"name":"lookup"}}],"role":"model"}}}`,
			want: []string{"lookup"},
		},
		{
			name: "multiple tool calls in order",
			data: `{"seq":14,"event":{"Content":{"parts":[{"functionCall":{"name":"a"}},{"functionCall":{"name":"b"}}],"role":"model"}}}`,
			want: []string{"a", "b"},
		},
		{
			name: "text mixed with a tool call yields only the call",
			data: `{"seq":15,"event":{"Content":{"parts":[{"text":"let me check"},{"functionCall":{"name":"lookup"}}],"role":"model"}}}`,
			want: []string{"lookup"},
		},
		{
			name: "plain model text has no tool calls",
			data: `{"seq":12,"event":{"Content":{"parts":[{"text":"hello world"}],"role":"model"},"Partial":false}}`,
			want: nil,
		},
		{
			name: "user-authored event is ignored",
			data: `{"seq":9,"event":{"Content":{"parts":[{"functionCall":{"name":"x"}}],"role":"user"}}}`,
			want: nil,
		},
		{
			name: "malformed json",
			data: `not json`,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ToolCalls(tc.data)
			if len(got) != len(tc.want) {
				t.Fatalf("ToolCalls = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("ToolCalls = %v, want %v", got, tc.want)
				}
			}
		})
	}
}
