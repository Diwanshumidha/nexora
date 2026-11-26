package mcp_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	nmcp "nexora/internal/mcp"
)

const redirectURI = "http://127.0.0.1:9/callback" // never contacted

var noRedirects = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func newOAuthEndpoint(t *testing.T) (string, *nmcp.OAuth, string, *nmcp.Guard) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mcp-oauth.json")
	oa, err := nmcp.OpenOAuth(path)
	if err != nil {
		t.Fatal(err)
	}
	base, guard := newOAuthGuardedEndpoint(t, time.Second, oa)
	return base, oa, path, guard
}

// browse plays the user's browser: it opens the authorization URL, answers
// the consent request in "the app" and returns where it was sent back to.
func browse(t *testing.T, oa *nmcp.OAuth, authURL string, allow bool) *url.URL {
	t.Helper()
	resp, err := noRedirects.Get(authURL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("authorize: got %d", resp.StatusCode)
	}
	// Relative: behind a path prefix (Tailscale's /nexora) an absolute
	// local path would leave the forwarded path.
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "authorize?request=") {
		t.Fatalf("waiting page redirect must be relative, got %q", loc)
	}
	waitURL, _ := resp.Request.URL.Parse(resp.Header.Get("Location"))

	// The waiting page shows the same code as the app.
	pending := oa.Pending()
	if len(pending) != 1 {
		t.Fatalf("pending: %+v", pending)
	}
	page := get(t, waitURL.String())
	if !strings.Contains(page, pending[0].Code) || !strings.Contains(page, "http-equiv=\"refresh\"") {
		t.Fatalf("waiting page lacks the code or refresh: %s", page)
	}
	if err := oa.Answer(pending[0].ID, allow); err != nil {
		t.Fatal(err)
	}

	resp, err = noRedirects.Get(waitURL.String())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("after answering: got %d", resp.StatusCode)
	}
	back, _ := url.Parse(resp.Header.Get("Location"))
	if !strings.HasPrefix(back.String(), redirectURI) {
		t.Fatalf("redirected to %s", back)
	}
	// The request is used up.
	if again, _ := noRedirects.Get(waitURL.String()); again.StatusCode != http.StatusNotFound {
		t.Fatalf("reused request: got %d", again.StatusCode)
	}
	return back
}

func get(t *testing.T, u string) string {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestOAuthWithSDKClient(t *testing.T) {
	base, oa, path, guard := newOAuthEndpoint(t)

	// A plain request to /mcp points at the discovery document.
	resp, err := http.Post(base+"/mcp", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if want := `resource_metadata="` + base + `/mcp/.well-known/oauth-protected-resource"`; resp.StatusCode != http.StatusUnauthorized ||
		!strings.Contains(resp.Header.Get("WWW-Authenticate"), want) {
		t.Fatalf("challenge: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}

	h, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{
			Metadata: &oauthex.ClientRegistrationMetadata{
				ClientName: "Test AI", RedirectURIs: []string{redirectURI},
				GrantTypes: []string{"authorization_code", "refresh_token"},
			},
		},
		RedirectURL: redirectURI,
		AuthorizationCodeFetcher: func(_ context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
			back := browse(t, oa, args.URL, true)
			q := back.Query()
			if q.Get("code") == "" {
				return nil, errors.New("no code: " + back.String())
			}
			return &auth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), &sdk.StreamableClientTransport{Endpoint: base + "/mcp", OAuthHandler: h}, nil)
	if err != nil {
		t.Fatalf("connect with OAuth: %v", err)
	}
	defer cs.Close()
	if _, err := cs.ListTools(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	// The UI can tell which app last got through.
	if last := guard.Stats().LastAccepted; last == nil || last.Client != "Test AI" || last.Public {
		t.Fatalf("last accepted: %+v", last)
	}

	conns := oa.Connections()
	if len(conns) != 1 || conns[0].ClientName != "Test AI" {
		t.Fatalf("connections: %+v", conns)
	}

	// The state survives a restart: the same token still works.
	ts, err := h.TokenSource(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tok, err := ts.Token()
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := nmcp.OpenOAuth(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.Valid(tok.AccessToken) {
		t.Fatal("access token lost on restart")
	}

	// Revoking disconnects it immediately.
	if err := oa.Revoke(conns[0].ID); err != nil {
		t.Fatal(err)
	}
	if oa.Valid(tok.AccessToken) {
		t.Fatal("revoked token still valid")
	}
}

// register is dynamic client registration by hand.
func register(t *testing.T, base string, body string) map[string]any {
	t.Helper()
	resp, err := http.Post(base+"/mcp/oauth/register", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: %d %v", resp.StatusCode, out)
	}
	return out
}

func tokenRequest(t *testing.T, base string, form url.Values) (int, map[string]any) {
	t.Helper()
	resp, err := http.PostForm(base+"/mcp/oauth/token", form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestOAuthCodeExchangeAndRefresh(t *testing.T) {
	base, oa, _, _ := newOAuthEndpoint(t)
	c := register(t, base, `{"client_name":"Hand","redirect_uris":["`+redirectURI+`"],"token_endpoint_auth_method":"client_secret_post"}`)
	id, secret := c["client_id"].(string), c["client_secret"].(string)
	if secret == "" {
		t.Fatal("confidential client got no secret")
	}

	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk-and-some-more"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	authURL := base + "/mcp/oauth/authorize?" + url.Values{
		"response_type": {"code"}, "client_id": {id}, "redirect_uri": {redirectURI}, "state": {"xyz"},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"},
	}.Encode()

	back := browse(t, oa, authURL, true)
	if back.Query().Get("state") != "xyz" || back.Query().Get("iss") != base+"/mcp" {
		t.Fatalf("redirect params: %s", back)
	}
	code := back.Query().Get("code")
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirectURI},
		"client_id": {id}, "client_secret": {secret}, "code_verifier": {"wrong-verifier-wrong-verifier-wrong-verifier-123"}}
	if st, out := tokenRequest(t, base, form); st != http.StatusBadRequest || out["error"] != "invalid_grant" {
		t.Fatalf("bad verifier: %d %v", st, out)
	}
	// A failed attempt burns the code.
	form.Set("code_verifier", verifier)
	if st, _ := tokenRequest(t, base, form); st != http.StatusBadRequest {
		t.Fatalf("burnt code accepted: %d", st)
	}

	// A fresh round with the right verifier.
	back = browse(t, oa, authURL, true)
	form.Set("code", back.Query().Get("code"))
	st, out := tokenRequest(t, base, form)
	if st != http.StatusOK {
		t.Fatalf("exchange: %d %v", st, out)
	}
	access, refresh := out["access_token"].(string), out["refresh_token"].(string)
	if !oa.Valid(access) {
		t.Fatal("access token not valid")
	}
	// The wrong secret is refused.
	if st, _ := tokenRequest(t, base, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh},
		"client_id": {id}, "client_secret": {"nope"}}); st != http.StatusUnauthorized {
		t.Fatalf("bad secret: %d", st)
	}
	st, out = tokenRequest(t, base, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh},
		"client_id": {id}, "client_secret": {secret}})
	if st != http.StatusOK || out["refresh_token"] == refresh {
		t.Fatalf("refresh: %d %v", st, out)
	}

	// The bearer token opens /mcp.
	req, _ := http.NewRequest(http.MethodPost, base+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Authorization", "Bearer "+out["access_token"].(string))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mcp with oauth token: %d", resp.StatusCode)
	}
}

