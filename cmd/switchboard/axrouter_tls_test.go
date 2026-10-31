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
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/go-steer/purser/authtest"

	"github.com/go-steer/switchboard/pkg/ax"
)

// Reaching the router over mTLS (#181): Agent Substrate v1.0's router serves
// TLS only and admits a pod certificate from its podidentity signer.

const gatewaySPIFFEID = "spiffe://cluster.local/ns/switchboard/sa/switchboard"

// routerFiles writes the two projected volumes a pod gets — its podidentity
// credential bundle and the servicedns trust bundle — and returns their paths
// and the CAs behind them.
func routerFiles(t *testing.T) (bundle, trust string, podidentity, servicedns *authtest.CA) {
	t.Helper()
	podidentity, servicedns = authtest.NewCA(t, "podidentity"), authtest.NewCA(t, "servicedns")
	dir := t.TempDir()
	bundle, trust = filepath.Join(dir, "credential-bundle.pem"), filepath.Join(dir, "trust-bundle.pem")
	cert := podidentity.Issue(t, authtest.LeafOptions{URISANs: []string{gatewaySPIFFEID}})
	der, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	out := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	for _, c := range cert.Certificate {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c})...)
	}
	if err := os.WriteFile(bundle, out, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trust, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: servicedns.Cert().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return bundle, trust, podidentity, servicedns
}

// newMTLSAXFixture is newAXFixture behind a router that takes mTLS only.
// ids records the client identity of every request.
func newMTLSAXFixture(t *testing.T) (f *axFixture, ids func() []string) {
	t.Helper()
	f = newAXFixture(t)
	bundle, trust, podidentity, servicedns := routerFiles(t)
	var mu sync.Mutex
	var seen []string
	h := f.router.handler()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if cs := req.TLS; cs != nil && len(cs.PeerCertificates) > 0 && len(cs.PeerCertificates[0].URIs) > 0 {
			mu.Lock()
			seen = append(seen, cs.PeerCertificates[0].URIs[0].String())
			mu.Unlock()
		}
		h.ServeHTTP(w, req)
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			f.router.conns.Add(1)
		}
	}
	srv.EnableHTTP2 = true
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{servicedns.IssueServer(t, ax.DefaultRouterServerName)},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    podidentity.Pool(),
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	f.router.srv = srv

	axc := *f.cfg.AX
	axc.Router = srv.URL // an IP: the server name is what is verified
	axc.RouterCredentialBundleFile, axc.RouterTrustBundleFile = bundle, trust
	f.cfg.AX = &axc
	set, err := buildAgents([]AgentConfig{f.cfg}, "triage", agentBuilder{ax: &axClients{}})
	if err != nil {
		t.Fatal(err)
	}
	f.r.setAgents(set)
	return f, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// A thread starts its task and talks to it through an mTLS router: every
// request carries the pod's certificate, the actor header and the bearer.
func TestAThreadReachesItsTaskOverMTLS(t *testing.T) {
	f, ids := newMTLSAXFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	f.say(t, ctx, "C1:1", "hello")
	waitFor(t, func() bool { return len(f.router.callsMatching("/inject")) == 1 }, "the message injected into the task")

	task := ax.TaskName("sb-triage", "slack", "C1:1")
	for _, c := range []string{"GET /readyz", "POST /sessions", "/inject"} {
		if got := f.router.callsMatching(c); len(got) == 0 || !strings.HasPrefix(got[0], "agents/"+task+" ") {
			t.Errorf("%s: router saw %v, want it for agents/%s", c, got, task)
		}
	}
	got := ids()
	if len(got) == 0 {
		t.Fatal("the router saw no client certificate")
	}
	for _, id := range got {
		if id != gatewaySPIFFEID {
			t.Errorf("client identity %q, want %s", id, gatewaySPIFFEID)
		}
	}
	f.router.mu.Lock()
	defer f.router.mu.Unlock()
	for _, b := range f.router.bearer {
		if b != "Bearer t0ken" {
			t.Errorf("bearer %q", b)
		}
	}
}

