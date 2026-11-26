package services

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"nexora/internal/config"
	"nexora/internal/extension"
	"nexora/internal/registry"
)

// releaseDir is a nexora-extensions checkout's `pnpm release` output.
func releaseDir() string {
	repo := os.Getenv("NEXORA_EXTENSIONS_REPO")
	if repo == "" {
		repo, _ = filepath.Abs("../../../nexora-extensions")
	}
	return filepath.Join(repo, "release")
}

// bumpVersion rewrites the manifest inside an extension zip.
func bumpVersion(t *testing.T, data []byte, version string) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range zr.File {
		rc, _ := f.Open()
		body, _ := io.ReadAll(rc)
		rc.Close()
		if f.Name == "nexora.json" {
			var m map[string]any
			_ = json.Unmarshal(body, &m)
			m["version"] = version
			body, _ = json.Marshal(m)
		}
		w, _ := zw.Create(f.Name)
		_, _ = w.Write(body)
	}
	_ = zw.Close()
	return buf.Bytes()
}

func TestCatalogInstallAndUpdate(t *testing.T) {
	zipV1, err := os.ReadFile(filepath.Join(releaseDir(), "csv-0.1.0.zip"))
	if err != nil {
		t.Skip("no nexora-extensions release; run `pnpm release` there")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}

	// A catalog that can be switched from 0.1.0 to 0.2.0 mid-test.
	var current atomic.Pointer[[]byte]
	var version atomic.Value
	current.Store(&zipV1)
	version.Store("0.1.0")
	mux := http.NewServeMux()
	mux.HandleFunc("/index.json", func(w http.ResponseWriter, _ *http.Request) {
		data := *current.Load()
		sum := sha256.Sum256(data)
		_ = json.NewEncoder(w).Encode(registry.Catalog{Schema: 1, Extensions: []registry.Entry{{
			ID: "csv", Name: "CSV", Version: version.Load().(string), Protocol: 1,
			URL: "csv.zip", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]),
		}}})
	})
	mux.HandleFunc("/csv.zip", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(*current.Load()) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	data := t.TempDir()
	cfg := &config.Config{
		DataDir: data, ExtensionsDir: filepath.Join(data, "extensions"),
		ExtensionDataDir: filepath.Join(data, "extension-data"), NodePath: "node",
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(cfg.ExtensionsDir, 0o755))
	mgr := extension.NewManager(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer mgr.StopAll(context.Background())
	saved := filepath.Join(data, "catalog.json")
	svc := NewCatalogService(mgr, registry.New(srv.URL+"/index.json"), saved, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	state := func() string {
		view, err := svc.List(ctx, true)
		must(err)
		return view.Items[0].State
	}

	if s := state(); s != "available" {
		t.Fatalf("before install: %s", s)
	}
	info, err := svc.Install(ctx, "csv")
	must(err)
	if info.Status != extension.StatusRunning || len(info.Tools) == 0 {
		t.Fatalf("installed: %+v", info)
	}
	_, err = mgr.SetSettings(ctx, "csv", extension.Values{"maxRows": 7})
	must(err)
	if s := state(); s != "installed" {
		t.Fatalf("after install: %s", s)
	}
	if _, err := svc.Install(ctx, "csv"); err == nil || !strings.Contains(err.Error(), "already installed") {
		t.Fatalf("second install: %v", err)
	}

	// Publish 0.2.0: the catalog offers an update, which keeps settings.
	zipV2 := bumpVersion(t, zipV1, "0.2.0")
	current.Store(&zipV2)
	version.Store("0.2.0")
	if s := state(); s != "update" {
		t.Fatalf("after publish: %s", s)
	}
	info, err = svc.Update(ctx, "csv")
	must(err)
	if info.Manifest.Version != "0.2.0" || info.Status != extension.StatusRunning {
		t.Fatalf("updated: %s %s %s", info.Manifest.Version, info.Status, info.Error)
	}
	_, values, err := mgr.Settings("csv")
	must(err)
	if values["maxRows"] != float64(7) && values["maxRows"] != 7 {
		t.Fatalf("settings lost: %+v", values)
	}
	if _, err := os.Stat(filepath.Join(cfg.ExtensionsDir, "csv.old")); err == nil {
		t.Fatal("old version left behind")
	}
	if s := state(); s != "installed" {
		t.Fatalf("after update: %s", s)
	}

	if u := svc.Updates(); len(u) != 0 {
		t.Fatalf("updates after update: %+v", u)
	}

	// A package file: an older one is refused, a newer one updates in place.
	pkg := func(version string, data []byte) string {
		path := filepath.Join(t.TempDir(), "csv-"+version+".nxext")
		must(os.WriteFile(path, data, 0o644))
		return path
	}
	if _, err := svc.InstallFile(ctx, pkg("0.1.0", zipV1)); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("downgrade from file: %v", err)
	}
	info, err = svc.InstallFile(ctx, pkg("0.3.0", bumpVersion(t, zipV1, "0.3.0")))
	must(err)
	if info.Manifest.Version != "0.3.0" || info.Status != extension.StatusRunning {
		t.Fatalf("file update: %s %s %s", info.Manifest.Version, info.Status, info.Error)
	}

	// Offline: the saved copy is shown, flagged, here and after a restart.
	srv.Close()
	view, err := svc.List(ctx, true)
	must(err)
	if !view.Offline || len(view.Items) != 1 || view.FetchedAt.IsZero() {
		t.Fatalf("offline view: %+v", view)
	}
	restarted := NewCatalogService(mgr, registry.New(srv.URL+"/index.json"), saved, slog.New(slog.NewTextHandler(io.Discard, nil)))
	restarted.loadSaved()
	view, err = restarted.List(ctx, false)
	must(err)
	if len(view.Items) != 1 || view.Items[0].Version != "0.2.0" {
		t.Fatalf("saved catalog after restart: %+v", view)
	}
}
