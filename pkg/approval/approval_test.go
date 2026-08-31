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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/switchboard/pkg/daemon"
)

var testSession = daemon.Session{App: "core-agent", ID: "sess-123"}

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(daemon.Config{BaseURL: srv.URL, BearerToken: "tok", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// frames writes a perms stream carrying the given raw SSE records.
func frames(t *testing.T, records ...string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, rec := range records {
			fmt.Fprint(w, rec)
		}
	}
}

func collect(t *testing.T, c *Client) ([]Prompt, error) {
	t.Helper()
	var got []Prompt
	err := c.Stream(context.Background(), testSession, "switchboard", func(p Prompt) error {
		got = append(got, p)
		return nil
	})
	return got, err
}

// The values here are the ones core-agent actually emits, not plausible
// stand-ins: verb is a bash command's FIRST TOKEN ("git", not "push"), source
// is a subagent name rather than a policy, and access is the daemon's short
// form. A fixture with invented values pins the JSON tags and nothing else,
// and the tags are not where the mistakes are.
func TestStreamReadsEveryFieldOffAPromptFrame(t *testing.T) {
	c := newTestClient(t, frames(t, "event: prompt\ndata: "+`{"id":"p1","kind":"bash","tool":"bash","detail":"git push --force origin main","verb":"git","source":"watch-prod-cluster","persist_tool":"bash","persist_key":"git","access":"w","at":"2026-08-31T10:00:00Z"}`+"\n\n"))

	got, err := collect(t, c)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d prompts, want 1", len(got))
	}
	want := Prompt{
		ID: "p1", Kind: KindBash, Tool: "bash", Detail: "git push --force origin main",
		Verb: "git", Source: "watch-prod-cluster", PersistTool: "bash", PersistKey: "git",
		Access: "w", At: time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC),
	}
	if got[0] != want {
		t.Errorf("prompt =\n %+v\nwant\n %+v", got[0], want)
	}
}

// The daemon stringifies the access mode before serializing, and the zero
// value stringifies to "none" — so `omitempty` never drops the field and a
// non-path-scope prompt arrives carrying a word, not an empty string. Code
// that treats "" as "no access mode to show" would show "none" on every bash
// prompt in the channel.
func TestStreamCarriesTheAccessModeTheDaemonActuallySends(t *testing.T) {
	c := newTestClient(t, frames(t, "event: prompt\ndata: "+`{"id":"p1","kind":"bash","access":"none"}`+"\n\n"))
	got, err := collect(t, c)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got[0].Access != "none" {
		t.Errorf("Access = %q, want the daemon's short form verbatim", got[0].Access)
	}
}

// The daemon sends the tool under "tool", not "tool_name". Getting this
// wrong loses the tool silently: the prompt still posts, just without the
// thing it is about.
func TestStreamReadsTheToolFromTheFieldTheDaemonUses(t *testing.T) {
	c := newTestClient(t, frames(t, "event: prompt\ndata: "+`{"id":"p1","tool":"kubectl","tool_name":"WRONG"}`+"\n\n"))
	got, err := collect(t, c)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(got) != 1 || got[0].Tool != "kubectl" {
		t.Fatalf("Tool = %q, want kubectl", got[0].Tool)
	}
}

func TestStreamAddressesTheSessionByItsAppQualifiedRoute(t *testing.T) {
	var path, auth, caller, accept string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		caller, accept = r.Header.Get("X-Asserted-Caller"), r.Header.Get("Accept")
		w.WriteHeader(http.StatusOK)
	})
	if _, err := collect(t, c); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if want := "/sessions/core-agent/sess-123/perms/stream"; path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if auth != "Bearer tok" {
		t.Errorf("Authorization = %q", auth)
	}
	if caller != "switchboard" {
		t.Errorf("X-Asserted-Caller = %q", caller)
	}
	if accept != "text/event-stream" {
		t.Errorf("Accept = %q", accept)
	}
}

func TestStreamSaysWhenTheAgentOffersNoBroker(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "perms/stream capability not registered", http.StatusNotImplemented)
	})
	_, err := collect(t, c)
	if !errors.Is(err, ErrNotSupported) {
		t.Fatalf("err = %v, want ErrNotSupported", err)
	}
	// A session that never asks permission is not a broken session; the
	// caller must be able to tell this apart from a daemon in trouble.
	if daemon.IsTransient(err) {
		t.Error("a missing capability was reported as worth retrying")
	}
}

