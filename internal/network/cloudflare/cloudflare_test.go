package cloudflare

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeBinary(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cloudflared")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

var target, _ = url.Parse("http://127.0.0.1:7377/mcp")

func TestExposeStatusClose(t *testing.T) {
	bin := fakeBinary(t, `
echo "INF Requesting new quick Tunnel on trycloudflare.com..." >&2
echo "INF |  https://brave-otter-flies.trycloudflare.com  |" >&2
trap 'exit 0' TERM
while true; do sleep 0.05; done
`)
	p := &Provider{Binary: bin}
	ctx := context.Background()
	ex, err := p.Expose(ctx, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ex.URL != "https://brave-otter-flies.trycloudflare.com/mcp" || !ex.Public {
		t.Fatalf("got %+v", ex)
	}
	if err := p.Status(ctx); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := p.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("close should stop cloudflared promptly")
	}
	if err := p.Status(ctx); err == nil {
		t.Fatal("status should fail after close")
	}
}

func TestDetectsExit(t *testing.T) {
	p := &Provider{Binary: fakeBinary(t, `echo "https://a-b.trycloudflare.com" >&2; sleep 0.1; exit 1`)}
	if _, err := p.Expose(context.Background(), target, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for p.Status(context.Background()) == nil {
		if time.Now().After(deadline) {
			t.Fatal("exit not detected")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestFailsWithOutput(t *testing.T) {
	p := &Provider{Binary: fakeBinary(t, `echo "ERR failed to reach the edge" >&2; exit 1`)}
	_, err := p.Expose(context.Background(), target, nil)
	if err == nil || !strings.Contains(err.Error(), "failed to reach the edge") {
		t.Fatalf("got %v", err)
	}
}

func TestTimeout(t *testing.T) {
	p := &Provider{Binary: fakeBinary(t, `sleep 30`)}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := p.Expose(ctx, target, nil); err == nil || !strings.Contains(err.Error(), "in time") {
		t.Fatalf("got %v", err)
	}
}

func TestNotInstalled(t *testing.T) {
	p := &Provider{Binary: "definitely-not-cloudflared"}
	if err := p.Available(context.Background()); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("got %v", err)
	}
}
