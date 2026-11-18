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
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/switchboard/pkg/chat"
	"github.com/go-steer/switchboard/pkg/daemon"
)

// TestRouterNarrationAndToolCallInSameFrame verifies that an agent frame
// carrying both assistant narration text and a functionCall delivers the
// narration AND still surfaces the tool activity in stream and status modes,
// rather than returning early after deliverText and dropping the tool call.
func TestRouterNarrationAndToolCallInSameFrame(t *testing.T) {
	const narratedCall = `{"seq":1,"event":{"Content":{"parts":[` +
		`{"text":"Opening the PR now."},` +
		`{"functionCall":{"id":"c1","name":"propose-change","args":{"request":"bump billing memory"}}}` +
		`],"role":"model"},"Partial":false}}`
	const callResult = `{"seq":2,"event":{"Content":{"parts":[` +
		`{"functionResponse":{"id":"c1","name":"propose-change","response":{"result":"PR #8"}}}` +
		`],"role":"user"}}}`
	const finalAnswer = `{"seq":3,"event":{"Content":{"parts":[{"text":"Opened PR #8."}],"role":"model"},"Partial":false}}`

	t.Run("stream", func(t *testing.T) {
		release := make(chan struct{})
		router, fake := newEventRouter(t, ProgressStream, release, narratedCall, callResult, finalAnswer)
		router.reanchorDelay = 0
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		if err := router.Handle(ctx, chat.Message{
			Conversation: "C1:1", Channel: "C1", Caller: "u@example.com", Text: "fix it",
		}); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		first := recvReply(t, fake.replies)
		if first.Kind != chat.KindProgress {
			t.Fatalf("first reply kind = %q, want progress", first.Kind)
		}
		close(release)

		var sawNarration, sawActivity, sawAnswer bool
		deadline := time.After(2 * time.Second)
		for !sawNarration || !sawActivity || !sawAnswer {
			select {
			case r := <-fake.replies:
				switch {
				case r.Kind == "" && r.Text == "Opening the PR now.":
					sawNarration = true
				case r.Kind == chat.KindActivity && strings.Contains(r.Text, "propose-change"):
					sawActivity = true
				case r.Kind == "" && r.Text == "Opened PR #8.":
					sawAnswer = true
				}
			case <-deadline:
				t.Fatalf("timed out waiting for replies: sawNarration=%v sawActivity=%v sawAnswer=%v",
					sawNarration, sawActivity, sawAnswer)
			}
		}
		waitFor(t, func() bool {
			for _, u := range fake.updatedCalls() {
				if strings.Contains(u.text, markOK+" Ran **propose-change**") {
					return true
				}
			}
			return false
		}, "parent propose-change notice ticked off")
	})

	t.Run("status", func(t *testing.T) {
		release := make(chan struct{})
		router, fake := newEventRouter(t, ProgressStatus, release, narratedCall, callResult, finalAnswer)
		router.reanchorDelay = 10 * time.Second // same-frame tool call re-anchors immediately without waiting
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		if err := router.Handle(ctx, chat.Message{
			Conversation: "C1:2", Channel: "C1", Caller: "u@example.com", Text: "fix it",
		}); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		first := recvReply(t, fake.replies)
		if first.Kind != chat.KindProgress {
			t.Fatalf("first reply kind = %q, want progress", first.Kind)
		}
		close(release)

		waitFor(t, func() bool {
			for _, u := range fake.updatedCalls() {
				if strings.Contains(u.text, "running `propose-change` (step 1)") {
					return true
				}
			}
			return false
		}, "status placeholder updated with propose-change step")

		var sawNarration, sawAnswer bool
		deadline := time.After(2 * time.Second)
		for !sawNarration || !sawAnswer {
			select {
			case r := <-fake.replies:
				switch {
				case r.Kind == "" && r.Text == "Opening the PR now.":
					sawNarration = true
				case r.Kind == "" && r.Text == "Opened PR #8.":
					sawAnswer = true
				}
			case <-deadline:
				t.Fatalf("timed out waiting for replies: sawNarration=%v sawAnswer=%v", sawNarration, sawAnswer)
			}
		}
	})
}

type subagentRig struct {
	sseCh   chan sseItem
	mu      sync.Mutex
	known   map[string]bool
	subLogs map[string][]rawSubEvent
	subCode map[string]int
	hits    map[string]int
	sinces  map[string][]int64
}

type sseItem struct {
	typ  string
	data string
}

type rawSubEvent struct {
	seq int64
	raw string
}

