package mcp

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// OAuth is a small OAuth 2.1 authorization server for the MCP endpoint, so
// clients that follow the MCP authorization spec (claude.ai and ChatGPT
// connectors, Claude Code, ...) can connect with just the endpoint URL
// instead of the secret token URL.
//
// Flow: the client discovers the metadata, registers itself (dynamic client
// registration), and sends the user's browser to /authorize. Nexora shows
// the request in the app with a short code that the browser page shows too;
// once the user approves there, the browser is redirected back with an
// authorization code, which the client exchanges (with PKCE) for an access
// token and a rotating refresh token.
//
// Everything lives under the MCP base path (/mcp/oauth/..., /mcp/.well-known/...)
// because some network providers only forward that path. Clients, grants
// and token hashes are persisted (0600); tokens themselves never are.
type OAuth struct {
	path   string
	now    func() time.Time
	regLim *rate.Limiter

	mu       sync.Mutex
	clients  map[string]*oauthClient
	grants   map[string]*grant
	access   map[string]accessToken // sha256(token) -> token
	codes    map[string]*authCode   // sha256(code) -> code
	requests map[string]*ConsentRequest

	// OnChange, if set, is called (in its own goroutine) whenever the set
	// of pending consent requests changes.
	OnChange func()
}

const (
	consentTTL  = 5 * time.Minute
	codeTTL     = 2 * time.Minute
	accessTTL   = time.Hour
	refreshIdle = 30 * 24 * time.Hour // a connection unused this long must sign in again
	// refreshGrace lets a client retry a refresh whose response it lost.
	refreshGrace = time.Minute
	maxClients   = 200
)

type oauthClient struct {
	ID           string    `json:"id"`
	SecretHash   string    `json:"secretHash,omitempty"`
	AuthMethod   string    `json:"authMethod"`
	Name         string    `json:"name"`
	RedirectURIs []string  `json:"redirectUris"`
	CreatedAt    time.Time `json:"createdAt"`
}

type grant struct {
	ID          string    `json:"id"`
	ClientID    string    `json:"clientId"`
	ClientName  string    `json:"clientName"`
	Via         string    `json:"via"` // host the user approved it through
	CreatedAt   time.Time `json:"createdAt"`
	LastUsed    time.Time `json:"lastUsed"`
	RefreshHash string    `json:"refreshHash"`
	// PrevRefreshHash stays valid for refreshGrace after rotation.
	PrevRefreshHash string    `json:"prevRefreshHash,omitempty"`
	RotatedAt       time.Time `json:"rotatedAt"`
}

type accessToken struct {
	GrantID string    `json:"grantId"`
	Expires time.Time `json:"expires"`
}

type authCode struct {
	clientID, redirectURI, challenge, via string
	expires                               time.Time
}

