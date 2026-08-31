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
	"slices"
	"testing"
)

func TestStatusUpdated(t *testing.T) {
	for _, tt := range []struct {
		name   string
		data   string
		want   string
		wantOK bool
	}{
		{
			name:   "the snapshot the daemon opens a stream with",
			data:   `{"model":"gemini-3.7-flash","provider":"vertex","perm_mode":"yolo","turn_state":"idle","context_pct":3}`,
			want:   TurnStateIdle,
			wantOK: true,
		},
		{
			name:   "turn start",
			data:   `{"turn_state":"streaming"}`,
			want:   TurnStateStreaming,
			wantOK: true,
		},
		{
			name:   "blocked on a human",
			data:   `{"turn_state":"awaiting_permission"}`,
			want:   TurnStateAwaitingPermission,
			wantOK: true,
		},
		{
			// The other half of the blocked pair. Both wire strings are spelled
			// out here because no daemon emits either yet: there is no live
			// traffic to catch a typo in the constant, and the day one starts
			// emitting is the day a wrong spelling reads as an unknown state
			// and the turn parks with nothing logged.
			name:   "blocked on an elicitation",
			data:   `{"turn_state":"awaiting_elicit"}`,
			want:   TurnStateAwaitingElicit,
			wantOK: true,
		},
		{
			// The v1.4.0 capabilities merge frame, which this build does not
			// read. It must not make the frame unparseable.
			name:   "fields from a newer daemon are ignored, not fatal",
			data:   `{"turn_state":"idle","capabilities":{"features":{"mcp":true}},"paused_since":"2026-08-19T00:00:00Z"}`,
			want:   TurnStateIdle,
			wantOK: true,
		},
		{
			name:   "an unrecognized state is preserved, not normalized",
			data:   `{"turn_state":"compacting"}`,
			want:   "compacting",
			wantOK: true,
		},
		{name: "no turn_state", data: `{"model":"gemini-3.7-flash"}`},
		{name: "not an object", data: `"streaming"`},
		{name: "not JSON", data: `{`},
		{name: "empty", data: ``},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := StatusUpdated(tt.data)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (got %+v)", ok, tt.wantOK, got)
			}
			if got.TurnState != tt.want {
				t.Errorf("TurnState = %q, want %q", got.TurnState, tt.want)
			}
		})
	}
}

// TestSessionStatusPredicates pins the one distinction the relay turns on: a
// session blocked on a human is not idle. Reading it as idle would retire the
// thread's progress message for a turn that is still owed an answer.
func TestSessionStatusPredicates(t *testing.T) {
	for _, tt := range []struct {
		state                  string
		working, blocked, idle bool
	}{
		{TurnStateStreaming, true, false, false},
		{TurnStateIdle, false, false, true},
		{TurnStateAwaitingPermission, false, true, false},
		{TurnStateAwaitingElicit, false, true, false},
		// A state added by a daemon newer than this build is none of the three:
		// it must not be guessed into one, least of all idle.
		{"compacting", false, false, false},
		{"", false, false, false},
	} {
		s := SessionStatus{TurnState: tt.state}
		if s.Working() != tt.working || s.Blocked() != tt.blocked || s.Idle() != tt.idle {
			t.Errorf("%q: working/blocked/idle = %v/%v/%v, want %v/%v/%v",
				tt.state, s.Working(), s.Blocked(), s.Idle(), tt.working, tt.blocked, tt.idle)
		}
	}
}

