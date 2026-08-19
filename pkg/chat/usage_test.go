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
	"testing"
	"time"
)

func TestUsageLine(t *testing.T) {
	for _, tt := range []struct {
		name string
		u    Usage
		want string
	}{{
		name: "live capture",
		u:    Usage{Model: "gemini-3.7-flash", TokensIn: 5000, TokensOut: 1, CostUSD: 0.0037537, Latency: 3142 * time.Millisecond},
		want: "gemini-3.7-flash · 5,000 in / 1 out · $0.0038 · 3.1s",
	}, {
		name: "sub-second latency",
		u:    Usage{Model: "m", TokensIn: 12, TokensOut: 3, CostUSD: 0.5, Latency: 840 * time.Millisecond},
		want: "m · 12 in / 3 out · $0.5000 · 840ms",
	}, {
		name: "dollars round to cents",
		u:    Usage{CostUSD: 12.3456},
		want: "$12.35",
	}, {
		name: "vanishingly small cost",
		u:    Usage{CostUSD: 0.00001},
		want: "<$0.0001",
	}, {
		name: "big token counts group",
		u:    Usage{TokensIn: 1234567, TokensOut: 890},
		want: "1,234,567 in / 890 out",
	}, {
		// A partial report degrades to a shorter line rather than showing a
		// field the daemon never sent as a zero.
		name: "no cost reported",
		u:    Usage{Model: "m", TokensIn: 10, TokensOut: 2, Latency: time.Second},
		want: "m · 10 in / 2 out · 1.0s",
	}, {
		name: "empty",
		u:    Usage{},
		want: "",
	}} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.u.Line(); got != tt.want {
				t.Errorf("Line() = %q, want %q", got, tt.want)
			}
		})
	}
}
