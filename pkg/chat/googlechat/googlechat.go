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

// Package googlechat implements chat.Adapter over Google Chat. Ingress is
// Pub/Sub — the app is configured to publish events to a topic and switchboard
// pulls them from a subscription, so (like Slack Socket Mode) no public webhook
// is exposed, matching the distroless posture. Egress is the Google Chat REST
// API (spaces.messages create/patch/delete), which lets every long-turn
// progress mode work: the placeholder can be edited in place and retired.
//
// A conversation is keyed on space + thread, so every mention in a thread
// continues the same core-agent session. The caller asserted to the daemon is
// the sender's user resource name (users/NNN); per-caller credential resolution
// and verified identity live in core-agent (W0), not here. All platform
// specifics stay behind chat.Adapter so the router never imports this package.
package googlechat

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"cloud.google.com/go/pubsub"
	chatv1 "google.golang.org/api/chat/v1"
	"google.golang.org/api/option"

	"github.com/go-steer/switchboard/pkg/chat"
)

// messageReplyOption tells Chat to post into the referenced thread, starting a
// new thread only if that one no longer exists — the threading behavior a
// conversation-scoped gateway wants.
const messageReplyOption = "REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD"

// messenger is the subset of the Chat REST surface the adapter's egress needs,
// narrowed to an interface so Send/Update/Delete are testable with a fake and
// the adapter never depends on a live Chat service in unit tests.
type messenger interface {
	create(ctx context.Context, parent string, msg *chatv1.Message) (*chatv1.Message, error)
	patch(ctx context.Context, name, text string) error
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
	// Logf is an optional structured-ish logger; nil discards.
	Logf func(format string, args ...any)
}

// Adapter satisfies chat.Adapter.
var _ chat.Adapter = (*Adapter)(nil)

// Adapter is the Google Chat implementation of chat.Adapter.
type Adapter struct {
	ps   *pubsub.Client
	sub  string
	msg  messenger
	logf func(string, ...any)
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
		ps:   ps,
		sub:  cfg.SubscriptionID,
		msg:  restMessenger{svc: svc},
		logf: logf,
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

// dispatch translates one Pub/Sub message into a turn and hands it to the
// router. It always acks: a message we cannot turn into a turn (a lifecycle
// event, a bot sender, a malformed payload) must not be redelivered forever,
// and a daemon failure is logged rather than retried (a retry would re-inject
// and could duplicate the turn), mirroring the Slack adapter.
func (a *Adapter) dispatch(ctx context.Context, h chat.Handler, m *pubsub.Message) {
	defer m.Ack()
	msg, ok, err := messageFromEvent(m.Data)
	if err != nil {
		a.logf("googlechat: %v", err)
		return
	}
	if !ok {
		return
	}
	if err := h.Handle(ctx, msg); err != nil {
		a.logf("googlechat: handle %s: %v", msg.Conversation, err)
	}
}

// Send posts the reply into its originating space + thread, returning a ref to
// the first posted message (for later Update/Delete). A long reply is split
// across several in-thread posts so no single message exceeds Chat's limit; an
// empty reply posts nothing.
func (a *Adapter) Send(ctx context.Context, r chat.Reply) (chat.MessageRef, error) {
	space, thread, ok := splitConversation(r.Conversation)
	if !ok {
		return chat.MessageRef{}, fmt.Errorf("googlechat: malformed conversation key %q", r.Conversation)
	}
	text := strings.TrimSpace(r.Text)
	if text == "" {
		return chat.MessageRef{}, nil // nothing worth posting
	}
	var first chat.MessageRef
	for _, part := range chunk(text, chatTextLimit) {
		msg := &chatv1.Message{Text: part}
		if thread != "" {
			msg.Thread = &chatv1.Thread{Name: thread}
		}
		created, err := a.msg.create(ctx, space, msg)
		if err != nil {
			return first, fmt.Errorf("googlechat: post to %s: %w", r.Conversation, err)
		}
		if first.ID == "" {
			first = chat.MessageRef{Conversation: r.Conversation, ID: created.Name}
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

// Update replaces a previously posted message's text in place — the mechanism
// behind long-turn status edits. A zero ref no-ops. Google Chat supports
// editing an app's own messages, so this never returns chat.ErrUnsupported.
func (a *Adapter) Update(ctx context.Context, ref chat.MessageRef, r chat.Reply) error {
	if ref.ID == "" {
		return nil
	}
	if err := a.msg.patch(ctx, ref.ID, strings.TrimSpace(r.Text)); err != nil {
		return fmt.Errorf("googlechat: update %s: %w", ref.ID, err)
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
		return fmt.Errorf("googlechat: delete %s: %w", ref.ID, err)
	}
	return nil
}

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

func (m restMessenger) patch(ctx context.Context, name, text string) error {
	_, err := m.svc.Spaces.Messages.Patch(name, &chatv1.Message{Text: text}).
		UpdateMask("text").
		Context(ctx).
		Do()
	return err
}

func (m restMessenger) delete(ctx context.Context, name string) error {
	_, err := m.svc.Spaces.Messages.Delete(name).Context(ctx).Do()
	return err
}
