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

package chat

import (
	"strings"
	"testing"
)

// A lone button is not a decision — there is nothing to weigh it against — and
// neither is an empty one. Adapters guard on this before building an
// interactive surface, so it has to be false in both cases and on nil.
func TestDecidingIsFalseWithoutTwoAnswers(t *testing.T) {
	for name, d := range map[string]*Decision{
		"nil":   nil,
		"empty": {ID: "p1"},
		"one":   {ID: "p1", Options: []DecisionOption{{Value: "deny", Label: "Deny"}}},
	} {
		if d.Deciding() {
			t.Errorf("%s decision reports itself as one to answer", name)
		}
	}
	two := &Decision{ID: "p1", Options: []DecisionOption{
		{Value: "deny", Label: "Deny"},
		{Value: "allow-once", Label: "Allow once"},
	}}
	if !two.Deciding() {
		t.Error("a two-answer decision does not report itself as one to answer")
	}
}

// The prose form is the whole fallback on a platform whose buttons cannot
// reach switchboard, so it has to name every answer — not a count, not the
// first few.
func TestDecisionTextNamesEveryAnswer(t *testing.T) {
	d := &Decision{ID: "p1", Options: []DecisionOption{
		{Value: "deny", Label: "Deny"},
		{Value: "allow-once", Label: "Allow once"},
		{Value: "allow-always", Label: "Always allow this directory", Broad: true},
	}}
	got := DecisionText(d)
	for _, o := range d.Options {
		if !strings.Contains(got, o.Label) {
			t.Errorf("prose form omits %q:\n%s", o.Label, got)
		}
	}
	if n := strings.Count(got, "\n"); n != len(d.Options)-1 {
		t.Errorf("got %d line breaks for %d answers:\n%s", n, len(d.Options), got)
	}
}

// An empty decision renders as nothing rather than as a stray bullet or a
// header with no list under it: the router concatenates this onto a body, and
// "" is the only value that adds nothing.
func TestDecisionTextIsEmptyWhenThereIsNothingToOffer(t *testing.T) {
	if got := DecisionText(nil); got != "" {
		t.Errorf("nil decision rendered %q", got)
	}
	if got := DecisionText(&Decision{ID: "p1"}); got != "" {
		t.Errorf("answerless decision rendered %q", got)
	}
}
