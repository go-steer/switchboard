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

// Package version centralizes build-identity reporting for
// cmd/switchboard, ported from core-agent's / k8s-lookout's
// internal/version. The package vars are overridable at release time
// via -ldflags. Without them there are two fallbacks, and which one
// applies depends on how the binary was built:
//
//   - Built from a checkout (`go build`, `go run`): Go embeds vcs.*
//     settings when -buildvcs=true, so the SHA and dirty flag are real
//     and the version stays the -dev default — the marker that says
//     "this is nobody's release".
//   - Built from the module cache (`go install module@version`): there
//     is no checkout to stamp, so vcs.* is absent and the module
//     version is the only identity there is. Read since #43; before
//     that an `@v0.1.0` install reported "v0.1.0-dev (commit none,
//     built unknown)", byte-identical to someone's uncommitted tree.
//
// Since Go 1.24 a checkout build stamps Main.Version as well, as a
// pseudo-version derived from the tags, so the two cases are told apart
// by the vcs.* stamp rather than by the module version — see resolve.
//
// Known limit, shared with core-agent: a build from a LINKED git
// worktree reads the main checkout's git state, so the SHA and dirty
// flag describe that tree rather than the worktree being built.
//
// Release process:
//
//	go build -ldflags "\
//	  -X github.com/go-steer/switchboard/internal/version.Version=v0.1.0 \
//	  -X github.com/go-steer/switchboard/internal/version.Commit=$(git rev-parse HEAD) \
//	  -X github.com/go-steer/switchboard/internal/version.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
//	" ./cmd/switchboard
package version

import (
	"fmt"
	"runtime/debug"
	"strings"
)

// Build-time metadata. Defaults assume an in-development build off
// main; release-time -ldflags injection overrides them with the real
// tag, commit, and build date.
var (
	// Version is the semver tag for released builds, or vX.Y.Z-dev
	// for in-development builds. Bump this manually on main right
	// after cutting a release.
	Version = "v0.2.0-dev"

	// Commit is the git SHA the binary was built from. Defaults to
	// "none" so the debug.BuildInfo fallback can detect that nothing
	// was injected; release builds get the full SHA via -ldflags.
	Commit = "none"

	// Date is the build timestamp in ISO 8601. Same default-sentinel
	// pattern as Commit.
	Date = "unknown"
)

// String renders the build identity for `switchboard version`, the
// --version flag, and the startup log line. prog is the binary name so
// the format starts with what the operator typed.
//
// Format:
//
//	<prog> <semver> (commit <8-char-sha>[, modified], built <date>)
func String(prog string) string {
	v, c, d, dirty := resolveBuildInfo(Version, Commit, Date)
	return format(prog, v, c, d, dirty)
}

// format renders already-resolved fields, so a test can drive it with a
// build info of its own rather than whatever built the test binary.
func format(prog, v, c, d string, dirty bool) string {
	short := c
	if len(short) > 8 {
		short = short[:8]
	}
	mod := ""
	if dirty {
		mod = ", modified"
	}
	return fmt.Sprintf("%s %s (commit %s%s, built %s)", prog, v, short, mod, d)
}

// resolveBuildInfo fills unset (-ldflags-less) fields from the build
// metadata Go embeds in the binary, so `go install`ed builds still
// report something better than the -dev defaults.
func resolveBuildInfo(v, c, d string) (version, commit, date string, dirty bool) {
	bi, ok := debug.ReadBuildInfo()
	return resolve(v, c, d, bi, ok)
}

// resolve is resolveBuildInfo with the build info passed in, so the
// precedence between -ldflags, the VCS stamp, and the module version is
// testable without building a binary per case.
//
// The three sources are tried strongest first, and the module version
// is deliberately last. Since Go 1.24 a build from a checkout stamps
// Main.Version too — as a pseudo-version derived from the tags, e.g.
// v0.0.0-20260819221521-7dd09e5f5c79 — so Main.Version being set is NOT
// on its own evidence of an installed binary, and preferring it outright
// would replace the "-dev" marker on every developer's `go build` with a
// restatement of the SHA already in the next field.
//
// What actually separates the two is the VCS stamp: it is written only
// when building from a checkout, so a module-cache build (`go install
// module@version`) is the case where the module version is the *only*
// identity available. Hence: take vcs.* first, and consult Main.Version
// only if nothing stamped a commit.
func resolve(v, c, d string, bi *debug.BuildInfo, ok bool) (version, commit, date string, dirty bool) {
	version, commit, date = v, c, d
	// A commit means -ldflags ran, which is the release build stating
	// its identity outright; nothing embedded overrides that.
	if commit != "none" {
		return version, commit, date, false
	}
	if !ok || bi == nil {
		return version, commit, date, false
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if s.Value != "" {
				commit = s.Value
			}
		case "vcs.time":
			if date == "unknown" && s.Value != "" {
				date = s.Value
			}
		case "vcs.modified":
			dirty = strings.EqualFold(s.Value, "true")
		}
	}
	// Nothing stamped a commit, so there was no checkout to stamp one
	// from: this is a module-cache build, and the module version is all
	// there is. Still skipped when the caller injected a Version without
	// a Commit — a partial -ldflags is an explicit claim, and an
	// inferred version must not silently outrank it. "(devel)" is what
	// the main module's own source reports and says nothing the -dev
	// default does not.
	if commit == "none" && strings.HasSuffix(version, "-dev") {
		if mv := bi.Main.Version; mv != "" && mv != "(devel)" {
			version = mv
		}
	}
	return version, commit, date, dirty
}
