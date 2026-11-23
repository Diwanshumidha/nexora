package mcp

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Guard holds the endpoint's secret token and the request checks around it.
// The token can be rotated at runtime; old URLs stop working immediately.
type Guard struct {
	path string

	mu    sync.RWMutex
	token string

	allowedOrigins map[string]bool

	// publicHost returns the live network provider's public host (e.g.
	// "laptop.tailnet.ts.net:8443"), or "" when nothing is exposed.
	publicHost atomic.Pointer[func() string]

	// limiter throttles failed attempts (requests with a wrong token).
	// Behind a tunnel every client shares one address, so it is global:
	// valid tokens are never throttled, only failures.
	limiter *rate.Limiter

	rejected    atomic.Int64
	lastReject  atomic.Pointer[RejectInfo]
	originBlock atomic.Int64
}

// RejectInfo describes the latest refused request, for the UI.
type RejectInfo struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
	Remote string    `json:"remote"`
}

// GuardStats summarises refused requests since start.
type GuardStats struct {
	Rejected       int64       `json:"rejected"`
	OriginsBlocked int64       `json:"originsBlocked"`
	Last           *RejectInfo `json:"last,omitempty"`
}

// aiClientOrigins are the web apps of hosted AI clients. Their connectors
// call from servers but may still send an Origin header; they always need the
// token too, so allowing them doesn't weaken the drive-by protection.
var aiClientOrigins = []string{"https://chatgpt.com", "https://chat.openai.com", "https://claude.ai"}

// OpenGuard loads the token from path, creating one on first use.
// allowedOrigins lists browser origins allowed to call the endpoint (normally
// none: MCP clients are not web pages).
func OpenGuard(path string, allowedOrigins []string) (*Guard, error) {
	g := &Guard{
		path:           path,
		allowedOrigins: map[string]bool{},
		limiter:        rate.NewLimiter(rate.Every(6*time.Second), 20), // ~10 failures/min after a burst
	}
	for _, o := range append(append([]string{}, aiClientOrigins...), allowedOrigins...) {
		if o = strings.TrimSpace(o); o != "" {
			g.allowedOrigins[strings.TrimSuffix(o, "/")] = true
		}
	}
	tok, err := loadOrCreateToken(path)
	if err != nil {
		return nil, err
	}
	g.token = tok
	return g, nil
}

// SetPublicHost tells the guard which non-local Host header to accept: the
// one the live network provider exposes. Proxies such as Tailscale Funnel
// and cloudflared forward the public Host unchanged.
func (g *Guard) SetPublicHost(fn func() string) { g.publicHost.Store(&fn) }

func (g *Guard) Token() string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.token
}

// Rotate replaces the token with a new random one and persists it.
func (g *Guard) Rotate() (string, error) {
	tok, err := newToken()
	if err != nil {
		return "", err
	}
	if err := writeToken(g.path, tok); err != nil {
		return "", err
	}
	g.mu.Lock()
	g.token = tok
	g.mu.Unlock()
	return tok, nil
}

func (g *Guard) Stats() GuardStats {
	return GuardStats{Rejected: g.rejected.Load(), OriginsBlocked: g.originBlock.Load(), Last: g.lastReject.Load()}
}

func (g *Guard) reject(w http.ResponseWriter, r *http.Request, status int, reason string) {
	g.rejected.Add(1)
	g.lastReject.Store(&RejectInfo{At: time.Now(), Reason: reason, Remote: r.RemoteAddr})
	http.Error(w, http.StatusText(status), status)
}

// check authorises a request. It refuses browser-originated requests (DNS
// rebinding, drive-by pages), throttles repeated bad tokens and compares
// tokens in constant time.
func (g *Guard) check(w http.ResponseWriter, r *http.Request, provided string) bool {
	if origin := r.Header.Get("Origin"); origin != "" && !g.allowedOrigins[strings.TrimSuffix(origin, "/")] {
		g.originBlock.Add(1)
		g.reject(w, r, http.StatusForbidden, "browser origin "+origin)
		return false
	}
	if !g.hostAllowed(r) {
		g.reject(w, r, http.StatusForbidden, "unexpected host "+r.Host)
		return false
	}
	if tokenMatches(provided, g.Token()) {
		return true
	}
	if !g.limiter.Allow() {
		g.reject(w, r, http.StatusTooManyRequests, "too many bad tokens")
		return false
	}
	g.reject(w, r, http.StatusUnauthorized, "bad or missing token")
	return false
}

