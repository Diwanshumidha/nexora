package extension

import (
	"slices"
	"testing"
)

func TestSplitCommandLine(t *testing.T) {
	for in, want := range map[string][]string{
		`npx -y @modelcontextprotocol/server-github`:          {"npx", "-y", "@modelcontextprotocol/server-github"},
		`uvx  mcp-server-git --repository "/home/me/my repo"`: {"uvx", "mcp-server-git", "--repository", "/home/me/my repo"},
		`node 'a "b"' c\ d ""`:                                {"node", `a "b"`, "c d", ""},
		`C:\Tools\server.exe --flag`:                          {`C:\Tools\server.exe`, "--flag"},
		`echo "say \"hi\""`:                                   {"echo", `say "hi"`},
	} {
		got, err := SplitCommandLine(in)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("SplitCommandLine(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "   ", `npx "unterminated`} {
		if _, err := SplitCommandLine(bad); err == nil {
			t.Errorf("SplitCommandLine(%q): expected error", bad)
		}
	}
}

func TestNewMCPManifest(t *testing.T) {
	m, values, err := NewMCPManifest(MCPServerSpec{
		Name:        "GitHub Issues",
		CommandLine: "npx -y @modelcontextprotocol/server-github",
		Env:         map[string]string{"GITHUB_PERSONAL_ACCESS_TOKEN": "ghp_x", "LOG_LEVEL": "debug"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != "github-issues" || m.Namespace != "githubIssues" || m.Command != "npx" || len(m.Args) != 2 {
		t.Fatalf("unexpected manifest: %+v", m)
	}
	if m.Env["GITHUB_PERSONAL_ACCESS_TOKEN"] != "${GITHUB_PERSONAL_ACCESS_TOKEN}" || values["GITHUB_PERSONAL_ACCESS_TOKEN"] != "ghp_x" {
		t.Fatalf("token not moved to settings: %+v %+v", m.Env, values)
	}
	types := map[string]SettingType{}
	for _, s := range m.Settings {
		types[s.Key] = s.Type
	}
	if types["GITHUB_PERSONAL_ACCESS_TOKEN"] != SettingSecret || types["LOG_LEVEL"] != SettingString {
		t.Fatalf("setting types: %v", types)
	}

	m, values, err = NewMCPManifest(MCPServerSpec{
		Name: "Linear", URL: "https://mcp.linear.app/mcp", Headers: map[string]string{"Authorization": "Bearer x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.Headers["Authorization"] != "${header_Authorization}" || values["header_Authorization"] != "Bearer x" {
		t.Fatalf("header not moved to settings: %+v %+v", m.Headers, values)
	}

	for name, spec := range map[string]MCPServerSpec{
		"no name":        {CommandLine: "npx x"},
		"nothing to run": {Name: "x"},
		"both":           {Name: "x", CommandLine: "npx x", URL: "https://x"},
		"bad url":        {Name: "x", URL: "not a url"},
		"local headers":  {Name: "x", CommandLine: "npx x", Headers: map[string]string{"A": "b"}},
		"bad env name":   {Name: "x", CommandLine: "npx x", Env: map[string]string{"BAD-NAME": "1"}},
		"bad namespace":  {Name: "x", Namespace: "not-valid", CommandLine: "npx x"},
	} {
		if _, _, err := NewMCPManifest(spec); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	for name, want := range map[string]string{"7zip": "mcp7zip", "My Server!": "myServer"} {
		if got := mcpNamespace(name); got != want {
			t.Errorf("mcpNamespace(%q) = %q, want %q", name, got, want)
		}
	}
	for name, want := range map[string]string{"7zip": "mcp-7zip", "X": "mcp-x", "Hello, World": "hello-world"} {
		if got := mcpID(name); got != want {
			t.Errorf("mcpID(%q) = %q, want %q", name, got, want)
		}
	}
}
