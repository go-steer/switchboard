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

package slack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/slack-go/slack"
)

// Keyed replies (chat.Reply.Key).
//
// chat.postMessage takes no idempotency key, so Slack cannot drop a repeated
// post the way Google Chat drops a repeated request ID. What it can do is
// carry the key in message metadata — invisible in the client, returned by
// conversations.replies — so that when the router is unsure whether a reply
// already went out (Reply.Verify: posted by a process that then died), the
// thread can be asked. Only then: the lookup is an API call, and every other
// post skips it.
//
// Best effort, deliberately. The lookup needs a history scope the minimal app
// does not have; without it, or on any other failure, the reply is posted and
// the worst case is the duplicate that would have happened anyway. A message
// Slack accepted but is not yet returning is the remaining window.

// metaEventType names switchboard's metadata on a message.
const metaEventType = "switchboard_reply"

// metaKey is the payload field carrying the key.
const metaKey = "key"

// maxVerifyPages bounds the thread read. conversations.replies returns a
// thread oldest first, so the cap reads the oldest pages; a reply in a thread
// longer than this is not found and is posted again. Slack also rate-limits
// this method, and tightly for apps outside the Marketplace — a page refused
// for that ends the read with what earlier pages found.
const maxVerifyPages = 5

// partKey is the key for one message of a reply: the reply's own key for a
// reply posted as one message, and a numbered part for each message of a
// chunked one, so verification can post only the parts that are missing.
func partKey(key string, part int) string {
	if part < 0 {
		return key
	}
	return fmt.Sprintf("%s/part-%d", key, part)
}

// metadataOpt attaches a part's key, or nothing for an unkeyed reply.
func metadataOpt(key string) []slack.MsgOption {
	if key == "" {
		return nil
	}
	return []slack.MsgOption{slack.MsgOptionMetadata(slack.SlackMetadata{
		EventType:    metaEventType,
		EventPayload: map[string]any{metaKey: metaValue(key)},
	})}
}

// metaValue is what a part key is stored as: hashed, because metadata is
// readable by any app in the workspace with a history scope, and a session
// reference is nobody else's business. The same hash Google Chat's request
// IDs get, for the same reason.
func metaValue(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "switchboard-" + hex.EncodeToString(sum[:16])
}

// postedKeys returns which of a reply's part keys are already present in a
// thread, with the timestamp of the message carrying each, keyed by part key.
// A nil map means not even the first page could be read and nothing is known:
// the caller posts as if verification had not been asked for. A later page
// that fails ends the read with what was found so far.
func (a *Adapter) postedKeys(ctx context.Context, channel, thread, key string, parts int) map[string]string {
	if thread == "" {
		return nil // a reply that roots its own thread has nothing to look in
	}
	// The hashes this reply's parts would carry, back to the part keys.
	want := map[string]string{metaValue(partKey(key, -1)): partKey(key, -1)}
	for i := 0; i < parts; i++ {
		want[metaValue(partKey(key, i))] = partKey(key, i)
	}
	found := map[string]string{}
	params := &slack.GetConversationRepliesParameters{
		ChannelID:          channel,
		Timestamp:          thread,
		Limit:              200,
		IncludeAllMetadata: true,
	}
	for page := 0; page < maxVerifyPages; page++ {
		msgs, hasMore, cursor, err := a.api.GetConversationRepliesContext(ctx, params)
		if err != nil {
			a.warnVerify(err)
			if page == 0 {
				return nil
			}
			return found
		}
		for _, m := range msgs {
			if m.Metadata.EventType != metaEventType {
				continue
			}
			v, _ := m.Metadata.EventPayload[metaKey].(string)
			if k, ok := want[v]; ok {
				found[k] = m.Timestamp
			}
		}
		if !hasMore || cursor == "" {
			break
		}
		params.Cursor = cursor
	}
	return found
}

// warnVerify reports a failed lookup, naming the scope once when that is the
// reason: it is the one cause an operator can fix, and saying it on every
// verified reply would bury it.
func (a *Adapter) warnVerify(err error) {
	if strings.Contains(err.Error(), "missing_scope") {
		a.scopeOnce.Do(func() {
			a.logf.Warnf("slack: cannot check a thread for a reply that may already be posted " +
				"(add channels:history, groups:history, im:history and mpim:history to verify); " +
				"posting, which can duplicate a message after a crash")
		})
		return
	}
	a.logf.Warnf("slack: checking a thread for an already-posted reply: %v; posting anyway", err)
}
