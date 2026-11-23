package extension

import "testing"

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
