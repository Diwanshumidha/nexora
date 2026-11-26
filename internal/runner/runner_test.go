package runner_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nexora/internal/cache"
	"nexora/internal/codemode"
	"nexora/internal/config"
	"nexora/internal/extension"
	"nexora/internal/jobs"
	"nexora/internal/permissions"
	"nexora/internal/runner"
)

func newRunner(t *testing.T) *runner.Runner {
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
	store, err := cache.Open(cache.Options{Path: filepath.Join(dir, "cache.json")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return runner.New(mgr, queue, perms, store, time.Minute)
}

func run(t *testing.T, r *runner.Runner, code string) jobs.Job {
	t.Helper()
	job, err := r.Run(context.Background(), code, runner.SourceApp, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

// tools.store keeps values from one run to the next, without approvals.
func TestStoreBetweenRuns(t *testing.T) {
	r := newRunner(t)
	if api := r.API(); !strings.Contains(api, "store: {") || !strings.Contains(api, "ttl?: number | string") {
		t.Fatalf("API missing tools.store:\n%s", api)
	}
	first := run(t, r, `
		const seen = await tools.store.get({ key: "runs" }) ?? 0
		await tools.store.set({ key: "runs", value: seen + 1, ttl: "1h" })
		return seen`)
	second := run(t, r, `return await tools.store.get({ key: "runs" })`)
	if first.Error != "" || second.Error != "" {
		t.Fatalf("errors: %q %q", first.Error, second.Error)
	}
	if v := first.Result.(*codemode.Result).Value; v != float64(0) {
		t.Fatalf("first run = %v", v)
	}
	if v := second.Result.(*codemode.Result).Value; v != float64(1) {
		t.Fatalf("second run = %v", v)
	}

	bad := run(t, r, `await tools.store.set({ key: "x", value: 1, ttl: "1y" })`)
	if !strings.Contains(bad.Error, "tools.store.set: ttl must be") {
		t.Fatalf("bad ttl error = %q", bad.Error)
	}
}