// A frame that will not parse cannot be answered and cannot be correlated to
// say so — but the prompts after it can. Failing the stream on one bad frame
// would abandon them too.
func TestStreamSkipsAFrameItCannotParseAndKeepsReading(t *testing.T) {
	c := newTestClient(t, frames(t,
		"event: prompt\ndata: {\"id\":\"p1\"}\n\n",
		"event: prompt\ndata: {not json\n\n",
		"event: prompt\ndata: {\"id\":\"\",\"tool\":\"bash\"}\n\n",
		"event: prompt\ndata: {\"id\":\"p2\"}\n\n",
	))
	got, err := collect(t, c)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(got) != 2 || got[0].ID != "p1" || got[1].ID != "p2" {
		t.Fatalf("prompts = %+v, want p1 and p2", got)
	}
}

// The daemon may grow a second event name on this stream. An old switchboard
// must ignore it rather than try to answer it.
func TestStreamIgnoresEventNamesItDoesNotKnow(t *testing.T) {
	c := newTestClient(t, frames(t,
		"event: heartbeat\ndata: {\"id\":\"nope\"}\n\n",
		"event: prompt\ndata: {\"id\":\"p1\"}\n\n",
	))
	got, err := collect(t, c)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(got) != 1 || got[0].ID != "p1" {
		t.Fatalf("prompts = %+v, want only p1", got)
	}
}

func TestStreamStopsWhenTheCallbackDoes(t *testing.T) {
	stop := errors.New("enough")
	c := newTestClient(t, frames(t,
		"event: prompt\ndata: {\"id\":\"p1\"}\n\n",
		"event: prompt\ndata: {\"id\":\"p2\"}\n\n",
	))
	n := 0
	err := c.Stream(context.Background(), testSession, "switchboard", func(Prompt) error {
		n++
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatalf("err = %v, want the callback's own error back", err)
	}
	if n != 1 {
		t.Errorf("callback ran %d times after asking to stop", n)
	}
}

// A stream the daemon closes cleanly is not a failure: it does that on
// shutdown and when the agent is done.
func TestStreamEndingCleanlyIsNotAnError(t *testing.T) {
	c := newTestClient(t, frames(t))
	if _, err := collect(t, c); err != nil {
		t.Fatalf("Stream: %v", err)
	}
}

type respondCall struct {
	path   string
	caller string
	body   map[string]any
}

func respondServer(t *testing.T, status int, reply string) (*Client, *respondCall) {
	t.Helper()
	got := &respondCall{}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		got.caller = r.Header.Get("X-Asserted-Caller")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got.body)
		if status != http.StatusOK {
			http.Error(w, reply, status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, reply)
	})
	return c, got
}

// The one rule of this endpoint: the approver is asserted in the header, and
// the body's approver field is left off entirely. Setting it can only turn a
// working request into a 400, since the daemon uses it to disagree with the
// caller, never to widen what it records.
func TestRespondNamesTheApproverInTheHeaderAndNotTheBody(t *testing.T) {
	c, got := respondServer(t, http.StatusOK, `{"acknowledged":true,"approver":"alice@example.com"}`)

	ack, err := c.Respond(context.Background(), testSession, "alice@example.com", "p1", AllowOnce)
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if got.caller != "alice@example.com" {
		t.Errorf("X-Asserted-Caller = %q, want the pressing human", got.caller)
	}
	if _, ok := got.body["approver"]; ok {
		t.Errorf("body carried an approver field: %v", got.body)
	}
	if got.body["id"] != "p1" || got.body["decision"] != "allow-once" {
		t.Errorf("body = %v, want id p1 / decision allow-once", got.body)
	}
	if want := "/sessions/core-agent/sess-123/perms/respond"; got.path != want {
		t.Errorf("path = %q, want %q", got.path, want)
	}
	if !ack.Attributed() || ack.Approver != "alice@example.com" {
		t.Errorf("ack = %+v, want the approver read back", ack)
	}
}

