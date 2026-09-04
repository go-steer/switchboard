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
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/pubsub"
	chatv1 "google.golang.org/api/chat/v1"
	"google.golang.org/api/googleapi"

	"github.com/go-steer/switchboard/pkg/chat"
)

// fakeMessenger records egress calls for the Send/Update/Delete tests.
type fakeMessenger struct {
	creates []createCall
	patches []patchCall
	deletes []string
	n       int
	// createErr fails every create; cardErr fails only the ones carrying a
	// card, which is how the card-rejection fallback is exercised.
	createErr error
	cardErr   error
	patchErr  error
	// assignThread, when set, is the thread the fake reports each created
	// message landed in — simulating Chat assigning a thread in a flat space.
	assignThread string
}

type createCall struct {
	parent, text, thread, fallback string
	card                           *chatv1.GoogleAppsCardV1Card
}

type patchCall struct {
	name, text, mask string
	card             *chatv1.GoogleAppsCardV1Card
}

func cardOf(msg *chatv1.Message) *chatv1.GoogleAppsCardV1Card {
	if len(msg.CardsV2) == 0 {
		return nil
	}
	return msg.CardsV2[0].Card
}

func (f *fakeMessenger) create(_ context.Context, parent string, msg *chatv1.Message) (*chatv1.Message, error) {
	card := cardOf(msg)
	if card != nil && f.cardErr != nil {
		return nil, f.cardErr
	}
	if f.createErr != nil {
		return nil, f.createErr
	}
	thread := ""
	if msg.Thread != nil {
		thread = msg.Thread.Name
	}
	f.creates = append(f.creates, createCall{
		parent: parent, text: msg.Text, thread: thread, fallback: msg.FallbackText, card: card,
	})
	f.n++
	out := &chatv1.Message{Name: fmt.Sprintf("spaces/AAA/messages/M%d", f.n)}
	if f.assignThread != "" {
		out.Thread = &chatv1.Thread{Name: f.assignThread}
	}
	return out, nil
}

func (f *fakeMessenger) patch(_ context.Context, name string, msg *chatv1.Message, mask string) error {
	card := cardOf(msg)
	if card != nil && f.cardErr != nil {
		return f.cardErr
	}
	if f.patchErr != nil {
		return f.patchErr
	}
	f.patches = append(f.patches, patchCall{name: name, text: msg.Text, mask: mask, card: card})
	return nil
}

func (f *fakeMessenger) delete(_ context.Context, name string) error {
	f.deletes = append(f.deletes, name)
	return nil
}

// newTestAdapter builds an adapter with cards off — the plain-text baseline
// most egress tests care about. Card behaviour is opted into per test.
func newTestAdapter(m messenger) *Adapter {
	return &Adapter{msg: m, cards: CardsOff}
}

func apiErr(code int) error {
	return &googleapi.Error{Code: code, Message: http.StatusText(code)}
}

