package noderuntime

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestParseVersion(t *testing.T) {
	for in, want := range map[string]semver{"v26.13.1\n": {26, 13, 1}, "22.13.0": {22, 13, 0}, "v23.0.0-nightly": {23, 0, 0}} {
		got, err := parseVersion(in)
		if err != nil || got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	if _, err := parseVersion("hello"); err == nil {
		t.Error("garbage parsed")
	}
	if !(semver{20, 11, 0}).less(MinVersion) || (semver{22, 13, 0}).less(MinVersion) || (semver{23, 0, 0}).less(MinVersion) {
		t.Error("version ordering")
	}
}

func TestPlatformsHaveChecksums(t *testing.T) {
	for _, p := range [][2]string{{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "amd64"}, {"darwin", "arm64"}, {"windows", "amd64"}, {"windows", "arm64"}} {
		if _, ok := checksums[platformOf(p[0], p[1])]; !ok {
			t.Errorf("no checksum for %s/%s", p[0], p[1])
		}
	}
	if archiveName("win-x64") != "node-"+Version+"-win-x64.zip" || archiveName("linux-x64") != "node-"+Version+"-linux-x64.tar.gz" {
		t.Error("archive names")
	}
}

// fakeNode writes a script that reports version, standing in for node.
func fakeNode(t *testing.T, dir, version string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake node is a shell script")
	}
	p := filepath.Join(dir, "node")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho "+version+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResolveOrder(t *testing.T) {
	sys := t.TempDir()
	fakeNode(t, sys, "v20.11.0")
	t.Setenv("PATH", sys)

	// An old system Node is reported, not used.
	r := New(t.TempDir(), "", "", quiet)
	s := r.Resolve(context.Background())
	if s.State != StateOutdated || s.Found != "20.11.0" {
		t.Fatalf("old system node: %+v", s)
	}
	if _, err := r.Node(); err == nil || !strings.Contains(err.Error(), "20.11.0") {
		t.Fatalf("Node(): %v", err)
	}

	// A new enough one is.
	fakeNode(t, sys, "v24.1.0")
	if s := r.Resolve(context.Background()); s.State != StateReady || s.Source != SourceSystem {
		t.Fatalf("system node: %+v", s)
	}

	// NEXORA_NODE wins, and is not second-guessed when broken.
	r = New(t.TempDir(), filepath.Join(sys, "missing"), "", quiet)
	if s := r.Resolve(context.Background()); s.State != StateError || !strings.Contains(s.Error, "NEXORA_NODE") {
		t.Fatalf("broken override: %+v", s)
	}
}

// release builds a Node-like archive for this platform: a folder with
// bin/node, a symlinked bin/npx and a lib file.
func release(t *testing.T, mutate func(tw *tar.Writer)) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	root := folderName(platform()) + "/"
	add := func(h *tar.Header, body string) {
		h.Size = int64(len(body))
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	add(&tar.Header{Name: root, Typeflag: tar.TypeDir, Mode: 0o755}, "")
	add(&tar.Header{Name: root + "bin/node", Typeflag: tar.TypeReg, Mode: 0o755}, "#!/bin/sh\necho "+Version+"\n")
	add(&tar.Header{Name: root + "lib/npx-cli.js", Typeflag: tar.TypeReg, Mode: 0o644}, "// npx")
	add(&tar.Header{Name: root + "bin/npx", Typeflag: tar.TypeSymlink, Linkname: "../lib/npx-cli.js"}, "")
	if mutate != nil {
		mutate(tw)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func serve(t *testing.T, body []byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+Version+"/"+archiveName(platform()) {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func withChecksum(t *testing.T, body []byte) {
	t.Helper()
	sum := sha256.Sum256(body)
	plat := platform()
	old, had := checksums[plat]
	checksums[plat] = hex.EncodeToString(sum[:])
	t.Cleanup(func() {
		if had {
			checksums[plat] = old
		} else {
			delete(checksums, plat)
		}
	})
}

func waitDone(t *testing.T, r *Runtime) Status {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s := r.Status(); s.State != StateDownloading {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("download did not finish")
	return Status{}
}

func TestDownloadInstallsAndRetries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake release is a tarball with a shell script")
	}
	t.Setenv("PATH", t.TempDir()) // no system node
	body := release(t, nil)
	withChecksum(t, body)
	dir := t.TempDir()
	// A previous version is cleaned up.
	if err := os.MkdirAll(filepath.Join(dir, "node-v1.0.0-old"), 0o755); err != nil {
		t.Fatal(err)
	}

	r := New(dir, "", serve(t, body), quiet)
	if s := r.Resolve(context.Background()); s.State != StateMissing || s.Pinned != Version {
		t.Fatalf("before: %+v", s)
	}
	ready := make(chan struct{})
	r.OnReady = func() { close(ready) }
	if err := r.Download(); err != nil {
		t.Fatal(err)
	}
	s := waitDone(t, r)
	if s.State != StateReady || s.Source != SourceManaged || s.Version != strings.TrimPrefix(Version, "v") {
		t.Fatalf("after: %+v", s)
	}
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("OnReady not called")
	}
	if r.BinDir() != filepath.Join(dir, folderName(platform()), "bin") {
		t.Fatalf("bin dir %q", r.BinDir())
	}
	if target, err := os.Readlink(filepath.Join(r.BinDir(), "npx")); err != nil || target != "../lib/npx-cli.js" {
		t.Fatalf("npx link: %q %v", target, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "node-v1.0.0-old")); !os.IsNotExist(err) {
		t.Fatal("old runtime kept")
	}
	leftovers, _ := filepath.Glob(filepath.Join(dir, ".*"))
	if len(leftovers) != 0 {
		t.Fatalf("temp files left: %v", leftovers)
	}

	// A fresh start finds it without downloading.
	if s := New(dir, "", "", quiet).Resolve(context.Background()); s.State != StateReady || s.Source != SourceManaged {
		t.Fatalf("restart: %+v", s)
	}
}

func TestDownloadRejectsTampering(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake release is a tarball")
	}
	t.Setenv("PATH", t.TempDir())
	body := release(t, nil)
	withChecksum(t, body)
	tampered := append(bytes.Clone(body[:len(body)-1]), body[len(body)-1]^1)

	dir := t.TempDir()
	r := New(dir, "", serve(t, tampered), quiet)
	r.Resolve(context.Background())
	_ = r.Download()
	if s := waitDone(t, r); s.State != StateError || !strings.Contains(s.Error, "tampered") {
		t.Fatalf("tampered: %+v", s)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("tampered download left files: %v", entries)
	}
}

func TestDownloadRejectsEscapingArchives(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake release is a tarball")
	}
	t.Setenv("PATH", t.TempDir())
	for name, evil := range map[string]*tar.Header{
		"dotdot":   {Name: "../evil", Typeflag: tar.TypeReg, Mode: 0o644},
		"abs link": {Name: folderName(platform()) + "/bin/x", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
		"out link": {Name: folderName(platform()) + "/bin/y", Typeflag: tar.TypeSymlink, Linkname: "../../../outside"},
	} {
		t.Run(name, func(t *testing.T) {
			body := release(t, func(tw *tar.Writer) { _ = tw.WriteHeader(evil) })
			withChecksum(t, body)
			dir := t.TempDir()
			r := New(dir, "", serve(t, body), quiet)
			_ = r.Download()
			if s := waitDone(t, r); s.State != StateError || !strings.Contains(s.Error, "unsafe") {
				t.Fatalf("%+v", s)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "evil")); err == nil {
				t.Fatal("wrote outside the runtime dir")
			}
		})
	}
}
