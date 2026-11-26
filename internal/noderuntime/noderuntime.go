// Package noderuntime finds the Node.js that runs JS extensions, and can
// download a pinned, checksum-verified Node.js when there is none.
//
// Like VS Code, which always runs extensions on the Node it ships, Nexora
// prefers a runtime it manages: one known version, independent of what (if
// anything) the user has installed. It is downloaded on request, not
// bundled, to keep the installer small. Lookup order:
//
//  1. NEXORA_NODE, if set (an explicit choice: never second-guessed).
//  2. The managed runtime in <data>/runtime/node-<version>-<platform>.
//  3. node on PATH, if it is new enough.
//
// "New enough" matters: extensions run under the Node permission model
// (--permission), which older versions reject or name differently.
package noderuntime

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Version is the managed Node.js release (an LTS line). To bump it, update
// checksums from https://nodejs.org/dist/<version>/SHASUMS256.txt.
const Version = "v26.13.1"

// checksums are the SHA-256 of each platform's archive for Version, copied
// from the release's SHASUMS256.txt, so a download can't be swapped even
// through a mirror.
var checksums = map[string]string{
	"linux-x64":    "518674de2b621728a5e4486f5fb3b7d2900bb3cc4aa97969edaa463db99f9778",
	"linux-arm64":  "80a330b35415382878df18d41afbd106ea89bfb3be0087d6608e289a14c5cc7d",
	"darwin-x64":   "82bb96078a24ca50b9c9a38f2379e6be6d87171fcd400fc14c8934b3bce96e2f",
	"darwin-arm64": "aec2514ea1ceb2ba52455e9ec65986f6749a06dd759e5db2a72e6889668c9735",
	"win-x64":      "e2edda75bfb089654705cf65a13b58933bfd5f57148e8ebda916f58a2474f1f7",
	"win-arm64":    "181ebe431b90787113a9e4fc80d38ce86d37888a353a0386de44d2872e48cb97",
}

// MinVersion is the oldest Node that runs extensions: the permission model
// became stable (--permission, no "experimental") in 22.13.
var MinVersion = semver{22, 13, 0}

// DefaultMirror is where releases are downloaded from; NEXORA_NODE_MIRROR
// overrides it (the checksums still apply).
const DefaultMirror = "https://nodejs.org/dist"

// State of the runtime.
type State string

const (
	StateReady       State = "ready"
	StateMissing     State = "missing"  // no Node found
	StateOutdated    State = "outdated" // a Node was found but is too old
	StateDownloading State = "downloading"
	StateError       State = "error" // NEXORA_NODE is broken, or a download failed
)

// Source of the Node in use.
const (
	SourceCustom  = "custom" // NEXORA_NODE
	SourceManaged = "managed"
	SourceSystem  = "system"
)

// Status is the runtime as shown in the UI.
type Status struct {
	State   State  `json:"state"`
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
	Source  string `json:"source,omitempty"`
	// Found is the version of a Node that was found but is too old.
	Found    string `json:"found,omitempty"`
	Required string `json:"required"`
	// Pinned is the version a download installs; empty when this platform
	// has no download.
	Pinned string `json:"pinned,omitempty"`
	// Downloaded and Total (bytes; Total 0 if unknown) while downloading.
	Downloaded int64  `json:"downloaded,omitempty"`
	Total      int64  `json:"total,omitempty"`
	Error      string `json:"error,omitempty"`
}

// ErrUnavailable is returned by Node when no usable Node exists.
var ErrUnavailable = errors.New("Node.js is not available")

type Runtime struct {
	dir      string // <data>/runtime
	override string // NEXORA_NODE
	mirror   string
	client   *http.Client
	log      *slog.Logger

	mu     sync.Mutex
	status Status
	cancel context.CancelFunc

	// OnReady, if set, is called (in its own goroutine) when a download or
	// a later Resolve makes the runtime ready.
	OnReady func()
}

// New prepares a runtime; call Resolve before use.
func New(dir, override, mirror string, log *slog.Logger) *Runtime {
	if mirror == "" {
		mirror = DefaultMirror
	}
	r := &Runtime{
		dir: dir, override: override, mirror: strings.TrimSuffix(mirror, "/"),
		client: &http.Client{}, log: log.With("component", "node"),
	}
	r.status = r.base()
	return r
}

func (r *Runtime) base() Status {
	s := Status{Required: ">= " + MinVersion.String(), State: StateMissing}
	if _, ok := checksums[platform()]; ok {
		s.Pinned = Version
	}
	return s
}

// Resolve looks for a usable Node (see the package doc) and records it.
func (r *Runtime) Resolve(ctx context.Context) Status {
	s := r.find(ctx)
	r.mu.Lock()
	if r.status.State == StateDownloading {
		defer r.mu.Unlock()
		return r.status // the download reports when it's done
	}
	became := r.status.State != StateReady && s.State == StateReady
	r.status = s
	onReady := r.OnReady
	r.mu.Unlock()
	if became && onReady != nil {
		go onReady() // e.g. the user installed Node and checked again
	}
	return s
}

