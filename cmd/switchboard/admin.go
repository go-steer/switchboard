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
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/go-steer/purser"
	"github.com/go-steer/purser/authn"
	"github.com/go-steer/purser/authn/mtls"
	"github.com/go-steer/purser/authn/oidc"
	"github.com/go-steer/purser/authz"
	"github.com/go-steer/purser/httpmw"

	"github.com/go-steer/switchboard/internal/logging"
)

// The admin API (#157, docs/agent-identity.md): agents, the default and
// channels' agent settings, changed at runtime through registryAdmin.
//
// It is served on its own listener, never on the public Gateway, and every
// request needs a caller purser has authenticated and an --admin-allow rule
// has made an admin — reads included. The design let any authenticated caller
// read; with an OIDC issuer like Google, "authenticated" is anyone with an
// account, and a read shows every daemon URL.

// The authentication methods.
const (
	adminAuthOIDC  = "oidc"  // an ID token from --admin-oidc-issuer
	adminAuthMTLS  = "mtls"  // a client certificate, over the gateway's own identity
	adminAuthToken = "token" // a static bearer token; loopback only
)

// adminConfig is the admin API's settings.
type adminConfig struct {
	Addr      string
	Auth      string
	Issuer    string
	Audiences []string
	Allow     []string // --admin-allow grants: email:…, domain:…, id:…
	Token     string   // token mode: the value, read from --admin-token-env
	Identity  *gatewayIdentity
	Platform  string // for channel ID checks
	Logf      logging.Logf
	Audit     *auditLog
	// OIDCClient fetches the issuer's keys; nil is purser's default. A test
	// seam, for authtest's issuer.
	OIDCClient *http.Client
}

// adminServer serves the API.
type adminServer struct {
	cfg    adminConfig
	reg    *registryAdmin
	router *Router
	h      http.Handler
	tls    *tls.Config // mtls mode: the listener's
}

// maxAdminBody bounds a request body: an agent entry is a few hundred bytes.
const maxAdminBody = 64 << 10

