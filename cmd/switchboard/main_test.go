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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/go-steer/switchboard/pkg/chat/googlechat"
)

// TestSplitList checks the allowlist flag parser tolerates the shapes a
// Kubernetes manifest or a shell actually produces.
func TestSplitList(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{",,", nil},
		{"C0123", []string{"C0123"}},
		{"C0123,C4567", []string{"C0123", "C4567"}},
		{" C0123 , C4567 ", []string{"C0123", "C4567"}},
		{"C0123,,C4567,", []string{"C0123", "C4567"}},
		{"C0123:1723742400.0001", []string{"C0123:1723742400.0001"}},
	} {
		if got := splitList(tc.in); !slices.Equal(got, tc.want) {
			t.Errorf("splitList(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestRunServeIngressValidation checks a misconfigured ingress is refused at
// startup, before anything dials a chat platform — a caller's escalation path
// silently not existing is exactly the failure this surface must not have.
func TestRunServeIngressValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		token string // value for $SWITCHBOARD_INGRESS_TOKEN
		want  string
	}{
		{
			name: "googlechat is refused",
			args: []string{"--platform", "googlechat", "--ingress-addr", "127.0.0.1:0"},
			want: "Slack-only",
		},
		{
			name: "no token",
			args: []string{"--platform", "slack", "--ingress-addr", "127.0.0.1:0"},
			want: "no ingress token",
		},
		{
			name:  "token from a named env var",
			args:  []string{"--platform", "slack", "--ingress-addr", "127.0.0.1:0", "--ingress-token-env", "NOPE_UNSET"},
			token: "set-but-under-the-other-name",
			want:  "$NOPE_UNSET",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SWITCHBOARD_DAEMON_TOKEN", "daemon-token")
			t.Setenv("SWITCHBOARD_INGRESS_TOKEN", tc.token)
			err := runServe(tc.args)
			if err == nil {
				t.Fatal("runServe accepted a misconfigured ingress")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestDefaultCardModeIsRich pins the out-of-the-box Google Chat rendering.
// rich is the only mode in which a long fenced answer cannot break at a message
// boundary, so the default is a correctness choice, not a cosmetic one, and it
// should not drift back without someone deciding to.
//
// Asserted against what --help prints, which is the flag as registered, rather
// than against the constant: a test that reads the constant the registration is
// supposed to use passes just as happily when the registration stops using it.
func TestDefaultCardModeIsRich(t *testing.T) {
	if mode, ok := googlechat.ParseCardMode(defaultCardMode); !ok || mode != googlechat.CardsRich {
		t.Fatalf("defaultCardMode = (%q, %v), want rich", mode, ok)
	}
	// The env var wins over the flag default, so it must not be set here.
	t.Setenv("SWITCHBOARD_GOOGLECHAT_CARDS", "")
	usage := captureStderr(t, func() { _ = runServe([]string{"-h"}) })
	if !strings.Contains(usage, "-googlechat-cards") {
		t.Fatalf("--help did not describe the flag:\n%s", usage)
	}
	if !strings.Contains(usage, `(default "rich")`) {
		i := strings.Index(usage, "-googlechat-cards")
		t.Fatalf("--googlechat-cards does not default to rich:\n%s", usage[i:min(i+400, len(usage))])
	}
}

// TestLogFormatValidation checks a typo in --log-format fails at startup
// rather than quietly selecting a rendering nobody asked for. A deployment
// setting json wants json: a collector handed text logs parses none of it, and
// the failure shows up as an empty log view long after the deploy.
//
// Checked ahead of the daemon token, so the token is set here and the error
// under test is the only one available.
func TestLogFormatValidation(t *testing.T) {
	t.Setenv("SWITCHBOARD_DAEMON_TOKEN", "daemon-token")
	t.Setenv("SWITCHBOARD_LOG_FORMAT", "")
	err := runServe([]string{"--log-format", "logfmt"})
	if err == nil {
		t.Fatal("runServe accepted an unknown --log-format")
	}
	if !strings.Contains(err.Error(), "--log-format") {
		t.Errorf("error = %q, want it to name the flag", err)
	}
}

// TestDefaultLogFormatIsText pins the out-of-the-box rendering. Asserted
// against what --help prints — the flag as registered — for the same reason
// TestDefaultCardModeIsRich is: reading the constant the registration is
// supposed to use passes just as happily when the registration stops using it.
func TestDefaultLogFormatIsText(t *testing.T) {
	t.Setenv("SWITCHBOARD_LOG_FORMAT", "")
	usage := captureStderr(t, func() { _ = runServe([]string{"-h"}) })
	i := strings.Index(usage, "-log-format")
	if i < 0 {
		t.Fatalf("--help did not describe the flag:\n%s", usage)
	}
	if !strings.Contains(usage[i:min(i+400, len(usage))], `(default "text")`) {
		t.Errorf("--log-format does not default to text:\n%s", usage[i:min(i+400, len(usage))])
	}
}

// TestLogFormatFromTheEnvironment checks the env var a container sets is read.
// The flag exists for a local run; a deployment sets SWITCHBOARD_LOG_FORMAT,
// and an env var that is registered but not consulted looks identical to one
// that is honoured right up until someone reads the logs.
func TestLogFormatFromTheEnvironment(t *testing.T) {
	t.Setenv("SWITCHBOARD_DAEMON_TOKEN", "daemon-token")
	t.Setenv("SWITCHBOARD_LOG_FORMAT", "nonsense")
	if err := runServe(nil); err == nil || !strings.Contains(err.Error(), "nonsense") {
		t.Fatalf("error = %v, want it to reject the value from the environment", err)
	}
}

// TestLogFormatReachesTheLogger checks the parsed flag is the format the
// process actually writes in. Everything else about --log-format is tested one
// layer down, in internal/logging, where the renderings are pinned exactly;
// what cannot be tested there is the single line that hands one of them to
// logging.New, and a build that always passed text would satisfy every other
// test in both packages.
//
// Driven off the build-identity line, which is logged before the config checks
// precisely so there is something on stderr from a run that goes no further.
func TestLogFormatReachesTheLogger(t *testing.T) {
	t.Setenv("SWITCHBOARD_LOG_FORMAT", "")
	// No daemon token, so runServe logs the one line and then gives up.
	t.Setenv("SWITCHBOARD_DAEMON_TOKEN", "")

	out := captureStderr(t, func() { _ = runServe([]string{"--log-format", "json"}) })
	line, _, _ := strings.Cut(out, "\n")
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("--log-format json did not produce JSON: %v\n%s", err, out)
	}
	msg, _ := rec["message"].(string)
	if !strings.Contains(msg, prog) {
		t.Errorf(`"message" = %q, want the build identity`, msg)
	}
	if _, ok := rec["time"]; !ok {
		t.Errorf("record carries no time: %v", rec)
	}

	// And the default is genuinely a different rendering, not the same one
	// under another name.
	out = captureStderr(t, func() { _ = runServe(nil) })
	line, _, _ = strings.Cut(out, "\n")
	if strings.HasPrefix(line, "{") {
		t.Errorf("text format produced JSON: %q", line)
	}
	if !strings.Contains(line, " "+prog+": ") {
		t.Errorf("text line %q is missing the %q prefix", line, prog)
	}
}

// TestStartupFailureIsLoggedOnce checks a failure that happens after the
// logger exists goes through it. In json that is the difference between a
// stream a collector reads end to end and one whose last line — the line
// saying why the process will not start, read during a crash loop — is
// dropped as unparseable.
//
// Once, not twice: main prints an error itself, and the two reporters would
// otherwise both fire.
func TestStartupFailureIsLoggedOnce(t *testing.T) {
	t.Setenv("SWITCHBOARD_LOG_FORMAT", "json")
	t.Setenv("SWITCHBOARD_DAEMON_TOKEN", "")

	var err error
	out := captureStderr(t, func() { err = runServe(nil) })

	// main suppresses its own print for this, so the error must still say what
	// went wrong to anything that looks at it.
	var logged loggedError
	if !errors.As(err, &logged) {
		t.Fatalf("error = %#v, want one marked as already logged", err)
	}
	const want = "no daemon token"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to still mention %q", err, want)
	}
	if n := strings.Count(out, want); n != 1 {
		t.Errorf("the failure was logged %d times, want 1:\n%s", n, out)
	}
	for i, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if !json.Valid([]byte(line)) {
			t.Errorf("line %d of a json run is not JSON: %q", i, line)
		}
	}
}

// TestReportOnceLeavesAnAlreadyLoggedFailureAlone checks the two reporters do
// not both fire. A listener's bind failure is logged where it happens, with the
// name of the listener attached; serve's deferred reporter would otherwise
// repeat it a line later in a vaguer form, which reads in a log as two
// failures.
func TestReportOnceLeavesAnAlreadyLoggedFailureAlone(t *testing.T) {
	var lines []string
	logf := func(format string, a ...any) { lines = append(lines, fmt.Sprintf(format, a...)) }
	bind := errors.New("listen tcp 127.0.0.1:9090: address already in use")

	if got := reportOnce(logf, nil); got != nil {
		t.Errorf("reportOnce(nil) = %v, want nil", got)
	}
	if len(lines) != 0 {
		t.Errorf("a nil error was logged: %q", lines)
	}

	// Not yet reported: log it, and mark it so nothing else does.
	got := reportOnce(logf, bind)
	if len(lines) != 1 || !strings.Contains(lines[0], "already in use") {
		t.Fatalf("logged %q, want the failure once", lines)
	}
	var logged loggedError
	if !errors.As(got, &logged) {
		t.Fatalf("reportOnce returned %#v, want it marked as logged", got)
	}
	if !errors.Is(got, bind) {
		t.Errorf("the wrapper lost the cause: %v", got)
	}

	// Already reported: pass it through in silence.
	if again := reportOnce(logf, got); again != got {
		t.Errorf("reportOnce rewrapped an already-logged error: %#v", again)
	}
	if len(lines) != 1 {
		t.Errorf("the failure was logged %d times, want 1: %q", len(lines), lines)
	}
}

// TestMainReportsAStartupFailureOnce runs the real main in a subprocess. Every
// other test here drives runServe, which cannot see main's own print — and
// main printing an error serve has already logged is exactly the double-report
// this guards, in the format (json) where the extra line is also unparseable.
func TestMainReportsAStartupFailureOnce(t *testing.T) {
	if os.Getenv("SWITCHBOARD_TEST_RUN_MAIN") == "1" {
		// The test binary's own -test.run is still on the command line, and
		// main parses os.Args. Drop it: the run under test is configured
		// entirely through the environment below.
		os.Args = os.Args[:1]
		main()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMainReportsAStartupFailureOnce$")
	// Appended last so these win over whatever the developer's shell exports.
	cmd.Env = append(os.Environ(),
		"SWITCHBOARD_TEST_RUN_MAIN=1",
		"SWITCHBOARD_LOG_FORMAT=json",
		"SWITCHBOARD_DAEMON_TOKEN=",
	)
	var stderr strings.Builder
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	err := cmd.Run()

	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("main exited with %v, want status 1\n%s", err, stderr.String())
	}
	out := stderr.String()
	const want = "no daemon token"
	if n := strings.Count(out, want); n != 1 {
		t.Errorf("the failure was reported %d times, want 1:\n%s", n, out)
	}
	for i, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if !json.Valid([]byte(line)) {
			t.Errorf("line %d of a json run is not JSON: %q", i, line)
		}
	}
}

// captureStderr runs fn with os.Stderr redirected and returns what it wrote.
// The flag package prints its defaults there, and that output is the only place
// a registered default is observable from outside runServe.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	saved := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		_, _ = io.Copy(&b, r)
		done <- b.String()
	}()
	fn()
	os.Stderr = saved
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

