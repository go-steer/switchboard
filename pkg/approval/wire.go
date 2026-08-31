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

package approval

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-steer/switchboard/pkg/daemon"
)

// eventPrompt is the only SSE event name the perms stream defines.
const eventPrompt = "prompt"

// promptFrame is the JSON payload of a prompt event. Field names track
// core-agent's attach.PromptFrame; note that the tool arrives under "tool",
// not "tool_name".
type promptFrame struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	Tool        string    `json:"tool"`
	Detail      string    `json:"detail"`
	Verb        string    `json:"verb"`
	Source      string    `json:"source"`
	PersistTool string    `json:"persist_tool"`
	PersistKey  string    `json:"persist_key"`
	Access      string    `json:"access"`
	At          time.Time `json:"at"`
}

func (f promptFrame) prompt() Prompt {
	return Prompt{
		ID:          f.ID,
		Kind:        f.Kind,
		Tool:        f.Tool,
		Detail:      f.Detail,
		Verb:        f.Verb,
		Source:      f.Source,
		PersistTool: f.PersistTool,
		PersistKey:  f.PersistKey,
		Access:      f.Access,
		At:          f.At,
	}
}

// respondRequest is the POST body of /perms/respond.
//
// The wire format also has an "approver" field. It is absent here on purpose,
// and not merely unset: the daemon attributes the decision from the verified
// caller and uses this field only to check a client's claim against that
// verdict, so the field can never widen what is recorded — it can only
// disagree and earn a 400. A struct with no such field cannot acquire one by
// accident. See Client.Respond.
type respondRequest struct {
	ID       string `json:"id"`
	Decision string `json:"decision"`
}

// respondResponse is the 200 body of /perms/respond.
type respondResponse struct {
	Acknowledged bool   `json:"acknowledged"`
	Approver     string `json:"approver"`
}

// request builds a request against one of the two perms routes for a session.
// route is the leaf ("stream" or "respond"); the session path comes from the
// four-verb client so both agree on how a session is addressed — app-qualified,
// because the unqualified shortcut 409s when an id is ambiguous across apps.
func (c *Client) request(ctx context.Context, method string, sess daemon.Session, route, assertedCaller string, body []byte) (*http.Request, error) {
	if sess.App == "" || sess.ID == "" {
		return nil, fmt.Errorf("approval: session is incomplete (app=%q id=%q)", sess.App, sess.ID)
	}
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	url := c.cfg.BaseURL + "/sessions/" + sess.App + "/" + sess.ID + "/perms/" + route
	req, err := http.NewRequestWithContext(ctx, method, url, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.BearerToken)
	if assertedCaller != "" {
		req.Header.Set("X-Asserted-Caller", assertedCaller)
	}
	return req, nil
}

// statusErr turns a non-2xx into the most specific error available: the two
// status codes that mean something particular on these routes become their
// sentinels, and everything else becomes a *daemon.StatusError so callers get
// the same transient/terminal split — and the same daemon.IsTransient — that
// they already apply to the four verbs.
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
	case http.StatusNotImplemented:
		return &routeError{sentinel: ErrNotSupported, status: status}
	case http.StatusNotFound:
		// The session route itself 404s for a session the daemon has never
		// heard of, and the prompt id 404s once it is answered. Both mean the
		// thing being addressed is gone, and both leave a caller with nothing
		// to do but say so, so they are not worth splitting apart here.
		return &routeError{sentinel: ErrNotFound, status: status}
	}
	return status
}

// routeError carries both of the things a caller needs from one of these two
// meaningful status codes: the sentinel, so it can be matched with errors.Is,
// and the underlying *daemon.StatusError, so daemon.IsTransient reaches the
// status code and gives the same verdict it gives for the four verbs. Handing
// back only the sentinel would make both of these fail closed the wrong way —
// IsTransient treats an error it cannot classify as worth retrying, so "this
// agent has no permission broker" would come out as "try again shortly".
type routeError struct {
	sentinel error
	status   *daemon.StatusError
}

func (e *routeError) Error() string {
	return fmt.Sprintf("%s (%s %s: %s)", e.sentinel, e.status.Method, e.status.Path, e.status.Message)
}

// Unwrap returns both so errors.Is finds the sentinel and errors.As finds the
// status.
func (e *routeError) Unwrap() []error { return []error{e.sentinel, e.status} }
