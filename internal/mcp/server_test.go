package mcp_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"nexora/internal/config"
	"nexora/internal/extension"
	"nexora/internal/jobs"
	nmcp "nexora/internal/mcp"
	"nexora/internal/permissions"
	"nexora/internal/runner"
)

const token = "test-token"

func newEndpoint(t *testing.T, inlineWait time.Duration) string {
	url, _ := newGuardedEndpoint(t, inlineWait)
	return url
}

func newGuardedEndpoint(t *testing.T, inlineWait time.Duration) (string, *nmcp.Guard) {
	t.Helper()
	dir := t.TempDir()
	mgr := extension.NewManager(&config.Config{ExtensionsDir: dir, ExtensionDataDir: dir},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	queue := jobs.New(jobs.Options{Workers: 2})
	t.Cleanup(queue.Close)
	perms, err := permissions.Open(filepath.Join(dir, "permissions.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := nmcp.NewServer(runner.New(mgr, queue, perms, time.Minute), nmcp.Options{InlineWait: inlineWait})
	tokenFile := filepath.Join(dir, "mcp-token")
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	guard, err := nmcp.OpenGuard(tokenFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(nmcp.NewHandler(srv, guard))
	t.Cleanup(ts.Close)
	return ts.URL, guard
}

func connect(t *testing.T, url string) *sdk.ClientSession {
	t.Helper()
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), &sdk.StreamableClientTransport{Endpoint: url}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, cs *sdk.ClientSession, tool string, args any) (nmcp.JobOutput, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	var out nmcp.JobOutput
	raw, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return out, res.IsError
}

// busy blocks the script for ms milliseconds (code mode has no timers).
func busy(ms int) string {
	return `const t = Date.now(); while (Date.now() - t < ` + itoa(ms) + `) {}`
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestRunCodeInline(t *testing.T) {
	cs := connect(t, newEndpoint(t, 2*time.Second)+"/mcp/"+token)
	out, isErr := call(t, cs, "run_code", map[string]any{"code": `console.log("hi"); return 6 * 7`})
	if isErr || out.Status != "succeeded" || out.Result != float64(42) || len(out.Logs) != 1 {
		t.Fatalf("got %+v (isError=%v)", out, isErr)
	}
}

func TestRunCodeFallsBackToJob(t *testing.T) {
	cs := connect(t, newEndpoint(t, 200*time.Millisecond)+"/mcp/"+token)

	out, _ := call(t, cs, "run_code", map[string]any{"code": busy(700) + "; return 'slow done'"})
	if out.Status != "running" || out.JobID == "" || !strings.Contains(out.Next, "get_job") {
		t.Fatalf("expected a running job, got %+v", out)
	}

	deadline := time.Now().Add(5 * time.Second)
	for out.Status == "running" || out.Status == "queued" {
		if time.Now().After(deadline) {
			t.Fatal("job did not finish")
		}
		out, _ = call(t, cs, "get_job", map[string]any{"jobId": out.JobID})
	}
	if out.Status != "succeeded" || out.Result != "slow done" {
		t.Fatalf("got %+v", out)
	}
}

func TestScriptErrorAndCancel(t *testing.T) {
	cs := connect(t, newEndpoint(t, 200*time.Millisecond)+"/mcp/"+token)

	out, isErr := call(t, cs, "run_code", map[string]any{"code": `throw new Error("bad input")`})
	if !isErr || out.Status != "failed" || !strings.Contains(out.Error, "bad input") {
		t.Fatalf("got %+v (isError=%v)", out, isErr)
	}

	out, _ = call(t, cs, "run_code", map[string]any{"code": busy(60000)})
	out, _ = call(t, cs, "cancel_job", map[string]any{"jobId": out.JobID})
	out, _ = call(t, cs, "get_job", map[string]any{"jobId": out.JobID, "waitSeconds": 1})
	if out.Status != "cancelled" {
		t.Fatalf("expected cancelled, got %+v", out)
	}
}

func TestAuth(t *testing.T) {
	base := newEndpoint(t, time.Second)
	for name, req := range map[string]*http.Request{
		"no token":    mustReq(base+"/mcp", ""),
		"wrong token": mustReq(base+"/mcp/nope", ""),
		"bad bearer":  mustReq(base+"/mcp", "Bearer nope"),
	} {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status %d", name, resp.StatusCode)
		}
	}

	// Bearer header works as an alternative to the path token.
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil)
	httpClient := &http.Client{Transport: bearer{token}}
	cs, err := client.Connect(context.Background(), &sdk.StreamableClientTransport{Endpoint: base + "/mcp", HTTPClient: httpClient}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs.Close()
}

func mustReq(url, auth string) *http.Request {
	req, _ := http.NewRequest("POST", url, strings.NewReader(`{}`))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	return req
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func status(t *testing.T, req *http.Request) int {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestRejectsBrowserOrigins(t *testing.T) {
	base, guard := newGuardedEndpoint(t, time.Second)
	// A web page (or a DNS-rebinding attack) sends an Origin header; even
	// with the right token it is refused.
	req := mustReq(base+"/mcp/"+token, "")
	req.Header.Set("Origin", "https://evil.example")
	if got := status(t, req); got != http.StatusForbidden {
		t.Fatalf("got %d", got)
	}
	if st := guard.Stats(); st.OriginsBlocked != 1 || st.Last == nil || !strings.Contains(st.Last.Reason, "evil.example") {
		t.Fatalf("stats: %+v", st)
	}
}

func TestThrottlesBadTokensButNotGoodOnes(t *testing.T) {
	base, guard := newGuardedEndpoint(t, time.Second)
	codes := map[int]int{}
	for range 40 {
		codes[status(t, mustReq(base+"/mcp/wrong", ""))]++
	}
	if codes[http.StatusTooManyRequests] == 0 || codes[http.StatusUnauthorized] == 0 {
		t.Fatalf("expected 401s then 429s, got %v", codes)
	}
	cs := connect(t, base+"/mcp/"+token) // the real client still gets in
	if _, err := cs.ListTools(context.Background(), nil); err != nil {
		t.Fatalf("valid token throttled: %v", err)
	}
	if guard.Stats().Rejected != 40 {
		t.Fatalf("stats: %+v", guard.Stats())
	}
}

func TestRotateToken(t *testing.T) {
	base, guard := newGuardedEndpoint(t, time.Second)
	fresh, err := guard.Rotate()
	if err != nil || fresh == token || len(fresh) != 64 {
		t.Fatalf("rotate: %q %v", fresh, err)
	}
	if got := status(t, mustReq(base+"/mcp/"+token, "")); got != http.StatusUnauthorized {
		t.Fatalf("old token should stop working, got %d", got)
	}
	cs := connect(t, base+"/mcp/"+fresh)
	if _, err := cs.ListTools(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}

// hostRT sends every request with a fixed Host header, like a proxy that
// forwards the public hostname (Tailscale Funnel, cloudflared).
type hostRT struct{ host string }

func (h hostRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Host = h.host
	return http.DefaultTransport.RoundTrip(r)
}

func TestHostCheckFollowsTheLiveProvider(t *testing.T) {
	base, guard := newGuardedEndpoint(t, time.Second)
	const public = "laptop.tail1234.ts.net:8443"
	withHost := func(host string) *http.Request {
		req := mustReq(base+"/mcp/"+token, "")
		req.Host = host
		return req
	}

	// Nothing exposed: a public Host is DNS rebinding as far as we know.
	if got := status(t, withHost(public)); got != http.StatusForbidden {
		t.Fatalf("unexposed public host: got %d", got)
	}

	live := public
	guard.SetPublicHost(func() string { return live })
	if got := status(t, withHost("evil.example")); got != http.StatusForbidden {
		t.Fatalf("other hosts stay blocked: got %d", got)
	}

	// The provider's host works end to end with a real MCP client.
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), &sdk.StreamableClientTransport{
		Endpoint: base + "/mcp/" + token, HTTPClient: &http.Client{Transport: hostRT{public}},
	}, nil)
	if err != nil {
		t.Fatalf("public host refused: %v", err)
	}
	if _, err := cs.ListTools(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	cs.Close()

	// Port 443 may be implied either way.
	live = "laptop.tail1234.ts.net"
	if got := status(t, withHost("laptop.tail1234.ts.net:443")); got == http.StatusForbidden {
		t.Fatal(":443 should match a host without a port")
	}

	// Provider turned off: its host is refused again.
	live = ""
	if got := status(t, withHost(public)); got != http.StatusForbidden {
		t.Fatalf("after turning the provider off: got %d", got)
	}
	if !strings.Contains(guard.Stats().Last.Reason, "unexpected host") {
		t.Fatalf("stats: %+v", guard.Stats().Last)
	}
}

func TestAcceptsHostedAIClients(t *testing.T) {
	base, _ := newGuardedEndpoint(t, time.Second)
	// ChatGPT's connector may send its web app's Origin; with the token it
	// gets through, as does a URL pasted with a trailing slash.
	req := mustReq(base+"/mcp/"+token, "")
	req.Header.Set("Origin", "https://chatgpt.com")
	if got := status(t, req); got == http.StatusForbidden || got == http.StatusUnauthorized {
		t.Fatalf("chatgpt origin: got %d", got)
	}
	if got := status(t, mustReq(base+"/mcp/"+token+"/", "")); got == http.StatusNotFound || got == http.StatusUnauthorized {
		t.Fatalf("trailing slash: got %d", got)
	}
	// Without the token it is still refused.
	req = mustReq(base+"/mcp/nope", "")
	req.Header.Set("Origin", "https://chatgpt.com")
	if got := status(t, req); got != http.StatusUnauthorized {
		t.Fatalf("no token: got %d", got)
	}
}
