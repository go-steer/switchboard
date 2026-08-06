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
		fmt.Fprint(w, `{"session_id":"sess-123"}`)
	})
	sid, err := c.CreateSession(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if sid != "sess-123" {
		t.Fatalf("sid = %q, want sess-123", sid)
	}
}

func TestCreateSessionEmptyIDErrors(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"session_id":""}`)
	})
	if _, err := c.CreateSession(context.Background(), ""); err == nil {
		t.Fatal("expected error on empty session_id")
	}
}

func TestInjectBody(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sessions/sess-1/inject" {
			t.Errorf("path = %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), `"message":"hello"`) {
			t.Errorf("body = %s", b)
		}
		w.WriteHeader(http.StatusOK)
	})
	if err := c.Inject(context.Background(), "sess-1", "bob@example.com", "hello"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
}

func TestNon2xxErrors(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	if err := c.Wake(context.Background(), "s", ""); err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestSubscribeParsesSSE(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sessions/s/events" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: turn\ndata: line one\ndata: line two\n\nevent: done\ndata: {}\n\n")
	})

	var got []Event
	err := c.Subscribe(context.Background(), "s", "", func(e Event) error {
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