func TestStreamOpened(t *testing.T) {
	// The frame core-agent 1.5.0 actually sends, trimmed to the fields read.
	const live = `{"protocol_version":"1.5.0","server":"core-agent/0.9.2",` +
		`"event_types":["status-update","usage-update","inbox","turn-complete","turn-error",` +
		`"pause","stream-chunk","tool-call","tool-result"],` +
		`"features":{"mcp":true},"slash_commands":["compact"],"caller_id":"someone@example.com"}`

	c, ok := StreamOpened(live)
	if !ok {
		t.Fatal("a live capabilities frame did not parse")
	}
	if c.ProtocolVersion != "1.5.0" || c.Server != "core-agent/0.9.2" {
		t.Errorf("unexpected identity %+v", c)
	}
	if !c.Advertises(EventTurnError) || c.Advertises("telepathy") {
		t.Errorf("Advertises is wrong on %v", c.EventTypes)
	}
	// The daemon does not list its legacy event name, so nothing may require it.
	if c.Advertises(EventAgent) {
		t.Error("the live frame was expected not to advertise the legacy agent event")
	}
	if got := c.Missing(EventStatusUpdate, EventUsage, EventTurnComplete, EventTurnError); len(got) != 0 {
		t.Errorf("Missing = %v, want none against a current daemon", got)
	}

	// An older daemon: what it does not send is named, in the order asked.
	old, ok := StreamOpened(`{"protocol_version":"1.1.0","server":"core-agent/0.4.0",` +
		`"event_types":["usage-update","turn-complete"]}`)
	if !ok {
		t.Fatal("an older capabilities frame did not parse")
	}
	want := []string{EventStatusUpdate, EventTurnError}
	if got := old.Missing(EventStatusUpdate, EventUsage, EventTurnComplete, EventTurnError); !slices.Equal(got, want) {
		t.Errorf("Missing = %v, want %v", got, want)
	}

	// A frame that advertises nothing reports nothing missing: one fact about
	// the daemon, not a line per event switchboard happens to want.
	bare, ok := StreamOpened(`{"protocol_version":"1.0.0"}`)
	if !ok {
		t.Fatal("a frame carrying only a version did not parse")
	}
	if got := bare.Missing(EventTurnError); len(got) != 0 {
		t.Errorf("Missing = %v, want none when nothing was advertised", got)
	}

	for _, bad := range []string{``, `{`, `{}`, `[]`} {
		if c, ok := StreamOpened(bad); ok {
			t.Errorf("StreamOpened(%q) = (%+v, true), want not ok", bad, c)
		}
	}
}

// The optional routes a daemon serves arrive on the frame that opens every
// stream, so a caller can know whether a session will answer /perms before it
// asks. Dropping the map means probing for a 501 to learn something that was
// already on the wire.
func TestStreamOpenedReadsWhichOptionalRoutesTheDaemonServes(t *testing.T) {
	c, ok := StreamOpened(`{"protocol_version":"1.5.0","server":"core-agent/0.9.2",` +
		`"event_types":["turn-complete"],"features":{"mcp":true,"perms_stream":true,"retired":false}}`)
	if !ok {
		t.Fatal("a frame carrying features did not parse")
	}
	if !c.Offers(FeaturePermsStream) {
		t.Errorf("Offers(%q) = false on %v", FeaturePermsStream, c.Features)
	}
	// Present-and-false is the same answer as absent: not offered.
	if c.Offers("retired") || c.Offers("telepathy") {
		t.Errorf("Offers said yes to something the daemon did not serve: %v", c.Features)
	}
	// Features and EventTypes answer different questions, and neither may be
	// read off the other's list.
	if c.Advertises(FeaturePermsStream) || c.Offers("turn-complete") {
		t.Error("the two capability lists were conflated")
	}
}

// A daemon predating the features map — or one whose agent registered no
// optional routes at all — reads as offering nothing, rather than panicking
// on a nil map.
func TestOffersIsFalseWhenTheDaemonSentNoFeatures(t *testing.T) {
	c, ok := StreamOpened(`{"protocol_version":"1.1.0","event_types":["turn-complete"]}`)
	if !ok {
		t.Fatal("a frame with no features did not parse")
	}
	if c.Offers(FeaturePermsStream) {
		t.Error("Offers said yes against a daemon that listed no features")
	}
	if (Capabilities{}).Offers(FeaturePermsStream) {
		t.Error("the zero Capabilities offers something")
	}
}

// A frame carrying only features is still a frame worth reading: the
// capability list is the reason to parse it at all.
func TestStreamOpenedAcceptsAFrameThatIsOnlyFeatures(t *testing.T) {
	c, ok := StreamOpened(`{"features":{"perms_stream":true}}`)
	if !ok {
		t.Fatal("a features-only frame was discarded")
	}
	if !c.Offers(FeaturePermsStream) {
		t.Errorf("Offers = false on %v", c.Features)
	}
}