// An approval the daemon could not attribute still took effect, and the
// caller has to be able to tell that apart from one it could.
func TestRespondReportsAnApprovalRecordedOnNobody(t *testing.T) {
	c, _ := respondServer(t, http.StatusOK, `{"acknowledged":true}`)
	ack, err := c.Respond(context.Background(), testSession, "", "p1", Deny)
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if ack.Attributed() {
		t.Errorf("ack = %+v, want an unattributed approval", ack)
	}
}

func TestRespondReportsAPromptThatIsNoLongerPending(t *testing.T) {
	c, _ := respondServer(t, http.StatusNotFound, "prompt id not found")
	_, err := c.Respond(context.Background(), testSession, "alice", "p1", Deny)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if daemon.IsTransient(err) {
		t.Error("an answered prompt was reported as worth retrying")
	}
}

// Stream carries no prompt id, so a 404 there is the session, not a prompt.
// It reaches the same sentinel — the daemon gives both the same code — and
// must also stay reachable as a 404 status, which is how the router already
// recognises a session that has gone.
func TestStreamReportsASessionTheDaemonNoLongerHas(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "session not found", http.StatusNotFound)
	})
	_, err := collect(t, c)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	var se *daemon.StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusNotFound {
		t.Fatalf("err = %v, want the 404 still reachable as a *daemon.StatusError", err)
	}
	if daemon.IsTransient(err) {
		t.Error("a vanished session was reported as worth retrying")
	}
}

// A 2xx that declines to acknowledge contradicts itself, and the alternative
// to checking is reporting it as a successful anonymous approval — telling a
// thread the call was released on the strength of the status line alone.
func TestRespondDoesNotTreatAnUnacknowledgedReplyAsSuccess(t *testing.T) {
	c, _ := respondServer(t, http.StatusOK, `{"acknowledged":false}`)
	ack, err := c.Respond(context.Background(), testSession, "alice", "p1", Deny)
	if err == nil {
		t.Fatalf("Respond succeeded with ack %+v on a reply that acknowledged nothing", ack)
	}
}

func TestRespondReportsASessionWithNoBroker(t *testing.T) {
	c, _ := respondServer(t, http.StatusNotImplemented, "not registered")
	_, err := c.Respond(context.Background(), testSession, "alice", "p1", Deny)
	if !errors.Is(err, ErrNotSupported) {
		t.Fatalf("err = %v, want ErrNotSupported", err)
	}
}

// Everything that is not one of the two meaningful codes keeps the
// transient/terminal split the four verbs already use, so the router does not
// need a second rule for these two routes.
func TestRespondKeepsTheTransientSplitForEveryOtherFailure(t *testing.T) {
	c, _ := respondServer(t, http.StatusBadGateway, "upstream is unwell")
	_, err := c.Respond(context.Background(), testSession, "alice", "p1", Deny)
	if !daemon.IsTransient(err) {
		t.Fatalf("err = %v, want a transient failure", err)
	}
	var se *daemon.StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusBadGateway {
		t.Fatalf("err = %v, want a *daemon.StatusError carrying 502", err)
	}
}

// A decision the daemon would reject is caught here, so a bug in the caller
// costs an error instead of a round trip and a 400.
func TestRespondRefusesADecisionTheDaemonWouldNotAccept(t *testing.T) {
	sent := false
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		sent = true
		w.WriteHeader(http.StatusOK)
	})
	for _, d := range []Decision{"", "allow", "APPROVE", "allow_once"} {
		if _, err := c.Respond(context.Background(), testSession, "alice", "p1", d); err == nil {
			t.Errorf("Respond accepted %q", string(d))
		}
	}
	if _, err := c.Respond(context.Background(), testSession, "alice", "", AllowOnce); err == nil {
		t.Error("Respond accepted an empty prompt id")
	}
	if sent {
		t.Error("a request went out for a decision that was never valid")
	}
}

