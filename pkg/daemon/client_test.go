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
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
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

func TestStatusErrorTransient(t *testing.T) {
	cases := []struct {
		code int
		want bool
	}{
		{http.StatusBadRequest, false},
		{http.StatusNotFound, false},
		{http.StatusConflict, false},
		{http.StatusInternalServerError, true},
		{http.StatusServiceUnavailable, true},
		// The one 5xx that is permanent: the route is not implemented for
		// this session, and will not be until the process is replaced.
		// Retrying it is a loop; telling an operator to try again is a lie.
		{http.StatusNotImplemented, false},
	}
	for _, tc := range cases {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", tc.code)
		})
		err := c.Wake(context.Background(), Session{App: "a", ID: "s"}, "")
		if err == nil {
			t.Fatalf("status %d: want error", tc.code)
		}
		var se *StatusError
		if !errors.As(err, &se) {
			t.Fatalf("status %d: err = %v, want *StatusError", tc.code, err)
		}
		if se.StatusCode != tc.code {
			t.Errorf("status %d: StatusError.StatusCode = %d", tc.code, se.StatusCode)
		}
		if se.Transient() != tc.want {
			t.Errorf("status %d: Transient() = %v, want %v", tc.code, se.Transient(), tc.want)
		}
		if got := IsTransient(err); got != tc.want {
			t.Errorf("status %d: IsTransient() = %v, want %v", tc.code, got, tc.want)
		}
	}
}

