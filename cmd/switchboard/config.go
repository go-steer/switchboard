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
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/go-steer/switchboard/pkg/approval"
	"github.com/go-steer/switchboard/pkg/chat"
)

// The gateway configuration file (#71).
//
// switchboard grew 23 flags, nine of which acquired a SWITCHBOARD_* environment
// alternative one at a time and fourteen of which did not, so a Deployment sets
// some settings under args: and others under env: with no rule explaining
// which. The file fixes that by giving every setting one home, but it is not
// the reason the file exists. Two things flags cannot do at all are:
//
//   - hold structure. --approvers, --googlechat-commands and --ingress-allow are
//     a list, a map and a list flattened into single strings, and most of
//     parseApprovers is the cost of getting one of them back out again.
//   - scope a setting to a channel. #65 wanted "the SRE channel approves prod, a
//     scratch channel approves nothing" and shipped global because a flag has
//     nowhere to put the channel.
//
// JSON rather than YAML, and -c/--config rather than only the long form, both
// because core-agent already made those choices and AGENTS.md says mirror it.
// The alias is not a courtesy: core-agent shipped -c alone and a distroless
// Deployment written as args: ["--config=..."] exited at flag-parse during a
// live demo (go-steer/core-agent#209).
//
// One thing deliberately not mirrored: core-agent discovers .agents/config.json
// when no path is given, and switchboard reads a file only when told to. A CLI
// picking up the config of the directory you are standing in is a convenience;
// a long-lived gateway silently changing who may approve a production change
// because of a file in its working directory is not. The path is always
// explicit, and a --config naming a file that is not there is a startup error
// rather than a fallback to flag defaults — starting anyway is precisely how a
// narrowed approver list would quietly become an open one.

// Config is the on-disk gateway configuration. Every field is a pointer or a
// map so that absent is distinguishable from zero: a file that omits
// "approvals" must defer to the flag, while one that sets it to false must beat
// the flag's default. Nothing here is required — a file may set one setting and
// say nothing about the rest.
type Config struct {
	// Process-wide settings, none of which vary by channel.
	DaemonURL       *string `json:"daemon_url,omitempty"`
	Platform        *string `json:"platform,omitempty"`
	OutboundOnly    *bool   `json:"outbound_only,omitempty"`
	CallerID        *string `json:"caller_id,omitempty"`
	LogFormat       *string `json:"log_format,omitempty"`
	MetricsAddr     *string `json:"metrics_addr,omitempty"`
	IngressAddr     *string `json:"ingress_addr,omitempty"`
	GoogleProject   *string `json:"google_project,omitempty"`
	GoogleSub       *string `json:"google_subscription,omitempty"`
	GoogleLogEvents *bool   `json:"googlechat_log_events,omitempty"`

	// The HTTP interaction endpoint (#29). GoogleIngress picks the transport;
	// the other three configure the endpoint and the identity its callers must
	// prove. GoogleChatSA is an identity to check against, not a credential —
	// it is a public service-account address — so it belongs in the file the
	// same way a conversation allow-list does.
	GoogleIngress     *string `json:"googlechat_ingress,omitempty"`
	GoogleListen      *string `json:"googlechat_listen,omitempty"`
	GoogleEndpointURL *string `json:"googlechat_endpoint_url,omitempty"`
	GoogleChatSA      *string `json:"googlechat_service_account,omitempty"`

	// The two render modes. Process-wide rather than per-channel, and not for
	// want of wanting them scoped: they are read by the adapter when it is
	// built, and the adapter has no way to ask the router what a channel wants.
	// Putting them in a channel block would mean accepting a setting and
	// ignoring it, which is the failure DisallowUnknownFields exists to prevent.
	RichBlocks  *bool   `json:"slack_rich_blocks,omitempty"`
	GoogleCards *string `json:"googlechat_cards,omitempty"`

	// Structured settings, which are the ones a flag cannot hold properly.
	// IngressAllow is a list; GoogleCommands maps a Chat command ID to a
	// gateway verb.
	IngressAllow   []string          `json:"ingress_allow,omitempty"`
	GoogleCommands map[string]string `json:"googlechat_commands,omitempty"`

	// Token *variable names*, never token values — see checkNoSecrets. These
	// exist in the file for the same reason they exist as flags: naming the
	// variable is configuration, and what is in it is not.
	TokenEnv        *string `json:"token_env,omitempty"`
	AppTokenEnv     *string `json:"slack_app_token_env,omitempty"`
	BotTokenEnv     *string `json:"slack_bot_token_env,omitempty"`
	IngressTokenEnv *string `json:"ingress_token_env,omitempty"`

	// Defaults are the channel-scopable settings as they apply to a channel
	// with no entry of its own.
	Defaults ChannelConfig `json:"defaults,omitempty"`

	// Channels overrides Defaults per platform channel, keyed by the channel
	// ID an adapter reports in Message.Channel — a Slack "C0123ABC", a Chat
	// "spaces/AAAA". Not a channel name: resolving one costs an API call and a
	// scope switchboard does not otherwise need.
	Channels map[string]ChannelConfig `json:"channels,omitempty"`
}