// hostAllowed is DNS-rebinding protection that understands the network
// layer: when listening on loopback, the Host must be local or exactly the
// live provider's public host. (Listening on another interface is an
// explicit choice to serve that network, so any Host is accepted there.)
func (g *Guard) hostAllowed(r *http.Request) bool {
	local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok || local == nil || !isLoopback(hostOnly(local.String())) {
		return true
	}
	if isLoopback(hostOnly(r.Host)) {
		return true
	}
	if fn := g.publicHost.Load(); fn != nil {
		if pub := (*fn)(); pub != "" && sameHost(pub, r.Host) {
			return true
		}
	}
	return false
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

func isLoopback(host string) bool {
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// sameHost compares hosts case-insensitively, treating ":443" as implied.
func sameHost(a, b string) bool {
	norm := func(h string) string { return strings.TrimSuffix(strings.ToLower(h), ":443") }
	return norm(a) == norm(b)
}

// NewHandler serves srv over streamable HTTP at /mcp, guarded by g.
//
// The token is accepted either as "Authorization: Bearer <token>" or as a
// secret path segment (/mcp/<token>), because some AI clients (e.g. ChatGPT
// connectors) only take a URL. Anyone with the token can run code against
// the user's extensions, so treat the URL like a password.
func NewHandler(srv *mcp.Server, g *Guard) http.Handler {
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{
			// Stateless: no session affinity is needed, so requests survive
			// proxy reconnects and load balancing. Job state lives in the queue.
			Stateless: true,
			// The SDK's own DNS-rebinding check rejects every non-local Host,
			// which breaks Tailscale Funnel and tunnels. Guard.hostAllowed does
			// the same check but also accepts the live provider's public host.
			DisableLocalhostProtection: true,
		})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	authed := func(w http.ResponseWriter, r *http.Request, provided string) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if !g.check(w, r, provided) {
			return
		}
		mcpHandler.ServeHTTP(w, r)
	}
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		authed(w, r, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	})
	byPath := func(w http.ResponseWriter, r *http.Request) { authed(w, r, r.PathValue("token")) }
	mux.HandleFunc("/mcp/{token}", byPath)
	mux.HandleFunc("/mcp/{token}/", byPath) // URLs pasted with a trailing slash
	return mux
}

func tokenMatches(provided, want string) bool {
	return want != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(want)) == 1
}

// HTTPServer runs the MCP endpoint in the background.
type HTTPServer struct {
	srv *http.Server
	ln  net.Listener
	log *slog.Logger
}

// Listen binds addr (e.g. "127.0.0.1:7377") and starts serving.
func Listen(addr string, handler http.Handler, log *slog.Logger) (*HTTPServer, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("mcp listen on %s: %w", addr, err)
	}
	s := &HTTPServer{
		ln:  ln,
		log: log,
		srv: &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
			// No WriteTimeout: tool calls legitimately block for up to the
			// inline wait. IdleTimeout reclaims abandoned keep-alives.
			IdleTimeout: 2 * time.Minute,
		},
	}
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("mcp server stopped", "err", err)
		}
	}()
	return s, nil
}

// Addr is the bound address (useful when listening on port 0).
func (s *HTTPServer) Addr() string { return s.ln.Addr().String() }

func (s *HTTPServer) Shutdown(ctx context.Context) error { return s.srv.Shutdown(ctx) }

func loadOrCreateToken(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		if tok := strings.TrimSpace(string(b)); tok != "" {
			return tok, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	tok, err := newToken()
	if err != nil {
		return "", err
	}
	return tok, writeToken(path, tok)
}

func newToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func writeToken(path, tok string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(tok+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
