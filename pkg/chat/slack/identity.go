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
	"errors"

	"github.com/slack-go/slack"

	"github.com/go-steer/switchboard/pkg/chat"
)

// Agent attribution (#140, phase 5): with several agents behind one app, an
// agent's reply is posted under the agent's name, and its icon when it has
// one, so a thread shows who answered. That is chat.postMessage's username and
// icon_url, which need the chat:write.customize scope. An app without it is
// told once, in the log, and goes on posting as itself: a missing scope must
// never cost a reply.

// postAs posts a message as the reply's agent where it names one.
func (a *Adapter) postAs(ctx context.Context, channel string, ag *chat.AgentIdentity, opts ...slack.MsgOption) (string, error) {
	ident := a.identityOpts(ag)
	if len(ident) > 0 {
		_, ts, err := a.api.PostMessageContext(ctx, channel, append(opts, ident...)...)
		if !isMissingScope(err) {
			return ts, err
		}
		if a.noCustomize.CompareAndSwap(false, true) {
			a.logf.Warnf("slack: posting as each agent needs the chat:write.customize scope (%v); replies go out as the app", err)
		}
	}
	_, ts, err := a.api.PostMessageContext(ctx, channel, opts...)
	return ts, err
}

// identityOpts are the options that post a message as ag, none when there is
// no agent to name or the app cannot.
func (a *Adapter) identityOpts(ag *chat.AgentIdentity) []slack.MsgOption {
	if ag == nil || ag.Label == "" || a.noCustomize.Load() {
		return nil
	}
	opts := []slack.MsgOption{slack.MsgOptionUsername(clampRunes(ag.Label, maxUsername))}
	if ag.IconURL != "" {
		opts = append(opts, slack.MsgOptionIconURL(ag.IconURL))
	}
	return opts
}

// maxUsername keeps a display name to what Slack shows in full.
const maxUsername = 80

func isMissingScope(err error) bool {
	var se slack.SlackErrorResponse
	return errors.As(err, &se) && se.Err == "missing_scope"
}
