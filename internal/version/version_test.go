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

package version

import (
	"runtime/debug"
	"strings"
	"testing"
)

// buildInfo assembles the shape debug.ReadBuildInfo returns: a main
// module version plus the vcs.* settings.
//
// Both are populated at once for a checkout build, which is the whole
// subtlety this package has to get right — see resolve. The three
// shapes below were captured from real Go 1.26 builds of this module:
//
//	go install pkg@v0.0.0-2026...  mod v0.0.0-2026...   no vcs.* at all
//	go build (checkout)            mod v0.0.0-2026...   vcs.revision set
//	go run   (checkout)            mod (devel)          vcs.revision set
func buildInfo(mainVersion string, settings ...string) *debug.BuildInfo {
	bi := &debug.BuildInfo{}
	bi.Main.Version = mainVersion
	for i := 0; i < len(settings); i += 2 {
		bi.Settings = append(bi.Settings, debug.BuildSetting{Key: settings[i], Value: settings[i+1]})
	}
	return bi
}

// The defaults the package ships with, spelled out so a test reads as
// the scenario it is describing rather than as three sentinels.
const (
	devVersion = "v0.1.0-dev"
	noCommit   = "none"
	noDate     = "unknown"
)

func TestResolve(t *testing.T) {
	const sha = "a6e51f013566d0f1c0ffee0000000000deadbeef"

	for _, tc := range []struct {
		name        string
		v, c, d     string
		bi          *debug.BuildInfo
		ok          bool
		wantVersion string
		wantCommit  string
		wantDate    string
		wantDirty   bool
	}{
		{
			// The case #43 filed: the module version is the only field
			// populated, and it used to be the only one not read.
			name: "go install at a tag",
			v:    devVersion, c: noCommit, d: noDate,
			bi:          buildInfo("v0.1.0"),
			ok:          true,
			wantVersion: "v0.1.0", wantCommit: noCommit, wantDate: noDate,
		},
		{
			// A pseudo-version is unreadable but still identifies a
			// commit, which is strictly more than "-dev" says.
			name: "go install at a pseudo-version",
			v:    devVersion, c: noCommit, d: noDate,
			bi:          buildInfo("v0.0.0-20260819120633-a6e51f013566"),
			ok:          true,
			wantVersion: "v0.0.0-20260819120633-a6e51f013566", wantCommit: noCommit, wantDate: noDate,
		},
		{
			// `go run` and test binaries report "(devel)", which says
			// nothing the -dev default does not already say.
			name: "go run from a checkout",
			v:    devVersion, c: noCommit, d: noDate,
			bi: buildInfo("(devel)",
				"vcs.revision", sha, "vcs.time", "2026-08-19T12:06:33Z", "vcs.modified", "false"),
			ok:          true,
			wantVersion: devVersion, wantCommit: sha, wantDate: "2026-08-19T12:06:33Z",
		},
		{
			// The case that makes the module version untrustworthy on
			// its own. Since Go 1.24 a checkout build stamps a
			// pseudo-version into Main.Version as well, so BOTH sources
			// are populated — and the right answer is still -dev,
			// because this is a developer's build. Preferring
			// Main.Version here would strip the one marker that says
			// so, and replace it with a restatement of the SHA that is
			// already in the next field.
			name: "go build from a checkout stamps a module version too",
			v:    devVersion, c: noCommit, d: noDate,
			bi: buildInfo("v0.0.0-20260819221521-7dd09e5f5c79",
				"vcs.revision", sha, "vcs.time", "2026-08-19T12:06:33Z", "vcs.modified", "false"),
			ok:          true,
			wantVersion: devVersion, wantCommit: sha, wantDate: "2026-08-19T12:06:33Z",
		},
		{
			// Same, at a release commit: Go stamps the bare tag rather
			// than a pseudo-version. A build from a checkout that
			// happens to sit on the tag is still not the release
			// artifact, so it must not claim to be one — this is the
			// "every dev build claims to be the release that just
			// shipped" failure, arriving by a different route than the
			// forgotten manual bump.
			name: "go build from a checkout sitting on a tag",
			v:    devVersion, c: noCommit, d: noDate,
			bi:          buildInfo("v0.1.0", "vcs.revision", sha, "vcs.modified", "false"),
			ok:          true,
			wantVersion: devVersion, wantCommit: sha, wantDate: noDate,
		},
		{
			name: "build from a dirty checkout",
			v:    devVersion, c: noCommit, d: noDate,
			bi:          buildInfo("(devel)", "vcs.revision", sha, "vcs.modified", "TRUE"),
			ok:          true,
			wantVersion: devVersion, wantCommit: sha, wantDate: noDate, wantDirty: true,
		},
		{
			// -ldflags is a release build stating its identity
			// outright. Nothing embedded may override it — least of all
			// a module version, which for a released artifact would be
			// saying the same thing less precisely.
			name: "ldflags win over everything embedded",
			v:    "v0.1.0", c: sha, d: "2026-08-19T12:06:33Z",
			bi:          buildInfo("v9.9.9", "vcs.revision", "0000000000000000000000000000000000000000"),
			ok:          true,
			wantVersion: "v0.1.0", wantCommit: sha, wantDate: "2026-08-19T12:06:33Z",
		},
		{
			// A partial injection: -ldflags named a Version but no
			// Commit, so the early return does not fire. The explicit
			// claim must still win over the inferred one — before #43
			// it could not lose, because nothing else ever wrote the
			// version field.
			name: "ldflags Version only, module cache build",
			v:    "v1.2.3", c: noCommit, d: noDate,
			bi:          buildInfo("v0.1.0"),
			ok:          true,
			wantVersion: "v1.2.3", wantCommit: noCommit, wantDate: noDate,
		},
		{
			// Same partial injection, for Date: an injected build date
			// is not overwritten by the VCS commit time.
			name: "ldflags Date only, checkout build",
			v:    devVersion, c: noCommit, d: "2020-01-01T00:00:00Z",
			bi:          buildInfo("(devel)", "vcs.revision", sha, "vcs.time", "2026-08-19T12:06:33Z"),
			ok:          true,
			wantVersion: devVersion, wantCommit: sha, wantDate: "2020-01-01T00:00:00Z",
		},
		{
			name: "no build info at all",
			v:    devVersion, c: noCommit, d: noDate,
			bi:          nil,
			ok:          false,
			wantVersion: devVersion, wantCommit: noCommit, wantDate: noDate,
		},
		{
			// ok false with a non-nil pointer: the flag is the contract,
			// so it is honoured even when there is a struct to read.
			name: "build info reported as unavailable",
			v:    devVersion, c: noCommit, d: noDate,
			bi:          buildInfo("v0.1.0", "vcs.revision", sha),
			ok:          false,
			wantVersion: devVersion, wantCommit: noCommit, wantDate: noDate,
		},
		{
			// ok true with a nil pointer should not panic; the two are
			// returned together and a caller may not check both.
			name: "nil build info reported as ok",
			v:    devVersion, c: noCommit, d: noDate,
			bi:          nil,
			ok:          true,
			wantVersion: devVersion, wantCommit: noCommit, wantDate: noDate,
		},
		{
			// An empty module version is not an identity, and must not
			// blank out the default.
			name: "empty module version",
			v:    devVersion, c: noCommit, d: noDate,
			bi:          buildInfo(""),
			ok:          true,
			wantVersion: devVersion, wantCommit: noCommit, wantDate: noDate,
		},
		{
			// Nothing embedded beyond "(devel)": there is no identity
			// to be had, and the defaults stand rather than being
			// blanked out.
			name: "nothing embedded but the devel marker",
			v:    devVersion, c: noCommit, d: noDate,
			bi:          buildInfo("(devel)"),
			ok:          true,
			wantVersion: devVersion, wantCommit: noCommit, wantDate: noDate,
		},
		{
			// An empty vcs.revision must not overwrite the sentinel
			// with a blank, which would render as "commit , built ...".
			name: "empty vcs.revision",
			v:    devVersion, c: noCommit, d: noDate,
			bi:          buildInfo("(devel)", "vcs.revision", "", "vcs.time", ""),
			ok:          true,
			wantVersion: devVersion, wantCommit: noCommit, wantDate: noDate,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, c, d, dirty := resolve(tc.v, tc.c, tc.d, tc.bi, tc.ok)
			if v != tc.wantVersion {
				t.Errorf("version = %q, want %q", v, tc.wantVersion)
			}
			if c != tc.wantCommit {
				t.Errorf("commit = %q, want %q", c, tc.wantCommit)
			}
			if d != tc.wantDate {
				t.Errorf("date = %q, want %q", d, tc.wantDate)
			}
			if dirty != tc.wantDirty {
				t.Errorf("dirty = %t, want %t", dirty, tc.wantDirty)
			}
		})
	}
}

