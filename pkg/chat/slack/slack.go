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

// Package slack implements chat.Adapter over Slack Socket Mode — an
// outbound WebSocket, so switchboard needs no public webhook. The MVP
// engages on app-mentions (@switchboard): a mention roots a conversation
// on its thread, and further mentions in that same thread continue it
// (same conversation key => same core-agent session). Plain thread
// replies without a mention are a later phase. All platform specifics
// stay behind chat.Adapter so the router never imports this package.
package slack

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/go-steer/switchboard/pkg/chat"
)

// CallerMode selects how a Slack user maps onto the daemon's
// X-Asserted-Caller identity.
type CallerMode string

const (
	// CallerEmail resolves the user's email via users.info (needs the
	// users:read.email scope) and asserts that. Default; matches the
	// daemon's usual per-caller credential keying.
	CallerEmail CallerMode = "email"
	// CallerID asserts the raw Slack user ID (e.g. U0123ABC) with no
	// extra API call or scope.
	CallerID CallerMode = "id"
)

// Config constructs an Adapter. Tokens are loaded from env by the caller
// (never bare flags).
type Config struct {
	// AppToken is the Socket Mode app-level token (xapp-...).
	AppToken string
	// BotToken is the bot user OAuth token (xoxb-...).
	BotToken string
	// CallerMode selects caller identity resolution; empty means
	// CallerEmail.
	CallerMode CallerMode
	// Logf is an optional structured-ish logger; nil discards.
	Logf func(format string, args ...any)
}

// Adapter is the Slack implementation of chat.Adapter.
type Adapter struct {
	api  *slack.Client
	sm   *socketmode.Client
	mode CallerMode
	logf func(string, ...any)

	// botUserID is this bot's own user ID, resolved at Run start and
	// used to ignore our own posts (loop guard).
	botUserID string

	mu         sync.Mutex
	callerByID map[string]string // Slack user ID -> asserted caller
}

// New validates the config and builds an Adapter. It does not open the
// socket; Run does.
func New(cfg Config) (*Adapter, error) {
	if cfg.AppToken == "" {
		return nil, errors.New("slack: AppToken (xapp-) is required")
	}
	if cfg.BotToken == "" {
		return nil, errors.New("slack: BotToken (xoxb-) is required")
	}
	mode := cfg.CallerMode
	if mode == "" {
		mode = CallerEmail
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	api := slack.New(cfg.BotToken, slack.OptionAppLevelToken(cfg.AppToken))
	sm := socketmode.New(api)
	return &Adapter{
		api:        api,
		sm:         sm,
		mode:       mode,
		logf:       logf,
		callerByID: make(map[string]string),
	}, nil
}

// Name identifies the platform.
func (a *Adapter) Name() string { return "slack" }

// Run opens the Socket Mode connection and dispatches each app-mention to
// h until ctx is cancelled or the socket fails unrecoverably.
func (a *Adapter) Run(ctx context.Context, h chat.Handler) error {
	if auth, err := a.api.AuthTestContext(ctx); err != nil {
		return fmt.Errorf("slack: auth test: %w", err)
	} else {
		a.botUserID = auth.UserID
		a.logf("slack: connected as %s (%s)", auth.User, auth.UserID)
	}

	// socketmode.Client.RunContext blocks reading the socket; run it in a
	// goroutine and consume its Events channel here so ctx cancellation
	// unblocks us even if the socket is quiet.
	errc := make(chan error, 1)
	go func() { errc <- a.sm.RunContext(ctx) }()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errc:
			return err
		case evt := <-a.sm.Events:
			a.dispatch(ctx, h, evt)
		}
	}
}

// dispatch acks and routes a single Socket Mode event.
func (a *Adapter) dispatch(ctx context.Context, h chat.Handler, evt socketmode.Event) {
	switch evt.Type {
	case socketmode.EventTypeConnecting:
		a.logf("slack: connecting")
	case socketmode.EventTypeConnected:
		a.logf("slack: socket connected")
	case socketmode.EventTypeConnectionError:
		a.logf("slack: connection error: %v", evt.Data)
	case socketmode.EventTypeEventsAPI:
		api, ok := evt.Data.(slackevents.EventsAPIEvent)
		if !ok {
			return
		}
		// Ack promptly so Slack does not redeliver; handling is async.
		if evt.Request != nil {
			a.sm.Ack(*evt.Request)
		}
		mention, ok := api.InnerEvent.Data.(*slackevents.AppMentionEvent)
		if !ok {
			return
		}
		a.handleMention(ctx, h, mention)
	}
}

