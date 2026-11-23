// Package config resolves Nexora's on-disk locations and settings.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const AppName = "nexora"

type Config struct {
	// DataDir is the root of all user data (~/.config/nexora on Linux).
	DataDir string
	// ExtensionsDir holds installed extensions, one folder per extension id.
	ExtensionsDir string
	// ExtensionDataDir holds per-extension private storage.
	ExtensionDataDir string
	// ExtensionSettingsDir holds the user's settings for each extension.
	ExtensionSettingsDir string
	// DevExtensionsDirs are additional folders scanned for extensions, used
	// while developing (e.g. ./extensions in the repo).
	DevExtensionsDirs []string
	// NodePath is the Node.js binary used to run JS extensions.
	NodePath string

	// Workers is the number of code-mode jobs executed concurrently.
	Workers int
	// MaxQueued caps jobs waiting for a worker.
	MaxQueued int
	// JobTimeout bounds a single job's runtime.
	JobTimeout time.Duration

	// MCPAddr is where the MCP endpoint listens; empty disables it.
	MCPAddr string
	// MCPTokenFile holds the MCP access token.
	MCPTokenFile string
	// MCPAllowedOrigins are browser origins allowed to call the endpoint
	// (normally none; MCP clients are not web pages).
	MCPAllowedOrigins []string
	// NetworkFile persists the active network provider.
	NetworkFile string
	// HistoryFile records finished runs.
	HistoryFile string
	// PermissionsFile holds per-extension modes and per-tool overrides.
	PermissionsFile string
	// ApprovalTimeout is how long a tool call waits for the user's OK.
	ApprovalTimeout time.Duration
	// RegistryURL is the extension catalog (index.json) the "+" panel
	// installs from; empty hides the catalog.
	RegistryURL string
	// MCPInlineWait is how long an MCP call waits for a result before
	// returning a job ID. Keep it below any proxy timeout (e.g. 100 s).
	MCPInlineWait time.Duration
}

// DefaultRegistryURL is the published extension catalog: the index.json
// asset of the nexora-extensions repo's latest GitHub release, e.g.
// "https://github.com/<owner>/nexora-extensions/releases/latest/download/index.json".
// Empty until that repo is published; NEXORA_REGISTRY_URL overrides it.
const DefaultRegistryURL = ""

func Load() (*Config, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	data := filepath.Join(base, AppName)
	cfg := &Config{
		DataDir:              data,
		ExtensionsDir:        filepath.Join(data, "extensions"),
		ExtensionDataDir:     filepath.Join(data, "extension-data"),
		ExtensionSettingsDir: filepath.Join(data, "extension-settings"),
		NodePath:             envOr("NEXORA_NODE", "node"),
		MCPAddr:              envOr("NEXORA_MCP_ADDR", "127.0.0.1:7377"),
		MCPTokenFile:         filepath.Join(data, "mcp-token"),
		NetworkFile:          filepath.Join(data, "network.json"),
		HistoryFile:          filepath.Join(data, "history.jsonl"),
		PermissionsFile:      filepath.Join(data, "permissions.json"),
		RegistryURL:          envOr("NEXORA_REGISTRY_URL", DefaultRegistryURL),
	}
	if cfg.MCPAddr == "off" {
		cfg.MCPAddr = ""
	}
	if v := os.Getenv("NEXORA_MCP_ALLOWED_ORIGINS"); v != "" {
		cfg.MCPAllowedOrigins = strings.Split(v, ",")
	}
	if cfg.Workers, err = envInt("NEXORA_WORKERS", 4); err != nil {
		return nil, err
	}
	if cfg.MaxQueued, err = envInt("NEXORA_MAX_QUEUED", 100); err != nil {
		return nil, err
	}
	if cfg.JobTimeout, err = envDuration("NEXORA_JOB_TIMEOUT", 15*time.Minute); err != nil {
		return nil, err
	}
	if cfg.MCPInlineWait, err = envDuration("NEXORA_MCP_INLINE_WAIT", 80*time.Second); err != nil {
		return nil, err
	}
	if cfg.ApprovalTimeout, err = envDuration("NEXORA_APPROVAL_TIMEOUT", 5*time.Minute); err != nil {
		return nil, err
	}
	for _, dir := range filepath.SplitList(os.Getenv("NEXORA_DEV_EXTENSIONS")) {
		if dir == "" {
			continue
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			return nil, err
		}
		cfg.DevExtensionsDirs = append(cfg.DevExtensionsDirs, abs)
	}
	for _, dir := range []string{cfg.ExtensionsDir, cfg.ExtensionDataDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer, got %q", key, v)
	}
	return n, nil
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration like 80s or 15m, got %q", key, v)
	}
	return d, nil
}
