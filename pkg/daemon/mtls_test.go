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

package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Under MutualTLS the transport authenticates switchboard, so the token is
// not required — and not sent: an empty "Bearer " header would be read by a
// daemon as a credential that failed (docs/agent-identity.md).
func TestMutualTLSSendsNoBearerHeader(t *testing.T) {
	var got []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"app":"core-agent","sessionID":"s1"}`))
	}))
	defer srv.Close()

	c, err := New(Config{BaseURL: srv.URL, MutualTLS: true, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateSession(context.Background(), "u@x.com"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "" {
		t.Errorf("Authorization = %q, want none", got)
	}
}

func TestMutualTLSIsValidated(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg  Config
		want string
	}{
		"no client":  {Config{BaseURL: "https://d", MutualTLS: true}, "needs an HTTPClient"},
		"plain http": {Config{BaseURL: "http://d", MutualTLS: true, HTTPClient: http.DefaultClient}, "https BaseURL"},
		"no token":   {Config{BaseURL: "https://d"}, "BearerToken is required"},
		"trailing /": {Config{BaseURL: "https://d/", MutualTLS: true, HTTPClient: http.DefaultClient}, "must not end"},
	} {
		if err := tc.cfg.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}