func TestOAuthDeniedAndBadRequests(t *testing.T) {
	base, oa, _, _ := newOAuthEndpoint(t)
	c := register(t, base, `{"client_name":"Hand","redirect_uris":["`+redirectURI+`"],"token_endpoint_auth_method":"none"}`)
	if _, ok := c["client_secret"]; ok {
		t.Fatal("public client got a secret")
	}
	q := url.Values{"response_type": {"code"}, "client_id": {c["client_id"].(string)}, "redirect_uri": {redirectURI},
		"code_challenge": {"abc"}, "code_challenge_method": {"S256"}}

	back := browse(t, oa, base+"/mcp/oauth/authorize?"+q.Encode(), false)
	if back.Query().Get("error") != "access_denied" {
		t.Fatalf("deny: %s", back)
	}

	// An unregistered redirect is shown, never followed.
	bad := url.Values{}
	for k, v := range q {
		bad[k] = v
	}
	bad.Set("redirect_uri", "https://evil.example/cb")
	if resp, _ := noRedirects.Get(base + "/mcp/oauth/authorize?" + bad.Encode()); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("foreign redirect: %d", resp.StatusCode)
	}
	// PKCE is required.
	q.Del("code_challenge")
	resp, _ := noRedirects.Get(base + "/mcp/oauth/authorize?" + q.Encode())
	if loc, _ := url.Parse(resp.Header.Get("Location")); resp.StatusCode != http.StatusFound || loc.Query().Get("error") != "invalid_request" {
		t.Fatalf("no pkce: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	// Plain-http redirects off this machine are refused at registration.
	r, _ := http.Post(base+"/mcp/oauth/register", "application/json",
		strings.NewReader(`{"redirect_uris":["http://evil.example/cb"]}`))
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("http redirect registered: %d", r.StatusCode)
	}
	if len(oa.Pending()) != 0 {
		t.Fatalf("leftover requests: %+v", oa.Pending())
	}
}

func TestOAuthMetadataFollowsThePublicURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp-oauth.json")
	oa, _ := nmcp.OpenOAuth(path)
	base, guard := newOAuthGuardedEndpoint(t, time.Second, oa)
	const public = "https://laptop.tail1234.ts.net:8443/nexora"
	guard.SetPublicURL(func() string { return public })

	for _, p := range []string{"/mcp/.well-known/oauth-authorization-server", "/.well-known/oauth-authorization-server/mcp", "/mcp/.well-known/openid-configuration"} {
		req, _ := http.NewRequest(http.MethodGet, base+p, nil)
		req.Host = "laptop.tail1234.ts.net:8443"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		resp.Body.Close()
		if m["issuer"] != public || m["token_endpoint"] != public+"/oauth/token" {
			t.Fatalf("%s: %v", p, m)
		}
	}
	// Locally the issuer is the local URL.
	var prm map[string]any
	_ = json.Unmarshal([]byte(get(t, base+"/mcp/.well-known/oauth-protected-resource")), &prm)
	if prm["resource"] != base+"/mcp" {
		t.Fatalf("local resource: %v", prm)
	}
}
