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
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-steer/switchboard/pkg/chat"
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
			name: "no token",
			args: []string{"--platform", "slack", "--ingress-addr", "127.0.0.1:0"},
			want: "no ingress token",
		},
		{
			// The same check, on the platform that used to be turned away
			// before reaching it. Its refusal was the whole of #39.
			name: "no token on googlechat either",
			args: []string{"--platform", "googlechat", "--ingress-addr", "127.0.0.1:0"},
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

// TestRunServeIngressIsNotSlackOnly checks a configured ingress on
// --platform googlechat gets past the ingress gate and on to building the
// adapter, which is as far as this can go without a Pub/Sub subscription to
// dial. Until #39 it stopped one step earlier, and every agent-initiated use
// case — a scheduled digest, a 3am escalation — was impossible on Chat.
//
// The assertion is on where it got to, not on success: the adapter refusing a
// half-configured subscription is the run having reached a check that lives
// beyond the one being tested. It is refused for being half a pair rather than
// for the subscription alone: with neither flag set, a bridged run is refused
// earlier and by a different check, and an --outbound-only one is not refused
// at all (#23).
func TestRunServeIngressIsNotSlackOnly(t *testing.T) {
	t.Setenv("SWITCHBOARD_DAEMON_TOKEN", "daemon-token")
	t.Setenv("SWITCHBOARD_INGRESS_TOKEN", "ingress-token")
	// Both Chat flags default from the environment, so an operator's own
	// exported values would otherwise decide what this test configures.
	t.Setenv("SWITCHBOARD_GOOGLE_PROJECT", "")
	t.Setenv("SWITCHBOARD_GOOGLE_SUBSCRIPTION", "")

	err := runServe([]string{
		"--platform", "googlechat", "--ingress-addr", "127.0.0.1:0",
		"--google-subscription", "sub-without-a-project",
	})
	if err == nil {
		t.Fatal("runServe returned nil; want the adapter to refuse a subscription with no project")
	}
	if strings.Contains(err.Error(), "Slack-only") {
		t.Fatalf("the ingress still refuses googlechat: %v", err)
	}
	if !strings.Contains(err.Error(), "googlechat adapter") {
		t.Errorf("error = %q, want it to come from building the adapter", err)
	}
}

// TestRunServeRefusesWhenItCanNeitherReceiveNorPost: an outbound-only run is a
// real shape (#23), but only with somewhere for the digests to come from. With
// --outbound-only and no ingress the process would start, log a banner and sit
// there forever doing nothing — healthy to every probe, useless to everyone —
// so it refuses instead, naming the knob that is missing.
//
// Both platforms, because the check is deliberately ahead of the adapter: it
// reads only the two flags that decide the shape, so nothing about it is
// platform-specific and neither platform's credentials are touched to reach it.
// That is what makes it reachable on Chat at all — googlechat.New builds a REST
// service from Application Default Credentials, which a hermetic test has none
// of.
func TestRunServeRefusesWhenItCanNeitherReceiveNorPost(t *testing.T) {
	for _, platform := range []string{"slack", "googlechat"} {
		t.Run(platform, func(t *testing.T) {
			// The ingress address defaults from the environment, so an exported
			// value would otherwise give this run the direction it is meant to
			// be missing.
			t.Setenv("SWITCHBOARD_INGRESS_ADDR", "")

			err := runServe([]string{"--platform", platform, "--outbound-only"})
			if err == nil {
				t.Fatal("runServe started a process that can neither receive nor post")
			}
			if !strings.Contains(err.Error(), "nothing to do") {
				t.Errorf("error = %q, want it to say there is nothing to do", err)
			}
			if !strings.Contains(err.Error(), "--ingress-addr") {
				t.Errorf("error = %q, want it to name the flag that would give it a direction", err)
			}
		})
	}
}

// TestRunServeRequiresAnInboundSourceUnlessDeclaredOutboundOnly: the mode is
// declared, never inferred from whether a credential happens to be set.
//
// This is the whole reason --outbound-only is a flag. The recommended
// deployment is bidirectional — a Socket Mode bridge that also serves the
// ingress — and if an emptied or rotated app-token secret were read as "this
// one only posts", that deployment would come back up posting, passing
// /healthz, and answering nobody, with nothing in the logs a pager could match
// on. It fails loudly instead, and says how to mean it on purpose.
func TestRunServeRequiresAnInboundSourceUnlessDeclaredOutboundOnly(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		env  map[string]string
		want []string
	}{
		{
			// The message has to name the var this run was told to read, not
			// the default: an operator who renamed it is the one most likely to
			// have emptied it by mistake.
			name: "slack",
			args: []string{"--platform", "slack", "--slack-app-token-env", "TEAM_SLACK_APP_TOKEN"},
			env:  map[string]string{"TEAM_SLACK_APP_TOKEN": ""},
			want: []string{"$TEAM_SLACK_APP_TOKEN", "--outbound-only"},
		},
		{
			name: "googlechat",
			args: []string{"--platform", "googlechat"},
			env: map[string]string{
				"SWITCHBOARD_GOOGLE_PROJECT":      "a-project",
				"SWITCHBOARD_GOOGLE_SUBSCRIPTION": "",
			},
			want: []string{"--google-subscription", "--outbound-only"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SWITCHBOARD_DAEMON_TOKEN", "daemon-token")
			t.Setenv("SWITCHBOARD_SLACK_BOT_TOKEN", "xoxb-x")
			// An ingress is configured, so this run has a direction: what it is
			// refused for is being unable to receive without having said so.
			t.Setenv("SWITCHBOARD_INGRESS_TOKEN", "ingress-token")
			t.Setenv("SWITCHBOARD_INGRESS_ADDR", "127.0.0.1:0")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			err := runServe(tc.args)
			if err == nil {
				t.Fatal("runServe silently degraded a bridge into an outbound-only run")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to mention %q", err, want)
				}
			}
		})
	}
}

