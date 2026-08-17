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
		fmt.Fprintf(os.Stderr, "%s: %v\n", prog, err)
		os.Exit(1)
	}
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

func runServe(args []string) error {
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
		"how to derive X-Asserted-Caller from a Slack user: \"email\" (users.info) or \"id\" (raw user ID)")
	richBlocks := fs.Bool("slack-rich-blocks", false,
		"render replies as Slack Block Kit (headers, lists, tables, code); mrkdwn text is always sent as the fallback")
	progressMode := fs.String("progress-mode", "indicator",
		"long-turn feedback: \"indicator\" (placeholder cleared on reply), \"status\" "+
			"(one message edited with the running tool), \"stream\" (a notice per tool + each turn), or \"off\"")
	googleProject := fs.String("google-project", envOr("SWITCHBOARD_GOOGLE_PROJECT", ""),
		"GCP project hosting the Google Chat Pub/Sub subscription (--platform googlechat)")
	googleSub := fs.String("google-subscription", envOr("SWITCHBOARD_GOOGLE_SUBSCRIPTION", ""),
		"Pub/Sub subscription carrying Google Chat events (--platform googlechat)")
	googleCards := fs.String("googlechat-cards", envOr("SWITCHBOARD_GOOGLECHAT_CARDS", "status"),
		"Google Chat card rendering: \"status\" (gateway progress/notice/ack cards), \"rich\" "+
			"(also lay agent replies out as cards), or \"off\"; text is always sent as the fallback")
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
		"comma-separated conversations the outbound ingress may post into (channel IDs, or "+
			"full channel:thread keys); empty = any conversation the bot can reach")
	showVersion := fs.Bool("version", false, "print build identity and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		fmt.Println(version.String(prog))
		return nil
	}

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
	// authenticated, or a platform it does not support, should fail here rather
	// than after the adapter has dialed a chat platform.
	ingressToken := ""
	if *ingressAddr != "" {
		if *platform != "slack" {
			return fmt.Errorf("--ingress-addr is Slack-only for now (got --platform %q)", *platform)
		}
		if ingressToken = os.Getenv(*ingressTokenEnv); ingressToken == "" {
			return fmt.Errorf("no ingress token in $%s (set --ingress-token-env to the right var)", *ingressTokenEnv)
		}
	}

	dc, err := daemon.New(daemon.Config{BaseURL: *daemonURL, BearerToken: token})
	if err != nil {
		return err
	}

	logf := func(format string, a ...any) { fmt.Fprintf(os.Stderr, prog+": "+format+"\n", a...) }

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

	fmt.Fprintf(os.Stderr, "%s: %s\n", prog, version.String(prog))
	fmt.Fprintf(os.Stderr, "%s: bridging %s -> %s\n", prog, adapter.Name(), *daemonURL)
	if ing != nil {
		fmt.Fprintf(os.Stderr, "%s: outbound ingress on %s%s\n", prog, *ingressAddr, ingressPath)
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
		return srvErr
	}
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		return runErr
	}
	fmt.Fprintf(os.Stderr, "%s: shutting down\n", prog)
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

// parseCallerMode validates the --caller-id flag value.
func parseCallerMode(s string) (slack.CallerMode, error) {
	switch slack.CallerMode(s) {
	case slack.CallerEmail:
		return slack.CallerEmail, nil
	case slack.CallerID:
		return slack.CallerID, nil
	default:
		return "", fmt.Errorf("invalid --caller-id %q (want \"email\" or \"id\")", s)
	}
}

// parseProgressMode validates the --progress-mode flag value.
func parseProgressMode(s string) (ProgressMode, error) {
	switch ProgressMode(s) {
	case ProgressOff, ProgressIndicator, ProgressStatus, ProgressStream:
		return ProgressMode(s), nil
	default:
		return "", fmt.Errorf("invalid --progress-mode %q (want \"indicator\", \"status\", \"stream\", or \"off\")", s)
	}
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
