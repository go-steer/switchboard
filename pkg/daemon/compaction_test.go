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

// A checkpoint row core-agent writes after a mark_task_done turn is
// model-role text, but history bookkeeping, not a reply. This is the frame as
// captured from core-agent 24f2ae32 on the live Slack rig, where it was posted
// into the thread ahead of the user's next question.
const checkpointFrame = `{"seq":294,"event":{"Content":{"parts":[{"text":"All requested tasks have been completed. \n\n### Summary:\n- **Cluster:** ` + "`std-simian-test`" + `"}],"role":"model"},"CustomMetadata":{"checkpoint_note":"Checked namespaces in the std-simian-test cluster via kubectl get ns. Found 20 namespaces.","compaction":"checkpoint"},"Partial":false,"ID":"checkpoint-1791211305549810986","InvocationID":"","Author":"core_agent"}}`

func TestAgentTextSkipsCompactionRows(t *testing.T) {
	for name, frame := range map[string]string{
		"checkpoint": checkpointFrame,
		"summary":    `{"seq":7,"event":{"Content":{"parts":[{"text":"Summary of the conversation so far"}],"role":"model"},"CustomMetadata":{"compaction":"summary"},"Author":"core_agent"}}`,
	} {
		r, ok := AgentText(frame)
		if ok {
			t.Errorf("%s: AgentText = %+v, ok; want it skipped as history bookkeeping", name, r)
		}
		if r.Seq == 0 {
			t.Errorf("%s: seq dropped; the relay still needs it to advance its cursor", name)
		}
	}
	// Other metadata does not hide an answer.
	r, ok := AgentText(`{"seq":8,"event":{"Content":{"parts":[{"text":"the answer"}],"role":"model"},"CustomMetadata":{"finish_reason":"STOP"}}}`)
	if !ok || r.Text != "the answer" {
		t.Errorf("answer with unrelated metadata = %+v, %v; want it relayed", r, ok)
	}
}
