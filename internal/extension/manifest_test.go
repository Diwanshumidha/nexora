package extension

import (
	"strings"
	"testing"
)

func TestManifestValidate(t *testing.T) {
	base := func() Manifest {
		return Manifest{ID: "ok-ext", Namespace: "okExt", Runtime: RuntimeNode, Main: "dist/index.js"}
	}
	cases := map[string]func(*Manifest){
		"bad id":          func(m *Manifest) { m.ID = "Bad_ID" },
		"bad namespace":   func(m *Manifest) { m.Namespace = "not-valid" },
		"no main":         func(m *Manifest) { m.Main = "" },
		"main escapes":    func(m *Manifest) { m.Main = "../../evil.js" },
		"absolute main":   func(m *Manifest) { m.Main = "/etc/evil.js" },
		"unknown runtime": func(m *Manifest) { m.Runtime = "python" },
		"mcp no command":  func(m *Manifest) { m.Runtime = RuntimeMCP },
		"mcp command and url": func(m *Manifest) {
			m.Runtime, m.Command, m.URL = RuntimeMCP, "npx", "https://example.com/mcp"
		},
		"mcp url scheme": func(m *Manifest) { m.Runtime, m.URL = RuntimeMCP, "file:///etc/passwd" },
		"mcp unknown ref": func(m *Manifest) {
			m.Runtime, m.Command, m.Env = RuntimeMCP, "npx", map[string]string{"TOKEN": "${tokn}"}
		},
	}
	ok := base()
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
	for name, mutate := range cases {
		m := base()
		mutate(&m)
		if m.Validate() == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestMCPManifestValid(t *testing.T) {
	for _, m := range []Manifest{
		{ID: "gh", Runtime: RuntimeMCP, Command: "npx", Args: []string{"-y", "server", "${DATA}"},
			Env: map[string]string{"TOKEN": "${token}"}, Settings: []SettingSpec{{Key: "token", Type: SettingSecret, Label: "Token"}}},
		{ID: "remote", Runtime: RuntimeMCP, URL: "https://example.com/mcp", Headers: map[string]string{"Authorization": "Bearer ${HOME}"}},
	} {
		if err := m.Validate(); err != nil {
			t.Errorf("%s: %v", m.ID, err)
		}
	}
}

func TestJSToolName(t *testing.T) {
	for in, want := range map[string]string{
		"get_issue": "get_issue", "get-issue": "get_issue", "github.search": "github_search",
		"2fa": "_2fa", "": "_", "camelCase": "camelCase",
	} {
		if got := jsToolName(in); got != want {
			t.Errorf("jsToolName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReservedNamespace(t *testing.T) {
	m := &Manifest{ID: "store", Runtime: RuntimeNode, Main: "dist/index.js"}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "built into Nexora") {
		t.Fatalf("Validate = %v", err)
	}
	// An MCP server named "Store" gets a namespace that doesn't clash.
	man, _, err := NewMCPManifest(MCPServerSpec{Name: "Store", CommandLine: "store-mcp"})
	if err != nil || man.Namespace != "storeServer" {
		t.Fatalf("namespace = %q, err %v", man.Namespace, err)
	}
}
