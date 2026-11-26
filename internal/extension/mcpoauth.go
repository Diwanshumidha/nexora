package extension

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

// ErrSignInRequired means a remote MCP server wants an OAuth sign-in that
// only the user can start (Manager.SignIn).
var ErrSignInRequired = errors.New("sign-in required")

// signInTimeout bounds how long a sign-in waits for the user in the browser.
const signInTimeout = 5 * time.Minute

// AuthState is where a remote MCP server stands with OAuth.
type AuthState string

const (
	AuthNone     AuthState = ""          // no OAuth involved (yet)
	AuthRequired AuthState = "required"  // the server needs the user to sign in
	AuthPending  AuthState = "pending"   // waiting for the user in the browser
	AuthSignedIn AuthState = "signed-in" // connected with a stored token
)

type oauthOptions struct {
	ID    string
	Store *oauthStore
	// Interactive allows a browser sign-in while connecting. Otherwise, or
	// once connected, a server asking for authorization fails the request
	// with ErrSignInRequired, so Nexora never opens the browser on its own.
	Interactive bool
	OpenURL     func(string) error
	// OnSignInRequired is called when the server rejects the stored token
	// (or there is none) outside an interactive sign-in.
	OnSignInRequired func()
}

// oauthHandler implements auth.OAuthHandler for a remote MCP server. The
// SDK's AuthorizationCodeHandler does discovery, dynamic client
// registration, PKCE and the code exchange; this wraps it with a loopback
// redirect, token persistence and the interactive/background split.
type oauthHandler struct {
	opts        oauthOptions
	resource    string // the MCP server URL the token is for
	interactive atomic.Bool

	mu sync.Mutex
	ts oauth2.TokenSource
}

var _ auth.OAuthHandler = (*oauthHandler)(nil)

func newOAuthHandler(resource string, opts oauthOptions) (*oauthHandler, error) {
	h := &oauthHandler{opts: opts, resource: resource}
	h.interactive.Store(opts.Interactive)
	s, err := opts.Store.load(opts.ID)
	if err != nil {
		return nil, fmt.Errorf("read sign-in: %w", err)
	}
	// A token is only good for the server it was issued for.
	if s != nil && s.Token != nil && s.Resource == resource {
		h.ts = h.persisting(s.config(), s.Token, false)
	}
	return h, nil
}

func (h *oauthHandler) TokenSource(context.Context) (oauth2.TokenSource, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ts, nil
}

func (h *oauthHandler) signedIn() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ts != nil
}

// Authorize runs when the server answers 401 or 403.
func (h *oauthHandler) Authorize(ctx context.Context, req *http.Request, resp *http.Response) error {
	if !h.interactive.Load() {
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode == http.StatusForbidden && !insufficientScope(resp) {
			return fmt.Errorf("%s refused the request (403 Forbidden)", hostOf(h.resource))
		}
		if h.opts.OnSignInRequired != nil {
			h.opts.OnSignInRequired()
		}
		return ErrSignInRequired
	}
	if h.opts.OpenURL == nil {
		resp.Body.Close()
		return errors.New("no browser available to sign in")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		resp.Body.Close()
		return fmt.Errorf("listen for the sign-in redirect: %w", err)
	}
	defer ln.Close()
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port)

	inner, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{
			Metadata: &oauthex.ClientRegistrationMetadata{
				ClientName:              "Nexora",
				RedirectURIs:            []string{redirect},
				TokenEndpointAuthMethod: "none",
				GrantTypes:              []string{"authorization_code", "refresh_token"},
				ResponseTypes:           []string{"code"},
			},
		},
		RedirectURL: redirect,
		AuthorizationCodeFetcher: func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
			return awaitRedirect(ctx, ln, args.URL, h.opts.OpenURL)
		},
		RequestRefreshToken:   true,
		AcceptUnadvertisedIss: true,
		NewTokenSource: func(ctx context.Context, cfg *oauth2.Config, tok *oauth2.Token) (oauth2.TokenSource, error) {
			return h.persisting(cfg, tok, true), nil
		},
	})
	if err != nil {
		resp.Body.Close()
		return err
	}
	if err := inner.Authorize(ctx, req, resp); err != nil {
		return fmt.Errorf("sign in to %s: %w", hostOf(h.resource), err)
	}
	ts, _ := inner.TokenSource(ctx)
	h.mu.Lock()
	h.ts = ts
	h.mu.Unlock()
	return nil
}

