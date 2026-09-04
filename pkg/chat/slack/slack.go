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
// X-Asserted-Caller identity. Aliased to the provider-neutral type so
// every adapter is configured by the same flag with the same meaning.
type CallerMode = chat.CallerMode

const (
	// CallerEmail resolves the user's email via users.info (needs the
	// users:read.email scope) and asserts that. Default; matches the
	// daemon's usual per-caller credential keying.
	CallerEmail = chat.CallerEmail
	// CallerID asserts the raw Slack user ID (e.g. U0123ABC) with no
	// extra API call or scope.
	CallerID = chat.CallerID
)

// Config constructs an Adapter. Tokens are loaded from env by the caller
// (never bare flags).
type Config struct {
	// AppToken is the Socket Mode app-level token (xapp-...). Empty builds an
	// egress-only adapter: it posts, edits and deletes with the bot token,
	// and Run refuses with chat.ErrNoInbound because there is no socket to
	// open. That is what an outbound-only deployment wants — Socket Mode is a
	// receive capability, and a service that only posts should not hold the
	// credential for it, nor have its uptime bound to a connection it never
	// reads (#23).
	AppToken string
	// BotToken is the bot user OAuth token (xoxb-...).
	BotToken string
	// CallerMode selects caller identity resolution; empty means
	// CallerEmail.
	CallerMode CallerMode
	// RichBlocks opts replies into Block Kit rendering (headers, lists,
	// tables, code, blockquotes). The mrkdwn text is always sent alongside
	// as the fallback; on an invalid_blocks rejection the send retries with
	// text only. Default (false) posts flat mrkdwn.
	RichBlocks bool
	// Logf is where the adapter logs; the zero value discards.
	Logf chat.Logf
}

// Adapter satisfies chat.Adapter.
var _ chat.Adapter = (*Adapter)(nil)

// Adapter is the Slack implementation of chat.Adapter.
type Adapter struct {
	api        *slack.Client
	sm         *socketmode.Client
	mode       CallerMode
	richBlocks bool
	logf       chat.Logf

	// botUserID is this bot's own user ID, resolved at Run start and
	// used to ignore our own posts (loop guard).
	botUserID string

	mu         sync.Mutex
	callerByID map[string]string // Slack user ID -> asserted caller
}

// New validates the config and builds an Adapter. It does not open the
// socket; Run does — and with no AppToken there is no socket to open, which
// builds an egress-only adapter rather than failing (see Config.AppToken).
func New(cfg Config) (*Adapter, error) {
	if cfg.BotToken == "" {
		return nil, errors.New("slack: BotToken (xoxb-) is required")
	}
	mode := cfg.CallerMode
	if mode == "" {
		mode = CallerEmail
	}
	// The app-level token is a Socket Mode credential and nothing else, so an
	// egress-only client is built without it and never opens a socket. Egress
	// authenticates with the bot token either way.
	var opts []slack.Option
	if cfg.AppToken != "" {
		opts = append(opts, slack.OptionAppLevelToken(cfg.AppToken))
	}
	api := slack.New(cfg.BotToken, opts...)
	var sm *socketmode.Client
	if cfg.AppToken != "" {
		sm = socketmode.New(api)
	}
	return &Adapter{
		api:        api,
		sm:         sm,
		mode:       mode,
		richBlocks: cfg.RichBlocks,
		logf:       cfg.Logf,
		callerByID: make(map[string]string),
	}, nil
}

// Name identifies the platform.
func (a *Adapter) Name() string { return "slack" }

