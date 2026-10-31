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
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-steer/purser/authn/mtls"
	"github.com/go-steer/purser/client"

	"github.com/go-steer/switchboard/pkg/daemon"
)

// Reaching the router over mTLS (#181).
//
// From Agent Substrate v1.0 the atenet router serves no plaintext: its
// listeners are TLS only, and it admits a client only with a certificate from
// the podidentity signer whose SPIFFE ID is on its allowlist
// (--ingress-allowed-spiffe-ids). The router's own certificate comes from a
// different signer, servicedns, and carries only the DNS name of its Service.
//
// So a client presents its pod certificate and verifies the router as a
// standard-CA server: purser's PKI client profile, with both halves read from
// projected volumes and re-read as the kubelet rotates them.

// The defaults: the router's in-cluster address, the name its certificate
// carries, and the paths ax-server's own manifest mounts the two volumes at.
const (
	// DefaultRouter is the router's TLS address inside the cluster.
	DefaultRouter = "https://atenet-router.ate-system.svc.cluster.local"
	// DefaultRouterServerName is the one DNS name on the router's
	// certificate. Dialing any other name (…svc.cluster.local, an IP)
	// verifies against it.
	DefaultRouterServerName = "atenet-router.ate-system.svc"
	// DefaultCredentialBundleFile is a podCertificate projection's
	// credentialBundlePath: the private key, then the chain, leaf first.
	DefaultCredentialBundleFile = "/run/podidentity.podcert.ate.dev/credential-bundle.pem"
	// DefaultTrustBundleFile is a clusterTrustBundle projection of the
	// servicedns signer's roots.
	DefaultTrustBundleFile = "/run/servicedns-ca/trust-bundle.pem"
)

// RouterTLS is how a client authenticates to the router and verifies it.
// Empty fields take the defaults above.
type RouterTLS struct {
	// CredentialBundleFile is the client's key and certificate chain in one
	// PEM file, as a podCertificate projection's credentialBundlePath
	// writes it (signer podidentity.podcert.ate.dev/identity).
	CredentialBundleFile string
	// TrustBundleFile is the roots the router's certificate must chain to,
	// as a clusterTrustBundle projection writes them (signer
	// servicedns.podcert.ate.dev/identity).
	TrustBundleFile string
	// ServerName is the name verified on the router's certificate.
	ServerName string
	// ReloadInterval is how often both files are re-read; zero is purser's
	// default (30s). A pod certificate is refreshed half an hour before it
	// expires, so the default is ample; a test shortens it.
	ReloadInterval time.Duration
	// OnError hears a reload that failed, while the last good certificate
	// or pool stays in use. Without it a mount gone bad is noticed only when
	// the certificate expires.
	OnError func(error)
}

// WithDefaults is t with every empty field filled in.
func (t RouterTLS) WithDefaults() RouterTLS {
	if t.CredentialBundleFile == "" {
		t.CredentialBundleFile = DefaultCredentialBundleFile
	}
	if t.TrustBundleFile == "" {
		t.TrustBundleFile = DefaultTrustBundleFile
	}
	if t.ServerName == "" {
		t.ServerName = DefaultRouterServerName
	}
	return t
}

// RouterClient is an HTTP client for an https router, and the file watchers
// behind its certificates.
type RouterClient struct {
	// HTTP follows no redirects and has no Timeout, like the plain client
	// New makes: it carries the session contract's SSE streams. Pass it as
	// Config.HTTPClient.
	HTTP *http.Client

	cert  *mtls.CertificateFileSource
	roots *mtls.CertPoolFileSource
}

// NewRouterClient reads the credential and trust bundles once, so a missing
// or malformed file fails here, at startup, rather than at a thread's first
// message, and keeps re-reading them.
func NewRouterClient(t RouterTLS) (*RouterClient, error) {
	t = t.WithDefaults()
	cert, err := mtls.NewCertificateFileSource(mtls.CertificateFileOptions{
		CredentialBundlePath: t.CredentialBundleFile,
		ReloadInterval:       t.ReloadInterval,
		OnError:              t.OnError,
	})
	if err != nil {
		return nil, fmt.Errorf("ax: router client certificate: %w", err)
	}
	roots, err := mtls.NewCertPoolFileSource(mtls.CertPoolFileOptions{
		Path:           t.TrustBundleFile,
		ReloadInterval: t.ReloadInterval,
		OnError:        t.OnError,
	})
	if err != nil {
		_ = cert.Close()
		return nil, fmt.Errorf("ax: router trust bundle: %w", err)
	}
	cfg, err := client.NewPKI(client.PKIOptions{
		GetClientCertificate: cert.GetClientCertificate,
		// Consulted per handshake, so a rotated servicedns CA needs no
		// restart.
		RootCAsSource: roots,
		// Always set: every pod a Service selects gets a servicedns
		// certificate from the same CA for that Service's name, so the
		// name is what tells the router from any of them.
		ServerName: t.ServerName,
	})
	if err != nil {
		_ = cert.Close()
		_ = roots.Close()
		return nil, fmt.Errorf("ax: router TLS: %w", err)
	}
	tr := client.Transport(cfg)
	// purser's transport speaks HTTP/2 when the router offers it, so every
	// stream to every task shares one connection; pings find a dead one
	// before it stalls them all (as cmd/switchboard's identity client does).
	tr.HTTP2 = &http.HTTP2Config{SendPingTimeout: 30 * time.Second, PingTimeout: 15 * time.Second}
	return &RouterClient{
		HTTP:  &http.Client{Transport: tr, CheckRedirect: daemon.NoRedirects},
		cert:  cert,
		roots: roots,
	}, nil
}

// Close stops re-reading the files; handshakes after it fail.
func (r *RouterClient) Close() error {
	if r == nil {
		return nil
	}
	r.HTTP.CloseIdleConnections()
	return errors.Join(r.cert.Close(), r.roots.Close())
}