// handleMention normalizes an app-mention and hands it to the router. The
// router's Handle does the daemon round-trip; run it off the socket loop
// so a slow daemon call does not stall event reading.
func (a *Adapter) handleMention(ctx context.Context, h chat.Handler, ev *slackevents.AppMentionEvent) {
	// Ignore our own mentions (defensive; app_mention normally never
	// fires for the bot itself) and other bots to avoid loops.
	if ev.User == "" || ev.User == a.botUserID || ev.BotID != "" {
		return
	}
	conv := conversationKey(ev.Channel, threadRoot(ev.ThreadTimeStamp, ev.TimeStamp))
	text := stripMentions(ev.Text)
	// Resolve the caller and run the daemon round-trip off the socket
	// read loop: resolveCaller may make a users.info call on a cache miss,
	// and Handle does create/inject/wake, neither of which should stall
	// event ingestion.
	go func() {
		msg := chat.Message{
			Conversation: conv,
			Caller:       a.resolveCaller(ctx, ev.User),
			Text:         text,
		}
		if err := h.Handle(ctx, msg); err != nil {
			a.logf("slack: handle %s: %v", conv, err)
		}
	}()
}

// Send renders the reply to Slack mrkdwn and posts it into its originating
// channel + thread. A long turn is split into several ordered in-thread posts
// so no single message is truncated. Text is passed with escape=false because
// toMrkdwn has already escaped Slack control characters itself.
func (a *Adapter) Send(ctx context.Context, r chat.Reply) error {
	channel, thread, ok := splitConversation(r.Conversation)
	if !ok {
		return fmt.Errorf("slack: malformed conversation key %q", r.Conversation)
	}
	rendered := toMrkdwn(r.Text)
	if strings.TrimSpace(rendered) == "" {
		return nil // nothing worth posting
	}
	for _, chunk := range chunkMessage(rendered, slackTextLimit) {
		if strings.TrimSpace(chunk) == "" {
			continue
		}
		if _, _, err := a.api.PostMessageContext(ctx, channel,
			slack.MsgOptionText(chunk, false),
			slack.MsgOptionTS(thread),
		); err != nil {
			return fmt.Errorf("slack: post to %s: %w", r.Conversation, err)
		}
	}
	return nil
}

// resolveCaller maps a Slack user ID onto the asserted-caller identity,
// caching the result. On CallerEmail it calls users.info once per user;
// if the email is unavailable it falls back to the user ID so a turn is
// still attributed (the daemon may then reject an unknown caller, which
// is surfaced upstream rather than silently dropped here).
func (a *Adapter) resolveCaller(ctx context.Context, userID string) string {
	if a.mode == CallerID {
		return userID
	}
	a.mu.Lock()
	if c, ok := a.callerByID[userID]; ok {
		a.mu.Unlock()
		return c
	}
	a.mu.Unlock()

	caller := userID
	if u, err := a.api.GetUserInfoContext(ctx, userID); err != nil {
		a.logf("slack: users.info %s: %v (falling back to user ID)", userID, err)
	} else if email := u.Profile.Email; email != "" {
		caller = email
	} else {
		a.logf("slack: user %s has no email (need users:read.email?); using user ID", userID)
	}

	a.mu.Lock()
	a.callerByID[userID] = caller
	a.mu.Unlock()
	return caller
}

var mentionRE = regexp.MustCompile(`<@[^>]+>`)

// stripMentions removes Slack user-mention markup (<@U123>, <@U123|name>)
// and normalizes surrounding whitespace, leaving the human-readable body.
func stripMentions(text string) string {
	return strings.TrimSpace(strings.Join(strings.Fields(mentionRE.ReplaceAllString(text, " ")), " "))
}

// threadRoot returns the thread timestamp to key a conversation on: the
// existing thread ts for a reply, else the message's own ts (a top-level
// mention roots a new thread).
func threadRoot(threadTS, msgTS string) string {
	if threadTS != "" {
		return threadTS
	}
	return msgTS
}

// conversationKey encodes channel + thread into the stable chat.Message
// conversation key. Slack channel IDs contain no colon, so a single colon
// separator round-trips via splitConversation.
func conversationKey(channel, thread string) string {
	return channel + ":" + thread
}

// splitConversation is the inverse of conversationKey.
func splitConversation(key string) (channel, thread string, ok bool) {
	channel, thread, ok = strings.Cut(key, ":")
	if !ok || channel == "" || thread == "" {
		return "", "", false
	}
	return channel, thread, true
}