// Run opens the Socket Mode connection and dispatches each app-mention to
// h until ctx is cancelled or the socket fails unrecoverably. An adapter built
// without an app token has no connection to open and returns chat.ErrNoInbound.
func (a *Adapter) Run(ctx context.Context, h chat.Handler) error {
	if a.sm == nil {
		return fmt.Errorf("slack: no app token, so Socket Mode is not configured: %w", chat.ErrNoInbound)
	}
	if auth, err := a.api.AuthTestContext(ctx); err != nil {
		return fmt.Errorf("slack: auth test: %w", err)
	} else {
		a.botUserID = auth.UserID
		a.logf.Infof("slack: connected as %s (%s)", auth.User, auth.UserID)
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
		a.logf.Infof("slack: connecting")
	case socketmode.EventTypeConnected:
		a.logf.Infof("slack: socket connected")
	case socketmode.EventTypeConnectionError:
		a.logf.Warnf("slack: connection error: %v", evt.Data)
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
	case socketmode.EventTypeSlashCommand:
		sc, ok := evt.Data.(slack.SlashCommand)
		if !ok {
			return
		}
		a.handleSlashCommand(ctx, h, evt.Request, sc)
	case socketmode.EventTypeInteractive:
		cb, ok := evt.Data.(slack.InteractionCallback)
		if !ok {
			return
		}
		a.handleInteractive(ctx, h, evt.Request, cb)
	}
}

// handleInteractive routes a button press back to the gateway.
//
// The ack is the first thing this does, before the press is read as one of
// ours. Slack gives three seconds and then tells the person their request
// failed, and answering a permission prompt is exactly the moment not to say
// that: the decision is about to be applied whatever the dialog claims, so a
// late ack turns a working approval into one somebody re-presses. Handing the
// press on happens off this goroutine for the same reason — the router's side
// of it is a daemon round trip.
//
// The only interaction that reaches Slack without an ack is one the socket loop
// could not read as a callback at all, which it drops before calling this.
func (a *Adapter) handleInteractive(ctx context.Context, h chat.Handler, req *socketmode.Request, cb slack.InteractionCallback) {
	if req != nil {
		if err := a.sm.Ack(*req); err != nil {
			// WARN where the slash command's ack below is ERROR, and the
			// difference is what the ack carries. This one is empty: the press
			// is handed on regardless, so a lost ack costs a spinner. The slash
			// command's ack *is* the reply, and losing it loses the answer.
			a.logf.Warnf("slack: ack interaction: %v", err)
		}
	}
	press, ok := pressFrom(cb)
	if !ok {
		return
	}
	go func() {
		// Resolved from the callback's own user, never from the message the
		// buttons are attached to: switchboard posted that message, and the
		// whole point of the press is that a *person* answered.
		press.Caller = a.resolveCaller(ctx, cb.User.ID)
		if err := h.HandlePress(ctx, press); err != nil {
			a.logf.Errorf("slack: press %s on %s: %v", press.Option, press.Conversation, err)
		}
	}()
}