func newAdminServer(c adminConfig, r *Router, reg *registryAdmin) (*adminServer, error) {
	if reg == nil {
		return nil, errors.New("--admin-addr needs a bridged run: an outbound-only one has no agents")
	}
	s := &adminServer{cfg: c, reg: reg, router: r}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/agents", s.listAgents)
	mux.HandleFunc("GET /v1/agents/{name}", s.getAgent)
	mux.HandleFunc("PUT /v1/agents/{name}", s.putAgent)
	mux.HandleFunc("DELETE /v1/agents/{name}", s.deleteAgent)
	mux.HandleFunc("GET /v1/defaults", s.getDefaults)
	mux.HandleFunc("PUT /v1/defaults", s.putDefaults)
	// {channel...}: a Chat space is "spaces/AAA", slash included.
	mux.HandleFunc("GET /v1/channels/{channel...}", s.getChannel)
	mux.HandleFunc("PUT /v1/channels/{channel...}", s.putChannel)
	mux.HandleFunc("DELETE /v1/channels/{channel...}", s.deleteChannel)

	var gate httpmw.Gate
	switch c.Auth {
	case adminAuthToken:
		if c.Token == "" {
			return nil, errors.New("--admin-auth token: no token in --admin-token-env's variable")
		}
		if !httpmw.IsLoopback(c.Addr) {
			// A shared token cannot say who made a change, which is most of
			// what the audit trail is for; kept for a laptop, not a cluster.
			return nil, fmt.Errorf("--admin-auth token is for a loopback --admin-addr only (got %q); use oidc or mtls", c.Addr)
		}
		s.h = s.tokenGate(mux)
	case adminAuthOIDC, adminAuthMTLS:
		rules, err := adminRules(c.Allow)
		if err != nil {
			return nil, err
		}
		a, err := s.authenticator()
		if err != nil {
			return nil, err
		}
		a, err = authz.WithRules(a, rules)
		if err != nil {
			return nil, err
		}
		mw, err := httpmw.NewCaller(httpmw.CallerOptions{Authenticator: a, Enforce: true})
		if err != nil {
			return nil, err
		}
		gate = mw
		s.h = mw.Middleware()(s.requireAdmin(mux))
	default:
		return nil, fmt.Errorf("invalid --admin-auth %q (want oidc, mtls or token)", c.Auth)
	}
	if gate != nil {
		// Refuses a non-loopback address unless the chain enforces a
		// credential: the property this whole surface rests on.
		if err := httpmw.CheckBind(c.Addr, gate); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// authenticator builds the oidc or mtls authenticator, and for mtls the
// listener's TLS config.
func (s *adminServer) authenticator() (authn.Authenticator, error) {
	c := s.cfg
	if c.Auth == adminAuthOIDC {
		if c.Issuer == "" || len(c.Audiences) == 0 {
			return nil, errors.New("--admin-auth oidc needs --admin-oidc-issuer and --admin-oidc-audience")
		}
		return oidc.New(oidc.Options{Issuer: c.Issuer, Audiences: c.Audiences, HTTPClient: c.OIDCClient})
	}
	id := c.Identity
	switch {
	case id.spiffe():
		cfg, a, err := mtls.NewSPIFFE(mtls.SPIFFEOptions{SVIDSource: id.svid, BundleSource: id.bundle})
		if err != nil {
			return nil, fmt.Errorf("--admin-auth mtls: %w", err)
		}
		s.tls = cfg
		return a, nil
	case id != nil && id.kind == identityPKI:
		cfg, a, err := mtls.NewPKI(mtls.PKIOptions{
			// The gateway's certificate serves the listener too, so it
			// needs the server usage as well as the client one.
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return id.clientCert(nil) },
			ClientCAs:      id.roots,
		})
		if err != nil {
			return nil, fmt.Errorf("--admin-auth mtls: %w", err)
		}
		s.tls = cfg
		return a, nil
	}
	return nil, errors.New("--admin-auth mtls serves over the gateway's own certificate: set --identity")
}

// adminRules turns --admin-allow into purser rules granting the admin bit.
func adminRules(allow []string) (*authz.Rules, error) {
	if len(allow) == 0 {
		return nil, errors.New("--admin-allow is required: name who may administer this gateway (email:…, domain:… or id:…)")
	}
	var rules []authz.Rule
	for _, g := range allow {
		kind, val, ok := strings.Cut(strings.TrimSpace(g), ":")
		val = strings.TrimSpace(val)
		if !ok || val == "" {
			return nil, fmt.Errorf("--admin-allow %q: want email:ADDRESS, domain:DOMAIN or id:IDENTITY", g)
		}
		var m authz.Matcher
		switch kind {
		case "email", "id":
			m = authz.MatchIdentity(val)
		case "domain":
			m = authz.MatchEmailDomain(val)
		default:
			return nil, fmt.Errorf("--admin-allow %q: want email:, domain: or id:", g)
		}
		rules = append(rules, authz.Rule{Name: g, Match: m, Admin: true})
	}
	return authz.NewRules(rules...)
}

// requireAdmin refuses an authenticated caller no rule made an admin.
func (s *adminServer) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, ok := purser.CallerFromContext(r.Context()); !ok || !c.Admin {
			writeAdminError(w, http.StatusForbidden, "not an administrator of this gateway")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// tokenGate is token mode's authentication: the token is the admin.
func (s *adminServer) tokenGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		const scheme = "Bearer "
		if !strings.HasPrefix(h, scheme) || subtle.ConstantTimeCompare([]byte(h[len(scheme):]), []byte(s.cfg.Token)) != 1 {
			writeAdminError(w, http.StatusUnauthorized, "missing or wrong bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// serve runs the listener until ctx ends.
func (s *adminServer) serve(ctx context.Context) error {
	return serveHTTPTLS(ctx, "admin", s.cfg.Addr, s.h, s.tls, s.cfg.Logf)
}

// --- views -------------------------------------------------------------------

// agentView is an agent as the API shows it.
type agentView struct {
	AgentConfig
	// Pinned: declared in the config file, so read-only here.
	Pinned bool `json:"pinned"`
	// Active: in the registry now. A runtime agent the environment has
	// broken (its token variable gone) is kept but inactive; problems says
	// why.
	Active bool `json:"active"`
}

type agentsView struct {
	Agents       []agentView `json:"agents"`
	DefaultAgent string      `json:"default_agent"`
	Problems     []string    `json:"problems,omitempty"`
}

func (s *adminServer) agentsNow() agentsView {
	st, pinned, problems := s.reg.view()
	set := s.router.agents()
	v := agentsView{DefaultAgent: set.def}
	for _, ac := range pinned {
		v.Agents = append(v.Agents, agentView{AgentConfig: ac, Pinned: true, Active: true})
	}
	for _, ac := range st.Agents {
		_, active := set.byName[ac.Name]
		if slices.ContainsFunc(pinned, func(p AgentConfig) bool { return p.Name == ac.Name }) {
			active = false // shadowed by the config's
		}
		v.Agents = append(v.Agents, agentView{AgentConfig: ac, Active: active})
	}
	for _, p := range problems {
		v.Problems = append(v.Problems, p.Error())
	}
	return v
}

func (s *adminServer) listAgents(w http.ResponseWriter, r *http.Request) {
	writeAdminJSON(w, http.StatusOK, s.agentsNow())
}

func (s *adminServer) getAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	for _, a := range s.agentsNow().Agents {
		if a.Name == name {
			writeAdminJSON(w, http.StatusOK, a)
			return
		}
	}
	writeAdminError(w, http.StatusNotFound, fmt.Sprintf("no agent %q", name))
}

func (s *adminServer) putAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var ac AgentConfig
	if !decodeAdmin(w, r, &ac) {
		return
	}
	if ac.Name != "" && ac.Name != name {
		writeAdminError(w, http.StatusBadRequest, fmt.Sprintf("the body names agent %q and the path %q", ac.Name, name))
		return
	}
	ac.Name = name
	before, _ := s.reg.agentEntry(name)
	err := s.reg.putAgent(ac)
	s.audited(r, "put agent "+name, before, ac, err)
	s.reply(w, err, http.StatusOK, func() any { a, _ := s.reg.agentEntry(name); return a })
}

func (s *adminServer) deleteAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	before, _ := s.reg.agentEntry(name)
	err := s.reg.removeAgent(name)
	s.audited(r, "delete agent "+name, before, nil, err)
	s.reply(w, err, http.StatusNoContent, nil)
}

type defaultsView struct {
	DefaultAgent string `json:"default_agent"`
	Pinned       bool   `json:"pinned"`
}

func (s *adminServer) getDefaults(w http.ResponseWriter, r *http.Request) {
	writeAdminJSON(w, http.StatusOK, defaultsView{DefaultAgent: s.router.agents().def, Pinned: s.reg.pinnedDefault})
}

func (s *adminServer) putDefaults(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DefaultAgent string `json:"default_agent"`
	}
	if !decodeAdmin(w, r, &body) {
		return
	}
	before := s.router.agents().def
	err := s.reg.setDefault(strings.TrimSpace(body.DefaultAgent))
	s.audited(r, "put defaults", before, body.DefaultAgent, err)
	s.reply(w, err, http.StatusOK, func() any {
		return defaultsView{DefaultAgent: s.router.agents().def, Pinned: s.reg.pinnedDefault}
	})
}