// persisting returns a refreshing token source for cfg that saves each new
// token, so a restart doesn't need another sign-in.
func (h *oauthHandler) persisting(cfg *oauth2.Config, tok *oauth2.Token, isNew bool) oauth2.TokenSource {
	s := &storedOAuth{
		Resource: h.resource,
		ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret,
		AuthURL: cfg.Endpoint.AuthURL, TokenURL: cfg.Endpoint.TokenURL, AuthStyle: cfg.Endpoint.AuthStyle,
		Scopes: cfg.Scopes,
	}
	p := &persistingSource{
		base: cfg.TokenSource(context.Background(), tok),
		save: func(t *oauth2.Token) {
			s.Token = t
			_ = h.opts.Store.save(h.opts.ID, s)
		},
	}
	if isNew {
		p.save(tok)
	}
	p.last = tok.AccessToken
	return p
}

type persistingSource struct {
	base oauth2.TokenSource
	save func(*oauth2.Token)

	mu   sync.Mutex
	last string // access token last saved
}

func (p *persistingSource) Token() (*oauth2.Token, error) {
	t, err := p.base.Token()
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if t.AccessToken != p.last {
		p.last = t.AccessToken
		p.save(t)
	}
	return t, nil
}

// awaitRedirect opens the authorization URL in the browser and serves the
// redirect on ln until it arrives, ctx ends or signInTimeout passes.
func awaitRedirect(ctx context.Context, ln net.Listener, authURL string, openURL func(string) error) (*auth.AuthorizationResult, error) {
	type result struct {
		res *auth.AuthorizationResult
		err error
	}
	got := make(chan result, 1)
	srv := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/callback" {
				http.NotFound(w, r)
				return
			}
			q := r.URL.Query()
			var res result
			if e := q.Get("error"); e != "" {
				if d := q.Get("error_description"); d != "" {
					e += ": " + d
				}
				res.err = fmt.Errorf("the server declined the sign-in (%s)", e)
			} else {
				res.res = &auth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")}
			}
			writeRedirectPage(w, res.err)
			select {
			case got <- res:
			default:
			}
		}),
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		// Shutdown, not Close, so the page above reaches the browser.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	if err := openURL(authURL); err != nil {
		return nil, fmt.Errorf("open the browser: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, signInTimeout)
	defer cancel()
	select {
	case r := <-got:
		return r.res, r.err
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("sign-in was not completed within %s", signInTimeout)
		}
		return nil, errors.New("sign-in cancelled")
	}
}

func writeRedirectPage(w http.ResponseWriter, err error) {
	title, msg := "Signed in", "You can close this tab and return to Nexora."
	if err != nil {
		title, msg = "Sign-in failed", err.Error()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>Nexora</title>
<body style="font:16px system-ui,sans-serif;display:grid;place-items:center;height:90vh;margin:0">
<div style="text-align:center"><h1 style="font-size:20px">%s</h1><p>%s</p></div>`,
		html.EscapeString(title), html.EscapeString(msg))
}

func insufficientScope(resp *http.Response) bool {
	cs, err := oauthex.ParseWWWAuthenticate(resp.Header.Values("WWW-Authenticate"))
	if err != nil {
		return false
	}
	for _, c := range cs {
		if c.Params["error"] == "insufficient_scope" {
			return true
		}
	}
	return false
}

func hostOf(u string) string {
	if p, err := url.Parse(u); err == nil && p.Host != "" {
		return p.Host
	}
	return u
}

// storedOAuth is a server's registered client and current token.
type storedOAuth struct {
	Resource     string           `json:"resource"`
	ClientID     string           `json:"clientId"`
	ClientSecret string           `json:"clientSecret,omitempty"`
	AuthURL      string           `json:"authUrl,omitempty"`
	TokenURL     string           `json:"tokenUrl"`
	AuthStyle    oauth2.AuthStyle `json:"authStyle,omitempty"`
	Scopes       []string         `json:"scopes,omitempty"`
	Token        *oauth2.Token    `json:"token"`
}

func (s *storedOAuth) config() *oauth2.Config {
	return &oauth2.Config{
		ClientID: s.ClientID, ClientSecret: s.ClientSecret, Scopes: s.Scopes,
		Endpoint: oauth2.Endpoint{AuthURL: s.AuthURL, TokenURL: s.TokenURL, AuthStyle: s.AuthStyle},
	}
}

// oauthStore persists sign-ins, one 0600 JSON file per extension, beside
// (not in) the settings so they never show up as settings.
type oauthStore struct {
	dir string
	mu  sync.Mutex
}

func (s *oauthStore) path(id string) string { return filepath.Join(s.dir, id+".json") }

func (s *oauthStore) load(id string) (*storedOAuth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var v storedOAuth
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func (s *oauthStore) save(id string, v *storedOAuth) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	path := s.path(id)
	if err := os.WriteFile(path+".tmp", raw, 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func (s *oauthStore) remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