// handleSlashCommand routes a native Slack slash command (e.g.
// "/switchboard progress status") to the gateway and acks it with an
// ephemeral reply visible only to the invoker. The router's HandleCommand is
// an in-memory map operation, so running it inline keeps well within Slack's
// 3s ack window. The command's scope is the channel it was issued in.
func (a *Adapter) handleSlashCommand(ctx context.Context, h chat.Handler, req *socketmode.Request, sc slack.SlashCommand) {
	cmd := parseSlashCommand(sc)
	cmd.Caller = a.resolveCaller(ctx, sc.UserID)
	ack, err := h.HandleCommand(ctx, cmd)
	if err != nil {
		a.logf.Errorf("slack: slash command %q: %v", cmd.Name, err)
		ack = "Sorry, that command failed."
	}
	if req != nil {
		if aerr := a.sm.Ack(*req, map[string]any{"response_type": "ephemeral", "text": ack}); aerr != nil {
			a.logf.Errorf("slack: ack slash command: %v", aerr)
		}
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
	// A mention that tightly matches a gateway command (e.g. "@switchboard
	// progress status") configures the gateway instead of running an agent
	// turn; anything else is agent input.
	if cmd, ok := parseMentionCommand(text); ok {
		cmd.Channel = ev.Channel
		go a.runMentionCommand(ctx, h, cmd, conv, ev.User)
		return
	}
	// Resolve the caller and run the daemon round-trip off the socket
	// read loop: resolveCaller may make a users.info call on a cache miss,
	// and Handle does create/inject/wake, neither of which should stall
	// event ingestion.
	go func() {
		msg := chat.Message{
			Conversation: conv,
			Channel:      ev.Channel,
			Caller:       a.resolveCaller(ctx, ev.User),
			Text:         text,
		}
		if err := h.Handle(ctx, msg); err != nil {
			a.logf.Errorf("slack: handle %s: %v", conv, err)
		}
	}()
}

// runMentionCommand executes a command parsed from an app-mention and posts
// its acknowledgment back into the originating thread (unlike a slash command,
// a mention has a thread to reply in). Run off the socket loop because
// resolveCaller may make a users.info call on a cache miss.
func (a *Adapter) runMentionCommand(ctx context.Context, h chat.Handler, cmd chat.Command, conv, userID string) {
	cmd.Caller = a.resolveCaller(ctx, userID)
	ack, err := h.HandleCommand(ctx, cmd)
	if err != nil {
		a.logf.Errorf("slack: mention command %q: %v", cmd.Name, err)
		return
	}
	if _, err := a.Send(ctx, chat.Reply{Conversation: conv, Text: ack}); err != nil {
		a.logf.Errorf("slack: command ack %s: %v", conv, err)
	}
}

// Send renders the reply and posts it into its originating channel + thread —
// or at the top level of the channel when the conversation key carries no
// thread, as an outbound-ingress post does — returning a ref to the first
// posted message (for later Update/Delete). The
// always-on baseline is Slack mrkdwn, split into several ordered in-thread
// posts so no single message is truncated (escape=false because toMrkdwn has
// already escaped Slack control characters). When RichBlocks is enabled it
// first attempts a single Block Kit message (with the mrkdwn text attached as
// the notification/fallback); if the renderer declines (nil) or Slack rejects
// the payload (invalid_blocks), it falls back to the plain mrkdwn path so a
// rich render never loses a message. A reply carrying Usage gets it as a
// trailing context block — only on the Block Kit path, since appending a line
// to chunked mrkdwn would land it in whichever chunk had room.
func (a *Adapter) Send(ctx context.Context, r chat.Reply) (chat.MessageRef, error) {
	channel, thread, ok := splitConversation(r.Conversation)
	if !ok {
		return chat.MessageRef{}, fmt.Errorf("slack: malformed conversation key %q", r.Conversation)
	}
	rendered := toMrkdwn(r.Text)
	if strings.TrimSpace(rendered) == "" {
		return chat.MessageRef{}, nil // nothing worth posting
	}

	// A question with answers is posted as buttons whatever RichBlocks says.
	// That flag is an opinion about how prose should look; this is the
	// difference between a question somebody can answer and one they can only
	// read. On rejection it falls through to the same text path as everything
	// else, which still carries the options in prose.
	if blocks := sanitizeBlocks(decisionBlocks(r.Text, r.Decision, toMrkdwn)); blocks != nil {
		opts := append(threadOpt(thread),
			slack.MsgOptionBlocks(toSlackBlocks(blocks)...),
			slack.MsgOptionText(clamp(rendered, maxSectionText), false),
		)
		_, ts, err := a.api.PostMessageContext(ctx, channel, opts...)
		if err == nil {
			return chat.MessageRef{Conversation: landedKey(channel, thread, ts), ID: ts}, nil
		}
		if !isBlockRejection(err) {
			return chat.MessageRef{}, fmt.Errorf("slack: post decision to %s: %w", r.Conversation, platformErr(err))
		}
		a.logf.Warnf("slack: decision blocks rejected for %s (%v); retrying as text", r.Conversation, err)
	}

	if a.richBlocks {
		blocks := sanitizeBlocks(withUsageFooter(renderBlocks(r.Text, toMrkdwn), r.Usage))
		if blocks != nil {
			// The text fallback is for notifications/old clients only; clamp it
			// so a very long turn does not bloat the payload (blocks carry the
			// full content).
			opts := append(threadOpt(thread),
				slack.MsgOptionBlocks(toSlackBlocks(blocks)...),
				slack.MsgOptionText(clamp(rendered, maxSectionText), false),
			)
			_, ts, err := a.api.PostMessageContext(ctx, channel, opts...)
			if err == nil {
				return chat.MessageRef{Conversation: landedKey(channel, thread, ts), ID: ts}, nil
			}
			if !isBlockRejection(err) {
				return chat.MessageRef{}, fmt.Errorf("slack: post blocks to %s: %w", r.Conversation, platformErr(err))
			}
			a.logf.Warnf("slack: blocks rejected for %s (%v); retrying as text", r.Conversation, err)
		}
	}

	var ref chat.MessageRef // first posted message, returned to the caller
	for _, chunk := range chunkMessage(rendered, slackTextLimit) {
		if strings.TrimSpace(chunk) == "" {
			continue
		}
		opts := append(threadOpt(thread), slack.MsgOptionText(chunk, false))
		_, ts, err := a.api.PostMessageContext(ctx, channel, opts...)
		if err != nil {
			return ref, fmt.Errorf("slack: post to %s: %w", r.Conversation, platformErr(err))
		}
		if ref.ID == "" {
			// landedKey reads the thread this message was posted to, so it must
			// run before the adoption below overwrites it.
			ref = chat.MessageRef{Conversation: landedKey(channel, thread, ts), ID: ts}
		}
		// A thread-less post roots its own thread: adopt it so the rest of a
		// chunked message replies under the first part instead of scattering
		// across the channel.
		if thread == "" {
			thread = ts
		}
	}
	return ref, nil
}

// threadOpt targets an existing thread, or nothing at all when the
// conversation carries no thread — Slack's thread_ts must be omitted, not sent
// empty, for a message to land at the top level of a channel.
func threadOpt(thread string) []slack.MsgOption {
	if thread == "" {
		return nil
	}
	return []slack.MsgOption{slack.MsgOptionTS(thread)}
}

// Update replaces a previously posted message's content in place — the
// mechanism behind long-turn status edits. A zero ref no-ops. Rendering
// mirrors Send but targets a single message: Block Kit when enabled and
// accepted, else clamped mrkdwn (an update cannot be split across messages).
// Slack supports editing, so this never returns chat.ErrUnsupported.
func (a *Adapter) Update(ctx context.Context, ref chat.MessageRef, r chat.Reply) error {
	if ref.ID == "" {
		return nil
	}
	channel, _, ok := splitConversation(ref.Conversation)
	if !ok {
		return fmt.Errorf("slack: malformed conversation key %q", ref.Conversation)
	}
	rendered := toMrkdwn(r.Text)

	if a.richBlocks {
		blocks := sanitizeBlocks(renderBlocks(r.Text, toMrkdwn))
		if blocks != nil {
			_, _, _, err := a.api.UpdateMessageContext(ctx, channel, ref.ID,
				slack.MsgOptionBlocks(toSlackBlocks(blocks)...),
				slack.MsgOptionText(clamp(rendered, maxSectionText), false),
			)
			if err == nil {
				return nil
			}
			if !isBlockRejection(err) {
				return fmt.Errorf("slack: update blocks in %s: %w", ref.Conversation, platformErr(err))
			}
			a.logf.Warnf("slack: blocks rejected updating %s (%v); retrying as text", ref.Conversation, err)
		}
	}

	// An empty block set, explicitly, not a missing one. chat.update leaves
	// whatever blocks the message already has when the field is absent, so a
	// text-only edit of a message that was posted with blocks keeps rendering
	// them — including a permission question's buttons, which stay pressable
	// after the decision they were asking about has been made. Sending "[]"
	// is what takes them down, and it costs nothing on a message that never
	// had any.
	if _, _, _, err := a.api.UpdateMessageContext(ctx, channel, ref.ID,
		slack.MsgOptionText(clamp(rendered, slackTextLimit), false),
		slack.MsgOptionBlocks(),
	); err != nil {
		return fmt.Errorf("slack: update %s: %w", ref.Conversation, platformErr(err))
	}
	return nil
}

// Delete removes a previously posted message — used to clear a progress
// placeholder once the real reply is ready. A zero ref no-ops. Slack
// supports deletion, so this never returns chat.ErrUnsupported.
func (a *Adapter) Delete(ctx context.Context, ref chat.MessageRef) error {
	if ref.ID == "" {
		return nil
	}
	channel, _, ok := splitConversation(ref.Conversation)
	if !ok {
		return fmt.Errorf("slack: malformed conversation key %q", ref.Conversation)
	}
	if _, _, err := a.api.DeleteMessageContext(ctx, channel, ref.ID); err != nil {
		return fmt.Errorf("slack: delete %s: %w", ref.Conversation, platformErr(err))
	}
	return nil
}

// clamp truncates s to at most max bytes, mirroring Slack's field limits.
func clamp(s string, max int) string {
	if len(s) > max {
		return s[:max]
	}
	return s
}

// FitsOneMessage reports whether text renders into a single Slack message
// rather than being split across several by Send. It renders the text to
// measure it, because mrkdwn escaping can grow it (every "&" becomes
// "&amp;") — a caller that needs one editable message cannot afford to
// guess from the raw length. Implements chat.TextFitter.
func (a *Adapter) FitsOneMessage(text string) bool {
	return len(toMrkdwn(text)) <= slackTextLimit
}

// classify maps Slack's error codes onto the provider-neutral sentinels in
// pkg/chat, so callers outside this package can tell a permanent failure from
// a transient one without learning Slack's vocabulary. Anything unrecognized
// (rate limits, transport failures, an error code Slack added since) stays
// unclassified and is treated as retryable.
func classify(err error) error {
	var se slack.SlackErrorResponse
	if !errors.As(err, &se) {
		return nil
	}
	switch se.Err {
	case "channel_not_found", "message_not_found", "thread_not_found", "user_not_found":
		return chat.ErrNotFound
	case "not_in_channel", "is_archived", "channel_is_archived", "cant_update_message",
		"cant_delete_message", "message_not_editable", "restricted_action", "no_permission",
		"access_denied", "ekm_access_denied", "not_allowed_token_type":
		return chat.ErrDenied
	}
	return nil
}

// platformErr wraps a Slack failure with its classification, if it has one.
// The result reads exactly like err (no sentinel noise in the log line) but
// errors.Is finds chat.ErrNotFound / chat.ErrDenied through it.
func platformErr(err error) error {
	class := classify(err)
	if class == nil {
		return err
	}
	return classifiedError{err: err, class: class}
}

// classifiedError carries a platform error and its neutral classification.
type classifiedError struct {
	err   error
	class error
}

func (e classifiedError) Error() string   { return e.err.Error() }
func (e classifiedError) Unwrap() []error { return []error{e.err, e.class} }

// isBlockRejection reports whether err is Slack rejecting the blocks payload
// specifically (invalid_blocks / invalid_block(s)), as opposed to a transport
// or auth error — the former is recoverable by resending as plain text.
func isBlockRejection(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "invalid_blocks") ||
		strings.Contains(msg, "invalid_block") ||
		strings.Contains(msg, "blocks_")
}

