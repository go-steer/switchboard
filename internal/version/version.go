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
// via -ldflags; plain `go build` / `go install` falls back to the VCS
// metadata Go embeds when -buildvcs=true, so dev builds still report a
// real SHA. (Known limit, shared with core-agent: Go embeds no vcs.*
// settings when building from a LINKED git worktree — such builds
// report "commit none".)
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
	Version = "v0.1.0-dev"

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

// resolveBuildInfo fills unset (-ldflags-less) fields from the VCS
// metadata Go embeds in the binary, so `go install`ed dev builds still
// report a real SHA and dirty flag.
func resolveBuildInfo(v, c, d string) (version, commit, date string, dirty bool) {
	version, commit, date = v, c, d
	if commit != "none" {
		return version, commit, date, false
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
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
	return version, commit, date, dirty
}