func TestAXRouterTLSIsValidated(t *testing.T) {
	f := newAXFixture(t)
	bundle, trust, _, _ := routerFiles(t)
	for name, tc := range map[string]struct {
		mutate func(*AXConfig)
		want   string
	}{
		"router files with an http router":  {func(c *AXConfig) { c.RouterTrustBundleFile = trust }, "are for an https router"},
		"a server name with an http router": {func(c *AXConfig) { c.RouterServerName = "atenet-router.ate-system.svc" }, "are for an https router"},
		"no credential bundle": {func(c *AXConfig) {
			c.Router, c.RouterCredentialBundleFile, c.RouterTrustBundleFile = "https://atenet-router.ate-system.svc", filepath.Join(t.TempDir(), "nope.pem"), trust
		}, "nope.pem"},
		"no trust bundle": {func(c *AXConfig) {
			c.Router, c.RouterCredentialBundleFile, c.RouterTrustBundleFile = "https://atenet-router.ate-system.svc", bundle, filepath.Join(t.TempDir(), "nope.pem")
		}, "nope.pem"},
	} {
		c := f.cfg
		axc := *c.AX
		tc.mutate(&axc)
		c.AX = &axc
		if _, err := buildAgent(c, agentBuilder{ax: &axClients{}}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

// An agent that names no router gets the in-cluster https one, and with it
// the pod-certificate files at their default paths.
func TestAnAXAgentDefaultsToTheHTTPSRouter(t *testing.T) {
	if _, err := os.Stat(ax.DefaultCredentialBundleFile); err == nil {
		t.Skipf("%s exists here; the test wants it absent", ax.DefaultCredentialBundleFile)
	}
	f := newAXFixture(t)
	c := f.cfg
	axc := *c.AX
	axc.Router = ""
	c.AX = &axc
	_, err := buildAgent(c, agentBuilder{ax: &axClients{}})
	if err == nil || !strings.Contains(err.Error(), ax.DefaultRouter) || !strings.Contains(err.Error(), ax.DefaultCredentialBundleFile) {
		t.Errorf("err = %v, want the default router and its default credential bundle", err)
	}
}

// Agents on one router share its client: one pair of file watchers, and one
// pool of connections, so two agents' requests ride one TLS connection.
func TestAXAgentsShareTheRouterClient(t *testing.T) {
	f, _ := newMTLSAXFixture(t)
	b := agentBuilder{ax: &axClients{}}
	for _, name := range []string{"triage", "deploy"} {
		c := f.cfg
		c.Name = name
		axc := *c.AX
		axc.Atespace = name // a different ax client
		c.AX = &axc
		a, err := buildAgent(c, b)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := a.ax.client.HTTPClient("t").Get(f.router.srv.URL + "/readyz")
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if len(b.ax.routers) != 1 {
		t.Errorf("router clients = %d, want 1", len(b.ax.routers))
	}
	if len(b.ax.clients) != 2 {
		t.Errorf("ax clients = %d, want 2", len(b.ax.clients))
	}
	if n := f.router.conns.Load(); n != 1 {
		t.Errorf("TLS connections = %d, want both agents on one", n)
	}
}

// A runtime agent's router files are fenced like its template: the gateway
// reads them, so an admin may not aim them anywhere but the default mounts.
func TestARuntimeAXAgentsRouterFilesAreFenced(t *testing.T) {
	f := newAXFixture(t)
	base, err := buildAgents([]AgentConfig{{Name: "a", DaemonURL: "http://a:1", TokenEnv: "SWITCHBOARD_AGENT_TOKEN_TRIAGE"}}, "a", agentBuilder{ax: &axClients{}})
	if err != nil {
		t.Fatal(err)
	}
	r := NewRouter(nil, f.out, ProgressOff, nil, nil)
	r.setAgents(base)
	admin := newRegistryAdmin(r, registryConfig{Base: base, TokenPrefix: "SWITCHBOARD_AGENT_TOKEN_", Builder: agentBuilder{ax: &axClients{}},
		AXTemplateDir: filepath.Dir(f.cfg.AX.TaskTemplateFile)})
	for name, mutate := range map[string]func(*AXConfig){
		"a credential bundle elsewhere": func(c *AXConfig) {
			c.RouterCredentialBundleFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
		},
		"a trust bundle elsewhere": func(c *AXConfig) { c.RouterTrustBundleFile = "/etc/switchboard/secret.pem" },
		"another server name":      func(c *AXConfig) { c.RouterServerName = "mast-web.agents.svc" },
	} {
		c := f.cfg
		axc := *c.AX
		axc.Router = "https://atenet-router.ate-system.svc"
		mutate(&axc)
		c.AX = &axc
		if err := admin.putAgent(c); !errors.Is(err, errRejected) || !strings.Contains(err.Error(), "a runtime agent uses the default") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// The defaults pass the fence (and fail later, on the files missing here).
	c := f.cfg
	axc := *c.AX
	axc.Router = "https://atenet-router.ate-system.svc"
	axc.RouterCredentialBundleFile, axc.RouterTrustBundleFile = ax.DefaultCredentialBundleFile, ax.DefaultTrustBundleFile
	axc.RouterServerName = ax.DefaultRouterServerName
	c.AX = &axc
	if err := admin.putAgent(c); err != nil && strings.Contains(err.Error(), "a runtime agent uses the default") {
		t.Errorf("the default paths were fenced off: %v", err)
	}
}