// ConsentRequest is a client waiting for the user to approve it in the app.
type ConsentRequest struct {
	ID         string `json:"id"`
	ClientName string `json:"clientName"`
	// RedirectHost is where the approval is sent: the real identity of
	// the client, since anyone can register any name.
	RedirectHost string `json:"redirectHost"`
	// Code is shown on the browser page and in the app; they must match.
	Code      string    `json:"code"`
	Via       string    `json:"via"` // the host the browser came through
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`

	clientID, redirectURI, state, challenge, issuer string
	decision                                        consentDecision
}

type consentDecision int

const (
	consentPending consentDecision = iota
	consentAllowed
	consentDenied
)

// Connection is an approved client, for the UI.
type Connection struct {
	ID         string    `json:"id"`
	ClientName string    `json:"clientName"`
	Via        string    `json:"via"`
	CreatedAt  time.Time `json:"createdAt"`
	LastUsed   time.Time `json:"lastUsed"`
}

// OpenOAuth loads the authorization server's state from path.
func OpenOAuth(path string) (*OAuth, error) {
	o := &OAuth{
		path:     path,
		now:      time.Now,
		regLim:   rate.NewLimiter(rate.Every(10*time.Second), 10),
		clients:  map[string]*oauthClient{},
		grants:   map[string]*grant{},
		access:   map[string]accessToken{},
		codes:    map[string]*authCode{},
		requests: map[string]*ConsentRequest{},
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return o, nil
	}
	if err != nil {
		return nil, err
	}
	var s oauthStore
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	for _, c := range s.Clients {
		o.clients[c.ID] = c
	}
	for _, g := range s.Grants {
		o.grants[g.ID] = g
	}
	for h, a := range s.Access {
		o.access[h] = a
	}
	return o, nil
}

type oauthStore struct {
	Clients []*oauthClient         `json:"clients"`
	Grants  []*grant               `json:"grants"`
	Access  map[string]accessToken `json:"access"`
}

// saveLocked persists clients, grants and live access token hashes,
// dropping whatever has expired. Caller holds o.mu.
func (o *OAuth) saveLocked() error {
	now := o.now()
	s := oauthStore{Access: map[string]accessToken{}}
	for h, a := range o.access {
		if now.After(a.Expires) || o.grants[a.GrantID] == nil {
			delete(o.access, h)
			continue
		}
		s.Access[h] = a
	}
	for id, g := range o.grants {
		if now.Sub(g.LastUsed) > refreshIdle {
			delete(o.grants, id)
			continue
		}
		s.Grants = append(s.Grants, g)
	}
	for _, c := range o.clients {
		s.Clients = append(s.Clients, c)
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(o.path), 0o755); err != nil {
		return err
	}
	tmp := o.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, o.path)
}

// Valid reports whether tok is a live access token, and marks its
// connection as used.
func (o *OAuth) Valid(tok string) bool {
	_, ok := o.lookup(tok)
	return ok
}

// lookup returns the client name behind a live access token and marks its
// connection as used.
func (o *OAuth) lookup(tok string) (string, bool) {
	if tok == "" {
		return "", false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	a, ok := o.access[hashToken(tok)]
	if !ok || o.now().After(a.Expires) {
		return "", false
	}
	g := o.grants[a.GrantID]
	if g == nil {
		return "", false
	}
	g.LastUsed = o.now() // persisted with the next refresh
	return g.ClientName, true
}

// Pending lists consent requests waiting for the user, oldest first.
func (o *OAuth) Pending() []ConsentRequest {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.expireLocked()
	var out []ConsentRequest
	for _, r := range o.requests {
		if r.decision == consentPending {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Answer approves or denies a pending consent request. The browser that
// started it picks the answer up on its next poll.
func (o *OAuth) Answer(id string, allow bool) error {
	o.mu.Lock()
	r, ok := o.requests[id]
	if !ok || r.decision != consentPending || o.now().After(r.ExpiresAt) {
		o.mu.Unlock()
		return errors.New("this connection request is no longer waiting")
	}
	r.decision = consentDenied
	if allow {
		r.decision = consentAllowed
	}
	o.mu.Unlock()
	o.changed()
	return nil
}

// Connections lists approved clients, most recently used first.
func (o *OAuth) Connections() []Connection {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := []Connection{}
	for _, g := range o.grants {
		out = append(out, Connection{ID: g.ID, ClientName: g.ClientName, Via: g.Via, CreatedAt: g.CreatedAt, LastUsed: g.LastUsed})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastUsed.After(out[j].LastUsed) })
	return out
}

// Revoke disconnects a client: its access and refresh tokens stop working
// immediately.
func (o *OAuth) Revoke(id string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.grants[id] == nil {
		return errors.New("unknown connection")
	}
	delete(o.grants, id)
	for h, a := range o.access {
		if a.GrantID == id {
			delete(o.access, h)
		}
	}
	return o.saveLocked()
}

func (o *OAuth) changed() {
	if o.OnChange != nil {
		go o.OnChange()
	}
}

// expireLocked drops expired consent requests and codes. Caller holds o.mu.
func (o *OAuth) expireLocked() {
	now := o.now()
	for id, r := range o.requests {
		if now.After(r.ExpiresAt) {
			delete(o.requests, id)
		}
	}
	for h, c := range o.codes {
		if now.After(c.expires) {
			delete(o.codes, h)
		}
	}
}

/* ------------------------------------------------------------- handlers */

// mount registers the OAuth endpoints. The issuer is the MCP endpoint's
// URL as the client sees it (Guard.baseURL), e.g. https://laptop.ts.net/nexora.
func (o *OAuth) mount(mux *http.ServeMux, g *Guard) {
	prm := func(w http.ResponseWriter, r *http.Request) {
		base := g.baseURL(r)
		writeJSON(w, http.StatusOK, map[string]any{
			"resource":                 base,
			"authorization_servers":    []string{base},
			"bearer_methods_supported": []string{"header"},
			"resource_name":            "Nexora",
		})
	}
	asm := func(w http.ResponseWriter, r *http.Request) {
		base := g.baseURL(r)
		writeJSON(w, http.StatusOK, map[string]any{
			"issuer":                                         base,
			"authorization_endpoint":                         base + "/oauth/authorize",
			"token_endpoint":                                 base + "/oauth/token",
			"registration_endpoint":                          base + "/oauth/register",
			"revocation_endpoint":                            base + "/oauth/revoke",
			"response_types_supported":                       []string{"code"},
			"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
			"code_challenge_methods_supported":               []string{"S256"},
			"token_endpoint_auth_methods_supported":          []string{"none", "client_secret_post", "client_secret_basic"},
			"authorization_response_iss_parameter_supported": true,
		})
	}
	// Metadata is served both under the MCP path (reachable through every
	// provider) and at the RFC 8414/9728 well-known locations for the
	// "/mcp" resource path (reachable locally and through tunnels).
	for _, p := range []string{"/mcp/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		mux.Handle(p, o.public(g, prm))
	}
	for _, p := range []string{
		"/mcp/.well-known/oauth-authorization-server", "/mcp/.well-known/openid-configuration",
		"/.well-known/oauth-authorization-server/mcp", "/.well-known/openid-configuration/mcp",
	} {
		mux.Handle(p, o.public(g, asm))
	}
	mux.Handle("/mcp/oauth/register", o.public(g, o.register))
	mux.Handle("/mcp/oauth/token", o.public(g, o.token))
	mux.Handle("/mcp/oauth/revoke", o.public(g, o.revoke))
	mux.HandleFunc("/mcp/oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		if !g.hostAllowed(r) {
			g.reject(w, r, http.StatusForbidden, "unexpected host "+r.Host)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Frame-Options", "DENY") // no clickjacking the page
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
		o.authorize(w, r, g.baseURL(r))
	})
}

// public wraps an endpoint that clients call directly, possibly from a
// browser: CORS is open (the endpoints are protected by client secrets,
// PKCE and one-time codes, not by origin), the Host is still checked.
func (o *OAuth) public(g *Guard, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, MCP-Protocol-Version")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if !g.hostAllowed(r) {
			g.reject(w, r, http.StatusForbidden, "unexpected host "+r.Host)
			return
		}
		h(w, r)
	})
}

// register implements dynamic client registration (RFC 7591).
func (o *OAuth) register(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !o.regLim.Allow() {
		oauthError(w, http.StatusTooManyRequests, "slow_down", "too many registrations")
		return
	}
	var req struct {
		RedirectURIs []string `json:"redirect_uris"`
		ClientName   string   `json:"client_name"`
		AuthMethod   string   `json:"token_endpoint_auth_method"`
		GrantTypes   []string `json:"grant_types"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "body must be JSON")
		return
	}
	if len(req.RedirectURIs) == 0 || len(req.RedirectURIs) > 10 {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "one to ten redirect_uris are required")
		return
	}
	for _, u := range req.RedirectURIs {
		if err := checkRedirectURI(u); err != nil {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", err.Error())
			return
		}
	}
	switch req.AuthMethod {
	case "none", "client_secret_post", "client_secret_basic":
	case "":
		req.AuthMethod = "client_secret_basic" // the RFC 7591 default
	default:
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "unsupported token_endpoint_auth_method")
		return
	}
	name := strings.TrimSpace(req.ClientName)
	if len([]rune(name)) > 80 {
		name = string([]rune(name)[:80])
	}
	if name == "" {
		name = "Unnamed client"
	}

	c := &oauthClient{
		ID: randomToken(16), AuthMethod: req.AuthMethod, Name: name,
		RedirectURIs: req.RedirectURIs, CreatedAt: o.now(),
	}
	var secret string
	if c.AuthMethod != "none" {
		secret = randomToken(32)
		c.SecretHash = hashToken(secret)
	}

	o.mu.Lock()
	o.pruneClientsLocked()
	if len(o.clients) >= maxClients {
		o.mu.Unlock()
		oauthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "too many registered clients")
		return
	}
	o.clients[c.ID] = c
	err := o.saveLocked()
	o.mu.Unlock()
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not save the client")
		return
	}

	resp := map[string]any{
		"client_id":                  c.ID,
		"client_id_issued_at":        c.CreatedAt.Unix(),
		"client_name":                c.Name,
		"redirect_uris":              c.RedirectURIs,
		"token_endpoint_auth_method": c.AuthMethod,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
	}
	if secret != "" {
		resp["client_secret"] = secret
		resp["client_secret_expires_at"] = 0
	}
	writeJSON(w, http.StatusCreated, resp)
}