// ChannelConfig is the set of settings that may differ between channels. A nil
// field defers: in Defaults to the flag, and in a Channels entry to Defaults.
type ChannelConfig struct {
	// Name is documentation and nothing else. The loader reads it and never
	// uses it. It exists because the keys of Channels are opaque IDs and JSON
	// has no comments, so without somewhere to write "#sre" a reviewer cannot
	// tell what a block governs — which is the sharpest edge of choosing JSON
	// over YAML, and cheap to blunt.
	Name string `json:"name,omitempty"`

	Approvals    *bool   `json:"approvals,omitempty"`
	ProgressMode *string `json:"progress_mode,omitempty"`
	ShowUsage    *bool   `json:"show_usage,omitempty"`

	// Approvers replaces the wider list rather than extending it: a channel
	// naming approvers is answering "who may approve here", and the reading
	// where it adds to the default has no way to say "fewer than the default"
	// at all. The corollary is that a channel can widen as well as narrow —
	// including back to ["channel"] — so a narrowed default is not a floor, and
	// the startup banner counts both.
	Approvers []string `json:"approvers,omitempty"`
}

// loadConfig reads and validates a config file.
//
// Unknown keys are refused rather than ignored. A misspelled "approver" that
// decodes to nothing is the same failure class as an --approvers list nobody
// can match: the run starts cleanly, announces settings that look right, and
// the control silently does nothing. There is no version of that worth being
// lenient for.
func loadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Parsed twice: once loosely, to establish that the file is one well-formed
	// JSON document and to hand the secret scan something it cannot fail open
	// on, and once strictly into Config. The order is the point. A scan that
	// takes bytes has to parse them itself and then decide what to do when it
	// cannot, and "could not read it, so no secrets found" is the wrong answer
	// on a security check — particularly here, where the file shape that
	// defeats the loose parse is one the strict decode accepts.
	dec := json.NewDecoder(bytes.NewReader(raw))
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	// One value and nothing after it. Trailing JSON is a file that was
	// concatenated or half-edited, and decoding only the first object would
	// silently honour half of somebody's intent.
	//
	// The probe is Token, not More. More is "is there another element", and it
	// answers no to a closing brace — so a duplicated }, which is the commonest
	// hand-edit mistake there is, reads as a clean end of file. Token asks for
	// the next thing in the stream and only io.EOF means there was not one.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: trailing data after the top-level object", path)
	}
	if err := checkNoSecrets(doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	strict := json.NewDecoder(bytes.NewReader(raw))
	strict.DisallowUnknownFields()
	var cfg Config
	if err := strict.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

// secretKeyHints name a value that must never be in the file. The rule is
// positional rather than a pattern match on the value: a key that sounds like a
// credential is refused whatever it holds, because the alternative is guessing
// at token shapes and being wrong about the one that mattered.
var secretKeyHints = []string{"token", "secret", "password", "credential", "apikey", "api_key"}

// secretValueHints catch the paste that got the key name right and the value
// wrong — a "slack_bot_token_env" set to the xoxb- itself rather than to the
// name of the variable holding it.
var secretValueHints = []string{"xoxb-", "xapp-", "xoxp-", "xoxa-", "-----BEGIN "}

// checkNoSecrets refuses a config file carrying a credential.
//
// AGENTS.md keeps tokens out of argv by indirection: a flag names the variable
// and the value lives in the environment. A config file is exactly the artifact
// somebody will paste an xoxb- into and then commit, and unlike argv it is
// designed to be checked in — so this is a startup error rather than a line in
// the docs. Keys ending in _env are the sanctioned form and are exempt from the
// name rule, though not from the value rule: naming a variable is configuration;
// holding the secret is not.
//
// doc is the file already parsed into the generic shape, so there is no input
// this can decline to read and then pass.
func checkNoSecrets(doc any) error {
	var walk func(prefix string, v any) error
	walk = func(prefix string, v any) error {
		switch t := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			// Sorted so a file with several offending keys always reports the
			// same one, rather than whichever the map happened to yield.
			sort.Strings(keys)
			for _, k := range keys {
				child := k
				if prefix != "" {
					child = prefix + "." + k
				}
				lower := strings.ToLower(k)
				if !strings.HasSuffix(lower, "_env") {
					for _, hint := range secretKeyHints {
						if strings.Contains(lower, hint) {
							return fmt.Errorf("%q looks like a credential, and credentials do not go in the config file: "+
								"name the environment variable holding it instead (a %q key)", child, k+"_env")
						}
					}
				}
				if err := walk(child, t[k]); err != nil {
					return err
				}
			}
		case []any:
			for i, e := range t {
				if err := walk(fmt.Sprintf("%s[%d]", prefix, i), e); err != nil {
					return err
				}
			}
		case string:
			for _, hint := range secretValueHints {
				if strings.Contains(t, hint) {
					return fmt.Errorf("%s holds what looks like a live credential (%q…): "+
						"put it in an environment variable and name the variable here", prefix, hint)
				}
			}
		}
		return nil
	}
	return walk("", doc)
}

