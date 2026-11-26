package extension_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"nexora/internal/codemode"
	"nexora/internal/config"
	"nexora/internal/extension"
	"nexora/internal/protocol"
)

// The test binary doubles as a stdio MCP server when this variable is set
// (through the manifest's env, since MCP servers get a minimal environment).
const mcpServerEnv = "NEXORA_TEST_MCP_SERVER"

func TestMain(m *testing.M) {
	if os.Getenv(mcpServerEnv) == "1" {
		if err := testMCPServer().Run(context.Background(), &mcp.StdioTransport{}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type greetIn struct {
	Name string `json:"name"`
}
type addIn struct {
	A, B float64
}
type addOut struct {
	Sum float64 `json:"sum"`
}

func testMCPServer() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1.0.0"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "get-greeting", Description: "Greets", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(_ context.Context, _ *mcp.CallToolRequest, in greetIn) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Hello, " + in.Name + "!"}}}, nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "add"},
		func(_ context.Context, _ *mcp.CallToolRequest, in addIn) (*mcp.CallToolResult, addOut, error) {
			return nil, addOut{Sum: in.A + in.B}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "fail"},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return nil, nil, fmt.Errorf("it broke")
		})
	mcp.AddTool(s, &mcp.Tool{Name: "env"},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			text := os.Getenv("TOKEN") + "|" + strings.Join(os.Args[1:], ",") + "|" + os.Getenv("SECRET_FROM_SHELL")
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "grow"},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			mcp.AddTool(s, &mcp.Tool{Name: "late"},
				func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
					return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "late"}}}, nil, nil
				})
			return &mcp.CallToolResult{}, nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "crash"},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			os.Exit(3)
			return nil, nil, nil
		})
	return s
}