// pruneClientsLocked forgets registrations that never led to a connection
// after a day. Caller holds o.mu.
func (o *OAuth) pruneClientsLocked() {
	used := map[string]bool{}
	for _, g := range o.grants {
		used[g.ClientID] = true
	}
	for id, c := range o.clients {
		if !used[id] && o.now().Sub(c.CreatedAt) > 24*time.Hour {
			delete(o.clients, id)
		}
	}
}

// authorize starts a consent request (first visit) or reports its outcome
// (the waiting page polls with ?request=<id>).
func (o *OAuth) authorize(w http.ResponseWriter, r *http.Request, issuer string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	if id := q.Get("request"); id != "" {
		o.poll(w, r, id)
		return
	}

	clientID, redirectURI := q.Get("client_id"), q.Get("redirect_uri")
	o.mu.Lock()
	c := o.clients[clientID]
	o.mu.Unlock()
	// Without a known client and redirect URI there is nowhere safe to send
	// an error, so it is shown here.
	if c == nil {
		renderPage(w, http.StatusBadRequest, page{Title: "Unknown app", Body: "This app is not registered with Nexora. Try connecting again from the app."})
		return
	}
	if !redirectAllowed(c.RedirectURIs, redirectURI) {
		renderPage(w, http.StatusBadRequest, page{Title: "Invalid redirect", Body: "The app asked to return to an address it did not register."})
		return
	}
	fail := func(code, desc string) {
		redirectWith(w, r, redirectURI, url.Values{"error": {code}, "error_description": {desc}, "state": {q.Get("state")}, "iss": {issuer}})
	}
	if q.Get("response_type") != "code" {
		fail("unsupported_response_type", "only response_type=code is supported")
		return
	}
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		fail("invalid_request", "PKCE with code_challenge_method=S256 is required")
		return
	}

	via := r.Host
	req := &ConsentRequest{
		ID: randomToken(16), ClientName: c.Name, RedirectHost: redirectHost(redirectURI),
		Code: pairingCode(), Via: via, CreatedAt: o.now(), ExpiresAt: o.now().Add(consentTTL),
		clientID: c.ID, redirectURI: redirectURI, state: q.Get("state"),
		challenge: q.Get("code_challenge"), issuer: issuer,
	}
	o.mu.Lock()
	o.expireLocked()
	if len(o.requests) >= 20 {
		o.mu.Unlock()
		fail("temporarily_unavailable", "too many pending sign-ins")
		return
	}
	o.requests[req.ID] = req
	o.mu.Unlock()
	o.changed()
	time.AfterFunc(consentTTL+time.Second, o.changed) // so the ask window notices it expired

	// Relative, so it resolves against the URL the browser used (e.g.
	// /nexora/oauth/authorize behind Tailscale). http.Redirect would make it
	// absolute from the local path (/mcp/...), which proxies don't forward.
	w.Header().Set("Location", "authorize?request="+req.ID)
	w.WriteHeader(http.StatusSeeOther)
}

