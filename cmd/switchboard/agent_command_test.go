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
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-steer/switchboard/pkg/chat"
	"github.com/go-steer/switchboard/pkg/daemon"
)

// slackThreadingSender behaves like Slack: a post to a bare channel lands as a new
// top-level message, whose thread is keyed by the message.
type slackThreadingSender struct{ *fakeSender }

func (s slackThreadingSender) Send(ctx context.Context, r chat.Reply) (chat.MessageRef, error) {
	ref, err := s.fakeSender.Send(ctx, r)
	if err == nil && !strings.Contains(r.Conversation, ":") {
		ref.Conversation = r.Conversation + ":" + ref.ID
	}
	return ref, err
}

func agentCmd(conv, channel, text string) chat.Command {
	return chat.Command{Name: "agent", Channel: channel, Caller: "u@x.com", Conversation: conv,
		Args: strings.Fields(text), Text: text}
}

func TestBareAgentListsTheChannelsAgents(t *testing.T) {
	r, _, _, _ := twoAgentRouter(t)
	ack, err := r.HandleCommand(context.Background(), agentCmd("", "C2", ""))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ack, "**Agents here**\n") || !strings.Contains(ack, "• **a** (`a`)\n") || !strings.Contains(ack, "• **b** (`b`), the default") {
		t.Errorf("listing = %q, want both agents with C2's default marked", ack)
	}
	r.setChannels(map[string]channelSettings{"C3": {defaultAgent: "b", agents: []string{"b"}}})
	if ack, _ := r.HandleCommand(context.Background(), agentCmd("", "C3", "")); strings.Contains(ack, "(`a`)") {
		t.Errorf("listing = %q offers an agent the channel does not allow", ack)
	}
}

// Google Chat: a command arrives in a thread of its own, but Chat will not take
// an app's reply there (measured), so the agent's thread is a starter's, as on
// Slack — on the chosen agent, not the channel's default — and the command
// gets no separate acknowledgment.
func TestAgentOnChatStartsFromAStarterNotTheCommandsThread(t *testing.T) {
	_, a, b, _ := twoAgentRouter(t)
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	r := NewRouter(nil, slackThreadingSender{fake}, ProgressOff, nil, nil)
	set, _ := newAgentSet("a", &agent{name: "a", daemon: a.client}, &agent{name: "b", display: "Infra agent", daemon: b.client})
	r.setAgents(set)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ack, err := r.HandleCommand(ctx, agentCmd("C1:77", "C1", "b how many pods?"))
	if err != nil {
		t.Fatal(err)
	}
	if ack != "" {
		t.Errorf("ack = %q, want none: the starter is the acknowledgment", ack)
	}
	starter := recvReply(t, fake.replies)
	if starter.Conversation != "C1" || !strings.HasPrefix(starter.Text, "**Infra agent**\nhow many pods?") {
		t.Errorf("starter = %+v, want the agent and the prompt posted in the channel", starter)
	}
	waitCount(t, &b.injects, 1, "agent b injects")
	if a.creates.Load() != 0 {
		t.Error("the channel's default agent got a session")
	}
	if _, has := r.conversationAgent("C1:77"); has {
		t.Error("the command's own thread was given the agent; Chat will not take replies there")
	}
	if got, _ := r.conversationAgent("C1:ts1"); got != "b" {
		t.Errorf("starter thread agent = %q, want b", got)
	}
	// The starter thread's next plain message stays with b.
	if err := r.Handle(ctx, chat.Message{Conversation: "C1:ts1", Channel: "C1", Caller: "u@x.com", Text: "and nodes?"}); err != nil {
		t.Fatal(err)
	}
	waitCount(t, &b.injects, 2, "agent b injects")
}

