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
	"strings"
	"testing"
)

func mustDecode(t *testing.T, payload string) inbound {
	t.Helper()
	in, err := decodeEvent([]byte(payload))
	if err != nil {
		t.Fatalf("decodeEvent: %v", err)
	}
	return in
}

func TestDecodeEventMalformed(t *testing.T) {
	if _, err := decodeEvent([]byte(`{"type": "MESSAGE", `)); err == nil {
		t.Fatalf("expected error for malformed json")
	}
}

// TestDecodeAddonEvents covers the Google Workspace add-on dialect — the
// framework this gateway targets, where the payload that is set is the type.
func TestDecodeAddonEvents(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    inbound
	}{
		{
			name: "message",
			payload: `{
				"chat": {
					"user": {"name": "users/123", "type": "HUMAN"},
					"space": {"name": "spaces/AAA"},
					"messagePayload": {
						"message": {
							"name": "spaces/AAA/messages/M1",
							"argumentText": " hello there ",
							"text": "@switchboard hello there",
							"thread": {"name": "spaces/AAA/threads/T1"},
							"sender": {"name": "users/123", "type": "HUMAN"}
						},
						"space": {"name": "spaces/AAA"}
					}
				}
			}`,
			want: inbound{
				kind:   kindMessage,
				space:  "spaces/AAA",
				thread: "spaces/AAA/threads/T1",
				caller: "users/123",
				text:   "hello there",
			},
		},
		{
			name: "message falls back to text and sender",
			payload: `{
				"chat": {
					"messagePayload": {
						"message": {
							"text": "  plain body  ",
							"sender": {"name": "users/77", "type": "HUMAN"},
							"space": {"name": "spaces/B"}
						}
					}
				}
			}`,
			want: inbound{kind: kindMessage, space: "spaces/B", caller: "users/77", text: "plain body"},
		},
		{
			name: "bot message ignored",
			payload: `{
				"chat": {
					"space": {"name": "spaces/AAA"},
					"messagePayload": {"message": {"argumentText": "loop?", "sender": {"type": "BOT"}}}
				}
			}`,
			want: inbound{},
		},
		{
			name: "empty message ignored",
			payload: `{
				"chat": {
					"space": {"name": "spaces/AAA"},
					"messagePayload": {"message": {"argumentText": "   ", "text": ""}}
				}
			}`,
			want: inbound{},
		},
		{
			name: "slash command with quoted command id",
			payload: `{
				"chat": {
					"user": {"name": "users/5"},
					"space": {"name": "spaces/AAA"},
					"appCommandPayload": {
						"appCommandMetadata": {"appCommandId": "2", "appCommandType": "SLASH_COMMAND"},
						"message": {"argumentText": " stream ", "thread": {"name": "spaces/AAA/threads/T1"}},
						"space": {"name": "spaces/AAA"}
					}
				}
			}`,
			want: inbound{
				kind:    kindCommand,
				space:   "spaces/AAA",
				thread:  "spaces/AAA/threads/T1",
				caller:  "users/5",
				cmdID:   2,
				cmdArgs: "stream",
			},
		},
		{
			name: "quick command with unquoted id and no message",
			payload: `{
				"chat": {
					"user": {"name": "users/5"},
					"space": {"name": "spaces/AAA"},
					"appCommandPayload": {
						"appCommandMetadata": {"appCommandId": 7, "appCommandType": "QUICK_COMMAND"}
					}
				}
			}`,
			want: inbound{kind: kindCommand, space: "spaces/AAA", caller: "users/5", cmdID: 7},
		},
		{
			name: "button click",
			payload: `{
				"chat": {
					"user": {"name": "users/9"},
					"space": {"name": "spaces/AAA"},
					"buttonClickedPayload": {
						"message": {
							"name": "spaces/AAA/messages/M9",
							"thread": {"name": "spaces/AAA/threads/T1"}
						}
					}
				},
				"commonEventObject": {
					"parameters": {"switchboard_command": "progress", "switchboard_arg": "stream"}
				}
			}`,
			want: inbound{
				kind:        kindButton,
				space:       "spaces/AAA",
				thread:      "spaces/AAA/threads/T1",
				caller:      "users/9",
				messageName: "spaces/AAA/messages/M9",
				params: map[string]string{
					"switchboard_command": "progress",
					"switchboard_arg":     "stream",
				},
			},
		},
		{
			name: "button click with no parameters is not ours",
			payload: `{
				"chat": {
					"space": {"name": "spaces/AAA"},
					"buttonClickedPayload": {"message": {"name": "spaces/AAA/messages/M9"}}
				}
			}`,
			want: inbound{},
		},
		{
			name:    "added to space",
			payload: `{"chat": {"addedToSpacePayload": {"space": {"name": "spaces/AAA"}}}}`,
			want:    inbound{kind: kindWelcome, space: "spaces/AAA"},
		},
		{
			name: "added by mention defers to the message event",
			payload: `{"chat": {"addedToSpacePayload": {
				"space": {"name": "spaces/AAA"}, "interactionAdd": true}}}`,
			want: inbound{},
		},
		{
			name:    "removed from space ignored",
			payload: `{"chat": {"space": {"name": "spaces/AAA"}, "removedFromSpacePayload": {}}}`,
			want:    inbound{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertInbound(t, mustDecode(t, tt.payload), tt.want)
		})
	}
}