// A resolver applies one precedence to every setting: an explicitly passed
// flag, else a non-empty environment variable, else the config file, else the
// flag's registered default.
//
// "Explicitly passed" is what makes this honest, and it is why the flags are no
// longer registered with environment-derived defaults. A flag whose default
// environment is indistinguishable after parsing from one left alone, so a file
// could never be layered underneath it — the environment would win by pretending
// to be the default. flag.FlagSet.Visit walks only the flags actually on argv
// (as against VisitAll, which walks every registered flag), which is the one
// place that distinction survives.
//
// Each setting resolves in place, over the variable the flag already parsed
// into, so the several hundred lines downstream that read *daemonURL keep
// reading it and there is no second copy of the config to keep in step.
type resolver struct {
	set map[string]bool

	// errs are the environment values that would not parse. Collected rather
	// than returned one at a time so that an operator with two bad variables
	// hears about both in one run, and so the resolution block reads as a list
	// of settings rather than a ladder of error checks.
	errs []error
}

func newResolver(fs *flag.FlagSet) *resolver {
	r := &resolver{set: make(map[string]bool)}
	fs.Visit(func(f *flag.Flag) { r.set[f.Name] = true })
	return r
}

// err reports every environment value that would not parse, or nil.
//
// Only str and boolean collect into errs, which is what makes one check after
// the block of them enough: list cannot fail, and mapping hands its error back
// at the call site. A resolver method that starts collecting errors after being
// called past that check would be silently ignored, so it should return them
// the way mapping does instead.
func (r *resolver) err() error { return errors.Join(r.errs...) }

// origin names where a setting's value came from, for an error message about
// it. A value the operator wrote as --approvers and one the operator wrote into
// a file need different advice, and "invalid --approvers" pointing at a flag
// nobody passed is the kind of hint that costs an hour.
//
// fileKey is the setting's path in the file, empty when the file did not set
// it. Spelled out by the caller rather than derived from name, because the file
// nests where the flags are flat: --approvers is defaults.approvers, and
// pointing at a top-level "approvers" that would itself be refused as an
// unknown key is no better than pointing at the flag nobody passed.
func (r *resolver) origin(name, envName, fileKey string) string {
	switch {
	case r.set[name]:
		return "--" + name
	case envName != "" && os.Getenv(envName) != "":
		return "$" + envName
	case fileKey != "":
		return "the config file's " + fileKey
	}
	return "--" + name
}

// passed reports whether a flag was given on the command line.
func (r *resolver) passed(name string) bool { return r.set[name] }

