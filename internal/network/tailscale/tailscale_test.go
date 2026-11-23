package tailscale

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
)

const host = "laptop.tail1234.ts.net"

// fakeDaemon is an in-memory tailscaled LocalAPI with ETag concurrency.
type fakeDaemon struct {
	mu        sync.Mutex
	state     string
	funnelCap bool
	sc        *ipn.ServeConfig
	version   int
	// conflicts makes the next N SetServeConfig calls fail with 412.
	conflicts int
	sets      int
}

func newDaemon() *fakeDaemon {
	return &fakeDaemon{state: "Running", funnelCap: true, sc: &ipn.ServeConfig{}}
}

func (d *fakeDaemon) StatusWithoutPeers(context.Context) (*ipnstate.Status, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	caps := tailcfg.NodeCapMap{tailcfg.CapabilityHTTPS: nil}
	if d.funnelCap {
		caps[tailcfg.NodeAttrFunnel] = nil
		caps[tailcfg.CapabilityFunnelPorts+"?ports=443,8443,10000"] = nil
	}
	return &ipnstate.Status{
		BackendState: d.state,
		CertDomains:  []string{host},
		Self:         &ipnstate.PeerStatus{DNSName: host + ".", CapMap: caps},
	}, nil
}

func (d *fakeDaemon) GetServeConfig(context.Context) (*ipn.ServeConfig, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	sc := clone(d.sc)
	sc.ETag = strconv.Itoa(d.version)
	return sc, nil
}

func (d *fakeDaemon) SetServeConfig(_ context.Context, sc *ipn.ServeConfig) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conflicts > 0 {
		d.conflicts--
		d.version++ // someone else wrote
		return &local.PreconditionsFailedError{}
	}
	if sc.ETag != strconv.Itoa(d.version) {
		return &local.PreconditionsFailedError{}
	}
	d.sc, d.version, d.sets = clone(sc), d.version+1, d.sets+1
	return nil
}

func (d *fakeDaemon) config() *ipn.ServeConfig {
	d.mu.Lock()
	defer d.mu.Unlock()
	return clone(d.sc)
}

func clone(sc *ipn.ServeConfig) *ipn.ServeConfig {
	raw, _ := json.Marshal(sc)
	out := new(ipn.ServeConfig)
	_ = json.Unmarshal(raw, out)
	return out
}

var target, _ = url.Parse("http://127.0.0.1:7377/mcp")

func opts(mode, port string) map[string]string {
	return map[string]string{"mode": mode, "port": port, "path": "/nexora"}
}

func TestFunnelExposeAndClose(t *testing.T) {
	d := newDaemon()
	p := NewWithClient(d)
	ctx := context.Background()

	ex, err := p.Expose(ctx, target, opts("funnel", "8443"))
	if err != nil {
		t.Fatal(err)
	}
	if ex.URL != "https://"+host+":8443/nexora" || !ex.Public {
		t.Fatalf("got %+v", ex)
	}
	sc := d.config()
	hp := ipn.HostPort(host + ":8443")
	if h := sc.Web[hp].Handlers["/nexora"]; h == nil || h.Proxy != target.String() {
		t.Fatalf("handler not set: %+v", sc.Web)
	}
	if !sc.AllowFunnel[hp] || !sc.TCP[8443].HTTPS {
		t.Fatalf("funnel/https not enabled: %+v", sc)
	}
	if err := p.Status(ctx); err != nil {
		t.Fatalf("status: %v", err)
	}

	if err := p.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if sc := d.config(); len(sc.Web) != 0 || len(sc.AllowFunnel) != 0 || len(sc.TCP) != 0 {
		t.Fatalf("close should restore an empty config, got %+v", sc)
	}
	if err := p.Close(ctx); err != nil {
		t.Fatal("close must be idempotent")
	}
}

func TestTailnetModeOn443(t *testing.T) {
	p := NewWithClient(newDaemon())
	ex, err := p.Expose(context.Background(), target, opts("tailnet", "443"))
	if err != nil {
		t.Fatal(err)
	}
	if ex.URL != "https://"+host+"/nexora" || ex.Public {
		t.Fatalf("got %+v", ex)
	}
}

