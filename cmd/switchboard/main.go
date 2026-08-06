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
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-steer/switchboard/internal/version"
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

	dc, err := daemon.New(daemon.Config{BaseURL: *daemonURL, BearerToken: token})
	if err != nil {
		return err
	}
	_ = dc // router + chat adapters wired in W1 phases; see docs/DESIGN.md

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(os.Stderr, "%s: %s\n", prog, version.String(prog))
	fmt.Fprintf(os.Stderr, "%s: scaffold only — no chat adapters registered yet (see docs/DESIGN.md)\n", prog)
	<-ctx.Done()
	fmt.Fprintf(os.Stderr, "%s: shutting down\n", prog)
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
