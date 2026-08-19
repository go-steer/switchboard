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

// Package googlechat implements chat.Adapter over Google Chat, as a Google
// Workspace add-on that extends Chat.
//
// Ingress is Pub/Sub — the add-on is configured to publish events to a topic
// and switchboard pulls them from a subscription, so (like Slack Socket Mode)
// no public webhook is exposed, matching the distroless posture. That choice
// costs one add-on feature: a dialog needs a synchronous HTTP response, so
// there are none here; interaction happens through card buttons instead, whose
// clicks arrive as ordinary events and are answered by patching the card. In
// exchange the gateway stays a pull-only client with no inbound surface.
//
// Egress is the Google Chat REST API (spaces.messages create/patch/delete),
// which lets every long-turn progress mode work: the placeholder can be edited
// in place and retired. Replies are Chat text by default and cards where a card
// says it better (see cards.go); a card is always paired with a text fallback,
// and any card Chat rejects degrades to that text rather than losing the reply.
//
// A conversation is keyed on space + thread, so every mention in a thread
// continues the same core-agent session. The caller asserted to the daemon is
// the sender's email address, or their user resource name (users/NNN) when the
// event carried no email or CallerMode says so; per-caller credential resolution
// and verified identity live in core-agent (W0), not here. All platform
// specifics stay behind chat.Adapter so the router never imports this package.
package googlechat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"cloud.google.com/go/pubsub"
	chatv1 "google.golang.org/api/chat/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/go-steer/switchboard/pkg/chat"
)

// messageReplyOption tells Chat to post into the referenced thread, starting a
// new thread only if that one no longer exists — the threading behavior a
// conversation-scoped gateway wants.
const messageReplyOption = "REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD"

// patchMask is the update mask every rewrite sends. Both fields are always
// named: Chat clears a masked field the patch omits, so naming both is what
// makes a card replace the text it supersedes, and vice versa.
//
// Only a fixed set of paths is updatable — text, attachment, cards, cards_v2,
// accessory_widgets, quoted_message_metadata. fallbackText is a real field on
// Message but is not among them, and naming it fails the whole request with
// 400 INVALID_ARGUMENT. A patched card therefore keeps whatever fallback text
// it was created with.
const patchMask = "text,cardsV2"

// messenger is the subset of the Chat REST surface the adapter's egress needs,
// narrowed to an interface so Send/Update/Delete are testable with a fake and
// the adapter never depends on a live Chat service in unit tests.
type messenger interface {
	create(ctx context.Context, parent string, msg *chatv1.Message) (*chatv1.Message, error)
	patch(ctx context.Context, name string, msg *chatv1.Message, mask string) error
	delete(ctx context.Context, name string) error
}

// Config constructs an Adapter. Credentials come from Application Default
// Credentials (workload identity in cluster, or GOOGLE_APPLICATION_CREDENTIALS
// locally) — never bare flags — and must grant both Pub/Sub subscribe on the
// subscription and the Chat bot scope for egress.
type Config struct {
	// ProjectID is the GCP project hosting the Pub/Sub subscription.
	ProjectID string
	// SubscriptionID is the Pub/Sub subscription carrying Google Chat events.
	SubscriptionID string
	// Cards selects how much of the output is rendered as cards. The zero
	// value means CardsRich (gateway messages and a structured answer both as
	// cards); CardsStatus leaves answers as text.
	Cards CardMode
	// CallerMode selects which form of the sender's identity is asserted.
	// The zero value means chat.CallerEmail.
	CallerMode chat.CallerMode
	// Commands maps a Chat app-command ID onto a gateway command verb. Chat
	// identifies a slash or quick command by the numeric ID configured in the
	// Chat API console, never by name, so this is how "/progress" (ID 2)
	// becomes the "progress" verb. An ID with no mapping falls back to reading
	// the verb from the first word the invoker typed, which is what a single
	// catch-all "/switchboard progress stream" command needs.
	Commands map[int64]string
	// LogEvents logs every inbound Pub/Sub payload verbatim. It exists to
	// capture real Chat traffic as decoder fixtures (see
	// docs/googlechat-setup.md): what Google actually sends is the one thing
	// hand-written test fixtures cannot verify. Off by default — the payload
	// carries the message text and the sender's resource name.
	LogEvents bool
	// Logf is an optional structured-ish logger; nil discards.
	Logf func(format string, args ...any)
}

// Adapter satisfies chat.Adapter.
var _ chat.Adapter = (*Adapter)(nil)

