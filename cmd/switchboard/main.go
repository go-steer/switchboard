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
	metricsAddr := fs.String("metrics-addr", envOr("SWITCHBOARD_METRICS_ADDR", ""),
		"Prometheus /metrics + /healthz listener address (host:port); empty = disabled")
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Serve /metrics + /healthz when an address is configured. A bind failure
	// cancels ctx (stop closes the Done channel, unblocking adapter.Run) so the
	// process exits rather than running without the health surface a deployment's
	// probes depend on. metricsErr carries that failure so serve reports it as a
	// non-zero exit; it holds nil on clean shutdown or when metrics are disabled.
	metricsErr := make(chan error, 1)
	if *metricsAddr != "" {
		go func() {
			err := serveMetrics(ctx, *metricsAddr, m)
			if err != nil {
				logf("metrics server: %v", err)
				stop()
			}
			metricsErr <- err
		}()
	} else {
		metricsErr <- nil
	}

	fmt.Fprintf(os.Stderr, "%s: %s\n", prog, version.String(prog))
	fmt.Fprintf(os.Stderr, "%s: bridging %s -> %s\n", prog, adapter.Name(), *daemonURL)
	runErr := adapter.Run(ctx, router)

	// A metrics bind failure cancels ctx, so adapter.Run returns
	// context.Canceled; surface the underlying metrics error as the real cause.
	if err := <-metricsErr; err != nil {
		return err
	}
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		return runErr
	}
	fmt.Fprintf(os.Stderr, "%s: shutting down\n", prog)
	return nil
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

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
