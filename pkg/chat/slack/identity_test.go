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
	"net/url"
	"sync"
	"testing"

	"github.com/go-steer/switchboard/pkg/chat"
)

// postRecorder fakes chat.postMessage, recording each form; with noScope it
// refuses any post that names a username, as Slack does without
// chat:write.customize.
type postRecorder struct {
	mu      sync.Mutex
	posts   []url.Values
	noScope bool
}

func (p *postRecorder) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		p.mu.Lock()
		p.posts = append(p.posts, r.Form)
		refuse := p.noScope && r.Form.Get("username") != ""
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if refuse {
			_, _ = w.Write([]byte(`{"ok":false,"error":"missing_scope","needed":"chat:write.customize"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C1","ts":"1.1"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

var agentB = &chat.AgentIdentity{Name: "b", Label: "Agent B", IconURL: "https://example.com/b.png"}

// An agent's reply is posted under the agent's name and icon.
func TestAnAgentsReplyIsPostedAsTheAgent(t *testing.T) {
	for name, rich := range map[string]bool{"text": false, "blocks": true} {
		t.Run(name, func(t *testing.T) {
			p := &postRecorder{}
			a := newTestAdapter(p.server(t).URL)
			a.richBlocks = rich
			if _, err := a.Send(context.Background(), chat.Reply{Conversation: "C1:9.9", Text: "hello", Agent: agentB}); err != nil {
				t.Fatal(err)
			}
			if len(p.posts) != 1 || p.posts[0].Get("username") != "Agent B" || p.posts[0].Get("icon_url") != agentB.IconURL {
				t.Errorf("posts = %v, want one as Agent B", p.posts)
			}
		})
	}
}

// An unsigned reply posts as the app, as before.
func TestAnUnsignedReplyPostsAsTheApp(t *testing.T) {
	p := &postRecorder{}
	a := newTestAdapter(p.server(t).URL)
	if _, err := a.Send(context.Background(), chat.Reply{Conversation: "C1:9.9", Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	if p.posts[0].Has("username") || p.posts[0].Has("icon_url") {
		t.Errorf("post = %v, want no identity", p.posts[0])
	}
}

// Without chat:write.customize the reply still goes out, as the app, and the
// app stops asking: one refused attempt, not one per post.
func TestWithoutTheScopeRepliesGoOutAsTheApp(t *testing.T) {
	p := &postRecorder{noScope: true}
	a := newTestAdapter(p.server(t).URL)
	for range 2 {
		if _, err := a.Send(context.Background(), chat.Reply{Conversation: "C1:9.9", Text: "hello", Agent: agentB}); err != nil {
			t.Fatalf("Send: %v, want the reply posted as the app", err)
		}
	}
	if len(p.posts) != 3 {
		t.Fatalf("posts = %d, want a refused one, a retry, then one as the app", len(p.posts))
	}
	if p.posts[1].Has("username") || p.posts[2].Has("username") {
		t.Errorf("posts after the refusal still name the agent: %v", p.posts[1:])
	}
}
