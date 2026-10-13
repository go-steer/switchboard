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

package main

import (
	"crypto"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/purser/authtest"
	"github.com/go-steer/purser/client"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
)

// records parses the audit lines written so far.
func (s *syncBuffer) records(t *testing.T) []auditRecord {
	t.Helper()
	var out []auditRecord
	for _, line := range strings.Split(strings.TrimSpace(s.String()), "\n") {
		if line == "" {
			continue
		}
		var rec auditRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("audit line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// adminFixture is the registry fixture behind an OIDC-protected admin API on
// a Google Chat gateway: admin@example.com and anyone at ops.example.com are
// admins.
type adminFixture struct {
	*registryFixture
	iss   *authtest.Issuer
	srv   *httptest.Server
	audit *syncBuffer
}

func newAdminFixture(t *testing.T) *adminFixture {
	t.Helper()
	f := &adminFixture{registryFixture: newRegistryFixture(t, nil), iss: authtest.NewIssuer(t), audit: &syncBuffer{}}
	s, err := newAdminServer(adminConfig{
		Addr:       "0.0.0.0:0", // public: allowed, because OIDC enforces a credential
		Auth:       adminAuthOIDC,
		Issuer:     f.iss.URL(),
		Audiences:  []string{authtest.DefaultAudience},
		Allow:      []string{"email:admin@example.com", "domain:ops.example.com"},
		Platform:   "googlechat",
		Audit:      &auditLog{w: f.audit, now: time.Now},
		OIDCClient: f.iss.Client(),
	}, f.r, f.admin)
	if err != nil {
		t.Fatal(err)
	}
	f.srv = httptest.NewServer(s.h)
	t.Cleanup(f.srv.Close)
	return f
}

// do sends a request as email ("" sends no credential) and returns the status
// and body.
func (f *adminFixture) do(t *testing.T, email, method, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, f.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if email != "" {
		req.Header.Set("Authorization", "Bearer "+f.iss.Mint(t, authtest.TokenOptions{Subject: "s-" + email, Email: email}))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

const admin = "admin@example.com"

// Every request needs an authenticated admin, reads included: with an issuer
// like Google, merely authenticated is anyone with an account.
func TestTheAdminAPIAdmitsOnlyAdmins(t *testing.T) {
	f := newAdminFixture(t)
	for who, want := range map[string]int{
		"":                       http.StatusUnauthorized,
		"stranger@gmail.com":     http.StatusForbidden,
		admin:                    http.StatusOK,
		"oncall@ops.example.com": http.StatusOK,
	} {
		if got, body := f.do(t, who, "GET", "/v1/agents", ""); got != want {
			t.Errorf("%q: status %d, want %d (%s)", who, got, want, body)
		}
	}
}

// An agent added through the API is offered at once, and the change is
// audited with who made it.
func TestAnAgentAddedThroughTheAPIIsOfferedAndAudited(t *testing.T) {
	f := newAdminFixture(t)
	body := `{"display_name":"Agent C","daemon_url":"` + f.c.url + `","token_env":"TOK_REG"}`
	if got, resp := f.do(t, admin, "PUT", "/v1/agents/c", body); got != http.StatusOK {
		t.Fatalf("PUT: %d %s", got, resp)
	}
	if got := strings.Join(choiceNames(f.r.AgentChoices("spaces/AAA")), ","); got != "a*,b,c" {
		t.Errorf("choices = %s, want c offered", got)
	}
	_, list := f.do(t, admin, "GET", "/v1/agents", "")
	var v agentsView
	if err := json.Unmarshal([]byte(list), &v); err != nil {
		t.Fatal(err)
	}
	var c *agentView
	for i := range v.Agents {
		if v.Agents[i].Name == "c" {
			c = &v.Agents[i]
		}
	}
	if c == nil || !c.Active || c.Pinned || v.Agents[0].Name != "a" || !v.Agents[0].Pinned {
		t.Errorf("listing = %+v, want a pinned and c active", v.Agents)
	}
	recs := f.audit.records(t)
	if len(recs) != 1 || recs[0].Kind != auditAdmin || recs[0].Caller != admin || recs[0].Outcome != auditApplied ||
		recs[0].Admin == nil || recs[0].Admin.Change != "put agent c" || !strings.Contains(string(recs[0].Admin.After), "Agent C") {
		t.Errorf("audit = %+v", recs)
	}
}

// Registry errors map onto statuses, and a refusal is audited as one.
func TestAdminErrorsMapToStatuses(t *testing.T) {
	f := newAdminFixture(t)
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{"PUT", "/v1/agents/a", `{"daemon_url":"http://x","token_env":"TOK_REG"}`, http.StatusConflict}, // pinned
		{"DELETE", "/v1/agents/nope", "", http.StatusNotFound},
		{"PUT", "/v1/channels/spaces/AAA", `{"default_agent":"nope"}`, http.StatusUnprocessableEntity},
		{"PUT", "/v1/agents/c", `{"name":"d","daemon_url":"http://x","token_env":"TOK_REG"}`, http.StatusBadRequest},
		{"PUT", "/v1/agents/c", `{"daemon_url":"http://x","token":"secret"}`, http.StatusBadRequest}, // unknown field
		{"GET", "/v1/channels/C1", "", http.StatusBadRequest},                                        // not a Chat space
		{"PUT", "/v1/agents/Bad_Name", `{"daemon_url":"http://x","token_env":"TOK_REG"}`, http.StatusUnprocessableEntity},
	} {
		if got, body := f.do(t, admin, tc.method, tc.path, tc.body); got != tc.want {
			t.Errorf("%s %s: %d, want %d (%s)", tc.method, tc.path, got, tc.want, body)
		}
	}
	var refused int
	for _, r := range f.audit.records(t) {
		if r.Outcome == auditRefused && r.Admin.Reason != "" {
			refused++
		}
	}
	if refused != 4 {
		t.Errorf("refused records = %d, want one per refused change (4)", refused)
	}
}

// A Chat space ID has a slash in it; the route takes the rest of the path.
func TestAChannelIsAddressedByItsWholeID(t *testing.T) {
	f := newAdminFixture(t)
	if got, body := f.do(t, admin, "PUT", "/v1/channels/spaces/AAA", `{"default_agent":"b","agents":[" b "]}`); got != http.StatusOK {
		t.Fatalf("PUT: %d %s", got, body)
	}
	_, body := f.do(t, admin, "GET", "/v1/channels/spaces/AAA", "")
	var v channelView
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	if v.Channel != "spaces/AAA" || v.DefaultAgent != "b" || strings.Join(v.Agents, ",") != "b" || v.Runtime == nil {
		t.Errorf("view = %+v", v)
	}
	if got, _ := f.do(t, admin, "DELETE", "/v1/channels/spaces/AAA", ""); got != http.StatusNoContent {
		t.Errorf("DELETE: %d", got)
	}
}

func TestTheDefaultCanBeMovedUnlessPinned(t *testing.T) {
	f := newAdminFixture(t)
	if got, body := f.do(t, admin, "PUT", "/v1/defaults", `{"default_agent":"b"}`); got != http.StatusOK || !strings.Contains(body, `"default_agent":"b"`) {
		t.Fatalf("PUT: %d %s", got, body)
	}
	f.admin.pinnedDefault = true
	if got, _ := f.do(t, admin, "PUT", "/v1/defaults", `{"default_agent":"a"}`); got != http.StatusConflict {
		t.Errorf("pinned: %d, want 409", got)
	}
}

// Settings that would leave the API unprotected, or nobody able to use it,
// stop startup.
func TestUnsafeAdminSettingsAreRefused(t *testing.T) {
	f := newRegistryFixture(t, nil)
	for name, c := range map[string]adminConfig{
		"token on a public address": {Addr: "0.0.0.0:9000", Auth: adminAuthToken, Token: "t"},
		"token without a token":     {Addr: "127.0.0.1:9000", Auth: adminAuthToken},
		"oidc without admins":       {Addr: "127.0.0.1:9000", Auth: adminAuthOIDC, Issuer: "https://issuer.example", Audiences: []string{"x"}},
		"oidc without an issuer":    {Addr: "127.0.0.1:9000", Auth: adminAuthOIDC, Allow: []string{"email:a@b.c"}},
		"mtls without an identity":  {Addr: "127.0.0.1:9000", Auth: adminAuthMTLS, Allow: []string{"email:a@b.c"}},
		"a malformed grant":         {Addr: "127.0.0.1:9000", Auth: adminAuthOIDC, Issuer: "https://issuer.example", Audiences: []string{"x"}, Allow: []string{"alice"}},
		"an unknown method":         {Addr: "127.0.0.1:9000", Auth: "basic"},
	} {
		if _, err := newAdminServer(c, f.r, f.admin); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := newAdminServer(adminConfig{Addr: "127.0.0.1:9000", Auth: adminAuthOIDC}, f.r, nil); err == nil {
		t.Error("an outbound-only run (no registry) was accepted")
	}
}

// On loopback, a static token is the admin: for a laptop.
func TestTheTokenModeOnLoopback(t *testing.T) {
	f := newRegistryFixture(t, nil)
	s, err := newAdminServer(adminConfig{Addr: "127.0.0.1:0", Auth: adminAuthToken, Token: "sesame"}, f.r, f.admin)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.h)
	defer srv.Close()
	for tok, want := range map[string]int{"": http.StatusUnauthorized, "wrong": http.StatusUnauthorized, "sesame": http.StatusOK} {
		req, _ := http.NewRequest("GET", srv.URL+"/v1/defaults", nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("token %q: %d, want %d", tok, resp.StatusCode, want)
		}
	}
}

// In mtls mode the listener presents the gateway's own SVID and admits a
// caller whose SVID an --admin-allow id: rule names.
func TestTheMTLSModeAdmitsAnAllowedWorkload(t *testing.T) {
	ca := authtest.NewCA(t, "trust")
	f := newRegistryFixture(t, nil)
	const automation = "spiffe://example.org/ns/ops/sa/automation"
	s, err := newAdminServer(adminConfig{
		Addr: "0.0.0.0:0", Auth: adminAuthMTLS, Allow: []string{"id:" + automation},
		Identity: spiffeIdentity(t, ca), Platform: "slack",
	}, f.r, f.admin)
	if err != nil {
		t.Fatal(err)
	}
	url := serveTLS(t, s.h, s.tls)

	get := func(id string) int {
		t.Helper()
		cert := svidCert(t, ca, id)
		sid, _ := spiffeid.FromString(id)
		td, _ := spiffeid.TrustDomainFromString(testTD)
		gw, _ := spiffeid.FromString(testClientID) // the gateway's own SVID serves the listener
		cfg, err := client.NewSPIFFE(client.SPIFFEOptions{
			SVIDSource:   staticSVID{&x509svid.SVID{ID: sid, Certificates: []*x509.Certificate{cert.Leaf}, PrivateKey: cert.PrivateKey.(crypto.Signer)}},
			BundleSource: x509bundle.FromX509Authorities(td, []*x509.Certificate{ca.Cert()}),
			Authorize:    spiffeid.MatchID(gw),
		})
		if err != nil {
			t.Fatal(err)
		}
		resp, err := (&http.Client{Transport: client.Transport(cfg)}).Get(url + "/v1/agents")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := get(automation); got != http.StatusOK {
		t.Errorf("the allowed workload: %d", got)
	}
	if got := get("spiffe://example.org/ns/ops/sa/other"); got != http.StatusForbidden {
		t.Errorf("another workload: %d, want 403", got)
	}
}
