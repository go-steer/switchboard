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
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	chatv1 "google.golang.org/api/chat/v1"

	"github.com/go-steer/switchboard/pkg/chat"
)

var confirmDecision = &chat.Decision{
	ID: "core-agent/s1#p7",
	Options: []chat.DecisionOption{
		{Value: "deny", Label: "Deny"},
		{Value: "allow-once", Label: "Allow once"},
		{Value: "allow-always", Label: "Always allow <saved>", Broad: true},
	},
}

// clickOn builds an add-on click on a button of hosting, the way Chat
// delivers one: the hosting card whole under the message, the button's
// parameters under commonEventObject.
func clickOn(t *testing.T, hosting *chatv1.GoogleAppsCardV1Card, params map[string]string) string {
	t.Helper()
	ev := map[string]any{
		"chat": map[string]any{
			"user":  map[string]any{"name": "users/7", "email": "bob@example.com"},
			"space": map[string]any{"name": "spaces/AAA"},
			"buttonClickedPayload": map[string]any{
				"message": map[string]any{
					"name":    "spaces/AAA/messages/Q1",
					"thread":  map[string]any{"name": "spaces/AAA/threads/T1"},
					"cardsV2": singleCard(hosting),
				},
			},
		},
		"commonEventObject": map[string]any{"parameters": params},
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// click posts one click through the ingress handler and returns the card it
// answered with in-band.
func click(t *testing.T, a *Adapter, h chat.Handler, body string) *chatv1.GoogleAppsCardV1Card {
	t.Helper()
	r := postEvent(body)
	r.Header.Set("Authorization", "Bearer good-token")
	rec := httptest.NewRecorder()
	var turns turnGroup
	a.eventHandler(context.Background(), &turns, h).ServeHTTP(rec, r)
	turns.Wait()
	msg, ok := clickEnvelope(t, rec.Body.Bytes())
	if !ok || len(msg.CardsV2) != 1 {
		t.Fatalf("response = %s, want a card in an updateMessageAction", rec.Body)
	}
	return msg.CardsV2[0].Card
}

// TestABroadAnswerTakesTwoPresses is #92: on Chat a Broad answer used to apply
// on one tap, where Slack asks first. Its first press must reach nobody and
// swap the row for a confirmation naming what it grants; Back must restore the
// question exactly; and only Yes may reach the router, as the answer pressed.
func TestABroadAnswerTakesTwoPresses(t *testing.T) {
	f := &fakeMessenger{}
	a := newIngressAdapter(t, f)
	a.cards = CardsStatus
	h := &pressHandler{a: a}

	question := decisionCard("**Permission needed** — `bash`", confirmDecision, testAudience)
	orig := buttonsOf(question)
	if len(orig) != 3 || paramsOf(orig[2])[paramStage] != stageConfirm || paramsOf(orig[1])[paramStage] != "" {
		t.Fatalf("only the Broad answer's button should ask for confirmation: %+v", orig)
	}

	// First press on the Broad answer: a confirmation, and no press.
	confirm := click(t, a, h, clickOn(t, question, paramsOf(orig[2])))
	if len(h.presses) != 0 {
		t.Fatalf("the first press on a Broad answer reached the router: %+v", h.presses)
	}
	text := cardText(confirm)
	if !strings.Contains(text, "Permission needed") || !strings.Contains(text, "this outlasts the request being made") {
		t.Errorf("confirmation lost the question or does not say what it grants:\n%s", text)
	}
	if !strings.Contains(text, "Always allow &lt;saved&gt;") {
		t.Errorf("the label is not shown escaped on the confirmation line:\n%s", text)
	}
	cb := buttonsOf(confirm)
	if len(cb) != 2 || cb[0].Text != "Yes, allow" || cb[1].Text != "Back" {
		t.Fatalf("confirmation buttons = %+v, want Yes, allow and Back", cb)
	}
	if p := paramsOf(cb[0]); p[paramOption] != "allow-always" || p[paramStage] != "" || p[paramDecision] != confirmDecision.ID {
		t.Errorf("Yes = %v, want the real press of allow-always", p)
	}

	// Back: the question as it was.
	back := click(t, a, h, clickOn(t, confirm, paramsOf(cb[1])))
	if len(h.presses) != 0 {
		t.Fatalf("Back reached the router: %+v", h.presses)
	}
	if got, want := cardText(back), cardText(question); got != want {
		t.Errorf("Back did not restore the question:\n got %q\nwant %q", got, want)
	}
	rb := buttonsOf(back)
	if len(rb) != len(orig) {
		t.Fatalf("Back restored %d buttons, want %d", len(rb), len(orig))
	}
	for i := range orig {
		if rb[i].Text != orig[i].Text || !mapsEqual(paramsOf(rb[i]), paramsOf(orig[i])) {
			t.Errorf("restored button %d = %q %v, want %q %v", i, rb[i].Text, paramsOf(rb[i]), orig[i].Text, paramsOf(orig[i]))
		}
	}

	// And through again, this time to Yes: one press, as allow-always.
	confirm = click(t, a, h, clickOn(t, back, paramsOf(rb[2])))
	click(t, a, h, clickOn(t, confirm, paramsOf(buttonsOf(confirm)[0])))
	if len(h.presses) != 1 || h.presses[0].Option != "allow-always" || h.presses[0].Caller != "bob@example.com" {
		t.Fatalf("presses = %+v, want one allow-always from the clicker", h.presses)
	}
}

// clickRaw posts one click and returns whether it was answered with an edit,
// and the edits that went over REST.
func clickRaw(t *testing.T, a *Adapter, f *fakeMessenger, h chat.Handler, body string) (edited bool) {
	t.Helper()
	r := postEvent(body)
	r.Header.Set("Authorization", "Bearer good-token")
	rec := httptest.NewRecorder()
	var turns turnGroup
	a.eventHandler(context.Background(), &turns, h).ServeHTTP(rec, r)
	turns.Wait()
	_, ok := clickEnvelope(t, rec.Body.Bytes())
	return ok || len(f.patches) > 0
}

// A second press on the Broad answer while the card is already its
// confirmation — a double tap, or two people at once, with Chat echoing the
// card as it now is — must leave the confirmation as it is. Built again from
// itself, it read its own Yes as the only answer, and Back then restored a
// question with no buttons at all.
func TestADoubleTapOnABroadAnswerChangesNothing(t *testing.T) {
	f := &fakeMessenger{}
	a := newIngressAdapter(t, f)
	a.cards = CardsStatus
	h := &pressHandler{a: a}
	question := decisionCard("q", confirmDecision, testAudience)
	confirm := click(t, a, h, clickOn(t, question, paramsOf(buttonsOf(question)[2])))
	f.patches = nil

	if clickRaw(t, a, f, h, clickOn(t, confirm, paramsOf(buttonsOf(question)[2]))) {
		t.Errorf("a repeated confirm press rewrote the confirmation")
	}
	if len(h.presses) != 0 {
		t.Errorf("a repeated confirm press reached the router: %+v", h.presses)
	}
}

// A stage click on a card the router has already settled — no buttons left
// for the decision — must not rewrite it: live buttons would come back over
// the record of who decided, and pressing them could find nothing pending.
func TestAStaleStageClickLeavesASettledCardAlone(t *testing.T) {
	f := &fakeMessenger{}
	a := newIngressAdapter(t, f)
	a.cards = CardsStatus
	h := &pressHandler{a: a}
	question := decisionCard("q", confirmDecision, testAudience)
	confirm := click(t, a, h, clickOn(t, question, paramsOf(buttonsOf(question)[2])))
	back := paramsOf(buttonsOf(confirm)[1])
	settled := decisionCard("q\n\n✅ Allowed and saved — ana@example.com", nil, testAudience)
	f.patches = nil

	for _, p := range []map[string]string{back, paramsOf(buttonsOf(question)[2])} {
		if clickRaw(t, a, f, h, clickOn(t, settled, p)) {
			t.Errorf("a stale %s click rewrote a settled card", p[paramStage])
		}
	}
}

// A narrow answer is still one press.
func TestANarrowAnswerIsOnePress(t *testing.T) {
	f := &fakeMessenger{}
	a := newIngressAdapter(t, f)
	a.cards = CardsStatus
	h := &pressHandler{a: a}
	question := decisionCard("Allow bash?", confirmDecision, testAudience)
	click(t, a, h, clickOn(t, question, paramsOf(buttonsOf(question)[1])))
	if len(h.presses) != 1 || h.presses[0].Option != "allow-once" {
		t.Fatalf("presses = %+v, want allow-once at once", h.presses)
	}
}

// Without the hosting card the question cannot be restored, so the
// confirmation offers Yes alone — still a second press naming the grant.
func TestAConfirmationWithoutTheHostingCardOffersYesOnly(t *testing.T) {
	card, _ := confirmCard(nil, confirmDecision.ID, "allow-always", testAudience)
	b := buttonsOf(card)
	if len(b) != 1 || b[0].Text != "Yes, allow" {
		t.Fatalf("buttons = %+v, want Yes alone", b)
	}
	if !strings.Contains(cardText(card), "<b>allow-always</b>"+confirmSuffix) {
		t.Errorf("confirmation does not name the grant: %s", cardText(card))
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
