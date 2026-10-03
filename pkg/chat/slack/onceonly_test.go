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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-steer/switchboard/pkg/chat"
)

// threadSlack is a fake Slack that keeps every posted message with its
// metadata and serves them back from conversations.replies, the way the real
// one does with include_all_metadata.
type threadSlack struct {
	mu      sync.Mutex
	posts   []map[string]any // as conversations.replies would return them
	reads   int
	noScope bool // conversations.replies answers missing_scope
	// pageThenLimit serves everything as page one with more to come, then
	// refuses page two as rate-limited.
	pageThenLimit bool
}

func (s *threadSlack) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		s.mu.Lock()
		ts := fmt.Sprintf("200.%03d", len(s.posts)+1)
		msg := map[string]any{"ts": ts, "text": r.FormValue("text")}
		if raw := r.FormValue("metadata"); raw != "" {
			var md map[string]any
			if err := json.Unmarshal([]byte(raw), &md); err != nil {
				t.Errorf("metadata %q is not JSON: %v", raw, err)
			}
			msg["metadata"] = md
		}
		s.posts = append(s.posts, msg)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"channel":"C0","ts":%q}`, ts)
	})
	mux.HandleFunc("/conversations.replies", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		s.mu.Lock()
		s.reads++
		if r.FormValue("include_all_metadata") != "1" {
			t.Errorf("conversations.replies without include_all_metadata; the keys would not come back")
		}
		body := map[string]any{"ok": true, "messages": s.posts, "has_more": false}
		if s.noScope {
			body = map[string]any{"ok": false, "error": "missing_scope"}
		}
		if s.pageThenLimit {
			if r.FormValue("cursor") == "" {
				body["has_more"] = true
				body["response_metadata"] = map[string]any{"next_cursor": "page2"}
			} else {
				body = map[string]any{"ok": false, "error": "ratelimited"}
			}
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (s *threadSlack) count() (posts, reads int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.posts), s.reads
}

// TestAKeyedReplyCarriesItsKey: the key rides in message metadata, where a
// later process can find it, and an ordinary post never reads the thread.
func TestAKeyedReplyCarriesItsKey(t *testing.T) {
	s := &threadSlack{}
	a := newTestAdapter(s.server(t).URL)
	if _, err := a.Send(context.Background(), chat.Reply{Conversation: "C0:100.5", Text: "all green", Key: "core-agent/s1#7"}); err != nil {
		t.Fatal(err)
	}
	posts, reads := s.count()
	if posts != 1 || reads != 0 {
		t.Fatalf("posts = %d, reads = %d; want one post and no lookup", posts, reads)
	}
	md, _ := s.posts[0]["metadata"].(map[string]any)
	payload, _ := md["event_payload"].(map[string]any)
	if md["event_type"] != metaEventType || payload[metaKey] != metaValue(partKey("core-agent/s1#7", 0)) {
		t.Errorf("metadata = %v, want the hashed part key under %s", md, metaEventType)
	}
	if strings.Contains(fmt.Sprint(md), "core-agent/s1") {
		t.Errorf("metadata = %v; the session reference is readable by other apps and must be hashed", md)
	}
}

// TestAVerifiedReplyAlreadyPostedIsNotPostedAgain is the point: a reply the
// last process may have posted before it died is looked for, and not posted
// twice.
func TestAVerifiedReplyAlreadyPostedIsNotPostedAgain(t *testing.T) {
	s := &threadSlack{}
	a := newTestAdapter(s.server(t).URL)
	r := chat.Reply{Conversation: "C0:100.5", Text: "all green", Key: "core-agent/s1#7"}
	if _, err := a.Send(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	r.Verify = true
	ref, err := a.Send(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if posts, reads := s.count(); posts != 1 || reads != 1 {
		t.Errorf("posts = %d, reads = %d; want the reply found and not reposted", posts, reads)
	}
	if ref.ID != "200.001" {
		t.Errorf("ref = %+v, want the message already in the thread", ref)
	}
}

// TestAVerifiedChunkedReplyPostsOnlyTheMissingParts: a crash part-way through
// a long reply left some parts in the thread; the next process posts the rest.
func TestAVerifiedChunkedReplyPostsOnlyTheMissingParts(t *testing.T) {
	s := &threadSlack{}
	a := newTestAdapter(s.server(t).URL)
	long := strings.Repeat("word ", slackTextLimit/5+50)
	parts := chunkMessage(toMrkdwn(long), slackTextLimit)
	if len(parts) < 2 {
		t.Fatalf("test text chunked into %d part(s), want several", len(parts))
	}
	// The first part made it out under the dead process.
	s.posts = append(s.posts, map[string]any{"ts": "199.000", "metadata": map[string]any{
		"event_type": metaEventType, "event_payload": map[string]any{metaKey: metaValue(partKey("k#9", 0))},
	}})

	ref, err := a.Send(context.Background(), chat.Reply{Conversation: "C0:100.5", Text: long, Key: "k#9", Verify: true})
	if err != nil {
		t.Fatal(err)
	}
	posts, _ := s.count()
	if posts != len(parts) {
		t.Errorf("thread holds %d message(s), want %d: the missing parts and nothing twice", posts, len(parts))
	}
	if ref.ID != "199.000" {
		t.Errorf("ref = %+v, want the first part, which was already there", ref)
	}
}

// TestAPartlyPostedReplyIsFinishedAsChunksEvenWithRichBlocks: with some parts
// in the thread, the blocks render is not tried — succeeding now where it was
// rejected before, it would post the whole reply on top of them.
func TestAPartlyPostedReplyIsFinishedAsChunksEvenWithRichBlocks(t *testing.T) {
	s := &threadSlack{}
	a := newTestAdapter(s.server(t).URL)
	a.richBlocks = true
	long := strings.Repeat("word ", slackTextLimit/5+50)
	parts := chunkMessage(toMrkdwn(long), slackTextLimit)
	s.posts = append(s.posts, map[string]any{"ts": "199.000", "metadata": map[string]any{
		"event_type": metaEventType, "event_payload": map[string]any{metaKey: metaValue(partKey("k#9", 0))},
	}})
	if _, err := a.Send(context.Background(), chat.Reply{Conversation: "C0:100.5", Text: long, Key: "k#9", Verify: true}); err != nil {
		t.Fatal(err)
	}
	if posts, _ := s.count(); posts != len(parts) {
		t.Errorf("thread holds %d message(s), want %d: the remaining chunks, no blocks copy", posts, len(parts))
	}
}

// TestAVerifiedReplyPostsWhenTheThreadCannotBeRead: without a history scope
// the lookup fails, and the reply is posted — the worst case is a duplicate,
// never a lost answer.
func TestAVerifiedReplyPostsWhenTheThreadCannotBeRead(t *testing.T) {
	s := &threadSlack{noScope: true}
	a := newTestAdapter(s.server(t).URL)
	var warned []string
	a.logf = func(_ chat.Level, format string, args ...any) { warned = append(warned, fmt.Sprintf(format, args...)) }
	r := chat.Reply{Conversation: "C0:100.5", Text: "all green", Key: "core-agent/s1#7", Verify: true}
	for range 2 {
		if _, err := a.Send(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	if posts, _ := s.count(); posts != 2 {
		t.Errorf("posts = %d, want both posted when nothing can be checked", posts)
	}
	scope := 0
	for _, w := range warned {
		if strings.Contains(w, "channels:history") {
			scope++
		}
	}
	if scope != 1 {
		t.Errorf("the missing-scope note was logged %d time(s), want once: %q", scope, warned)
	}
}

// TestALaterPageFailingKeepsWhatWasFound: page one found the reply, page two
// was rate-limited. What was found stands — the reply is not posted again.
func TestALaterPageFailingKeepsWhatWasFound(t *testing.T) {
	s := &threadSlack{pageThenLimit: true}
	a := newTestAdapter(s.server(t).URL)
	r := chat.Reply{Conversation: "C0:100.5", Text: "all green", Key: "core-agent/s1#7"}
	if _, err := a.Send(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	r.Verify = true
	if _, err := a.Send(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if posts, reads := s.count(); posts != 1 || reads != 2 {
		t.Errorf("posts = %d, reads = %d; want the reply found on page one and not reposted", posts, reads)
	}
}