func TestPreservesOtherHandlers(t *testing.T) {
	d := newDaemon()
	d.sc.SetWebHandler(&ipn.HTTPHandler{Proxy: "http://127.0.0.1:3000"}, host, 443, "/", true, "")
	p := NewWithClient(d)
	ctx := context.Background()

	// Tailnet mode can share the port with the user's private app.
	if _, err := p.Expose(ctx, target, opts("tailnet", "443")); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if h := d.config().Web[ipn.HostPort(host+":443")].Handlers["/"]; h == nil || h.Proxy != "http://127.0.0.1:3000" {
		t.Fatal("the user's own handler must survive expose and close")
	}

	// Funnel on that port would publish the private app: refused, nothing changed.
	before := d.sets
	if _, err := p.Expose(ctx, target, opts("funnel", "443")); err == nil || !strings.Contains(err.Error(), "Funnel would make it public") {
		t.Fatalf("expected a refusal, got %v", err)
	}
	if d.sets != before {
		t.Fatal("a refused expose must not write config")
	}
}

func TestConflicts(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		setup func(sc *ipn.ServeConfig)
		mode  string
		want  string
	}{
		"path taken": {func(sc *ipn.ServeConfig) {
			sc.SetWebHandler(&ipn.HTTPHandler{Proxy: "http://127.0.0.1:9999"}, host, 8443, "/nexora", true, "")
		}, "tailnet", "already used"},
		"tailnet on public port": {func(sc *ipn.ServeConfig) {
			sc.SetWebHandler(&ipn.HTTPHandler{Proxy: "http://127.0.0.1:3000"}, host, 8443, "/blog", true, "")
			sc.SetFunnel(host, 8443, true)
		}, "tailnet", "publicly funnelled"},
		"tcp forward": {func(sc *ipn.ServeConfig) {
			sc.TCP = map[uint16]*ipn.TCPPortHandler{8443: {TCPForward: "127.0.0.1:22"}}
		}, "funnel", "forwards TCP"},
	} {
		t.Run(name, func(t *testing.T) {
			d := newDaemon()
			tc.setup(d.sc)
			_, err := NewWithClient(d).Expose(ctx, target, opts(tc.mode, "8443"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
		})
	}
}

func TestLeftoverHandlerFromCrashIsReused(t *testing.T) {
	d := newDaemon()
	d.sc.SetWebHandler(&ipn.HTTPHandler{Proxy: target.String()}, host, 8443, "/nexora", true, "")
	d.sc.SetFunnel(host, 8443, true)
	if _, err := NewWithClient(d).Expose(context.Background(), target, opts("funnel", "8443")); err != nil {
		t.Fatalf("our own stale handler should be overwritten: %v", err)
	}
}

func TestRetriesOnConcurrentEdit(t *testing.T) {
	d := newDaemon()
	d.conflicts = 2
	if _, err := NewWithClient(d).Expose(context.Background(), target, opts("funnel", "8443")); err != nil {
		t.Fatalf("should retry after 412s: %v", err)
	}
	d.conflicts = 5
	if _, err := NewWithClient(d).Expose(context.Background(), target, opts("funnel", "10000")); !local.IsPreconditionsFailedError(err) {
		t.Fatalf("should give up after 3 attempts, got %v", err)
	}
}

func TestPreconditions(t *testing.T) {
	ctx := context.Background()

	d := newDaemon()
	d.funnelCap = false
	if _, err := NewWithClient(d).Expose(ctx, target, opts("funnel", "8443")); err == nil || !strings.Contains(err.Error(), "funnel") {
		t.Fatalf("expected a funnel capability error, got %v", err)
	}

	d = newDaemon()
	d.state = "Stopped"
	p := NewWithClient(d)
	if err := p.Available(ctx); err == nil || !strings.Contains(err.Error(), "tailscale up") {
		t.Fatalf("got %v", err)
	}

	for _, bad := range []map[string]string{
		{"mode": "public", "port": "8443", "path": "/x"},
		{"mode": "funnel", "port": "99999", "path": "/x"},
		{"mode": "funnel", "port": "8443", "path": "/"},
	} {
		if _, err := NewWithClient(newDaemon()).Expose(ctx, target, bad); err == nil {
			t.Errorf("options %v should be rejected", bad)
		}
	}
}

func TestStatusDetectsExternalChanges(t *testing.T) {
	d := newDaemon()
	p := NewWithClient(d)
	ctx := context.Background()
	if _, err := p.Expose(ctx, target, opts("funnel", "8443")); err != nil {
		t.Fatal(err)
	}
	// e.g. the user ran `tailscale funnel reset`
	d.mu.Lock()
	d.sc = &ipn.ServeConfig{}
	d.mu.Unlock()
	if err := p.Status(ctx); err == nil {
		t.Fatal("status should report the lost exposure")
	}
}

func TestAccessDeniedHint(t *testing.T) {
	err := explain(&local.AccessDeniedError{})
	if !strings.Contains(err.Error(), "--operator=$USER") || !errors.As(err, new(*local.AccessDeniedError)) {
		t.Fatalf("got %v", err)
	}
}