// Every value the daemon documents must survive the round trip verbatim: a
// decision this client mangles is a permission granted or refused wrongly.
func TestEveryDecisionGoesOutOnTheWireUnchanged(t *testing.T) {
	want := []string{"deny", "allow-once", "allow-session", "allow-session-verb", "allow-session-tool", "allow-always"}
	// From the exported vocabulary, so this also pins what Decisions hands out:
	// a caller iterating it to be exhaustive is only exhaustive if it is.
	all := Decisions()
	if len(all) != len(want) {
		t.Fatalf("Decisions returns %d values, want the %d the daemon accepts", len(all), len(want))
	}
	scratch := Decisions()
	scratch[0] = ""
	if Decisions()[0] == "" {
		t.Error("Decisions hands out the vocabulary itself, so a caller can edit it")
	}
	for i, d := range all {
		if !d.Valid() {
			t.Errorf("%q is not Valid", string(d))
		}
		c, got := respondServer(t, http.StatusOK, `{"acknowledged":true}`)
		if _, err := c.Respond(context.Background(), testSession, "alice", "p1", d); err != nil {
			t.Fatalf("Respond(%q): %v", string(d), err)
		}
		if got.body["decision"] != want[i] {
			t.Errorf("decision on the wire = %v, want %q", got.body["decision"], want[i])
		}
	}
	if Deny.Allows() {
		t.Error("Deny reads as an approval")
	}
	for _, d := range all[1:] {
		if !d.Allows() {
			t.Errorf("%q does not read as an approval", string(d))
		}
	}
}

// Allows is a standalone predicate on an exported type, so nothing forces it
// to be called only on values Respond has already validated. A mangled button
// id or an unset field must not read as "approved" on the way to a thread
// notice or an audit line.
func TestAnUnrecognisedDecisionDoesNotReadAsAnApproval(t *testing.T) {
	for _, d := range []Decision{"", "allow", "APPROVE", "allow_once", "yes", "some-future-decision"} {
		if d.Allows() {
			t.Errorf("%q reads as an approval", string(d))
		}
		if d.Valid() {
			t.Errorf("%q reads as a decision the daemon accepts", string(d))
		}
	}
}

// A 2xx whose body is unreadable must not report an empty approver: that is
// indistinguishable from the daemon saying it recorded nobody, which is the
// one distinction this path exists to keep. Nor may it read as a plain failure
// — the daemon took the request before the reply went wrong, so the decision
// has probably taken effect and a caller must be able to tell that apart from
// an answer that never arrived.
func TestRespondDoesNotPassOffAnUnreadableReplyAsAnonymous(t *testing.T) {
	for name, body := range map[string]string{
		"unreadable":     `{"acknowledged":`,
		"unacknowledged": `{"acknowledged":false}`,
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := respondServer(t, http.StatusOK, body)
			ack, err := c.Respond(context.Background(), testSession, "alice", "p1", Deny)
			if err == nil {
				t.Fatalf("Respond succeeded with ack %+v", ack)
			}
			if !errors.Is(err, ErrMaybeApplied) {
				t.Errorf("err = %v, want it to warn the decision probably landed", err)
			}
			if errors.Is(err, ErrNotFound) {
				t.Errorf("err = %v reads as a prompt that was never there", err)
			}
		})
	}
}

func TestAnIncompleteSessionIsRefusedBeforeItBecomesAWeirdURL(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request went out for an incomplete session: %s", r.URL.Path)
	})
	for _, sess := range []daemon.Session{{App: "core-agent"}, {ID: "sess-123"}, {}} {
		if _, err := c.Respond(context.Background(), sess, "alice", "p1", Deny); err == nil {
			t.Errorf("Respond accepted session %+v", sess)
		}
		if err := c.Stream(context.Background(), sess, "sb", func(Prompt) error { return nil }); err == nil {
			t.Errorf("Stream accepted session %+v", sess)
		}
	}
}

func decisions(opts []Option) []Decision {
	out := make([]Decision, len(opts))
	for i, o := range opts {
		out[i] = o.Decision
	}
	return out
}

func TestOptionsOffersTheWholeVocabularyWhenTheGateFoundAVerb(t *testing.T) {
	got := decisions(Options(Prompt{Kind: KindBash, Tool: "bash", Verb: "git"}))
	want := []Decision{Deny, AllowOnce, AllowSession, AllowSessionVerb, AllowSessionTool, AllowAlways}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("options = %v, want %v (narrowest first)", got, want)
	}
}

