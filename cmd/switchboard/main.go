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

// Command switchboard is the chat-gateway companion for core-agent: it
// bridges chat platforms (Slack, Google Chat) onto the frozen core-agent
// daemon contract so operators can drive agents from a thread. It is a
// distroless multicall binary mirroring k8s-lookout's cmd/lookout — the
// default subcommand is `serve`; `version` reports build identity.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/go-steer/switchboard/internal/logging"
	"github.com/go-steer/switchboard/internal/version"
	"github.com/go-steer/switchboard/pkg/chat"
	"github.com/go-steer/switchboard/pkg/chat/googlechat"
	"github.com/go-steer/switchboard/pkg/chat/slack"
	"github.com/go-steer/switchboard/pkg/daemon"
)

const prog = "switchboard"

func main() {
	args := os.Args[1:]
	sub := "serve"
	if len(args) > 0 && !isFlag(args[0]) {
		sub, args = args[0], args[1:]
	}

	var err error
	switch sub {
	case "serve":
		err = runServe(args)
	case "version":
		fmt.Println(version.String(prog))
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "%s: unknown subcommand %q\n\n", prog, sub)
		usage()
		os.Exit(2)
	}
	if err != nil {
		// serve reports its own failures once it has a logger, so that a
		// --log-format json stream does not end on a line no collector can
		// parse — a crash loop being exactly when that line is read.
		var logged loggedError
		if !errors.As(err, &logged) {
			fmt.Fprintf(os.Stderr, "%s: %v\n", prog, err)
		}
		os.Exit(1)
	}
}

// loggedError wraps an error serve has already written to the log, so main
// exits non-zero without printing it a second time. It delegates Error and
// Unwrap to what it carries, so a caller that inspects the error — a test, or
// anything matching on it — sees no difference.
type loggedError struct{ error }

func (e loggedError) Unwrap() error { return e.error }

// reportOnce logs err and marks it as reported, so main exits non-zero without
// printing it again. An error that is already marked — a listener's bind
// failure, logged where it happened with the name of the listener attached — is
// passed through untouched rather than said a second time in a vaguer form.
func reportOnce(logf func(string, ...any), err error) error {
	if err == nil {
		return nil
	}
	var logged loggedError
	if errors.As(err, &logged) {
		return err
	}
	logf("%v", err)
	return loggedError{err}
}

func isFlag(s string) bool { return len(s) > 0 && s[0] == '-' }

func usage() {
	fmt.Fprintf(os.Stderr, `%[1]s — chat-gateway companion for core-agent

Usage:
  %[1]s serve   [flags]   bridge chat platforms onto the core-agent daemon (default)
  %[1]s version           print build identity
  %[1]s help              show this help
`, prog)
}

// defaultCardMode is what --googlechat-cards selects when nothing else does.
// rich rather than status, because a card is not chunked and so is the only
// mode a long fenced answer cannot break in, and because the render is already
// conditional on the answer having structure — the mode is a no-op for
// conversational traffic and only escalates when there is something to lay out.
const defaultCardMode = string(googlechat.CardsRich)

