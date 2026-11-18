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
	"reflect"
	"testing"
)

func TestToolCallsSubagentAndPartialFiltering(t *testing.T) {
	if got := ToolCalls(`{"seq":0,"event":{"Content":{"parts":[{"functionCall":{"id":"c1","name":"bash"}}],"role":"model"},"Partial":true}}`); got != nil {
		t.Fatalf("partial ToolCalls = %+v, want nil", got)
	}
	if got := ToolResults(`{"seq":0,"event":{"Content":{"parts":[{"functionResponse":{"id":"c1","name":"bash","response":{"ok":true}}}],"role":"user"},"Partial":true}}`); got != nil {
		t.Fatalf("partial ToolResults = %+v, want nil", got)
	}

	raw := `{"seq":18,"event":{"Content":{"parts":[` +
		`{"functionCall":{"id":"s1","name":"spawn_agent","args":{"agent":"propose-change","task":"fix memory"}}},` +
		`{"functionCall":{"id":"s2","name":"spawn_agent","args":{"name":"cluster-check","agent":"triage"}}},` +
		`{"functionCall":{"id":"s3","name":"get_pod","args":{"name":"billing-0"}}}` +
		`],"role":"model"}}}`
	want := []ToolCall{
		{ID: "s1", Name: "spawn_agent", Arg: "propose-change", Subagent: "propose-change"},
		{ID: "s2", Name: "spawn_agent", Arg: "cluster-check", Subagent: "cluster-check"},
		{ID: "s3", Name: "get_pod", Arg: "billing-0"},
	}
	if got := ToolCalls(raw); !reflect.DeepEqual(got, want) {
		t.Fatalf("ToolCalls = %+v, want %+v", got, want)
	}
}

func TestClientConfigRoundTrip(t *testing.T) {
	cfg := Config{BaseURL: "http://127.0.0.1:7777", BearerToken: "tok", OmitAssertedCaller: true}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := c.Config(); got != cfg {
		t.Fatalf("Config() = %+v, want %+v", got, cfg)
	}
}
