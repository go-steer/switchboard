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
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/go-steer/switchboard/pkg/chat"
)

// podTable is shaped like the answer that lost its footer on the GKE
// deployment: a heading over a wide table whose aligned-column rendering
// pushes the text form past chatTextLimit, while the whole answer is a small
// card.
func podTable(rows int) string {
	var b strings.Builder
	b.WriteString("## Pods in `std-simian-test`\n\n| Namespace | Pod | Status | Restarts | Age |\n| :--- | :--- | :--- | ---: | :--- |\n")
	for i := 0; i < rows; i++ {
		fmt.Fprintf(&b, "| `kube-system` | `pdcsi-node-%05d-abcde` | Running | %d | 12d |\n", i, i%3)
	}
	return b.String()
}

func TestAnAnswerSentAsOneCardFitsOneMessage(t *testing.T) {
	text := podTable(70)
	if n := len(toChatText(text)); n <= chatTextLimit {
		t.Fatalf("fixture renders to %d bytes of text, want past chatTextLimit (%d) to exercise the card path", n, chatTextLimit)
	}
	if answerCard(text) == nil {
		t.Fatal("fixture does not render as one card")
	}
	rich := &Adapter{cards: CardsRich}
	if !rich.FitsOneMessage(text) {
		t.Error("an answer posted as one card was reported as split; its usage footer would be skipped")
	}
	// Without rich cards the same answer goes out as split text.
	for _, mode := range []CardMode{CardsOff, CardsStatus} {
		if (&Adapter{cards: mode}).FitsOneMessage(text) {
			t.Errorf("cards=%v: reported as one message, but it is sent as split text", mode)
		}
	}
	// A card too big for one message still goes as text, and does not fit.
	if huge := podTable(600); rich.FitsOneMessage(huge) {
		t.Error("an answer past the card ceiling was reported as one message")
	}
}

// A card Chat rejects on an edit falls back to text — but only when the text
// fits one message. A long answer (or ingress timeline) clamped into one would
// overwrite the message with a cut-off copy: for an answer whose card was
// already rejected at Send, the head of text the following messages repeat.
// The edit is refused instead and the message left alone (caught in review).
func TestARejectedCardEditDoesNotClampALongTextIntoOneMessage(t *testing.T) {
	f := &fakeMessenger{cardErr: apiErr(http.StatusBadRequest)}
	a := newTestAdapter(f)
	a.cards = CardsRich
	ref := chat.MessageRef{Conversation: "spaces/AAA:spaces/AAA/threads/T1", ID: "spaces/AAA/messages/M1"}

	long := chat.Reply{Conversation: ref.Conversation, Text: podTable(70), Usage: &chat.Usage{TokensIn: 10, TokensOut: 2}}
	if err := a.Update(context.Background(), ref, long); err == nil {
		t.Error("the edit was reported as done")
	}
	if len(f.patches) != 0 {
		t.Errorf("patched %d time(s) with %q; want the message left alone", len(f.patches), f.patches[0].text)
	}

	// A short one still falls back to text, as before.
	short := chat.Reply{Conversation: ref.Conversation, Text: "# Done\n\nall green", Usage: &chat.Usage{TokensIn: 10, TokensOut: 2}}
	if err := a.Update(context.Background(), ref, short); err != nil {
		t.Fatal(err)
	}
	if len(f.patches) != 1 || f.patches[0].card != nil {
		t.Errorf("patches = %+v, want the text fallback", f.patches)
	}
}
