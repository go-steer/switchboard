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
	"strings"
	"testing"

	"cloud.google.com/go/pubsub"
	chatv1 "google.golang.org/api/chat/v1"

	"github.com/go-steer/switchboard/pkg/chat"
)

func mustDecode(t *testing.T, payload string) *chatv1.DeprecatedEvent {
	t.Helper()
	ev, err := decodeEvent([]byte(payload))
	if err != nil {
		t.Fatalf("decodeEvent: %v", err)
	}
	return ev
}

func TestDecodeEventMalformed(t *testing.T) {
	if _, err := decodeEvent([]byte(`{"type": "MESSAGE", `)); err == nil {
		t.Fatalf("expected error for malformed json")
	}
}

func TestIsUserMessage(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    bool
	}{
		{
			name:    "human message",
			payload: `{"type": "MESSAGE", "message": {"argumentText": "hi", "sender": {"type": "HUMAN"}}}`,
			want:    true,
		},
		{
			name:    "non-message event",
			payload: `{"type": "ADDED_TO_SPACE", "space": {"name": "spaces/X"}}`,
			want:    false,
		},
		{
			name:    "message event with no message",
			payload: `{"type": "MESSAGE"}`,
			want:    false,
		},
		{
			name:    "bot sender",
			payload: `{"type": "MESSAGE", "message": {"argumentText": "loop?", "sender": {"type": "BOT"}}}`,
			want:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isUserMessage(mustDecode(t, tt.payload)); got != tt.want {
				t.Fatalf("isUserMessage = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMessageFromEvent(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		wantOK  bool
		wantMsg chat.Message
	}{
		{
			name: "message with argument text",
			payload: `{
				"type": "MESSAGE",
				"user": {"name": "users/123", "type": "HUMAN"},
				"space": {"name": "spaces/AAA"},
				"message": {
					"name": "spaces/AAA/messages/M1",
					"argumentText": " hello there ",
					"text": "@switchboard hello there",
					"thread": {"name": "spaces/AAA/threads/T1"},
					"sender": {"name": "users/123", "type": "HUMAN"}
				}
			}`,
			wantOK: true,
			wantMsg: chat.Message{
				Conversation: "spaces/AAA:spaces/AAA/threads/T1",
				Channel:      "spaces/AAA",
				Caller:       "users/123",
				Text:         "hello there",
			},
		},
		{
			name: "falls back to text when argumentText empty",
			payload: `{
				"type": "MESSAGE",
				"user": {"name": "users/9"},
				"space": {"name": "spaces/B"},
				"message": {"text": "  plain body  ", "thread": {"name": "spaces/B/threads/T"}}
			}`,
			wantOK: true,
			wantMsg: chat.Message{
				Conversation: "spaces/B:spaces/B/threads/T",
				Channel:      "spaces/B",
				Caller:       "users/9",
				Text:         "plain body",
			},
		},
		{
			name: "caller falls back to message sender",
			payload: `{
				"type": "MESSAGE",
				"space": {"name": "spaces/C"},
				"message": {"argumentText": "hi", "sender": {"name": "users/77", "type": "HUMAN"}}
			}`,
			wantOK: true,
			wantMsg: chat.Message{
				Conversation: "spaces/C:",
				Channel:      "spaces/C",
				Caller:       "users/77",
				Text:         "hi",
			},
		},
		{
			name:    "empty text ignored",
			payload: `{"type": "MESSAGE", "space": {"name": "spaces/E"}, "message": {"argumentText": "   ", "text": ""}}`,
			wantOK:  false,
		},
		{
			name:    "no space ignored",
			payload: `{"type": "MESSAGE", "message": {"argumentText": "hi"}}`,
			wantOK:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, ok := messageFromEvent(mustDecode(t, tt.payload))
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if tt.wantOK && msg != tt.wantMsg {
				t.Fatalf("message = %+v, want %+v", msg, tt.wantMsg)
			}
		})
	}
}

func TestCommandFromEvent(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		wantOK  bool
		wantCmd chat.Command
	}{
		{
			name: "slash command with verb and arg",
			payload: `{
				"type": "MESSAGE",
				"user": {"name": "users/5"},
				"space": {"name": "spaces/AAA"},
				"message": {
					"argumentText": "progress status",
					"slashCommand": {"commandId": "1"},
					"thread": {"name": "spaces/AAA/threads/T1"}
				}
			}`,
			wantOK: true,
			wantCmd: chat.Command{
				Channel: "spaces/AAA",
				Caller:  "users/5",
				Name:    "progress",
				Args:    []string{"status"},
			},
		},
		{
			name: "slash command verb only",
			payload: `{
				"type": "MESSAGE",
				"space": {"name": "spaces/AAA"},
				"message": {"argumentText": "PROGRESS", "slashCommand": {"commandId": "1"}}
			}`,
			wantOK: true,
			wantCmd: chat.Command{
				Channel: "spaces/AAA",
				Name:    "progress", // lower-cased
			},
		},
		{
			name: "bare slash command",
			payload: `{
				"type": "MESSAGE",
				"space": {"name": "spaces/AAA"},
				"message": {"argumentText": "", "slashCommand": {"commandId": "1"}}
			}`,
			wantOK: true,
			wantCmd: chat.Command{
				Channel: "spaces/AAA",
			},
		},
		{
			name:    "ordinary message is not a command",
			payload: `{"type": "MESSAGE", "space": {"name": "spaces/AAA"}, "message": {"argumentText": "progress status"}}`,
			wantOK:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, ok := commandFromEvent(mustDecode(t, tt.payload))
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if cmd.Channel != tt.wantCmd.Channel || cmd.Caller != tt.wantCmd.Caller || cmd.Name != tt.wantCmd.Name {
				t.Fatalf("cmd = %+v, want %+v", cmd, tt.wantCmd)
			}
			if strings.Join(cmd.Args, ",") != strings.Join(tt.wantCmd.Args, ",") {
				t.Fatalf("cmd.Args = %v, want %v", cmd.Args, tt.wantCmd.Args)
			}
		})
	}
}