func (r *Runtime) find(ctx context.Context) Status {
	s := r.base()
	if r.override != "" {
		v, err := probe(ctx, r.override)
		switch {
		case err != nil:
			s.State, s.Error = StateError, fmt.Sprintf("NEXORA_NODE=%s does not run: %v", r.override, err)
		case v.less(MinVersion):
			s.State, s.Found = StateOutdated, v.String()
			s.Error = fmt.Sprintf("NEXORA_NODE=%s is Node %s; extensions need %s or newer", r.override, v, MinVersion)
		default:
			s.State, s.Path, s.Version, s.Source = StateReady, r.override, v.String(), SourceCustom
		}
		return s
	}
	if p := r.managedNode(); p != "" {
		if v, err := probe(ctx, p); err == nil {
			s.State, s.Path, s.Version, s.Source = StateReady, p, v.String(), SourceManaged
			return s
		} else {
			r.log.Warn("managed Node.js does not run; ignoring it", "path", p, "err", err)
		}
	}
	if p, err := exec.LookPath("node"); err == nil {
		v, err := probe(ctx, p)
		switch {
		case err != nil:
			r.log.Warn("node on PATH does not run", "path", p, "err", err)
		case v.less(MinVersion):
			s.State, s.Found = StateOutdated, v.String()
		default:
			s.State, s.Path, s.Version, s.Source = StateReady, p, v.String(), SourceSystem
		}
	}
	return s
}

// Status returns the last resolved or download status.
func (r *Runtime) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

// Node returns the node binary to run extensions with.
func (r *Runtime) Node() (string, error) {
	s := r.Status()
	if s.State == StateReady {
		return s.Path, nil
	}
	return "", fmt.Errorf("%w: %s", ErrUnavailable, explain(s))
}

// BinDir is the managed runtime's directory of node, npm and npx, to put
// on the PATH of local MCP servers (most start with npx). Empty unless the
// managed runtime is in use.
func (r *Runtime) BinDir() string {
	s := r.Status()
	if s.State != StateReady || s.Source != SourceManaged {
		return ""
	}
	return filepath.Dir(s.Path)
}

func explain(s Status) string {
	switch {
	case s.Error != "":
		return s.Error
	case s.State == StateOutdated:
		return fmt.Sprintf("Node %s is installed but extensions need %s or newer; download the runtime from the banner", s.Found, MinVersion)
	case s.State == StateDownloading:
		return "the Node.js runtime is still downloading"
	default:
		return "extensions need Node.js; download the runtime from the banner"
	}
}

// Download fetches, verifies and unpacks the pinned runtime in the
// background. Progress and the outcome are reported through Status.
func (r *Runtime) Download() error {
	plat := platform()
	sum, ok := checksums[plat]
	if !ok {
		return fmt.Errorf("no Node.js download for %s; install Node %s or newer yourself", plat, MinVersion)
	}
	r.mu.Lock()
	if r.status.State == StateDownloading {
		r.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	prev := r.status
	r.status = r.base()
	r.status.State, r.status.Found = StateDownloading, prev.Found
	r.mu.Unlock()

	go func() {
		defer cancel()
		err := r.install(ctx, plat, sum)
		s := r.find(context.Background())
		if err == nil && s.State != StateReady {
			err = errors.New("the downloaded runtime does not run on this computer")
		}
		if err != nil {
			s.State, s.Error = StateError, err.Error()
			if errors.Is(err, context.Canceled) {
				s = r.find(context.Background()) // back to how it was
			}
			r.log.Warn("Node.js download failed", "err", err)
		} else {
			r.log.Info("Node.js runtime installed", "version", s.Version, "path", s.Path)
		}
		r.mu.Lock()
		r.status, r.cancel = s, nil
		r.mu.Unlock()
		if err == nil && r.OnReady != nil {
			go r.OnReady()
		}
	}()
	return nil
}

// Cancel stops a running download.
func (r *Runtime) Cancel() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel != nil {
		r.cancel()
	}
}

func (r *Runtime) install(ctx context.Context, plat, sum string) error {
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return err
	}
	name := archiveName(plat)
	archive, err := os.CreateTemp(r.dir, ".download-*")
	if err != nil {
		return err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()

	if err := r.fetch(ctx, r.mirror+"/"+Version+"/"+name, archive, sum); err != nil {
		return err
	}

	tmp, err := os.MkdirTemp(r.dir, ".extract-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if strings.HasSuffix(name, ".zip") {
		err = extractZip(archive.Name(), tmp)
	} else {
		err = extractTarGz(archive.Name(), tmp)
	}
	if err != nil {
		return fmt.Errorf("unpacking %s: %w", name, err)
	}

	final := filepath.Join(r.dir, folderName(plat))
	if err := os.RemoveAll(final); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(tmp, folderName(plat)), final); err != nil {
		return fmt.Errorf("installing the runtime: %w", err)
	}
	r.removeOld(final)
	return nil
}

