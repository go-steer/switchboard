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

//go:build integration

// This file holds integration tests that run the router against a real
// core-agent daemon, exercising the actual shipped wire contract rather
// than an httptest stand-in. It is build-tagged out of the default suite
// because it needs the core-agent binary; run it with:
//
//	go build -o /tmp/core-agent ./cmd/core-agent   # in the core-agent repo
//	CORE_AGENT_BIN=/tmp/core-agent go test -tags=integration ./cmd/switchboard -run Integration -v

package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/switchboard/pkg/chat"
	"github.com/go-steer/switchboard/pkg/daemon"
)

// TestIntegrationRealDaemonRoundTrip stands up a real multi-session
// core-agent daemon (echo provider, bearer-table auth, switchboard as a
// proxy identity) and drives the router through the full inbound path:
// create → inject → wake → SSE relay, asserting the agent's turn comes
// back through the sender attributed to the asserted caller.
func TestIntegrationRealDaemonRoundTrip(t *testing.T) {
	bin := os.Getenv("CORE_AGENT_BIN")
	if bin == "" {
		t.Skip("set CORE_AGENT_BIN to a built core-agent binary to run this test")
	}

	const (
		botToken   = "tok_switchboard_integration"
		botID      = "sa:switchboard-bot"
		callerMail = "alice@example.com"
	)
	port := freePort(t)
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	work := t.TempDir()
	usersFile := filepath.Join(work, "users.json")
	writeFileMode(t, usersFile, 0o600, fmt.Sprintf(`{
  "version": 1,
  "users": [
    { "identity": %q, "token": %q },
    { "identity": %q, "token": "tok_alice_placeholder" }
  ]
}`, botID, botToken, callerMail))

	agentsDir := filepath.Join(work, ".agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFileMode(t, filepath.Join(agentsDir, "config.json"), 0o644, fmt.Sprintf(`{
  "version": 1,
  "model": { "name": "echo" },
  "permissions": { "mode": "yolo" },
  "attach": {
    "listen": "127.0.0.1:%d",
    "multi_session": {
      "enabled": true,
      "auth": { "kind": "bearer_table", "table_file": %q },
      "proxy_identities": [%q],
      "default_identity": "anon",
      "allow_anonymous": false
    }
  }
}`, port, usersFile, botID))

	// Start the daemon headless in the work dir so it loads .agents/config.json.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--provider=echo", "--no-repl",
		"--session-db", "--session-db-path="+filepath.Join(work, "session.db"))
	cmd.Dir = work
	logPath := filepath.Join(work, "daemon.log")
	logf, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer logf.Close()
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
		if t.Failed() {
			if b, rerr := os.ReadFile(logPath); rerr == nil {
				t.Logf("daemon log:\n%s", b)
			}
		}
	})

	waitForListener(t, fmt.Sprintf("127.0.0.1:%d", port), 10*time.Second)

	dc, err := daemon.New(daemon.Config{BaseURL: baseURL, BearerToken: botToken})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	fake := &fakeSender{replies: make(chan chat.Reply, 4)}
	router := NewRouter(dc, fake, t.Logf)

	msg := chat.Message{Conversation: "C0:100.1", Caller: callerMail, Text: "ping from integration"}
	if err := router.Handle(ctx, msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	select {
	case rep := <-fake.replies:
		if rep.Conversation != msg.Conversation {
			t.Errorf("reply conversation = %q, want %q", rep.Conversation, msg.Conversation)
		}
		if strings.TrimSpace(rep.Text) == "" {
			t.Errorf("relayed reply text is empty")
		}
		t.Logf("real daemon relayed reply: %q", rep.Text)
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for a relayed reply from the real daemon")
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func writeFileMode(t *testing.T, path string, mode os.FileMode, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil { // WriteFile mode is pre-umask
		t.Fatal(err)
	}
}

func waitForListener(t *testing.T, addr string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("daemon did not bind %s within %s", addr, within)
}