func TestConversationKeyRoundTrip(t *testing.T) {
	tests := []struct {
		space, thread string
	}{
		{"spaces/AAA", "spaces/AAA/threads/T1"},
		{"spaces/BBB", ""}, // unthreaded space
	}
	for _, tt := range tests {
		key := conversationKey(tt.space, tt.thread)
		space, thread, ok := splitConversation(key)
		if !ok {
			t.Fatalf("splitConversation(%q) not ok", key)
		}
		if space != tt.space || thread != tt.thread {
			t.Fatalf("round-trip %q => (%q, %q), want (%q, %q)", key, space, thread, tt.space, tt.thread)
		}
	}
	if _, _, ok := splitConversation("no-colon"); ok {
		t.Fatalf("splitConversation of a key with no colon should not be ok")
	}
	if _, _, ok := splitConversation(":thread-only"); ok {
		t.Fatalf("splitConversation with empty space should not be ok")
	}
}

func TestChunk(t *testing.T) {
	if got := chunk("short", 100); len(got) != 1 || got[0] != "short" {
		t.Fatalf("short string should be one chunk, got %v", got)
	}

	// Prefers a newline break within the window.
	body := "line one\nline two\nline three"
	got := chunk(body, 12)
	for _, c := range got {
		if len(c) > 12 {
			t.Fatalf("chunk %q exceeds limit 12", c)
		}
	}
	if strings.Join(got, "") != body {
		t.Fatalf("chunks do not reassemble: %q", strings.Join(got, ""))
	}
	if got[0] != "line one\n" {
		t.Fatalf("first chunk should break on newline, got %q", got[0])
	}

	// Multi-byte runes are never split (each rune is 3 bytes here).
	multi := strings.Repeat("世", 10) // 30 bytes
	parts := chunk(multi, 7)         // limit not a multiple of 3
	if strings.Join(parts, "") != multi {
		t.Fatalf("multi-byte chunks do not reassemble")
	}
	for _, p := range parts {
		if len(p) > 7 {
			t.Fatalf("chunk %q exceeds limit 7", p)
		}
		if !utf8ValidWhole(p) {
			t.Fatalf("chunk %q split a multi-byte rune", p)
		}
	}
}