// Slack: a slash command has no thread, so a starter message opens one.
func TestAgentOpensAThreadWithAStarterWhenTheCommandHasNone(t *testing.T) {
	_, a, b, _ := twoAgentRouter(t)
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	r := NewRouter(nil, slackThreadingSender{fake}, ProgressOff, nil, nil)
	set, _ := newAgentSet("a", &agent{name: "a", daemon: a.client}, &agent{name: "b", display: "Infra agent", daemon: b.client})
	r.setAgents(set)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := agentCmd("", "C1", "b list\n  the nodes")
	cmd.CallerMention = "<@U1>"
	ack, err := r.HandleCommand(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	starter := recvReply(t, fake.replies)
	if starter.Conversation != "C1" || !strings.Contains(starter.Text, "**Infra agent** · asked by <@U1>") || !strings.Contains(starter.Text, "list\n  the nodes") {
		t.Errorf("starter = %+v, want the caller's mention, the agent and the prompt as typed, in the channel", starter)
	}
	if strings.Contains(starter.Text, "u@x.com") {
		t.Errorf("starter %q shows the caller's email in the channel", starter.Text)
	}
	if !strings.Contains(ack, "Starting a thread with Infra agent") {
		t.Errorf("ack = %q", ack)
	}
	waitCount(t, &b.injects, 1, "agent b injects")
	if got, _ := r.conversationAgent("C1:ts1"); got != "b" {
		t.Errorf("starter thread agent = %q, want b", got)
	}
}

// Inside a thread that already has an agent, the command does not switch it.
func TestAgentInsideAThreadDoesNotSwitchIt(t *testing.T) {
	r, a, b, _ := twoAgentRouter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Handle(ctx, chat.Message{Conversation: "C1:5", Channel: "C1", Caller: "u@x.com", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	waitCount(t, &a.injects, 1, "agent a injects")

	ack, _ := r.HandleCommand(ctx, agentCmd("C1:5", "C1", "b take over"))
	if !strings.Contains(ack, "This thread talks to a") {
		t.Errorf("ack = %q, want the thread's agent named", ack)
	}
	if b.creates.Load()+b.injects.Load() != 0 {
		t.Error("the thread was switched to agent b")
	}
	// Naming the thread's own agent with a prompt is just its next turn.
	if ack, _ := r.HandleCommand(ctx, agentCmd("C1:5", "C1", "a next question")); ack != "" {
		t.Errorf("ack = %q, want none", ack)
	}
	waitCount(t, &a.injects, 2, "agent a injects")
}

func TestAgentRefusesAnAgentTheChannelDoesNotOffer(t *testing.T) {
	r, _, b, _ := twoAgentRouter(t)
	r.setChannels(map[string]channelSettings{"C3": {defaultAgent: "a", agents: []string{"a"}}})
	ack, _ := r.HandleCommand(context.Background(), agentCmd("C3:1", "C3", "b hello"))
	if !strings.Contains(ack, `no agent "b" here`) {
		t.Errorf("ack = %q, want the refusal and the listing", ack)
	}
	if ack, _ := r.HandleCommand(context.Background(), agentCmd("C3:2", "C3", "nope hello")); !strings.Contains(ack, `no agent "nope" here`) {
		t.Errorf("ack = %q for an unknown agent", ack)
	}
	if b.creates.Load() != 0 {
		t.Error("a refused agent got a session")
	}
}

func TestAgentOnASingleAgentGateway(t *testing.T) {
	d := newRecordingDaemon(t, "s1")
	r := NewRouter(d.client, &fakeSender{replies: make(chan chat.Reply, 4)}, ProgressOff, nil, nil)
	if ack, _ := r.HandleCommand(context.Background(), agentCmd("C1:1", "C1", "default hi")); !strings.Contains(ack, "one agent") {
		t.Errorf("ack = %q", ack)
	}
	if d.creates.Load() != 0 {
		t.Error("a session was created")
	}
}

// stubDaemon answers session creation with create; everything else is a quiet
// open stream or an accepted inject.
func stubDaemon(t *testing.T, create http.HandlerFunc) *daemon.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions", create)
	mux.HandleFunc("POST /sessions/{app}/{sid}/inject", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"injected":"ok"}`)
	})
	mux.HandleFunc("GET /sessions/{app}/{sid}/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, err := daemon.New(daemon.Config{BaseURL: srv.URL, BearerToken: "tok", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The command answers at once, however slow the daemon: a Slack slash command
// has three seconds, and the adapter runs it on the loop every other event
// waits behind (caught in review).
func TestAgentAnswersBeforeTheThreadIsOpen(t *testing.T) {
	release := make(chan struct{})
	slow := stubDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"app":"core-agent","sessionID":"slow"}`)
	})
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	r := NewRouter(nil, slackThreadingSender{fake}, ProgressOff, nil, nil)
	set, _ := newAgentSet("a", &agent{name: "a", daemon: slow}, &agent{name: "b", daemon: slow})
	r.setAgents(set)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer close(release)

	done := make(chan string, 1)
	go func() {
		ack, _ := r.HandleCommand(ctx, agentCmd("", "C1", "b hello"))
		done <- ack
	}()
	select {
	case ack := <-done:
		if !strings.Contains(ack, "Starting a thread") {
			t.Errorf("ack = %q", ack)
		}
	case <-time.After(time.Second):
		t.Fatal("HandleCommand waited on the daemon; a Slack slash command would time out")
	}
}

// A starter whose session cannot be created is taken down, and the channel is
// told (caught in review).
func TestAFailedStartTakesTheStarterDown(t *testing.T) {
	broken := stubDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	r := NewRouter(nil, slackThreadingSender{fake}, ProgressOff, nil, nil)
	set, _ := newAgentSet("a", &agent{name: "a", daemon: broken}, &agent{name: "b", display: "Infra agent", daemon: broken})
	r.setAgents(set)

	if err := r.startAgentThread(context.Background(), agentCmd("", "C1", "b hello"), set.byName["b"], "hello"); err == nil {
		t.Fatal("startAgentThread succeeded against a daemon that refuses sessions")
	}
	if starter := recvReply(t, fake.replies); starter.Conversation != "C1" {
		t.Errorf("starter = %+v, want it in the channel", starter)
	}
	notice := recvReply(t, fake.replies)
	if notice.Conversation != "C1" || !strings.Contains(notice.Text, "Could not start a conversation with Infra agent") {
		t.Errorf("notice = %+v, want it in the channel", notice)
	}
	waitFor(t, func() bool { return containsRefID(fake.deletedRefs(), "ts1") }, "the starter was not taken down")
}

