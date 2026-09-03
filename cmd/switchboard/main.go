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
	"github.com/go-steer/switchboard/pkg/approval"
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
	// -c and --config are one setting under two spellings, bound to one
	// variable so the last one given wins rather than one shadowing the other.
	// core-agent shipped -c alone and a distroless Deployment written as
	// args: ["--config=..."] exited at flag-parse during a live demo
	// (go-steer/core-agent#209); the alias costs a line.
	var cfgPath string
	const cfgUsage = "path to a JSON gateway config file (#71); a flag beats $SWITCHBOARD_*, which beats the file"
	fs.StringVar(&cfgPath, "config", "", cfgUsage)
	fs.StringVar(&cfgPath, "c", "", "shorthand for --config")

	// No environment lookups in the defaults below. A flag whose default came from
	// the environment cannot be told apart after parsing from one nobody passed,
	// so the config file could never be layered underneath it — see resolver.
	daemonURL := fs.String("daemon-url", "http://127.0.0.1:7777",
		"core-agent daemon base URL (no trailing slash)")
	tokenEnv := fs.String("token-env", "SWITCHBOARD_DAEMON_TOKEN",
		"env var holding the daemon bearer token (never pass the token as a bare flag)")
	platform := fs.String("platform", "slack",
		"chat platform to bridge: \"slack\" (Socket Mode) or \"googlechat\" (Pub/Sub)")
	outboundOnly := fs.Bool("outbound-only", false,
		"post but never receive: open no Slack Socket Mode connection and build no Google "+
			"Chat Pub/Sub client, and require none of the credentials, grants or "+
			"subscriptions receiving takes. Needs --ingress-addr, which is then the only "+
			"way in; the daemon token is not read at all")
	appTokenEnv := fs.String("slack-app-token-env", "SWITCHBOARD_SLACK_APP_TOKEN",
		"env var holding the Slack Socket Mode app-level token (xapp-...); required unless "+
			"--outbound-only")
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
	approvals := fs.Bool("approvals", false,
		"relay the agent's permission prompts into the conversation as buttons, and send back "+
			"what someone presses; off by default because it is a grant — see --approvers for "+
			"who it goes to, and some of those answers outlive the request")
	approvers := fs.String("approvers", approversChannel,
		"who may answer a permission prompt (--approvals): \"channel\" for anyone who can post "+
			"in the conversation, or a comma-separated list of the identities switchboard "+
			"asserts (emails, or platform IDs under --caller-id \"id\")")
	googleProject := fs.String("google-project", "",
		"GCP project hosting the Google Chat Pub/Sub subscription (--platform googlechat)")
	googleSub := fs.String("google-subscription", "",
		"Pub/Sub subscription carrying Google Chat events (--platform googlechat); required "+
			"with --google-project unless --outbound-only")
	googleCards := fs.String("googlechat-cards", defaultCardMode,
		"Google Chat card rendering: \"rich\" (gateway cards, and a structured agent reply "+
			"laid out as a card), \"status\" (gateway progress/notice/ack cards only), or "+
			"\"off\"; text is always sent as the fallback")
	googleLogEvents := fs.Bool("googlechat-log-events", false,
		"log every inbound Google Chat payload verbatim, for capturing decoder fixtures; "+
			"the payload includes message text and sender identity, so leave this off in production")
	googleCommands := fs.String("googlechat-commands", "",
		"comma-separated Chat app-command ID to gateway verb mappings (e.g. \"1=progress,2=help\"), "+
			"matching the command IDs configured in the Chat API console")
	metricsAddr := fs.String("metrics-addr", "",
		"Prometheus /metrics + /healthz listener address (host:port); empty = disabled")
	ingressAddr := fs.String("ingress-addr", "",
		"outbound-ingress listener address (host:port) for POST/PATCH /v1/messages; empty = disabled")
	ingressTokenEnv := fs.String("ingress-token-env", "SWITCHBOARD_INGRESS_TOKEN",
		"env var holding the bearer token callers must present to the outbound ingress")
	ingressAllow := fs.String("ingress-allow", "",
		"comma-separated conversations the outbound ingress may post into (Slack channel IDs "+
			"or channel:thread_ts; Chat spaces/AAA or spaces/AAA:spaces/AAA/threads/BBB); "+
			"empty = any conversation the bot can reach")
	logFormat := fs.String("log-format", string(logging.Text),
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

	// The config file, before anything reads a setting — including the logger,
	// because log_format is itself file-settable.
	//
	// Only when asked for: core-agent discovers .agents/config.json in the
	// working directory and switchboard deliberately does not. A CLI picking up
	// the config of the directory you are standing in is a convenience; a
	// long-lived gateway changing who may approve a production change because of
	// a file that appeared next to it is not. A named file that is not there is
	// a startup error rather than a fall back to the defaults — starting anyway
	// is precisely how a narrowed approver list would quietly become an open one.
	if !hasFlag(fs, "config", "c") {
		cfgPath = os.Getenv("SWITCHBOARD_CONFIG")
	}
	cfg := &Config{}
	if cfgPath != "" {
		if cfg, err = loadConfig(cfgPath); err != nil {
			return err
		}
	}

	res := newResolver(fs)
	res.str("daemon-url", "SWITCHBOARD_DAEMON_URL", cfg.DaemonURL, daemonURL)
	res.str("token-env", "", cfg.TokenEnv, tokenEnv)
	res.str("platform", "", cfg.Platform, platform)
	res.boolean("outbound-only", "", cfg.OutboundOnly, outboundOnly)
	res.str("slack-app-token-env", "", cfg.AppTokenEnv, appTokenEnv)
	res.str("slack-bot-token-env", "", cfg.BotTokenEnv, botTokenEnv)
	res.str("caller-id", "", cfg.CallerID, callerID)
	res.boolean("slack-rich-blocks", "", cfg.RichBlocks, richBlocks)
	res.str("google-project", "SWITCHBOARD_GOOGLE_PROJECT", cfg.GoogleProject, googleProject)
	res.str("google-subscription", "SWITCHBOARD_GOOGLE_SUBSCRIPTION", cfg.GoogleSub, googleSub)
	res.str("googlechat-cards", "SWITCHBOARD_GOOGLECHAT_CARDS", cfg.GoogleCards, googleCards)
	res.boolean("googlechat-log-events", "", cfg.GoogleLogEvents, googleLogEvents)
	res.str("metrics-addr", "SWITCHBOARD_METRICS_ADDR", cfg.MetricsAddr, metricsAddr)
	res.str("ingress-addr", "SWITCHBOARD_INGRESS_ADDR", cfg.IngressAddr, ingressAddr)
	res.str("ingress-token-env", "", cfg.IngressTokenEnv, ingressTokenEnv)
	res.str("log-format", "SWITCHBOARD_LOG_FORMAT", cfg.LogFormat, logFormat)
	// The channel-scopable four resolve into the same variables, and then again
	// per channel below: what lands here is the posture for a channel the file
	// says nothing about.
	res.boolean("approvals", "", cfg.Defaults.Approvals, approvals)
	res.str("progress-mode", "", cfg.Defaults.ProgressMode, progressMode)
	res.boolean("show-usage", "", cfg.Defaults.ShowUsage, showUsage)
	if err := res.err(); err != nil {
		return err
	}
	allowList := res.list("ingress-allow", "SWITCHBOARD_INGRESS_ALLOW", *ingressAllow, cfg.IngressAllow)

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

	// inbound records whether this run has an event source to consume. It is
	// declared, not inferred from whether a credential happens to be set: a
	// bridge whose app token is emptied by a bad rotation must keep failing
	// loudly rather than quietly becoming a process that posts and answers
	// nobody (#23).
	inbound := !*outboundOnly

	callerMode, err := parseCallerMode(*callerID)
	if err != nil {
		return err
	}

	// After the caller mode, because an approver list is only meaningful against
	// the identity this gateway will assert — see parseApprovers.
	if v, ok := os.LookupEnv(envApprovers); ok && strings.TrimSpace(v) == "" && !res.passed("approvers") {
		// An empty environment value reads as unset everywhere else here, which
		// is right for a cosmetic default and wrong for this one: an unset
		// ConfigMap key renders as the empty string, and quietly falling through
		// to the file — or to "channel" — would widen the grant at the moment
		// somebody was trying to set it.
		//
		// Not when the flag was passed, though. The flag outranks the variable
		// everywhere else, and an operator who has just written the list out on
		// the command line is not being saved from anything by a refusal to
		// start over a variable that is about to be ignored.
		return fmt.Errorf("%s is set but empty: pass %q to let anyone in the conversation answer", envApprovers, approversChannel)
	}
	approverFields := res.list("approvers", envApprovers, *approvers, cfg.Defaults.Approvers)
	fileApprovers := ""
	if cfg.Defaults.Approvers != nil {
		fileApprovers = "defaults.approvers"
	}
	policy, err := parseApproverList(approverFields, callerMode)
	if err != nil {
		return fmt.Errorf("invalid value for %s: %w", res.origin("approvers", envApprovers, fileApprovers), err)
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
	appCommands, err := res.mapping("googlechat-commands", "SWITCHBOARD_GOOGLECHAT_COMMANDS", *googleCommands, cfg.GoogleCommands)
	if err != nil {
		return err
	}

	// The per-channel table, last of the settings work because it is built on
	// top of everything above it: the defaults it overlays, the platform whose
	// channel IDs its keys have to look like, and the caller mode its approver
	// lists are checked against.
	//
	// Note the one place precedence inverts: a channels block wins over an
	// explicitly passed flag, because the flag says "everywhere" and the block
	// says "here", and the narrower scope is the one that was meant. Documented
	// in the README next to the precedence rule, since it is the one rule a
	// reader would otherwise get wrong.
	defaults := channelSettings{
		progress:  progress,
		approvals: *approvals,
		showUsage: *showUsage,
		approvers: policy,
	}
	byChannel, err := channelsFrom(cfg, defaults, *platform, callerMode)
	if err != nil {
		return err
	}
	// Said out loud, because the file is the one input to this run that is not
	// in the process args: an operator reading the log of a gateway that is
	// behaving oddly should not have to infer which file it read, or whether it
	// read one at all. The channel IDs go in the line too — they are the keys
	// that had to match what the platform sends, and seeing them is how a block
	// written for a room the bot is not in gets noticed.
	if cfgPath != "" {
		if ids := channelIDs(byChannel); len(ids) > 0 {
			logf("config: read %s, %d configured channel(s): %s", cfgPath, len(ids), strings.Join(ids, " "))
		} else {
			logf("config: read %s, no configured channels", cfgPath)
		}
	}

	// Whether anything, anywhere, relays permission prompts. Not the same as
	// *approvals since #71: a file may leave the default off and turn one
	// channel on, and the client is built once for the whole process.
	wantApprovals := *approvals || anyApprovals(byChannel)

	// Only a bridged run talks to the daemon: the ingress posts straight through
	// the adapter. So an outbound-only deployment is not asked for a bearer
	// token it would never present.
	var dc *daemon.Client
	var ac *approval.Client
	if inbound {
		token := os.Getenv(*tokenEnv)
		if token == "" {
			return fmt.Errorf("no daemon token in $%s (set --token-env to the right var)", *tokenEnv)
		}
		dcfg := daemon.Config{BaseURL: *daemonURL, BearerToken: token}
		if dc, err = daemon.New(dcfg); err != nil {
			return err
		}
		// Same daemon, same credential, different routes — and built here
		// rather than inside the router so that a run with approvals off
		// everywhere holds no client for a surface it does not offer.
		if wantApprovals {
			if ac, err = approval.New(dcfg); err != nil {
				return err
			}
		}
	}

	// An outbound-only run is a real deployment shape — a monitoring loop that
	// posts digests receives nothing — but only with the ingress. With neither
	// there is no work to do at all, and a container that starts and sits there
	// is worse than one that refuses: nothing is wrong, nothing is happening,
	// and no probe distinguishes the two.
	//
	// Last of the checks that need no credentials, and still ahead of the
	// adapter: after the flag values above, so a typo in one of them is
	// reported as a typo rather than hidden behind this; before anything is
	// built, so it is the same refusal on both platforms and reaching it costs
	// nothing.
	if !inbound && *ingressAddr == "" {
		return errors.New("nothing to do: --outbound-only and --ingress-addr is unset, " +
			"so this process could neither receive nor be asked to post")
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

	var adapter chat.Adapter
	switch *platform {
	case "slack":
		appToken := os.Getenv(*appTokenEnv)
		switch {
		case *outboundOnly && appToken != "":
			// Not an error — the run is unambiguous — but worth saying, because
			// the token is doing nothing and somebody provisioned it expecting
			// otherwise.
			logf("warning: --outbound-only, so $%s is ignored and no Socket Mode connection is opened", *appTokenEnv)
			appToken = ""
		case !*outboundOnly && appToken == "":
			return fmt.Errorf("no Slack app token in $%s (set it, or pass --outbound-only "+
				"if this deployment only posts)", *appTokenEnv)
		}
		adapter, err = slack.New(slack.Config{
			AppToken:   appToken,
			BotToken:   os.Getenv(*botTokenEnv),
			CallerMode: callerMode,
			RichBlocks: *richBlocks,
			Logf:       logf,
		})
		if err != nil {
			// The bot token is the only credential slack.New still insists on;
			// naming the app-token var here would send an outbound-only
			// operator after a token they deliberately left out (#23).
			return fmt.Errorf("slack adapter: %w (set $%s)", err, *botTokenEnv)
		}
	case "googlechat":
		project, sub := *googleProject, *googleSub
		switch {
		case *outboundOnly && (project != "" || sub != ""):
			// The env vars are named alongside the flags because they are where
			// a Deployment usually sets these, and an operator told only about
			// flags they never passed would go looking in the wrong place.
			logf("warning: --outbound-only, so --google-project/--google-subscription " +
				"($SWITCHBOARD_GOOGLE_PROJECT/$SWITCHBOARD_GOOGLE_SUBSCRIPTION) are " +
				"ignored and no Pub/Sub client is built")
			project, sub = "", ""
		case !*outboundOnly && sub == "":
			return errors.New("no --google-subscription (set it together with --google-project, " +
				"or pass --outbound-only if this deployment only posts)")
		}
		adapter, err = googlechat.New(googlechat.Config{
			ProjectID:      project,
			SubscriptionID: sub,
			Cards:          cardMode,
			CallerMode:     callerMode,
			Commands:       appCommands,
			LogEvents:      *googleLogEvents,
			Logf:           logf,
		})
		if err != nil {
			// No flag hint: this also carries ADC failures, which naming the
			// subscription flags would misattribute.
			return fmt.Errorf("googlechat adapter: %w", err)
		}
	default:
		return fmt.Errorf("invalid --platform %q (want \"slack\" or \"googlechat\")", *platform)
	}
	m := newMetrics()
	// The router is what an inbound turn is bridged through, so an outbound-only
	// run has none — and needs no daemon to point it at.
	var router *Router
	if inbound {
		router = NewRouter(dc, adapter, progress, m, logf)
		configureRouter(router, ac, defaults, byChannel)
	}
	if wantApprovals {
		switch {
		case !inbound:
			logf("warning: approvals answer prompts raised by an agent turn, and an outbound-only run has none")
		case !*approvals:
			// On in a channel and off by default. Worth its own line: the two
			// branches below describe the default posture, and saying either of
			// them here would describe a posture most conversations do not have.
			logf("approvals: permission prompts go to the conversation in %d configured channel(s) and nowhere else",
				countApprovals(byChannel))
		case policy.open():
			// Said on every start, like the outbound-only banner, because this
			// one is a grant: from here on, anyone who can post in a
			// conversation can answer that session's permission prompts, and
			// some of those answers outlive the request.
			logf("approvals: permission prompts go to the conversation, and anyone who can post there can answer them")
		default:
			// The narrowed posture is announced too, and by count rather than
			// by name: the names are already in the process args for anyone
			// entitled to read them, and a log line is the wrong place to
			// publish a list of who can approve production changes.
			logf("approvals: permission prompts go to the conversation, and %d named approver(s) may answer them", len(policy.allowed))
		}
		// Counted, not named, for the same reason. Reported at all because a
		// channel list replaces the default rather than narrowing it, so a run
		// whose banner says "3 named approvers" may still have a room where
		// anyone can answer — and that is exactly the fact worth one line.
		//
		// Not on an outbound-only run: nothing above has described a posture
		// there, only warned that there are no prompts to have one about, and
		// counting the approvers of a feature that cannot fire reads as a
		// working configuration.
		if open, named := approverSpread(byChannel); inbound && open+named > 0 {
			logf("approvals: %d configured channel(s) let anyone answer, %d name their own approvers", open, named)
		}
	} else if !policy.open() {
		logf("warning: an approver list names who may answer permission prompts, and approvals are off")
	}
	if *showUsage {
		switch {
		case !inbound:
			logf("warning: --show-usage describes an agent turn, and an outbound-only run has none")
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
		icfg := ingressConfig{
			Token:   ingressToken,
			Allow:   allowList,
			Out:     adapter,
			Metrics: m,
			Logf:    logf,
		}
		// Only when there is a router to bind against. Assigned inside the
		// branch and not from the variable directly: a nil *Router in an
		// interface field is not a nil interface, and the ingress reads that
		// field to decide whether a caller may name a session at all.
		if router != nil {
			icfg.Bind = router
		}
		ing, err = newIngress(icfg)
		if err != nil {
			return err
		}
		if len(allowList) == 0 {
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
	if inbound {
		logf("bridging %s -> %s", adapter.Name(), *daemonURL)
	} else {
		// Said plainly, because it is the difference between a quiet gateway
		// and a broken one: nobody can talk to the agent through this process.
		logf("outbound-only: posting to %s, receiving nothing", adapter.Name())
	}
	if ing != nil {
		logf("outbound ingress on %s%s", *ingressAddr, ingressPath)
	}

	// With no event source there is nothing to run, so wait on the same ctx
	// adapter.Run would have: a signal, or a listener failure that called stop.
	var runErr error
	if inbound {
		runErr = adapter.Run(ctx, router)
	} else {
		<-ctx.Done()
		runErr = ctx.Err()
	}

	// The run is over: bring the listeners down with it and collect what they
	// have to say.
	if srvErr := stopListeners(stop, listeners, srvErrs); srvErr != nil {
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

// stopListeners ends the run's listeners and reports the first failure any of
// them had, having drained them all so a later one is not reported as the exit
// code ahead of the one that actually failed. A bind failure has usually
// already cancelled ctx and been logged where it happened.
//
// It cancels *before* it drains, and that order is the whole point: the servers
// return when ctx is done, and ctx is only cancelled here or by serve's
// deferred stop — which cannot run until serve returns, which is what this is
// waiting to do. Draining first parks the process forever on an adapter.Run
// failure that did not itself cancel ctx, with /healthz still answering 200:
// alive to every probe, doing nothing, and never restarted.
func stopListeners(stop func(), listeners int, srvErrs <-chan error) error {
	stop()
	var srvErr error
	for range listeners {
		if err := <-srvErrs; err != nil && srvErr == nil {
			srvErr = err
		}
	}
	return srvErr
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
