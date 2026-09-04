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

import "testing"

func TestInboxChanged(t *testing.T) {
	for _, tt := range []struct {
		name     string
		data     string
		want     InboxChange
		wantOK   bool
		queued   bool
		dequeued bool
	}{
		{
			name:   "a message landing on the inbox",
			data:   `{"state":"queued","prompt_id":"0199-abc","queued_at":"2026-09-04T10:00:00Z"}`,
			want:   InboxChange{State: InboxQueued, PromptID: "0199-abc"},
			wantOK: true,
			queued: true,
		},
		{
			name:     "a turn taking it up",
			data:     `{"state":"dequeued","prompt_id":"0199-abc"}`,
			want:     InboxChange{State: InboxDequeued, PromptID: "0199-abc"},
			wantOK:   true,
			dequeued: true,
		},
		{
			// The spec reserves room for more states and names "injected" as a
			// candidate. Parsing has to succeed — the frame is well formed — while
			// neither predicate claims it, so a consumer switching on the two it
			// knows leaves the set alone rather than guessing a direction.
			name:   "a state this build does not know is neither",
			data:   `{"state":"injected","prompt_id":"0199-abc"}`,
			want:   InboxChange{State: "injected", PromptID: "0199-abc"},
			wantOK: true,
		},
		{
			name:   "fields from a newer daemon are ignored, not fatal",
			data:   `{"state":"queued","prompt_id":"0199-abc","caller":"users/1","priority":3}`,
			want:   InboxChange{State: InboxQueued, PromptID: "0199-abc"},
			wantOK: true,
			queued: true,
		},
		// Both fields are load-bearing: an event that does not name a message
		// cannot move a set keyed by message, and one that does not say what
		// became of it cannot say which way.
		{name: "no prompt_id", data: `{"state":"queued"}`},
		{name: "no state", data: `{"prompt_id":"0199-abc"}`},
		{name: "not an object", data: `"queued"`},
		{name: "not JSON", data: `{`},
		{name: "empty", data: ``},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := InboxChanged(tt.data)
			if ok != tt.wantOK {
				t.Fatalf("InboxChanged(%s) ok = %t, want %t", tt.data, ok, tt.wantOK)
			}
			if got != tt.want {
				t.Errorf("InboxChanged(%s) = %+v, want %+v", tt.data, got, tt.want)
			}
			if got.Queued() != tt.queued {
				t.Errorf("Queued() = %t, want %t", got.Queued(), tt.queued)
			}
			if got.Dequeued() != tt.dequeued {
				t.Errorf("Dequeued() = %t, want %t", got.Dequeued(), tt.dequeued)
			}
		})
	}
}