func TestSendThreadsAndReturnsFirstRef(t *testing.T) {
	f := &fakeMessenger{}
	a := newTestAdapter(f)
	ref, err := a.Send(context.Background(), chat.Reply{
		Conversation: "spaces/AAA:spaces/AAA/threads/T1",
		Text:         "hello",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(f.creates) != 1 {
		t.Fatalf("want 1 create, got %d", len(f.creates))
	}
	c := f.creates[0]
	if c.parent != "spaces/AAA" || c.thread != "spaces/AAA/threads/T1" || c.text != "hello" {
		t.Fatalf("unexpected create %+v", c)
	}
	if ref.ID != "spaces/AAA/messages/M1" || ref.Conversation != "spaces/AAA:spaces/AAA/threads/T1" {
		t.Fatalf("unexpected ref %+v", ref)
	}
}

// TestSendRendersChatMarkup checks the always-on baseline translates a model
// turn's markdown into Chat's dialect rather than posting the delimiters raw.
func TestSendRendersChatMarkup(t *testing.T) {
	f := &fakeMessenger{}
	a := newTestAdapter(f)
	if _, err := a.Send(context.Background(), chat.Reply{
		Conversation: "spaces/AAA:t",
		Text:         "**bold** and [docs](https://example.com)",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := f.creates[0].text; got != "*bold* and <https://example.com|docs>" {
		t.Fatalf("text = %q", got)
	}
}

func TestSendChunksLongReply(t *testing.T) {
	f := &fakeMessenger{}
	a := newTestAdapter(f)
	// Build a reply longer than the limit with newline break points.
	line := strings.Repeat("x", 3000) + "\n"
	body := line + line // 6002 bytes, one newline mid-way under 4096
	ref, err := a.Send(context.Background(), chat.Reply{
		Conversation: "spaces/AAA:spaces/AAA/threads/T1",
		Text:         body,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(f.creates) < 2 {
		t.Fatalf("expected reply to be split into >=2 messages, got %d", len(f.creates))
	}
	var parts []string
	for _, c := range f.creates {
		if len(c.text) > chatTextLimit {
			t.Fatalf("chunk exceeds limit: %d bytes", len(c.text))
		}
		if c.thread != "spaces/AAA/threads/T1" {
			t.Fatalf("every chunk must target the thread, got %q", c.thread)
		}
		parts = append(parts, c.text)
	}
	// Rejoined on the boundary newline the split was taken at.
	if strings.Join(parts, "\n") != strings.TrimSpace(body) {
		t.Fatalf("chunks do not reassemble to the reply")
	}
	if ref.ID != "spaces/AAA/messages/M1" {
		t.Fatalf("ref should be the first message, got %q", ref.ID)
	}
}

func TestSendFlatSpaceAdoptsThread(t *testing.T) {
	// A chunked reply in a flat space (no incoming thread) should adopt the
	// thread the first message landed in so later chunks stay together.
	f := &fakeMessenger{assignThread: "spaces/AAA/threads/assigned"}
	a := newTestAdapter(f)
	line := strings.Repeat("y", 3000) + "\n"
	if _, err := a.Send(context.Background(), chat.Reply{
		Conversation: "spaces/AAA:", // no thread
		Text:         line + line,
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(f.creates) < 2 {
		t.Fatalf("expected >=2 creates, got %d", len(f.creates))
	}
	if f.creates[0].thread != "" {
		t.Fatalf("first chunk should be top-level, got thread %q", f.creates[0].thread)
	}
	for i, c := range f.creates[1:] {
		if c.thread != "spaces/AAA/threads/assigned" {
			t.Fatalf("chunk %d should adopt the assigned thread, got %q", i+1, c.thread)
		}
	}
}

// TestSendRefNamesTheThreadItLandedIn is the ref half of #39: a post into a
// bare space is assigned a thread by Chat, and the returned ref has to name it.
// The outbound ingress builds the continuation of an overflowing message from
// ref.Conversation, so a ref that still says only "spaces/AAA" sent it down
// Slack's "the id is the thread" path and produced
// spaces/AAA:spaces/AAA/messages/CCC — a message resource name in a thread
// field.
func TestSendRefNamesTheThreadItLandedIn(t *testing.T) {
	const assigned = "spaces/AAA/threads/assigned"
	for _, tc := range []struct {
		name         string
		conv         string
		assignThread string
		cards        CardMode
		kind         chat.ReplyKind
		want         string
	}{
		{
			name:         "a bare space adopts the assigned thread",
			conv:         "spaces/AAA",
			assignThread: assigned,
			want:         "spaces/AAA:" + assigned,
		},
		{
			// The same conversation written the way conversationKey renders an
			// unthreaded space. The colon is not a thread, so this must adopt
			// too — reading the key for a separator rather than for a thread is
			// how the bug looked from the ingress side.
			name:         "a trailing colon is not a thread",
			conv:         "spaces/AAA:",
			assignThread: assigned,
			want:         "spaces/AAA:" + assigned,
		},
		{
			// The caller's thread is the more specific of the two, and Chat
			// cannot have moved the message out of it.
			name:         "an explicit thread is kept",
			conv:         "spaces/AAA:spaces/AAA/threads/T1",
			assignThread: assigned,
			want:         "spaces/AAA:spaces/AAA/threads/T1",
		},
		{
			// Not expected from Chat, but the key is not invented from nothing.
			name: "no thread reported leaves the key alone",
			conv: "spaces/AAA",
			want: "spaces/AAA",
		},
		{
			// The card path returns its own ref, so it needs the same treatment
			// — and a gateway card is what an ingress-posted notice renders as.
			name:         "the card path names it too",
			conv:         "spaces/AAA",
			assignThread: assigned,
			cards:        CardsStatus,
			kind:         chat.KindNotice,
			want:         "spaces/AAA:" + assigned,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeMessenger{assignThread: tc.assignThread}
			a := newTestAdapter(f)
			if tc.cards != "" {
				a.cards = tc.cards
			}
			ref, err := a.Send(context.Background(), chat.Reply{
				Conversation: tc.conv, Kind: tc.kind, Text: "hello",
			})
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			if tc.cards != "" && (len(f.creates) != 1 || f.creates[0].card == nil) {
				t.Fatalf("wanted the card path, got %+v", f.creates)
			}
			if ref.Conversation != tc.want {
				t.Errorf("ref.Conversation = %q, want %q", ref.Conversation, tc.want)
			}
			if ref.ID != "spaces/AAA/messages/M1" {
				t.Errorf("ref.ID = %q, want the created message", ref.ID)
			}
		})
	}
}

func TestSendTopLevelWhenNoThread(t *testing.T) {
	f := &fakeMessenger{}
	a := newTestAdapter(f)
	if _, err := a.Send(context.Background(), chat.Reply{
		Conversation: "spaces/AAA:",
		Text:         "hi",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(f.creates) != 1 || f.creates[0].thread != "" {
		t.Fatalf("expected a single top-level (no thread) create, got %+v", f.creates)
	}
}

func TestSendEmptyPostsNothing(t *testing.T) {
	f := &fakeMessenger{}
	a := newTestAdapter(f)
	ref, err := a.Send(context.Background(), chat.Reply{Conversation: "spaces/AAA:t", Text: "   "})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(f.creates) != 0 || ref.ID != "" {
		t.Fatalf("empty reply should post nothing, got creates=%d ref=%+v", len(f.creates), ref)
	}
}

func TestSendMalformedConversation(t *testing.T) {
	f := &fakeMessenger{}
	a := newTestAdapter(f)
	if _, err := a.Send(context.Background(), chat.Reply{Conversation: ":thread-only", Text: "hi"}); err == nil {
		t.Fatalf("expected error for a conversation key with no space")
	}
}

// A bare space is a legal conversation on the ingress API — the same call that
// works for Slack has to work here, without a magic trailing colon.
func TestSendBareSpacePostsTopLevel(t *testing.T) {
	f := &fakeMessenger{}
	a := newTestAdapter(f)
	ref, err := a.Send(context.Background(), chat.Reply{Conversation: "spaces/AAA", Text: "hi"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(f.creates) != 1 || f.creates[0].parent != "spaces/AAA" || f.creates[0].thread != "" {
		t.Fatalf("unexpected creates %+v", f.creates)
	}
	if ref.Conversation != "spaces/AAA" {
		t.Fatalf("ref conversation = %q, want the key it was sent with", ref.Conversation)
	}
}

func TestSendPropagatesCreateError(t *testing.T) {
	f := &fakeMessenger{createErr: errors.New("boom")}
	a := newTestAdapter(f)
	if _, err := a.Send(context.Background(), chat.Reply{Conversation: "spaces/AAA:t", Text: "hi"}); err == nil {
		t.Fatalf("expected create error to propagate")
	}
}

// TestSendGatewayCard checks a router-classified reply renders as a card with
// the plain text kept as the fallback — never both as visible content.
func TestSendGatewayCard(t *testing.T) {
	f := &fakeMessenger{}
	a := newTestAdapter(f)
	a.cards = CardsStatus
	if _, err := a.Send(context.Background(), chat.Reply{
		Conversation: "spaces/AAA:t",
		Text:         "⏳ Working…",
		Kind:         chat.KindProgress,
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	c := f.creates[0]
	if c.card == nil {
		t.Fatalf("a progress reply should render as a card in status mode")
	}
	if c.text != "" {
		t.Fatalf("a card message must not also carry visible text, got %q", c.text)
	}
	if c.fallback != "⏳ Working…" {
		t.Fatalf("fallback = %q", c.fallback)
	}
}

// TestSendAnswerIsTextUntilRichMode: the default renders the gateway's own
// messages as cards but leaves model output as text.
func TestSendAnswerCardsOnlyInRichMode(t *testing.T) {
	const answer = "# Findings\n\nsomething\n\n## More\n\ndetail\n"
	for _, tt := range []struct {
		mode     CardMode
		wantCard bool
	}{
		{CardsOff, false},
		{CardsStatus, false},
		{CardsRich, true},
	} {
		f := &fakeMessenger{}
		a := newTestAdapter(f)
		a.cards = tt.mode
		if _, err := a.Send(context.Background(), chat.Reply{Conversation: "spaces/AAA:t", Text: answer}); err != nil {
			t.Fatalf("%s: Send: %v", tt.mode, err)
		}
		if got := f.creates[0].card != nil; got != tt.wantCard {
			t.Fatalf("%s: card = %v, want %v", tt.mode, got, tt.wantCard)
		}
	}
}

// TestSendLongAnswerInRichMode walks the whole egress path for the mode that is
// now the default: a long fenced answer, sent, and observed as Chat would see
// it. answerCard's own tests check the card in isolation; this one checks what
// Send actually hands the API — one message while the card fits, and the text
// path once it does not.
func TestSendLongAnswerInRichMode(t *testing.T) {
	body := func(lines int) string {
		var b strings.Builder
		b.WriteString("# Deploy\n\nRun it:\n\n```hcl\n")
		for i := 0; i < lines; i++ {
			fmt.Fprintf(&b, "resource \"google_project_service\" \"svc_%04d\" { service = \"x\" }\n", i)
		}
		b.WriteString("```\n")
		return b.String()
	}

	// Inside the ceiling: one card message, spilled across widgets, complete.
	f := &fakeMessenger{}
	a := newTestAdapter(f)
	a.cards = CardsRich
	if _, err := a.Send(context.Background(), chat.Reply{
		Conversation: "spaces/AAA:spaces/AAA/threads/T1",
		Text:         body(200),
		Kind:         chat.KindAnswer,
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(f.creates) != 1 || f.creates[0].card == nil {
		t.Fatalf("want one card message, got %+v", f.creates)
	}
	var widgets, size int
	for _, s := range f.creates[0].card.Sections {
		for _, w := range s.Widgets {
			if w.TextParagraph != nil {
				widgets++
				size += len(w.TextParagraph.Text)
				if strings.Contains(w.TextParagraph.Text, "…") {
					t.Errorf("widget was truncated: %q", w.TextParagraph.Text)
				}
			}
		}
	}
	if widgets < 2 {
		t.Errorf("want the answer spilled across widgets, got %d", widgets)
	}
	if size < len(body(200))/2 {
		t.Errorf("card carries %d bytes of a %d-byte answer", size, len(body(200)))
	}

	// Past it: no card, and the answer arrives whole across several messages
	// rather than costing a rejected write on the way there.
	f = &fakeMessenger{}
	a = newTestAdapter(f)
	a.cards = CardsRich
	if _, err := a.Send(context.Background(), chat.Reply{
		Conversation: "spaces/AAA:spaces/AAA/threads/T1",
		Text:         body(2000),
		Kind:         chat.KindAnswer,
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(f.creates) < 2 {
		t.Fatalf("want the oversize answer split across messages, got %d", len(f.creates))
	}
	for _, c := range f.creates {
		if c.card != nil {
			t.Errorf("an oversize answer must not attempt a card: %+v", c)
		}
	}
	var joined strings.Builder
	for _, c := range f.creates {
		joined.WriteString(c.text)
	}
	for _, want := range []string{"svc_0000", "svc_1000", "svc_1999"} {
		if !strings.Contains(joined.String(), want) {
			t.Errorf("%q was lost on the way to the text fallback", want)
		}
	}
}

// TestSendFallsBackWhenChatRejectsACard is the rule that a rich render must
// never cost a reply.
func TestSendFallsBackWhenChatRejectsACard(t *testing.T) {
	f := &fakeMessenger{cardErr: apiErr(http.StatusBadRequest)}
	a := newTestAdapter(f)
	a.cards = CardsStatus
	ref, err := a.Send(context.Background(), chat.Reply{
		Conversation: "spaces/AAA:t",
		Text:         "⚠️ That turn didn't go through.",
		Kind:         chat.KindNotice,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(f.creates) != 1 || f.creates[0].card != nil {
		t.Fatalf("expected one text create after the card was rejected, got %+v", f.creates)
	}
	if f.creates[0].text == "" || ref.ID == "" {
		t.Fatalf("the reply must survive as text: %+v", f.creates[0])
	}
}

// TestSendDoesNotRetryANonCardFailure: a 403 would fail the text post too, so
// retrying just doubles the damage.
func TestSendDoesNotRetryANonCardFailure(t *testing.T) {
	f := &fakeMessenger{cardErr: apiErr(http.StatusForbidden)}
	a := newTestAdapter(f)
	a.cards = CardsStatus
	_, err := a.Send(context.Background(), chat.Reply{
		Conversation: "spaces/AAA:t", Text: "nope", Kind: chat.KindNotice,
	})
	if err == nil {
		t.Fatalf("expected the error to propagate")
	}
	if !errors.Is(err, chat.ErrDenied) {
		t.Fatalf("a 403 should classify as chat.ErrDenied, got %v", err)
	}
	if len(f.creates) != 0 {
		t.Fatalf("must not retry as text, got %+v", f.creates)
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		code int
		want error
	}{
		{http.StatusNotFound, chat.ErrNotFound},
		{http.StatusGone, chat.ErrNotFound},
		{http.StatusForbidden, chat.ErrDenied},
		{http.StatusUnauthorized, chat.ErrDenied},
		{http.StatusTooManyRequests, nil},
		{http.StatusInternalServerError, nil},
	}
	for _, tt := range tests {
		got := platformErr(apiErr(tt.code))
		if tt.want == nil {
			if errors.Is(got, chat.ErrNotFound) || errors.Is(got, chat.ErrDenied) {
				t.Fatalf("%d should stay unclassified (retryable), got %v", tt.code, got)
			}
			continue
		}
		if !errors.Is(got, tt.want) {
			t.Fatalf("%d should classify as %v, got %v", tt.code, tt.want, got)
		}
	}
	// An unclassified error reads exactly like the original.
	plain := errors.New("boom")
	if platformErr(plain) != plain {
		t.Fatalf("an unclassifiable error should pass through unchanged")
	}
}

func TestUpdateAndDelete(t *testing.T) {
	f := &fakeMessenger{}
	a := newTestAdapter(f)

	// Zero refs no-op without touching the service.
	if err := a.Update(context.Background(), chat.MessageRef{}, chat.Reply{Text: "x"}); err != nil {
		t.Fatalf("Update zero ref: %v", err)
	}
	if err := a.Delete(context.Background(), chat.MessageRef{}); err != nil {
		t.Fatalf("Delete zero ref: %v", err)
	}
	if len(f.patches) != 0 || len(f.deletes) != 0 {
		t.Fatalf("zero refs must not call the service")
	}

	ref := chat.MessageRef{Conversation: "spaces/AAA:t", ID: "spaces/AAA/messages/M1"}
	if err := a.Update(context.Background(), ref, chat.Reply{Text: " working "}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(f.patches) != 1 || f.patches[0].name != ref.ID || f.patches[0].text != "working" {
		t.Fatalf("unexpected patch %+v", f.patches)
	}
	// The mask has to name every field the patch could be replacing, or a
	// message that used to carry a card would keep it alongside the new text.
	if f.patches[0].mask != patchMask {
		t.Fatalf("update mask %q, want %q", f.patches[0].mask, patchMask)
	}
	if err := a.Delete(context.Background(), ref); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(f.deletes) != 1 || f.deletes[0] != ref.ID {
		t.Fatalf("unexpected delete %+v", f.deletes)
	}
}

func TestUpdateToCard(t *testing.T) {
	f := &fakeMessenger{}
	a := newTestAdapter(f)
	a.cards = CardsStatus
	ref := chat.MessageRef{Conversation: "spaces/AAA:t", ID: "spaces/AAA/messages/M1"}
	if err := a.Update(context.Background(), ref, chat.Reply{
		Text: "🔧 Running `bash`", Kind: chat.KindActivity,
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(f.patches) != 1 || f.patches[0].card == nil {
		t.Fatalf("expected a card patch, got %+v", f.patches)
	}
	if f.patches[0].text != "" {
		t.Fatalf("a card patch must not also set visible text, got %q", f.patches[0].text)
	}
}

// Chat accepts only a fixed set of paths in a messages.patch update mask, and
// rejects the entire request with 400 if the mask names anything else. Naming
// fallbackText — a real field on Message, but not an updatable one — broke
// every in-place edit in all three card modes, so pin the mask against the
// documented set rather than against a shape that merely looks plausible.
func TestPatchMaskNamesOnlyUpdatablePaths(t *testing.T) {
	// google.golang.org/api/chat/v1, SpacesMessagesPatchCall.UpdateMask.
	updatable := map[string]bool{
		"text": true, "attachment": true, "cards": true, "cardsV2": true,
		"accessoryWidgets": true, "quotedMessageMetadata": true,
	}

	f := &fakeMessenger{}
	a := newTestAdapter(f)
	a.cards = CardsStatus
	ref := chat.MessageRef{Conversation: "spaces/AAA:t", ID: "spaces/AAA/messages/M1"}

	// Both arms of rewrite: the card patch and the plain-text patch.
	if err := a.Update(context.Background(), ref, chat.Reply{Text: "x", Kind: chat.KindActivity}); err != nil {
		t.Fatalf("Update to card: %v", err)
	}
	if err := a.Update(context.Background(), ref, chat.Reply{Text: "x"}); err != nil {
		t.Fatalf("Update to text: %v", err)
	}
	if len(f.patches) != 2 {
		t.Fatalf("expected two patches, got %d", len(f.patches))
	}
	for _, p := range f.patches {
		for _, path := range strings.Split(p.mask, ",") {
			if !updatable[path] {
				t.Fatalf("mask %q names %q, which Chat does not accept in an update mask", p.mask, path)
			}
		}
	}
}

// fakeHandler records the turns, commands and presses dispatch routes to the
// router.
type fakeHandler struct {
	msgs    []chat.Message
	cmds    []chat.Command
	presses []chat.Press
	ack     string
	err     error
	choices []string
}

func (h *fakeHandler) Handle(_ context.Context, m chat.Message) error {
	h.msgs = append(h.msgs, m)
	return nil
}

func (h *fakeHandler) HandleCommand(_ context.Context, c chat.Command) (string, error) {
	h.cmds = append(h.cmds, c)
	return h.ack, h.err
}

// HandlePress is here to satisfy chat.Handler. This adapter has no interactive
// surface yet (#29), so nothing in this package should ever call it — which is
// what the recording is for.
func (h *fakeHandler) HandlePress(_ context.Context, p chat.Press) error {
	h.presses = append(h.presses, p)
	return h.err
}

// choiceHandler adds the optional chat.CommandChoices capability, which is how
// the welcome — card and text fallback both — names the progress modes without
// this package knowing them.
type choiceHandler struct{ fakeHandler }

func (h *choiceHandler) Choices(name string) []string {
	if name != "progress" {
		return nil
	}
	return h.choices
}

func TestDispatchRoutesCommandToHandleCommand(t *testing.T) {
	f := &fakeMessenger{}
	h := &fakeHandler{ack: "progress mode set to status"}
	a := newTestAdapter(f)

	payload := []byte(`{
		"chat": {
			"user": {"name": "users/5"},
			"space": {"name": "spaces/AAA"},
			"appCommandPayload": {
				"appCommandMetadata": {"appCommandId": "1", "appCommandType": "SLASH_COMMAND"},
				"message": {"argumentText": "progress status", "thread": {"name": "spaces/AAA/threads/T1"}}
			}
		}
	}`)
	a.dispatch(context.Background(), h, &pubsub.Message{Data: payload})

	if len(h.cmds) != 1 {
		t.Fatalf("want 1 command, got %d", len(h.cmds))
	}
	if len(h.msgs) != 0 {
		t.Fatalf("command must not become an agent turn: %+v", h.msgs)
	}
	cmd := h.cmds[0]
	if cmd.Name != "progress" || cmd.Channel != "spaces/AAA" || cmd.Caller != "users/5" {
		t.Fatalf("unexpected command %+v", cmd)
	}
	if strings.Join(cmd.Args, ",") != "status" {
		t.Fatalf("unexpected args %v", cmd.Args)
	}
	// The ack posts back into the invoking thread.
	if len(f.creates) != 1 {
		t.Fatalf("want 1 ack post, got %d", len(f.creates))
	}
	c := f.creates[0]
	if c.parent != "spaces/AAA" || c.thread != "spaces/AAA/threads/T1" || c.text != "progress mode set to status" {
		t.Fatalf("unexpected ack post %+v", c)
	}
}

// TestDispatchMapsConfiguredCommandID: Chat identifies a command by the numeric
// ID configured in the console, so a dedicated /progress command carries its
// argument alone and the verb comes from the mapping.
func TestDispatchMapsConfiguredCommandID(t *testing.T) {
	f := &fakeMessenger{}
	h := &fakeHandler{ack: "ok"}
	a := newTestAdapter(f)
	a.cmds = map[int64]string{2: "progress"}

	payload := []byte(`{
		"chat": {
			"space": {"name": "spaces/AAA"},
			"appCommandPayload": {
				"appCommandMetadata": {"appCommandId": "2", "appCommandType": "SLASH_COMMAND"},
				"message": {"argumentText": "stream"}
			}
		}
	}`)
	a.dispatch(context.Background(), h, &pubsub.Message{Data: payload})

	if len(h.cmds) != 1 {
		t.Fatalf("want 1 command, got %d", len(h.cmds))
	}
	if h.cmds[0].Name != "progress" || strings.Join(h.cmds[0].Args, ",") != "stream" {
		t.Fatalf("unexpected command %+v", h.cmds[0])
	}
}

// TestDispatchQuickCommandCarriesNoText: a quick command has no message at all,
// so the mapping is the only thing that can name it.
func TestDispatchQuickCommand(t *testing.T) {
	f := &fakeMessenger{}
	h := &fakeHandler{ack: "ok"}
	a := newTestAdapter(f)
	a.cmds = map[int64]string{9: "progress"}

	payload := []byte(`{
		"chat": {
			"space": {"name": "spaces/AAA"},
			"appCommandPayload": {"appCommandMetadata": {"appCommandId": 9, "appCommandType": "QUICK_COMMAND"}}
		}
	}`)
	a.dispatch(context.Background(), h, &pubsub.Message{Data: payload})

	if len(h.cmds) != 1 || h.cmds[0].Name != "progress" || len(h.cmds[0].Args) != 0 {
		t.Fatalf("unexpected commands %+v", h.cmds)
	}
}

func TestDispatchEmptyAckPostsNothing(t *testing.T) {
	f := &fakeMessenger{}
	h := &fakeHandler{ack: ""}
	a := newTestAdapter(f)

	payload := []byte(`{
		"chat": {
			"space": {"name": "spaces/AAA"},
			"appCommandPayload": {
				"appCommandMetadata": {"appCommandId": "1"},
				"message": {"argumentText": "progress"}
			}
		}
	}`)
	a.dispatch(context.Background(), h, &pubsub.Message{Data: payload})

	if len(h.cmds) != 1 {
		t.Fatalf("want 1 command, got %d", len(h.cmds))
	}
	if len(f.creates) != 0 {
		t.Fatalf("empty ack must post nothing, got %+v", f.creates)
	}
}

func TestDispatchRoutesMessageToHandle(t *testing.T) {
	f := &fakeMessenger{}
	h := &fakeHandler{}
	a := newTestAdapter(f)

	payload := []byte(`{
		"chat": {
			"user": {"name": "users/7"},
			"space": {"name": "spaces/AAA"},
			"messagePayload": {
				"message": {"argumentText": "hello there", "thread": {"name": "spaces/AAA/threads/T1"}}
			}
		}
	}`)
	a.dispatch(context.Background(), h, &pubsub.Message{Data: payload})

	if len(h.msgs) != 1 || len(h.cmds) != 0 {
		t.Fatalf("want 1 turn and no command, got msgs=%+v cmds=%+v", h.msgs, h.cmds)
	}
	got := h.msgs[0]
	if got.Text != "hello there" || got.Caller != "users/7" || got.Channel != "spaces/AAA" ||
		got.Conversation != "spaces/AAA:spaces/AAA/threads/T1" {
		t.Fatalf("unexpected turn %+v", got)
	}
}

// TestDispatchCallerMode pins which identity reaches the daemon. It matters
// operationally, not cosmetically: core-agent rejects a turn outright when the
// asserted caller is not provisioned, so an adapter that quietly switched forms
// would take the gateway down for every user at once.
func TestDispatchCallerMode(t *testing.T) {
	const withEmail = `{
		"chat": {
			"user": {"name": "users/7", "email": "ada@example.com"},
			"space": {"name": "spaces/AAA"},
			"messagePayload": {"message": {"argumentText": "hello there"}}
		}
	}`
	const withoutEmail = `{
		"chat": {
			"user": {"name": "users/7"},
			"space": {"name": "spaces/AAA"},
			"messagePayload": {"message": {"argumentText": "hello there"}}
		}
	}`

	tests := []struct {
		name    string
		mode    chat.CallerMode
		payload string
		want    string
	}{
		{"email is the default", chat.CallerEmail, withEmail, "ada@example.com"},
		{"id opts out of the lookup", chat.CallerID, withEmail, "users/7"},
		{"no email degrades to the resource name", chat.CallerEmail, withoutEmail, "users/7"},
		{"id is unaffected by a missing email", chat.CallerID, withoutEmail, "users/7"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &fakeHandler{}
			a := newTestAdapter(&fakeMessenger{})
			a.caller = tt.mode
			a.dispatch(context.Background(), h, &pubsub.Message{Data: []byte(tt.payload)})

			if len(h.msgs) != 1 {
				t.Fatalf("want 1 turn, got %+v", h.msgs)
			}
			if h.msgs[0].Caller != tt.want {
				t.Fatalf("caller = %q, want %q", h.msgs[0].Caller, tt.want)
			}
		})
	}
}

// TestDispatchCallerModeAppliesToCommands: a command is attributed too, and the
// gateway would look inconsistent — one identity for turns, another for the
// /progress that configures them — if the rewrite lived on the message path.
func TestDispatchCallerModeAppliesToCommands(t *testing.T) {
	h := &fakeHandler{ack: "ok"}
	a := newTestAdapter(&fakeMessenger{})
	a.cmds = map[int64]string{1: "progress"}

	payload := []byte(`{
		"chat": {
			"user": {"name": "users/7", "email": "ada@example.com"},
			"space": {"name": "spaces/AAA"},
			"appCommandPayload": {
				"appCommandMetadata": {"appCommandId": 1},
				"message": {"argumentText": "status"}
			}
		}
	}`)
	a.dispatch(context.Background(), h, &pubsub.Message{Data: payload})

	if len(h.cmds) != 1 || h.cmds[0].Caller != "ada@example.com" {
		t.Fatalf("want the command attributed to the email, got %+v", h.cmds)
	}
}

// TestNewDefaultsAndValidatesCallerMode: the zero Config must not silently mean
// "no mode", and a typo in --caller-id must fail at startup rather than assert
// something the daemon has never heard of.
func TestNewDefaultsAndValidatesCallerMode(t *testing.T) {
	if got := (&Adapter{}).callerOf(inbound{caller: "users/7", email: "ada@example.com"}); got != "ada@example.com" {
		t.Fatalf("zero-value mode = %q, want the email", got)
	}
	_, err := New(Config{
		ProjectID: "p", SubscriptionID: "s", CallerMode: chat.CallerMode("nickname"),
	})
	if err == nil || !strings.Contains(err.Error(), "caller mode") {
		t.Fatalf("New with a bogus caller mode: err = %v, want a caller-mode complaint", err)
	}
}

// TestNewRequiresTheInboundPairTogether: receiving needs a project and a
// subscription, and half of that pair is a typo, not a configuration. Neither
// is legal too — that is the outbound-only shape (#23) — but it cannot be
// asserted through New, which then builds a Chat REST service from ADC that a
// hermetic test has no business having; TestRunWithoutASubscriptionIsEgressOnly
// covers what such an adapter does instead.
func TestNewRequiresTheInboundPairTogether(t *testing.T) {
	// Both halves are checked before New reaches any credential, so these
	// return without ADC on the machine running them.
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"project without a subscription", Config{ProjectID: "p"}, "SubscriptionID"},
		{"subscription without a project", Config{SubscriptionID: "s"}, "ProjectID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg)
			if err == nil {
				t.Fatalf("New(%+v) succeeded, want a complaint about the missing half", tc.cfg)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to name %s", err, tc.want)
			}
		})
	}
}

// TestNewWithoutASubscriptionBuildsNoPubSubClient: the point of #23 on Chat is
// that an outbound-only deployment needs no topic, no subscription and no
// roles/pubsub.subscriber. That only holds if New skips pubsub.NewClient
// entirely — build one it will never read and the grant is required again, for
// nothing.
//
// New also constructs the Chat REST service from Application Default
// Credentials, which is what makes this awkward to assert hermetically: with no
// ADC there is no success to inspect. So it is asserted from both sides. Where
// ADC exists, on the client that should not be there; where it does not, on
// which of the two constructors failed — a Pub/Sub error here is the regression
// itself, and says so rather than being skipped past.
func TestNewWithoutASubscriptionBuildsNoPubSubClient(t *testing.T) {
	// An emulator makes pubsub.NewClient succeed without credentials, which
	// would turn the no-ADC half of this below into a false pass.
	t.Setenv("PUBSUB_EMULATOR_HOST", "")

	a, err := New(Config{})
	switch {
	case err == nil:
		if a.ps != nil {
			t.Error("New built a Pub/Sub client for an adapter with no subscription to read")
		}
	case strings.Contains(err.Error(), "pubsub"):
		t.Fatalf("New reached pubsub.NewClient with no subscription configured: %v", err)
	default:
		t.Skipf("no Application Default Credentials for the Chat REST service, "+
			"so New cannot get as far as the client this test is about: %v", err)
	}
}

// TestRunWithoutASubscriptionIsEgressOnly: an adapter built with no
// subscription has nothing to pull from, so Run says so instead of blocking on
// a source that does not exist — while egress, which is a Chat REST call and
// never touches Pub/Sub, keeps working (#23).
func TestRunWithoutASubscriptionIsEgressOnly(t *testing.T) {
	f := &fakeMessenger{}
	a := newTestAdapter(f) // no ps, the shape New leaves when SubscriptionID is empty
	if a.ps != nil {
		t.Fatal("test adapter has a Pub/Sub client; this test asserts the case without one")
	}

	err := a.Run(context.Background(), nil)
	if !errors.Is(err, chat.ErrNoInbound) {
		t.Errorf("Run = %v, want chat.ErrNoInbound", err)
	}

	if _, err := a.Send(context.Background(), chat.Reply{
		Conversation: "spaces/AAA",
		Text:         "digest",
	}); err != nil {
		t.Fatalf("Send on an egress-only adapter: %v", err)
	}
	if len(f.creates) != 1 || f.creates[0].text != "digest" {
		t.Fatalf("want the digest posted, got %+v", f.creates)
	}
}

// TestDispatchAckCarriesNoButtons: a handler that reports choices used to turn
// them into a button row on the ack. A click never reaches this app (#28), so
// the row was a control that could only fail, and the ack is now exactly what
// the handler said — this one names the values itself, the one confirming a
// change does not. Asserted at dispatch level because the regression to catch
// is the wiring growing the row back from CommandChoices, which the builder
// cannot see: gatewayCard is never handed the choices at all.
func TestDispatchAckCarriesNoButtons(t *testing.T) {
	f := &fakeMessenger{}
	h := &choiceHandler{}
	h.ack = "Progress mode for this channel is *off*."
	h.choices = []string{"off", "indicator", "status", "stream"}
	a := newTestAdapter(f)
	a.cards = CardsStatus

	payload := []byte(`{
		"chat": {
			"space": {"name": "spaces/AAA"},
			"appCommandPayload": {
				"appCommandMetadata": {"appCommandId": "1"},
				"message": {"argumentText": "progress"}
			}
		}
	}`)
	a.dispatch(context.Background(), h, &pubsub.Message{Data: payload})

	if len(f.creates) != 1 || f.creates[0].card == nil {
		t.Fatalf("expected an ack card, got %+v", f.creates)
	}
	assertNothingClickable(t, "dispatched ack", f.creates[0].card)
	if got := cardText(f.creates[0].card); !strings.Contains(got, "Progress mode for this channel is") {
		t.Fatalf("the ack card should carry the handler's ack, got %q", got)
	}
}

// TestDispatchButtonClick covers the round trip a click would make if one ever
// arrived: no dialog, no synchronous response — the click runs the command and
// the hosting card is rewritten. Nothing sends this event today, since no card
// the gateway posts has a button (#28); the path is kept, and kept tested, for
// the HTTP interaction endpoint in #29.
func TestDispatchButtonClick(t *testing.T) {
	f := &fakeMessenger{}
	h := &choiceHandler{}
	h.ack = "Progress mode for this channel set to *stream*."
	h.choices = []string{"off", "stream"}
	a := newTestAdapter(f)
	a.cards = CardsStatus

	payload := []byte(`{
		"chat": {
			"user": {"name": "users/9"},
			"space": {"name": "spaces/AAA"},
			"buttonClickedPayload": {
				"message": {"name": "spaces/AAA/messages/M9", "thread": {"name": "spaces/AAA/threads/T1"}}
			}
		},
		"commonEventObject": {
			"parameters": {"switchboard_command": "progress", "switchboard_arg": "stream"}
		}
	}`)
	a.dispatch(context.Background(), h, &pubsub.Message{Data: payload})

	if len(h.cmds) != 1 {
		t.Fatalf("want 1 command, got %d", len(h.cmds))
	}
	cmd := h.cmds[0]
	if cmd.Name != "progress" || strings.Join(cmd.Args, ",") != "stream" || cmd.Caller != "users/9" {
		t.Fatalf("a click should run exactly what typing it would: %+v", cmd)
	}
	if len(f.creates) != 0 {
		t.Fatalf("a click answers by patching, not by posting again: %+v", f.creates)
	}
	if len(f.patches) != 1 || f.patches[0].name != "spaces/AAA/messages/M9" || f.patches[0].card == nil {
		t.Fatalf("unexpected patch %+v", f.patches)
	}
}

// TestDispatchButtonClickIsIdempotent: an at-least-once ingress may deliver the
// same click twice — Pub/Sub redelivery, or a retried POST under #29 — so
// handling it twice must leave the same card rather than compounding.
func TestDispatchButtonClickIsIdempotent(t *testing.T) {
	f := &fakeMessenger{}
	h := &choiceHandler{}
	h.ack = "Progress mode for this channel set to *stream*."
	h.choices = []string{"off", "stream"}
	a := newTestAdapter(f)
	a.cards = CardsStatus

	payload := []byte(`{
		"chat": {
			"space": {"name": "spaces/AAA"},
			"buttonClickedPayload": {"message": {"name": "spaces/AAA/messages/M9"}}
		},
		"commonEventObject": {"parameters": {"switchboard_command": "progress", "switchboard_arg": "stream"}}
	}`)
	a.dispatch(context.Background(), h, &pubsub.Message{Data: payload})
	a.dispatch(context.Background(), h, &pubsub.Message{Data: payload})

	if len(f.patches) != 2 {
		t.Fatalf("want 2 patches, got %d", len(f.patches))
	}
	if f.patches[0].name != f.patches[1].name || f.patches[0].fallbackText() != f.patches[1].fallbackText() {
		t.Fatalf("a redelivered click should write the same card: %+v", f.patches)
	}
}

func (p patchCall) fallbackText() string {
	if p.card == nil || len(p.card.Sections) == 0 || len(p.card.Sections[0].Widgets) == 0 {
		return p.text
	}
	w := p.card.Sections[0].Widgets[0]
	if w.DecoratedText != nil {
		return w.DecoratedText.Text
	}
	return p.text
}

// TestDispatchButtonClickWithoutAHostMessageStillAnswers: a click we cannot
// locate the card for should not silently do nothing.
func TestDispatchButtonClickWithoutHostMessage(t *testing.T) {
	f := &fakeMessenger{}
	h := &fakeHandler{ack: "done"}
	a := newTestAdapter(f)

	payload := []byte(`{
		"type": "CARD_CLICKED",
		"space": {"name": "spaces/AAA"},
		"common": {"parameters": {"switchboard_command": "progress", "switchboard_arg": "off"}}
	}`)
	a.dispatch(context.Background(), h, &pubsub.Message{Data: payload})

	if len(f.patches) != 0 {
		t.Fatalf("nothing to patch, got %+v", f.patches)
	}
	if len(f.creates) != 1 || f.creates[0].text != "done" {
		t.Fatalf("expected a fresh reply in the thread, got %+v", f.creates)
	}
}

func TestDispatchWelcomesANewSpace(t *testing.T) {
	f := &fakeMessenger{}
	h := &choiceHandler{}
	h.choices = []string{"off", "stream"}
	a := newTestAdapter(f)
	a.cards = CardsStatus

	a.dispatch(context.Background(), h, &pubsub.Message{
		Data: []byte(`{"chat": {"addedToSpacePayload": {"space": {"name": "spaces/AAA"}}}}`),
	})

	if len(f.creates) != 1 || f.creates[0].card == nil {
		t.Fatalf("expected a welcome card, got %+v", f.creates)
	}
	if f.creates[0].card.Header == nil {
		t.Fatalf("the welcome should introduce the app")
	}
	// The point of the CommandChoices capability: the values reach the card
	// from the handler, so this package hard-codes no router vocabulary. They
	// are named in the text now that a button row cannot be clicked (#28).
	if text := cardText(f.creates[0].card); !strings.Contains(text, "progress &lt;off|stream&gt;") {
		t.Fatalf("the welcome should name the handler's progress modes, got %q", text)
	}
	// And the fallback the same list, from the same place — it is the text of
	// this very message, shown wherever the card cannot render.
	if got := f.creates[0].fallback; !strings.Contains(got, "progress <off|stream>") {
		t.Fatalf("the welcome fallback should name the same modes, got %q", got)
	}
	if len(h.msgs) != 0 || len(h.cmds) != 0 {
		t.Fatalf("a welcome is not a turn: msgs=%+v cmds=%+v", h.msgs, h.cmds)
	}
}

// TestDispatchDoesNotDoubleAnswerAnInteractiveAdd: when the app is added by an
// @mention, Chat sends the mention as its own event too.
func TestDispatchDoesNotDoubleAnswerAnInteractiveAdd(t *testing.T) {
	f := &fakeMessenger{}
	h := &fakeHandler{}
	a := newTestAdapter(f)

	a.dispatch(context.Background(), h, &pubsub.Message{
		Data: []byte(`{"chat": {"addedToSpacePayload": {"space": {"name": "spaces/AAA"}, "interactionAdd": true}}}`),
	})

	if len(f.creates) != 0 {
		t.Fatalf("the mention's own event answers this one, got %+v", f.creates)
	}
}

func TestDispatchIgnoresUnactionableEvents(t *testing.T) {
	for _, payload := range []string{
		`{"chat": {"space": {"name": "spaces/A"}, "removedFromSpacePayload": {}}}`,
		`{"chat": {"space": {"name": "spaces/A"}, "messagePayload": {"message": {"sender": {"type": "BOT"}, "text": "mine"}}}}`,
		`not json at all`,
	} {
		f := &fakeMessenger{}
		h := &fakeHandler{}
		a := newTestAdapter(f)
		a.dispatch(context.Background(), h, &pubsub.Message{Data: []byte(payload)})
		if len(h.msgs) != 0 || len(h.cmds) != 0 || len(f.creates) != 0 {
			t.Fatalf("payload %q should be ignored", payload)
		}
	}
}

func TestFitsOneMessage(t *testing.T) {
	a := newTestAdapter(&fakeMessenger{})
	if !a.FitsOneMessage("short") {
		t.Fatalf("a short text fits")
	}
	if a.FitsOneMessage(strings.Repeat("x", chatTextLimit+1)) {
		t.Fatalf("an over-long text does not fit")
	}
}
