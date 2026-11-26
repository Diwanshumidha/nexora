package extension

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"nexora/internal/config"
	"nexora/internal/protocol"
)

const (
	HostVersion = "0.1.0"

	initTimeout = 10 * time.Second
	stopTimeout = 5 * time.Second
)

// Status of an extension in the manager.
type Status string

const (
	StatusStopped Status = "stopped"
	StatusRunning Status = "running"
	StatusError   Status = "error"
)

// Info is the public view of an extension, safe to send to the frontend.
type Info struct {
	Manifest *Manifest           `json:"manifest"`
	Dir      string              `json:"dir"`
	Status   Status              `json:"status"`
	Error    string              `json:"error,omitempty"`
	Tools    []protocol.ToolSpec `json:"tools"`
	// Stale is true when the extension's code changed on disk after it
	// started, so a restart would load a different version.
	Stale bool `json:"stale,omitempty"`
	// Auth is the OAuth state of a remote MCP server.
	Auth AuthState `json:"auth,omitempty"`
}

// NodeRuntime finds the node binary for JS extensions (see
// internal/noderuntime).
type NodeRuntime interface {
	// Node returns the binary, or an error saying why there is none.
	Node() (string, error)
	// BinDir is a directory with node, npm and npx to put first on the PATH
	// of local MCP servers, or "" to leave PATH alone.
	BinDir() string
}

type entry struct {
	// lifecycle serialises Start/Stop/Remove for this extension so two
	// concurrent starts cannot spawn two processes.
	lifecycle sync.Mutex

	// Guarded by Manager.mu.
	info    Info
	live    conn // nil unless running
	removed bool
	// mainMod is the entry file's modification time when the process started.
	mainMod time.Time
	// cancelSignIn aborts a sign-in waiting for the browser.
	cancelSignIn context.CancelFunc
}

// Manager discovers, installs, removes and runs extensions, and routes tool
// calls to them. Each extension runs in its own process.
type Manager struct {
	cfg      *config.Config
	log      *slog.Logger
	settings *settingsStore
	oauth    *oauthStore
	policy   SettingsPolicy
	openURL  func(string) error // opens the browser to sign in
	node     NodeRuntime        // nil: cfg.NodePath, else "node" on PATH

	mu   sync.RWMutex
	exts map[string]*entry // keyed by extension id
	byNS map[string]*entry // keyed by namespace, for tool call routing
}

func NewManager(cfg *config.Config, log *slog.Logger) *Manager {
	settingsDir := cfg.ExtensionSettingsDir
	if settingsDir == "" {
		settingsDir = filepath.Join(filepath.Dir(cfg.ExtensionDataDir), "extension-settings")
	}
	protected := []string{cfg.DataDir, cfg.ExtensionsDir, cfg.ExtensionDataDir, settingsDir}
	return &Manager{
		cfg: cfg, log: log, exts: map[string]*entry{}, byNS: map[string]*entry{},
		settings: &settingsStore{dir: settingsDir},
		oauth:    &oauthStore{dir: filepath.Join(settingsDir, "oauth")},
		policy:   SettingsPolicy{Protected: protected},
	}
}

// SetNodeRuntime makes extensions use rt to find Node.js. Call it before
// starting extensions.
func (m *Manager) SetNodeRuntime(rt NodeRuntime) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.node = rt
}

func (m *Manager) nodePath() (string, error) {
	m.mu.RLock()
	rt := m.node
	m.mu.RUnlock()
	if rt != nil {
		return rt.Node()
	}
	if m.cfg.NodePath != "" {
		return m.cfg.NodePath, nil
	}
	return "node", nil
}

func (m *Manager) nodeBinDir() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.node == nil {
		return ""
	}
	return m.node.BinDir()
}

