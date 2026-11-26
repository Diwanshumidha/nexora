package extension_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nexora/internal/config"
	"nexora/internal/extension"
)

// rawExtension speaks the protocol directly, so these tests need no build step.
const rawExtension = `
import { createInterface } from "node:readline";
import { readFileSync } from "node:fs";
let settings = {};
const send = (m) => process.stdout.write(JSON.stringify(m) + "\n");
createInterface({ input: process.stdin }).on("line", (line) => {
  const msg = JSON.parse(line);
  switch (msg.method) {
    case "initialize":
      settings = msg.params.settings;
      return send({ jsonrpc: "2.0", id: msg.id, result: { tools: [
        { name: "ping" }, { name: "die" }, { name: "big" }, { name: "bad-name" }, { name: "ping" },
        { name: "settings" }, { name: "readFile" },
      ] } });
    case "settings/update":
      settings = msg.params.settings;
      return send({ jsonrpc: "2.0", id: msg.id, result: {} });
    case "tools/call":
      if (msg.params.name === "settings") return send({ jsonrpc: "2.0", id: msg.id, result: { settings, pid: process.pid } });
      if (msg.params.name === "readFile") {
        try { return send({ jsonrpc: "2.0", id: msg.id, result: readFileSync(msg.params.args.path, "utf8") }); }
        catch (e) { return send({ jsonrpc: "2.0", id: msg.id, error: { code: -32002, message: e.code ?? e.message } }); }
      }
      if (msg.params.name === "die") process.exit(3);
      if (msg.params.name === "big") {
        if (!msg.params.args?.quiet) console.error("x".repeat(200000)); // longer than bufio.Scanner's default limit
        return send({ jsonrpc: "2.0", id: msg.id, result: "y".repeat(200000) });
      }
      return send({ jsonrpc: "2.0", id: msg.id, result: "pong" });
    case "shutdown":
      send({ jsonrpc: "2.0", id: msg.id, result: {} });
      return process.exit(0);
  }
});
`