// TestInstalledBinaryIsDistinguishableFromADevTree is the whole point of
// #43 stated as one assertion: before the fix these rendered the same
// string, so a deployment reporting its version told you nothing.
//
// The three build infos are the shapes real Go 1.26 builds produce,
// captured with `go version -m`. The developer ones are here because
// the obvious fix — prefer Main.Version — makes THEM the casualty: a
// checkout build stamps a module version too, so reading it
// unconditionally trades one indistinguishable pair for another.
func TestInstalledBinaryIsDistinguishableFromADevTree(t *testing.T) {
	const sha = "7dd09e5f5c7996f244d6f6743016e0c2b9208a43"
	const pseudo = "v0.0.0-20260819221521-7dd09e5f5c79"

	// `go install github.com/go-steer/switchboard/cmd/switchboard@v0.1.0`
	installed := render(resolve(devVersion, noCommit, noDate, buildInfo("v0.1.0"), true))
	// `go build ./cmd/switchboard` in a checkout.
	built := render(resolve(devVersion, noCommit, noDate, buildInfo(pseudo, "vcs.revision", sha), true))
	// `go run ./cmd/switchboard`.
	ran := render(resolve(devVersion, noCommit, noDate, buildInfo("(devel)", "vcs.revision", sha), true))

	if installed == built || installed == ran {
		t.Fatalf("an @v0.1.0 install is indistinguishable from a developer build: %q", installed)
	}
	if !strings.Contains(installed, "v0.1.0") || strings.Contains(installed, "-dev") {
		t.Errorf("installed build reports %q, want the release version", installed)
	}
	// Both developer builds keep the marker that says they are nobody's
	// release. This is what the docs tell a developer to check —
	// docs/slack-setup.md and docs/googlechat-setup.md both say to run
	// `go build ... && switchboard version` to confirm what is running.
	for name, got := range map[string]string{"go build": built, "go run": ran} {
		if !strings.Contains(got, devVersion) {
			t.Errorf("%s reports %q, want the %s marker", name, got, devVersion)
		}
	}
}

