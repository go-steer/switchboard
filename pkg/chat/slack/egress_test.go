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
	"net/http"
	"net/http/httptest"
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
