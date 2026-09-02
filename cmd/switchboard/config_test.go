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
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/switchboard/pkg/chat"
)

// writeConfig drops a config file in the test's own directory and returns its
// path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "switchboard.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// ------------------------------------------------------------- the loader

func TestLoadConfigReadsWhatItIsGiven(t *testing.T) {
	path := writeConfig(t, `{
	  "daemon_url": "http://daemon:7777",
	  "ingress_allow": ["C1", "C2"],
	  "defaults": {"approvals": true, "progress_mode": "status"},
	  "channels": {"C9": {"name": "#sre", "approvers": ["ana@example.com"]}}
	}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.DaemonURL == nil || *cfg.DaemonURL != "http://daemon:7777" {
		t.Errorf("daemon_url = %v, want http://daemon:7777", cfg.DaemonURL)
	}
	if len(cfg.IngressAllow) != 2 {
		t.Errorf("ingress_allow = %v, want two entries", cfg.IngressAllow)
	}
	if cfg.Defaults.Approvals == nil || !*cfg.Defaults.Approvals {
		t.Errorf("defaults.approvals did not survive the decode")
	}
	if got := cfg.Channels["C9"].Name; got != "#sre" {
		t.Errorf("channels[C9].name = %q, want #sre", got)
	}
}

// A file that omits a setting must be distinguishable from one that sets it to
// its zero value: "approvals": false has to beat a flag default of true, while
// saying nothing has to lose to it. That is the whole reason every field is a
// pointer.
func TestAnOmittedSettingIsNotTheSameAsFalse(t *testing.T) {
	omitted, err := loadConfig(writeConfig(t, `{}`))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if omitted.Defaults.Approvals != nil {
		t.Errorf("an omitted approvals decoded to %v, want nil", *omitted.Defaults.Approvals)
	}
	set, err := loadConfig(writeConfig(t, `{"defaults": {"approvals": false}}`))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if set.Defaults.Approvals == nil {
		t.Fatal(`"approvals": false decoded to nil, and is now indistinguishable from unset`)
	}
	if *set.Defaults.Approvals {
		t.Errorf(`"approvals": false decoded to true`)
	}
}

// A key nobody reads is the same failure as an approver list nobody matches:
// the run starts, the setting looks present, and the control does nothing.
func TestAMisspelledKeyIsRefused(t *testing.T) {
	_, err := loadConfig(writeConfig(t, `{"defaults": {"approver": ["ana@example.com"]}}`))
	if err == nil {
		t.Fatal("loadConfig accepted a key it will never read")
	}
	if !strings.Contains(err.Error(), "approver") {
		t.Errorf("error = %q, want it to name the key", err)
	}
}

func TestTrailingDataIsRefused(t *testing.T) {
	_, err := loadConfig(writeConfig(t, `{"platform": "slack"}{"platform": "googlechat"}`))
	if err == nil {
		t.Fatal("loadConfig honoured the first half of a concatenated file")
	}
	if !strings.Contains(err.Error(), "trailing data") {
		t.Errorf("error = %q, want it to mention trailing data", err)
	}
}

func TestAMissingFileIsAStartupError(t *testing.T) {
	if _, err := loadConfig(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("loadConfig fell back to the defaults for a file that is not there")
	}
}

// ------------------------------------------------------------- credentials

// The config file is the artifact somebody pastes a token into and then
// commits, which is exactly what AGENTS.md keeps out of argv by indirection.
func TestCredentialsAreRefused(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{
			name: "a key that sounds like a credential",
			body: `{"slack_bot_token": "whatever"}`,
			want: "slack_bot_token",
		},
		{
			name: "a live token in an otherwise correct key",
			body: `{"slack_bot_token_env": "xoxb-1-2-abcdef"}`,
			want: "live credential",
		},
		{
			name: "a token buried in a channel block",
			body: `{"channels": {"C1": {"secret": "hunter2"}}}`,
			want: "channels.C1.secret",
		},
		{
			name: "a private key pasted anywhere at all",
			body: `{"daemon_url": "-----BEGIN PRIVATE KEY-----"}`,
			want: "live credential",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadConfig(writeConfig(t, tc.body))
			if err == nil {
				t.Fatal("loadConfig accepted a file carrying a credential")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// The sanctioned form. Naming the variable is configuration; holding what is in
// it is not, and refusing the name too would leave nowhere to say it.
func TestNamingTheVariableIsAllowed(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, `{"slack_bot_token_env": "TEAM_SLACK_BOT_TOKEN"}`))
	if err != nil {
		t.Fatalf("loadConfig refused a token *variable name*: %v", err)
	}
	if cfg.BotTokenEnv == nil || *cfg.BotTokenEnv != "TEAM_SLACK_BOT_TOKEN" {
		t.Errorf("slack_bot_token_env = %v", cfg.BotTokenEnv)
	}
}

// ------------------------------------------------------------- precedence

// newTestResolver parses args into a throwaway flag set and returns the
// resolver plus the two variables the tests below resolve into.
func newTestResolver(t *testing.T, args []string) (*resolver, *string, *bool) {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	s := fs.String("thing", "built-in", "")
	b := fs.Bool("switch", false, "")
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return newResolver(fs), s, b
}

// The whole ladder, one rung at a time: a flag beats the environment beats the
// file beats the built-in default.
func TestPrecedence(t *testing.T) {
	file := "from-file"
	for _, tc := range []struct {
		name string
		args []string
		env  string // "" means leave it unset
		file *string
		want string
	}{
		{name: "nothing set", want: "built-in"},
		{name: "the file alone", file: &file, want: "from-file"},
		{name: "the environment beats the file", env: "from-env", file: &file, want: "from-env"},
		{
			name: "an explicit flag beats both",
			args: []string{"--thing", "from-flag"},
			env:  "from-env", file: &file, want: "from-flag",
		},
		{
			// The case a flag registered with an envOr default could not express:
			// the operator passed the built-in value on purpose, and it must win
			// over the file rather than looking like a flag nobody touched.
			name: "a flag passed with the default value still beats the file",
			args: []string{"--thing", "built-in"},
			file: &file, want: "built-in",
		},
		{
			// An unset ConfigMap key renders as "", and reading that as an
			// intentional empty string would blank a setting somebody meant to
			// leave to the file.
			name: "an empty environment value is not a value",
			env:  "", file: &file, want: "from-file",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv("SWITCHBOARD_TEST_THING", tc.env)
			}
			res, got, _ := newTestResolver(t, tc.args)
			res.str("thing", "SWITCHBOARD_TEST_THING", tc.file, got)
			if err := res.err(); err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if *got != tc.want {
				t.Errorf("resolved %q, want %q", *got, tc.want)
			}
		})
	}
}

func TestBooleanPrecedence(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name string
		args []string
		env  string
		file *bool
		want bool
	}{
		{name: "nothing set", want: false},
		{name: "the file turns it on", file: &yes, want: true},
		{name: "the file turns it off against a flag nobody passed", file: &no, want: false},
		{name: "the environment beats the file", env: "true", file: &no, want: true},
		{name: "an explicit flag beats both", args: []string{"--switch"}, env: "false", file: &no, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv("SWITCHBOARD_TEST_SWITCH", tc.env)
			}
			res, _, got := newTestResolver(t, tc.args)
			res.boolean("switch", "SWITCHBOARD_TEST_SWITCH", tc.file, got)
			if err := res.err(); err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if *got != tc.want {
				t.Errorf("resolved %v, want %v", *got, tc.want)
			}
		})
	}
}

// A bool that reads an unparseable value as its zero is a feature quietly off,
// and two of these gate a grant.
func TestAnUnparseableBooleanIsRefused(t *testing.T) {
	t.Setenv("SWITCHBOARD_TEST_SWITCH", "sure")
	res, _, got := newTestResolver(t, nil)
	res.boolean("switch", "SWITCHBOARD_TEST_SWITCH", nil, got)
	err := res.err()
	if err == nil {
		t.Fatal("an unparseable boolean resolved silently")
	}
	if !strings.Contains(err.Error(), "SWITCHBOARD_TEST_SWITCH") {
		t.Errorf("error = %q, want it to name the variable", err)
	}
}

// Every bad environment value in one run, not the first one and then another
// restart to find the second.
func TestEveryBadEnvironmentValueIsReportedTogether(t *testing.T) {
	t.Setenv("SWITCHBOARD_TEST_A", "sure")
	t.Setenv("SWITCHBOARD_TEST_B", "nope")
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	a, b := fs.Bool("a", false, ""), fs.Bool("b", false, "")
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	res := newResolver(fs)
	res.boolean("a", "SWITCHBOARD_TEST_A", nil, a)
	res.boolean("b", "SWITCHBOARD_TEST_B", nil, b)
	err := res.err()
	if err == nil {
		t.Fatal("two unparseable booleans resolved silently")
	}
	for _, want := range []string{"SWITCHBOARD_TEST_A", "SWITCHBOARD_TEST_B"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

// The list a flag can only hold flattened, on all four rungs. The flag and the
// environment rungs need a resolver that has actually seen the flag passed,
// which is why this builds two.
func TestListPrecedence(t *testing.T) {
	unset := func(t *testing.T) *resolver {
		t.Helper()
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.String("allow", "", "")
		if err := fs.Parse(nil); err != nil {
			t.Fatalf("parse: %v", err)
		}
		return newResolver(fs)
	}
	passed := func(t *testing.T) *resolver {
		t.Helper()
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.String("allow", "", "")
		if err := fs.Parse([]string{"--allow=C8"}); err != nil {
			t.Fatalf("parse: %v", err)
		}
		return newResolver(fs)
	}

	t.Run("the flag outranks everything", func(t *testing.T) {
		t.Setenv("SBTEST_ALLOW", "C7")
		got := passed(t).list("allow", "SBTEST_ALLOW", "C8", []string{"C1"})
		if len(got) != 1 || got[0] != "C8" {
			t.Errorf("list = %v, want the flag's [C8]", got)
		}
	})
	t.Run("the environment outranks the file", func(t *testing.T) {
		t.Setenv("SBTEST_ALLOW", "C7, C6")
		got := unset(t).list("allow", "SBTEST_ALLOW", "", []string{"C1"})
		if len(got) != 2 || got[0] != "C7" || got[1] != "C6" {
			t.Errorf("list = %v, want the environment's [C7 C6]", got)
		}
	})
	t.Run("the file is read and trimmed", func(t *testing.T) {
		got := unset(t).list("allow", "SBTEST_ALLOW", "", []string{"C1", " C2 "})
		if len(got) != 2 || got[1] != "C2" {
			t.Errorf("list = %v, want [C1 C2] trimmed", got)
		}
	})
	// An empty array does not fall through to the layer underneath it. That is
	// the whole claim: for ingress_allow an empty list admits everything, just
	// as an unset flag does, so what is being checked here is that the flag's
	// value is not picked up, not that nothing is admitted.
	t.Run("an empty array does not fall through to the flag", func(t *testing.T) {
		got := unset(t).list("allow", "SBTEST_ALLOW", "C9", []string{})
		if len(got) != 0 {
			t.Errorf("an empty array resolved to %v, want the file's empty list", got)
		}
	})
	t.Run("an absent array falls through", func(t *testing.T) {
		got := unset(t).list("allow", "SBTEST_ALLOW", "C9", nil)
		if len(got) != 1 || got[0] != "C9" {
			t.Errorf("an absent array resolved to %v, want the flag default", got)
		}
	})
}

// The map a flag can only hold flattened, on the same four rungs.
func TestMappingPrecedence(t *testing.T) {
	newRes := func(t *testing.T, args []string) *resolver {
		t.Helper()
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.String("commands", "", "")
		if err := fs.Parse(args); err != nil {
			t.Fatalf("parse: %v", err)
		}
		return newResolver(fs)
	}
	file := map[string]string{"1": "deploy"}

	t.Run("the flag outranks everything", func(t *testing.T) {
		t.Setenv("SBTEST_COMMANDS", "3=env")
		got, err := newRes(t, []string{"--commands=2=flag"}).mapping("commands", "SBTEST_COMMANDS", "2=flag", file)
		if err != nil {
			t.Fatalf("mapping: %v", err)
		}
		if len(got) != 1 || got[2] != "flag" {
			t.Errorf("mapping = %v, want the flag's 2=flag", got)
		}
	})
	t.Run("the environment outranks the file", func(t *testing.T) {
		t.Setenv("SBTEST_COMMANDS", "3=env")
		got, err := newRes(t, nil).mapping("commands", "SBTEST_COMMANDS", "", file)
		if err != nil {
			t.Fatalf("mapping: %v", err)
		}
		if len(got) != 1 || got[3] != "env" {
			t.Errorf("mapping = %v, want the environment's 3=env", got)
		}
	})
	t.Run("the file is read", func(t *testing.T) {
		got, err := newRes(t, nil).mapping("commands", "SBTEST_COMMANDS", "", file)
		if err != nil {
			t.Fatalf("mapping: %v", err)
		}
		if len(got) != 1 || got[1] != "deploy" {
			t.Errorf("mapping = %v, want the file's 1=deploy", got)
		}
	})
	// A command ID that is not a number is a mapping that will never fire, and
	// the file is the spelling where it is easiest to write one by hand.
	t.Run("a file key that is not a command ID is refused", func(t *testing.T) {
		_, err := newRes(t, nil).mapping("commands", "SBTEST_COMMANDS", "", map[string]string{"deploy": "deploy"})
		if err == nil {
			t.Fatal("mapping accepted a command ID that is not a number")
		}
	})
}

// ------------------------------------------------------- per-channel blocks

func TestChannelsInheritWhatTheyDoNotSay(t *testing.T) {
	def := channelSettings{
		progress:  ProgressStatus,
		approvals: true,
		showUsage: true,
		approvers: mustApprovers(t, "ana@example.com"),
	}
	cfg := &Config{Channels: map[string]ChannelConfig{
		"C1": {Name: "#scratch"},
	}}
	got, err := channelsFrom(cfg, def, "slack", chat.CallerEmail)
	if err != nil {
		t.Fatalf("channelsFrom: %v", err)
	}
	s := got["C1"]
	if s.progress != ProgressStatus || !s.approvals || !s.showUsage {
		t.Errorf("a block that says nothing changed something: %+v", s)
	}
	if !s.approvers.allows("ana@example.com") || s.approvers.allows("ben@example.com") {
		t.Errorf("a block that says nothing did not inherit the approver list")
	}
}

// The decision the user settled: a channel's approvers replace the wider list
// rather than adding to it.
func TestAChannelApproverListReplacesTheDefault(t *testing.T) {
	def := channelSettings{approvals: true, approvers: mustApprovers(t, "ana@example.com")}
	cfg := &Config{Channels: map[string]ChannelConfig{
		"C1": {Approvers: []string{"ben@example.com"}},
	}}
	got, err := channelsFrom(cfg, def, "slack", chat.CallerEmail)
	if err != nil {
		t.Fatalf("channelsFrom: %v", err)
	}
	s := got["C1"]
	if !s.approvers.allows("ben@example.com") {
		t.Error("the channel's own approver was refused")
	}
	if s.approvers.allows("ana@example.com") {
		t.Error("the default approver survived a list that replaces it")
	}
}

// The corollary of replacing, and the reason the startup banner counts it: a
// channel can widen as well as narrow, so a narrowed default is not a floor.
func TestAChannelCanWidenBackToTheWholeRoom(t *testing.T) {
	def := channelSettings{approvals: true, approvers: mustApprovers(t, "ana@example.com")}
	cfg := &Config{Channels: map[string]ChannelConfig{
		"C1": {Approvers: []string{approversChannel}},
	}}
	got, err := channelsFrom(cfg, def, "slack", chat.CallerEmail)
	if err != nil {
		t.Fatalf("channelsFrom: %v", err)
	}
	if !got["C1"].approvers.open() {
		t.Error(`a channel listing "channel" did not widen back to the whole room`)
	}
	if open, named := approverSpread(got); open != 1 || named != 0 {
		t.Errorf("banner counted open=%d named=%d, want 1 and 0", open, named)
	}
}

// Startup is the only place a per-channel mistake is visible: the alternative
// is discovering it from the one thread that needed the setting.
func TestABadChannelBlockFailsAtStartup(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		block      ChannelConfig
	}{
		{
			name:  "a progress mode nobody renders",
			block: ChannelConfig{ProgressMode: strPtr("verbose")},
			want:  "progress_mode",
		},
		{
			name:  "an approver list nobody can match",
			block: ChannelConfig{Approvers: []string{"ana@example.com ben@example.com"}},
			want:  "approvers",
		},
		{
			name:  "an approver list that names nobody",
			block: ChannelConfig{Approvers: []string{}},
			want:  "names nobody",
		},
		{
			name:  "emails against a run that asserts platform IDs",
			block: ChannelConfig{Approvers: []string{"ana@example.com"}},
			want:  "platform IDs",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mode := chat.CallerEmail
			if strings.Contains(tc.want, "platform IDs") {
				mode = chat.CallerID
			}
			cfg := &Config{Channels: map[string]ChannelConfig{"C1": tc.block}}
			_, err := channelsFrom(cfg, channelSettings{}, "slack", mode)
			if err == nil {
				t.Fatal("channelsFrom accepted a block it cannot honour")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "C1") {
				t.Errorf("error = %q, want it to name the channel", err)
			}
		})
	}
}

func strPtr(s string) *string { return &s }

// ----------------------------------------------------------- the resolver

func TestSettingsForResolvesThreeLayers(t *testing.T) {
	r := NewRouter(nil, nil, ProgressIndicator, nil, nil)
	r.setShowUsage(true)
	r.setChannels(map[string]channelSettings{
		"C1": {progress: ProgressOff, approvals: true, approvers: mustApprovers(t, "ana@example.com")},
	})

	// A channel nobody configured gets the defaults.
	if got := r.settingsFor("C2"); got.progress != ProgressIndicator || !got.showUsage {
		t.Errorf("an unconfigured channel = %+v, want the defaults", got)
	}
	// A conversation with no channel at all — an ingress post — likewise.
	if got := r.settingsFor(""); got.progress != ProgressIndicator {
		t.Errorf("an empty channel = %+v, want the defaults", got)
	}
	// A configured channel gets its own block, whole.
	c1 := r.settingsFor("C1")
	if c1.progress != ProgressOff || !c1.approvals || c1.showUsage {
		t.Errorf("C1 = %+v, want its own block rather than a merge at read time", c1)
	}
	// And a runtime override is narrower still — but only over the progress
	// mode, which is the only thing a chat command can change.
	r.setProgress("C1", ProgressStream)
	after := r.settingsFor("C1")
	if after.progress != ProgressStream {
		t.Errorf("progress after the command = %q, want stream", after.progress)
	}
	if after.approvers.allows("ben@example.com") {
		t.Error("a progress command reached the approver list")
	}
}

// ------------------------------------------------------------ end to end

// The file has to be consulted at all. A config that parses and is never read
// looks identical to one that works, right up until somebody it names is
// refused.
func TestRunServeReadsTheConfigFile(t *testing.T) {
	t.Setenv("SWITCHBOARD_DAEMON_TOKEN", "daemon-token")
	// Setenv before Unsetenv, for the cleanup: a bare Unsetenv would clear the
	// variable for every test that runs after this one, which is how a suite
	// starts depending on its own order.
	t.Setenv(envApprovers, "")
	os.Unsetenv(envApprovers)
	path := writeConfig(t, `{"defaults": {"approvals": true, "approvers": ["ana@example.com ben@example.com"]}}`)
	err := runServe([]string{"--config", path, "--outbound-only", "--ingress-addr", "127.0.0.1:0"})
	if err == nil {
		t.Fatal("runServe accepted an approver list it cannot honour")
	}
	if !strings.Contains(err.Error(), "config file") {
		t.Errorf("error = %q, want it to say the value came from the file", err)
	}
}

// -c and --config are one setting. core-agent shipped the short form alone and
// a Deployment written as args: ["--config=..."] exited at flag-parse during a
// live demo (go-steer/core-agent#209).
func TestBothSpellingsOfTheConfigFlag(t *testing.T) {
	for _, spelling := range []string{"--config", "-c"} {
		t.Run(spelling, func(t *testing.T) {
			t.Setenv("SWITCHBOARD_DAEMON_TOKEN", "daemon-token")
			err := runServe([]string{spelling, filepath.Join(t.TempDir(), "absent.json")})
			if err == nil {
				t.Fatalf("%s named a file that is not there and the run started anyway", spelling)
			}
			if !strings.Contains(err.Error(), "absent.json") {
				t.Errorf("error = %q, want it to name the file", err)
			}
		})
	}
}

// A path in the environment, for a Deployment that sets it there. Same file,
// same refusal to invent one.
func TestTheConfigPathMayComeFromTheEnvironment(t *testing.T) {
	t.Setenv("SWITCHBOARD_DAEMON_TOKEN", "daemon-token")
	t.Setenv("SWITCHBOARD_CONFIG", filepath.Join(t.TempDir(), "absent.json"))
	err := runServe(nil)
	if err == nil {
		t.Fatal("$SWITCHBOARD_CONFIG named a file that is not there and the run started anyway")
	}
	if !strings.Contains(err.Error(), "absent.json") {
		t.Errorf("error = %q, want it to name the file", err)
	}
}

// The name a person types for a room is not the name the platform sends, and a
// block keyed by the wrong one narrows nothing while looking like it does.
func TestAChannelKeyMustLookLikeAChannelID(t *testing.T) {
	for _, tc := range []struct {
		name, platform, id string
		ok                 bool
	}{
		{name: "a Slack ID", platform: "slack", id: "C0123ABCD", ok: true},
		{name: "a DM", platform: "slack", id: "D0123ABCD", ok: true},
		{name: "the name you type", platform: "slack", id: "#sre"},
		{name: "a person", platform: "slack", id: "@ana"},
		{name: "a space name on Slack", platform: "slack", id: "spaces/AAAA1111"},
		{name: "a space name", platform: "googlechat", id: "spaces/AAAA1111", ok: true},
		{name: "a bare space id", platform: "googlechat", id: "AAAA1111"},
		{name: "a Slack ID on Google Chat", platform: "googlechat", id: "C0123ABCD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Channels: map[string]ChannelConfig{tc.id: {}}}
			_, err := channelsFrom(cfg, channelSettings{}, tc.platform, chat.CallerEmail)
			if tc.ok && err != nil {
				t.Fatalf("channelsFrom refused a real channel ID: %v", err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatal("channelsFrom accepted a key the platform will never send")
				}
				if !strings.Contains(err.Error(), tc.id) {
					t.Errorf("error = %q, want it to name the key", err)
				}
			}
		})
	}
}

// The habit imported from the flag, where a comma is the separator. In an array
// it is inside one element, and an approver called "ana@x.com,ben@x.com" is
// nobody — the run would start, banner one approver, and refuse both of them.
func TestACommaInsideAFileApproverIsRefused(t *testing.T) {
	cfg := &Config{Channels: map[string]ChannelConfig{
		"C1": {Approvers: []string{"ana@example.com,ben@example.com"}},
	}}
	_, err := channelsFrom(cfg, channelSettings{}, "slack", chat.CallerEmail)
	if err == nil {
		t.Fatal("channelsFrom accepted two identities in one array element")
	}
	if !strings.Contains(err.Error(), "separate approvers with commas") {
		t.Errorf("error = %q, want the advice about commas", err)
	}
}

// The wiring, not the resolution. A run that reads a file full of per-channel
// rules and forgets to hand them to the router looks exactly like a run that
// works, so the handover is one function and this asserts every layer of it.
func TestConfigureRouterInstallsEveryLayer(t *testing.T) {
	r := NewRouter(nil, nil, ProgressIndicator, nil, nil)
	def := channelSettings{
		progress:  ProgressIndicator,
		approvals: true,
		showUsage: true,
		approvers: mustApprovers(t, "ana@example.com"),
	}
	byChannel := map[string]channelSettings{
		"C1": {progress: ProgressOff, approvals: false, approvers: mustApprovers(t, "ben@example.com")},
	}
	configureRouter(r, nil, def, byChannel)

	d := r.settingsFor("C2")
	if !d.approvals || !d.showUsage || !d.approvers.allows("ana@example.com") {
		t.Errorf("defaults = %+v, want the process posture", d)
	}
	c1 := r.settingsFor("C1")
	if c1.approvals || c1.progress != ProgressOff || !c1.approvers.allows("ben@example.com") {
		t.Errorf("C1 = %+v, want the file's block for it", c1)
	}
}

// The precedence exception, stated because it is the one rule a reader would
// otherwise get wrong: a channel block outranks even an explicitly passed flag,
// because the flag says "everywhere" and the block says "here".
func TestAChannelBlockOutranksAnExplicitFlag(t *testing.T) {
	def := channelSettings{approvals: true, approvers: mustApprovers(t, "ana@example.com")}
	cfg := &Config{Channels: map[string]ChannelConfig{
		"C1": {Approvers: []string{"ben@example.com"}},
	}}
	got, err := channelsFrom(cfg, def, "slack", chat.CallerEmail)
	if err != nil {
		t.Fatalf("channelsFrom: %v", err)
	}
	if got["C1"].approvers.allows("ana@example.com") {
		t.Error("the flag's list survived in a channel that named its own")
	}
}

// An empty $SWITCHBOARD_APPROVERS aborts the run, because an unset ConfigMap key
// renders as "" and falling through would widen the grant. Not when the flag was
// passed, though: the flag outranks the variable everywhere else, and refusing
// to start over a value that is about to be ignored saves nobody from anything.
func TestAnExplicitApproversFlagSurvivesAnEmptyEnvironment(t *testing.T) {
	t.Setenv("SWITCHBOARD_DAEMON_TOKEN", "daemon-token")
	t.Setenv(envApprovers, "")
	err := runServe([]string{
		"--approvals", "--approvers", "ana@example.com ben@example.com",
		"--outbound-only", "--ingress-addr", "127.0.0.1:0",
	})
	if err == nil {
		t.Fatal("runServe accepted an approver list it cannot honour")
	}
	// Reached the flag's own validation, which is the proof the empty variable
	// did not abort first.
	if !strings.Contains(err.Error(), "--approvers") {
		t.Errorf("error = %q, want the flag to have been the value that was used", err)
	}
}
