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

package googlechat

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/switchboard/pkg/chat"
)

// The secrets planted below stand in for live values: an HTTP add-on event
// carries the sender's OAuth access token and two ID tokens, and the first live
// capture wrote all three into the log verbatim.
const (
	fakeOAuth    = "ya29.FAKE-oauth-access-token"
	fakeUserID   = "eyJFAKE.user-id-token.sig"
	fakeSystemID = "eyJFAKE.system-id-token.sig"
	fakeConfig   = "ADpxFAKEconfigtoken%3D%3D"
)

func loggedEvent(t *testing.T, payload string) string {
	t.Helper()
	var lines []string
	a := &Adapter{
		msg:       &fakeMessenger{},
		cards:     CardsOff,
		logEvents: true,
		logf: func(_ chat.Level, format string, args ...any) {
			lines = append(lines, fmt.Sprintf(format, args...))
		},
	}
	a.handleEvent(context.Background(), &choiceHandler{}, []byte(payload))
	for _, l := range lines {
		if strings.HasPrefix(l, "googlechat: event ") {
			return l
		}
	}
	t.Fatalf("no event line logged; got %q", lines)
	return ""
}

func TestLogEventsRedactsCredentials(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "events", "addon-live-http-slash-command.json"))
	if err != nil {
		t.Fatal(err)
	}
	payload := strings.NewReplacer(
		`"userOAuthToken": "REDACTED"`, `"userOAuthToken": "`+fakeOAuth+`"`,
		`"userIdToken": "REDACTED"`, `"userIdToken": "`+fakeUserID+`"`,
		`"systemIdToken": "REDACTED"`, `"systemIdToken": "`+fakeSystemID+`"`,
		`token\u003dREDACTED`, `token\u003d`+fakeConfig,
	).Replace(string(b))
	for _, s := range []string{fakeOAuth, fakeUserID, fakeSystemID, fakeConfig} {
		if !strings.Contains(payload, s) {
			t.Fatalf("fixture shape changed; %q was not planted", s)
		}
	}

	got := loggedEvent(t, payload)
	for _, s := range []string{fakeOAuth, fakeUserID, fakeSystemID, fakeConfig} {
		if strings.Contains(got, s) {
			t.Errorf("event log carries a credential %q: %s", s, got)
		}
	}
	for _, want := range []string{
		`"userOAuthToken":"REDACTED"`,
		`"systemIdToken":"REDACTED"`,
		`token\u003dREDACTED"`,
		// The rest is as Chat sent it, number spelling included, so the line
		// can still be dropped into testdata/events as a fixture.
		`"offset":-3.6E6`,
		`"commandId":1.0`,
		`"displayName":"Ada Lovelace"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("event log missing %s: %s", want, got)
		}
	}
}

// A payload that fails to parse is logged verbatim, because it is exactly the
// one worth seeing — so the redaction cannot depend on parsing it.
func TestLogEventsRedactsUnparseable(t *testing.T) {
	got := loggedEvent(t, `{"authorizationEventObject":{"userOAuthToken":"`+fakeOAuth+`"}, "token": "legacy-secret", oops`)
	if strings.Contains(got, fakeOAuth) || strings.Contains(got, "legacy-secret") {
		t.Errorf("unparseable event logged with its credentials: %s", got)
	}
	if !strings.Contains(got, "oops") {
		t.Errorf("unparseable event no longer logged verbatim: %s", got)
	}
}
