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

package ax

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/purser/authtest"
)

// The SPIFFE ID Substrate's podidentity signer gives the gateway's pod.
const testSPIFFEID = "spiffe://cluster.local/ns/switchboard/sa/switchboard"

// podCerts is the two signers a pod meets on the way to the router: the
// podidentity CA its own certificate comes from, which the router trusts, and
// the servicedns CA the router's certificate comes from, which the pod trusts.
type podCerts struct {
	podidentity, servicedns *authtest.CA
	dir                     string
	bundle, trust           string // the projected files
}

func newPodCerts(t *testing.T) *podCerts {
	t.Helper()
	dir := t.TempDir()
	p := &podCerts{
		podidentity: authtest.NewCA(t, "podidentity"),
		servicedns:  authtest.NewCA(t, "servicedns"),
		dir:         dir,
		bundle:      filepath.Join(dir, "credential-bundle.pem"),
		trust:       filepath.Join(dir, "trust-bundle.pem"),
	}
	p.writeBundle(t, p.podidentity.Issue(t, authtest.LeafOptions{URISANs: []string{testSPIFFEID}}))
	p.writeTrust(t, p.servicedns)
	return p
}

// writeBundle writes cert as the kubelet writes a podCertificate
// projection's credentialBundlePath: a PKCS#8 key, then the chain, leaf
// first. Renamed into place, as the kubelet's atomic writer does.
func (p *podCerts) writeBundle(t *testing.T, cert tls.Certificate) {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	out := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	for _, c := range cert.Certificate {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c})...)
	}
	writeAtomic(t, p.bundle, out)
}

// writeTrust writes ca's root as a clusterTrustBundle projection.
func (p *podCerts) writeTrust(t *testing.T, ca *authtest.CA) {
	t.Helper()
	writeAtomic(t, p.trust, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Cert().Raw}))
}

func writeAtomic(t *testing.T, path string, b []byte) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// tlsRouter is the router's ingress as Substrate v1.0 serves it: TLS only, a
// client certificate from the podidentity CA required, and a servicedns
// certificate of its own. It records what each request carried.
type tlsRouter struct {
	srv *httptest.Server

	mu     sync.Mutex
	actors []string
	ids    []string // the client certificate's URI SAN, per request
	protos []string
}

func newTLSRouter(t *testing.T, p *podCerts, serverCert tls.Certificate) *tlsRouter {
	t.Helper()
	r := &tlsRouter{}
	r.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.actors = append(r.actors, req.Header.Get(ActorHeader))
		if cs := req.TLS; cs != nil && len(cs.PeerCertificates) > 0 && len(cs.PeerCertificates[0].URIs) > 0 {
			r.ids = append(r.ids, cs.PeerCertificates[0].URIs[0].String())
		}
		r.protos = append(r.protos, req.Proto)
		r.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	r.srv.EnableHTTP2 = true
	r.srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    p.podidentity.Pool(),
	}
	r.srv.StartTLS()
	t.Cleanup(r.srv.Close)
	return r
}

func (r *tlsRouter) seen() (actors, ids, protos []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.actors...), append([]string(nil), r.ids...), append([]string(nil), r.protos...)
}

