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

// Package daemon is switchboard's thin client for the frozen core-agent
// daemon contract. A chat gateway maps a conversation (a Slack thread, a
// Google Chat space) onto a core-agent session and shuttles turns across
// the four verbs core-agent already ships:
//
//	POST   /sessions                  -> create a session (returns SessionID)
//	POST   /sessions/<sid>/inject     -> queue a user message on its inbox
//	POST   /sessions/<sid>/wake       -> nudge a sleeping session to run a turn
//	GET    /sessions/<sid>/events     -> SSE stream of the session's output
//
// Auth is a static Bearer token; per-turn attribution rides the
// X-Asserted-Caller header (the daemon stamps it as the session Owner and
// resolves per-caller MCP credentials from it — the seam where W0 and W1
// meet). This mirrors k8s-lookout's pkg/inject wire client; switchboard
// adds the /wake and /events verbs the interactive round-trip needs.
package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Config captures the daemon-side surface switchboard posts against.
// Constructed from CLI flags / env in main.go.
type Config struct {
	// BaseURL is the scheme + host + port with NO trailing slash —
	// e.g. "http://127.0.0.1:7777".
	BaseURL string

	// BearerToken authenticates switchboard to the daemon. Loaded
	// from an env var by main.go (never a bare flag).
	BearerToken string

	// HTTPClient lets tests swap in a client pointed at an
	// httptest.Server. Nil in production.
	HTTPClient *http.Client
}

// Client is the thinnest wire client that covers the interactive
// chat round-trip. It is safe for concurrent use.
type Client struct {
	cfg  Config
	http *http.Client
}

// New validates the config and returns a Client.
func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("daemon: BaseURL is required")
	}
	if strings.HasSuffix(cfg.BaseURL, "/") {
		return nil, fmt.Errorf("daemon: BaseURL must not end with '/' (got %q)", cfg.BaseURL)
	}
	if cfg.BearerToken == "" {
		return nil, errors.New("daemon: BearerToken is required")
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{cfg: cfg, http: hc}, nil
}

// CreateSession opens a new session and returns its ID. assertedCaller
// (may be empty) is the identity the daemon stamps as Owner; switchboard
// must be listed in the daemon's attach.multi_session.proxy_identities
// for it to be honored.
func (c *Client) CreateSession(ctx context.Context, assertedCaller string) (string, error) {
	var out struct {
		SessionID string `json:"session_id"`
	}
	if err := c.do(ctx, http.MethodPost, "/sessions", assertedCaller, struct{}{}, &out); err != nil {
		return "", err
	}
	if out.SessionID == "" {
		return "", errors.New("daemon: create session returned empty session_id")
	}
	return out.SessionID, nil
}

// Inject queues a user message on the session's inbox. assertedCaller
// attributes this turn to the originating chat user.
func (c *Client) Inject(ctx context.Context, sid, assertedCaller, text string) error {
	body := map[string]string{"message": text}
	return c.do(ctx, http.MethodPost, "/sessions/"+sid+"/inject", assertedCaller, body, nil)
}

// Wake nudges a sleeping session to run a turn (e.g. after an inject on
// a session that has gone idle).
func (c *Client) Wake(ctx context.Context, sid, assertedCaller string) error {
	return c.do(ctx, http.MethodPost, "/sessions/"+sid+"/wake", assertedCaller, struct{}{}, nil)
}

// Event is one server-sent event from a session's output stream.
type Event struct {
	Type string
	Data string
}

// Subscribe opens the SSE stream for a session and delivers events to fn
// until the context is cancelled, the stream ends, or fn returns an
// error. It is the read half of the round-trip: the gateway relays these
// back into the chat thread.
func (c *Client) Subscribe(ctx context.Context, sid, assertedCaller string, fn func(Event) error) error {
	req, err := c.newRequest(ctx, http.MethodGet, "/sessions/"+sid+"/events", assertedCaller, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return c.statusErr(resp)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var ev Event
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "": // dispatch on blank-line boundary
			if ev.Type != "" || ev.Data != "" {
				if err := fn(ev); err != nil {
					return err
				}
			}
			ev = Event{}
		case strings.HasPrefix(line, "event:"):
			ev.Type = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			d := strings.TrimSpace(line[len("data:"):])
			if ev.Data != "" {
				ev.Data += "\n" + d
			} else {
				ev.Data = d
			}
		}
	}
	return sc.Err()
}

// do performs a JSON request/response round-trip. out may be nil when
// the response body is not needed.
func (c *Client) do(ctx context.Context, method, path, assertedCaller string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("daemon: marshal request: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := c.newRequest(ctx, method, path, assertedCaller, body)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.statusErr(resp)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("daemon: decode response: %w", err)
		}
	}
	return nil
}

func (c *Client) newRequest(ctx context.Context, method, path, assertedCaller string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.BearerToken)
	if assertedCaller != "" {
		req.Header.Set("X-Asserted-Caller", assertedCaller)
	}
	return req, nil
}

func (c *Client) statusErr(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(b))
	if msg == "" {
		msg = resp.Status
	}
	return fmt.Errorf("daemon: %s %s: %s", resp.Request.Method, resp.Request.URL.Path, msg)
}
