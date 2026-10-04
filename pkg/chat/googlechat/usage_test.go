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
	"strings"
	"testing"
	"time"

	chatv1 "google.golang.org/api/chat/v1"

	"github.com/go-steer/switchboard/pkg/chat"
)

var testUsage = &chat.Usage{
	Model: "gemini-3.7-flash", TokensIn: 5000, TokensOut: 1,
	CostUSD: 0.0037537, Latency: 3142 * time.Millisecond,
}

const testUsageLine = "gemini-3.7-flash · 5,000 in / 1 out · $0.0038 · 3.1s"

// structuredAnswer is a turn answerCard renders (an ATX header is the
// structure that earns a card); a bare paragraph would go out as text.
const structuredAnswer = "# Deploy check\n\nall three replicas are ready\n"

func TestWithUsageFooter(t *testing.T) {
	card := withUsageFooter(answerCard(structuredAnswer), testUsage)
	if card == nil {
		t.Fatal("answerCard(structured) = nil, want a card")
	}
	last := card.Sections[len(card.Sections)-1]
	widgets := last.Widgets
	if len(widgets) < 2 {
		t.Fatalf("last section has %d widgets, want the answer plus a divider and footer", len(widgets))
	}
	// The footer stays last, with a divider setting it off from the answer.
	if widgets[len(widgets)-2].Divider == nil {
		t.Error("no divider before the usage footer")
	}
	dt := widgets[len(widgets)-1].DecoratedText
	if dt == nil {
		t.Fatalf("last widget is not a DecoratedText: %+v", widgets[len(widgets)-1])
	}
	if dt.StartIcon == nil || dt.StartIcon.MaterialIcon == nil || dt.StartIcon.MaterialIcon.Name != iconUsage {
		t.Errorf("footer icon = %+v, want %s", dt.StartIcon, iconUsage)
	}
	if !strings.Contains(dt.Text, testUsageLine) {
		t.Errorf("footer text = %q, want it to contain %q", dt.Text, testUsageLine)
	}
}

// TestWithUsageFooterNoCard covers the cases that must change nothing: no
// usage to show, and no card to attach it to (the answer is going out as plain
// text, where a footer line would have to survive the message chunker).
func TestWithUsageFooterNoCard(t *testing.T) {
	if got := withUsageFooter(nil, testUsage); got != nil {
		t.Errorf("withUsageFooter(nil, usage) = %+v, want nil", got)
	}
	card := answerCard(structuredAnswer)
	before := len(card.Sections[len(card.Sections)-1].Widgets)
	if got := withUsageFooter(card, nil); len(got.Sections[len(got.Sections)-1].Widgets) != before {
		t.Error("nil usage changed the card")
	}
	if got := withUsageFooter(card, &chat.Usage{}); len(got.Sections[len(got.Sections)-1].Widgets) != before {
		t.Error("empty usage changed the card")
	}
	// A card with no sections has no last section to append to.
	if got := withUsageFooter(&chatv1.GoogleAppsCardV1Card{}, testUsage); len(got.Sections) != 0 {
		t.Errorf("sectionless card gained sections: %+v", got)
	}
}

// TestSendProseAnswerCarriesTheFooter: answerCard renders nothing for a bare
// paragraph, which used to leave every plain answer without the cost an
// operator turned --show-usage on to see (reported from the live rig). With
// usage on, a plain answer is promoted to a card so the footer has something
// to ride; without it, it still goes as text.
func TestSendProseAnswerCarriesTheFooter(t *testing.T) {
	for _, tt := range []struct {
		name     string
		usage    *chat.Usage
		wantCard bool
	}{
		{"usage on", testUsage, true},
		{"usage off", nil, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeMessenger{}
			a := newTestAdapter(f)
			a.cards = CardsRich
			reply := chat.Reply{
				Conversation: "spaces/AAA:spaces/AAA/threads/T1",
				Text:         "all three replicas are ready",
				Usage:        tt.usage,
			}
			if _, err := a.Send(context.Background(), reply); err != nil {
				t.Fatalf("Send: %v", err)
			}
			if len(f.creates) != 1 {
				t.Fatalf("creates = %d, want 1", len(f.creates))
			}
			if got := f.creates[0].card != nil; got != tt.wantCard {
				t.Fatalf("card = %v, want %v", got, tt.wantCard)
			}
			if tt.wantCard {
				got := renderCall(f.creates[0])
				if !strings.Contains(got, testUsageLine) || !strings.Contains(got, "all three replicas are ready") {
					t.Errorf("plain answer card = %q, want the answer and the usage line", got)
				}
			}
		})
	}
}