// RetryNeedingNode starts the extensions that failed and may have failed
// for lack of Node.js: JS extensions and local MCP servers. Call it once a
// runtime becomes available.
func (m *Manager) RetryNeedingNode(ctx context.Context) {
	for _, info := range m.List() {
		man := info.Manifest
		if info.Status != StatusError || (man.Runtime != RuntimeNode && man.Command == "") {
			continue
		}
		go func() {
			if err := m.Start(ctx, man.ID); err != nil {
				m.log.Warn("extension still failing", "id", man.ID, "err", err)
			}
		}()
	}
}

// Discover scans the installed and dev extension folders. Already
// registered extensions are left untouched, so it is safe to call again.
func (m *Manager) Discover() error {
	roots := append([]string{m.cfg.ExtensionsDir}, m.cfg.DevExtensionsDirs...)
	for _, root := range roots {
		children, err := os.ReadDir(root)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return err
		}
		for _, c := range children {
			if !c.IsDir() {
				continue
			}
			dir := filepath.Join(root, c.Name())
			if _, err := os.Stat(filepath.Join(dir, ManifestFile)); err != nil {
				continue // not an extension folder
			}
			if err := m.register(dir); err != nil && !errors.Is(err, errAlreadyRegistered) {
				m.log.Warn("skipping extension", "dir", dir, "err", err)
			}
		}
	}
	return nil
}

var errAlreadyRegistered = errors.New("already registered")

func (m *Manager) register(dir string) error {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	man, err := LoadManifest(dir)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, dup := m.exts[man.ID]; dup {
		if e.info.Dir == dir {
			return errAlreadyRegistered
		}
		return fmt.Errorf("duplicate extension id %q (already loaded from %s)", man.ID, e.info.Dir)
	}
	if e, dup := m.byNS[man.Namespace]; dup {
		return fmt.Errorf("namespace %q already used by %s", man.Namespace, e.info.Manifest.ID)
	}
	e := &entry{info: Info{Manifest: man, Dir: dir, Status: StatusStopped}}
	m.exts[man.ID] = e
	m.byNS[man.Namespace] = e
	return nil
}

func (m *Manager) lookup(id string) (*entry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.exts[id]
	if !ok {
		return nil, fmt.Errorf("unknown extension %q", id)
	}
	return e, nil
}

// SetURLOpener sets how the manager opens the browser for an OAuth sign-in.
func (m *Manager) SetURLOpener(open func(string) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.openURL = open
}

// Start launches an extension process and performs the handshake. It is a
// no-op if the extension is already running. A remote MCP server that needs
// a sign-in fails with ErrSignInRequired: only SignIn opens the browser.
func (m *Manager) Start(ctx context.Context, id string) error {
	e, err := m.lookup(id)
	if err != nil {
		return err
	}
	e.lifecycle.Lock()
	defer e.lifecycle.Unlock()
	return m.startLocked(ctx, e, false)
}

// SignIn (re)connects a remote MCP server, letting it send the user through
// an OAuth sign-in in the browser. It returns once the user finished, or
// CancelSignIn or signInTimeout ended the wait.
func (m *Manager) SignIn(ctx context.Context, id string) error {
	e, err := m.lookup(id)
	if err != nil {
		return err
	}
	if man := e.info.Manifest; man.Runtime != RuntimeMCP || man.URL == "" {
		return fmt.Errorf("%s is not a remote MCP server", id)
	}
	e.lifecycle.Lock()
	defer e.lifecycle.Unlock()
	if err := m.stopLocked(ctx, e); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m.mu.Lock()
	e.cancelSignIn = cancel
	e.info.Auth = AuthPending
	e.info.Error = ""
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		e.cancelSignIn = nil
		m.mu.Unlock()
	}()
	return m.startLocked(ctx, e, true)
}

// CancelSignIn stops waiting for the browser in a pending SignIn.
func (m *Manager) CancelSignIn(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.exts[id]; ok && e.cancelSignIn != nil {
		e.cancelSignIn()
	}
}