// render is the real rendering String uses, driven off a build info of
// the test's choosing rather than whatever built the test binary.
func render(v, c, d string, dirty bool) string { return format("switchboard", v, c, d, dirty) }

// TestStringShortensTheCommit checks the rendering itself: an operator
// reading a startup line wants a SHA they can eyeball, not forty
// characters of it.
func TestStringShortensTheCommit(t *testing.T) {
	saved := []string{Version, Commit, Date}
	t.Cleanup(func() { Version, Commit, Date = saved[0], saved[1], saved[2] })

	Version, Commit, Date = "v0.1.0", "a6e51f013566d0f1c0ffee0000000000deadbeef", "2026-08-19T12:06:33Z"
	got := String("switchboard")
	const want = "switchboard v0.1.0 (commit a6e51f01, built 2026-08-19T12:06:33Z)"
	if got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

// TestFormat pins the two parts of the rendering that TestResolve
// cannot see, because they are about how a resolved field is written
// rather than which one won.
func TestFormat(t *testing.T) {
	const sha = "a6e51f013566d0f1c0ffee0000000000deadbeef"

	// The dirty marker is the difference between "this is the build we
	// shipped" and "this is someone's laptop with edits on top", and it
	// is the reason a bug report can be trusted or not.
	clean := format("switchboard", devVersion, sha, noDate, false)
	dirty := format("switchboard", devVersion, sha, noDate, true)
	if clean == dirty {
		t.Errorf("a modified build renders identically to a clean one: %q", clean)
	}
	if !strings.Contains(dirty, ", modified") {
		t.Errorf("modified build renders as %q, want it marked", dirty)
	}

	// prog is the binary name so the line starts with what the operator
	// typed — which only means anything if it is actually used.
	if got := format("gateway", devVersion, sha, noDate, false); !strings.HasPrefix(got, "gateway ") {
		t.Errorf("format ignored prog: %q", got)
	}

	// A short commit must not be truncated, and the sentinel least of
	// all: "commit non" would be a nonsense SHA rather than a marker.
	if got := format("switchboard", devVersion, noCommit, noDate, false); !strings.Contains(got, "commit none,") {
		t.Errorf("format mangled the sentinel commit: %q", got)
	}
}

// TestDefaultVersionIsADevVersion guards the manual bump documented on
// Version. The presubmit dev/ci/presubmits/verify-version-fallback
// checks it against the tags; this checks the shape alone, so a build
// that never reaches the presubmit still cannot ship a bare release
// version on main.
func TestDefaultVersionIsADevVersion(t *testing.T) {
	if !strings.HasSuffix(Version, "-dev") {
		t.Errorf("Version = %q, want an in-development version ending in -dev", Version)
	}
	if Commit != noCommit || Date != noDate {
		t.Errorf("Commit/Date = %q/%q, want the sentinels the fallbacks detect", Commit, Date)
	}
}