// resolveCaller maps a Slack user ID onto the asserted-caller identity,
// caching the result. On CallerEmail it calls users.info once per user;
// if the email is unavailable it falls back to the user ID so a turn is
// still attributed (the daemon may then reject an unknown caller, which
// is surfaced upstream rather than silently dropped here).
//
// A failed lookup is not cached. The fallback it produces is now an input to
// an authorization decision — an approver list is keyed by this string — and a
// cached fallback is a permanent one: a single 429 on the first press of the
// day would map an approver onto their user ID for the life of the process and
// refuse every answer they gave afterwards, with nothing to read but one
// users.info line from hours earlier. Re-asking costs one call per turn while
// the API is unhappy, which is the cheaper failure by a wide margin. A lookup
// that succeeded and simply had no email to give is a fact about the user
// rather than a blip, so that one is cached.
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

	u, err := a.api.GetUserInfoContext(ctx, userID)
	if err != nil {
		a.logf.Warnf("slack: users.info %s: %v (falling back to user ID, not cached)", userID, err)
		return userID
	}
	caller := userID
	if email := u.Profile.Email; email != "" {
		caller = email
	} else {
		a.logf.Warnf("slack: user %s has no email (need users:read.email?); using user ID", userID)
	}

	a.mu.Lock()
	a.callerByID[userID] = caller
	a.mu.Unlock()
	return caller
}