// SignOut forgets a remote MCP server's sign-in and disconnects it.
func (m *Manager) SignOut(ctx context.Context, id string) error {
	e, err := m.lookup(id)
	if err != nil {
		return err
	}
	e.lifecycle.Lock()
	defer e.lifecycle.Unlock()
	if err := m.stopLocked(ctx, e); err != nil {
		return err
	}
	if err := m.oauth.remove(id); err != nil {
		return err
	}
	m.mu.Lock()
	e.info.Auth = AuthRequired
	e.info.Status = StatusStopped
	e.info.Error = ""
	m.mu.Unlock()
	return nil
}

// startLocked starts e; the caller must hold e.lifecycle. interactive
// allows an OAuth sign-in in the browser.
func (m *Manager) startLocked(ctx context.Context, e *entry, interactive bool) error {
	id := e.info.Manifest.ID
	m.mu.RLock()
	removed, running := e.removed, e.live != nil
	man, dir := e.info.Manifest, e.info.Dir
	m.mu.RUnlock()
	if removed {
		return fmt.Errorf("unknown extension %q", id)
	}
	if running {
		return nil
	}

	dataDir := filepath.Join(m.cfg.ExtensionDataDir, id)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	stored, err := m.settings.load(id)
	if err != nil {
		m.log.Warn("ignoring unreadable settings", "id", id, "err", err)
		stored = Values{}
	}
	values := resolve(man.Settings, stored)

	var (
		live  conn
		tools []protocol.ToolSpec
	)
	switch man.Runtime {
	case RuntimeMCP:
		var mc *mcpConn
		m.mu.RLock()
		openURL := m.openURL
		m.mu.RUnlock()
		mc, tools, err = connectMCP(ctx, man, mcpOptions{
			Dir: dir, DataDir: dataDir, Specs: man.Settings, Values: values, PathPrefix: m.nodeBinDir(),
			OnToolsChanged: func(tools []protocol.ToolSpec) { m.setTools(e, mc, tools) },
			OAuth: &oauthOptions{
				ID: id, Store: m.oauth, Interactive: interactive, OpenURL: openURL,
				OnSignInRequired: func() { m.signInExpired(e, mc) },
			},
		}, m.log)
		if err != nil {
			signIn := errors.Is(err, ErrSignInRequired)
			m.mu.Lock()
			e.info.Status = StatusError
			e.info.Error = fmt.Sprintf("connect to MCP server %s: %v", id, err)
			e.info.Auth = AuthNone
			if signIn {
				e.info.Error = "Sign in to " + hostOf(man.URL) + " to use this server's tools."
			}
			if signIn || interactive {
				e.info.Auth = AuthRequired
			}
			m.mu.Unlock()
			return fmt.Errorf("connect to MCP server %s: %w", id, err)
		}
		live = mc
		m.mu.Lock()
		e.info.Auth = AuthNone
		if mc.SignedIn() {
			e.info.Auth = AuthSignedIn
		}
		m.mu.Unlock()
	default:
		nodePath, err := m.nodePath()
		if err != nil {
			m.setError(e, err)
			return err
		}
		grantRead, grantWrite := grantedFolders(man.Settings, values)
		proc, err := spawn(man, spawnOptions{
			Dir: dir, DataDir: dataDir, NodePath: nodePath,
			GrantRead: grantRead, GrantWrite: grantWrite,
		}, m.log)
		if err != nil {
			m.setError(e, err)
			return err
		}
		tools, err = m.handshake(ctx, proc, id, dataDir, values)
		if err != nil {
			stopCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
			defer cancel()
			_ = proc.Stop(stopCtx)
			err = fmt.Errorf("initialize %s: %w", id, err)
			m.setError(e, err)
			return err
		}
		live = proc
	}

	m.mu.Lock()
	e.live = live
	e.mainMod = mainModTime(man, dir)
	e.info.Status = StatusRunning
	e.info.Error = ""
	e.info.Tools = m.validTools(id, tools)
	m.mu.Unlock()

	go m.watch(e, live)
	return nil
}