func (o *OAuth) poll(w http.ResponseWriter, r *http.Request, id string) {
	o.mu.Lock()
	o.expireLocked()
	req := o.requests[id]
	if req == nil {
		o.mu.Unlock()
		renderPage(w, http.StatusNotFound, page{Title: "Request expired", Body: "This sign-in request expired or was already used. Start connecting again from the app."})
		return
	}
	decision := req.decision
	var code string
	if decision != consentPending {
		delete(o.requests, id)
		if decision == consentAllowed {
			code = randomToken(32)
			o.codes[hashToken(code)] = &authCode{
				clientID: req.clientID, redirectURI: req.redirectURI, challenge: req.challenge,
				via: req.Via, expires: o.now().Add(codeTTL),
			}
		}
	}
	o.mu.Unlock()

	switch decision {
	case consentAllowed:
		redirectWith(w, r, req.redirectURI, url.Values{"code": {code}, "state": {req.state}, "iss": {req.issuer}})
	case consentDenied:
		redirectWith(w, r, req.redirectURI, url.Values{"error": {"access_denied"}, "error_description": {"the user declined in Nexora"}, "state": {req.state}, "iss": {req.issuer}})
	default:
		renderPage(w, http.StatusOK, page{
			Title: "Approve in Nexora", Refresh: true, Code: req.Code,
			Body: fmt.Sprintf("%s wants to connect to Nexora. Open Nexora on your computer and allow it if the app shows the same code.", req.ClientName),
		})
	}
}

