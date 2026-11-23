package extension

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSettingsValidate(t *testing.T) {
	home := t.TempDir()
	nexoraData := filepath.Join(home, ".config", "nexora")
	docs := filepath.Join(home, "Documents")
	for _, d := range []string{nexoraData, docs} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	specs := []SettingSpec{
		{Key: "folders", Type: SettingFolders, Label: "Trusted folders", Grant: GrantReadWrite},
		{Key: "sheet", Type: SettingString, Label: "Default sheet", Default: "Sheet1"},
		{Key: "rows", Type: SettingNumber, Label: "Max rows", Default: 1000.0},
		{Key: "mode", Type: SettingSelect, Label: "Mode", Choices: []string{"fast", "safe"}, Default: "safe"},
		{Key: "out", Type: SettingFolder, Label: "Output", Required: true},
	}
	p := SettingsPolicy{Protected: []string{nexoraData}}

	v, err := p.Validate(specs, Values{"folders": []any{docs, docs + "/"}, "out": docs})
	if err != nil {
		t.Fatal(err)
	}
	if got := v["folders"].([]string); len(got) != 1 || got[0] != docs {
		t.Fatalf("folders should be cleaned and de-duplicated: %v", got)
	}
	if v["sheet"] != "Sheet1" || v["rows"] != 1000.0 || v["mode"] != "safe" {
		t.Fatalf("defaults not applied: %v", v)
	}

	for name, tc := range map[string]struct {
		in   Values
		want string
	}{
		"home contains nexora data": {Values{"folders": []any{home}, "out": docs}, "Nexora's private data"},
		"inside nexora data":        {Values{"folders": []any{nexoraData}, "out": docs}, "Nexora's private data"},
		"root":                      {Values{"folders": []any{"/"}, "out": docs}, "whole disk"},
		"relative":                  {Values{"folders": []any{"Documents"}, "out": docs}, "absolute"},
		"missing":                   {Values{"folders": []any{filepath.Join(home, "nope")}, "out": docs}, "not a folder"},
		"required":                  {Values{}, "required"},
		"bad choice":                {Values{"mode": "yolo", "out": docs}, "one of"},
		"bad number":                {Values{"rows": "many", "out": docs}, "number"},
	} {
		if _, err := p.Validate(specs, tc.in); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: expected %q, got %v", name, tc.want, err)
		}
	}

	// Ungranted folder settings may point anywhere that exists.
	open := []SettingSpec{{Key: "dir", Type: SettingFolder, Label: "Dir"}}
	if _, err := p.Validate(open, Values{"dir": home}); err != nil {
		t.Fatalf("ungranted folders aren't sandbox grants: %v", err)
	}

	r, w := grantedFolders(specs, v)
	if len(r) != 1 || len(w) != 1 || r[0] != docs {
		t.Fatalf("grants: read=%v write=%v", r, w)
	}
}

func TestManifestSettingsValidation(t *testing.T) {
	base := Manifest{ID: "ok-ext", Runtime: RuntimeNode, Main: "dist/index.js"}
	for name, s := range map[string][]SettingSpec{
		"bad key":          {{Key: "my-key", Type: SettingString}},
		"unknown type":     {{Key: "x", Type: "color"}},
		"select no choice": {{Key: "x", Type: SettingSelect}},
		"grant on string":  {{Key: "x", Type: SettingString, Grant: GrantRead}},
		"duplicate":        {{Key: "x", Type: SettingString}, {Key: "x", Type: SettingNumber}},
	} {
		m := base
		m.Settings = s
		if m.Validate() == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