func utf8ValidWhole(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

// fakeMessenger records egress calls for the Send/Update/Delete tests.
type fakeMessenger struct {
	creates   []createCall
	patches   []patchCall
	deletes   []string
	n         int
	createErr error
	// assignThread, when set, is the thread the fake reports each created
	// message landed in — simulating Chat assigning a thread in a flat space.
	assignThread string
}

type createCall struct{ parent, text, thread string }
type patchCall struct{ name, text string }

func (f *fakeMessenger) create(_ context.Context, parent string, msg *chatv1.Message) (*chatv1.Message, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	thread := ""
	if msg.Thread != nil {
		thread = msg.Thread.Name
	}
	f.creates = append(f.creates, createCall{parent: parent, text: msg.Text, thread: thread})
	f.n++
	out := &chatv1.Message{Name: fmt.Sprintf("spaces/AAA/messages/M%d", f.n)}
	if f.assignThread != "" {
		out.Thread = &chatv1.Thread{Name: f.assignThread}
	}
	return out, nil
}

func (f *fakeMessenger) patch(_ context.Context, name, text string) error {
	f.patches = append(f.patches, patchCall{name: name, text: text})
	return nil
}

func (f *fakeMessenger) delete(_ context.Context, name string) error {
	f.deletes = append(f.deletes, name)
	return nil
}

func newTestAdapter(m messenger) *Adapter {
	return &Adapter{msg: m, logf: func(string, ...any) {}}
}

func TestSendThreadsAndReturnsFirstRef(t *testing.T) {
	f := &fakeMessenger{}
	a := newTestAdapter(f)
	ref, err := a.Send(context.Background(), chat.Reply{
		Conversation: "spaces/AAA:spaces/AAA/threads/T1",
		Text:         "hello",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(f.creates) != 1 {
		t.Fatalf("want 1 create, got %d", len(f.creates))
	}
	c := f.creates[0]
	if c.parent != "spaces/AAA" || c.thread != "spaces/AAA/threads/T1" || c.text != "hello" {
		t.Fatalf("unexpected create %+v", c)
	}
	if ref.ID != "spaces/AAA/messages/M1" || ref.Conversation != "spaces/AAA:spaces/AAA/threads/T1" {
		t.Fatalf("unexpected ref %+v", ref)
	}
}

func TestSendChunksLongReply(t *testing.T) {
	f := &fakeMessenger{}
	a := newTestAdapter(f)
	// Build a reply longer than the limit with newline break points.
	line := strings.Repeat("x", 3000) + "\n"
	body := line + line // 6002 bytes, one newline mid-way under 4096
	ref, err := a.Send(context.Background(), chat.Reply{
		Conversation: "spaces/AAA:spaces/AAA/threads/T1",
		Text:         body,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(f.creates) < 2 {
		t.Fatalf("expected reply to be split into >=2 messages, got %d", len(f.creates))
	}
	var reassembled strings.Builder
	for _, c := range f.creates {
		if len(c.text) > chatTextLimit {
			t.Fatalf("chunk exceeds limit: %d bytes", len(c.text))
		}
		if c.thread != "spaces/AAA/threads/T1" {
			t.Fatalf("every chunk must target the thread, got %q", c.thread)
		}
		reassembled.WriteString(c.text)
	}
	if reassembled.String() != strings.TrimSpace(body) {
		t.Fatalf("chunks do not reassemble to the reply")
	}
	if ref.ID != "spaces/AAA/messages/M1" {
		t.Fatalf("ref should be the first message, got %q", ref.ID)
	}
}

func TestSendFlatSpaceAdoptsThread(t *testing.T) {
	// A chunked reply in a flat space (no incoming thread) should adopt the
	// thread the first message landed in so later chunks stay together.
	f := &fakeMessenger{assignThread: "spaces/AAA/threads/assigned"}
	a := newTestAdapter(f)
	line := strings.Repeat("y", 3000) + "\n"
	if _, err := a.Send(context.Background(), chat.Reply{
		Conversation: "spaces/AAA:", // no thread
		Text:         line + line,
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(f.creates) < 2 {
		t.Fatalf("expected >=2 creates, got %d", len(f.creates))
	}
	if f.creates[0].thread != "" {
		t.Fatalf("first chunk should be top-level, got thread %q", f.creates[0].thread)
	}
	for i, c := range f.creates[1:] {
		if c.thread != "spaces/AAA/threads/assigned" {
			t.Fatalf("chunk %d should adopt the assigned thread, got %q", i+1, c.thread)
		}
	}
}

func TestSendTopLevelWhenNoThread(t *testing.T) {
	f := &fakeMessenger{}
	a := newTestAdapter(f)
	if _, err := a.Send(context.Background(), chat.Reply{
		Conversation: "spaces/AAA:",
		Text:         "hi",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(f.creates) != 1 || f.creates[0].thread != "" {
		t.Fatalf("expected a single top-level (no thread) create, got %+v", f.creates)
	}
}

func TestSendEmptyPostsNothing(t *testing.T) {
	f := &fakeMessenger{}
	a := newTestAdapter(f)
	ref, err := a.Send(context.Background(), chat.Reply{Conversation: "spaces/AAA:t", Text: "   "})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(f.creates) != 0 || ref.ID != "" {
		t.Fatalf("empty reply should post nothing, got creates=%d ref=%+v", len(f.creates), ref)
	}
}

func TestSendMalformedConversation(t *testing.T) {
	f := &fakeMessenger{}
	a := newTestAdapter(f)
	if _, err := a.Send(context.Background(), chat.Reply{Conversation: "no-colon", Text: "hi"}); err == nil {
		t.Fatalf("expected error for malformed conversation key")
	}
}

func TestSendPropagatesCreateError(t *testing.T) {
	f := &fakeMessenger{createErr: errors.New("boom")}
	a := newTestAdapter(f)
	if _, err := a.Send(context.Background(), chat.Reply{Conversation: "spaces/AAA:t", Text: "hi"}); err == nil {
		t.Fatalf("expected create error to propagate")
	}
}

func TestUpdateAndDelete(t *testing.T) {
	f := &fakeMessenger{}
	a := newTestAdapter(f)

	// Zero refs no-op without touching the service.
	if err := a.Update(context.Background(), chat.MessageRef{}, chat.Reply{Text: "x"}); err != nil {
		t.Fatalf("Update zero ref: %v", err)
	}
	if err := a.Delete(context.Background(), chat.MessageRef{}); err != nil {
		t.Fatalf("Delete zero ref: %v", err)
	}
	if len(f.patches) != 0 || len(f.deletes) != 0 {
		t.Fatalf("zero refs must not call the service")
	}

	ref := chat.MessageRef{Conversation: "spaces/AAA:t", ID: "spaces/AAA/messages/M1"}
	if err := a.Update(context.Background(), ref, chat.Reply{Text: " working "}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(f.patches) != 1 || f.patches[0].name != ref.ID || f.patches[0].text != "working" {
		t.Fatalf("unexpected patch %+v", f.patches)
	}
	if err := a.Delete(context.Background(), ref); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(f.deletes) != 1 || f.deletes[0] != ref.ID {
		t.Fatalf("unexpected delete %+v", f.deletes)
	}
}

// fakeHandler records the turns and commands dispatch routes to the router.
type fakeHandler struct {
	msgs []chat.Message
	cmds []chat.Command
	ack  string
	err  error
}

func (h *fakeHandler) Handle(_ context.Context, m chat.Message) error {
	h.msgs = append(h.msgs, m)
	return nil
}

func (h *fakeHandler) HandleCommand(_ context.Context, c chat.Command) (string, error) {
	h.cmds = append(h.cmds, c)
	return h.ack, h.err
}

// pubsubMessage wraps a payload as the Pub/Sub message dispatch consumes. Ack is
// a no-op in tests; the summary only cares that the payload routes correctly.
func TestDispatchRoutesCommandToHandleCommand(t *testing.T) {
	f := &fakeMessenger{}
	h := &fakeHandler{ack: "progress mode set to status"}
	a := newTestAdapter(f)

	payload := []byte(`{
		"type": "MESSAGE",
		"user": {"name": "users/5"},
		"space": {"name": "spaces/AAA"},
		"message": {
			"argumentText": "progress status",
			"slashCommand": {"commandId": "1"},
			"thread": {"name": "spaces/AAA/threads/T1"}
		}
	}`)
	a.dispatch(context.Background(), h, &pubsub.Message{Data: payload})

	if len(h.cmds) != 1 {
		t.Fatalf("want 1 command, got %d", len(h.cmds))
	}
	if len(h.msgs) != 0 {
		t.Fatalf("command must not become an agent turn: %+v", h.msgs)
	}
	cmd := h.cmds[0]
	if cmd.Name != "progress" || cmd.Channel != "spaces/AAA" || cmd.Caller != "users/5" {
		t.Fatalf("unexpected command %+v", cmd)
	}
	// The ack posts back into the invoking thread.
	if len(f.creates) != 1 {
		t.Fatalf("want 1 ack post, got %d", len(f.creates))
	}
	c := f.creates[0]
	if c.parent != "spaces/AAA" || c.thread != "spaces/AAA/threads/T1" || c.text != "progress mode set to status" {
		t.Fatalf("unexpected ack post %+v", c)
	}
}

func TestDispatchEmptyAckPostsNothing(t *testing.T) {
	f := &fakeMessenger{}
	h := &fakeHandler{ack: ""}
	a := newTestAdapter(f)

	payload := []byte(`{
		"type": "MESSAGE",
		"space": {"name": "spaces/AAA"},
		"message": {"argumentText": "progress", "slashCommand": {"commandId": "1"}}
	}`)
	a.dispatch(context.Background(), h, &pubsub.Message{Data: payload})

	if len(h.cmds) != 1 {
		t.Fatalf("want 1 command, got %d", len(h.cmds))
	}
	if len(f.creates) != 0 {
		t.Fatalf("empty ack must post nothing, got %+v", f.creates)
	}
}

func TestDispatchRoutesMessageToHandle(t *testing.T) {
	f := &fakeMessenger{}
	h := &fakeHandler{}
	a := newTestAdapter(f)

	payload := []byte(`{
		"type": "MESSAGE",
		"user": {"name": "users/7"},
		"space": {"name": "spaces/AAA"},
		"message": {"argumentText": "hello there", "thread": {"name": "spaces/AAA/threads/T1"}}
	}`)
	a.dispatch(context.Background(), h, &pubsub.Message{Data: payload})

	if len(h.msgs) != 1 || len(h.cmds) != 0 {
		t.Fatalf("want 1 turn and no command, got msgs=%+v cmds=%+v", h.msgs, h.cmds)
	}
	if h.msgs[0].Text != "hello there" || h.msgs[0].Caller != "users/7" {
		t.Fatalf("unexpected turn %+v", h.msgs[0])
	}
}
