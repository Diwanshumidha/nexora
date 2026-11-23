package extension_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"nexora/internal/config"
	"nexora/internal/extension"
)

// client calls one extension's tools in its real sandbox.
type client struct {
	t    *testing.T
	call func(tool string, args any) (json.RawMessage, error)
}

// do calls a tool that must succeed.
func (c client) do(tool string, args any) json.RawMessage {
	c.t.Helper()
	out, err := c.call(tool, args)
	if err != nil {
		c.t.Fatalf("%s: %v", tool, err)
	}
	return out
}

// bad calls a tool that must fail with an error containing want.
func (c client) bad(want, tool string, args any) {
	c.t.Helper()
	if _, err := c.call(tool, args); err == nil || !strings.Contains(err.Error(), want) {
		c.t.Fatalf("%s: expected an error containing %q, got %v", tool, want, err)
	}
}

// builtin starts one of the nexora-extensions repo's extensions in its real sandbox.
func builtin(t *testing.T, id string, settings extension.Values) client {
	t.Helper()
	exts := extensionsDir()
	if _, err := os.Stat(filepath.Join(exts, id, "dist/index.js")); err != nil {
		t.Skipf("%s extension not built; run `pnpm build` in nexora-extensions", id)
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
	must(t, mgr.Discover())
	ctx := context.Background()
	if settings != nil {
		_, err := mgr.SetSettings(ctx, id, settings)
		must(t, err)
	}
	must(t, mgr.Start(ctx, id))
	t.Cleanup(func() { mgr.StopAll(context.Background()) })
	return client{t: t, call: func(tool string, args any) (json.RawMessage, error) {
		raw, _ := json.Marshal(args)
		return mgr.CallTool(ctx, id, tool, raw)
	}}
}

type obj = map[string]any

func TestFilesExtension(t *testing.T) {
	trusted := t.TempDir()
	outside := t.TempDir()
	must(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("nope"), 0o644))
	// A symlink inside the trusted folder that points outside it.
	must(t, os.Symlink(outside, filepath.Join(trusted, "escape")))

	f := builtin(t, "files", extension.Values{"folders": []any{trusted}})
	in := func(p string) string { return filepath.Join(trusted, p) }
	has := func(out json.RawMessage, parts ...string) {
		t.Helper()
		for _, p := range parts {
			if !strings.Contains(string(out), p) {
				t.Fatalf("expected %q in %s", p, out)
			}
		}
	}

	// roots + writing (relative paths resolve against the first trusted folder)
	has(f.do("roots", nil), trusted)
	f.do("writeText", obj{"path": "docs/report.md", "content": "Q3 revenue grew\nsecond line"})
	f.bad("already exists", "writeText", obj{"path": "docs/report.md", "content": "x"})
	f.do("writeText", obj{"path": in("docs/report.md"), "content": "v2: Q3 revenue grew", "overwrite": true})
	f.do("appendText", obj{"path": "docs/report.md", "content": "\nappended"})
	var read struct{ Content string }
	must(t, json.Unmarshal(f.do("readText", obj{"path": "docs/report.md"}), &read))
	if read.Content != "v2: Q3 revenue grew\nappended" {
		t.Fatalf("readText: %q", read.Content)
	}

	// list / tree / find / size / hash
	f.do("mkdir", obj{"path": "archive/2024"})
	has(f.do("list", obj{}), `"archive"`, `"docs"`)
	has(f.do("tree", obj{"depth": 3}), "2024/", "report.md")
	has(f.do("find", obj{"name": "*.MD"}), "report.md")
	has(f.do("find", obj{"contains": "revenue"}), "1: v2: Q3 revenue grew")
	has(f.do("size", obj{"path": "docs"}), `"files":1`)
	if out := f.do("hash", obj{"path": "docs/report.md"}); len(out) != 66 {
		t.Fatalf("hash: %s", out)
	}

	// copy / move / rename
	f.do("copy", obj{"from": "docs/report.md", "to": "archive"}) // into a folder
	f.do("copy", obj{"from": "docs", "to": "docs-backup"})       // a whole folder
	f.do("move", obj{"from": "archive/report.md", "to": "archive/2024/q3.md"})
	f.do("rename", obj{"path": "archive/2024/q3.md", "newName": "q3-final.md"})
	for _, p := range []string{"archive/2024/q3-final.md", "docs-backup/report.md", "docs/report.md"} {
		if _, err := os.Stat(in(p)); err != nil {
			t.Fatalf("expected %s: %v", p, err)
		}
	}
	f.bad("already exists", "copy", obj{"from": "docs/report.md", "to": "docs-backup/report.md"})
	f.bad("into itself", "move", obj{"from": "docs", "to": "docs/inner"})
	f.bad("plain name", "rename", obj{"path": "docs", "newName": "../escaped"})

	// delete -> trash -> restore; the overwritten version is in the trash too
	f.do("delete", obj{"path": "docs-backup"})
	var trash []struct{ ID, Reason string }
	must(t, json.Unmarshal(f.do("listTrash", nil), &trash))
	if len(trash) != 2 || trash[0].Reason != "deleted" || trash[1].Reason != "overwritten" {
		t.Fatalf("trash: %+v", trash)
	}
	f.do("restore", obj{"id": trash[0].ID})
	if _, err := os.Stat(in("docs-backup/report.md")); err != nil {
		t.Fatal("restore should bring the folder back")
	}
	f.do("delete", obj{"path": "docs-backup", "permanent": true})
	has(f.do("emptyTrash", nil), `"removed":1`)

	// Safety: outside paths, symlink escapes, the root itself.
	f.bad("outside the trusted folders", "readText", obj{"path": filepath.Join(outside, "secret.txt")})
	f.bad("outside the trusted folders", "readText", obj{"path": "escape/secret.txt"})
	f.bad("outside the trusted folders", "copy", obj{"from": "docs/report.md", "to": outside})
	f.bad("outside the trusted folders", "list", obj{"path": "../"})
	f.bad("trusted folder itself", "delete", obj{"path": trusted})
	if _, err := os.Stat(filepath.Join(outside, "report.md")); err == nil {
		t.Fatal("nothing may be written outside the trusted folders")
	}
}