// signInExpired flags a connected server whose token was rejected, so the
// UI offers to sign in again. (During a connect, live is still nil and the
// connect error reports it instead.)
func (m *Manager) signInExpired(e *entry, live *mcpConn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if live == nil || e.live != conn(live) {
		return
	}
	e.info.Auth = AuthRequired
	e.info.Error = "The sign-in expired. Sign in again to keep using this server's tools."
}

// setTools replaces a running extension's tools after it announced a change.
func (m *Manager) setTools(e *entry, live conn, tools []protocol.ToolSpec) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e.live != live {
		return // stopped or restarted since
	}
	e.info.Tools = m.validTools(e.info.Manifest.ID, tools)
	m.log.Info("extension tools changed", "id", e.info.Manifest.ID, "tools", len(e.info.Tools))
}

func (m *Manager) handshake(ctx context.Context, proc *Process, id, dataDir string, values Values) ([]protocol.ToolSpec, error) {
	ctx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()
	var res protocol.InitializeResult
	err := proc.Call(ctx, protocol.MethodInitialize, protocol.InitializeParams{
		HostVersion: HostVersion,
		ExtensionID: id,
		DataDir:     dataDir,
		Settings:    values,
	}, &res)
	return res.Tools, err
}

// validTools drops tools whose names aren't JS identifiers, and duplicates.
func (m *Manager) validTools(id string, in []protocol.ToolSpec) []protocol.ToolSpec {
	tools := make([]protocol.ToolSpec, 0, len(in))
	seen := map[string]bool{}
	for _, t := range in {
		if !ValidToolName(t.Name) || seen[t.Name] {
			m.log.Warn("ignoring invalid or duplicate tool", "extension", id, "tool", t.Name)
			continue
		}
		seen[t.Name] = true
		tools = append(tools, t)
	}
	return tools
}

// watch marks the extension as errored if it exits or disconnects
// unexpectedly.
func (m *Manager) watch(e *entry, live conn) {
	<-live.Done()
	m.mu.Lock()
	defer m.mu.Unlock()
	if e.live != live {
		return // stopped deliberately, or already replaced
	}
	e.live = nil
	e.info.Status = StatusError
	e.info.Error = "extension process exited unexpectedly"
	if e.info.Manifest.Runtime == RuntimeMCP {
		e.info.Error = "MCP server stopped unexpectedly"
	}
	if err := live.ExitErr(); err != nil {
		e.info.Error += ": " + err.Error()
	}
	m.log.Error("extension crashed", "id", e.info.Manifest.ID, "err", live.ExitErr())
}

func (m *Manager) Stop(ctx context.Context, id string) error {
	e, err := m.lookup(id)
	if err != nil {
		return err
	}
	e.lifecycle.Lock()
	defer e.lifecycle.Unlock()
	return m.stopLocked(ctx, e)
}

// stopLocked stops e's process; the caller must hold e.lifecycle.
func (m *Manager) stopLocked(ctx context.Context, e *entry) error {
	m.mu.Lock()
	live := e.live
	e.live = nil
	if e.info.Status == StatusRunning {
		e.info.Status = StatusStopped
	}
	m.mu.Unlock()
	if live == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, stopTimeout)
	defer cancel()
	return live.Stop(ctx)
}

// StartAll starts every discovered extension concurrently, logging failures.
func (m *Manager) StartAll(ctx context.Context) {
	m.forEach(func(id string) {
		if err := m.Start(ctx, id); err != nil {
			m.log.Error("extension failed to start", "id", id, "err", err)
		}
	})
}

// StopAll stops every running extension concurrently.
func (m *Manager) StopAll(ctx context.Context) {
	m.forEach(func(id string) {
		if err := m.Stop(ctx, id); err != nil {
			m.log.Warn("extension failed to stop cleanly", "id", id, "err", err)
		}
	})
}