func newSubagentRig(t *testing.T, mode ProgressMode) (*Router, *fakeSender, *subagentRig) {
	t.Helper()
	rig := &subagentRig{
		sseCh:   make(chan sseItem, 64),
		known:   make(map[string]bool),
		subLogs: make(map[string][]rawSubEvent),
		subCode: make(map[string]int),
		hits:    make(map[string]int),
		sinces:  make(map[string][]int64),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"app":"core-agent","sessionID":"s1"}`)
	})
	mux.HandleFunc("POST /sessions/{app}/{sid}/inject", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{}`)
	})
	mux.HandleFunc("GET /sessions/{app}/{sid}/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", daemon.EventCapabilities, `{"events":["turn-complete","turn-error"]}`)
		f.Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case item := <-rig.sseCh:
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", item.typ, item.data)
				f.Flush()
			}
		}
	})
	mux.HandleFunc("GET /sessions/{app}/{sid}/agents/{name}/events", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)

		rig.mu.Lock()
		rig.hits[name]++
		rig.sinces[name] = append(rig.sinces[name], since)
		code := rig.subCode[name]
		known := rig.known[name] || len(rig.subLogs[name]) > 0
		all := slices.Clone(rig.subLogs[name])
		rig.mu.Unlock()

		if code != 0 {
			http.Error(w, "subagent error", code)
			return
		}
		if !known {
			http.Error(w, "not a subagent", http.StatusNotFound)
			return
		}
		var matching []json.RawMessage
		nextSince := since
		for _, ev := range all {
			if ev.seq > since {
				matching = append(matching, json.RawMessage(ev.raw))
				if ev.seq > nextSince {
					nextSince = ev.seq
				}
			}
		}
		if matching == nil {
			matching = []json.RawMessage{}
		}
		w.Header().Set("Content-Type", "application/json")
		resp := struct {
			Agent     string            `json:"agent"`
			Events    []json.RawMessage `json:"events"`
			NextSince int64             `json:"next_since"`
			Truncated bool              `json:"truncated"`
		}{
			Agent:     name,
			Events:    matching,
			NextSince: nextSince,
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	dc, err := daemon.New(daemon.Config{BaseURL: srv.URL, BearerToken: "tok", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	fake := &fakeSender{replies: make(chan chat.Reply, 128)}
	router := NewRouter(dc, fake, mode, nil, nil)
	router.subPollInterval = 15 * time.Millisecond
	router.reanchorDelay = 0
	return router, fake, rig
}

func (rig *subagentRig) registerSubagent(name string) {
	rig.mu.Lock()
	defer rig.mu.Unlock()
	rig.known[name] = true
}

func (rig *subagentRig) addSubEvent(name string, seq int64, raw string) {
	rig.mu.Lock()
	defer rig.mu.Unlock()
	rig.known[name] = true
	rig.subLogs[name] = append(rig.subLogs[name], rawSubEvent{seq: seq, raw: raw})
}

func (rig *subagentRig) setSubStatus(name string, code int) {
	rig.mu.Lock()
	defer rig.mu.Unlock()
	rig.subCode[name] = code
}

func (rig *subagentRig) hitCount(name string) int {
	rig.mu.Lock()
	defer rig.mu.Unlock()
	return rig.hits[name]
}

func (rig *subagentRig) sinceHistory(name string) []int64 {
	rig.mu.Lock()
	defer rig.mu.Unlock()
	return append([]int64(nil), rig.sinces[name]...)
}

func (rig *subagentRig) sendAgent(data string) {
	rig.sseCh <- sseItem{typ: daemon.EventAgent, data: data}
}

func (rig *subagentRig) sendLifecycle(typ, data string) {
	rig.sseCh <- sseItem{typ: typ, data: data}
}

// TestRouterSubagentProgressStatus verifies status-mode subagent progress:
//  1. Ordinary tools answer 404 once and are cached in notSubagent so later
//     calls to them never poll.
//  2. Sync specialist calls (propose-change) edit the single placeholder in
//     place with `propose-change` › `<inner_tool>` (call N) and clear on result.
//  3. spawn_agent calls resolve their subagent branch from args and render
//     `<subagent>` › `<inner_tool>` (call N).
func TestRouterSubagentProgressStatus(t *testing.T) {
	router, fake, rig := newSubagentRig(t, ProgressStatus)
	router.tickInterval = 30 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := router.Handle(ctx, chat.Message{
		Conversation: "C1:10", Channel: "C1", Caller: "u@example.com", Text: "open PR",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if first := recvReply(t, fake.replies); first.Kind != chat.KindProgress {
		t.Fatalf("first reply = %+v, want progress placeholder", first)
	}

	// 1. Ordinary tool `bash` answers 404 once, then a second `bash` call makes zero additional polls.
	rig.sendAgent(`{"seq":1,"event":{"Content":{"parts":[{"functionCall":{"id":"b1","name":"bash","args":{"command":"ls"}}}],"role":"model"},"Partial":false}}`)
	rig.sendAgent(`{"seq":2,"event":{"Content":{"parts":[{"functionResponse":{"id":"b1","name":"bash","response":{"exit_code":0}}}],"role":"user"}}}`)
	waitFor(t, func() bool { return rig.hitCount("bash") == 1 }, "first bash 404 probe")

	rig.sendAgent(`{"seq":3,"event":{"Content":{"parts":[{"functionCall":{"id":"b2","name":"bash","args":{"command":"pwd"}}}],"role":"model"},"Partial":false}}`)
	rig.sendAgent(`{"seq":4,"event":{"Content":{"parts":[{"functionResponse":{"id":"b2","name":"bash","response":{"exit_code":0}}}],"role":"user"}}}`)
	time.Sleep(40 * time.Millisecond)
	if got := rig.hitCount("bash"); got != 1 {
		t.Fatalf("bash subagent polls = %d, want 1 (cached after first 404)", got)
	}

	// 2. Sync specialist `propose-change` starts at seq 5; inner calls land at seq 6 and seq 8.
	rig.addSubEvent("propose-change", 6, `{"seq":6,"event":{"Content":{"parts":[{"functionCall":{"id":"ic1","name":"vcs_clone","args":{"repo":"go-steer/demo"}}}],"role":"model"},"Partial":false}}`)
	rig.sendAgent(`{"seq":5,"event":{"Content":{"parts":[{"functionCall":{"id":"pc1","name":"propose-change","args":{"request":"fix oom"}}}],"role":"model"},"Partial":false}}`)

	waitFor(t, func() bool {
		for _, u := range fake.updatedCalls() {
			if strings.Contains(u.text, "running `propose-change` › `vcs_clone` (call 1)") {
				return true
			}
		}
		return false
	}, "status placeholder showing propose-change › vcs_clone (call 1)")

	rig.addSubEvent("propose-change", 7, `{"seq":7,"event":{"Content":{"parts":[{"functionResponse":{"id":"ic1","name":"vcs_clone","response":{"ok":true}}}],"role":"user"}}}`)
	rig.addSubEvent("propose-change", 8, `{"seq":8,"event":{"Content":{"parts":[{"functionCall":{"id":"ic2","name":"vcs_pr_create","args":{"title":"fix: bump memory"}}}],"role":"model"},"Partial":false}}`)

	waitFor(t, func() bool {
		for _, u := range fake.updatedCalls() {
			if strings.Contains(u.text, "running `propose-change` › `vcs_pr_create` (call 2)") {
				return true
			}
		}
		return false
	}, "status placeholder showing propose-change › vcs_pr_create (call 2)")

	// Parent ToolResult arrives at seq 9: inner subagent trail clears, so the next tick falls back to the outer step.
	rig.sendAgent(`{"seq":9,"event":{"Content":{"parts":[{"functionResponse":{"id":"pc1","name":"propose-change","response":{"pr":8}}}],"role":"user"}}}`)
	updatesBefore := len(fake.updatedCalls())
	waitFor(t, func() bool {
		calls := fake.updatedCalls()
		for _, u := range calls[updatesBefore:] {
			if strings.Contains(u.text, "running `propose-change` (step 3)") && !strings.Contains(u.text, "›") {
				return true
			}
		}
		return false
	}, "ticker clearing subagent inner tool after parent ToolResult")

	// 3. spawn_agent resolves target subagent name ("cluster") from args.
	rig.addSubEvent("cluster", 11, `{"seq":11,"event":{"Content":{"parts":[{"functionCall":{"id":"sc1","name":"get_k8s_events","args":{"namespace":"prod"}}}],"role":"model"},"Partial":false}}`)
	rig.sendAgent(`{"seq":10,"event":{"Content":{"parts":[{"functionCall":{"id":"sp1","name":"spawn_agent","args":{"agent":"cluster","goal":"check events","wait":true}}}],"role":"model"},"Partial":false}}`)

	waitFor(t, func() bool {
		for _, u := range fake.updatedCalls() {
			if strings.Contains(u.text, "running `cluster` › `get_k8s_events` (call 1)") {
				return true
			}
		}
		return false
	}, "status placeholder showing spawned subagent cluster › get_k8s_events (call 1)")

	rig.sendAgent(`{"seq":12,"event":{"Content":{"parts":[{"functionResponse":{"id":"sp1","name":"spawn_agent","response":{"status":"completed"}}}],"role":"user"}}}`)
	rig.sendAgent(`{"seq":13,"event":{"Content":{"parts":[{"text":"PR #8 is open and cluster events are clean."}],"role":"model"},"Partial":false}}`)
	rig.sendLifecycle(daemon.EventTurnComplete, `{"model":"gemini","latency_ms":1200}`)

	waitFor(t, func() bool {
		select {
		case r := <-fake.replies:
			return r.Kind == "" && strings.Contains(r.Text, "PR #8 is open")
		default:
			return false
		}
	}, "final answer delivered")
}

// TestRouterSubagentProgressStream verifies stream-mode subagent progress:
// - live running notice edited in place on inner result with timestamp duration,
// - fast call+result in the same poll batch coalesced into one settled Send (0 Updates),
// - multi-call parallel frame rendered with (**subagent**) attribution,
// - synthetic driver tools (return_result / finish_task) suppressed,
// - final drain on parent ToolResult settles inner notices before parent notice,
// - second invocation of the same subagent advances ?since= without replay.
func TestRouterSubagentProgressStream(t *testing.T) {
	router, fake, rig := newSubagentRig(t, ProgressStream)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := router.Handle(ctx, chat.Message{
		Conversation: "C2:1", Channel: "C2", Caller: "u@example.com", Text: "investigate pods",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if first := recvReply(t, fake.replies); first.Kind != chat.KindProgress {
		t.Fatalf("first reply = %+v, want progress placeholder", first)
	}

	// Batch 1: in-flight get_k8s_pods (no result yet).
	rig.addSubEvent("_fallback", 2, `{
		"seq": 2,
		"event": {
			"Content": {"parts": [{"functionCall": {"id": "f1", "name": "get_k8s_pods", "args": {"namespace": "billing"}}}], "role": "model"},
			"Partial": false,
			"Timestamp": "2026-11-18T19:00:00Z"
		}
	}`)
	rig.sendAgent(`{"seq":1,"event":{"Content":{"parts":[{"functionCall":{"id":"p1","name":"_fallback","args":{"request":"check billing"}}}],"role":"model"},"Partial":false}}`)

	// Expect outer Running **fallback** and inner Running **fallback** › **get_k8s_pods**.
	var sawOuterRun, sawInnerRun bool
	deadline := time.After(2 * time.Second)
	for !sawOuterRun || !sawInnerRun {
		select {
		case r := <-fake.replies:
			if r.Kind != chat.KindActivity {
				continue
			}
			if strings.Contains(r.Text, markRunning+" Running **fallback**\n") {
				sawOuterRun = true
			}
			if strings.Contains(r.Text, markRunning+" Running **fallback** › **get_k8s_pods**") && strings.Contains(r.Text, "billing") {
				sawInnerRun = true
			}
		case <-deadline:
			t.Fatalf("timed out waiting for initial stream notices: sawOuterRun=%v sawInnerRun=%v", sawOuterRun, sawInnerRun)
		}
	}

	// Batch 2: result for get_k8s_pods (2.4s later), a fast coalesced read_file
	// call+result pair, a 2-tool parallel frame, and a synthetic return_result.
	rig.mu.Lock()
	rig.subLogs["_fallback"] = append(rig.subLogs["_fallback"],
		rawSubEvent{seq: 3, raw: `{"seq":3,"event":{"Content":{"parts":[{"functionResponse":{"id":"f1","name":"get_k8s_pods","response":{"pods":3}}}],"role":"user"},"Timestamp":"2026-11-18T19:00:02.4Z"}}`},
		rawSubEvent{seq: 4, raw: `{"seq":4,"event":{"Content":{"parts":[{"functionCall":{"id":"f2","name":"read_file","args":{"path":"deploy/billing.yaml"}}}],"role":"model"},"Partial":false,"Timestamp":"2026-11-18T19:00:03Z"}}`},
		rawSubEvent{seq: 5, raw: `{"seq":5,"event":{"Content":{"parts":[{"functionResponse":{"id":"f2","name":"read_file","response":{"content":"ok"}}}],"role":"user"},"Timestamp":"2026-11-18T19:00:03.05Z"}}`},
		rawSubEvent{seq: 6, raw: `{"seq":6,"event":{"Content":{"parts":[` +
			`{"functionCall":{"id":"f3","name":"grep","args":{"pattern":"memory"}}},` +
			`{"functionCall":{"id":"f4","name":"get_k8s_logs","args":{"pod":"billing-0"}}}` +
			`],"role":"model"},"Partial":false,"Timestamp":"2026-11-18T19:00:04Z"}}`},
		rawSubEvent{seq: 7, raw: `{"seq":7,"event":{"Content":{"parts":[{"functionCall":{"id":"syn1","name":"return_result","args":{"result":"root cause found"}}}],"role":"model"},"Partial":false,"Timestamp":"2026-11-18T19:00:05Z"}}`},
	)
	rig.mu.Unlock()

	var sawCoalescedRead, sawParallel bool
	deadline = time.After(2 * time.Second)
	for !sawCoalescedRead || !sawParallel {
		select {
		case r := <-fake.replies:
			if strings.Contains(r.Text, "return_result") {
				t.Fatalf("synthetic driver tool leaked into thread: %q", r.Text)
			}
			if r.Kind == chat.KindActivity && strings.Contains(r.Text, markOK+" Ran **fallback** › **read_file**") {
				sawCoalescedRead = true
			}
			if r.Kind == chat.KindActivity && strings.Contains(r.Text, "Running 2 tools (**fallback**)") {
				sawParallel = true
			}
		case <-deadline:
			t.Fatalf("timed out waiting for batch 2 notices: sawCoalescedRead=%v sawParallel=%v", sawCoalescedRead, sawParallel)
		}
	}

	// get_k8s_pods was edited in place with 2.4s duration, while read_file needed zero Updates.
	waitFor(t, func() bool {
		for _, u := range fake.updatedCalls() {
			if strings.Contains(u.text, markOK+" Ran **fallback** › **get_k8s_pods**") && strings.Contains(u.text, "⏱ 2.4s") {
				return true
			}
		}
		return false
	}, "in-place edit of get_k8s_pods with 2.4s duration")
	for _, u := range fake.updatedCalls() {
		if strings.Contains(u.text, "read_file") {
			t.Fatalf("coalesced read_file should not emit an Update: %+v", u)
		}
	}

	// Add final inner results at seq 8 right before parent ToolResult at seq 9:
	// finalDrain must settle the parallel notice BEFORE parent _fallback ticks off!
	rig.addSubEvent("_fallback", 8, `{"seq":8,"event":{"Content":{"parts":[`+
		`{"functionResponse":{"id":"f3","name":"grep","response":{"matches":1}}},`+
		`{"functionResponse":{"id":"f4","name":"get_k8s_logs","response":{"lines":20}}}`+
		`],"role":"user"},"Timestamp":"2026-11-18T19:00:05Z"}}`)
	rig.sendAgent(`{"seq":9,"event":{"Content":{"parts":[{"functionResponse":{"id":"p1","name":"_fallback","response":{"output":"done"}}}],"role":"user"}}}`)

	waitFor(t, func() bool {
		var innerIdx, outerIdx = -1, -1
		for i, u := range fake.updatedCalls() {
			if strings.Contains(u.text, markOK+" Ran 2 tools (**fallback**)") {
				innerIdx = i
			}
			if strings.HasPrefix(u.text, markOK+" Ran **fallback**\n") {
				outerIdx = i
			}
		}
		return innerIdx >= 0 && outerIdx > innerIdx
	}, "inner parallel notice settled ahead of outer _fallback notice")

	// Second _fallback call in the same turn (seq 20): ?since= must start >= 20 and never replay seq 2..8.
	rig.addSubEvent("_fallback", 21, `{"seq":21,"event":{"Content":{"parts":[{"functionCall":{"id":"f5","name":"get_k8s_events","args":{"namespace":"billing"}}}],"role":"model"},"Partial":false}}`)
	rig.addSubEvent("_fallback", 22, `{"seq":22,"event":{"Content":{"parts":[{"functionResponse":{"id":"f5","name":"get_k8s_events","response":{"ok":true}}}],"role":"user"}}}`)
	rig.sendAgent(`{"seq":20,"event":{"Content":{"parts":[{"functionCall":{"id":"p2","name":"_fallback","args":{"request":"double check events"}}}],"role":"model"},"Partial":false}}`)
	rig.sendAgent(`{"seq":23,"event":{"Content":{"parts":[{"functionResponse":{"id":"p2","name":"_fallback","response":{"output":"confirmed"}}}],"role":"user"}}}`)
	rig.sendAgent(`{"seq":24,"event":{"Content":{"parts":[{"text":"Billing pod OOM confirmed."}],"role":"model"},"Partial":false}}`)
	rig.sendLifecycle(daemon.EventTurnComplete, `{"model":"gemini","latency_ms":2000}`)

	var sawSecondCall, sawFinal bool
	deadline = time.After(2 * time.Second)
	for !sawSecondCall || !sawFinal {
		select {
		case r := <-fake.replies:
			if strings.Contains(r.Text, "get_k8s_pods") || strings.Contains(r.Text, "read_file") {
				t.Fatalf("second _fallback replayed first run's notice: %q", r.Text)
			}
			if r.Kind == chat.KindActivity && strings.Contains(r.Text, "**fallback** › **get_k8s_events**") {
				sawSecondCall = true
			}
			if r.Kind == "" && r.Text == "Billing pod OOM confirmed." {
				sawFinal = true
			}
		case <-deadline:
			t.Fatalf("timed out on second _fallback run: sawSecondCall=%v sawFinal=%v", sawSecondCall, sawFinal)
		}
	}
	sinces := rig.sinceHistory("_fallback")
	if len(sinces) == 0 || sinces[len(sinces)-1] < 20 {
		t.Fatalf("_fallback since history = %v, want final poll since >= 20", sinces)
	}
}

// TestRouterSubagentNoticeCapPreservesPendingParentNotice verifies that a
// chatty subagent posting more than noticeMemory (32) inner tool notices that
// settle across poll ticks evicts settled inner notices rather than the
// parent's still-pending delegation notice.
func TestRouterSubagentNoticeCapPreservesPendingParentNotice(t *testing.T) {
	router, fake, rig := newSubagentRig(t, ProgressStream)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := router.Handle(ctx, chat.Message{
		Conversation: "C3:1", Channel: "C3", Caller: "u@example.com", Text: "big investigation",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	_ = recvReply(t, fake.replies) // initial placeholder

	rig.registerSubagent("propose-change")
	rig.sendAgent(`{"seq":1,"event":{"Content":{"parts":[{"functionCall":{"id":"parent-1","name":"propose-change","args":{"request":"refactor 40 files"}}}],"role":"model"},"Partial":false}}`)
	waitFor(t, func() bool {
		select {
		case r := <-fake.replies:
			return r.Kind == chat.KindActivity && strings.Contains(r.Text, "Running **propose-change**")
		default:
			return false
		}
	}, "parent propose-change notice posted")

	// Stream 36 two-phase inner calls (each first seen as pending so it enters
	// e.notices, then settled on the next batch) — well past noticeMemory (32).
	const totalInner = 36
	seq := int64(2)
	for i := 1; i <= totalInner; i++ {
		callSeq := seq
		seq++
		rig.addSubEvent("propose-change", callSeq, fmt.Sprintf(
			`{"seq":%d,"event":{"Content":{"parts":[{"functionCall":{"id":"in-%d","name":"edit_file","args":{"path":"f%d.go"}}}],"role":"model"},"Partial":false}}`,
			callSeq, i, i,
		))
		waitFor(t, func() bool {
			for {
				select {
				case r := <-fake.replies:
					if strings.Contains(r.Text, fmt.Sprintf("f%d.go", i)) {
						return true
					}
				default:
					return false
				}
			}
		}, fmt.Sprintf("inner call %d posted", i))

		resSeq := seq
		seq++
		rig.addSubEvent("propose-change", resSeq, fmt.Sprintf(
			`{"seq":%d,"event":{"Content":{"parts":[{"functionResponse":{"id":"in-%d","name":"edit_file","response":{"ok":true}}}],"role":"user"}}}`,
			resSeq, i,
		))
		want := fmt.Sprintf("f%d.go", i)
		waitFor(t, func() bool {
			for _, u := range fake.updatedCalls() {
				if strings.Contains(u.text, markOK+" Ran **propose-change** › **edit_file**") && strings.Contains(u.text, want) {
					return true
				}
			}
			return false
		}, fmt.Sprintf("inner call %d settled", i))
	}

	// Now finish the parent propose-change call: its notice must STILL be in e.notices and tick off!
	rig.sendAgent(fmt.Sprintf(`{"seq":%d,"event":{"Content":{"parts":[{"functionResponse":{"id":"parent-1","name":"propose-change","response":{"pr":9}}}],"role":"user"}}}`, seq))
	waitFor(t, func() bool {
		for _, u := range fake.updatedCalls() {
			if strings.HasPrefix(u.text, markOK+" Ran **propose-change**\n") {
				return true
			}
		}
		return false
	}, "parent propose-change notice still ticked off after 36 inner notices")
}

// TestRouterSubagentPollingLifecycleAndUnsupported verifies that:
// - ProgressOff and ProgressIndicator make zero subagent HTTP requests,
// - 501 / 412 (ErrNotSupported) disables subagent polling for the session,
// - turn-error cancels an in-flight subagent poller immediately.
func TestRouterSubagentPollingLifecycleAndUnsupported(t *testing.T) {
	t.Run("off_and_indicator_never_poll", func(t *testing.T) {
		for _, mode := range []ProgressMode{ProgressOff, ProgressIndicator} {
			router, fake, rig := newSubagentRig(t, mode)
			rig.addSubEvent("_fallback", 2, `{"seq":2,"event":{"Content":{"parts":[{"functionCall":{"id":"x","name":"bash","args":{"command":"ls"}}}],"role":"model"},"Partial":false}}`)
			ctx, cancel := context.WithCancel(context.Background())

			if err := router.Handle(ctx, chat.Message{
				Conversation: "C4:1", Channel: "C4", Caller: "u@example.com", Text: "hi",
			}); err != nil {
				t.Fatalf("Handle(%s): %v", mode, err)
			}
			rig.sendAgent(`{"seq":1,"event":{"Content":{"parts":[{"functionCall":{"id":"p1","name":"_fallback","args":{"request":"x"}}}],"role":"model"},"Partial":false}}`)
			time.Sleep(50 * time.Millisecond)
			rig.sendAgent(`{"seq":3,"event":{"Content":{"parts":[{"functionResponse":{"id":"p1","name":"_fallback","response":{"ok":true}}}],"role":"user"}}}`)
			rig.sendAgent(`{"seq":4,"event":{"Content":{"parts":[{"text":"done"}],"role":"model"},"Partial":false}}`)
			rig.sendLifecycle(daemon.EventTurnComplete, `{"model":"gemini","latency_ms":100}`)

			waitFor(t, func() bool {
				for {
					select {
					case r := <-fake.replies:
						if r.Kind == "" && r.Text == "done" {
							return true
						}
					default:
						return false
					}
				}
			}, "final answer")
			if got := rig.hitCount("_fallback"); got != 0 {
				t.Fatalf("mode %s polled subagent %d times, want 0", mode, got)
			}
			cancel()
		}
	})

	t.Run("unsupported_daemon_disables_session_polling", func(t *testing.T) {
		router, fake, rig := newSubagentRig(t, ProgressStatus)
		rig.setSubStatus("_fallback", http.StatusNotImplemented)
		rig.setSubStatus("propose-change", http.StatusNotImplemented)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		if err := router.Handle(ctx, chat.Message{
			Conversation: "C5:1", Channel: "C5", Caller: "u@example.com", Text: "hi",
		}); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		_ = recvReply(t, fake.replies)

		rig.sendAgent(`{"seq":1,"event":{"Content":{"parts":[{"functionCall":{"id":"p1","name":"_fallback","args":{"request":"x"}}}],"role":"model"},"Partial":false}}`)
		waitFor(t, func() bool { return rig.hitCount("_fallback") == 1 }, "first 501 probe")
		rig.sendAgent(`{"seq":2,"event":{"Content":{"parts":[{"functionResponse":{"id":"p1","name":"_fallback","response":{"ok":true}}}],"role":"user"}}}`)

		// A later call to a DIFFERENT subagent on the same session must not probe at all.
		rig.sendAgent(`{"seq":3,"event":{"Content":{"parts":[{"functionCall":{"id":"p2","name":"propose-change","args":{"request":"y"}}}],"role":"model"},"Partial":false}}`)
		rig.sendAgent(`{"seq":4,"event":{"Content":{"parts":[{"functionResponse":{"id":"p2","name":"propose-change","response":{"ok":true}}}],"role":"user"}}}`)
		time.Sleep(45 * time.Millisecond)
		if got := rig.hitCount("propose-change"); got != 0 {
			t.Fatalf("propose-change hits after 501 = %d, want 0", got)
		}
	})

	t.Run("turn_error_stops_active_poller", func(t *testing.T) {
		router, fake, rig := newSubagentRig(t, ProgressStatus)
		rig.addSubEvent("_fallback", 2, `{"seq":2,"event":{"Content":{"parts":[{"functionCall":{"id":"x","name":"get_k8s_pods","args":{"namespace":"default"}}}],"role":"model"},"Partial":false}}`)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		if err := router.Handle(ctx, chat.Message{
			Conversation: "C6:1", Channel: "C6", Caller: "u@example.com", Text: "hi",
		}); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		_ = recvReply(t, fake.replies)

		rig.sendAgent(`{"seq":1,"event":{"Content":{"parts":[{"functionCall":{"id":"p1","name":"_fallback","args":{"request":"x"}}}],"role":"model"},"Partial":false}}`)
		waitFor(t, func() bool { return rig.hitCount("_fallback") >= 2 }, "active subagent polling")

		// Emit turn-error without a parent ToolResult: poller must halt promptly.
		rig.sendLifecycle(daemon.EventTurnError, `{"kind":"provider","code":"rate_limit","retryable":true,"message":"429"}`)
		waitFor(t, func() bool {
			for {
				select {
				case r := <-fake.replies:
					if r.Kind == chat.KindNotice {
						return true
					}
				default:
					return false
				}
			}
		}, "turn error notice")

		afterErr := rig.hitCount("_fallback")
		time.Sleep(60 * time.Millisecond)
		if got := rig.hitCount("_fallback"); got > afterErr+1 {
			t.Fatalf("poller kept running after turn-error: hits went from %d to %d", afterErr, got)
		}
	})

	t.Run("first_poll_404_before_branch_commit_recovers", func(t *testing.T) {
		router, fake, rig := newSubagentRig(t, ProgressStatus)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		if err := router.Handle(ctx, chat.Message{
			Conversation: "C7:1", Channel: "C7", Caller: "u@example.com", Text: "hi",
		}); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		_ = recvReply(t, fake.replies)

		// Do not register propose-change yet: first background poll gets 404 while
		// the sync subagent runner is still committing its opening branch row.
		rig.sendAgent(`{"seq":1,"event":{"Content":{"parts":[{"functionCall":{"id":"p1","name":"propose-change","args":{"request":"x"}}}],"role":"model"},"Partial":false}}`)
		waitFor(t, func() bool { return rig.hitCount("propose-change") >= 2 }, "multiple 404 backoff misses without cache poisoning")

		// Branch commits after multiple 404 polls; next poll must succeed rather than have cached 404.
		rig.addSubEvent("propose-change", 2, `{"seq":2,"event":{"Content":{"parts":[{"functionCall":{"id":"ic1","name":"vcs_clone","args":{"repo":"go-steer/demo"}}}],"role":"model"},"Partial":false}}`)
		waitFor(t, func() bool {
			for _, u := range fake.updatedCalls() {
				if strings.Contains(u.text, "running `propose-change` › `vcs_clone` (call 1)") {
					return true
				}
			}
			return false
		}, "status placeholder recovered after multiple transient 404s")
	})

	t.Run("failed_prelaunch_and_spawn_agent_do_not_poison_cache", func(t *testing.T) {
		router, fake, rig := newSubagentRig(t, ProgressStream)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		if err := router.Handle(ctx, chat.Message{
			Conversation: "C8:1", Channel: "C8", Caller: "u@example.com", Text: "hi",
		}); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		_ = recvReply(t, fake.replies)

		// 1. spawn_remote_agent is never polled.
		rig.sendAgent(`{"seq":1,"event":{"Content":{"parts":[{"functionCall":{"id":"r1","name":"spawn_remote_agent","args":{"agent":"remote"}}}],"role":"model"},"Partial":false}}`)
		rig.sendAgent(`{"seq":2,"event":{"Content":{"parts":[{"functionResponse":{"id":"r1","name":"spawn_remote_agent","response":{"ok":true}}}],"role":"user"}}}`)

		// 2. spawn_agent with explicit name fails pre-launch (plan_mode / policy) while returning 404.
		rig.sendAgent(`{"seq":3,"event":{"Content":{"parts":[{"functionCall":{"id":"sp1","name":"spawn_agent","args":{"name":"cluster-check","agent":"triage","wait":true}}}],"role":"model"},"Partial":false}}`)
		rig.sendAgent(`{"seq":4,"event":{"Content":{"parts":[{"functionResponse":{"id":"sp1","name":"spawn_agent","response":{"error":"plan required"}}}],"role":"user"}}}`)

		// 3. Uncatalogued propose-change fails pre-branch (denied) after live 404 probes.
		rig.sendAgent(`{"seq":5,"event":{"Content":{"parts":[{"functionCall":{"id":"pc1","name":"propose-change","args":{"request":"fix"}}}],"role":"model"},"Partial":false}}`)
		waitFor(t, func() bool { return rig.hitCount("propose-change") >= 2 }, "live 404 probes while parked on gate")
		rig.sendAgent(`{"seq":6,"event":{"Content":{"parts":[{"functionResponse":{"id":"pc1","name":"propose-change","response":{"error":"permission denied"}}}],"role":"user"}}}`)

		// 4. Ordinary tool kubectl_get succeeds (!res.Failed) with 404, so it IS cached on drain.
		rig.sendAgent(`{"seq":7,"event":{"Content":{"parts":[{"functionCall":{"id":"k1","name":"kubectl_get","args":{"resource":"pods"}}}],"role":"model"},"Partial":false}}`)
		rig.sendAgent(`{"seq":8,"event":{"Content":{"parts":[{"functionResponse":{"id":"k1","name":"kubectl_get","response":{"items":[]}}}],"role":"user"}}}`)
		waitFor(t, func() bool { return rig.hitCount("kubectl_get") == 1 }, "kubectl_get drained once")

		// Second kubectl_get must make zero additional probes.
		rig.sendAgent(`{"seq":9,"event":{"Content":{"parts":[{"functionCall":{"id":"k2","name":"kubectl_get","args":{"resource":"svc"}}}],"role":"model"},"Partial":false}}`)
		rig.sendAgent(`{"seq":10,"event":{"Content":{"parts":[{"functionResponse":{"id":"k2","name":"kubectl_get","response":{"items":[]}}}],"role":"user"}}}`)

		// 5. Retry both cluster-check (spawn_agent) and propose-change: neither was poisoned!
		rig.addSubEvent("cluster-check", 12, `{"seq":12,"event":{"Content":{"parts":[{"functionCall":{"id":"cc1","name":"get_k8s_nodes","args":{}}}],"role":"model"},"Partial":false}}`)
		rig.sendAgent(`{"seq":11,"event":{"Content":{"parts":[{"functionCall":{"id":"sp2","name":"spawn_agent","args":{"name":"cluster-check","agent":"triage","wait":true}}}],"role":"model"},"Partial":false}}`)

		rig.addSubEvent("propose-change", 15, `{"seq":15,"event":{"Content":{"parts":[{"functionCall":{"id":"pc2-1","name":"vcs_branch","args":{"name":"fix/oom"}}}],"role":"model"},"Partial":false}}`)
		rig.sendAgent(`{"seq":14,"event":{"Content":{"parts":[{"functionCall":{"id":"pc2","name":"propose-change","args":{"request":"fix"}}}],"role":"model"},"Partial":false}}`)

		var sawClusterInner, sawProposeInner bool
		deadline := time.After(2 * time.Second)
		for !sawClusterInner || !sawProposeInner {
			select {
			case r := <-fake.replies:
				if r.Kind == chat.KindActivity && strings.Contains(r.Text, "**cluster-check** › **get_k8s_nodes**") {
					sawClusterInner = true
				}
				if r.Kind == chat.KindActivity && strings.Contains(r.Text, "**propose-change** › **vcs_branch**") {
					sawProposeInner = true
				}
			case <-deadline:
				t.Fatalf("timed out waiting for retried subagent notices: cluster=%v propose=%v", sawClusterInner, sawProposeInner)
			}
		}
		if got := rig.hitCount("spawn_remote_agent"); got != 0 {
			t.Fatalf("spawn_remote_agent hits = %d, want 0", got)
		}
		if got := rig.hitCount("kubectl_get"); got != 1 {
			t.Fatalf("kubectl_get hits = %d, want 1", got)
		}
	})

	t.Run("idle_status_snapshot_stops_orphaned_poller", func(t *testing.T) {
		router, fake, rig := newSubagentRig(t, ProgressStatus)
		rig.addSubEvent("_fallback", 2, `{"seq":2,"event":{"Content":{"parts":[{"functionCall":{"id":"x","name":"get_k8s_pods","args":{}}}],"role":"model"},"Partial":false}}`)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		if err := router.Handle(ctx, chat.Message{
			Conversation: "C9:1", Channel: "C9", Caller: "u@example.com", Text: "hi",
		}); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		_ = recvReply(t, fake.replies)

		rig.sendAgent(`{"seq":1,"event":{"Content":{"parts":[{"functionCall":{"id":"p1","name":"_fallback","args":{"request":"x"}}}],"role":"model"},"Partial":false}}`)
		waitFor(t, func() bool { return rig.hitCount("_fallback") >= 2 }, "poller active")

		// Daemon reports turn_state=idle without a prior streaming frame (the
		// snapshot shape emitted on SSE reconnect after a disconnected abort).
		rig.sendLifecycle(daemon.EventStatusUpdate, `{"turn_state":"idle"}`)
		time.Sleep(30 * time.Millisecond)
		afterIdle := rig.hitCount("_fallback")
		time.Sleep(60 * time.Millisecond)
		if got := rig.hitCount("_fallback"); got > afterIdle+1 {
			t.Fatalf("orphaned poller kept running after idle status snapshot: %d -> %d", afterIdle, got)
		}
	})

	t.Run("autonomous_turn_narration_plus_subagent_call", func(t *testing.T) {
		router, fake, rig := newSubagentRig(t, ProgressStream)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Complete Turn 1 so turnInFlight returns to false on the live session.
		if err := router.Handle(ctx, chat.Message{
			Conversation: "C10:1", Channel: "C10", Caller: "u@example.com", Text: "watch",
		}); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		_ = recvReply(t, fake.replies)
		rig.sendAgent(`{"seq":1,"event":{"Content":{"parts":[{"text":"Watching."}],"role":"model"},"Partial":false}}`)
		rig.sendLifecycle(daemon.EventTurnComplete, `{"model":"gemini","latency_ms":50}`)
		waitFor(t, func() bool {
			for {
				select {
				case r := <-fake.replies:
					if r.Kind == "" && r.Text == "Watching." {
						return true
					}
				default:
					return false
				}
			}
		}, "turn 1 answer")

		// Autonomous wake turn (turnInFlight == false): first start a sibling
		// subagent watch (_fallback), then emit a narrated tool-call frame for
		// propose-change; deliverText must NOT treat the narrated frame as
		// ended (which would stop _fallback's poller!).
		rig.addSubEvent("_fallback", 11, `{"seq":11,"event":{"Content":{"parts":[{"functionCall":{"id":"f1","name":"get_k8s_logs","args":{"pod":"api-0"}}}],"role":"model"},"Partial":false}}`)
		rig.sendAgent(`{"seq":10,"event":{"Content":{"parts":[{"functionCall":{"id":"p-fb","name":"_fallback","args":{"request":"logs"}}}],"role":"model"},"Partial":false}}`)

		rig.addSubEvent("propose-change", 13, `{"seq":13,"event":{"Content":{"parts":[{"functionCall":{"id":"pc1","name":"vcs_create_pr","args":{"title":"fix: bump limits"}}}],"role":"model"},"Partial":false}}`)
		rig.sendAgent(`{"seq":12,"event":{"Content":{"parts":[{"text":"Alert fired — opening fix PR."},{"functionCall":{"id":"p-pc","name":"propose-change","args":{"request":"open PR"}}}],"role":"model"},"Partial":false}}`)

		// Also add another _fallback event AFTER seq 12 to prove _fallback's poller wasn't killed by deliverText!
		rig.addSubEvent("_fallback", 14, `{"seq":14,"event":{"Content":{"parts":[{"functionCall":{"id":"f2","name":"get_k8s_events","args":{"namespace":"prod"}}}],"role":"model"},"Partial":false}}`)

		var sawNarration, sawFallbackAfter, sawProposeInner bool
		deadline := time.After(2 * time.Second)
		for !sawNarration || !sawFallbackAfter || !sawProposeInner {
			select {
			case r := <-fake.replies:
				switch {
				case r.Kind == "" && r.Text == "Alert fired — opening fix PR.":
					sawNarration = true
				case r.Kind == chat.KindActivity && strings.Contains(r.Text, "**fallback** › **get_k8s_events**"):
					sawFallbackAfter = true
				case r.Kind == chat.KindActivity && strings.Contains(r.Text, "**propose-change** › **vcs_create_pr**"):
					sawProposeInner = true
				}
			case <-deadline:
				t.Fatalf("timed out on autonomous narrated delegation: narration=%v fallbackAfter=%v proposeInner=%v",
					sawNarration, sawFallbackAfter, sawProposeInner)
			}
		}
	})
}
