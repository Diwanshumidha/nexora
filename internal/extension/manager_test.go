package extension_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"nexora/internal/codemode"
	"nexora/internal/config"
	"nexora/internal/extension"
)

// TestHelloEndToEnd spawns the example extension and calls it from a
// code-mode script. Requires a built nexora-extensions checkout and Node.js.
func TestHelloEndToEnd(t *testing.T) {
	exts := extensionsDir()
	if _, err := os.Stat(filepath.Join(exts, "hello/dist/index.js")); err != nil {
		t.Skip("hello extension not built; run `pnpm build` in nexora-extensions")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}

	data := t.TempDir()
	cfg := &config.Config{
		ExtensionsDir:     filepath.Join(data, "extensions"),
		ExtensionDataDir:  filepath.Join(data, "extension-data"),
		DevExtensionsDirs: []string{exts},
		NodePath:          "node",
	}
	mgr := extension.NewManager(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := mgr.Discover(); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if err := mgr.Start(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	defer mgr.StopAll(ctx)

	info, _ := mgr.Get("hello")
	if len(info.Tools) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(info.Tools))
	}

	types := codemode.GenerateTypes([]codemode.APINamespace{{Name: "hello", Tools: info.Tools}})
	if !strings.Contains(types, "greet(input: {") {
		t.Fatalf("unexpected types:\n%s", types)
	}

	rt := codemode.New(mgr, codemode.Options{})
	res, err := rt.Run(ctx, `
		const [sum, msg] = await Promise.all([
			tools.hello.add({ a: 2, b: 3 }),
			tools.hello.greet({ name: "Nexora" }),
		])
		console.log(msg)
		return { sum, msg }
	`, []codemode.Namespace{{Name: "hello", Tools: []string{"greet", "add"}}})
	if err != nil {
		t.Fatal(err)
	}
	got := res.Value.(map[string]any)
	if got["msg"] != "Hello, Nexora!" || fmt.Sprint(got["sum"]) != "5" {
		t.Fatalf("unexpected result: %#v", got)
	}
	if len(res.Logs) != 1 {
		t.Fatalf("expected 1 log line, got %v", res.Logs)
	}
}