func TestFilesNeedsAFolder(t *testing.T) {
	builtin(t, "files", nil).bad("no trusted folders", "list", obj{})
}

func TestWebBlocksLocalNetworks(t *testing.T) {
	w := builtin(t, "web", nil)
	for _, u := range []string{
		"http://127.0.0.1:7377/mcp",      // Nexora's own MCP endpoint
		"http://localhost:8080/",         // anything on this machine
		"http://[::1]/",                  // IPv6 loopback
		"http://192.168.1.1/",            // the router
		"http://10.0.0.5/",               // LAN
		"http://100.101.102.103/",        // a tailnet address
		"http://169.254.169.254/latest/", // cloud metadata
		"http://[::ffff:127.0.0.1]/",     // IPv4-mapped loopback
		"http://2130706433/",             // 127.0.0.1 written as a number
	} {
		w.bad("private network", "get", obj{"url": u})
	}
	w.bad("Only http", "get", obj{"url": "file:///etc/passwd"})
	w.bad("credentials", "get", obj{"url": "https://user:pw@example.com/"})
}

func TestWebAllowedSites(t *testing.T) {
	builtin(t, "web", extension.Values{"allowedSites": "example.com"}).
		bad("not in Web's allowed sites", "get", obj{"url": "https://github.com/"})
}

func TestNotesExtension(t *testing.T) {
	n := builtin(t, "notes", nil)
	n.do("create", obj{"title": "Q3 / plan", "content": "# Plan\n- ship orbit"})
	n.bad("already exists", "create", obj{"title": "Q3 / plan", "content": "x"})
	n.do("append", obj{"title": "Q3 / plan", "content": "- ship files"})
	n.do("create", obj{"title": "../../escape", "content": "stays private"})

	var text string
	must(t, json.Unmarshal(n.do("read", obj{"title": "Q3 / plan"}), &text))
	if text != "# Plan\n- ship orbit\n- ship files" {
		t.Fatalf("read: %q", text)
	}
	if out := n.do("search", obj{"query": "FILES"}); !strings.Contains(string(out), "- ship files") {
		t.Fatalf("search: %s", out)
	}
	n.do("update", obj{"title": "Q3 / plan", "content": "done"})
	if out := string(n.do("list", nil)); !strings.Contains(out, `"Q3 / plan"`) || !strings.Contains(out, `"../../escape"`) {
		t.Fatalf("list: %s", out)
	}
	n.do("delete", obj{"title": "Q3 / plan"})
	n.bad("No note", "read", obj{"title": "Q3 / plan"})
}