// A thread already on another agent when the session is opened — a message
// that got there first — is said so, not answered by the wrong agent under the
// chosen one's name (caught in review).
func TestStartingOnAThreadAlreadyOnAnotherAgentSaysSo(t *testing.T) {
	r, a, b, fake := twoAgentRouter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Handle(ctx, chat.Message{Conversation: "C1:9", Channel: "C1", Caller: "u@x.com", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	waitCount(t, &a.injects, 1, "agent a injects")

	// The starter lands in a conversation something else opened first.
	r.out = landsIn{fake, "C1:9"}
	if err := r.startAgentThread(ctx, agentCmd("", "C1", "b go"), r.agents.byName["b"], "go"); err != nil {
		t.Fatal(err)
	}
	recvReply(t, fake.replies) // the starter
	got := recvReply(t, fake.replies)
	if !strings.Contains(got.Text, "This thread talks to a") {
		t.Errorf("notice = %q", got.Text)
	}
	if b.creates.Load()+b.injects.Load() != 0 || a.injects.Load() != 1 {
		t.Errorf("the prompt was run anyway (a injects %d, b %d)", a.injects.Load(), b.injects.Load())
	}
}

// landsIn sends everything through fake but reports it landed in conv.
type landsIn struct {
	*fakeSender
	conv string
}

func (l landsIn) Send(ctx context.Context, r chat.Reply) (chat.MessageRef, error) {
	ref, err := l.fakeSender.Send(ctx, r)
	ref.Conversation = l.conv
	return ref, err
}

// The router is the picker's directory: the channel's allowed agents, its
// default marked, descriptions passed through; none on a one-agent gateway.
func TestAgentChoicesForAPicker(t *testing.T) {
	r, _, _, _ := twoAgentRouter(t)
	r.agents.byName["b"].desc = "Infra questions"
	r.setChannels(map[string]channelSettings{"C3": {defaultAgent: "b", agents: []string{"b"}}})
	got := r.AgentChoices("C3")
	if len(got) != 1 || got[0].Name != "b" || !got[0].Default || got[0].Description != "Infra questions" {
		t.Errorf("C3 choices = %+v, want only b, the default, described", got)
	}
	if all := r.AgentChoices("C1"); len(all) != 2 || !all[0].Default || all[1].Default {
		t.Errorf("C1 choices = %+v, want a (default) and b", all)
	}
	single := NewRouter(newRecordingDaemon(t, "s").client, &fakeSender{replies: make(chan chat.Reply, 1)}, ProgressOff, nil, nil)
	if c := single.AgentChoices("C1"); c != nil {
		t.Errorf("one-agent gateway offers %+v, want no picker", c)
	}
}

// Naming a thread's own agent with a prompt is the thread's next turn, run in
// the background: the command answers within a deadline (a Slack ack, a Chat
// dialog response) however slow the daemon's inject (caught in review).
func TestTheSameAgentsNextTurnDoesNotHoldTheCommand(t *testing.T) {
	release := make(chan struct{})
	var injects atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"app":"core-agent","sessionID":"s1"}`)
	})
	mux.HandleFunc("POST /sessions/{app}/{sid}/inject", func(w http.ResponseWriter, r *http.Request) {
		if injects.Add(1) > 1 { // the first turn is quick; the command's turn hangs
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
		fmt.Fprint(w, `{"injected":"ok"}`)
	})
	mux.HandleFunc("GET /sessions/{app}/{sid}/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	dc, err := daemon.New(daemon.Config{BaseURL: srv.URL, BearerToken: "tok", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	r := NewRouter(nil, fake, ProgressOff, nil, nil)
	set, _ := newAgentSet("a", &agent{name: "a", daemon: dc}, &agent{name: "b", daemon: dc})
	r.setAgents(set)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer close(release)
	if err := r.Handle(ctx, chat.Message{Conversation: "C1:5", Channel: "C1", Caller: "u@x.com", Text: "hi"}); err != nil {
		t.Fatal(err)
	}

	done := make(chan string, 1)
	go func() {
		ack, _ := r.HandleCommand(ctx, agentCmd("C1:5", "C1", "a next question"))
		done <- ack
	}()
	select {
	case ack := <-done:
		if ack != "" {
			t.Errorf("ack = %q, want none", ack)
		}
	case <-time.After(time.Second):
		t.Fatal("the command waited on the daemon's inject")
	}
}