// TestDecodeLegacyEvents covers the Chat API interaction-events dialect, still
// spoken by an app whose operator has not yet converted it to an add-on.
func TestDecodeLegacyEvents(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    inbound
	}{
		{
			name: "message",
			payload: `{
				"type": "MESSAGE",
				"user": {"name": "users/123", "type": "HUMAN"},
				"space": {"name": "spaces/AAA"},
				"message": {
					"argumentText": " hello there ",
					"text": "@switchboard hello there",
					"thread": {"name": "spaces/AAA/threads/T1"},
					"sender": {"name": "users/123", "type": "HUMAN"}
				}
			}`,
			want: inbound{
				kind:   kindMessage,
				space:  "spaces/AAA",
				thread: "spaces/AAA/threads/T1",
				caller: "users/123",
				text:   "hello there",
			},
		},
		{
			name:    "bot sender ignored",
			payload: `{"type": "MESSAGE", "space": {"name": "spaces/A"}, "message": {"argumentText": "loop?", "sender": {"type": "BOT"}}}`,
			want:    inbound{},
		},
		{
			name:    "message with no space ignored",
			payload: `{"type": "MESSAGE", "message": {"argumentText": "hi"}}`,
			want:    inbound{},
		},
		{
			name: "slash command",
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
			want: inbound{
				kind:    kindCommand,
				space:   "spaces/AAA",
				thread:  "spaces/AAA/threads/T1",
				caller:  "users/5",
				cmdID:   1,
				cmdArgs: "progress status",
			},
		},
		{
			name: "card clicked",
			payload: `{
				"type": "CARD_CLICKED",
				"user": {"name": "users/9"},
				"space": {"name": "spaces/AAA"},
				"message": {"name": "spaces/AAA/messages/M9", "thread": {"name": "spaces/AAA/threads/T1"}},
				"common": {"parameters": {"switchboard_command": "progress", "switchboard_arg": "off"}}
			}`,
			want: inbound{
				kind:        kindButton,
				space:       "spaces/AAA",
				thread:      "spaces/AAA/threads/T1",
				caller:      "users/9",
				messageName: "spaces/AAA/messages/M9",
				params: map[string]string{
					"switchboard_command": "progress",
					"switchboard_arg":     "off",
				},
			},
		},
		{
			name:    "added to space",
			payload: `{"type": "ADDED_TO_SPACE", "space": {"name": "spaces/AAA"}}`,
			want:    inbound{kind: kindWelcome, space: "spaces/AAA"},
		},
		{
			// No second event follows a legacy mention-add — the triggering
			// message is inlined here — so this event has to answer it.
			name: "added to space by mention answers the message it carries",
			payload: `{"type": "ADDED_TO_SPACE", "space": {"name": "spaces/AAA"},
				"message": {"argumentText": "hi", "sender": {"type": "HUMAN"}}}`,
			want: inbound{kind: kindMessage, space: "spaces/AAA", text: "hi"},
		},
		{
			name: "added to space by a command runs the command",
			payload: `{"type": "ADDED_TO_SPACE", "space": {"name": "spaces/AAA"},
				"message": {"argumentText": " status ", "sender": {"type": "HUMAN"},
					"slashCommand": {"commandId": "1"}}}`,
			want: inbound{kind: kindCommand, space: "spaces/AAA", cmdID: 1, cmdArgs: "status"},
		},
		{
			// A quick command has no message, so APP_COMMAND is the only way it
			// can arrive in this dialect.
			name: "app command",
			payload: `{"type": "APP_COMMAND", "space": {"name": "spaces/AAA"},
				"user": {"name": "users/1"}, "appCommandMetadata": {"appCommandId": 2}}`,
			want: inbound{kind: kindCommand, space: "spaces/AAA", caller: "users/1", cmdID: 2},
		},
		{
			name:    "unknown type ignored",
			payload: `{"type": "REMOVED_FROM_SPACE", "space": {"name": "spaces/AAA"}}`,
			want:    inbound{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertInbound(t, mustDecode(t, tt.payload), tt.want)
		})
	}
}

