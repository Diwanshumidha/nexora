package mcp_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
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
	"nexora/internal/scripts"
)

func newScriptsEndpoint(t *testing.T) (string, *scripts.Manager) {
	t.Helper()
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := extension.NewManager(&config.Config{ExtensionsDir: dir, ExtensionDataDir: dir}, log)
	queue := jobs.New(jobs.Options{Workers: 2})
	t.Cleanup(queue.Close)
	perms, err := permissions.Open(filepath.Join(dir, "permissions.json"))
	if err != nil {
		t.Fatal(err)
	}
	run := runner.New(mgr, queue, perms, nil, time.Minute)
	store, err := scripts.OpenStore(filepath.Join(dir, "scripts.json"))
	if err != nil {
		t.Fatal(err)
	}
	saved := scripts.NewManager(store, run.SubmitSpec, queue.Get, log)
	srv := nmcp.NewServer(run, nmcp.Options{InlineWait: 5 * time.Second, Scripts: saved})
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
	return ts.URL + "/mcp/" + token, saved
}

func TestScriptTools(t *testing.T) {
	url, saved := newScriptsEndpoint(t)
	cs := connect(t, url)

	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "save_script", Arguments: map[string]any{
		"name": "Greet", "code": "return `hi ${args.who} x${args.times}`",
		"params": []map[string]any{
			{"name": "who", "type": "string", "required": true},
			{"name": "times", "type": "number", "default": 2},
		},
	}})
	if err != nil || res.IsError {
		t.Fatalf("save_script: %v %s", err, res.Content[0].(*sdk.TextContent).Text)
	}
	sc, err := saved.Get("greet")
	if err != nil || sc.CreatedBy != runner.SourceMCP {
		t.Fatalf("saved %+v, %v", sc, err)
	}

	// An AI can't replace a script, even its own.
	res, _ = cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "save_script", Arguments: map[string]any{
		"name": "greet", "code": "return 'replaced'",
	}})
	if !res.IsError {
		t.Fatal("save_script replaced an existing script")
	}

	res, err = cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "list_scripts", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	if !strings.Contains(string(raw), `"name":"Greet"`) || !strings.Contains(string(raw), `"savedBy":"ai"`) {
		t.Fatalf("list_scripts: %s", raw)
	}

	out, isErr := call(t, cs, "run_script", map[string]any{"name": "greet", "args": map[string]any{"who": "Ada"}})
	if isErr || out.Status != "succeeded" || out.Result != "hi Ada x2" {
		t.Fatalf("run_script: %+v", out)
	}
	if got, _ := saved.Get("greet"); got.LastRun == nil || got.LastRun.Source != runner.SourceMCP {
		t.Fatalf("last run not recorded: %+v", got.LastRun)
	}

	res, _ = cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "run_script", Arguments: map[string]any{"name": "greet"}})
	if !res.IsError {
		t.Fatal("run_script without a required argument succeeded")
	}
}
