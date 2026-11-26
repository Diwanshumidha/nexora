package extension_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"nexora/internal/extension"
)

// fakeOAuth is an authorization server plus an MCP server that requires its
// tokens: enough of the MCP authorization spec to drive a full sign-in.
type fakeOAuth struct {
	as, rs *httptest.Server
	issued atomic.Int32 // access tokens issued
	mu     sync.Mutex
	valid  map[string]bool
}

func newFakeOAuth(t *testing.T) *fakeOAuth {
	f := &fakeOAuth{valid: map[string]bool{}}
	asMux := http.NewServeMux()
	f.as = httptest.NewServer(asMux)
	t.Cleanup(f.as.Close)
	asMux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                                f.as.URL,
			"authorization_endpoint":                f.as.URL + "/authorize",
			"token_endpoint":                        f.as.URL + "/token",
			"registration_endpoint":                 f.as.URL + "/register",
			"response_types_supported":              []string{"code"},
			"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
			"code_challenge_methods_supported":      []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"none"},
		})
	})
	asMux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var meta map[string]any
		_ = json.NewDecoder(r.Body).Decode(&meta)
		meta["client_id"] = "client-1"
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, meta)
	})
	asMux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		back, _ := url.Parse(q.Get("redirect_uri"))
		back.RawQuery = url.Values{"code": {"the-code"}, "state": {q.Get("state")}}.Encode()
		http.Redirect(w, r, back.String(), http.StatusFound)
	})
	asMux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("code") != "the-code" && r.Form.Get("refresh_token") != "refresh" {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]string{"error": "invalid_grant"})
			return
		}
		tok := "access-" + string(rune('0'+f.issued.Add(1)))
		f.mu.Lock()
		f.valid[tok] = true
		f.mu.Unlock()
		writeJSON(w, map[string]any{"access_token": tok, "token_type": "Bearer", "refresh_token": "refresh", "expires_in": 3600})
	})

	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return testMCPServer() }, nil)
	rsMux := http.NewServeMux()
	f.rs = httptest.NewServer(rsMux)
	t.Cleanup(f.rs.Close)
	rsMux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"resource": f.rs.URL + "/mcp", "authorization_servers": []string{f.as.URL}})
	})
	rsMux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		tok, _ := cutPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		ok := f.valid[tok]
		f.mu.Unlock()
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+f.rs.URL+`/.well-known/oauth-protected-resource"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mcpHandler.ServeHTTP(w, r)
	})
	return f
}

func (f *fakeOAuth) revokeAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.valid = map[string]bool{}
}

func cutPrefix(s, p string) (string, bool) {
	if len(s) >= len(p) && s[:len(p)] == p {
		return s[len(p):], true
	}
	return s, false
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// browser stands in for the user: it follows the authorization URL, which
// the fake server redirects straight back to Nexora's loopback callback.
func browser(opened *atomic.Int32) func(string) error {
	return func(u string) error {
		opened.Add(1)
		go func() {
			resp, err := http.Get(u)
			if err == nil {
				resp.Body.Close()
			}
		}()
		return nil
	}
}

func authOf(mgr *extension.Manager, id string) extension.AuthState {
	info, _ := mgr.Get(id)
	return info.Auth
}

func TestMCPOAuth(t *testing.T) {
	f := newFakeOAuth(t)
	mgr := newMCPManager(t, map[string]any{
		"id": "remote", "name": "Remote", "version": "1.0.0", "runtime": "mcp", "url": f.rs.URL + "/mcp",
	})
	ctx := context.Background()
	var opened atomic.Int32
	mgr.SetURLOpener(browser(&opened))

	// Starting on its own never opens the browser.
	if err := mgr.Start(ctx, "remote"); !errors.Is(err, extension.ErrSignInRequired) {
		t.Fatalf("Start = %v, want ErrSignInRequired", err)
	}
	if got := authOf(mgr, "remote"); got != extension.AuthRequired {
		t.Fatalf("auth = %q", got)
	}
	if opened.Load() != 0 {
		t.Fatal("opened the browser without a sign-in")
	}

	if err := mgr.SignIn(ctx, "remote"); err != nil {
		t.Fatal(err)
	}
	if got := authOf(mgr, "remote"); got != extension.AuthSignedIn {
		t.Fatalf("auth = %q", got)
	}
	if got := run(t, mgr, "remote", `return await tools.remote.get_greeting({ name: "oauth" })`); got != "Hello, oauth!" {
		t.Fatalf("got %#v", got)
	}

	// The token outlives a restart, and is private.
	if err := mgr.Stop(ctx, "remote"); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Start(ctx, "remote"); err != nil {
		t.Fatal(err)
	}
	if opened.Load() != 1 {
		t.Fatalf("browser opened %d times", opened.Load())
	}
	info, _ := mgr.Get("remote")
	stored := filepath.Join(filepath.Dir(filepath.Dir(info.Dir)), "extension-settings", "oauth", "remote.json")
	if st, err := os.Stat(stored); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("stored sign-in: %v %v", st, err)
	}

	// A revoked token flags the extension instead of opening the browser.
	f.revokeAll()
	if _, err := mgr.CallTool(ctx, "remote", "get_greeting", json.RawMessage(`{"name":"x"}`)); err == nil {
		t.Fatal("call succeeded with a revoked token")
	}
	if got := authOf(mgr, "remote"); got != extension.AuthRequired {
		t.Fatalf("auth after revoke = %q", got)
	}

	if err := mgr.SignOut(ctx, "remote"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stored); !os.IsNotExist(err) {
		t.Fatalf("sign-in kept after sign-out: %v", err)
	}
	if err := mgr.Start(ctx, "remote"); !errors.Is(err, extension.ErrSignInRequired) {
		t.Fatalf("Start after sign-out = %v", err)
	}
}

func TestMCPOAuthCancel(t *testing.T) {
	f := newFakeOAuth(t)
	mgr := newMCPManager(t, map[string]any{
		"id": "remote", "name": "Remote", "version": "1.0.0", "runtime": "mcp", "url": f.rs.URL + "/mcp",
	})
	// A user who never finishes in the browser.
	mgr.SetURLOpener(func(string) error { return nil })
	done := make(chan error, 1)
	go func() { done <- mgr.SignIn(context.Background(), "remote") }()

	deadline := time.Now().Add(5 * time.Second)
	for authOf(mgr, "remote") != extension.AuthPending {
		if time.Now().After(deadline) {
			t.Fatal("sign-in never became pending")
		}
		time.Sleep(10 * time.Millisecond)
	}
	mgr.CancelSignIn("remote")
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled sign-in succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CancelSignIn did not end the sign-in")
	}
	if got := authOf(mgr, "remote"); got != extension.AuthRequired {
		t.Fatalf("auth = %q", got)
	}
}