// Adapter is the Google Chat implementation of chat.Adapter.
type Adapter struct {
	ps        *pubsub.Client
	sub       string
	msg       messenger
	cards     CardMode
	caller    chat.CallerMode
	cmds      map[int64]string
	logEvents bool
	logf      func(string, ...any)
}

// New validates the config and builds an Adapter, constructing the Pub/Sub
// client and Chat REST service from Application Default Credentials. It does not
// open the subscription; Run does. Client construction uses a background
// context because the clients are long-lived and outlive any single serve
// context.
func New(cfg Config) (*Adapter, error) {
	if cfg.ProjectID == "" {
		return nil, errors.New("googlechat: ProjectID is required")
	}
	if cfg.SubscriptionID == "" {
		return nil, errors.New("googlechat: SubscriptionID is required")
	}
	cards, ok := ParseCardMode(string(cfg.Cards))
	if !ok {
		return nil, fmt.Errorf("googlechat: invalid card mode %q (want off, status, or rich)", cfg.Cards)
	}
	caller := cfg.CallerMode
	if caller == "" {
		caller = chat.CallerEmail
	}
	if _, ok := chat.ParseCallerMode(string(caller)); !ok {
		return nil, fmt.Errorf("googlechat: invalid caller mode %q (want email or id)", cfg.CallerMode)
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	ctx := context.Background()
	ps, err := pubsub.NewClient(ctx, cfg.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("googlechat: pubsub client: %w", err)
	}
	svc, err := chatv1.NewService(ctx, option.WithScopes(chatv1.ChatBotScope))
	if err != nil {
		ps.Close()
		return nil, fmt.Errorf("googlechat: chat service: %w", err)
	}
	return &Adapter{
		ps:        ps,
		sub:       cfg.SubscriptionID,
		msg:       restMessenger{svc: svc},
		cards:     cards,
		caller:    caller,
		cmds:      cfg.Commands,
		logEvents: cfg.LogEvents,
		logf:      logf,
	}, nil
}

// Name identifies the platform.
func (a *Adapter) Name() string { return "googlechat" }

// Run pulls Google Chat events from the Pub/Sub subscription and dispatches
// each user message to h until ctx is cancelled or the subscription fails
// unrecoverably. Receive fans messages out across goroutines and manages ack
// deadlines while a handler runs, so the daemon round-trip can happen inline.
func (a *Adapter) Run(ctx context.Context, h chat.Handler) error {
	a.logf("googlechat: subscribing to %s", a.sub)
	sub := a.ps.Subscription(a.sub)
	return sub.Receive(ctx, func(ctx context.Context, m *pubsub.Message) {
		a.dispatch(ctx, h, m)
	})
}

// dispatch translates one Pub/Sub message into a turn, a gateway command, a
// button click, or a welcome, and hands it to the router. It always acks: a
// message we cannot act on (a lifecycle event, a bot sender, a malformed
// payload) must not be redelivered forever, and a daemon failure is logged
// rather than retried (a retry would re-inject and could duplicate the turn),
// mirroring the Slack adapter.
func (a *Adapter) dispatch(ctx context.Context, h chat.Handler, m *pubsub.Message) {
	defer m.Ack()
	// One line, one payload, compacted: the log is meant to be sliced straight
	// into testdata/events as a decoder fixture.
	if a.logEvents {
		a.logf("googlechat: event %s", compactJSON(m.Data))
	}
	in, err := decodeEvent(m.Data)
	if err != nil {
		a.logf("googlechat: %v", err)
		return
	}
	in.caller = a.callerOf(in)
	conv := conversationKey(in.space, in.thread)
	switch in.kind {
	case kindMessage:
		msg := chat.Message{
			Conversation: conv,
			Channel:      in.space,
			Caller:       in.caller,
			Text:         in.text,
		}
		if err := h.Handle(ctx, msg); err != nil {
			a.logf("googlechat: handle %s: %v", conv, err)
		}
	case kindCommand:
		a.runCommand(ctx, h, conv, a.commandOf(in))
	case kindButton:
		a.runButton(ctx, h, in, conv)
	case kindWelcome:
		a.welcome(ctx, h, conv)
	}
}

// callerOf picks which form of the sender's identity to assert. Email is the
// default because that is what the daemon keys per-caller credentials by, and
// what the Slack adapter asserts — a human should not be two identities
// depending on which chat window they used. The resource name is the fallback
// whenever the event carried no email (an event with no chat.user, a payload
// shape that omits the address), so a turn is never dropped for want of one.
// Note that the daemon must have the asserted form provisioned either way; see
// docs/googlechat-setup.md §C.
func (a *Adapter) callerOf(in inbound) string {
	if a.caller != chat.CallerID && in.email != "" {
		return in.email
	}
	return in.caller
}

// commandOf turns a decoded command event into a gateway command. The verb
// comes from the configured ID mapping when there is one, and otherwise from
// the first word of the argument text — the shape a single catch-all Chat
// command takes, where "/switchboard progress stream" carries its own verb.
func (a *Adapter) commandOf(in inbound) chat.Command {
	cmd := chat.Command{Channel: in.space, Caller: in.caller}
	fields := strings.Fields(in.cmdArgs)
	if verb, ok := a.cmds[in.cmdID]; ok && verb != "" {
		cmd.Name = strings.ToLower(verb)
		cmd.Args = fields
		return cmd
	}
	if len(fields) > 0 {
		cmd.Name = strings.ToLower(fields[0])
		cmd.Args = fields[1:]
	}
	return cmd
}

// runCommand runs a gateway command and posts its acknowledgment back into the
// invoking thread. Google Chat has no ephemeral async reply, so (like the Slack
// mention subcommand) the ack is a normal in-thread message — a card carrying a
// button per accepted value when the handler reports its choices, so the next
// change is a click rather than another typed command.
func (a *Adapter) runCommand(ctx context.Context, h chat.Handler, conv string, cmd chat.Command) {
	ack, err := h.HandleCommand(ctx, cmd)
	if err != nil {
		a.logf("googlechat: command %q: %v", cmd.Name, err)
		return
	}
	if ack == "" {
		return
	}
	if _, err := a.post(ctx, conv, a.ackCardFor(h, cmd.Name, ack), toChatText(ack)); err != nil {
		a.logf("googlechat: command ack %s: %v", conv, err)
	}
}

// runButton handles a click on a card the gateway posted. The click carries the
// command it stands for, so it runs exactly as though the invoker had typed it;
// the hosting card is then patched with the new acknowledgment, which is how a
// Pub/Sub add-on answers an interaction at all (there is no synchronous
// response to return one in). Patching is idempotent — the same click twice
// writes the same card — so a Pub/Sub redelivery is harmless.
func (a *Adapter) runButton(ctx context.Context, h chat.Handler, in inbound, conv string) {
	name := in.params[paramCommand]
	if name == "" {
		return // not one of ours
	}
	cmd := chat.Command{Channel: in.space, Caller: in.caller, Name: strings.ToLower(name)}
	if arg := in.params[paramArg]; arg != "" {
		cmd.Args = []string{arg}
	}
	ack, err := h.HandleCommand(ctx, cmd)
	if err != nil {
		a.logf("googlechat: button %q: %v", cmd.Name, err)
		return
	}
	if ack == "" {
		return
	}
	// Rewrite the card in place when we know which message hosts it; a click on
	// a card we cannot locate still deserves an answer, so it falls back to a
	// fresh reply in the thread.
	if in.messageName == "" {
		if _, err := a.post(ctx, conv, a.ackCardFor(h, cmd.Name, ack), toChatText(ack)); err != nil {
			a.logf("googlechat: button ack %s: %v", conv, err)
		}
		return
	}
	if err := a.rewrite(ctx, in.messageName, a.ackCardFor(h, cmd.Name, ack), toChatText(ack)); err != nil {
		a.logf("googlechat: button ack %s: %v", in.messageName, err)
	}
}

// welcome greets a space the app was just added to, explaining what a mention
// does and offering the progress modes as buttons.
func (a *Adapter) welcome(ctx context.Context, h chat.Handler, conv string) {
	var card *chatv1.GoogleAppsCardV1Card
	if a.cards != CardsOff {
		card = welcomeCard(choicesOf(h, "progress"))
	}
	if _, err := a.post(ctx, conv, card, toChatText(welcomeText)); err != nil {
		a.logf("googlechat: welcome %s: %v", conv, err)
	}
}

// ackCardFor renders a command acknowledgment as a card, or nil when cards are
// off. The accepted values come from the handler, so the buttons stay in step
// with what the command actually takes and this package learns no router
// vocabulary.
func (a *Adapter) ackCardFor(h chat.Handler, name, ack string) *chatv1.GoogleAppsCardV1Card {
	if a.cards == CardsOff {
		return nil
	}
	return ackCard(ack, name, choicesOf(h, name))
}

// choicesOf asks a handler for a command's accepted values, tolerating one that
// does not implement the optional capability.
func choicesOf(h chat.Handler, name string) []string {
	c, ok := h.(chat.CommandChoices)
	if !ok {
		return nil
	}
	return c.Choices(name)
}

// Send posts the reply into its originating space + thread, returning a ref to
// the first posted message (for later Update/Delete). A card is used where the
// reply's kind calls for one; otherwise, or if Chat rejects the card, the reply
// goes as Chat text split across as many in-thread posts as it needs so nothing
// is truncated. An empty reply posts nothing.
func (a *Adapter) Send(ctx context.Context, r chat.Reply) (chat.MessageRef, error) {
	return a.post(ctx, r.Conversation, a.cardFor(r), toChatText(strings.TrimSpace(r.Text)))
}

// cardFor picks the card that renders this reply, or nil for the text path.
func (a *Adapter) cardFor(r chat.Reply) *chatv1.GoogleAppsCardV1Card {
	if a.cards == CardsOff || strings.TrimSpace(r.Text) == "" {
		return nil
	}
	if r.Kind == chat.KindAnswer {
		if a.cards != CardsRich {
			return nil
		}
		return withUsageFooter(answerCard(r.Text), r.Usage)
	}
	return gatewayCard(r.Kind, toChatText(r.Text))
}

// post sends one reply into a conversation: a single card message when card is
// non-nil, otherwise text chunked to Chat's per-message limit. A card Chat
// rejects as malformed falls back to the text path — a render must never cost a
// reply — while any other failure is returned, so a missing space or a denied
// post reaches the caller classified.
func (a *Adapter) post(ctx context.Context, conv string, card *chatv1.GoogleAppsCardV1Card, text string) (chat.MessageRef, error) {
	space, thread, ok := splitConversation(conv)
	if !ok {
		return chat.MessageRef{}, fmt.Errorf("googlechat: malformed conversation key %q", conv)
	}
	text = strings.TrimSpace(text)
	if text == "" && card == nil {
		return chat.MessageRef{}, nil // nothing worth posting
	}

	if cards := singleCard(card); cards != nil {
		msg := &chatv1.Message{CardsV2: cards, FallbackText: clamp(text, chatTextLimit)}
		if thread != "" {
			msg.Thread = &chatv1.Thread{Name: thread}
		}
		created, err := a.msg.create(ctx, space, msg)
		if err == nil {
			return chat.MessageRef{Conversation: conv, ID: created.Name}, nil
		}
		if !isCardRejection(err) {
			return chat.MessageRef{}, fmt.Errorf("googlechat: post card to %s: %w", conv, platformErr(err))
		}
		a.logf("googlechat: card rejected for %s (%v); retrying as text", conv, err)
	}
	if text == "" {
		return chat.MessageRef{}, nil
	}

	var first chat.MessageRef
	for _, part := range chunk(text, chatTextLimit) {
		msg := &chatv1.Message{Text: part}
		if thread != "" {
			msg.Thread = &chatv1.Thread{Name: thread}
		}
		created, err := a.msg.create(ctx, space, msg)
		if err != nil {
			return first, fmt.Errorf("googlechat: post to %s: %w", conv, platformErr(err))
		}
		if first.ID == "" {
			first = chat.MessageRef{Conversation: conv, ID: created.Name}
		}
		// If we started without a thread (a flat space), adopt the thread the
		// first message landed in so the rest of a chunked reply stays together
		// rather than scattering into separate new threads.
		if thread == "" && created.Thread != nil && created.Thread.Name != "" {
			thread = created.Thread.Name
		}
	}
	return first, nil
}

// Update replaces a previously posted message in place — the mechanism behind
// long-turn status edits, and how a card answers a button click. A zero ref
// no-ops. Google Chat supports editing an app's own messages, so this never
// returns chat.ErrUnsupported. An update cannot be split across messages, so an
// over-long text is clamped rather than chunked.
func (a *Adapter) Update(ctx context.Context, ref chat.MessageRef, r chat.Reply) error {
	if ref.ID == "" {
		return nil
	}
	return a.rewrite(ctx, ref.ID, a.cardFor(r), toChatText(strings.TrimSpace(r.Text)))
}

// rewrite patches one message to the given card or text, falling back to text
// when Chat rejects the card.
func (a *Adapter) rewrite(ctx context.Context, name string, card *chatv1.GoogleAppsCardV1Card, text string) error {
	text = clamp(strings.TrimSpace(text), chatTextLimit)
	if cards := singleCard(card); cards != nil {
		// Chat clears any field named in the mask but absent from the message,
		// so patching to a card also drops the text the message used to carry.
		// FallbackText is deliberately not sent: it is not maskable, so it
		// would be ignored either way (see patchMask).
		err := a.msg.patch(ctx, name, &chatv1.Message{CardsV2: cards}, patchMask)
		if err == nil {
			return nil
		}
		if !isCardRejection(err) {
			return fmt.Errorf("googlechat: update %s: %w", name, platformErr(err))
		}
		a.logf("googlechat: card rejected updating %s (%v); retrying as text", name, err)
	}
	if err := a.msg.patch(ctx, name, &chatv1.Message{Text: text}, patchMask); err != nil {
		return fmt.Errorf("googlechat: update %s: %w", name, platformErr(err))
	}
	return nil
}

// Delete removes a previously posted message — used to clear a progress
// placeholder once the real reply is ready. A zero ref no-ops. Google Chat
// supports deleting an app's own messages, so this never returns
// chat.ErrUnsupported.
func (a *Adapter) Delete(ctx context.Context, ref chat.MessageRef) error {
	if ref.ID == "" {
		return nil
	}
	if err := a.msg.delete(ctx, ref.ID); err != nil {
		return fmt.Errorf("googlechat: delete %s: %w", ref.ID, platformErr(err))
	}
	return nil
}

// FitsOneMessage reports whether text renders into a single Chat message rather
// than being split across several by Send. It measures the rendered form,
// because the markup translation can change the length. Implements
// chat.TextFitter.
func (a *Adapter) FitsOneMessage(text string) bool {
	return len(toChatText(text)) <= chatTextLimit
}

// isCardRejection reports whether Chat refused the request because of the
// payload itself — the case worth retrying as plain text. A 400 from
// spaces.messages with a card attached is Chat saying the card is malformed,
// and a 413 is it saying the message is too big for one post, which is exactly
// what the text path splits. Every other status (auth, missing space, rate
// limit) means the text would fail too.
//
// answerCard keeps a card under Chat's 32,000-byte message ceiling itself
// (maxCardBytes), so 413 should be unreachable; it is here because the cost of
// being wrong about that is a dropped reply rather than a wasted write.
func isCardRejection(err error) bool {
	var ae *googleapi.Error
	if !errors.As(err, &ae) {
		return false
	}
	return ae.Code == http.StatusBadRequest || ae.Code == http.StatusRequestEntityTooLarge
}

// classify maps Chat's HTTP statuses onto the provider-neutral sentinels in
// pkg/chat, so callers outside this package can tell a permanent failure from a
// transient one without learning Chat's vocabulary. Anything unrecognized (429,
// 5xx, a transport failure) stays unclassified and is treated as retryable.
func classify(err error) error {
	var ae *googleapi.Error
	if !errors.As(err, &ae) {
		return nil
	}
	switch ae.Code {
	case http.StatusNotFound, http.StatusGone:
		return chat.ErrNotFound
	case http.StatusForbidden, http.StatusUnauthorized:
		return chat.ErrDenied
	}
	return nil
}

// platformErr wraps a Chat failure with its classification, if it has one. The
// result reads exactly like err (no sentinel noise in the log line) but
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

// restMessenger is the production messenger backed by the Chat REST service.
type restMessenger struct{ svc *chatv1.Service }

func (m restMessenger) create(ctx context.Context, parent string, msg *chatv1.Message) (*chatv1.Message, error) {
	call := m.svc.Spaces.Messages.Create(parent, msg)
	// The reply option only applies when replying into a specific thread; a
	// top-level post (no thread) omits it and starts a new thread.
	if msg.Thread != nil && msg.Thread.Name != "" {
		call = call.MessageReplyOption(messageReplyOption)
	}
	return call.Context(ctx).Do()
}

func (m restMessenger) patch(ctx context.Context, name string, msg *chatv1.Message, mask string) error {
	_, err := m.svc.Spaces.Messages.Patch(name, msg).
		UpdateMask(mask).
		Context(ctx).
		Do()
	return err
}

func (m restMessenger) delete(ctx context.Context, name string) error {
	_, err := m.svc.Spaces.Messages.Delete(name).Context(ctx).Do()
	return err
}
