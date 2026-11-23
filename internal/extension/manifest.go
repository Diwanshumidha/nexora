package extension

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// ManifestFile is the file every extension folder must contain.
const ManifestFile = "nexora.json"

// HostProtocol is the newest extension protocol this build speaks. Bump it
// when the SDK contract changes incompatibly; older extensions keep working
// as long as the host still supports their version.
const HostProtocol = 1

// Runtime identifies how an extension is executed.
type Runtime string

const (
	// RuntimeNode runs a JS extension built on @nexora/extension-sdk in its
	// own Node.js process.
	RuntimeNode Runtime = "node"
	// RuntimeMCP wraps an external MCP server (stdio) as an extension, so its
	// tools appear under tools.<namespace>.* like any other extension.
	RuntimeMCP Runtime = "mcp"
)

// Manifest is the static description of an extension (nexora.json).
type Manifest struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
	// Protocol is the host protocol the extension was built for (the SDK's
	// major contract). Missing means 1.
	Protocol    int    `json:"protocol,omitempty"`
	Description string `json:"description,omitempty"`
	Author      string `json:"author,omitempty"`
	Icon        string `json:"icon,omitempty"`

	// Namespace is the key under `tools` in code mode, e.g. "excel" ->
	// tools.excel.readSheet(). Defaults to ID.
	Namespace string `json:"namespace,omitempty"`

	Runtime Runtime `json:"runtime"`
	// Main is the entry file for RuntimeNode, relative to the extension dir.
	Main string `json:"main,omitempty"`
	// Command/Args/Env launch an external server for RuntimeMCP.
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`

	Permissions Permissions `json:"permissions"`

	// Settings are user-configurable options shown in the app.
	Settings []SettingSpec `json:"settings,omitempty"`
}

// Permissions are enforced by the host when spawning the extension process
// (via the Node.js permission model for RuntimeNode). Anything not granted
// is denied.
type Permissions struct {
	// FSRead / FSWrite are path globs the extension may access. "$HOME",
	// "$DATA" (the extension's private data dir) are expanded.
	FSRead  []string `json:"fsRead,omitempty"`
	FSWrite []string `json:"fsWrite,omitempty"`
	// Network allows outbound network access.
	Network bool `json:"network,omitempty"`
	// ChildProcess allows spawning subprocesses.
	ChildProcess bool `json:"childProcess,omitempty"`
}

var (
	idPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,63}$`)
	// Namespaces and tool names must be JS identifiers so the generated API
	// reads as tools.excel.readSheet().
	identPattern = regexp.MustCompile(`^[a-zA-Z_$][a-zA-Z0-9_$]*$`)
)

// ValidToolName reports whether name can be used as a tool method name.
func ValidToolName(name string) bool { return identPattern.MatchString(name) }

func LoadManifest(dir string) (*Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", ManifestFile, err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate applies defaults (namespace = id) and checks the manifest.
func (m *Manifest) Validate() error {
	if m.Namespace == "" {
		m.Namespace = m.ID
	}
	if m.Protocol == 0 {
		m.Protocol = 1
	}
	if m.Protocol > HostProtocol {
		return fmt.Errorf("extension %s needs a newer Nexora (protocol %d; this version supports %d)", m.ID, m.Protocol, HostProtocol)
	}
	if !idPattern.MatchString(m.ID) {
		return fmt.Errorf("invalid extension id %q", m.ID)
	}
	if !identPattern.MatchString(m.Namespace) {
		return fmt.Errorf("invalid namespace %q (must be a JS identifier)", m.Namespace)
	}
	seen := map[string]bool{}
	for _, s := range m.Settings {
		if err := s.validate(); err != nil {
			return fmt.Errorf("extension %s: %w", m.ID, err)
		}
		if seen[s.Key] {
			return fmt.Errorf("extension %s: duplicate setting %q", m.ID, s.Key)
		}
		seen[s.Key] = true
	}
	switch m.Runtime {
	case RuntimeNode:
		if m.Main == "" {
			return fmt.Errorf("extension %s: runtime node requires main", m.ID)
		}
		// main must stay inside the extension folder.
		if !filepath.IsLocal(m.Main) {
			return fmt.Errorf("extension %s: main %q must be a relative path inside the extension", m.ID, m.Main)
		}
	case RuntimeMCP:
		if m.Command == "" {
			return fmt.Errorf("extension %s: runtime mcp requires command", m.ID)
		}
	default:
		return fmt.Errorf("extension %s: unknown runtime %q", m.ID, m.Runtime)
	}
	return nil
}
