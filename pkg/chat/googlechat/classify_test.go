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

package googlechat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	chatv1 "google.golang.org/api/chat/v1"
	"google.golang.org/api/option"

	"github.com/go-steer/switchboard/pkg/chat"
)

func newRESTTestAdapter(t *testing.T, handler http.HandlerFunc) *Adapter {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	svc, err := chatv1.NewService(
		context.Background(),
		option.WithEndpoint(srv.URL),
		option.WithoutAuthentication(),
		option.WithHTTPClient(srv.Client()),
	)
	if err != nil {
		t.Fatalf("chatv1.NewService: %v", err)
	}
	return &Adapter{msg: restMessenger{svc: svc}, cards: CardsRich}
}

func TestClassifyPlatformErrorsOverREST(t *testing.T) {
	for _, tc := range []struct {
		code int
		want error
	}{
		{http.StatusNotFound, chat.ErrNotFound},
		{http.StatusGone, chat.ErrNotFound},
		{http.StatusForbidden, chat.ErrDenied},
		{http.StatusUnauthorized, chat.ErrDenied},
		{http.StatusTooManyRequests, nil},
		{http.StatusInternalServerError, nil},
	} {
		t.Run(fmt.Sprintf("%d", tc.code), func(t *testing.T) {
			a := newRESTTestAdapter(t, func(w http.ResponseWriter, _ *http.Request) {
				// Even if a permanent failure carries Retry-After, classification wins.
				w.Header().Set("Retry-After", "5")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.code)
				fmt.Fprintf(w, `{"error":{"code":%d,"message":"status-%d"}}`, tc.code, tc.code)
			})
			ref := chat.MessageRef{Conversation: "spaces/AAA:spaces/AAA/threads/T1", ID: "spaces/AAA/messages/M1"}
			_, sendErr := a.Send(context.Background(), chat.Reply{Conversation: ref.Conversation, Text: "hi"})
			updErr := a.Update(context.Background(), ref, chat.Reply{Conversation: ref.Conversation, Text: "hi"})
			delErr := a.Delete(context.Background(), ref)

			for name, err := range map[string]error{"Send": sendErr, "Update": updErr, "Delete": delErr} {
				if err == nil {
					t.Fatalf("%s returned no error for HTTP %d", name, tc.code)
				}
				if tc.want == nil {
					if errors.Is(err, chat.ErrNotFound) || errors.Is(err, chat.ErrDenied) {
						t.Errorf("%s classified HTTP %d as permanent: %v", name, tc.code, err)
					}
				} else {
					if !errors.Is(err, tc.want) {
						t.Errorf("%s error for HTTP %d = %v, want errors.Is %v", name, tc.code, err, tc.want)
					}
					if _, ok := chat.RetryAfter(err); ok {
						t.Errorf("%s permanent failure HTTP %d must not advertise RetryAfter", name, tc.code)
					}
				}
				if !strings.Contains(err.Error(), fmt.Sprintf("status-%d", tc.code)) {
					t.Errorf("%s error lost Chat's own wording: %v", name, err)
				}
			}
		})
	}
}

// A rate limit that came with a delay (Retry-After header in delta-seconds or
// HTTP-date, or google.rpc.RetryInfo in the JSON error details) surfaces it
// through chat.RetryAfter without tripping noActivityCards or altering the
// error message.
func TestARateLimitCarriesItsDelay(t *testing.T) {
	t.Run("Retry-After delta seconds over REST", func(t *testing.T) {
		a := newRESTTestAdapter(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "7")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"code":429,"message":"Quota exceeded for space"}}`)
		})
		ref := chat.MessageRef{Conversation: "spaces/AAA:spaces/AAA/threads/T1", ID: "spaces/AAA/messages/M1"}
		for name, err := range map[string]error{
			"Send": func() error {
				_, e := a.Send(context.Background(), chat.Reply{Conversation: ref.Conversation, Text: "hi"})
				return e
			}(),
			"Update": a.Update(context.Background(), ref, chat.Reply{Conversation: ref.Conversation, Text: "hi", Activity: &chat.Activity{Title: "Done · 5s · 1 call", Status: chat.StepDone}}),
		} {
			if err == nil {
				t.Fatalf("%s: no error from 429", name)
			}
			after, ok := chat.RetryAfter(err)
			if !ok || after != 7*time.Second {
				t.Errorf("%s: RetryAfter = %v, %t; want 7s (%v)", name, after, ok, err)
			}
			if !strings.Contains(err.Error(), "Quota exceeded for space") {
				t.Errorf("%s: error %q lost Chat's message", name, err)
			}
			if errors.Is(err, chat.ErrNotFound) || errors.Is(err, chat.ErrDenied) {
				t.Errorf("%s: 429 classified as permanent", name)
			}
		}
		if !a.DrawsActivity() {
			t.Error("429 stopped activity cards; only a 400/413 refusal should")
		}
	})

	t.Run("google.rpc.RetryInfo in JSON details over REST", func(t *testing.T) {
		a := newRESTTestAdapter(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"code":429,"message":"Resource has been exhausted","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"3.5s"}]}}`)
		})
		ref := chat.MessageRef{Conversation: "spaces/AAA:spaces/AAA/threads/T1", ID: "spaces/AAA/messages/M1"}
		err := a.Update(context.Background(), ref, chat.Reply{
			Conversation: ref.Conversation,
			Text:         "✔ Done",
			Activity:     &chat.Activity{Title: "Done · 2s · 1 call", Status: chat.StepDone},
		})
		after, ok := chat.RetryAfter(err)
		if !ok || after != 3500*time.Millisecond {
			t.Fatalf("RetryAfter = %v, %t; want 3.5s (%v)", after, ok, err)
		}
	})

	t.Run("HTTP-date, non-positive, and overflow Retry-After headers", func(t *testing.T) {
		now := time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)
		future := now.Add(12 * time.Second).Format(http.TimeFormat)
		if d, ok := parseRetryAfterHeader(future, now); !ok || d != 12*time.Second {
			t.Errorf("HTTP-date Retry-After = %v, %t; want 12s", d, ok)
		}
		past := now.Add(-5 * time.Second).Format(http.TimeFormat)
		if d, ok := parseRetryAfterHeader(past, now); ok || d != 0 {
			t.Errorf("past HTTP-date Retry-After = %v, %t; want 0, false", d, ok)
		}
		for _, bad := range []string{"", "0", "-4", "not-a-date"} {
			if d, ok := parseRetryAfterHeader(bad, now); ok || d != 0 {
				t.Errorf("parseRetryAfterHeader(%q) = %v, %t; want 0, false", bad, d, ok)
			}
		}
		if d, ok := parseRetryAfterHeader("9223372036854775807", now); !ok || d != maxRetryAfterHeader {
			t.Errorf("overflow Retry-After = %v, %t; want capped %v", d, ok, maxRetryAfterHeader)
		}
	})
}
