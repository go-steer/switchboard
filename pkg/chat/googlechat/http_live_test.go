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

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/switchboard/pkg/chat"
)

// The clicks below are captured, not written: the two button presses from the
// first live HTTP-ingress session (2026-10-04), scrubbed, with Chat's number
// spelling intact. The hand-written click bodies in http_test.go were
// built from the documentation; these are what Chat actually sends, and they
// go through the same handler, so a difference between the two shows up here.
// Live, both cards were edited in place.

func liveClick(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "events", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const (
	liveConv    = "spaces/AAQAXbGkY1s:spaces/AAQAXbGkY1s/threads/"
	liveChannel = "spaces/AAQAXbGkY1s"
)

func TestLiveApprovalClickIsAPressAnsweredInBand(t *testing.T) {
	f := &fakeMessenger{}
	a := newIngressAdapter(t, f)
	a.cards = CardsStatus
	h := &pressHandler{a: a}

	r := postEvent(liveClick(t, "addon-live-http-approval-click.json"))
	r.Header.Set("Authorization", "Bearer good-token")
	rec := httptest.NewRecorder()
	var wg turnGroup
	a.eventHandler(context.Background(), &wg, h).ServeHTTP(rec, r)
	wg.Wait()

	if len(h.presses) != 1 {
		t.Fatalf("presses = %d, want 1 (response %s)", len(h.presses), rec.Body)
	}
	conv := liveConv + "Wq6jDs3fNk5"
	want := chat.Press{
		Conversation: conv,
		Channel:      liveChannel,
		Caller:       "ada@example.com",
		DecisionID:   "core-agent/01a10664-b664-734b-b1f6-b823bd8f020a#60f01814ec2e051934b43233",
		Option:       "allow-once",
		Message:      chat.MessageRef{Conversation: conv, ID: "spaces/AAQAXbGkY1s/messages/Wq6jDs3fNk5.Wq6jDs3fNk5"},
	}
	if p := h.presses[0]; p != want {
		t.Errorf("press = %+v\nwant    %+v", p, want)
	}
	msg, ok := clickEnvelope(t, rec.Body.Bytes())
	if !ok || len(msg.CardsV2) != 1 || !strings.Contains(cardText(msg.CardsV2[0].Card), "Allowed") {
		t.Fatalf("response = %s, want the settled card in an updateMessageAction", rec.Body)
	}
	assertNothingClickable(t, "settled", msg.CardsV2[0].Card)
}

func TestLiveProgressClickIsACommandAnsweredInBand(t *testing.T) {
	f := &fakeMessenger{}
	a := newIngressAdapter(t, f)
	a.cards = CardsStatus
	h := &choiceHandler{}
	h.ack = "Progress mode for this channel set to *stream*."
	h.choices = []string{"off", "indicator", "status", "stream"}

	r := postEvent(liveClick(t, "addon-live-http-progress-click.json"))
	r.Header.Set("Authorization", "Bearer good-token")
	rec := httptest.NewRecorder()
	var wg turnGroup
	a.eventHandler(context.Background(), &wg, h).ServeHTTP(rec, r)
	wg.Wait()

	if len(h.cmds) != 1 || h.cmds[0].Name != "progress" || strings.Join(h.cmds[0].Args, ",") != "stream" {
		t.Fatalf("cmds = %+v, want progress stream (response %s)", h.cmds, rec.Body)
	}
	if h.cmds[0].Channel != liveChannel || h.cmds[0].Caller != "ada@example.com" {
		t.Errorf("command from %q in %q, want the clicker ada@example.com in %q",
			h.cmds[0].Caller, h.cmds[0].Channel, liveChannel)
	}
	msg, ok := clickEnvelope(t, rec.Body.Bytes())
	if !ok || len(msg.CardsV2) != 1 {
		t.Fatalf("response = %s, want the ack card in an updateMessageAction", rec.Body)
	}
	if !strings.Contains(cardText(msg.CardsV2[0].Card), "stream") {
		t.Errorf("ack card = %q, want it to name the new mode", cardText(msg.CardsV2[0].Card))
	}
}

// The Broad-answer confirmation from the live session (#92): the first press
// on "Allow for this session" and the Yes that followed. The confirmation is
// built from the card Chat echoes in the click, so this pins what that echo
// was measured to carry — the question's paragraph with its MARKDOWN text
// syntax intact, which is what keeps the command a code block once copied
// onto the confirmation.
func TestLiveBroadConfirmationKeepsTheQuestionAndGatesThePress(t *testing.T) {
	f := &fakeMessenger{}
	a := newIngressAdapter(t, f)
	a.cards = CardsStatus
	h := &pressHandler{a: a}

	confirm := click(t, a, h, liveClick(t, "addon-live-http-broad-confirm-click.json"))
	if len(h.presses) != 0 {
		t.Fatalf("the first press on a Broad answer reached the router: %+v", h.presses)
	}
	body := confirm.Sections[0].Widgets[0].TextParagraph
	if body == nil || body.TextSyntax != "MARKDOWN" || !strings.Contains(body.Text, "```\nkubectl config get-contexts\n```") {
		t.Errorf("the confirmation's question = %+v, want the echoed MARKDOWN paragraph with its fence", body)
	}
	if b := buttonsOf(confirm); len(b) != 2 || b[0].Text != "Yes, allow" || b[1].Text != "Back" {
		t.Errorf("confirmation buttons = %+v", b)
	}

	r := postEvent(liveClick(t, "addon-live-http-broad-yes-click.json"))
	r.Header.Set("Authorization", "Bearer good-token")
	rec := httptest.NewRecorder()
	var turns turnGroup
	a.eventHandler(context.Background(), &turns, h).ServeHTTP(rec, r)
	turns.Wait()
	if len(h.presses) != 1 || h.presses[0].Option != "allow-session" || h.presses[0].Caller != "ada@example.com" {
		t.Fatalf("presses = %+v, want one allow-session from the clicker", h.presses)
	}
}

// Back, from the same live session: pressed on the confirmation, it must
// restore the question's six answers, Broad ones still asking first, and
// reach nobody.
func TestLiveBackRestoresTheQuestion(t *testing.T) {
	f := &fakeMessenger{}
	a := newIngressAdapter(t, f)
	a.cards = CardsStatus
	h := &pressHandler{a: a}

	back := click(t, a, h, liveClick(t, "addon-live-http-broad-back-click.json"))
	if len(h.presses) != 0 {
		t.Fatalf("Back reached the router: %+v", h.presses)
	}
	b := buttonsOf(back)
	if len(b) != 6 || b[0].Text != "Deny" || b[5].Text != "Always allow (saved)" {
		t.Fatalf("restored buttons = %d, want the question's six answers", len(b))
	}
	if paramsOf(b[1])[paramStage] != "" || paramsOf(b[2])[paramStage] != stageConfirm {
		t.Errorf("restored row lost which answers ask first: once=%v session=%v", paramsOf(b[1]), paramsOf(b[2]))
	}
	if strings.Contains(cardText(back), confirmSuffix) {
		t.Errorf("Back left the confirmation line on the question")
	}
}