func routerClient(t *testing.T, rt RouterTLS) *RouterClient {
	t.Helper()
	rc, err := NewRouterClient(rt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	return rc
}

// A task is started and reached through an https router: the pod's
// certificate is presented, the router's verified under its Service name —
// though the client dials an IP, as it dials …svc.cluster.local in a cluster
// — and the actor header rides along, over HTTP/2.
func TestEnsureThroughAnMTLSRouter(t *testing.T) {
	p := newPodCerts(t)
	router := newTLSRouter(t, p, p.servicedns.IssueServer(t, DefaultRouterServerName))
	rc := routerClient(t, RouterTLS{CredentialBundleFile: p.bundle, TrustBundleFile: p.trust})

	axs, conn := fakeServer(t)
	tmpl, err := LoadTemplate(writeTemplate(t, testTemplate))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(router.srv.URL, "https://127.0.0.1:") {
		t.Fatalf("router URL %s: the test wants an address that is not the certificate's name", router.srv.URL)
	}
	c, err := New(Config{Router: router.srv.URL, Atespace: "agents", Template: tmpl, Conn: conn, ReadyTimeout: 5 * time.Second, HTTPClient: rc.HTTP})
	if err != nil {
		t.Fatal(err)
	}
	fastPoll(t)
	if _, err := c.Ensure(context.Background(), "sb-triage-0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if len(axs.creates) != 1 {
		t.Errorf("creates = %d", len(axs.creates))
	}
	actors, ids, protos := router.seen()
	if len(actors) != 1 || actors[0] != "agents/sb-triage-0123456789abcdef" {
		t.Errorf("router saw actors %v", actors)
	}
	if len(ids) != 1 || ids[0] != testSPIFFEID {
		t.Errorf("router saw client identities %v, want %s", ids, testSPIFFEID)
	}
	if len(protos) != 1 || protos[0] != "HTTP/2.0" {
		t.Errorf("protocols = %v, want HTTP/2.0", protos)
	}
}

// The router is pinned by its name and its CA. The servicedns CA certifies
// every Service-selected pod for its own Service's name, so a certificate
// from it for another name is somebody else; one from another CA is nobody.
func TestTheRouterIsVerified(t *testing.T) {
	p := newPodCerts(t)
	for name, cert := range map[string]tls.Certificate{
		"another service's name": p.servicedns.IssueServer(t, "mast-web.agents.svc"),
		"another CA":             authtest.NewCA(t, "impostor").IssueServer(t, DefaultRouterServerName),
	} {
		router := newTLSRouter(t, p, cert)
		rc := routerClient(t, RouterTLS{CredentialBundleFile: p.bundle, TrustBundleFile: p.trust})
		resp, err := rc.HTTP.Get(router.srv.URL + "/readyz")
		if err == nil {
			_ = resp.Body.Close()
			t.Errorf("%s: the request went through", name)
			continue
		}
		// purser's verifyWithSource, not some other failure (caught in
		// review: any error passed).
		if !strings.Contains(err.Error(), "server certificate did not verify") || !permanentTLSError(err) {
			t.Errorf("%s: err = %v, want the router's certificate refused", name, err)
		}
		if actors, _, _ := router.seen(); len(actors) != 0 {
			t.Errorf("%s: the router served %v", name, actors)
		}
	}
}

// A router_server_name other than the default is the one verified.
func TestTheServerNameCanBeSet(t *testing.T) {
	p := newPodCerts(t)
	router := newTLSRouter(t, p, p.servicedns.IssueServer(t, "atenet-router.ate-staging.svc"))
	for _, tc := range []struct {
		name string
		ok   bool
	}{{"", false}, {"atenet-router.ate-staging.svc", true}} {
		rc := routerClient(t, RouterTLS{CredentialBundleFile: p.bundle, TrustBundleFile: p.trust, ServerName: tc.name})
		resp, err := rc.HTTP.Get(router.srv.URL + "/readyz")
		if err == nil {
			_ = resp.Body.Close()
		}
		if (err == nil) != tc.ok {
			t.Errorf("server name %q: err = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

// The pod's certificate rotates under the gateway: the kubelet rewrites the
// bundle, and the next handshake presents the new one, with no restart.
func TestARotatedClientCertificateIsPresented(t *testing.T) {
	p := newPodCerts(t)
	router := newTLSRouter(t, p, p.servicedns.IssueServer(t, DefaultRouterServerName))
	rc := routerClient(t, RouterTLS{CredentialBundleFile: p.bundle, TrustBundleFile: p.trust, ReloadInterval: 10 * time.Millisecond})
	get := func() {
		t.Helper()
		resp, err := rc.HTTP.Get(router.srv.URL + "/readyz")
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	get()

	const rotated = "spiffe://cluster.local/ns/switchboard/sa/switchboard-rotated"
	p.writeBundle(t, p.podidentity.Issue(t, authtest.LeafOptions{URISANs: []string{rotated}}))
	deadline := time.Now().Add(5 * time.Second)
	for {
		rc.HTTP.CloseIdleConnections() // a fresh handshake: the certificate is per connection
		get()
		if _, ids, _ := router.seen(); ids[len(ids)-1] == rotated {
			return
		}
		if time.Now().After(deadline) {
			_, ids, _ := router.seen()
			t.Fatalf("the rotated certificate was never presented; router saw %v", ids)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The servicedns CA rotates too, by a new trust bundle: the router's new
// certificate is accepted once the file says so, with no restart.
func TestARotatedTrustBundleIsFollowed(t *testing.T) {
	p := newPodCerts(t)
	next := authtest.NewCA(t, "servicedns-next")
	router := newTLSRouter(t, p, next.IssueServer(t, DefaultRouterServerName))
	rc := routerClient(t, RouterTLS{CredentialBundleFile: p.bundle, TrustBundleFile: p.trust, ReloadInterval: 10 * time.Millisecond})
	if resp, err := rc.HTTP.Get(router.srv.URL + "/readyz"); err == nil {
		_ = resp.Body.Close()
		t.Fatal("a router certified by a CA not yet in the bundle was trusted")
	}
	p.writeTrust(t, next)
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := rc.HTTP.Get(router.srv.URL + "/readyz")
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the new CA was never trusted: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A missing or malformed file fails at construction, which is startup, not at
// a thread's first message.
func TestBadRouterFilesFailAtStartup(t *testing.T) {
	p := newPodCerts(t)
	certOnly := filepath.Join(p.dir, "cert-only.pem")
	data, _ := os.ReadFile(p.bundle)
	_, rest := pem.Decode(data) // drop the key
	writeAtomic(t, certOnly, rest)
	for name, rt := range map[string]RouterTLS{
		"no credential bundle": {CredentialBundleFile: filepath.Join(p.dir, "nope.pem"), TrustBundleFile: p.trust},
		"no key in the bundle": {CredentialBundleFile: certOnly, TrustBundleFile: p.trust},
		"no trust bundle":      {CredentialBundleFile: p.bundle, TrustBundleFile: filepath.Join(p.dir, "nope.pem")},
		"a key as the trust":   {CredentialBundleFile: p.bundle, TrustBundleFile: p.bundle},
	} {
		if rc, err := NewRouterClient(rt); err == nil {
			_ = rc.Close()
			t.Errorf("%s: accepted", name)
		}
	}
}

// An https router with no client built for it is refused: a default client
// would present no certificate and trust the public roots.
func TestAnHTTPSRouterNeedsItsClient(t *testing.T) {
	tmpl, _ := LoadTemplate(writeTemplate(t, testTemplate))
	for _, router := range []string{"https://r", "HTTPS://r"} {
		if _, err := New(Config{Server: "ax:8080", Router: router, Template: tmpl}); err == nil || !strings.Contains(err.Error(), "NewRouterClient") {
			t.Errorf("%s: err = %v", router, err)
		}
	}
	if _, err := New(Config{Server: "ax:8080", Router: "http://r", Template: tmpl}); err != nil {
		t.Errorf("an http router: %v", err)
	}
}

func TestRouterTLSDefaults(t *testing.T) {
	got := RouterTLS{}.WithDefaults()
	want := RouterTLS{CredentialBundleFile: DefaultCredentialBundleFile, TrustBundleFile: DefaultTrustBundleFile, ServerName: DefaultRouterServerName}
	if got.CredentialBundleFile != want.CredentialBundleFile || got.TrustBundleFile != want.TrustBundleFile || got.ServerName != want.ServerName {
		t.Errorf("defaults = %+v, want %+v", got, want)
	}
	set := RouterTLS{CredentialBundleFile: "/a", TrustBundleFile: "/b", ServerName: "c"}.WithDefaults()
	if set.CredentialBundleFile != "/a" || set.TrustBundleFile != "/b" || set.ServerName != "c" {
		t.Errorf("set fields were replaced: %+v", set)
	}
	if !RouterIsTLS(DefaultRouter) {
		t.Errorf("DefaultRouter %s is not https", DefaultRouter)
	}
}

// A handshake that fails on a certificate is reported at once, not retried
// for the whole ready timeout: neither an identity the router refuses (its
// allowlist) nor a router that fails verification fixes itself.
func TestEnsureFailsFastOnACertificateFailure(t *testing.T) {
	p := newPodCerts(t)
	stranger := newPodCerts(t) // its podidentity CA is not the router's
	for name, tc := range map[string]struct {
		router *tlsRouter
		files  *podCerts
	}{
		"the router refuses the client": {newTLSRouter(t, stranger, stranger.servicedns.IssueServer(t, DefaultRouterServerName)), &podCerts{bundle: p.bundle, trust: stranger.trust}},
		"the router fails verification": {newTLSRouter(t, p, p.servicedns.IssueServer(t, "mast-web.agents.svc")), p},
	} {
		rc := routerClient(t, RouterTLS{CredentialBundleFile: tc.files.bundle, TrustBundleFile: tc.files.trust})
		_, conn := fakeServer(t)
		tmpl, _ := LoadTemplate(writeTemplate(t, testTemplate))
		c, err := New(Config{Router: tc.router.srv.URL, Atespace: "agents", Template: tmpl, Conn: conn, ReadyTimeout: 30 * time.Second, HTTPClient: rc.HTTP})
		if err != nil {
			t.Fatal(err)
		}
		fastPoll(t)
		start := time.Now()
		_, err = c.Ensure(context.Background(), "sb-x-0123456789abcdef")
		if err == nil || !strings.Contains(err.Error(), "TLS handshake") {
			t.Errorf("%s: err = %v, want the handshake failure", name, err)
		}
		if d := time.Since(start); d > 10*time.Second {
			t.Errorf("%s: took %s, want it reported without waiting out the timeout", name, d)
		}
	}
}