// TestOutboundOnlyIgnoresTheInboundCredentialsOutLoud: the flag wins over any
// inbound credential still lying around, because a run whose mode depends on
// which of two contradictory things it was given is a run nobody can reason
// about. Ignoring them quietly is the other half of the trap, though — somebody
// provisioned that token expecting it to be used — so it is ignored in writing.
//
// Slack only: proving it on Chat means getting past googlechat.New, which needs
// Application Default Credentials.
func TestOutboundOnlyIgnoresTheInboundCredentialsOutLoud(t *testing.T) {
	t.Setenv("SWITCHBOARD_SLACK_APP_TOKEN", "xapp-left-over")
	t.Setenv("SWITCHBOARD_INGRESS_TOKEN", "ingress-token")
	t.Setenv("SWITCHBOARD_INGRESS_ADDR", "127.0.0.1:0")

	// The bot token is left unset, so the run reaches the warning and then dies
	// one line later in slack.New. Starting a server and shutting it down again
	// would be a much slower way to read the same log.
	t.Setenv("SWITCHBOARD_SLACK_BOT_TOKEN", "")

	var err error
	logs := captureStderr(t, func() { err = runServe([]string{"--platform", "slack", "--outbound-only"}) })
	if err == nil {
		t.Fatal("runServe = nil, want it to stop at the missing bot token")
	}
	if !strings.Contains(err.Error(), "slack adapter") {
		t.Fatalf("error = %q, want the run to have reached the adapter switch", err)
	}
	if !strings.Contains(logs, "SWITCHBOARD_SLACK_APP_TOKEN") || !strings.Contains(logs, "warning") {
		t.Errorf("logs did not warn that the app token is ignored:\n%s", logs)
	}
}

