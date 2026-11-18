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

// Package subagent is switchboard's client for core-agent's subagent turn-log
// route (core-agent v3.1.0+, go-steer/core-agent#1308):
//
//	GET /sessions/<app>/<sid>/agents/<name>/events?since=<seq>
//
// A parent session's SSE /events stream carries only the parent's own frames:
// when the parent delegates to a synchronous specialist (_fallback,
// propose-change) or spawns a background subagent (spawn_agent), the SSE
// stream sees one outer functionCall at the start and one functionResponse at
// the end, nothing between. Polling this route while that outer call is in
// flight lets switchboard surface the subagent's inner tool calls in status
// and stream modes (#193) without widening the SSE contract.
//
// Like pkg/approval, this client lives outside pkg/daemon on purpose. That
// package speaks only the four frozen verbs every daemon implements; this
// route is optional — it answers 501 (or a mux 404) on older daemons, 412 when
// the daemon runs without a durable event log, and 404 when the queried tool
// name is an ordinary tool rather than a subagent on the session.
package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-steer/switchboard/pkg/daemon"
)

// Client reads subagent turn logs from a core-agent daemon. Safe for
// concurrent use.
type Client struct {
	cfg  daemon.Config
	http *http.Client

	// pollDefault overrides eventsTimeout when non-zero; a test seam.
	pollDefault time.Duration
}

// eventsTimeout bounds one /agents/<name>/events poll when the caller supplies
// no tighter deadline.
const eventsTimeout = 15 * time.Second

// isSyntheticTool reports whether name is one of core-agent's loop-control
// pseudo-tools (pkg/agent.FinishTaskToolName and pkg/agent/autonomous
// driverTools). A subagent invokes finish_task or return_result (and its
// report_done / report_completed / mark_task_done aliases) with its full
// deliverable payload to hand control back to the parent, and schedule_next_turn
// to defer a standing loop. core-agent strips all of them from subagent call
// provenance (RunResult.Calls); surfacing them as tool notices would both
// double-announce completion and dump raw output prose as a tool argument.
func isSyntheticTool(name string) bool {
	switch name {
	case "finish_task", "return_result", "report_done", "report_completed", "mark_task_done", "schedule_next_turn":
		return true
	default:
		return false
	}
}

// ErrNotSubagent reports that the named tool has no subagent branch or roster
// entry on the session (404 or 400, or a name that cannot be a subagent branch
// at all). Callers treat the invocation as an ordinary tool call.
var ErrNotSubagent = errors.New("subagent: session has no subagent of that name")

// ErrNotSupported reports that the daemon does not serve subagent turn history
// for this session — either the route is unimplemented (501) or the daemon was
// started without an event log (412).
var ErrNotSupported = errors.New("subagent: session does not record subagent events")

// New returns a Client sharing the daemon's address, credential and transport
// with pkg/daemon.
func New(cfg daemon.Config) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	hc := cfg.HTTPClient
	switch {
	case hc == nil:
		hc = &http.Client{CheckRedirect: daemon.NoRedirects}
	case hc.Timeout != 0:
		clone := *hc
		clone.Timeout = 0
		hc = &clone
	}
	return &Client{cfg: cfg, http: hc}, nil
}

