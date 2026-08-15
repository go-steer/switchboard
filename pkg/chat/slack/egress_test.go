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

package slack

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/slack-go/slack"

	"github.com/go-steer/switchboard/pkg/chat"
)

// newTestAdapter builds an Adapter whose Slack client is aimed at a fake API
// server, so the egress methods can be exercised without a real workspace.
func newTestAdapter(apiURL string) *Adapter {
	api := slack.New("xoxb-test", slack.OptionAPIURL(apiURL+"/"))
	return &Adapter{
		api:        api,
		mode:       CallerEmail,
		logf:       func(string, ...any) {},
		callerByID: make(map[string]string),
	}
}

// TestSendReturnsRef checks that Send posts into the conversation's channel +
// thread and hands back a ref carrying the posted message's ts.
func TestSendReturnsRef(t *testing.T) {
	var gotChannel, gotThread string
	mux := http.NewServeMux()
	mux.HandleFunc("/chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotChannel = r.FormValue("channel")
		gotThread = r.FormValue("thread_ts")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C0","ts":"111.111"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := newTestAdapter(srv.URL)
	ref, err := a.Send(context.Background(), chat.Reply{Conversation: "C0:100.5", Text: "hello"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotChannel != "C0" {
		t.Errorf("posted channel = %q, want C0", gotChannel)
	}
	if gotThread != "100.5" {
		t.Errorf("posted thread_ts = %q, want 100.5", gotThread)
	}
	if ref.Conversation != "C0:100.5" || ref.ID != "111.111" {
		t.Errorf("ref = %+v, want {C0:100.5 111.111}", ref)
	}
}

// TestSendThreadlessPostsTopLevel checks a conversation key with no thread —
// what the outbound ingress hands over when a caller posts into a channel with
// no thread to reply in — posts at the top level of the channel. Slack must
// see no thread_ts at all: an empty one is not the same as an absent one.
func TestSendThreadlessPostsTopLevel(t *testing.T) {
	var gotChannel string
	var sawThreadTS bool
	mux := http.NewServeMux()
	mux.HandleFunc("/chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotChannel = r.FormValue("channel")
		_, sawThreadTS = r.Form["thread_ts"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C0","ts":"111.111"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := newTestAdapter(srv.URL)
	ref, err := a.Send(context.Background(), chat.Reply{Conversation: "C0", Text: "digest"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotChannel != "C0" {
		t.Errorf("posted channel = %q, want C0", gotChannel)
	}
	if sawThreadTS {
		t.Error("a thread-less post sent thread_ts, want it omitted")
	}
	// The ref round-trips back through Update, and its ID is also the thread
	// the message rooted — a caller can follow up under it with "C0:111.111".
	if ref.Conversation != "C0" || ref.ID != "111.111" {
		t.Errorf("ref = %+v, want {C0 111.111}", ref)
	}
}

// TestSendThreadlessChunksStayTogether checks a long thread-less message keeps
// its parts together: the first post roots a thread and the rest reply under
// it, rather than scattering across the channel.
func TestSendThreadlessChunksStayTogether(t *testing.T) {
	var threads []string
	mux := http.NewServeMux()
	mux.HandleFunc("/chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		threads = append(threads, r.FormValue("thread_ts"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"channel":"C0","ts":"%d.000"}`, len(threads))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := newTestAdapter(srv.URL)
	// Two chunks: chunkMessage splits on newlines at the byte limit.
	long := strings.Repeat("a", slackTextLimit-1) + "\n" + strings.Repeat("b", 100)
	if _, err := a.Send(context.Background(), chat.Reply{Conversation: "C0", Text: long}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(threads) < 2 {
		t.Fatalf("posted %d messages, want the text split across at least 2", len(threads))
	}
	if threads[0] != "" {
		t.Errorf("first chunk sent thread_ts %q, want none", threads[0])
	}
	for i, ts := range threads[1:] {
		if ts != "1.000" {
			t.Errorf("chunk %d thread_ts = %q, want 1.000 (the first message's ts)", i+1, ts)
		}
	}
}

// TestUpdateTargetsMessage checks Update edits the ref'd message (chat.update
// with the right channel + ts) and that a zero ref is a no-op.
func TestUpdateTargetsMessage(t *testing.T) {
	var calls int
	var gotChannel, gotTS string
	mux := http.NewServeMux()
	mux.HandleFunc("/chat.update", func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = r.ParseForm()
		gotChannel = r.FormValue("channel")
		gotTS = r.FormValue("ts")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C0","ts":"111.111"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := newTestAdapter(srv.URL)

	// Zero ref: no API call.
	if err := a.Update(context.Background(), chat.MessageRef{}, chat.Reply{Text: "x"}); err != nil {
		t.Fatalf("Update(zero): %v", err)
	}
	if calls != 0 {
		t.Fatalf("zero-ref Update made %d calls, want 0", calls)
	}

	ref := chat.MessageRef{Conversation: "C0:100.5", ID: "111.111"}
	if err := a.Update(context.Background(), ref, chat.Reply{Text: "edited"}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if calls != 1 {
		t.Fatalf("Update made %d calls, want 1", calls)
	}
	if gotChannel != "C0" || gotTS != "111.111" {
		t.Errorf("update targeted channel=%q ts=%q, want C0/111.111", gotChannel, gotTS)
	}
}

// TestDeleteTargetsMessage checks Delete removes the ref'd message and that a
// zero ref is a no-op.
func TestDeleteTargetsMessage(t *testing.T) {
	var calls int
	var gotChannel, gotTS string
	mux := http.NewServeMux()
	mux.HandleFunc("/chat.delete", func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = r.ParseForm()
		gotChannel = r.FormValue("channel")
		gotTS = r.FormValue("ts")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C0","ts":"111.111"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := newTestAdapter(srv.URL)

	if err := a.Delete(context.Background(), chat.MessageRef{}); err != nil {
		t.Fatalf("Delete(zero): %v", err)
	}
	if calls != 0 {
		t.Fatalf("zero-ref Delete made %d calls, want 0", calls)
	}

	ref := chat.MessageRef{Conversation: "C0:100.5", ID: "111.111"}
	if err := a.Delete(context.Background(), ref); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if calls != 1 {
		t.Fatalf("Delete made %d calls, want 1", calls)
	}
	if gotChannel != "C0" || gotTS != "111.111" {
		t.Errorf("delete targeted channel=%q ts=%q, want C0/111.111", gotChannel, gotTS)
	}
}
