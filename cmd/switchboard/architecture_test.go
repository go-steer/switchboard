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

package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	switchboardModule           = "github.com/go-steer/switchboard"
	switchboardModulePrefix     = switchboardModule + "/"
	tailLineThreshold           = 150
	maxNonTestFuncsOver150Lines = 2 // cmd/switchboard/main.go:runServe, cmd/switchboard/router.go:(*Router).relay
)

// packageLayer defines strict acyclic architecture layers across switchboard
// (higher layers may only import strictly lower layers, dstLayer < srcLayer).
//
// Layer 0 holds leaf internal support packages with zero internal imports.
// Layer 1 holds the provider-neutral chat contract (pkg/chat) and the thin
// 4-verb core-agent client (pkg/daemon); neither may import the other or any
// platform adapter / optional broker above it.
// Layer 2 holds chat platform adapters behind pkg/chat.Adapter and optional
// companion clients built atop pkg/daemon.
// Layer 3 holds the top-level switchboard binary and session router.
var packageLayer = map[string]int{
	"internal/axapi":      0,
	"internal/logging":    0,
	"internal/sse":        0,
	"internal/version":    0,
	"pkg/chat":            1,
	"pkg/daemon":          1,
	"pkg/approval":        2,
	"pkg/ax":              2,
	"pkg/chat/googlechat": 2,
	"pkg/chat/slack":      2,
	"pkg/subagent":        2,
	"cmd/switchboard":     3,
}

var skippedArchDirs = map[string]bool{
	".git":         true,
	".worktrees":   true,
	".claude":      true,
	".demo-daemon": true,
	".demo-rig":    true,
	".rig":         true,
	"bin":          true,
	"dist":         true,
	"vendor":       true,
	"testdata":     true,
	"node_modules": true,
}

func TestArchitectureLayeringAndComplexityTail(t *testing.T) {
	t.Parallel()
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	seenPackages := make(map[string]bool, len(packageLayer))
	var over150 []string

	err = filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if skippedArchDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if strings.HasSuffix(rel, "_test.go") {
			return nil
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(fset, path, raw, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}

		pkgRel := filepath.ToSlash(filepath.Dir(rel))
		seenPackages[pkgRel] = true
		for _, v := range validatePackageImports(rel, pkgRel, f, packageLayer) {
			t.Error(v)
		}

		over150 = append(over150, over150InFile(fset, rel, raw, f)...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	for pkg := range packageLayer {
		if !seenPackages[pkg] {
			t.Errorf("packageLayer registers %q, which has no non-test .go files in tree", pkg)
		}
	}

	if len(over150) > maxNonTestFuncsOver150Lines {
		t.Errorf("non-test functions over %d code lines = %d (ratchet ceiling %d): %v",
			tailLineThreshold, len(over150), maxNonTestFuncsOver150Lines, over150)
	}
}

func over150InFile(fset *token.FileSet, rel string, raw []byte, f *ast.File) []string {
	if ast.IsGenerated(f) {
		return nil
	}
	lines := strings.Split(string(raw), "\n")
	var over []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		start := fset.Position(fn.Pos()).Line
		end := fset.Position(fn.End()).Line
		if countCodeLines(lines, start, end) > tailLineThreshold {
			over = append(over, rel+":"+fn.Name.Name)
		}
	}
	return over
}

func validatePackageImports(relFile, pkgRel string, f *ast.File, layers map[string]int) []string {
	srcLayer, ok := layers[pkgRel]
	if !ok {
		return []string{fmt.Sprintf("%s: package %q is not registered in packageLayer architecture map", relFile, pkgRel)}
	}
	var errs []string
	for _, imp := range f.Imports {
		impPath, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: invalid import literal %s: %v", relFile, imp.Path.Value, err))
			continue
		}
		var dstPkg string
		switch {
		case impPath == switchboardModule:
			dstPkg = "."
		case strings.HasPrefix(impPath, switchboardModulePrefix):
			dstPkg = strings.TrimPrefix(impPath, switchboardModulePrefix)
		default:
			continue
		}
		dstLayer, known := layers[dstPkg]
		if !known {
			errs = append(errs, fmt.Sprintf("%s: imports unregistered internal package %q", relFile, dstPkg))
			continue
		}
		if dstLayer >= srcLayer {
			errs = append(errs, fmt.Sprintf("layering violation in %s (%s, layer %d): cannot import %s (layer %d)",
				relFile, pkgRel, srcLayer, dstPkg, dstLayer))
		}
	}
	return errs
}

func countCodeLines(lines []string, startLine, endLine int) int {
	n := 0
	for i := startLine - 1; i < endLine && i < len(lines); i++ {
		s := strings.TrimSpace(lines[i])
		if s == "" || strings.HasPrefix(s, "//") {
			continue
		}
		n++
	}
	return n
}