// channelView is a channel's agent settings: what applies now, and where it
// comes from.
type channelView struct {
	Channel      string         `json:"channel"`
	DefaultAgent string         `json:"default_agent"`
	Agents       []string       `json:"agents"`
	Runtime      *channelAgents `json:"runtime,omitempty"` // what the API set, if anything
	Pinned       bool           `json:"pinned"`
}

func (s *adminServer) channelNow(id string) channelView {
	set, cs := s.router.routing(id)
	def, _ := set.channelAgent(cs)
	v := channelView{Channel: id, DefaultAgent: set.resolve(def), Pinned: s.reg.pinnedChannels[id]}
	for _, a := range set.allowedIn(cs) {
		v.Agents = append(v.Agents, a.name)
	}
	if ca, ok := s.reg.current().Channels[id]; ok {
		v.Runtime = &ca
	}
	return v
}

func (s *adminServer) getChannel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("channel")
	if !s.validChannel(w, id) {
		return
	}
	writeAdminJSON(w, http.StatusOK, s.channelNow(id))
}

func (s *adminServer) putChannel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("channel")
	if !s.validChannel(w, id) {
		return
	}
	var ca channelAgents
	if !decodeAdmin(w, r, &ca) {
		return
	}
	ca.DefaultAgent = strings.TrimSpace(ca.DefaultAgent)
	var names []string
	for _, n := range ca.Agents {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	ca.Agents = names
	before := s.reg.current().Channels[id]
	err := s.reg.setChannel(id, ca)
	s.audited(r, "put channel "+id, before, ca, err)
	s.reply(w, err, http.StatusOK, func() any { return s.channelNow(id) })
}