func (m *Manager) forEach(fn func(id string)) {
	var wg sync.WaitGroup
	for _, info := range m.List() {
		wg.Go(func() { fn(info.Manifest.ID) })
	}
	wg.Wait()
}

// Install copies an extension folder into the user's extensions dir and
// registers it. node_modules and symlinks are skipped: extensions must be
// bundled into self-contained files.
func (m *Manager) Install(srcDir string) (*Info, error) {
	man, err := LoadManifest(srcDir)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(srcDir, man.Main)); man.Runtime == RuntimeNode && err != nil {
		return nil, fmt.Errorf("extension %s is not built: %w", man.ID, err)
	}
	if _, err := m.lookup(man.ID); err == nil {
		return nil, fmt.Errorf("extension %q is already installed", man.ID)
	}
	dst := filepath.Join(m.cfg.ExtensionsDir, man.ID)
	if _, err := os.Stat(dst); err == nil {
		return nil, fmt.Errorf("extension %q is already installed", man.ID)
	}
	if err := copyExtension(dst, srcDir); err != nil {
		_ = os.RemoveAll(dst)
		return nil, fmt.Errorf("copy extension: %w", err)
	}
	if err := m.register(dst); err != nil {
		_ = os.RemoveAll(dst)
		return nil, err
	}
	info, _ := m.Get(man.ID)
	return info, nil
}

// Update replaces an installed extension with the build in srcDir (same
// id), keeping its settings and data. The new files are staged first and
// swapped in with renames, so a failed copy leaves the old version intact. A
// running extension is restarted on the new code.
func (m *Manager) Update(ctx context.Context, srcDir string) (*Info, error) {
	man, err := LoadManifest(srcDir)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(srcDir, man.Main)); man.Runtime == RuntimeNode && err != nil {
		return nil, fmt.Errorf("extension %s is not built: %w", man.ID, err)
	}
	e, err := m.lookup(man.ID)
	if err != nil {
		return nil, err
	}

	e.lifecycle.Lock()
	m.mu.RLock()
	dir, wasRunning, oldNS := e.info.Dir, e.live != nil, e.info.Manifest.Namespace
	other := m.byNS[man.Namespace]
	m.mu.RUnlock()
	if filepath.Dir(dir) != filepath.Clean(m.cfg.ExtensionsDir) {
		e.lifecycle.Unlock()
		return nil, fmt.Errorf("%s is a development extension (%s); rebuild it there instead", man.ID, dir)
	}
	if other != nil && other != e {
		e.lifecycle.Unlock()
		return nil, fmt.Errorf("namespace %q already used by %s", man.Namespace, other.info.Manifest.ID)
	}

	staging, old := dir+".new", dir+".old"
	_ = os.RemoveAll(staging)
	_ = os.RemoveAll(old)
	if err := copyExtension(staging, srcDir); err != nil {
		_ = os.RemoveAll(staging)
		e.lifecycle.Unlock()
		return nil, fmt.Errorf("copy extension: %w", err)
	}
	if err := m.stopLocked(ctx, e); err != nil {
		m.log.Warn("stop before update failed", "id", man.ID, "err", err)
	}
	if err := os.Rename(dir, old); err != nil {
		_ = os.RemoveAll(staging)
		e.lifecycle.Unlock()
		return nil, err
	}
	if err := os.Rename(staging, dir); err != nil {
		_ = os.Rename(old, dir)
		_ = os.RemoveAll(staging)
		e.lifecycle.Unlock()
		return nil, err
	}
	_ = os.RemoveAll(old)

	m.mu.Lock()
	e.info.Manifest = man
	e.info.Error = ""
	e.info.Tools = nil
	if e.info.Status == StatusError {
		e.info.Status = StatusStopped
	}
	delete(m.byNS, oldNS)
	m.byNS[man.Namespace] = e
	m.mu.Unlock()
	e.lifecycle.Unlock()

	if wasRunning {
		if err := m.Start(ctx, man.ID); err != nil {
			info, _ := m.Get(man.ID)
			return info, err
		}
	}
	info, _ := m.Get(man.ID)
	return info, nil
}