// The control-plane gate records ANY non-deny answer as allow-once and
// remembers nothing, so the four wider decisions are all exactly allow-once
// there. Offering "Always allow (saved)" would tell someone they had
// installed a standing grant on the file governing the agent's own
// permissions, when nothing was saved and the next write re-prompts.
func TestOptionsWithholdsGrantsAControlPlaneWriteWouldNotKeep(t *testing.T) {
	got := decisions(Options(Prompt{Kind: KindControlPlaneWrite, Tool: "write_file", Verb: "git"}))
	want := []Decision{Deny, AllowOnce}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("options = %v, want %v — the rest are allow-once wearing a wider label", got, want)
	}
	for _, o := range Options(Prompt{Kind: KindControlPlaneWrite}) {
		if o.Broad {
			t.Errorf("%q is marked as outliving a request the gate will not remember", string(o.Decision))
		}
	}
}

// The daemon accepts allow-session-verb on a verbless prompt, and it widens
// nothing. A button whose press means less than its label says is worse than
// no button, so it is not offered — which is what core-agent's own reference
// client does.
func TestOptionsHidesTheVerbAnswerWhenThereIsNoVerbToScopeItTo(t *testing.T) {
	for _, o := range Options(Prompt{Kind: KindGeneric, Tool: "bash"}) {
		if o.Decision == AllowSessionVerb {
			t.Fatalf("offered %q on a prompt with no verb", string(o.Decision))
		}
	}
	if n := len(Options(Prompt{Tool: "bash"})); n != 5 {
		t.Errorf("got %d options, want 5", n)
	}
}

// Slack caps button text at 75 characters, so an agent-supplied tool name is
// the far end deciding whether switchboard's API call is valid. The bound
// checked here is derived from labelCap rather than written out, so raising
// the cap past what a button holds fails here instead of at the Slack API.
func TestOptionsKeepsALabelInsideAButton(t *testing.T) {
	const slackButtonCap = 75
	// The longest fixed text any label wraps around its clipped fragment.
	const surround = len("Allow every  this session")
	if labelCap+surround > slackButtonCap {
		t.Fatalf("labelCap %d + %d of fixed text exceeds the %d a button holds", labelCap, surround, slackButtonCap)
	}
	long := strings.Repeat("verylongtoolname", 40)
	for _, o := range Options(Prompt{Tool: long, Verb: long}) {
		if n := len([]rune(o.Label)); n > slackButtonCap {
			t.Errorf("label %d runes, want <= %d: %q", n, slackButtonCap, o.Label)
		}
		if strings.Contains(o.Label, long) {
			t.Errorf("label carried the whole tool name: %q", o.Label)
		}
	}
}

// Every prompt must be answerable, including one whose kind this build has
// never heard of — the alternative is an agent blocked forever on a question
// switchboard declined to ask. An unknown kind gets the daemon's full
// vocabulary rather than the control-plane pair: withholding answers is a
// fact known about one specific gate, and guessing it about an unknown one
// risks leaving a blocked agent nothing but deny.
func TestOptionsAnswersAKindThisBuildDoesNotKnow(t *testing.T) {
	got := decisions(Options(Prompt{Kind: "some_future_kind", Tool: "bash", Verb: "git"}))
	want := []Decision{Deny, AllowOnce, AllowSession, AllowSessionVerb, AllowSessionTool, AllowAlways}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("options = %v, want %v", got, want)
	}
	if got[0] != Deny {
		t.Errorf("first option is %q, want the narrowest answer", string(got[0]))
	}
}

// Deny and allow-once end with the request; everything else outlives it, and
// a caller that wants to treat the wide answers differently reads this rather
// than re-deriving the list.
func TestOptionsMarksTheAnswersThatOutliveTheRequest(t *testing.T) {
	broad := map[Decision]bool{}
	for _, o := range Options(Prompt{Tool: "bash", Verb: "rm"}) {
		broad[o.Decision] = o.Broad
	}
	for d, want := range map[Decision]bool{
		Deny: false, AllowOnce: false, AllowSession: true,
		AllowSessionVerb: true, AllowSessionTool: true, AllowAlways: true,
	} {
		if broad[d] != want {
			t.Errorf("%q Broad = %v, want %v", string(d), broad[d], want)
		}
	}
}