// str resolves a string setting. envName may be empty for a setting with no
// environment alternative.
func (r *resolver) str(name, envName string, fileVal *string, p *string) {
	if r.set[name] {
		return
	}
	if envName != "" {
		if v, ok := os.LookupEnv(envName); ok && v != "" {
			*p = v
			return
		}
	}
	if fileVal != nil {
		*p = *fileVal
	}
}

// boolean is str for a boolean. The environment spelling is deliberately strict
// — no silent fallback to false — because a bool that reads an unparseable
// value as its zero is a feature quietly off, and two of these gate a grant.
func (r *resolver) boolean(name, envName string, fileVal *bool, p *bool) {
	if r.set[name] {
		return
	}
	if envName != "" {
		if v, ok := os.LookupEnv(envName); ok && v != "" {
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "true", "1", "yes":
				*p = true
			case "false", "0", "no":
				*p = false
			default:
				r.errs = append(r.errs, fmt.Errorf("$%s = %q is not a boolean (want true or false)", envName, v))
			}
			return
		}
	}
	if fileVal != nil {
		*p = *fileVal
	}
}

// list resolves a setting the flag spells as one comma-separated string and the
// file spells as an array, which is the shape flags cannot hold. It returns
// rather than resolving in place because the two spellings are different types.
//
// An empty array is honoured as empty rather than falling through to the next
// layer, which is not the same as saying it means nothing is allowed: for
// ingress_allow an empty list admits every conversation, exactly as an unset
// --ingress-allow does, and serve warns about it at startup either way. What
// this rung guarantees is only that "ingress_allow": [] cannot silently pick up
// an --ingress-allow default underneath it.
func (r *resolver) list(name, envName, flagVal string, fileVal []string) []string {
	if r.set[name] {
		return splitList(flagVal)
	}
	if envName != "" {
		if v, ok := os.LookupEnv(envName); ok && v != "" {
			return splitList(v)
		}
	}
	if fileVal != nil {
		out := make([]string, 0, len(fileVal))
		for _, s := range fileVal {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return splitList(flagVal)
}

// channelsFrom resolves the file's channel blocks into the table the router
// reads, each one a complete answer for its channel rather than a delta: def is
// the process-wide posture, and every field a block does not mention is taken
// from it. Resolved here, at startup, so that a bad progress mode or an approver
// list nobody can match fails before the adapter dials a platform, instead of
// being discovered from the one thread that needed it.
func channelsFrom(cfg *Config, def channelSettings, platform string, mode chat.CallerMode) (map[string]channelSettings, error) {
	if len(cfg.Channels) == 0 {
		return nil, nil
	}
	out := make(map[string]channelSettings, len(cfg.Channels))
	// Sorted so a file with two bad blocks always reports the same one first.
	ids := make([]string, 0, len(cfg.Channels))
	for id := range cfg.Channels {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		c := cfg.Channels[id]
		if err := checkChannelID(id, platform); err != nil {
			return nil, err
		}
		s := def
		if c.Approvals != nil {
			s.approvals = *c.Approvals
		}
		if c.ShowUsage != nil {
			s.showUsage = *c.ShowUsage
		}
		if c.ProgressMode != nil {
			m, err := parseProgressMode(*c.ProgressMode)
			if err != nil {
				return nil, fmt.Errorf("channels[%q].progress_mode: %w", id, err)
			}
			s.progress = m
		}
		if c.Approvers != nil {
			// Replaces rather than extends — see ChannelConfig.Approvers. Which
			// means a channel can widen as well as narrow, including all the way
			// back to ["channel"], and nothing here treats the default as a floor.
			p, err := parseApproverList(c.Approvers, mode)
			if err != nil {
				return nil, fmt.Errorf("channels[%q].approvers: %w", id, err)
			}
			s.approvers = p
		}
		out[id] = s
	}
	return out, nil
}

// slackChannelID and chatSpaceName are the two shapes Message.Channel can
// actually hold: Slack's C/D/G-prefixed ID, and Google Chat's space resource
// name. Both are what the platform sends, not what a human types.
//
// Shape only, deliberately loose about length. The job is to catch the room
// name typed where the ID belongs, not to second-guess how a platform allocates
// IDs — a check that gets stricter than the platform starts rejecting valid
// config, which is a worse failure than the one it was added for.
var (
	slackChannelID = regexp.MustCompile(`^[CDG][A-Z0-9]+$`)
	chatSpaceName  = regexp.MustCompile(`^spaces/[A-Za-z0-9_-]+$`)
)

// checkChannelID refuses a channels key that cannot be a channel ID on the
// platform this gateway is bridging.
//
// The name a person uses for a room is not the name the platform sends.
// "channels": {"#sre": {"approvers": [...]}} is the obvious thing to write and
// matches nothing at all, because Message.Channel carries C0SRE0000 — and the
// failure is silent in the direction that matters: the block that was meant to
// narrow who may approve in #sre never applies, so #sre keeps the wider default
// and the run says nothing about it. Same for "@someone", and for a Slack ID in
// a Google Chat deployment. Startup is the only place this is visible.
func checkChannelID(id, platform string) error {
	switch {
	case platform == "slack" && slackChannelID.MatchString(id):
		return nil
	case platform == "googlechat" && chatSpaceName.MatchString(id):
		return nil
	case platform == "slack":
		return fmt.Errorf("channels[%q] is not a Slack channel ID (want the platform's ID, like %q, not the name you type)", id, "C0123ABCD")
	case platform == "googlechat":
		return fmt.Errorf("channels[%q] is not a Google Chat space name (want %q)", id, "spaces/AAAA1111")
	default:
		// An unknown platform is refused a few lines later with a better
		// message; do not pre-empt it by rejecting every key here.
		return nil
	}
}

// configureRouter installs the resolved settings on a freshly built router.
//
// One function rather than four calls at the one call site, so that "the router
// was given every layer" is a thing a test can assert. Forgetting setChannels
// is a run that parses a file full of per-channel rules and enforces none of
// them, which looks exactly like a run that works.
func configureRouter(r *Router, ac *approval.Client, def channelSettings, byChannel map[string]channelSettings) {
	r.setShowUsage(def.showUsage)
	r.setApprovals(ac, def.approvals)
	r.setApprovers(def.approvers)
	r.setChannels(byChannel)
}

// channelIDs lists the configured channels, sorted, for the startup line.
func channelIDs(byChannel map[string]channelSettings) []string {
	ids := make([]string, 0, len(byChannel))
	for id := range byChannel {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// anyApprovals reports whether any channel turns permission relaying on. The
// approval client is one connection to one daemon and is built or not built
// once, so a file that enables approvals in a single channel and nowhere else
// still needs it.
func anyApprovals(byChannel map[string]channelSettings) bool {
	return countApprovals(byChannel) > 0
}

// countApprovals counts the channels that relay permission prompts.
func countApprovals(byChannel map[string]channelSettings) int {
	n := 0
	for _, s := range byChannel {
		if s.approvals {
			n++
		}
	}
	return n
}

// approverSpread counts, among the channels that relay prompts, how many let
// anyone in the room answer and how many name their own approvers — the two
// numbers the startup banner reports, because a channel list replaces the
// process default rather than narrowing it and can therefore widen it.
func approverSpread(byChannel map[string]channelSettings) (open, named int) {
	for _, s := range byChannel {
		if !s.approvals {
			continue
		}
		if s.approvers.open() {
			open++
		} else {
			named++
		}
	}
	return open, named
}

// hasFlag reports whether any of these flag names was given on argv. Used for
// the config path itself, which is resolved before there is a resolver.
func hasFlag(fs *flag.FlagSet, names ...string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		for _, n := range names {
			if f.Name == n {
				found = true
			}
		}
	})
	return found
}

// mapping is list for the Chat app-command table, which the flag spells as
// "1=progress,2=help" and the file as an object. Parsing stays with the flag
// spelling: the file's keys are already separated, so only the flag and
// environment forms can be malformed.
func (r *resolver) mapping(name, envName, flagVal string, fileVal map[string]string) (map[int64]string, error) {
	if !r.set[name] {
		if v, ok := os.LookupEnv(envName); ok && v != "" {
			return parseAppCommands(v)
		}
		if fileVal != nil {
			pairs := make([]string, 0, len(fileVal))
			for k, v := range fileVal {
				pairs = append(pairs, k+"="+v)
			}
			// Sorted so a malformed entry is always reported as the same one.
			sort.Strings(pairs)
			return parseAppCommands(strings.Join(pairs, ","))
		}
	}
	return parseAppCommands(flagVal)
}
