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
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/purser/authn/mtls"
	"github.com/go-steer/purser/authtest"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
)

const (
	testTD       = "example.org"
	testServerID = "spiffe://example.org/ns/agents/sa/core-agent"
	testOtherID  = "spiffe://example.org/ns/agents/sa/impostor"
	testClientID = "spiffe://example.org/ns/switchboard/sa/switchboard"
)

// svidCert issues a certificate carrying a SPIFFE ID, usable at both ends.
func svidCert(t *testing.T, ca *authtest.CA, id string) tls.Certificate {
	t.Helper()
	return ca.Issue(t, authtest.LeafOptions{
		URISANs:     []string{id},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	})
}

func writePEM(t *testing.T, path, kind string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeKeyPair writes a certificate and its key as PEM files.
func writeKeyPair(t *testing.T, cert tls.Certificate, certPath, keyPath string) {
	t.Helper()
	writePEM(t, certPath, "CERTIFICATE", cert.Leaf.Raw)
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	writePEM(t, keyPath, "PRIVATE KEY", key)
}

// spiffeDir lays out an SVID as GKE managed workload identity delivers it.
func spiffeDir(t *testing.T, ca *authtest.CA, id string) string {
	t.Helper()
	dir := t.TempDir()
	writeKeyPair(t, svidCert(t, ca, id), filepath.Join(dir, mtls.GKECertFile), filepath.Join(dir, mtls.GKEKeyFile))
	writePEM(t, filepath.Join(dir, mtls.GKEBundleFile), "CERTIFICATE", ca.Cert().Raw)
	return dir
}

type staticSVID struct{ svid *x509svid.SVID }

func (s staticSVID) GetX509SVID() (*x509svid.SVID, error) { return s.svid, nil }

// peerSeen records what a test daemon saw of its callers.
type peerSeen struct {
	mu     sync.Mutex
	uris   []string
	cns    []string
	bearer []string
}

func (p *peerSeen) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			leaf := r.TLS.PeerCertificates[0]
			for _, u := range leaf.URIs {
				p.uris = append(p.uris, u.String())
			}
			p.cns = append(p.cns, leaf.Subject.CommonName)
		}
		p.bearer = append(p.bearer, r.Header.Get("Authorization"))
		p.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"app":"core-agent","sessionID":"s1"}`)
	})
}

// spiffeDaemon serves the contract over the SPIFFE mTLS profile, as core-agent
// will once it adopts purser, presenting serverID.
func spiffeDaemon(t *testing.T, ca *authtest.CA, serverID string) (string, *peerSeen) {
	t.Helper()
	cert := svidCert(t, ca, serverID)
	id, err := spiffeid.FromString(serverID)
	if err != nil {
		t.Fatal(err)
	}
	td, _ := spiffeid.TrustDomainFromString(testTD)
	cfg, _, err := mtls.NewSPIFFE(mtls.SPIFFEOptions{
		SVIDSource:   staticSVID{&x509svid.SVID{ID: id, Certificates: []*x509.Certificate{cert.Leaf}, PrivateKey: cert.PrivateKey.(crypto.Signer)}},
		BundleSource: x509bundle.FromX509Authorities(td, []*x509.Certificate{ca.Cert()}),
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := &peerSeen{}
	return serveTLS(t, seen.handler(), cfg), seen
}

// serveTLS serves h over cfg as given. Not httptest's StartTLS: that puts its
// own certificate in Certificates, which a client sending no SNI (dialing an
// IP) is shown instead of the GetCertificate an SVID server answers with.
func serveTLS(t *testing.T, h http.Handler, cfg *tls.Config) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h, TLSConfig: cfg, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })
	return "https://" + ln.Addr().String()
}

func spiffeIdentity(t *testing.T, ca *authtest.CA) *gatewayIdentity {
	t.Helper()
	gid, err := loadIdentity(context.Background(), identityConfig{
		Kind: "spiffe-files", SPIFFEDir: spiffeDir(t, ca, testClientID), SPIFFETrustDomain: testTD,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gid.close)
	return gid
}

// An agent with auth.method spiffe is reached with the gateway's SVID, and no
// bearer token goes with it.
func TestASPIFFEAgentIsReachedAsTheGateway(t *testing.T) {
	ca := authtest.NewCA(t, "trust")
	url, seen := spiffeDaemon(t, ca, testServerID)
	a, err := buildAgent(AgentConfig{
		Name: "platform", DaemonURL: url,
		Auth: &AgentAuth{Method: authSPIFFE, ServerID: testServerID},
	}, agentBuilder{approvals: true, identity: spiffeIdentity(t, ca)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.daemon.CreateSession(context.Background(), "u@x.com"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	seen.mu.Lock()
	defer seen.mu.Unlock()
	if len(seen.uris) != 1 || seen.uris[0] != testClientID {
		t.Errorf("the daemon saw %v, want the gateway's SPIFFE ID", seen.uris)
	}
	if seen.bearer[0] != "" {
		t.Errorf("a bearer token went too: %q", seen.bearer[0])
	}
	if a.approvals == nil {
		t.Error("no approval client built")
	}
}

// A daemon presenting any other identity — another workload of the same trust
// domain that won the address — is refused before a request is sent.
func TestASPIFFEAgentPresentingTheWrongIdentityIsRefused(t *testing.T) {
	ca := authtest.NewCA(t, "trust")
	url, seen := spiffeDaemon(t, ca, testOtherID)
	a, err := buildAgent(AgentConfig{
		Name: "platform", DaemonURL: url,
		Auth: &AgentAuth{Method: authSPIFFE, ServerID: testServerID},
	}, agentBuilder{identity: spiffeIdentity(t, ca)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.daemon.CreateSession(context.Background(), "u@x.com"); err == nil {
		t.Fatal("reached a daemon presenting the wrong SPIFFE ID")
	}
	seen.mu.Lock()
	defer seen.mu.Unlock()
	if len(seen.bearer) != 0 {
		t.Errorf("the impostor received %d request(s)", len(seen.bearer))
	}
}

// pkiFiles writes a client key pair and a CA bundle for --identity pki.
func pkiFiles(t *testing.T, ca *authtest.CA, cn string) identityConfig {
	t.Helper()
	dir := t.TempDir()
	c := identityConfig{Kind: "pki", CertFile: filepath.Join(dir, "tls.crt"), KeyFile: filepath.Join(dir, "tls.key"), CAFile: filepath.Join(dir, "ca.crt")}
	writeKeyPair(t, ca.IssueClient(t, cn), c.CertFile, c.KeyFile)
	writePEM(t, c.CAFile, "CERTIFICATE", ca.Cert().Raw)
	return c
}

func TestAPKIAgentIsReachedWithTheGatewaysCertificate(t *testing.T) {
	ca := authtest.NewCA(t, "trust")
	seen := &peerSeen{}
	srv := httptest.NewUnstartedServer(seen.handler())
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{ca.IssueServer(t, "127.0.0.1")},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    ca.Pool(),
		MinVersion:   tls.VersionTLS13,
	}
	srv.StartTLS()
	defer srv.Close()

	gid, err := loadIdentity(context.Background(), pkiFiles(t, ca, "switchboard"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := buildAgent(AgentConfig{Name: "general", DaemonURL: srv.URL, Auth: &AgentAuth{Method: authPKI}}, agentBuilder{identity: gid})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.daemon.CreateSession(context.Background(), "u@x.com"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	seen.mu.Lock()
	defer seen.mu.Unlock()
	if len(seen.cns) != 1 || seen.cns[0] != "switchboard" || seen.bearer[0] != "" {
		t.Errorf("the daemon saw %v (bearer %q), want the gateway's certificate alone", seen.cns, seen.bearer)
	}
}

// A certificate renewed in place is served at the next handshake.
func TestARenewedCertificateIsPickedUp(t *testing.T) {
	ca := authtest.NewCA(t, "trust")
	c := pkiFiles(t, ca, "before")
	kp := &keyPairFile{certFile: c.CertFile, keyFile: c.KeyFile}
	first, err := kp.get(nil)
	if err != nil {
		t.Fatal(err)
	}
	writeKeyPair(t, ca.IssueClient(t, "after"), c.CertFile, c.KeyFile)
	later := time.Now().Add(time.Minute)
	for _, p := range []string{c.CertFile, c.KeyFile} {
		if err := os.Chtimes(p, later, later); err != nil {
			t.Fatal(err)
		}
	}
	second, err := kp.get(nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Leaf.Subject.CommonName != "before" || second.Leaf.Subject.CommonName != "after" {
		t.Errorf("served %q then %q, want the renewal picked up", first.Leaf.Subject.CommonName, second.Leaf.Subject.CommonName)
	}
}

// Every way an entry and the gateway's identity can disagree fails at build,
// naming what is missing, rather than at the first turn.
func TestCertificateAgentsAreValidated(t *testing.T) {
	t.Setenv("TOK_ID", "tok")
	ca := authtest.NewCA(t, "trust")
	spiffe := spiffeIdentity(t, ca)
	none := &gatewayIdentity{kind: identityNone}
	for name, tc := range map[string]struct {
		ac   AgentConfig
		id   *gatewayIdentity
		want string
	}{
		"spiffe without an identity": {AgentConfig{Name: "a", DaemonURL: "https://a", Auth: &AgentAuth{Method: authSPIFFE, ServerID: testServerID}}, none, "--identity spiffe-files"},
		"spiffe over http":           {AgentConfig{Name: "a", DaemonURL: "http://a", Auth: &AgentAuth{Method: authSPIFFE, ServerID: testServerID}}, spiffe, "needs an https URL"},
		"spiffe without server_id":   {AgentConfig{Name: "a", DaemonURL: "https://a", Auth: &AgentAuth{Method: authSPIFFE}}, spiffe, ".auth.server_id"},
		"pki with a spiffe identity": {AgentConfig{Name: "a", DaemonURL: "https://a", Auth: &AgentAuth{Method: authPKI}}, spiffe, "--identity pki"},
		"a token as well":            {AgentConfig{Name: "a", DaemonURL: "https://a", TokenEnv: "TOK_ID", Auth: &AgentAuth{Method: authSPIFFE, ServerID: testServerID}}, spiffe, "both given"},
		"an unknown method":          {AgentConfig{Name: "a", DaemonURL: "https://a", Auth: &AgentAuth{Method: "kerberos"}}, spiffe, "want bearer, spiffe or pki"},
		"bearer with no token_env":   {AgentConfig{Name: "a", DaemonURL: "https://a", Auth: &AgentAuth{Method: authBearer}}, none, ".token_env is required"},
	} {
		if _, err := buildAgent(tc.ac, agentBuilder{identity: tc.id}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	// An entry with only token_env is bearer, as every config before this was.
	if _, err := buildAgent(AgentConfig{Name: "a", DaemonURL: "http://a", TokenEnv: "TOK_ID"}, agentBuilder{}); err != nil {
		t.Errorf("a plain token_env entry: %v", err)
	}
}

func TestIdentitySettingsAreValidated(t *testing.T) {
	for name, tc := range map[string]struct {
		c    identityConfig
		want string
	}{
		"unknown kind":           {identityConfig{Kind: "magic"}, "invalid --identity"},
		"spiffe-files, no td":    {identityConfig{Kind: "spiffe-files", SPIFFEDir: t.TempDir()}, "--spiffe-trust-domain"},
		"spiffe-files, no files": {identityConfig{Kind: "spiffe-files", SPIFFEDir: t.TempDir(), SPIFFETrustDomain: testTD}, "identity:"},
		"pki, no files":          {identityConfig{Kind: "pki"}, "needs --tls-cert-file"},
		"pki, empty ca":          {identityConfig{Kind: "pki", CertFile: "x", KeyFile: "y", CAFile: os.DevNull}, "no certificates"},
	} {
		if _, err := loadIdentity(context.Background(), tc.c); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	if gid, err := loadIdentity(context.Background(), identityConfig{}); err != nil || gid.kind != identityNone {
		t.Errorf("the default: %+v, %v; want none", gid, err)
	}
}