// TestDecodeBadCommandID guards the reason appCommandId has its own
// unmarshaller: the two dialects carry the same number in different shapes
// (bare int32 vs int64-format string), and an ID this code cannot read must
// degrade to "no command" rather than drop the whole event.
func TestDecodeBadCommandID(t *testing.T) {
	unreadable := []string{`"not-a-number"`, `{}`, `1.5`, `99999999999999999999`, `""`, `null`}
	for _, id := range unreadable {
		payload := `{"chat": {"space": {"name": "spaces/A"},
			"appCommandPayload": {"appCommandMetadata": {"appCommandId": ` + id + `}}}}`
		in := mustDecode(t, payload)
		if in.kind != kindCommand || in.cmdID != 0 {
			t.Fatalf("appCommandId %s: got %+v, want a command with id 0", id, in)
		}
	}
	// Both shapes of a readable ID reach the same value.
	for _, id := range []string{`3`, `"3"`} {
		payload := `{"chat": {"space": {"name": "spaces/A"},
			"appCommandPayload": {"appCommandMetadata": {"appCommandId": ` + id + `}}}}`
		if in := mustDecode(t, payload); in.cmdID != 3 {
			t.Fatalf("appCommandId %s: cmdID = %d, want 3", id, in.cmdID)
		}
	}
}

func assertInbound(t *testing.T, got, want inbound) {
	t.Helper()
	if got.kind != want.kind || got.space != want.space || got.thread != want.thread ||
		got.caller != want.caller || got.text != want.text || got.cmdID != want.cmdID ||
		got.cmdArgs != want.cmdArgs || got.messageName != want.messageName {
		t.Fatalf("inbound = %+v, want %+v", got, want)
	}
	if len(got.params) != len(want.params) {
		t.Fatalf("params = %v, want %v", got.params, want.params)
	}
	for k, v := range want.params {
		if got.params[k] != v {
			t.Fatalf("params[%q] = %q, want %q", k, got.params[k], v)
		}
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
	// The ingress API accepts a bare channel as a conversation, so a key with
	// no separator is a top-level post in that space, not a malformed key.
	space, thread, ok := splitConversation("spaces/CCC")
	if !ok || space != "spaces/CCC" || thread != "" {
		t.Fatalf("bare space => (%q, %q, %v), want (spaces/CCC, \"\", true)", space, thread, ok)
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
		if strings.ContainsRune(p, '�') {
			t.Fatalf("chunk %q split a multi-byte rune", p)
		}
	}
}