// The gate that raises a path-scope prompt consults neither the session grant
// nor the session-tool grant on the way back in, so both leave the same
// out-of-scope path prompting exactly as before. The tool grant is the worse
// of the two: inert for the path shown, but NOT inert — the file-write gate
// does read it, so approving an out-of-scope write this way silently stops the
// prompting for every in-scope write by that tool.
func TestOptionsWithholdsSessionGrantsAPathScopePromptWouldNotHonour(t *testing.T) {
	p := Prompt{
		Kind:       KindPathScope,
		Tool:       "write_file",
		Detail:     "write /home/u/.ssh/authorized_keys (out of scope)",
		PersistKey: "/home/u/.ssh/authorized_keys",
		Access:     "w",
	}
	got := decisions(Options(p))
	want := []Decision{Deny, AllowOnce, AllowAlways}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("options = %v, want %v — only these do what they say on a path prompt", got, want)
	}
}

// An always-allow on a path prompt does not persist the path in front of the
// reader: the daemon widens it to the enclosing directory tree and promotes a
// write to read-write. The label has to carry that, because the prompt above
// it names one file and nothing else on screen suggests otherwise.
func TestOptionsNamesTheTreeAStandingPathGrantActuallyCovers(t *testing.T) {
	var label string
	for _, o := range Options(Prompt{Kind: KindPathScope, Tool: "write_file", PersistKey: "/home/u/.ssh/authorized_keys"}) {
		if o.Decision == AllowAlways {
			label = o.Label
		}
	}
	if label == "" {
		t.Fatal("no standing grant offered on a path prompt; it is the one wide answer that gate honours")
	}
	if !strings.Contains(strings.ToLower(label), "directory") {
		t.Errorf("label %q does not say the grant covers a directory", label)
	}
	if strings.Contains(label, "authorized_keys") {
		t.Errorf("label %q names the one path, but the grant covers its whole directory", label)
	}
}

// A namespaced toolset reports its NAMESPACE as the tool, while the grant is
// scoped to the underlying tool. Interpolating the field would render "Allow
// every mcp this session" for a press that trusts one MCP tool — a label
// describing a far wider grant than the button installs.
func TestOptionsDoesNotPutANamespaceWhereAToolNameBelongs(t *testing.T) {
	var label string
	for _, o := range Options(Prompt{Kind: KindGeneric, Tool: "mcp", Detail: "deploy_service(env=prod)"}) {
		if o.Decision == AllowSessionTool {
			label = o.Label
		}
	}
	if label == "" {
		t.Fatal("no session-tool answer offered on a generic prompt")
	}
	if strings.Contains(label, "mcp") {
		t.Errorf("label %q names the namespace as though it were the tool", label)
	}

	// A prompt whose Tool means what it looks like still gets named.
	for _, o := range Options(Prompt{Kind: KindBash, Tool: "bash"}) {
		if o.Decision == AllowSessionTool && !strings.Contains(o.Label, "bash") {
			t.Errorf("label %q dropped a tool name that was accurate", o.Label)
		}
	}
}

func TestNewRefusesAConfigThatAddressesNoDaemon(t *testing.T) {
	for _, cfg := range []daemon.Config{
		{BearerToken: "t"},
		{BaseURL: "http://x/", BearerToken: "t"},
		{BaseURL: "http://x"},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("New accepted %+v", cfg)
		}
	}
}

// A whole-request timeout bounds reading the body, and reading the body is
// the whole of Stream. It must be absent whether the client was built here or
// handed in — the natural way to reach this constructor is with the same
// daemon.Config the four-verb client got, and a shared client carrying a 30s
// deadline would cut every prompt stream at thirty seconds while looking
// exactly like the daemon hanging up.
func TestNoClientTimeoutSurvivesToCutAStream(t *testing.T) {
	c, err := New(daemon.Config{BaseURL: "http://x", BearerToken: "t"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.http.Timeout != 0 {
		t.Errorf("default client Timeout = %v, want none", c.http.Timeout)
	}

	shared := &http.Client{Timeout: 30 * time.Second}
	c, err = New(daemon.Config{BaseURL: "http://x", BearerToken: "t", HTTPClient: shared})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.http.Timeout != 0 {
		t.Errorf("supplied client Timeout = %v, want it dropped", c.http.Timeout)
	}
	// Dropped on a copy: the caller's client is still theirs, and the
	// four-verb client sharing it must keep its own deadline.
	if shared.Timeout != 30*time.Second {
		t.Errorf("the caller's own client was mutated: Timeout = %v", shared.Timeout)
	}
}