// ValidName reports whether name satisfies core-agent's subagent branch-name
// rules (internal/subagentlog.ValidateName): non-empty, no leading or trailing
// whitespace, no '.', '/' or space, and no ASCII control bytes. Checked before
// building a URL so a tool name like "mcp.read" or "../other" is rejected
// locally rather than reshaping the path.
func ValidName(name string) bool {
	if name == "" || strings.TrimSpace(name) != name || strings.ContainsAny(name, ". /") {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// Frame is one event from a subagent's turn log carrying tool calls, tool
// results, or both. Each element on the wire has the same {seq, event} shape
// as SSE EventAgent frames, so Calls and Results go through daemon.ToolCalls
// and daemon.ToolResults — inheriting the same argument summariser, credential
// redaction, and verdict rules.
type Frame struct {
	Seq     int64
	At      time.Time
	Calls   []daemon.ToolCall
	Results []daemon.ToolResult
}

// Page is one cursor-paginated slice of a subagent's events after since.
type Page struct {
	Events    []Frame
	NextSince int64
	Truncated bool
}

// Events fetches one page of tool activity for subagent name in sess with
// seq > since. Pass Page.NextSince back as since on the next poll; when
// Page.Truncated is true and Page.NextSince > since, more events are already
// waiting behind this page.
func (c *Client) Events(ctx context.Context, sess daemon.Session, assertedCaller, name string, since int64) (Page, error) {
	if sess.App == "" || sess.ID == "" {
		return Page{}, fmt.Errorf("subagent: session is incomplete (app=%q id=%q)", sess.App, sess.ID)
	}
	if !ValidName(name) {
		return Page{}, ErrNotSubagent
	}
	if since < 0 {
		since = 0
	}

	if _, ok := ctx.Deadline(); !ok {
		limit := c.pollDefault
		if limit == 0 {
			limit = eventsTimeout
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, limit)
		defer cancel()
	}

	q := url.Values{}
	q.Set("since", strconv.FormatInt(since, 10))
	rawURL := c.cfg.BaseURL + "/sessions/" + url.PathEscape(sess.App) + "/" + url.PathEscape(sess.ID) +
		"/agents/" + url.PathEscape(name) + "/events?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return Page{}, err
	}
	req.Header.Set("Accept", "application/json")
	if c.cfg.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.BearerToken)
	}
	if assertedCaller != "" && !c.cfg.OmitAssertedCaller {
		req.Header.Set("X-Asserted-Caller", assertedCaller)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return Page{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Page{}, c.statusErr(resp)
	}

	var wire eventsResponse
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return Page{}, fmt.Errorf("subagent: decode response: %w", err)
	}

	out := Page{
		NextSince: max(wire.NextSince, since),
		Truncated: wire.Truncated,
	}
	for _, raw := range wire.Events {
		f, maxSeq := parseFrame(raw)
		if maxSeq > out.NextSince {
			out.NextSince = maxSeq
		}
		if len(f.Calls) == 0 && len(f.Results) == 0 {
			continue
		}
		out.Events = append(out.Events, f)
	}
	return out, nil
}

type eventsResponse struct {
	Events    []json.RawMessage `json:"events"`
	NextSince int64             `json:"next_since"`
	Truncated bool              `json:"truncated"`
}

type frameMeta struct {
	Seq   int64 `json:"seq"`
	Event *struct {
		Timestamp time.Time `json:"Timestamp"`
	} `json:"event"`
}

func parseFrame(raw json.RawMessage) (Frame, int64) {
	var meta frameMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return Frame{}, 0
	}
	data := string(raw)
	calls := filterCalls(daemon.ToolCalls(data))
	results := filterResults(daemon.ToolResults(data))
	var at time.Time
	if meta.Event != nil {
		at = meta.Event.Timestamp
	}
	return Frame{
		Seq:     meta.Seq,
		At:      at,
		Calls:   calls,
		Results: results,
	}, meta.Seq
}

func filterCalls(in []daemon.ToolCall) []daemon.ToolCall {
	if len(in) == 0 {
		return nil
	}
	out := in[:0:len(in)]
	var copied bool
	for i, c := range in {
		if isSyntheticTool(c.Name) {
			if !copied {
				out = make([]daemon.ToolCall, 0, len(in)-1)
				out = append(out, in[:i]...)
				copied = true
			}
			continue
		}
		if copied {
			out = append(out, c)
		} else {
			out = in[:i+1]
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func filterResults(in []daemon.ToolResult) []daemon.ToolResult {
	if len(in) == 0 {
		return nil
	}
	out := in[:0:len(in)]
	var copied bool
	for i, r := range in {
		if isSyntheticTool(r.Name) {
			if !copied {
				out = make([]daemon.ToolResult, 0, len(in)-1)
				out = append(out, in[:i]...)
				copied = true
			}
			continue
		}
		if copied {
			out = append(out, r)
		} else {
			out = in[:i+1]
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (c *Client) statusErr(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(b))
	if msg == "" {
		msg = resp.Status
	}
	status := &daemon.StatusError{
		Method:     resp.Request.Method,
		Path:       resp.Request.URL.Path,
		StatusCode: resp.StatusCode,
		Message:    msg,
	}
	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusBadRequest:
		return &routeError{sentinel: ErrNotSubagent, status: status}
	case http.StatusPreconditionFailed, http.StatusNotImplemented:
		return &routeError{sentinel: ErrNotSupported, status: status}
	}
	return status
}

type routeError struct {
	sentinel error
	status   *daemon.StatusError
}

func (e *routeError) Error() string {
	return fmt.Sprintf("%s (%s %s: %s)", e.sentinel, e.status.Method, e.status.Path, e.status.Message)
}

func (e *routeError) Unwrap() []error { return []error{e.sentinel, e.status} }