// TestParseCallerMode and TestParseProgressMode pin the flag vocabulary: a
// typo must fail at startup rather than silently selecting a default.
func TestParseCallerMode(t *testing.T) {
	for _, ok := range []string{"email", "id"} {
		if _, err := parseCallerMode(ok); err != nil {
			t.Errorf("parseCallerMode(%q) = %v, want no error", ok, err)
		}
	}
	if _, err := parseCallerMode("username"); err == nil {
		t.Error("parseCallerMode accepted an unknown mode")
	}
}

func TestParseProgressMode(t *testing.T) {
	for _, ok := range []string{"off", "indicator", "status", "stream"} {
		if _, err := parseProgressMode(ok); err != nil {
			t.Errorf("parseProgressMode(%q) = %v, want no error", ok, err)
		}
	}
	if _, err := parseProgressMode("verbose"); err == nil {
		t.Error("parseProgressMode accepted an unknown mode")
	}
}

func TestParseAppCommands(t *testing.T) {
	got, err := parseAppCommands(" 1=progress , 2=help ")
	if err != nil {
		t.Fatalf("parseAppCommands: %v", err)
	}
	if len(got) != 2 || got[1] != "progress" || got[2] != "help" {
		t.Fatalf("unexpected mapping %v", got)
	}
	if got, err := parseAppCommands("  "); err != nil || got != nil {
		t.Fatalf("an empty mapping should be nil, got (%v, %v)", got, err)
	}
	// A malformed entry has to fail loudly: silently dropping it would leave a
	// configured command routing to whatever the message text happens to say.
	for _, bad := range []string{"progress", "1=", "=progress", "x=progress", "1=progress,oops"} {
		if _, err := parseAppCommands(bad); err == nil {
			t.Errorf("parseAppCommands(%q) accepted a malformed entry", bad)
		}
	}
}