// mentionRE matches Slack user-mention markup (<@U123>, <@U123|name>) plus any
// spaces/tabs hugging it — but never newlines, so line breaks and leading
// indentation survive the strip.
var mentionRE = regexp.MustCompile(`[ \t]*<@[^>]+>[ \t]*`)

// stripMentions removes Slack user-mention markup and trims surrounding
// whitespace, leaving the human-readable body. Each mention (with its adjacent
// spaces) collapses to a single space so words never run together, but internal
// newlines and indentation are preserved: markdown block structure (headers,
// lists, tables, code fences) is newline-driven, so flattening all whitespace
// would turn a multi-line turn into a single unrenderable line.
func stripMentions(text string) string {
	return strings.TrimSpace(mentionRE.ReplaceAllString(text, " "))
}

// mentionCommandVerbs are the leading words that mark an app-mention as a
// gateway command rather than an agent turn. Kept deliberately small: a
// mention is only treated as a command on a tight match (see
// parseMentionCommand), so a normal turn that happens to mention a verb still
// reaches the daemon. The router owns what each verb means and validates the
// argument; the adapter only recognizes the surface.
var mentionCommandVerbs = map[string]bool{"progress": true}

// parseMentionCommand recognizes a gateway command inside an app-mention body,
// e.g. "progress status". To keep agent turns from being swallowed it matches
// only tightly: a known verb alone (query the setting) or a known verb plus a
// single argument (set it). Three or more tokens — "progress on the ticket?" —
// is treated as agent input, not a command.
func parseMentionCommand(text string) (chat.Command, bool) {
	fields := strings.Fields(text)
	if len(fields) == 0 || len(fields) > 2 || !mentionCommandVerbs[strings.ToLower(fields[0])] {
		return chat.Command{}, false
	}
	cmd := chat.Command{Name: strings.ToLower(fields[0])}
	if len(fields) == 2 {
		cmd.Args = []string{fields[1]}
	}
	return cmd, true
}

