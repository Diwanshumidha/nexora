package noderuntime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestOnlineDownload installs the real pinned release from nodejs.org
// (about 60 MB) and runs it the way extensions are run.
func TestOnlineDownload(t *testing.T) {
	if os.Getenv("NEXORA_ONLINE_TESTS") == "" {
		t.Skip("set NEXORA_ONLINE_TESTS=1")
	}
	t.Setenv("PATH", t.TempDir()) // no system node
	r := New(t.TempDir(), "", "", quiet)
	r.Resolve(context.Background())
	if err := r.Download(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Minute)
	for r.Status().State == StateDownloading && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	s := r.Status()
	if s.State != StateReady || "v"+s.Version != Version {
		t.Fatalf("%+v", s)
	}
	// The flags extensions run with are accepted.
	out, err := exec.Command(s.Path, "--permission", "--disable-warning=ExperimentalWarning",
		"-e", "console.log(process.permission.has('fs.read', '/etc'))").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "false" {
		t.Fatalf("permission model: %s %v", out, err)
	}
	// npx runs from the bin dir alone, as local MCP servers get it.
	npx := exec.Command(filepath.Join(r.BinDir(), "npx"), "--version")
	npx.Env = []string{"PATH=" + r.BinDir(), "HOME=" + t.TempDir()}
	if out, err := npx.CombinedOutput(); err != nil {
		t.Fatalf("npx: %s %v", out, err)
	}
}