func newMCPManager(t *testing.T, man map[string]any) *extension.Manager {
	t.Helper()
	data := t.TempDir()
	cfg := &config.Config{
		ExtensionsDir:    filepath.Join(data, "extensions"),
		ExtensionDataDir: filepath.Join(data, "extension-data"),
	}
	dir := filepath.Join(cfg.ExtensionsDir, man["id"].(string))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(man)
	if err := os.WriteFile(filepath.Join(dir, extension.ManifestFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	mgr := extension.NewManager(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := mgr.Discover(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mgr.StopAll(context.Background()) })
	return mgr
}

func run(t *testing.T, mgr *extension.Manager, ns, script string) any {
	t.Helper()
	var names []string
	for _, info := range mgr.List() {
		if info.Manifest.Namespace == ns {
			for _, tool := range info.Tools {
				names = append(names, tool.Name)
			}
		}
	}
	res, err := codemode.New(mgr, codemode.Options{}).Run(context.Background(), script,
		[]codemode.Namespace{{Name: ns, Tools: names}})
	if err != nil {
		t.Fatal(err)
	}
	return res.Value
}

func TestMCPStdio(t *testing.T) {
	t.Setenv("SECRET_FROM_SHELL", "leaked")
	mgr := newMCPManager(t, map[string]any{
		"id": "mcptest", "name": "MCP test", "version": "1.0.0", "runtime": "mcp",
		"command": os.Args[0],
		"args":    []string{"--dir=${DIR}", "${folders}"},
		"env":     map[string]string{mcpServerEnv: "1", "TOKEN": "${token}"},
		"settings": []map[string]any{
			{"key": "token", "type": "secret", "label": "API token", "required": true},
			{"key": "folders", "type": "folders", "label": "Folders"},
		},
	})
	ctx := context.Background()

	err := mgr.Start(ctx, "mcptest")
	if err == nil || !strings.Contains(err.Error(), "API token is not set") {
		t.Fatalf("want missing token error, got %v", err)
	}
	// Saving the missing setting retries the start.
	a, b := t.TempDir(), t.TempDir()
	if _, err := mgr.SetSettings(ctx, "mcptest", extension.Values{"token": "s3cret", "folders": []any{a, b}}); err != nil {
		t.Fatal(err)
	}
	info, _ := mgr.Get("mcptest")
	if info.Status != extension.StatusRunning {
		t.Fatalf("status %s: %s", info.Status, info.Error)
	}

	byName := map[string]protocol.ToolSpec{}
	for _, tool := range info.Tools {
		byName[tool.Name] = tool
	}
	if _, ok := byName["get_greeting"]; !ok {
		t.Fatalf("get-greeting not exposed as get_greeting: %v", info.Tools)
	}
	if !byName["get_greeting"].ReadOnly || byName["add"].ReadOnly {
		t.Fatal("readOnlyHint not mapped")
	}
	types := codemode.GenerateTypes([]codemode.APINamespace{{Name: "mcptest", Tools: info.Tools}})
	if !strings.Contains(types, "get_greeting(input: {") || !strings.Contains(types, "sum") {
		t.Fatalf("unexpected types:\n%s", types)
	}

	got := run(t, mgr, "mcptest", `
		const [greeting, added, env] = await Promise.all([
			tools.mcptest.get_greeting({ name: "Nexora" }),
			tools.mcptest.add({ A: 2, B: 3 }),
			tools.mcptest.env({}),
		])
		let failed = ""
		try { await tools.mcptest.fail({}) } catch (e) { failed = String(e.message ?? e) }
		return { greeting, sum: added.sum, env, failed }
	`).(map[string]any)
	if got["greeting"] != "Hello, Nexora!" || fmt.Sprint(got["sum"]) != "5" {
		t.Fatalf("unexpected result: %#v", got)
	}
	dir, _ := filepath.Abs(filepath.Dir(info.Dir) + "/mcptest")
	if want := "s3cret|--dir=" + dir + "," + a + "," + b + "|"; got["env"] != want {
		t.Fatalf("env/args = %q, want %q", got["env"], want)
	}
	if !strings.Contains(got["failed"].(string), "it broke") {
		t.Fatalf("tool error not thrown: %q", got["failed"])
	}

	// Tools added at runtime show up through notifications/tools/list_changed.
	run(t, mgr, "mcptest", `await tools.mcptest.grow({})`)
	waitFor(t, func() bool {
		info, _ := mgr.Get("mcptest")
		return slices.ContainsFunc(info.Tools, func(s protocol.ToolSpec) bool { return s.Name == "late" })
	})

	// A crashing server is reported, and calls fail cleanly.
	_, _ = mgr.CallTool(ctx, "mcptest", "crash", nil)
	waitFor(t, func() bool {
		info, _ := mgr.Get("mcptest")
		return info.Status == extension.StatusError
	})
	info, _ = mgr.Get("mcptest")
	if !strings.Contains(info.Error, "MCP server stopped unexpectedly") {
		t.Fatalf("unexpected error: %s", info.Error)
	}
	if _, err := mgr.CallTool(ctx, "mcptest", "add", nil); err == nil {
		t.Fatal("call to crashed server succeeded")
	}

	// And it can be started again.
	if err := mgr.Start(ctx, "mcptest"); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Stop(ctx, "mcptest"); err != nil {
		t.Fatal(err)
	}
}

func TestMCPHTTP(t *testing.T) {
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return testMCPServer() }, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer srv.Close()

	mgr := newMCPManager(t, map[string]any{
		"id": "remote", "name": "Remote", "version": "1.0.0", "runtime": "mcp",
		"url":      srv.URL,
		"headers":  map[string]string{"Authorization": "Bearer ${token}"},
		"settings": []map[string]any{{"key": "token", "type": "secret", "label": "Token"}},
	})
	ctx := context.Background()
	if err := mgr.Start(ctx, "remote"); err == nil {
		t.Fatal("connected without a token")
	}
	if _, err := mgr.SetSettings(ctx, "remote", extension.Values{"token": "tok"}); err != nil {
		t.Fatal(err)
	}
	got := run(t, mgr, "remote", `return await tools.remote.get_greeting({ name: "web" })`)
	if got != "Hello, web!" {
		t.Fatalf("got %#v", got)
	}
	if err := mgr.Stop(ctx, "remote"); err != nil {
		t.Fatal(err)
	}
}

func TestMCPCreate(t *testing.T) {
	data := t.TempDir()
	cfg := &config.Config{
		ExtensionsDir:    filepath.Join(data, "extensions"),
		ExtensionDataDir: filepath.Join(data, "extension-data"),
	}
	mgr := extension.NewManager(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	defer mgr.StopAll(ctx)

	spec := extension.MCPServerSpec{
		Name:        "My Server",
		CommandLine: fmt.Sprintf("%q", os.Args[0]),
		Env:         map[string]string{mcpServerEnv: "1"},
	}
	man, values, err := extension.NewMCPManifest(spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Create(man, values); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Start(ctx, "my-server"); err != nil {
		t.Fatal(err)
	}
	if got := run(t, mgr, "myServer", `return await tools.myServer.get_greeting({ name: "x" })`); got != "Hello, x!" {
		t.Fatalf("got %#v", got)
	}
	if _, err := mgr.Create(man, values); err == nil {
		t.Fatal("duplicate create succeeded")
	}

	// It survives a restart of the app: rediscovered from disk.
	mgr.StopAll(ctx)
	mgr2 := extension.NewManager(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := mgr2.Discover(); err != nil {
		t.Fatal(err)
	}
	if err := mgr2.Start(ctx, "my-server"); err != nil {
		t.Fatal(err)
	}
	if err := mgr2.Remove(ctx, "my-server"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.ExtensionsDir, "my-server")); !os.IsNotExist(err) {
		t.Fatalf("folder not removed: %v", err)
	}
}