// parseSlashCommand normalizes a Slack slash command into a neutral
// chat.Command. The invoked command word (e.g. "/switchboard") is dropped —
// Slack carries it in sc.Command — and the remaining text is split into the
// verb and its arguments. An explicit surface, so it parses freely (no arity
// guard); the router validates. Caller is filled in by the dispatcher.
func parseSlashCommand(sc slack.SlashCommand) chat.Command {
	cmd := chat.Command{Channel: sc.ChannelID}
	if fields := strings.Fields(sc.Text); len(fields) > 0 {
		cmd.Name = strings.ToLower(fields[0])
		cmd.Args = fields[1:]
	}
	return cmd
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

// landedKey names the conversation a just-posted message went into, for the
// ref Send hands back. channel and thread are what splitConversation read from
// the key it was asked to post to, and ts is the timestamp Slack assigned.
//
// A post with no thread roots one of its own, and the ref has to say so: a ref
// is an address to come back to, and the outbound ingress builds the
// continuation of an overflowing message out of ref.Conversation. Slack is the
// platform where rebuilding it from the id would have worked anyway — a
// top-level message's id *is* the thread_ts of the thread it roots — but that
// identity holds on no other platform, so the ingress must be able to trust
// the ref instead of knowing the rule (#39). Update and Delete are unaffected:
// both take the channel back out with splitConversation.
func landedKey(channel, thread, ts string) string {
	if thread != "" {
		return conversationKey(channel, thread)
	}
	return conversationKey(channel, ts)
}

// splitConversation is the inverse of conversationKey. The thread may be
// absent — a bare channel ID ("C0123"), or "C0123:" — which egress reads as
// "post a new top-level message in this channel". That is the shape the
// outbound ingress hands to Send when a caller posts with no thread to reply
// in; every inbound key carries a thread. ok is false only when the channel is
// missing.
func splitConversation(key string) (channel, thread string, ok bool) {
	channel, thread, _ = strings.Cut(key, ":")
	if channel == "" {
		return "", "", false
	}
	return channel, thread, true
}