func (s *adminServer) deleteChannel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("channel")
	if !s.validChannel(w, id) {
		return
	}
	before := s.reg.current().Channels[id]
	err := s.reg.clearChannel(id)
	s.audited(r, "delete channel "+id, before, nil, err)
	s.reply(w, err, http.StatusNoContent, nil)
}

// validChannel refuses an ID this platform cannot have: a setting for it
// would apply to nothing, and silently.
func (s *adminServer) validChannel(w http.ResponseWriter, id string) bool {
	if err := checkChannelID(id, s.cfg.Platform); err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return false
	}
	return true
}

// --- plumbing ----------------------------------------------------------------

// reply maps a registry error onto a status, or writes body() with ok.
func (s *adminServer) reply(w http.ResponseWriter, err error, ok int, body func() any) {
	switch {
	case err == nil && body == nil:
		w.WriteHeader(ok)
	case err == nil:
		writeAdminJSON(w, ok, body())
	case errors.Is(err, errPinned):
		writeAdminError(w, http.StatusConflict, err.Error())
	case errors.Is(err, errUnknown):
		writeAdminError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, errRejected):
		writeAdminError(w, http.StatusUnprocessableEntity, err.Error())
	default:
		s.cfg.Logf.Errorf("admin: %v", err)
		writeAdminError(w, http.StatusInternalServerError, "the change could not be saved")
	}
}

// audited records a change attempt, applied or refused, with who made it.
func (s *adminServer) audited(r *http.Request, what string, before, after any, err error) {
	rec := auditRecord{Kind: auditAdmin, Outcome: auditApplied, Admin: &auditAdminChange{Change: what}}
	if c, ok := purser.CallerFromContext(r.Context()); ok {
		rec.Caller = c.Identity
	} else if s.cfg.Auth == adminAuthToken {
		rec.Caller = "token"
	}
	if src, ok := purser.AuthSourceFromContext(r.Context()); ok {
		rec.Admin.AuthSource = string(src)
	}
	if by, ok := purser.ProxyByFromContext(r.Context()); ok {
		rec.Admin.ProxiedBy = by
	}
	rec.Admin.Before, _ = json.Marshal(before)
	rec.Admin.After, _ = json.Marshal(after)
	if err != nil {
		rec.Outcome = auditRefused
		rec.Admin.Reason = err.Error()
	}
	s.cfg.Audit.record(rec)
	if err == nil {
		s.cfg.Logf.Infof("admin: %s by %s", what, rec.Caller)
	}
}

func decodeAdmin(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxAdminBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeAdminError(w, http.StatusBadRequest, "request body: "+err.Error())
		return false
	}
	return true
}

func writeAdminJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAdminError(w http.ResponseWriter, status int, msg string) {
	writeAdminJSON(w, status, map[string]string{"error": msg})
}
