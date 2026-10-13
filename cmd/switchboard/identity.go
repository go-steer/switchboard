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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-steer/purser/authn/mtls"
	"github.com/go-steer/purser/client"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"

	"github.com/go-steer/switchboard/internal/logging"
	"github.com/go-steer/switchboard/pkg/daemon"
)

// The gateway's own identity (#158, docs/agent-identity.md).
//
// switchboard can authenticate to a daemon as itself — one workload identity,
// presented as a client certificate through purser's client half — instead of
// with a bearer token per agent. The daemon then grants that identity the
// right to assert callers. This file holds the identity, configured once, and
// builds the HTTP client each agent is reached through.
//
// Caller credentials are still core-agent's business: switchboard only proves
// who *it* is, and goes on asserting the chat user in X-Asserted-Caller.

// identityKind is how the gateway gets its certificate.
type identityKind string

const (
	identityNone       identityKind = "none"                // bearer tokens only
	identitySPIFFEFile identityKind = "spiffe-files"        // an SVID delivered as files (GKE managed workload identity)
	identitySPIFFEAPI  identityKind = "spiffe-workload-api" // an SVID from a SPIFFE Workload API socket (SPIRE)
	identityPKI        identityKind = "pki"                 // a certificate from a standard CA (cert-manager)
)

// identityConfig is the identity settings as flags and the config file give
// them. Paths and names only: nothing here is a secret.
type identityConfig struct {
	Kind              string
	SPIFFEDir         string // spiffe-files: the credentials directory
	SPIFFETrustDomain string // spiffe-files: the domain the bundle file vouches for
	SPIFFESocket      string // spiffe-workload-api: the Workload API address
	CertFile          string // pki
	KeyFile           string // pki
	CAFile            string // pki
}

// gatewayIdentity is the identity in use. The zero kind is none.
type gatewayIdentity struct {
	kind identityKind

	svid   x509svid.Source
	bundle x509bundle.Source

	clientCert func(*tls.CertificateRequestInfo) (*tls.Certificate, error)
	roots      *x509.CertPool

	closer io.Closer

	// clients caches each agent's HTTP client by what builds it, so a
	// registry change (which rebuilds every runtime agent, twice) keeps the
	// connections it has rather than handshaking again (caught in review).
	mu      sync.Mutex
	clients map[string]*http.Client
}

// loadIdentity sets up the identity c describes, reading its credentials once
// so a missing or malformed file stops startup rather than the first turn.
func loadIdentity(ctx context.Context, c identityConfig, logf logging.Logf) (*gatewayIdentity, error) {
	kind := identityKind(strings.TrimSpace(c.Kind))
	if kind == "" {
		kind = identityNone
	}
	switch kind {
	case identityNone:
		return &gatewayIdentity{kind: identityNone}, nil
	case identitySPIFFEFile:
		if c.SPIFFETrustDomain == "" {
			return nil, errors.New("--identity spiffe-files needs --spiffe-trust-domain: the bundle file does not say which domain it vouches for (on GKE, PROJECT_ID.svc.id.goog)")
		}
		td, err := spiffeid.TrustDomainFromString(c.SPIFFETrustDomain)
		if err != nil {
			return nil, fmt.Errorf("--spiffe-trust-domain %q: %w", c.SPIFFETrustDomain, err)
		}
		opts := mtls.GKEFileOptions(td)
		if c.SPIFFEDir != "" {
			opts.CertPath = filepath.Join(c.SPIFFEDir, mtls.GKECertFile)
			opts.KeyPath = filepath.Join(c.SPIFFEDir, mtls.GKEKeyFile)
			opts.BundlePath = filepath.Join(c.SPIFFEDir, mtls.GKEBundleFile)
		}
		// Reported, or a mount gone bad is only noticed when the SVID
		// expires (caught in review).
		opts.OnError = func(err error) { logf.Warnf("identity: reload the SVID files: %v", err) }
		src, err := mtls.NewSPIFFEFileSource(opts)
		if err != nil {
			return nil, fmt.Errorf("identity: %w", err)
		}
		return &gatewayIdentity{kind: kind, svid: src, bundle: src, closer: src}, nil
	case identitySPIFFEAPI:
		var opts []workloadapi.X509SourceOption
		if c.SPIFFESocket != "" {
			opts = append(opts, workloadapi.WithClientOptions(workloadapi.WithAddr(c.SPIFFESocket)))
		}
		// Bounded: NewX509Source waits for the first SVID, and an agent that
		// never answers would hang startup with nothing said.
		wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		src, err := workloadapi.NewX509Source(wctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("identity: SPIFFE Workload API: %w", err)
		}
		return &gatewayIdentity{kind: kind, svid: src, bundle: src, closer: src}, nil
	case identityPKI:
		if c.CertFile == "" || c.KeyFile == "" || c.CAFile == "" {
			return nil, errors.New("--identity pki needs --tls-cert-file, --tls-key-file and --tls-ca-file")
		}
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("--tls-ca-file: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("--tls-ca-file %s: no certificates in it", c.CAFile)
		}
		kp := &keyPairFile{certFile: c.CertFile, keyFile: c.KeyFile, logf: logf}
		if _, err := kp.get(nil); err != nil {
			return nil, err
		}
		return &gatewayIdentity{kind: kind, clientCert: kp.get, roots: roots}, nil
	}
	return nil, fmt.Errorf("invalid --identity %q (want none, spiffe-files, spiffe-workload-api or pki)", c.Kind)
}

