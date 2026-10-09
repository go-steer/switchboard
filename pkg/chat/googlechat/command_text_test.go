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

import "testing"

// A free-text argument keeps its spacing and line breaks, whether the verb
// comes from a configured command ID or from the first word of a catch-all
// command's text.
func TestACommandKeepsItsArgumentText(t *testing.T) {
	a := &Adapter{cmds: map[int64]string{2: "agent"}}
	mapped := a.commandOf(inbound{space: "spaces/A", cmdID: 2, cmdArgs: "infra list\n  the nodes"})
	if mapped.Name != "agent" || mapped.Text != "infra list\n  the nodes" || mapped.Args[0] != "infra" {
		t.Errorf("mapped = %+v", mapped)
	}
	catchAll := a.commandOf(inbound{space: "spaces/A", cmdID: 9, cmdArgs: "agent infra list\n  the nodes"})
	if catchAll.Name != "agent" || catchAll.Text != "infra list\n  the nodes" {
		t.Errorf("catch-all = %+v", catchAll)
	}
}
