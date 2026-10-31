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

package ax

import (
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/go-steer/switchboard/internal/axapi"
)

// fieldNumbers is the top-level field numbers of a marshalled message.
func fieldNumbers(t *testing.T, b []byte) map[protowire.Number]bool {
	t.Helper()
	out := map[protowire.Number]bool{}
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			t.Fatal(protowire.ParseError(n))
		}
		b = b[n:]
		m := protowire.ConsumeFieldValue(num, typ, b)
		if m < 0 {
			t.Fatal(protowire.ParseError(m))
		}
		b = b[m:]
		out[num] = true
	}
	return out
}

// The task a template becomes is sent with the rebased fork's numbers (#182).
// A copy of the proto from before the rebase sent http as 8, which a rebased
// ax-server would read as egress, so a task lost its port without an error.
func TestTaskSpecWireNumbers(t *testing.T) {
	tmpl, err := ParseTemplate([]byte(`kind: Task
spec:
  image: agent-mast
  workspace: {name: w}
  config: {model: gemini}
  egress:
    - hosts: [example.com]
      ports: [443]
      credentials:
        - {header: Authorization, prefix: "Bearer ", credentialUri: "secret://x"}
  http: {port: 8484}
  idle: {suspendAfter: 10m, busyPath: /busy}
  onCompletion: Suspend
`), "test")
	if err != nil {
		t.Fatal(err)
	}
	spec := tmpl.task.GetSpec()
	b, err := proto.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	got := fieldNumbers(t, b)
	for num, field := range map[protowire.Number]string{
		1: "image", 5: "workspace", 7: "config", 100: "egress", 101: "http", 102: "idle", 103: "on_completion",
	} {
		if !got[num] {
			t.Errorf("%s is not field %d; fields sent: %v", field, num, got)
		}
	}
	for _, tc := range []struct {
		name string
		msg  proto.Message
		want []protowire.Number
	}{
		{"TaskHTTP", spec.GetHttp(), []protowire.Number{1}},
		{"TaskIdle", spec.GetIdle(), []protowire.Number{1, 2}},
		{"EgressRule", spec.GetEgress()[0], []protowire.Number{1, 2, 3}},
		{"CredentialInjection", spec.GetEgress()[0].GetCredentials()[0], []protowire.Number{1, 2, 3}},
	} {
		b, err := proto.Marshal(tc.msg)
		if err != nil {
			t.Fatal(err)
		}
		got := fieldNumbers(t, b)
		if len(got) != len(tc.want) {
			t.Errorf("%s: fields %v, want %v", tc.name, got, tc.want)
		}
		for _, n := range tc.want {
			if !got[n] {
				t.Errorf("%s: no field %d in %v", tc.name, n, got)
			}
		}
	}
	// The status fields Ensure and Inspect read.
	st, err := proto.Marshal(&axapi.TaskStatus{Phase: "Unknown", Conditions: []*axapi.Condition{{Type: "Ready", Reason: "SubstrateUnavailable"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := fieldNumbers(t, st); !got[1] || !got[7] {
		t.Errorf("TaskStatus: phase 1 and conditions 7, got %v", got)
	}
}
