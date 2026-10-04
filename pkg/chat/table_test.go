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

// The table from the first live Google Chat session, trimmed: what a model
// actually sends, and what Chat showed as raw pipes with the delimiter row
// reading like a blank first row.
func TestTablesToCodeAlignsALiveTable(t *testing.T) {
	in := "Here are the GKE clusters:\n\n" +
		"| Name | Location | Nodes | Status |\n" +
		"| :--- | :--- | :--- | :--- |\n" +
		"| ap-eu-west3-test | europe-west3 | — | RUNNING |\n" +
		"| agent-substrate | us-central1 | 5 | RUNNING |\n" +
		"\nThat's all of them."
	want := "Here are the GKE clusters:\n\n" +
		"```\n" +
		"Name              Location      Nodes  Status\n" +
		"----------------  ------------  -----  -------\n" +
		"ap-eu-west3-test  europe-west3  —      RUNNING\n" +
		"agent-substrate   us-central1   5      RUNNING\n" +
		"```\n" +
		"\nThat's all of them."
	if got := TablesToCode(in); got != want {
		t.Errorf("TablesToCode:\n%s\nwant:\n%s", got, want)
	}
}

func TestTablesToCode(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{
			"no outer pipes, right-aligned column, markup flattened",
			"Pod | Restarts\n--- | ---:\n**api** | 3\n`db` | 12",
			"```\nPod  Restarts\n---  --------\napi         3\ndb         12\n```",
		},
		{
			"a link keeps its label and its URL; an escaped pipe stays in its cell",
			"| A | B |\n|---|---|\n| [docs](https://x.example) | a \\| b |",
			"```\nA                         B\n------------------------  -----\ndocs (https://x.example)  a | b\n```",
		},
		{
			"only markup that is there is removed: identifiers survive",
			"| File | Expr |\n|---|---|\n| `__init__.py` | 2**10 |\n| __bold__ | ~~old~~ new |",
			"```\nFile         Expr\n-----------  -------\n__init__.py  2**10\nbold         old new\n```",
		},
		{
			"an all-empty column renders instead of panicking",
			"| name | |\n|---|---|\n| a | |\n| b | |",
			"```\nname\n----  -\na\nb\n```",
		},
		{
			"dunders survive in a URL and mid-word; escaped backticks stay backticks",
			"| A | B |\n|---|---|\n| [init](https://d.example/#object.__init__) | my__init__.py \\`x\\` |",
			"```\nA" + strings.Repeat(" ", 42) + "B\n" + strings.Repeat("-", 41) + "  " + strings.Repeat("-", 17) +
				"\ninit (https://d.example/#object.__init__)  my__init__.py `x`\n```",
		},
		{
			"a code span inside a dropped link title cannot shift the next span",
			"| A |\n|---|\n| [a](u \"`t`\") `z` |",
			"```\nA\n-------\na (u) z\n```",
		},
		{
			"a cell holding ``` is left as written",
			"| A | B |\n|---|---|\n| run ``` here | x |",
			"| A | B |\n|---|---|\n| run ``` here | x |",
		},
		{
			"the body ends where another block starts",
			"| A |\n|---|\n| 1 |\n## Head | x\n> quote | y",
			"```\nA\n-\n1\n```\n## Head | x\n> quote | y",
		},
		{
			"short and long rows are padded and cut to the header",
			"| A | B |\n|---|---|\n| 1 |\n| 1 | 2 | 3 |",
			"```\nA  B\n-  -\n1\n1  2\n```",
		},
		{
			"a table already in a code block is left alone",
			"```\n| A | B |\n|---|---|\n| 1 | 2 |\n```",
			"```\n| A | B |\n|---|---|\n| 1 | 2 |\n```",
		},
		{
			"a pipe in prose is not a table",
			"run `a | b` and see\nthen more",
			"run `a | b` and see\nthen more",
		},
		{
			"a delimiter row with the wrong cell count is not a table",
			"| A | B |\n|---|\n| 1 | 2 |",
			"| A | B |\n|---|\n| 1 | 2 |",
		},
		{
			"a pipeline over a setext underline is not a table",
			"cat file |\n---",
			"cat file |\n---",
		},
		{
			"a list item is not a header",
			"- a | b\n- | -",
			"- a | b\n- | -",
		},
		{
			"an indented table is left alone (list item, or indented code)",
			"  | A | B |\n  |---|---|\n  | 1 | 2 |",
			"  | A | B |\n  |---|---|\n  | 1 | 2 |",
		},
		{
			"spaces inside a delimiter cell do not make one",
			"| A |\n| - - |\n| 1 |",
			"| A |\n| - - |\n| 1 |",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := TablesToCode(tc.in); got != tc.want {
				t.Errorf("TablesToCode(%q):\n%s\nwant:\n%s", tc.in, got, tc.want)
			}
		})
	}
}

// One long cell in an early column would be padded onto every row, turning a
// couple of kilobytes into dozens of messages; such a table is left as written.
func TestTablesToCodeLeavesAnOverWideTableAlone(t *testing.T) {
	in := "| Note | N |\n|---|---|\n| " + strings.Repeat("x", maxTableColumn+1) + " | 1 |\n| a | 2 |"
	if got := TablesToCode(in); got != in {
		t.Errorf("an over-wide table was converted:\n%s", got)
	}
	// The last column is not padded, so a long one there is fine.
	last := "| N | Note |\n|---|---|\n| 1 | " + strings.Repeat("x", 200) + " |"
	if got := TablesToCode(last); !strings.HasPrefix(got, "```") {
		t.Errorf("a long last column blocked conversion:\n%s", got)
	}
}

// FuzzTablesToCode: converting is idempotent — the output has no tables left
// to convert — and never invents a NUL, which is what a leaked placeholder
// from flattenCell would look like.
func FuzzTablesToCode(f *testing.F) {
	for _, s := range []string{
		"| A | B |\n|---|---:|\n| `x` | [l](u) |",
		"| a | |\n|---|---|\n| | |",
		"a | b\n- | -\n1 | 2",
		"| __a__ | 2**3 |\n|:-:|-|\n| \\` | ~~s~~ |",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out := TablesToCode(s)
		if again := TablesToCode(out); again != out {
			t.Errorf("not idempotent:\n%q\n->\n%q\n->\n%q", s, out, again)
		}
		if strings.Contains(out, "\x00") && !strings.Contains(s, "\x00") {
			t.Errorf("a placeholder leaked: %q -> %q", s, out)
		}
	})
}

// Text with no table comes back as the same string, so the adapters can run
// every reply through this at no cost to the replies it does not concern.
func TestTablesToCodeLeavesOtherTextAlone(t *testing.T) {
	for _, in := range []string{"", "plain", "a | b", "- list\n- items", strings.Repeat("x|", 50)} {
		if got := TablesToCode(in); got != in {
			t.Errorf("TablesToCode(%q) = %q", in, got)
		}
	}
}