func TestValidatePackageImports(t *testing.T) {
	t.Parallel()

	parseImports := func(t *testing.T, rawLiterals ...string) *ast.File {
		t.Helper()
		var b strings.Builder
		b.WriteString("package sample\nimport (\n")
		for _, lit := range rawLiterals {
			fmt.Fprintf(&b, "\t%s\n", lit)
		}
		b.WriteString(")\n")
		f, err := parser.ParseFile(token.NewFileSet(), "sample.go", b.String(), 0)
		if err != nil {
			t.Fatalf("parse sample imports: %v", err)
		}
		return f
	}

	cases := []struct {
		name    string
		pkg     string
		imports []string
		wantSub string
	}{
		{
			name: "router allowed downward imports",
			pkg:  "cmd/switchboard",
			imports: []string{
				`"context"`,
				`"github.com/go-steer/switchboard/internal/logging"`,
				`"github.com/go-steer/switchboard/pkg/chat"`,
				`"github.com/go-steer/switchboard/pkg/chat/slack"`,
				`"github.com/go-steer/switchboard/pkg/daemon"`,
			},
		},
		{
			name:    "provider seam blocks pkg/chat importing adapter",
			pkg:     "pkg/chat",
			imports: []string{`"github.com/go-steer/switchboard/pkg/chat/slack"`},
			wantSub: "layering violation",
		},
		{
			name:    "raw backtick import literal still enforced",
			pkg:     "pkg/chat",
			imports: []string{"`github.com/go-steer/switchboard/pkg/chat/slack`"},
			wantSub: "layering violation",
		},
		{
			name:    "provider seam blocks cross-adapter peer import",
			pkg:     "pkg/chat/slack",
			imports: []string{`"github.com/go-steer/switchboard/pkg/chat/googlechat"`},
			wantSub: "layering violation",
		},
		{
			name:    "thin daemon blocks pkg/daemon importing pkg/approval",
			pkg:     "pkg/daemon",
			imports: []string{`"github.com/go-steer/switchboard/pkg/approval"`},
			wantSub: "layering violation",
		},
		{
			name:    "peer layer 1 blocks pkg/daemon importing pkg/chat",
			pkg:     "pkg/daemon",
			imports: []string{`"github.com/go-steer/switchboard/pkg/chat"`},
			wantSub: "layering violation",
		},
		{
			name:    "unregistered source package rejected",
			pkg:     "pkg/unregistered",
			imports: []string{`"context"`},
			wantSub: "not registered in packageLayer",
		},
		{
			name:    "unregistered internal destination rejected",
			pkg:     "cmd/switchboard",
			imports: []string{`"github.com/go-steer/switchboard/pkg/ghost"`},
			wantSub: "unregistered internal package",
		},
		{
			name:    "root module import rejected",
			pkg:     "cmd/switchboard",
			imports: []string{`"github.com/go-steer/switchboard"`},
			wantSub: "unregistered internal package \".\"",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := parseImports(t, tc.imports...)
			errs := validatePackageImports(tc.pkg+"/sample.go", tc.pkg, f, packageLayer)
			if tc.wantSub == "" {
				if len(errs) != 0 {
					t.Fatalf("unexpected errors: %v", errs)
				}
				return
			}
			if len(errs) != 1 || !strings.Contains(errs[0], tc.wantSub) {
				t.Fatalf("errs = %v, want single error containing %q", errs, tc.wantSub)
			}
		})
	}
}

func TestOver150SkipsGeneratedFiles(t *testing.T) {
	t.Parallel()

	var body strings.Builder
	for i := 0; i < 160; i++ {
		fmt.Fprintf(&body, "\tx += %d\n", i)
	}

	handwritten := "package sample\nfunc big() int {\n\tx := 0\n" + body.String() + "\treturn x\n}\n"
	fset := token.NewFileSet()
	fHand, err := parser.ParseFile(fset, "hand.go", handwritten, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	if got := over150InFile(fset, "pkg/sample/hand.go", []byte(handwritten), fHand); len(got) != 1 || got[0] != "pkg/sample/hand.go:big" {
		t.Fatalf("handwritten over150InFile = %v, want [pkg/sample/hand.go:big]", got)
	}

	generated := "// Code generated by protoc-gen-go. DO NOT EDIT.\n\n" +
		"package axapi\nimport \"github.com/go-steer/switchboard/pkg/daemon\"\nfunc big() int {\n\tx := 0\n" + body.String() + "\treturn x\n}\n"
	fGen, err := parser.ParseFile(fset, "ax.pb.go", generated, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	if got := over150InFile(fset, "internal/axapi/ax.pb.go", []byte(generated), fGen); len(got) != 0 {
		t.Fatalf("generated over150InFile = %v, want empty", got)
	}
	if errs := validatePackageImports("internal/axapi/ax.pb.go", "internal/axapi", fGen, packageLayer); len(errs) != 1 {
		t.Fatalf("generated validatePackageImports = %v, want layering violation", errs)
	}
}

func TestCountCodeLines(t *testing.T) {
	t.Parallel()

	src := strings.Split(strings.Join([]string{
		"package sample",         // 1
		"",                       // 2
		"// doc comment",         // 3
		"func example() {",       // 4 (code 1)
		"\t// inline comment",    // 5 (ignored)
		"",                       // 6 (ignored)
		"\tx := 1",               // 7 (code 2)
		"\t_ = x // trailing ok", // 8 (code 3)
		"}",                      // 9 (code 4)
	}, "\n"), "\n")

	if got := countCodeLines(src, 4, 9); got != 4 {
		t.Fatalf("countCodeLines(4..9) = %d, want 4", got)
	}
	if got := countCodeLines(src, 4, 999); got != 4 {
		t.Fatalf("countCodeLines clamped out-of-range end = %d, want 4", got)
	}
}