// token implements the authorization_code and refresh_token grants.
func (o *OAuth) token(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "body must be form-encoded")
		return
	}
	c, ok := o.authenticateClient(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="nexora"`)
		oauthError(w, http.StatusUnauthorized, "invalid_client", "unknown client or bad client credentials")
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		o.exchangeCode(w, r, c)
	case "refresh_token":
		o.refresh(w, r, c)
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "use authorization_code or refresh_token")
	}
}

func (o *OAuth) exchangeCode(w http.ResponseWriter, r *http.Request, c *oauthClient) {
	f := r.PostForm
	o.mu.Lock()
	defer o.mu.Unlock()
	h := hashToken(f.Get("code"))
	ac := o.codes[h]
	delete(o.codes, h) // one attempt per code
	switch {
	case ac == nil || o.now().After(ac.expires) || ac.clientID != c.ID:
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the code is invalid or expired")
		return
	case f.Get("redirect_uri") != "" && f.Get("redirect_uri") != ac.redirectURI:
		oauthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri does not match")
		return
	case !pkceMatches(f.Get("code_verifier"), ac.challenge):
		oauthError(w, http.StatusBadRequest, "invalid_grant", "code_verifier does not match")
		return
	}
	now := o.now()
	g := &grant{ID: randomToken(12), ClientID: c.ID, ClientName: c.Name, Via: ac.via, CreatedAt: now, LastUsed: now}
	o.grants[g.ID] = g
	o.issueLocked(w, g)
}

func (o *OAuth) refresh(w http.ResponseWriter, r *http.Request, c *oauthClient) {
	h := hashToken(r.PostForm.Get("refresh_token"))
	o.mu.Lock()
	defer o.mu.Unlock()
	now := o.now()
	var g *grant
	for _, cand := range o.grants {
		if cand.ClientID != c.ID {
			continue
		}
		if cand.RefreshHash == h || (cand.PrevRefreshHash == h && now.Sub(cand.RotatedAt) < refreshGrace) {
			g = cand
			break
		}
	}
	if g == nil || now.Sub(g.LastUsed) > refreshIdle {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token is invalid, expired or revoked")
		return
	}
	g.LastUsed = now
	o.issueLocked(w, g)
}

// issueLocked rotates g's refresh token, issues an access token and writes
// the token response. Caller holds o.mu.
func (o *OAuth) issueLocked(w http.ResponseWriter, g *grant) {
	access, refresh := randomToken(32), randomToken(32)
	now := o.now()
	o.access[hashToken(access)] = accessToken{GrantID: g.ID, Expires: now.Add(accessTTL)}
	if g.RefreshHash != "" {
		g.PrevRefreshHash, g.RotatedAt = g.RefreshHash, now
	}
	g.RefreshHash = hashToken(refresh)
	if err := o.saveLocked(); err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not save the token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int(accessTTL.Seconds()),
		"refresh_token": refresh,
	})
}

// revoke implements token revocation (RFC 7009): revoking either token of
// a connection disconnects it.
func (o *OAuth) revoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "body must be form-encoded")
		return
	}
	c, ok := o.authenticateClient(r)
	if !ok {
		oauthError(w, http.StatusUnauthorized, "invalid_client", "unknown client or bad client credentials")
		return
	}
	h := hashToken(r.PostForm.Get("token"))
	o.mu.Lock()
	grantID := ""
	if a, ok := o.access[h]; ok {
		grantID = a.GrantID
	}
	for _, g := range o.grants {
		if g.RefreshHash == h {
			grantID = g.ID
		}
	}
	g := o.grants[grantID]
	o.mu.Unlock()
	if g != nil && g.ClientID == c.ID {
		_ = o.Revoke(g.ID)
	}
	w.WriteHeader(http.StatusOK) // per RFC 7009, also for unknown tokens
}

// authenticateClient checks client_id and, for confidential clients, the
// secret (HTTP Basic or form).
func (o *OAuth) authenticateClient(r *http.Request) (*oauthClient, bool) {
	id, secret, basic := r.BasicAuth()
	if basic {
		// RFC 6749 §2.3.1: Basic credentials are form-encoded first.
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
	} else {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	o.mu.Lock()
	c := o.clients[id]
	o.mu.Unlock()
	if c == nil {
		return nil, false
	}
	if c.SecretHash == "" {
		return c, true
	}
	return c, subtle.ConstantTimeCompare([]byte(hashToken(secret)), []byte(c.SecretHash)) == 1
}

/* -------------------------------------------------------------- helpers */

// checkRedirectURI accepts https URLs, http on loopback (native apps) and
// private-use schemes such as cursor:// (RFC 8252).
func checkRedirectURI(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Fragment != "" {
		return fmt.Errorf("invalid redirect URI %q", raw)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "http":
		if isLoopback(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("redirect URI %q must use https", raw)
	case "javascript", "data", "file", "vbscript", "about", "blob":
		return fmt.Errorf("redirect URI scheme %q is not allowed", u.Scheme)
	}
	return nil
}

// redirectAllowed matches a registered redirect URI exactly, except that
// loopback URIs may use any port (RFC 8252 §7.3).
func redirectAllowed(registered []string, got string) bool {
	if got == "" {
		return len(registered) == 1 // the only one is implied
	}
	if slices.Contains(registered, got) {
		return true
	}
	g, err := url.Parse(got)
	if err != nil || g.Scheme != "http" || !isLoopback(g.Hostname()) {
		return false
	}
	for _, reg := range registered {
		r, err := url.Parse(reg)
		if err == nil && r.Scheme == "http" && r.Hostname() == g.Hostname() && r.Path == g.Path && r.RawQuery == g.RawQuery {
			return true
		}
	}
	return false
}

func redirectHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if u.Host == "" {
		return u.Scheme + "://"
	}
	if h, _, err := net.SplitHostPort(u.Host); err == nil && isLoopback(h) {
		return "an app on this computer"
	}
	return u.Hostname()
}

func redirectWith(w http.ResponseWriter, r *http.Request, target string, params url.Values) {
	u, err := url.Parse(target)
	if err != nil {
		http.Error(w, "bad redirect", http.StatusBadRequest)
		return
	}
	q := u.Query()
	for k, v := range params {
		if len(v) > 0 && v[0] != "" {
			q.Set(k, v[0])
		}
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func pkceMatches(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(challenge)) == 1
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// pairingCode is a short code like "K7M-Q2P", without look-alike characters.
func pairingCode() string {
	const alphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b[:3]) + "-" + string(b[3:])
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func oauthError(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

type page struct {
	Title, Body, Code string
	Refresh           bool
}

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
{{if .Refresh}}<meta http-equiv="refresh" content="2">{{end}}
<title>{{.Title}} · Nexora</title>
<style>
:root { --bg: #f6f6f8; --card: #fff; --fg: #16161c; --muted: #5d5d6b; --line: #e2e2e8; }
@media (prefers-color-scheme: dark) { :root { --bg: #0e0e12; --card: #17171d; --fg: #ececf1; --muted: #9a9aa8; --line: #2a2a33; } }
body { margin: 0; min-height: 100vh; display: grid; place-items: center; background: var(--bg); color: var(--fg);
  font: 15px/1.5 system-ui, -apple-system, "Segoe UI", sans-serif; padding: 16px; box-sizing: border-box; }
main { background: var(--card); border: 1px solid var(--line); border-radius: 14px; padding: 28px; max-width: 420px; width: 100%; box-sizing: border-box; }
.brand { font-weight: 600; letter-spacing: .02em; color: var(--muted); font-size: 13px; }
h1 { font-size: 20px; margin: 8px 0 12px; }
p { color: var(--muted); margin: 0; }
.code { font: 600 30px/1 ui-monospace, "SF Mono", Menlo, monospace; letter-spacing: .12em; text-align: center;
  padding: 18px; margin: 20px 0 16px; border: 1px dashed var(--line); border-radius: 10px; }
</style></head>
<body><main>
<div class="brand">NEXORA</div>
<h1>{{.Title}}</h1>
<p>{{.Body}}</p>
{{if .Code}}<div class="code">{{.Code}}</div><p>This page continues by itself once you answer.</p>{{end}}
</main></body></html>`))

func renderPage(w http.ResponseWriter, status int, p page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = pageTmpl.Execute(w, p)
}