// Create installs an extension that has no files besides its manifest,
// such as an MCP server added from the UI, with initial setting values.
func (m *Manager) Create(man *Manifest, values Values) (*Info, error) {
	if man.Runtime == RuntimeNode {
		return nil, errors.New("node extensions need their code; install them from a folder")
	}
	if err := man.Validate(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	_, idTaken := m.exts[man.ID]
	nsOwner := m.byNS[man.Namespace]
	m.mu.RUnlock()
	dst := filepath.Join(m.cfg.ExtensionsDir, man.ID)
	if _, err := os.Stat(dst); idTaken || err == nil {
		return nil, fmt.Errorf("an extension called %q already exists; pick another name", man.ID)
	}
	if nsOwner != nil {
		return nil, fmt.Errorf("tools.%s is already used by %s; pick another namespace", man.Namespace, nsOwner.info.Manifest.Name)
	}
	values, err := m.policy.Validate(man.Settings, values)
	if err != nil {
		return nil, err
	}
	raw, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return nil, err
	}
	err = os.WriteFile(filepath.Join(dst, ManifestFile), append(raw, '\n'), 0o600)
	if err == nil {
		err = m.settings.save(man.ID, values)
	}
	if err == nil {
		err = m.register(dst)
	}
	if err != nil {
		_ = os.RemoveAll(dst)
		_ = m.settings.remove(man.ID)
		return nil, err
	}
	info, _ := m.Get(man.ID)
	return info, nil
}

// Installed reports whether id lives in the installed-extensions folder (as
// opposed to a development folder, which Nexora never overwrites).
func (m *Manager) Installed(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.exts[id]
	return ok && filepath.Dir(e.info.Dir) == filepath.Clean(m.cfg.ExtensionsDir)
}

func copyExtension(dst, src string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir() && (d.Name() == "node_modules" || d.Name() == ".git"):
			return filepath.SkipDir
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case !d.Type().IsRegular():
			return nil // symlinks, devices, ...
		}
		return copyFile(target, path)
	})
}