func newTestManager(t testing.TB) (*extension.Manager, string) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	root := t.TempDir()
	src := filepath.Join(root, "src", "raw")
	must(t, os.MkdirAll(filepath.Join(src, "node_modules"), 0o755))
	must(t, os.WriteFile(filepath.Join(src, "index.mjs"), []byte(rawExtension), 0o644))
	must(t, os.WriteFile(filepath.Join(src, extension.ManifestFile), []byte(
		`{"id":"raw","name":"Raw","version":"0.0.0","runtime":"node","main":"index.mjs","permissions":{},
		  "settings":[{"key":"folders","type":"folders","label":"Folders","grant":"read"},
		              {"key":"greeting","type":"string","label":"Greeting","default":"hi"}]}`), 0o644))
	// pnpm-style symlink that os.CopyFS would reject.
	must(t, os.Symlink(root, filepath.Join(src, "node_modules", "link")))

	cfg := &config.Config{
		ExtensionsDir:    filepath.Join(root, "extensions"),
		ExtensionDataDir: filepath.Join(root, "data"),
		NodePath:         "node",
	}
	must(t, os.MkdirAll(cfg.ExtensionsDir, 0o755))
	return extension.NewManager(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))), src
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestInstallStartCrashRestartRemove(t *testing.T) {
	mgr, src := newTestManager(t)
	ctx := context.Background()
	defer mgr.StopAll(ctx)

	info, err := mgr.Install(src)
	must(t, err)
	if _, err := os.Stat(filepath.Join(info.Dir, "node_modules", "link")); !os.IsNotExist(err) {
		t.Fatal("symlink should not be copied")
	}
	if _, err := mgr.Install(src); err == nil {
		t.Fatal("second install should fail")
	}

	// Concurrent starts must not race or spawn twice.
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := mgr.Start(ctx, "raw"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()

	info, _ = mgr.Get("raw")
	if len(info.Tools) != 5 { // bad-name and the duplicate ping are dropped
		t.Fatalf("expected 3 valid tools, got %+v", info.Tools)
	}

	out, err := mgr.CallTool(ctx, "raw", "big", nil)
	must(t, err)
	if len(out) < 200000 {
		t.Fatalf("large result truncated: %d bytes", len(out))
	}

	// A crash fails the in-flight call promptly and flips status to error.
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := mgr.CallTool(callCtx, "raw", "die", nil); err == nil || callCtx.Err() != nil {
		t.Fatalf("expected prompt error from crashed extension, got %v", err)
	}
	waitFor(t, func() bool { i, _ := mgr.Get("raw"); return i.Status == extension.StatusError })
	if _, err := mgr.CallTool(ctx, "raw", "ping", nil); err == nil {
		t.Fatal("call to crashed extension should fail")
	}

	must(t, mgr.Start(ctx, "raw"))
	if i, _ := mgr.Get("raw"); i.Stale {
		t.Fatal("a freshly started extension is not stale")
	}
	// Rebuilding the extension on disk flags the running process as stale.
	later := time.Now().Add(2 * time.Second)
	must(t, os.Chtimes(filepath.Join(info.Dir, "index.mjs"), later, later))
	if i, _ := mgr.Get("raw"); !i.Stale {
		t.Fatal("changed code should mark the extension stale")
	}
	out, err = mgr.CallTool(ctx, "raw", "ping", nil)
	must(t, err)
	if string(out) != `"pong"` {
		t.Fatalf("got %s", out)
	}

	must(t, mgr.Remove(ctx, "raw"))
	if _, err := os.Stat(info.Dir); !os.IsNotExist(err) {
		t.Fatal("installed extension dir should be deleted")
	}
	if err := mgr.Start(ctx, "raw"); err == nil {
		t.Fatal("start after remove should fail")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSettingsGrantSandboxAccess(t *testing.T) {
	mgr, src := newTestManager(t)
	ctx := context.Background()
	defer mgr.StopAll(ctx)
	_, err := mgr.Install(src)
	must(t, err)
	must(t, mgr.Start(ctx, "raw"))

	secretDir := t.TempDir()
	file := filepath.Join(secretDir, "notes.txt")
	must(t, os.WriteFile(file, []byte("trusted"), 0o644))
	args, _ := json.Marshal(map[string]string{"path": file})

	// Not granted: the Node sandbox refuses.
	if _, err := mgr.CallTool(ctx, "raw", "readFile", args); err == nil || !strings.Contains(err.Error(), "ERR_ACCESS_DENIED") {
		t.Fatalf("expected the sandbox to refuse, got %v", err)
	}

	before := pid(t, mgr)
	values, err := mgr.SetSettings(ctx, "raw", extension.Values{"folders": []any{secretDir}})
	must(t, err)
	if values["greeting"] != "hi" {
		t.Fatalf("defaults should fill unset values: %v", values)
	}
	if pid(t, mgr) == before {
		t.Fatal("changing a granted folder must restart the extension")
	}
	out, err := mgr.CallTool(ctx, "raw", "readFile", args)
	if err != nil || string(out) != `"trusted"` {
		t.Fatalf("granted folder should be readable: %s %v", out, err)
	}

	// Plain settings are delivered live, without a restart.
	before = pid(t, mgr)
	_, err = mgr.SetSettings(ctx, "raw", extension.Values{"folders": []any{secretDir}, "greeting": "hello"})
	must(t, err)
	var got struct {
		Settings map[string]any `json:"settings"`
		PID      int            `json:"pid"`
	}
	raw, _ := mgr.CallTool(ctx, "raw", "settings", nil)
	must(t, json.Unmarshal(raw, &got))
	if got.PID != before || got.Settings["greeting"] != "hello" {
		t.Fatalf("expected a live update without restart: %+v", got)
	}

	// Values survive a restart (and reach initialize).
	must(t, mgr.Stop(ctx, "raw"))
	must(t, mgr.Start(ctx, "raw"))
	raw, _ = mgr.CallTool(ctx, "raw", "settings", nil)
	must(t, json.Unmarshal(raw, &got))
	if got.Settings["greeting"] != "hello" {
		t.Fatalf("settings not persisted: %+v", got.Settings)
	}
}

func pid(t *testing.T, mgr *extension.Manager) int {
	t.Helper()
	raw, err := mgr.CallTool(context.Background(), "raw", "settings", nil)
	must(t, err)
	var v struct {
		PID int `json:"pid"`
	}
	must(t, json.Unmarshal(raw, &v))
	return v.PID
}

// switchableNode stands in for the Node.js runtime manager.
type switchableNode struct{ path atomic.Pointer[string] }

func (n *switchableNode) Node() (string, error) {
	if p := n.path.Load(); p != nil {
		return *p, nil
	}
	return "", errors.New("Node.js is not available: extensions need Node.js")
}
func (n *switchableNode) BinDir() string { return "" }

func TestWaitsForNodeRuntime(t *testing.T) {
	mgr, src := newTestManager(t)
	ctx := context.Background()
	defer mgr.StopAll(ctx)
	rt := &switchableNode{}
	mgr.SetNodeRuntime(rt)

	_, err := mgr.Install(src)
	must(t, err)
	if err := mgr.Start(ctx, "raw"); err == nil || !strings.Contains(err.Error(), "Node.js") {
		t.Fatalf("start without node: %v", err)
	}
	if info, _ := mgr.Get("raw"); info.Status != extension.StatusError || !strings.Contains(info.Error, "needs Node.js") && !strings.Contains(info.Error, "need Node.js") {
		t.Fatalf("info: %+v", info)
	}

	// Node arrives (e.g. the download finished): failed extensions start.
	node := "node"
	rt.path.Store(&node)
	mgr.RetryNeedingNode(ctx)
	waitFor(t, func() bool { info, _ := mgr.Get("raw"); return info.Status == extension.StatusRunning })
}