// close releases the identity's credential watcher, if it has one.
func (g *gatewayIdentity) close() {
	if g != nil && g.closer != nil {
		_ = g.closer.Close()
	}
}

func (g *gatewayIdentity) spiffe() bool {
	return g != nil && (g.kind == identitySPIFFEFile || g.kind == identitySPIFFEAPI)
}

// httpClientFor builds the client an agent using auth is reached through. The
// daemon URL must be https: the certificate is the credential, and it travels
// only in a TLS handshake.
func (g *gatewayIdentity) httpClientFor(auth *AgentAuth, daemonURL string) (*http.Client, error) {
	u, err := url.Parse(daemonURL)
	if err != nil || !strings.HasPrefix(daemonURL, "https://") || u.Host == "" {
		return nil, fmt.Errorf(".daemon_url %q: auth.method %q needs an https URL", daemonURL, auth.Method)
	}
	switch {
	case auth.Method == authSPIFFE && auth.ServerName != "":
		return nil, errors.New(".auth.server_name is for pki; a spiffe daemon is named by server_id")
	case auth.Method == authPKI && auth.ServerID != "":
		return nil, errors.New(".auth.server_id is for spiffe; a pki daemon is named by server_name")
	}
	var cfg *tls.Config
	switch auth.Method {
	case authSPIFFE:
		if !g.spiffe() {
			return nil, errors.New(".auth.method \"spiffe\" needs the gateway's SPIFFE identity (--identity spiffe-files or spiffe-workload-api)")
		}
		id, err := spiffeid.FromString(auth.ServerID)
		if err != nil {
			return nil, fmt.Errorf(".auth.server_id %q: want the SPIFFE ID the daemon presents: %w", auth.ServerID, err)
		}
		if _, err := g.bundle.GetX509BundleForTrustDomain(id.TrustDomain()); err != nil {
			// Would fail every handshake: no anchors to verify the daemon by.
			return nil, fmt.Errorf(".auth.server_id %q: the gateway holds no trust bundle for %s", auth.ServerID, id.TrustDomain())
		}
		if c := g.cached(auth, u.Host); c != nil {
			return c, nil
		}
		cfg, err = client.NewSPIFFE(client.SPIFFEOptions{
			SVIDSource:   g.svid,
			BundleSource: g.bundle,
			Authorize:    spiffeid.MatchID(id),
		})
		if err != nil {
			return nil, fmt.Errorf(".auth: %w", err)
		}
	case authPKI:
		if g == nil || g.kind != identityPKI {
			return nil, errors.New(".auth.method \"pki\" needs the gateway's certificate (--identity pki)")
		}
		if c := g.cached(auth, u.Host); c != nil {
			return c, nil
		}
		cfg, err = client.NewPKI(client.PKIOptions{
			GetClientCertificate: g.clientCert,
			RootCAs:              g.roots,
			ServerName:           auth.ServerName,
		})
		if err != nil {
			return nil, fmt.Errorf(".auth: %w", err)
		}
	default:
		return nil, fmt.Errorf(".auth.method %q is not a certificate method", auth.Method)
	}
	tr := client.Transport(cfg)
	// purser's transport speaks HTTP/2, so every stream to this agent shares
	// one connection; pings find a dead one before it stalls them all, where
	// HTTP/1.1 kept each stream apart (caught in review).
	tr.HTTP2 = &http.HTTP2Config{SendPingTimeout: 30 * time.Second, PingTimeout: 15 * time.Second}
	hc := &http.Client{
		Transport: tr,
		// No client-wide Timeout, as for the plain client: it would cut the
		// long-lived SSE streams. And no redirects: the contract has none,
		// and following one could carry the asserted caller and the message
		// to a plain-http URL (caught in review).
		CheckRedirect: daemon.NoRedirects,
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.clients == nil {
		g.clients = make(map[string]*http.Client)
	}
	g.clients[clientKey(auth, u.Host)] = hc
	return hc, nil
}

func clientKey(auth *AgentAuth, host string) string {
	return auth.Method + "|" + auth.ServerID + "|" + auth.ServerName + "|" + host
}

// cached is an agent's client built before with the same settings.
func (g *gatewayIdentity) cached(auth *AgentAuth, host string) *http.Client {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.clients[clientKey(auth, host)]
}

// keyPairFile serves a certificate and key from files, re-reading them when
// either changes, so a rotated certificate (cert-manager renews in place) is
// picked up at the next handshake without a restart.
type keyPairFile struct {
	certFile, keyFile string
	logf              logging.Logf
	now               func() time.Time // nil: time.Now

	mu      sync.Mutex
	cert    *tls.Certificate
	modCert time.Time
	modKey  time.Time
	failing bool // a reload has failed since the last success: said once
}

func (k *keyPairFile) get(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	ci, cerr := os.Stat(k.certFile)
	ki, kerr := os.Stat(k.keyFile)
	if cerr == nil && kerr == nil && k.cert != nil && ci.ModTime().Equal(k.modCert) && ki.ModTime().Equal(k.modKey) {
		return k.usable()
	}
	var pair tls.Certificate
	err := errors.Join(cerr, kerr)
	if err == nil {
		pair, err = tls.LoadX509KeyPair(k.certFile, k.keyFile)
	}
	if err != nil {
		if k.cert == nil {
			return nil, fmt.Errorf("--tls-cert-file/--tls-key-file: %w", err)
		}
		// Mid-rotation, one file written and not the other: keep the pair
		// that worked until both agree — but say so, once, since a rotation
		// that never completes is otherwise silent (caught in review).
		if !k.failing {
			k.failing = true
			k.logf.Warnf("identity: reload --tls-cert-file/--tls-key-file: %v; still presenting the previous certificate", err)
		}
		return k.usable()
	}
	k.cert, k.modCert, k.modKey, k.failing = &pair, ci.ModTime(), ki.ModTime(), false
	return k.usable()
}

// usable is the cached pair, unless its certificate has expired: presenting
// one only defers the failure to the daemon (caught in review).
func (k *keyPairFile) usable() (*tls.Certificate, error) {
	now := time.Now
	if k.now != nil {
		now = k.now
	}
	if leaf := k.cert.Leaf; leaf != nil && now().After(leaf.NotAfter) {
		return nil, fmt.Errorf("--tls-cert-file: the certificate expired at %s and has not been renewed", leaf.NotAfter.UTC().Format(time.RFC3339))
	}
	return k.cert, nil
}
