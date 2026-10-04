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

package main

import (
	"strings"
	"testing"

	"github.com/go-steer/switchboard/pkg/daemon"
)

// A stream notice's tool name is bold, and bold shields nothing, so a
// model-supplied name must not be able to carry a link, open a code span, end
// the emphasis or italicise itself into the gateway's notice (caught in
// review: a [text](url) name rendered as a masked link on both platforms).
func TestABoldToolNameCarriesNoMarkup(t *testing.T) {
	for name, want := range map[string]string{
		"bash":                          "bash",
		"mcp__github__list_prs":         "mcp__github__list_prs",
		"k8s.get-pods:v1/x":             "k8s.get-pods:v1/x",
		"[click](https://evil.example)": "clickhttps://evil.example",
		"<https://evil.example|x>":      "https://evil.examplex",
		"ba`s*h":                        "bash",
		"_priv_":                        "priv",
		"a _b":                          "ab",
		"   ":                           "?",
		"***":                           "?",
	} {
		if got := boldName(name); got != want {
			t.Errorf("boldName(%q) = %q, want %q", name, got, want)
		}
	}
	got := activityText([]daemon.ToolCall{{ID: "1", Name: "[click](https://evil.example)", Arg: "ls"}}, nil, true)
	if strings.ContainsAny(strings.SplitN(got, "\n", 2)[0], "[]()<>|") {
		t.Errorf("notice header %q still carries link syntax", got)
	}
}