// TestRunServeOutboundOnlyServesTheIngress: with --outbound-only and an
// ingress, serve runs. It does not refuse, it does not call Run on an adapter
// that would answer chat.ErrNoInbound, and it serves the ingress for as long as
// a bridged run would — it waits on the same context Run would have, and a
// signal shuts it down cleanly.
//
// End to end through runServe rather than in pieces, because the thing that
// broke before #23 was the wiring: every part of an egress-only run existed and
// none of them could be reached. The Slack credentials here are nonsense
// strings and never leave the process: the adapter is built, never run, and the
// only request made is one the ingress rejects before it would post anything.
func TestRunServeOutboundOnlyServesTheIngress(t *testing.T) {
	// Deliberately absent. An outbound-only run has no daemon to present a
	// bearer token to, so being made to provision one would be asking for a
	// credential with nowhere to go — and this is the test that would notice.
	t.Setenv("SWITCHBOARD_DAEMON_TOKEN", "")
	t.Setenv("SWITCHBOARD_INGRESS_TOKEN", "ingress-token")
	t.Setenv("SWITCHBOARD_SLACK_BOT_TOKEN", "xoxb-x")
	t.Setenv("SWITCHBOARD_INGRESS_ADDR", "")
	// Cleared so the run logs the banner and nothing else about the mode: the
	// warning about an ignored app token contains "outbound-only" too, and an
	// exported token on a developer's machine would otherwise satisfy the
	// assertion below without the banner ever being printed.
	t.Setenv("SWITCHBOARD_SLACK_APP_TOKEN", "")

	// SIGTERM below goes to the whole test binary, whose default disposition is
	// to die silently. serve installs a handler, but only while it is running:
	// if it returned early — a config this test got wrong — the signal would
	// take the suite down with no output saying why. This one is held for the
	// test's lifetime so the signal always lands somewhere survivable.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM)
	defer signal.Stop(sigs)

	// serve binds the addresses itself, so the ports have to be named up front:
	// take two from the ephemeral range and hand them straight back.
	ingressAddr, metricsAddr := freeAddr(t), freeAddr(t)

	var runErr error
	logs := captureStderr(t, func() {
		done := make(chan struct{})
		go func() {
			defer close(done)
			runErr = runServe([]string{
				"--platform", "slack", "--outbound-only",
				"--ingress-addr", ingressAddr,
				"--metrics-addr", metricsAddr,
			})
		}()

		// Deferred, so that however the assertions below end, serve is stopped
		// and its goroutine joined: leaving it running would hold stderr, the
		// two ports, and the signal handler for the rest of the binary.
		defer func() {
			if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
				t.Errorf("SIGTERM: %v", err)
			}
			<-done
		}()

		// Wait on the ingress itself, not on /healthz: the two listeners start
		// concurrently, so a healthy metrics endpoint says nothing about
		// whether the port under test is bound yet. Any answer will do — this
		// is a liveness poll, and the status is asserted below.
		waitReady(t, "http://"+ingressAddr+ingressPath)

		// The pod is healthy, which is the claim an outbound-only Deployment's
		// probes rest on.
		if _, code := httpGet(t, "http://"+metricsAddr+"/healthz"); code != http.StatusOK {
			t.Errorf("/healthz = %d, want 200 on an outbound-only run", code)
		}

		// The ingress is listening and authenticating — an unauthorized POST is
		// refused by the ingress itself, without a platform call.
		req, err := http.NewRequest(http.MethodPost, "http://"+ingressAddr+ingressPath,
			strings.NewReader(`{"conversation":"C0123ABCD","text":"digest"}`))
		if err != nil {
			t.Errorf("new request: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer not-the-ingress-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Errorf("POST to the outbound-only ingress: %v", err)
			return
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("POST with a bad token = %d, want 401", resp.StatusCode)
		}
	})

	// A signal is a clean stop, not a failure — including on a run that never
	// had an adapter to stop.
	if runErr != nil {
		if errors.Is(runErr, chat.ErrNoInbound) {
			t.Fatalf("serve ran an adapter with no event source: %v", runErr)
		}
		t.Fatalf("runServe = %v, want a clean shutdown on SIGTERM", runErr)
	}
	// Said out loud, because a gateway nobody can talk to looks exactly like a
	// broken one in the logs otherwise. The whole line, since this is what an
	// operator greps for and the docs quote it verbatim.
	if !strings.Contains(logs, "outbound-only: posting to slack, receiving nothing") {
		t.Errorf("logs did not announce the outbound-only run:\n%s", logs)
	}
	if strings.Contains(logs, "bridging") {
		t.Errorf("logs claim a bridge this process does not have:\n%s", logs)
	}
}

// TestStopListenersCancelsBeforeItDrains: the shutdown has to cancel the run's
// context before waiting on the listeners, because a listener returns when that
// context is done. Wait first and the wait never ends — the only other thing
// that cancels is serve's deferred stop, which cannot run until serve returns,
// which is what the wait is blocking.
//
// The failure that reaches this is an adapter.Run error that did not itself
// cancel the context: a dropped Socket Mode connection giving up, say. The
// process would then sit on this line forever with /healthz answering 200 —
// alive to every probe, doing nothing, and never restarted. That is the one
// state this binary must not be able to reach, so it is pinned here rather than
// left to a live failure to demonstrate.
func TestStopListenersCancelsBeforeItDrains(t *testing.T) {
	stopped := make(chan struct{})
	srvErrs := make(chan error, 1)
	// A listener behaving exactly as serveMetrics and serveIngress do: it
	// returns when, and only when, the context is cancelled.
	go func() {
		<-stopped
		srvErrs <- nil
	}()

	done := make(chan error, 1)
	go func() { done <- stopListeners(func() { close(stopped) }, 1, srvErrs) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("stopListeners = %v, want nil from a listener that stopped cleanly", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stopListeners never returned: it drained the listeners without cancelling them first")
	}
}

// TestStopListenersReportsTheFirstFailure: every listener is drained, and the
// one reported is the first that failed — a second listener returning nil after
// a bind failure must not be what decides the exit code.
func TestStopListenersReportsTheFirstFailure(t *testing.T) {
	want := errors.New("metrics: bind failed")
	srvErrs := make(chan error, 2)
	srvErrs <- want
	srvErrs <- errors.New("ingress: stopped cleanly after the cancel")

	if got := stopListeners(func() {}, 2, srvErrs); !errors.Is(got, want) {
		t.Errorf("stopListeners = %v, want %v", got, want)
	}
	if len(srvErrs) != 0 {
		t.Errorf("%d listener results left undrained", len(srvErrs))
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
func captureStderr(t *testing.T, fn func()) (out string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	saved := os.Stderr
	os.Stderr = w
	copied := make(chan string, 1)
	go func() {
		var b strings.Builder
		_, _ = io.Copy(&b, r)
		copied <- b.String()
	}()
	// Deferred, because a t.Fatal inside fn unwinds through here: without this
	// os.Stderr would stay pointed at a closed pipe and every later test in the
	// binary — including the failure report — would write into nothing.
	defer func() {
		os.Stderr = saved
		_ = w.Close()
		out = <-copied
		_ = r.Close()
	}()
	fn()
	return
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
