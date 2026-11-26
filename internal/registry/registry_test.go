package registry

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const manifest = `{"id":"demo","name":"Demo","version":"1.2.0","protocol":1,"runtime":"node","main":"dist/index.js","permissions":{}}`

func makeZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// serve publishes a catalog with one entry for zipData; mutate can tamper
// with the entry after its checksum is computed.
func serve(t *testing.T, zipData []byte, mutate func(*Entry)) (*Client, Entry) {
	t.Helper()
	sum := sha256.Sum256(zipData)
	e := Entry{ID: "demo", Name: "Demo", Version: "1.2.0", Protocol: 1, URL: "demo-1.2.0.zip",
		Size: int64(len(zipData)), SHA256: hex.EncodeToString(sum[:])}
	if mutate != nil {
		mutate(&e)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/rel/index.json", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(Catalog{Schema: 1, Extensions: []Entry{e}})
	})
	mux.HandleFunc("/rel/demo-1.2.0.zip", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(zipData) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return New(srv.URL + "/rel/index.json"), e
}

func TestDownloadVerifiesAndUnpacks(t *testing.T) {
	c, _ := serve(t, makeZip(t, map[string]string{"nexora.json": manifest, "dist/index.js": "export {}"}), nil)
	cat, err := c.Fetch(context.Background())
	if err != nil || len(cat.Extensions) != 1 {
		t.Fatalf("fetch: %v %+v", err, cat)
	}
	dir, cleanup, err := c.Download(context.Background(), cat.Extensions[0])
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if b, err := os.ReadFile(filepath.Join(dir, "dist/index.js")); err != nil || string(b) != "export {}" {
		t.Fatalf("unpacked: %q %v", b, err)
	}
}

func TestDownloadRefusesBadPackages(t *testing.T) {
	good := makeZip(t, map[string]string{"nexora.json": manifest, "dist/index.js": "x"})
	for name, tc := range map[string]struct {
		zip    []byte
		mutate func(*Entry)
		want   string
	}{
		"tampered":      {good, func(e *Entry) { e.SHA256 = strings.Repeat("0", 64) }, "checksum mismatch"},
		"wrong version": {good, func(e *Entry) { e.Version = "9.9.9" }, "catalog says"},
		"zip slip": {makeZip(t, map[string]string{"nexora.json": manifest, "../../evil.js": "x"}),
			nil, "unsafe entry"},
		"newer protocol": {makeZip(t, map[string]string{
			"nexora.json":   strings.Replace(manifest, `"protocol":1`, `"protocol":99`, 1),
			"dist/index.js": "x"}), nil, "newer Nexora"},
	} {
		t.Run(name, func(t *testing.T) {
			c, e := serve(t, tc.zip, tc.mutate)
			if _, _, err := c.Download(context.Background(), e); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestRefusesPlainHTTPOffLoopback(t *testing.T) {
	_, err := New("http://example.com/index.json").Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refusing plain http") {
		t.Fatalf("got %v", err)
	}
	if _, err := New("").Fetch(context.Background()); err != ErrNotConfigured {
		t.Fatalf("got %v", err)
	}
}

func TestOpenArchive(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, files map[string]string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, makeZip(t, files), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// As published, and as made by zipping the extension's folder.
	for _, p := range []string{
		write("flat.nxext", map[string]string{"nexora.json": manifest, "dist/index.js": "export {}"}),
		write("nested.zip", map[string]string{"demo/nexora.json": manifest, "demo/dist/index.js": "export {}"}),
	} {
		got, man, cleanup, err := OpenArchive(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if man.ID != "demo" {
			t.Fatalf("%s: manifest %+v", p, man)
		}
		if _, err := os.Stat(filepath.Join(got, "dist", "index.js")); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		cleanup()
	}
	for p, want := range map[string]string{
		write("evil.nxext", map[string]string{"../x": "boom"}):    "unsafe entry",
		write("empty.zip", map[string]string{"readme.txt": "hi"}): "nexora.json",
		write("notes.txt", map[string]string{"a": "b"}):           "not an extension package",
	} {
		if _, _, _, err := OpenArchive(p); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want %q", filepath.Base(p), err, want)
		}
	}
}