func TestIsTransientNonStatusError(t *testing.T) {
	if IsTransient(nil) {
		t.Error("IsTransient(nil) = true, want false")
	}
	if !IsTransient(errors.New("connection refused")) {
		t.Error("IsTransient(network error) = false, want true (no structured rejection to say otherwise)")
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
		want []ToolCall
	}{
		{
			name: "single tool call",
			data: `{"seq":13,"event":{"Content":{"parts":[{"functionCall":{"name":"lookup"}}],"role":"model"}}}`,
			want: []ToolCall{{Name: "lookup"}},
		},
		{
			name: "multiple tool calls in order",
			data: `{"seq":14,"event":{"Content":{"parts":[{"functionCall":{"name":"a"}},{"functionCall":{"name":"b"}}],"role":"model"}}}`,
			want: []ToolCall{{Name: "a"}, {Name: "b"}},
		},
		{
			name: "text mixed with a tool call yields only the call",
			data: `{"seq":15,"event":{"Content":{"parts":[{"text":"let me check"},{"functionCall":{"name":"lookup"}}],"role":"model"}}}`,
			want: []ToolCall{{Name: "lookup"}},
		},
		{
			name: "id and argument summary come through",
			data: `{"seq":16,"event":{"Content":{"parts":[{"functionCall":{"id":"c1","name":"bash","args":{"command":"kubectl get pods -A"}}}],"role":"model"}}}`,
			want: []ToolCall{{ID: "c1", Name: "bash", Arg: "kubectl get pods -A"}},
		},
		{
			name: "a call with no name is skipped",
			data: `{"seq":17,"event":{"Content":{"parts":[{"functionCall":{"id":"c1"}},{"functionCall":{"name":"b"}}],"role":"model"}}}`,
			want: []ToolCall{{Name: "b"}},
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
			if got := ToolCalls(tc.data); !slices.Equal(got, tc.want) {
				t.Fatalf("ToolCalls = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestToolResults pins the half of the wire the gateway was blind to. The
// role filter that ToolCalls needs is exactly what kept results invisible:
// they are authored by the tool and the daemon labels the event "user".
func TestToolResults(t *testing.T) {
	cases := []struct {
		name string
		data string
		want []ToolResult
	}{
		{
			name: "a user-authored result is read, not filtered out",
			data: `{"seq":20,"event":{"Content":{"parts":[{"functionResponse":{"id":"c1","name":"bash","response":{"exit_code":0}}}],"role":"user"}}}`,
			want: []ToolResult{{ID: "c1", Name: "bash"}},
		},
		{
			name: "a non-zero exit is a failure with its code",
			data: `{"seq":21,"event":{"Content":{"parts":[{"functionResponse":{"id":"c2","name":"bash","response":{"exit_code":2}}}],"role":"user"}}}`,
			want: []ToolResult{{ID: "c2", Name: "bash", Failed: true, Detail: "exit 2"}},
		},
		{
			name: "an error field is a failure with no detail",
			data: `{"seq":22,"event":{"Content":{"parts":[{"functionResponse":{"name":"read","response":{"error":"no such file /etc/shadow-backup"}}}],"role":"user"}}}`,
			want: []ToolResult{{Name: "read", Failed: true}},
		},
		{
			name: "an unrecognised response shape is a success",
			data: `{"seq":23,"event":{"Content":{"parts":[{"functionResponse":{"name":"read","response":{"content":"hello"}}}],"role":"user"}}}`,
			want: []ToolResult{{Name: "read"}},
		},
		{
			name: "several results in one frame stay in order",
			data: `{"seq":24,"event":{"Content":{"parts":[{"functionResponse":{"id":"a","name":"x","response":{}}},{"functionResponse":{"id":"b","name":"y","response":{"exit_code":1}}}],"role":"user"}}}`,
			want: []ToolResult{{ID: "a", Name: "x"}, {ID: "b", Name: "y", Failed: true, Detail: "exit 1"}},
		},
		{
			name: "a call is not a result",
			data: `{"seq":25,"event":{"Content":{"parts":[{"functionCall":{"name":"bash"}}],"role":"model"}}}`,
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
			if got := ToolResults(tc.data); !slices.Equal(got, tc.want) {
				t.Fatalf("ToolResults = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestVerdict covers the response shapes verdict claims to read, and — more to
// the point — that everything else is called a success. Guessing failure from
// an unrecognised shape would put a red cross against calls that worked.
func TestVerdict(t *testing.T) {
	cases := []struct {
		name       string
		resp       map[string]any
		wantFailed bool
		wantDetail string
	}{
		{name: "nil response", resp: nil},
		{name: "empty response", resp: map[string]any{}},
		{name: "zero exit", resp: map[string]any{"exit_code": float64(0)}},
		{name: "camel exit", resp: map[string]any{"exitCode": float64(3)}, wantFailed: true, wantDetail: "exit 3"},
		{name: "returncode", resp: map[string]any{"returncode": float64(1)}, wantFailed: true, wantDetail: "exit 1"},
		// An HTTP status is not an exit code. Reading it as one inverted every
		// call an HTTP tool made: 200 is not a non-zero exit, it is the whole
		// point of the request succeeding.
		{name: "http 200 is a success", resp: map[string]any{"status_code": float64(200)}},
		{name: "http 204 is a success", resp: map[string]any{"status_code": float64(204)}},
		{name: "http 302 is a success", resp: map[string]any{"status_code": float64(302)}},
		{name: "http 101 is not a failure", resp: map[string]any{"status_code": float64(101)}},
		{name: "http 404 fails, and says so as a status", resp: map[string]any{"status_code": float64(404)}, wantFailed: true, wantDetail: "HTTP 404"},
		{name: "http 500 fails", resp: map[string]any{"status_code": float64(500)}, wantFailed: true, wantDetail: "HTTP 500"},
		{
			// Both spellings, as for exit_code: statusCode is the field name
			// on a Node response object, so a tool built on one reports its
			// 404 that way and had it read as a success.
			name:       "http status in camelCase",
			resp:       map[string]any{"statusCode": float64(404)},
			wantFailed: true,
			wantDetail: "HTTP 404",
		},
		{
			// Not in the range a status code occupies, so it is not one: fall
			// through rather than invent a verdict for it.
			name: "a status code out of HTTP range is ignored",
			resp: map[string]any{"status_code": float64(0)},
		},
		{
			name:       "a status code out of HTTP range defers to exit_code",
			resp:       map[string]any{"status_code": float64(9999), "exit_code": float64(2)},
			wantFailed: true,
			wantDetail: "exit 2",
		},
		// A 2xx says the request arrived and nothing more — the least specific
		// signal in the object. Returning success on it hid every failure a
		// tool reported alongside a successful transport.
		{
			name:       "a 2xx does not overrule an error the tool reported",
			resp:       map[string]any{"status_code": float64(200), "error": "connection reset by peer"},
			wantFailed: true,
		},
		{
			name:       "a 2xx does not overrule a non-zero exit code",
			resp:       map[string]any{"status_code": float64(200), "exit_code": float64(1)},
			wantFailed: true,
			wantDetail: "exit 1",
		},
		{
			name:       "a 4xx settles the call before the other keys are read",
			resp:       map[string]any{"status_code": float64(503), "exit_code": float64(0)},
			wantFailed: true,
			wantDetail: "HTTP 503",
		},
		{
			// A string exit code is not a number, so the exit branch declines it
			// and the error branch has nothing to say either.
			name: "non-numeric exit code is not read",
			resp: map[string]any{"exit_code": "0"},
		},
		{
			// Non-zero is a failure whatever the number, but the number itself
			// is tool-authored and goes into a chat room, so an implausible one
			// is reported without detail. int() on an out-of-range float64 is
			// not defined in Go either.
			name:       "an absurd exit code fails with no detail",
			resp:       map[string]any{"exit_code": 1234567890123456789.0},
			wantFailed: true,
		},
		{name: "an out-of-range exit code fails with no detail", resp: map[string]any{"exit_code": 1e300}, wantFailed: true},
		{name: "a fractional exit code fails with no detail", resp: map[string]any{"exit_code": 2.5}, wantFailed: true},
		{name: "a negative exit code is a signal, and renders", resp: map[string]any{"exit_code": float64(-9)}, wantFailed: true, wantDetail: "exit -9"},
		{name: "error string", resp: map[string]any{"error": "boom"}, wantFailed: true},
		{name: "err string", resp: map[string]any{"err": "boom"}, wantFailed: true},
		{name: "null error is not a failure", resp: map[string]any{"error": nil}},
		{name: "blank error is not a failure", resp: map[string]any{"error": "  "}},
		// A tool that reports success as a falsy error field is as common as
		// one that omits it. Reading mere presence as failure marked every one
		// of those calls ❌.
		{name: "error false is not a failure", resp: map[string]any{"error": false}},
		{name: "error zero is not a failure", resp: map[string]any{"error": float64(0)}},
		{name: "an empty error object is not a failure", resp: map[string]any{"error": map[string]any{}}},
		{name: "an empty error list is not a failure", resp: map[string]any{"error": []any{}}},
		{name: "error true is a failure", resp: map[string]any{"error": true}, wantFailed: true},
		{name: "a populated error object is a failure", resp: map[string]any{"error": map[string]any{"code": "E1"}}, wantFailed: true},
		{
			// The tool answered with output and no verdict field at all. Success.
			name: "output only",
			resp: map[string]any{"stdout": "hello", "duration_ms": float64(12)},
		},
		{
			// exit_code wins: a tool that reports both is reporting the code.
			name:       "exit code is read before the error field",
			resp:       map[string]any{"exit_code": float64(0), "error": "warning: deprecated"},
			wantFailed: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			failed, detail := verdict(tc.resp)
			if failed != tc.wantFailed || detail != tc.wantDetail {
				t.Fatalf("verdict = (%v, %q), want (%v, %q)", failed, detail, tc.wantFailed, tc.wantDetail)
			}
		})
	}
}

// TestSummariseArg pins the disclosure decision: one argument, scalars only,
// clamped, redacted, and stable across replays.
func TestSummariseArg(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{name: "no arguments", args: nil, want: ""},
		{
			name: "the preferred key wins over an alphabetically earlier one",
			args: map[string]any{"command": "ls -l", "background": "true"},
			want: "ls -l",
		},
		{
			name: "preference order decides between two preferred keys",
			args: map[string]any{"path": "/etc/hosts", "command": "cat"},
			want: "cat",
		},
		{
			name: "no preferred key falls back to the first scalar by name",
			args: map[string]any{"zebra": "z", "alpha": "a"},
			want: "a",
		},
		{
			// Sorted fallback, not map order: a notice that renders a different
			// argument on every reconnect replay is worse than one that renders
			// none.
			name: "the fallback skips composites to reach a scalar",
			args: map[string]any{"aaa": []any{1, 2}, "bbb": map[string]any{"k": "v"}, "ccc": "shown"},
			want: "shown",
		},
		{
			name: "a composite under a preferred key is skipped, not flattened",
			args: map[string]any{"command": []any{"sh", "-c", "echo hi"}, "note": "fallback"},
			want: "fallback",
		},
		{name: "numbers render", args: map[string]any{"limit": float64(42)}, want: "42"},
		{name: "bools render", args: map[string]any{"force": true}, want: "true"},
		{name: "a blank string is not a scalar worth showing", args: map[string]any{"query": "   "}, want: ""},
		{name: "nothing scalar at all", args: map[string]any{"body": map[string]any{"a": 1}}, want: ""},
		{
			name: "newlines are flattened to one line",
			args: map[string]any{"command": "set -e\ncd /tmp\nls"},
			want: "set -e cd /tmp ls",
		},
		{
			name: "a long argument is clamped with an ellipsis",
			args: map[string]any{"command": strings.Repeat("x", 300)},
			want: strings.Repeat("x", argSummaryCap-1) + "…",
		},
		{
			name: "a secret in the shown argument is redacted",
			args: map[string]any{"command": "curl -H 'Authorization: Bearer ya29.aaaaaaaaaa' https://x"},
			want: "curl -H 'Authorization: Bearer <redacted>' https://x",
		},
		{
			// The blast radius of one shown field: a token in a second argument is
			// not disclosed because the second argument is never rendered.
			name: "a secret in an argument that is not shown is never rendered",
			args: map[string]any{"command": "deploy", "token": "ghp_aaaaaaaaaaaaaaaaaaaa"},
			want: "deploy",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := summariseArg(tc.args); got != tc.want {
				t.Fatalf("summariseArg = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClampArgCutsOnARuneBoundary keeps a chat client from being handed half a
// rune. The clamp is a byte budget and the input is multi-byte on purpose.
func TestClampArgCutsOnARuneBoundary(t *testing.T) {
	got := clampArg(strings.Repeat("é", 200))
	if !utf8.ValidString(got) {
		t.Fatalf("clampArg produced invalid UTF-8: %q", got)
	}
	if len(got) > argSummaryCap+len("…") {
		t.Fatalf("clampArg = %d bytes, want at most %d", len(got), argSummaryCap+len("…"))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("clampArg = %q, want an ellipsis on the end", got)
	}
}

// TestSummariseArgHoldsItsBoundAfterRedaction pins the bound README and
// summariseArg both promise. `<redacted>` is longer than some of what it
// replaces, so redaction can grow a string: clamping only before it meant the
// documented cap was not a cap, and an argument with several short secrets in
// it came out longer than the one that had none.
func TestSummariseArgHoldsItsBoundAfterRedaction(t *testing.T) {
	// Short values under credential names: every replacement is a net gain.
	var b strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, "--token=%d ", i)
	}
	in := b.String()
	got := summariseArg(map[string]any{"command": in})
	if len(got) > argSummaryCap+len("…") {
		t.Fatalf("summariseArg = %d bytes for a %d-byte argument, want at most %d\n%q",
			len(got), len(in), argSummaryCap+len("…"), got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("summariseArg produced invalid UTF-8: %q", got)
	}
	if !strings.Contains(got, "<redacted>") {
		t.Fatalf("summariseArg = %q, want the secrets redacted before the cut", got)
	}
}

// TestSummariseArgDoesNotPublishATruncatedKey. The first cut has to be wider
// than the visible summary, because cutting can destroy the shape redaction
// matches on: an `sk-` key truncated below its length floor stops looking like
// a key, and clamping to argSummaryCap first published its head where the whole
// thing would have been elided.
func TestSummariseArgDoesNotPublishATruncatedKey(t *testing.T) {
	key := "sk-" + strings.Repeat("K", 40)
	got := summariseArg(map[string]any{"command": strings.Repeat("a", 100) + " " + key})
	if strings.Contains(got, "sk-K") {
		t.Fatalf("summariseArg published the head of a key that was cut below its floor:\n%s", got)
	}
}

// TestRedactionWindowLeavesEnoughToFillTheSummary. With the first cut landing
// on a word boundary, the window's width is no longer what keeps a credential
// whole — it is what keeps the summary worth reading. Redaction shrinks what it
// matches, so a window as narrow as the summary itself can spend the whole of
// it on one long secret and return two words where there was a command.
func TestRedactionWindowLeavesEnoughToFillTheSummary(t *testing.T) {
	arg := "SECRET_ONE=" + strings.Repeat("a", 300) + " " +
		strings.Repeat("run-a-real-command ", 12)
	got := summariseArg(map[string]any{"command": arg})
	if strings.Contains(got, "aaa") {
		t.Fatalf("the secret was not redacted, so this measures nothing:\n%s", got)
	}
	if len(got) < argSummaryCap-10 {
		t.Fatalf("the summary is %d bytes of a %d-byte budget; the window left nothing to say:\n%s",
			len(got), argSummaryCap, got)
	}
}

// TestSummariseArgDoesNotPublishAKeyStraddlingTheRedactionWindow. Widening the
// first cut is not on its own enough, because redaction *shrinks* what it
// matches: enough long secrets ahead of a key pull the cut back into the
// visible summary however wide the window was made, and the same truncated head
// is published from one boundary further out. Here three named secrets collapse
// 900-odd bytes to 65, and the key lands astride the window.
//
// Cutting on a word boundary is what actually settles it — a credential is one
// token, so it is either matched whole or absent.
func TestSummariseArgDoesNotPublishAKeyStraddlingTheRedactionWindow(t *testing.T) {
	filler := func(name string, n int, c string) string {
		return name + "=" + strings.Repeat(c, n) + " "
	}
	arg := filler("SECRET_ONE", 300, "a") + filler("SECRET_TWO", 300, "b") +
		filler("SECRET_TRE", 303, "c") + "sk-" + strings.Repeat("K", 48)
	if len(arg) <= redactWindow {
		t.Fatalf("the fixture is %d bytes, which does not reach the %d-byte window", len(arg), redactWindow)
	}
	got := summariseArg(map[string]any{"command": arg})
	if strings.Contains(got, "sk-K") {
		t.Fatalf("summariseArg published the head of a key cut by the redaction window:\n%s", got)
	}
}

// TestRedact covers the credential shapes the net claims to catch, and records
// that it is a net: the last case is a secret it does not recognise. That is
// accepted, not a bug — see summariseArg.
func TestRedact(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "flag value", in: "--token=abc123", want: "--token=<redacted>"},
		{name: "long flag with a space", in: "curl -u user --password hunter2", want: "curl -u user --password <redacted>"},
		{
			// The space-separated form is anchored to a flag on purpose: an
			// unanchored "auth" would eat the subcommand here.
			name: "a subcommand that reads like a credential name is left alone",
			in:   "gcloud auth login --quiet",
			want: "gcloud auth login --quiet",
		},
		{
			// A quoted value ends at its own closing quote, so the JSON around
			// it stays readable.
			name: "json field",
			in:   `{"api_key": "sk-live-xyz"}`,
			want: `{"api_key": "<redacted>"}`,
		},
		{name: "env assignment", in: "SECRET=shh run", want: "SECRET=<redacted> run"},
		{
			// The name may sit inside a longer identifier: this is how the
			// environment variables that hold real credentials are spelled, and
			// requiring the keyword to be the whole name missed all of them.
			name: "a credential word inside a longer identifier",
			in:   "set AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMIbPx",
			want: "set AWS_SECRET_ACCESS_KEY=<redacted>",
		},
		{
			// The header form: the credential follows the scheme, not the field
			// name. Matching "Authorization:" instead would have elided the
			// word "Bearer" and left the token in the room.
			name: "an authorization header",
			in:   "curl -H 'Authorization: Bearer c2VjcmV0LXRva2VuLXY5' https://internal",
			want: "curl -H 'Authorization: Bearer <redacted>' https://internal",
		},
		{
			name: "a password in a url",
			in:   "psql postgres://user:s3cr3t@db/app",
			want: "psql postgres://user:<redacted>@db/app",
		},
		{
			// The "@" is what makes userinfo userinfo. Without requiring it,
			// every URL with a port read as a credential.
			name: "a host and port is not a credential",
			in:   "curl https://example.com:8080/path",
			want: "curl https://example.com:8080/path",
		},
		{
			// Reaching forward for that "@" has to stop at the punctuation
			// that ends a URL in running text. The argument is already
			// flattened to one line, so whitespace alone does not bound the
			// search, and a class that allowed quotes and commas ran from this
			// port all the way to an ordinary email address — eating the port,
			// the key and the local part, and leaving invalid JSON behind.
			name: "a url and an email address in one line",
			in:   `{"url":"http://host:8080","email":"alice@example.com"}`,
			want: `{"url":"http://host:8080","email":"alice@example.com"}`,
		},
		{
			// An empty value must stay empty. Reaching across the space took
			// the next word instead, hiding a flag and redacting no secret.
			// The spaced form now needs whitespace on *both* sides of its
			// operator, which `--token=` cannot offer.
			name: "an empty flag value does not swallow the next flag",
			in:   "run --token= --verbose next",
			want: "run --token= --verbose next",
		},
		{
			// The same guard, with the next token not a flag: excluding a
			// leading "-" on the value covered only the case where it was one.
			name: "an empty flag value does not swallow the next word",
			in:   "run --token= next --verbose",
			want: "run --token= next --verbose",
		},
		{
			// And the inverse cost of that exclusion: a real secret is allowed
			// to begin with "-".
			name: "a spaced value may begin with a dash",
			in:   "password: -abc123def",
			want: "password: <redacted>",
		},
		{
			// A plural credential word is still a credential word. The suffix
			// rule required a separator, so `credentials` and `secrets` — the
			// spelling GCP payloads and Compose files use — matched nothing.
			name: "a plural credential word",
			in:   `gcloud --credentials abcdef123456 --secrets=xyz789`,
			want: `gcloud --credentials <redacted> --secrets=<redacted>`,
		},
		{
			// Digits continue a name too: SECRET1 is not a different kind of
			// thing from SECRET.
			name: "a numbered credential name",
			in:   "SECRET1=abcdef123456 TOKEN2=xyz789",
			want: "SECRET1=<redacted> TOKEN2=<redacted>",
		},
		{
			// `token` is the word left unpluralised, because a `tokens` is
			// nearly always a count. This is the cost of that and it is meant.
			name: "a plural token is read as a count, not a secret",
			in:   "--max-tokens=4096 --max_tokens 512",
			want: "--max-tokens=4096 --max_tokens 512",
		},
		{
			// `sk-` needs a length floor: an issued key is dozens of characters
			// and the bare prefix turns up inside ordinary words.
			name: "sk- inside an ordinary word is not a key",
			in:   "aws s3 cp s3://bucket/some-sk-thing .",
			want: "aws s3 cp s3://bucket/some-sk-thing .",
		},
		{
			name: "a real openai-shaped key",
			in:   "export OPENAI_KEY=sk-abcdefghijklmnopqrstuvwxyz01",
			want: "export OPENAI_KEY=<redacted>",
		},
		// `name: value` — the dominant shape in YAML, k8s manifests, .env dumps
		// and `printenv | grep`, all of which reach a `command` argument.
		{name: "a spaced colon value", in: "password: hunter2", want: "password: <redacted>"},
		{name: "a spaced equals value", in: "api_key = abc123", want: "api_key = <redacted>"},
		{name: "an unquoted json value", in: `{"api_key": abc123}`, want: `{"api_key": <redacted>`},
		{
			name: "a password in a url with no username",
			in:   "redis://:onlypass@host:6379/0",
			want: "redis://:<redacted>@host:6379/0",
		},
		{name: "a lowercase pem header", in: "-----begin rsa private key----- MII", want: "<redacted> MII"},
		// The credential word must sit at a separator boundary. Unanchored, it
		// matched inside any identifier containing it, and redacting ordinary
		// commands teaches people to ignore the marker — `--max-tokens` alone
		// would have fired on most of this repository's own tooling.
		{name: "a sampling parameter is not a credential", in: "llm --max-tokens=512 --temp=0.2", want: "llm --max-tokens=512 --temp=0.2"},
		{name: "an underscored sampling parameter", in: "python train.py --max_tokens=4096", want: "python train.py --max_tokens=4096"},
		{name: "a path that ends in tokens", in: "cat /var/log/tokens:latest", want: "cat /var/log/tokens:latest"},
		{name: "a word beginning with a credential word", in: "rg 'tokenizer:rename' src/", want: "rg 'tokenizer:rename' src/"},
		{
			// The worst kind of false positive: a flag hidden and the actual
			// credential left in the room. `--anyauth` is a real curl flag.
			name: "a flag that merely contains auth",
			in:   "curl --anyauth -u alice:letmein https://x",
			want: "curl --anyauth -u alice:letmein https://x",
		},
		{
			// The length floor keeps prose out of the bearer alternative.
			name: "bearer as an english word",
			in:   "Bearer authentication is required",
			want: "Bearer authentication is required",
		},
		{name: "space-separated auth flag", in: "myctl --auth s3cr3t deploy", want: "myctl --auth <redacted> deploy"},
		{name: "space-separated bearer flag", in: "myctl --bearer opaque-value", want: "myctl --bearer <redacted>"},
		{name: "bare github token", in: "echo ghp_abcdefghijklmnop", want: "echo <redacted>"},
		{name: "bare slack token", in: "post xoxb-1-2-abcdef", want: "post <redacted>"},
		{name: "bare google key", in: "AIzaSyAAAAAAAAAAAAAAA is the key", want: "<redacted> is the key"},
		// No "Bearer " prefix on purpose: with one, this passed on the bearer
		// alternative and would have stayed green with the JWT branch deleted.
		{name: "a bare jwt", in: "cookie=eyJhbGciOiJIUzI1NiJ9.e30.x", want: "cookie=<redacted>"},
		{name: "a jwt with no name around it", in: "eyJhbGciOiJIUzI1NiJ9.e30.x", want: "<redacted>"},
		{name: "pem header", in: "-----BEGIN RSA PRIVATE KEY----- MII", want: "<redacted> MII"},
		{name: "ordinary text is untouched", in: "kubectl get pods -A", want: "kubectl get pods -A"},
		{
			// The honest case. A high-entropy string under a name the pattern set
			// has never seen goes through. This is what "a net, not a guarantee"
			// means, and why status and indicator mode exist.
			name: "an unrecognised secret shape survives",
			in:   "deploy --key-material 7f3a9c1e5b2d8406",
			want: "deploy --key-material 7f3a9c1e5b2d8406",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := redact(tc.in); got != tc.want {
				t.Fatalf("redact(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
