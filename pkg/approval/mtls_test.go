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

package approval

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/go-steer/switchboard/pkg/daemon"
)

// Under MutualTLS the certificate is the credential, so a press goes out
// with no Authorization header at all (docs/agent-identity.md).
func TestAPressUnderMutualTLSSendsNoBearerHeader(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"p1","decision":"allow-once"}`))
	}))
	defer srv.Close()
	c, err := New(daemon.Config{BaseURL: srv.URL, MutualTLS: true, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Respond(context.Background(), testSession, "u@x.com", "p1", AllowOnce)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "" {
		t.Errorf("Authorization = %q, want none", got)
	}
}