func runServe(args []string) (err error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	daemonURL := fs.String("daemon-url", envOr("SWITCHBOARD_DAEMON_URL", "http://127.0.0.1:7777"),
		"core-agent daemon base URL (no trailing slash)")
	tokenEnv := fs.String("token-env", "SWITCHBOARD_DAEMON_TOKEN",
		"env var holding the daemon bearer token (never pass the token as a bare flag)")
	platform := fs.String("platform", "slack",
		"chat platform to bridge: \"slack\" (Socket Mode) or \"googlechat\" (Pub/Sub)")
	appTokenEnv := fs.String("slack-app-token-env", "SWITCHBOARD_SLACK_APP_TOKEN",
		"env var holding the Slack Socket Mode app-level token (xapp-...)")
	botTokenEnv := fs.String("slack-bot-token-env", "SWITCHBOARD_SLACK_BOT_TOKEN",
		"env var holding the Slack bot user OAuth token (xoxb-...)")
	callerID := fs.String("caller-id", "email",
		"how to derive X-Asserted-Caller: \"email\" (Slack users.info, Chat event payload) or \"id\" (raw platform user ID)")
	richBlocks := fs.Bool("slack-rich-blocks", false,
		"render replies as Slack Block Kit (headers, lists, tables, code); mrkdwn text is always sent as the fallback")
	progressMode := fs.String("progress-mode", "indicator",
		"long-turn feedback: \"indicator\" (placeholder cleared on reply), \"status\" "+
			"(one message edited with the running tool), \"stream\" (a notice per tool + each turn), or \"off\"")
	showUsage := fs.Bool("show-usage", false,
		"append each answer's model, tokens, cost and latency as a footer; rich renders only "+
			"(--slack-rich-blocks, --googlechat-cards rich), and off by default because it "+
			"discloses spend to everyone in the conversation")
	googleProject := fs.String("google-project", envOr("SWITCHBOARD_GOOGLE_PROJECT", ""),
		"GCP project hosting the Google Chat Pub/Sub subscription (--platform googlechat)")
	googleSub := fs.String("google-subscription", envOr("SWITCHBOARD_GOOGLE_SUBSCRIPTION", ""),
		"Pub/Sub subscription carrying Google Chat events (--platform googlechat)")
	googleCards := fs.String("googlechat-cards", envOr("SWITCHBOARD_GOOGLECHAT_CARDS", defaultCardMode),
		"Google Chat card rendering: \"rich\" (gateway cards, and a structured agent reply "+
			"laid out as a card), \"status\" (gateway progress/notice/ack cards only), or "+
			"\"off\"; text is always sent as the fallback")
	googleLogEvents := fs.Bool("googlechat-log-events", false,
		"log every inbound Google Chat payload verbatim, for capturing decoder fixtures; "+
			"the payload includes message text and sender identity, so leave this off in production")
	googleCommands := fs.String("googlechat-commands", envOr("SWITCHBOARD_GOOGLECHAT_COMMANDS", ""),
		"comma-separated Chat app-command ID to gateway verb mappings (e.g. \"1=progress,2=help\"), "+
			"matching the command IDs configured in the Chat API console")
	metricsAddr := fs.String("metrics-addr", envOr("SWITCHBOARD_METRICS_ADDR", ""),
		"Prometheus /metrics + /healthz listener address (host:port); empty = disabled")
	ingressAddr := fs.String("ingress-addr", envOr("SWITCHBOARD_INGRESS_ADDR", ""),
		"outbound-ingress listener address (host:port) for POST/PATCH /v1/messages; empty = disabled")
	ingressTokenEnv := fs.String("ingress-token-env", "SWITCHBOARD_INGRESS_TOKEN",
		"env var holding the bearer token callers must present to the outbound ingress")
	ingressAllow := fs.String("ingress-allow", envOr("SWITCHBOARD_INGRESS_ALLOW", ""),
		"comma-separated conversations the outbound ingress may post into (Slack channel IDs "+
			"or channel:thread_ts; Chat spaces/AAA or spaces/AAA:spaces/AAA/threads/BBB); "+
			"empty = any conversation the bot can reach")
	logFormat := fs.String("log-format", envOr("SWITCHBOARD_LOG_FORMAT", string(logging.Text)),
		"log rendering: \"text\" (timestamped lines for a terminal) or \"json\" (one object "+
			"per line for a collector)")
	showVersion := fs.Bool("version", false, "print build identity and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		fmt.Println(version.String(prog))
		return nil
	}

	format, ok := logging.ParseFormat(*logFormat)
	if !ok {
		return fmt.Errorf("invalid --log-format %q (want \"text\" or \"json\")", *logFormat)
	}
	// Every component logs through this one hook.
	logf := logging.New(os.Stderr, format, prog)

	// From here on a failure is logged rather than handed back to main to
	// print: the whole point of json is a stream a collector can read, and a
	// startup that fails is the run whose last line matters most. Everything
	// above — a flag that will not parse, a --log-format nobody can render —
	// happens before there is a logger to say it through, so main still
	// reports those plainly.
	defer func() { err = reportOnce(logf, err) }()

	// Build identity first, ahead of the config checks, so an operator whose
	// flags are rejected still learns which build rejected them.
	logf("%s", version.String(prog))

	token := os.Getenv(*tokenEnv)
	if token == "" {
		return fmt.Errorf("no daemon token in $%s (set --token-env to the right var)", *tokenEnv)
	}

	callerMode, err := parseCallerMode(*callerID)
	if err != nil {
		return err
	}

	progress, err := parseProgressMode(*progressMode)
	if err != nil {
		return err
	}

	// Both Google Chat knobs are validated here rather than inside the adapter
	// so a typo fails at startup instead of after Pub/Sub has been dialed.
	cardMode, ok := googlechat.ParseCardMode(*googleCards)
	if !ok {
		return fmt.Errorf("invalid --googlechat-cards %q (want \"off\", \"status\" or \"rich\")", *googleCards)
	}
	appCommands, err := parseAppCommands(*googleCommands)
	if err != nil {
		return err
	}

	// Validate the outbound-ingress config up front: a caller that cannot be
	// authenticated should fail here rather than after the adapter has dialed a
	// chat platform. The ingress itself is platform-agnostic — it speaks
	// chat.Reply and chat.MessageRef to whichever adapter serve built (#39).
	ingressToken := ""
	if *ingressAddr != "" {
		if ingressToken = os.Getenv(*ingressTokenEnv); ingressToken == "" {
			return fmt.Errorf("no ingress token in $%s (set --ingress-token-env to the right var)", *ingressTokenEnv)
		}
	}

	dc, err := daemon.New(daemon.Config{BaseURL: *daemonURL, BearerToken: token})
	if err != nil {
		return err
	}

	var adapter chat.Adapter
	switch *platform {
	case "slack":
		adapter, err = slack.New(slack.Config{
			AppToken:   os.Getenv(*appTokenEnv),
			BotToken:   os.Getenv(*botTokenEnv),
			CallerMode: callerMode,
			RichBlocks: *richBlocks,
			Logf:       logf,
		})
		if err != nil {
			return fmt.Errorf("slack adapter: %w (set $%s and $%s)", err, *appTokenEnv, *botTokenEnv)
		}
	case "googlechat":
		adapter, err = googlechat.New(googlechat.Config{
			ProjectID:      *googleProject,
			SubscriptionID: *googleSub,
			Cards:          cardMode,
			CallerMode:     callerMode,
			Commands:       appCommands,
			LogEvents:      *googleLogEvents,
			Logf:           logf,
		})
		if err != nil {
			return fmt.Errorf("googlechat adapter: %w (set --google-project and --google-subscription)", err)
		}
	default:
		return fmt.Errorf("invalid --platform %q (want \"slack\" or \"googlechat\")", *platform)
	}
	m := newMetrics()
	router := NewRouter(dc, adapter, progress, m, logf)
	router.setShowUsage(*showUsage)
	if *showUsage {
		switch {
		case *platform == "slack" && !*richBlocks:
			logf("warning: --show-usage needs --slack-rich-blocks; the footer will not be shown")
		case *platform == "googlechat" && cardMode != googlechat.CardsRich:
			logf("warning: --show-usage needs --googlechat-cards rich; the footer will not be shown")
		}
	}

	// The outbound ingress posts through the same adapter egress the router
	// replies with, so it is built once the adapter exists — and before
	// anything starts, so a bad config fails at startup, not on the first
	// caller.
	var ing *ingress
	if *ingressAddr != "" {
		allow := splitList(*ingressAllow)
		ing, err = newIngress(ingressConfig{
			Token:   ingressToken,
			Allow:   allow,
			Out:     adapter,
			Metrics: m,
			Logf:    logf,
		})
		if err != nil {
			return err
		}
		if len(allow) == 0 {
			logf("warning: outbound ingress on %s may post into ANY conversation "+
				"the bot can reach; narrow it with --ingress-allow", *ingressAddr)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Serve the optional listeners: /metrics + /healthz, and the outbound
	// ingress. A bind failure cancels ctx (stop closes the Done channel,
	// unblocking adapter.Run) so the process exits rather than running without
	// a surface something depends on — a deployment's probes, or a caller's
	// digests. srvErrs carries those failures so serve reports one as a
	// non-zero exit.
	srvErrs := make(chan error, 2)
	listeners := 0
	serveOptional := func(name, addr string, fn func() error) {
		if addr == "" {
			return
		}
		listeners++
		go func() {
			err := fn()
			if err != nil {
				logf("%s server: %v", name, err)
				stop()
			}
			srvErrs <- err
		}()
	}
	serveOptional("metrics", *metricsAddr, func() error { return serveMetrics(ctx, *metricsAddr, m) })
	serveOptional("ingress", *ingressAddr, func() error { return serveIngress(ctx, *ingressAddr, ing) })

	// Through logf, not straight to stderr: these are the first lines of the
	// run, and a JSON stream that opens with three unparseable ones is worse
	// than no banner at all.
	logf("bridging %s -> %s", adapter.Name(), *daemonURL)
	if ing != nil {
		logf("outbound ingress on %s%s", *ingressAddr, ingressPath)
	}
	runErr := adapter.Run(ctx, router)

	// A listener's bind failure cancels ctx, so adapter.Run returns
	// context.Canceled; surface the underlying error as the real cause. Drain
	// every listener so none is reported as the exit code before the one that
	// actually failed.
	var srvErr error
	for range listeners {
		if err := <-srvErrs; err != nil && srvErr == nil {
			srvErr = err
		}
	}
	if srvErr != nil {
		// serveOptional already logged this one, with the name of the listener
		// that failed attached.
		return loggedError{srvErr}
	}
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		return runErr
	}
	logf("shutting down")
	return nil
}

// splitList parses a comma-separated flag value into its non-empty, trimmed
// entries.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseCallerMode validates the --caller-id flag value. One flag serves
// every platform: the daemon keys per-caller credentials off whatever is
// asserted, so the same human must not be an email on Slack and a resource
// name on Chat.
func parseCallerMode(s string) (chat.CallerMode, error) {
	mode, ok := chat.ParseCallerMode(s)
	if !ok {
		return "", fmt.Errorf("invalid --caller-id %q (want \"email\" or \"id\")", s)
	}
	return mode, nil
}

// parseProgressMode validates the --progress-mode flag value against
// progressModes, the same list the `progress` command offers and reports
// through chat.CommandChoices.
func parseProgressMode(s string) (ProgressMode, error) {
	for _, m := range progressModes {
		if ProgressMode(s) == m {
			return m, nil
		}
	}
	quoted := make([]string, 0, len(progressModes))
	for _, m := range progressModes {
		quoted = append(quoted, strconv.Quote(string(m)))
	}
	return "", fmt.Errorf("invalid --progress-mode %q (want %s)", s, strings.Join(quoted, ", "))
}

// parseAppCommands parses the --googlechat-commands mapping. Chat identifies an
// app command by the numeric ID configured in the API console and never by its
// name, so the operator has to tell switchboard which ID means which verb.
func parseAppCommands(s string) (map[int64]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	out := map[int64]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		key, verb, found := strings.Cut(pair, "=")
		id, err := strconv.ParseInt(strings.TrimSpace(key), 10, 64)
		verb = strings.TrimSpace(verb)
		if !found || err != nil || verb == "" {
			return nil, fmt.Errorf("invalid --googlechat-commands entry %q (want \"<id>=<verb>\")", pair)
		}
		out[id] = verb
	}
	return out, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