func copyFile(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Remove stops and unregisters an extension. Installed extensions are
// deleted from disk; dev extensions are left in place.
func (m *Manager) Remove(ctx context.Context, id string) error {
	e, err := m.lookup(id)
	if err != nil {
		return err
	}
	e.lifecycle.Lock()
	defer e.lifecycle.Unlock()

	if err := m.stopLocked(ctx, e); err != nil {
		m.log.Warn("stop before remove failed", "id", id, "err", err)
	}
	m.mu.Lock()
	e.removed = true
	delete(m.exts, id)
	delete(m.byNS, e.info.Manifest.Namespace)
	m.mu.Unlock()

	if err := m.settings.remove(id); err != nil {
		m.log.Warn("removing settings", "id", id, "err", err)
	}
	if err := m.oauth.remove(id); err != nil {
		m.log.Warn("removing sign-in", "id", id, "err", err)
	}
	if filepath.Dir(e.info.Dir) == filepath.Clean(m.cfg.ExtensionsDir) {
		return os.RemoveAll(e.info.Dir)
	}
	return nil
}

// Settings returns an extension's setting specs and current values.
func (m *Manager) Settings(id string) ([]SettingSpec, Values, error) {
	e, err := m.lookup(id)
	if err != nil {
		return nil, nil, err
	}
	stored, err := m.settings.load(id)
	if err != nil {
		return nil, nil, err
	}
	specs := e.info.Manifest.Settings
	return specs, resolve(specs, stored), nil
}

// SetSettings validates and saves new values, then applies them: settings
// that grant folder access restart the extension (its sandbox is fixed at
// launch), as does any change to an MCP server; others are delivered to the
// running process. Saving settings of an MCP server that failed to start
// retries the start.
func (m *Manager) SetSettings(ctx context.Context, id string, in Values) (Values, error) {
	e, err := m.lookup(id)
	if err != nil {
		return nil, err
	}
	specs := e.info.Manifest.Settings
	values, err := m.policy.Validate(specs, in)
	if err != nil {
		return nil, err
	}
	stored, err := m.settings.load(id)
	if err != nil {
		stored = Values{}
	}
	prev := resolve(specs, stored)
	if err := m.settings.save(id, values); err != nil {
		return nil, err
	}

	m.mu.RLock()
	live, status, runtime := e.live, e.info.Status, e.info.Manifest.Runtime
	m.mu.RUnlock()
	if live == nil {
		if status == StatusError && runtime == RuntimeMCP {
			// Typically a missing token: try again with the new values.
			return values, m.Start(ctx, id)
		}
		return values, nil
	}
	oldR, oldW := grantedFolders(specs, prev)
	newR, newW := grantedFolders(specs, values)
	err = errRestartRequired
	if slices.Equal(oldR, newR) && slices.Equal(oldW, newW) {
		err = live.UpdateSettings(ctx, values)
	}
	if errors.Is(err, errRestartRequired) {
		if err := m.Stop(ctx, id); err != nil {
			return values, err
		}
		return values, m.Start(ctx, id)
	}
	return values, err
}

func (m *Manager) Get(id string) (*Info, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.exts[id]
	if !ok {
		return nil, false
	}
	info := m.infoLocked(e)
	return &info, true
}

// infoLocked returns e's public view, flagging code changed since start.
// Caller holds m.mu (read or write).
func (m *Manager) infoLocked(e *entry) Info {
	info := e.info
	if e.live != nil && !e.mainMod.IsZero() {
		info.Stale = mainModTime(e.info.Manifest, e.info.Dir).After(e.mainMod)
	}
	return info
}

func mainModTime(man *Manifest, dir string) time.Time {
	if man.Main == "" {
		return time.Time{}
	}
	fi, err := os.Stat(filepath.Join(dir, man.Main))
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

func (m *Manager) List() []Info {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Info, 0, len(m.exts))
	for _, e := range m.exts {
		out = append(out, m.infoLocked(e))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Manifest.ID < out[j].Manifest.ID })
	return out
}

// CallTool routes tools.<namespace>.<tool>(args) to the owning extension.
// args must be valid JSON; it is forwarded to the extension unchanged.
func (m *Manager) CallTool(ctx context.Context, namespace, tool string, args json.RawMessage) (json.RawMessage, error) {
	m.mu.RLock()
	e := m.byNS[namespace]
	var live conn
	if e != nil {
		live = e.live
	}
	m.mu.RUnlock()
	switch {
	case e == nil:
		return nil, fmt.Errorf("no extension provides namespace %q", namespace)
	case live == nil:
		return nil, fmt.Errorf("extension %q is not running", e.info.Manifest.ID)
	}
	if len(args) == 0 {
		args = json.RawMessage("{}")
	} else if !json.Valid(args) {
		// Invalid JSON would corrupt the frame stream for every caller.
		return nil, errors.New("tool arguments are not valid JSON")
	}
	return live.CallTool(ctx, tool, args)
}

// Tool resolves tools.<namespace>.<tool> to its extension and spec. ok is
// false if no extension provides the namespace; found is false if the
// extension does not list the tool (e.g. it is not running).
func (m *Manager) Tool(namespace, tool string) (extID string, spec protocol.ToolSpec, ok, found bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e := m.byNS[namespace]
	if e == nil {
		return "", protocol.ToolSpec{}, false, false
	}
	for _, t := range e.info.Tools {
		if t.Name == tool {
			return e.info.Manifest.ID, t, true, true
		}
	}
	return e.info.Manifest.ID, protocol.ToolSpec{}, true, false
}

func (m *Manager) setError(e *entry, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e.info.Status = StatusError
	e.info.Error = err.Error()
}