// fetch downloads url into w, reporting progress, and checks its SHA-256.
func (r *Runtime) fetch(ctx context.Context, url string, w io.Writer, sum string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Nexora")
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("downloading Node.js: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading Node.js: %s returned %s", url, resp.Status)
	}
	r.mu.Lock()
	r.status.Total = max(resp.ContentLength, 0)
	r.mu.Unlock()

	h := sha256.New()
	pw := &progress{r: r}
	// A stalled connection would otherwise hang forever.
	body := &idleReader{r: resp.Body, timeout: 60 * time.Second, cancel: func() { resp.Body.Close() }}
	if _, err := io.Copy(io.MultiWriter(w, h, pw), body); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("downloading Node.js: %w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sum {
		return fmt.Errorf("the download is corrupted or was tampered with (checksum %s, expected %s)", got[:12], sum[:12])
	}
	return nil
}

// removeOld deletes other managed versions once a new one is installed.
func (r *Runtime) removeOld(keep string) {
	entries, _ := os.ReadDir(r.dir)
	for _, e := range entries {
		p := filepath.Join(r.dir, e.Name())
		if e.IsDir() && strings.HasPrefix(e.Name(), "node-") && p != keep {
			_ = os.RemoveAll(p)
		}
	}
}

func (r *Runtime) managedNode() string {
	plat := platform()
	if _, ok := checksums[plat]; !ok {
		return ""
	}
	p := filepath.Join(r.dir, folderName(plat), "bin", "node")
	if runtime.GOOS == "windows" {
		p = filepath.Join(r.dir, folderName(plat), "node.exe")
	}
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

type progress struct{ r *Runtime }

func (p *progress) Write(b []byte) (int, error) {
	p.r.mu.Lock()
	p.r.status.Downloaded += int64(len(b))
	p.r.mu.Unlock()
	return len(b), nil
}

// idleReader fails a read that gets no data for timeout.
type idleReader struct {
	r       io.Reader
	timeout time.Duration
	cancel  func()
}

func (i *idleReader) Read(b []byte) (int, error) {
	t := time.AfterFunc(i.timeout, i.cancel)
	defer t.Stop()
	return i.r.Read(b)
}

/* ------------------------------------------------------------ platforms */

// platform is Node's name for this OS and CPU, e.g. "linux-x64".
func platform() string { return platformOf(runtime.GOOS, runtime.GOARCH) }

func platformOf(goos, goarch string) string {
	osName := map[string]string{"linux": "linux", "darwin": "darwin", "windows": "win"}[goos]
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[goarch]
	if osName == "" || arch == "" {
		return goos + "-" + goarch
	}
	return osName + "-" + arch
}

func folderName(plat string) string { return "node-" + Version + "-" + plat }

func archiveName(plat string) string {
	if strings.HasPrefix(plat, "win-") {
		return folderName(plat) + ".zip"
	}
	return folderName(plat) + ".tar.gz"
}

/* ------------------------------------------------------------- versions */

type semver [3]int

func (v semver) less(o semver) bool {
	for i := range v {
		if v[i] != o[i] {
			return v[i] < o[i]
		}
	}
	return false
}

func (v semver) String() string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }

func parseVersion(s string) (semver, error) {
	parts := strings.SplitN(strings.TrimPrefix(strings.TrimSpace(s), "v"), ".", 3)
	if len(parts) != 3 {
		return semver{}, fmt.Errorf("unexpected version %q", s)
	}
	var v semver
	for i, p := range parts {
		n, err := strconv.Atoi(strings.SplitN(p, "-", 2)[0])
		if err != nil {
			return semver{}, fmt.Errorf("unexpected version %q", s)
		}
		v[i] = n
	}
	return v, nil
}

// probe runs `node --version`.
func probe(ctx context.Context, node string) (semver, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, node, "--version").Output()
	if err != nil {
		return semver{}, err
	}
	return parseVersion(string(out))
}

/* ------------------------------------------------------------- archives */

// within returns dest/name, refusing names that escape dest.
func within(dest, name string) (string, error) {
	p := filepath.Join(dest, filepath.FromSlash(name))
	if p != dest && !strings.HasPrefix(p, dest+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe path %q in archive", name)
	}
	return p, nil
}

func extractTarGz(src, dest string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		p, err := within(dest, h.Name)
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := writeFile(p, tr, os.FileMode(h.Mode)&0o777); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// e.g. bin/npm -> ../lib/node_modules/npm/bin/npm-cli.js
			if filepath.IsAbs(h.Linkname) {
				return fmt.Errorf("unsafe link %q in archive", h.Name)
			}
			if _, err := within(dest, filepath.Join(filepath.Dir(h.Name), h.Linkname)); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(h.Linkname, p); err != nil {
				return err
			}
		}
	}
}

func extractZip(src, dest string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		p, err := within(dest, f.Name)
		if err != nil {
			return err
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = writeFile(p, rc, 0o755)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func writeFile(p string, r io.Reader, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode|0o200)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
