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

import "testing"

func TestRedactCredentials(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{
			"legacy verification token",
			`{"type":"MESSAGE","token":"legacy-secret","space":{}}`,
			`{"type":"MESSAGE","token":"REDACTED","space":{}}`,
		},
		{
			"escaped quote inside a value",
			`{"userOAuthToken":"ya29.ab\"cd-SECRET"}`,
			`{"userOAuthToken":"REDACTED"}`,
		},
		{
			"plain = in the redirect",
			`{"configCompleteRedirectUri":"https://chat.google.com/api/bot_config_complete?token=SEC=RET"}`,
			`{"configCompleteRedirectUri":"https://chat.google.com/api/bot_config_complete?token=REDACTED"}`,
		},
		{
			"token not first in the legacy redirect",
			`{"configCompleteRedirectUrl":"https://chat.google.com/api/bot_config_complete?a=1&token=SECRET&b=2"}`,
			`{"configCompleteRedirectUrl":"https://chat.google.com/api/bot_config_complete?a=1&token=REDACTED&b=2"}`,
		},
		{
			"text that only mentions a token",
			`{"text":"my \"token\":\"x\" here"}`,
			`{"text":"my \"token\":\"x\" here"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactCredentials(tc.in); got != tc.want {
				t.Errorf("redactCredentials(%s)\n got %s\nwant %s", tc.in, got, tc.want)
			}
		})
	}
}
