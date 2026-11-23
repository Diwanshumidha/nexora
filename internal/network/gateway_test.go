package network

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeProvider records calls and tracks whether it is exposed.
type fakeProvider struct {
	id string

	mu        sync.Mutex
	exposed   bool
	exposes   int
	lastOpts  map[string]string
	failNext  error
	healthErr error
}

func (f *fakeProvider) Info() ProviderInfo {
	return ProviderInfo{ID: f.id, Name: f.id, Settings: []Setting{{Key: "mode", Choices: []string{"a", "b"}, Default: "a"}}}
}

func (f *fakeProvider) Available(context.Context) error { return nil }

func (f *fakeProvider) Expose(_ context.Context, target *url.URL, o map[string]string) (Exposure, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return Exposure{}, err
	}
	f.exposed, f.lastOpts, f.healthErr = true, o, nil
	f.exposes++
	return Exposure{URL: "https://" + f.id + ".example" + target.Path, Public: true}, nil
}

func (f *fakeProvider) Status(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.healthErr
}

func (f *fakeProvider) Close(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exposed = false
	return nil
}

func (f *fakeProvider) is(fn func(*fakeProvider) bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fn(f)
}

func newGateway(t *testing.T, store string, ps ...Provider) *Gateway {
	t.Helper()
	g, err := New(ps, Options{
		Target:        "http://127.0.0.1:7377/mcp",
		StorePath:     store,
		CheckInterval: 10 * time.Millisecond,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func active(g *Gateway) *Endpoint {
	for _, ep := range g.List(context.Background()) {
		if ep.Active {
			return &ep
		}
	}
	return nil
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestOnlyOneProviderLive(t *testing.T) {
	ts, cf := &fakeProvider{id: "tailscale"}, &fakeProvider{id: "cloudflare"}
	g := newGateway(t, filepath.Join(t.TempDir(), "n.json"), ts, cf)
	ctx := context.Background()

	ep, err := g.Activate(ctx, "tailscale", map[string]string{"mode": "b", "junk": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if ep.State != StateUp || ep.Exposure.URL != "https://tailscale.example/mcp" {
		t.Fatalf("got %+v", ep)
	}
	if ts.lastOpts["mode"] != "b" || ts.lastOpts["junk"] != "" {
		t.Fatalf("options should be filtered to known settings: %v", ts.lastOpts)
	}

	// Switching closes the previous provider first.
	if _, err := g.Activate(ctx, "cloudflare", nil); err != nil {
		t.Fatal(err)
	}
	if ts.exposed || !cf.exposed {
		t.Fatalf("exactly one provider must be live: tailscale=%v cloudflare=%v", ts.exposed, cf.exposed)
	}
	n := 0
	for _, ep := range g.List(ctx) {
		if ep.Active {
			n++
		}
	}
	if n != 1 || active(g).ID != "cloudflare" {
		t.Fatal("exactly one provider must be marked active")
	}

	if err := g.Deactivate(); err != nil {
		t.Fatal(err)
	}
	if cf.exposed || active(g) != nil {
		t.Fatal("deactivate should close and clear the active provider")
	}
}

func TestRestoreOnRestartAndCloseOnShutdown(t *testing.T) {
	store := filepath.Join(t.TempDir(), "n.json")
	ts := &fakeProvider{id: "tailscale"}
	g := newGateway(t, store, ts)
	if _, err := g.Activate(context.Background(), "tailscale", map[string]string{"mode": "b"}); err != nil {
		t.Fatal(err)
	}
	g.Close()
	if ts.exposed || active(g) == nil {
		t.Fatal("shutdown should close the exposure but keep the provider selected")
	}

	ts2 := &fakeProvider{id: "tailscale"}
	g2 := newGateway(t, store, ts2)
	g2.Start()
	defer g2.Close()
	eventually(t, func() bool { return ts2.is(func(f *fakeProvider) bool { return f.exposed }) })
	if ts2.lastOpts["mode"] != "b" {
		t.Fatalf("options not restored: %v", ts2.lastOpts)
	}
}

func TestReexposesWhenHealthCheckFails(t *testing.T) {
	ts := &fakeProvider{id: "tailscale"}
	g := newGateway(t, filepath.Join(t.TempDir(), "n.json"), ts)
	g.Start()
	defer g.Close()
	if _, err := g.Activate(context.Background(), "tailscale", nil); err != nil {
		t.Fatal(err)
	}
	ts.mu.Lock()
	ts.healthErr = errors.New("funnel reset outside nexora")
	ts.mu.Unlock()
	eventually(t, func() bool { return ts.is(func(f *fakeProvider) bool { return f.exposes >= 2 }) })
	eventually(t, func() bool { return active(g).State == StateUp })
}

func TestFailureIsReportedWithBackoff(t *testing.T) {
	ts := &fakeProvider{id: "tailscale", failNext: errors.New("funnel not enabled")}
	g := newGateway(t, filepath.Join(t.TempDir(), "n.json"), ts)
	ep, err := g.Activate(context.Background(), "tailscale", nil)
	if err != nil {
		t.Fatal(err)
	}
	if ep.State != StateDown || ep.Error != "funnel not enabled" {
		t.Fatalf("got %+v", ep)
	}
	// The background loop waits for the backoff; an explicit activate retries now.
	g.Start()
	defer g.Close()
	time.Sleep(50 * time.Millisecond)
	if ts.is(func(f *fakeProvider) bool { return f.exposes > 0 }) {
		t.Fatal("should not retry before the backoff expires")
	}
	if ep, _ := g.Activate(context.Background(), "tailscale", nil); ep.State != StateUp {
		t.Fatalf("got %+v", ep)
	}
}

func TestBackoff(t *testing.T) {
	want := []time.Duration{20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 5 * time.Minute, 5 * time.Minute}
	for i, w := range want {
		if got := backoff(i + 1); got != w {
			t.Errorf("backoff(%d) = %s, want %s", i+1, got, w)
		}
	}
}

func TestUnknownProvider(t *testing.T) {
	g := newGateway(t, filepath.Join(t.TempDir(), "n.json"), &fakeProvider{id: "tailscale"})
	if _, err := g.Activate(context.Background(), "ngrok", nil); err == nil {
		t.Fatal("expected an error")
	}
}