// A structured answer answerCard turned down — for size — is not a plain
// answer: as a plain card its headers would go out as raw #.
func TestAStructuredAnswerIsNeverAPlainCard(t *testing.T) {
	var b strings.Builder
	b.WriteString("# Title\n\n")
	for i := 0; i < 90; i++ {
		b.WriteString("para\n\n")
	}
	if answerCard(b.String()) != nil {
		t.Fatal("fixture should be over answerCard's widget budget")
	}
	if plainAnswerCard(b.String()) != nil {
		t.Errorf("a structured answer became a plain card")
	}
	if plainAnswerCard("```\n# not a header\n---\n```\nplain") == nil {
		t.Errorf("a header inside a code block counted as structure")
	}
}

// Effectively-once (#99): the card shares its request id with the text's first
// part, and a replay renders text (usage is not replayed). So a plain answer is
// carded only when its text is one part — a longer one would re-post its tail.
func TestAPlainAnswerIsCardedOnlyWhenItsTextIsOnePart(t *testing.T) {
	long := strings.Repeat("all three replicas are ready. ", 200) // > chatTextLimit
	if plainAnswerCard(long) != nil {
		t.Errorf("an answer longer than one text message became a card")
	}
	if plainAnswerCard("short and plain") == nil {
		t.Errorf("a one-message plain answer did not become a card")
	}
}

// A plain card defuses mentions as the text path does.
func TestAPlainCardDefusesMentions(t *testing.T) {
	card := plainAnswerCard("ping <users/all> please")
	if card == nil {
		t.Fatal("no card")
	}
	if got := cardText(card); strings.Contains(got, "<users/all>") {
		t.Errorf("card carries a live mention: %q", got)
	}
}

// An answer too big for one card still goes as text — chunked, without the
// footer — rather than being cut to fit.
func TestAnOversizePlainAnswerStaysText(t *testing.T) {
	if card := plainAnswerCard(strings.Repeat("word ", 20000)); card != nil {
		t.Errorf("an oversize answer became a card")
	}
}

// TestSendAttachesUsageFooter drives cardFor through Send: the footer rides the
// answer card in rich mode, and is suppressed in status mode where the answer
// goes out as text.
func TestSendAttachesUsageFooter(t *testing.T) {
	for _, tt := range []struct {
		name       string
		mode       CardMode
		wantFooter bool
	}{
		{"rich", CardsRich, true},
		{"status", CardsStatus, false},
		{"off", CardsOff, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeMessenger{}
			a := newTestAdapter(f)
			a.cards = tt.mode
			reply := chat.Reply{
				Conversation: "spaces/AAA:spaces/AAA/threads/T1",
				Text:         structuredAnswer,
				Usage:        testUsage,
			}
			if _, err := a.Send(context.Background(), reply); err != nil {
				t.Fatalf("Send: %v", err)
			}
			if len(f.creates) != 1 {
				t.Fatalf("creates = %d, want 1", len(f.creates))
			}
			got := strings.Contains(renderCall(f.creates[0]), testUsageLine)
			if got != tt.wantFooter {
				t.Errorf("posted message contains usage = %v, want %v", got, tt.wantFooter)
			}
		})
	}
}

// renderCall flattens a posted message — text, fallback, and every card widget
// — into one string, so a test can ask whether the usage line reached Chat at
// all without caring which field carried it.
func renderCall(c createCall) string {
	var b strings.Builder
	b.WriteString(c.text)
	b.WriteString("\n")
	b.WriteString(c.fallback)
	if c.card == nil {
		return b.String()
	}
	for _, s := range c.card.Sections {
		for _, w := range s.Widgets {
			if w.TextParagraph != nil {
				b.WriteString("\n" + w.TextParagraph.Text)
			}
			if w.DecoratedText != nil {
				b.WriteString("\n" + w.DecoratedText.Text)
			}
		}
	}
	return b.String()
}
