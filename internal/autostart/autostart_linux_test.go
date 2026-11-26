package autostart

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxEntry(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("APPIMAGE", "/home/me/Apps/Nexora App.AppImage")

	if on, err := Enabled(); err != nil || on {
		t.Fatalf("initially: %v %v", on, err)
	}
	if err := Set(true); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(cfg, "autostart", "nexora.desktop"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `Exec="/home/me/Apps/Nexora App.AppImage" --background`) {
		t.Fatalf("entry:\n%s", raw)
	}
	if on, _ := Enabled(); !on {
		t.Fatal("not enabled")
	}
	if err := Set(false); err != nil {
		t.Fatal(err)
	}
	if on, _ := Enabled(); on {
		t.Fatal("still enabled")
	}
	if err := Set(false); err != nil {
		t.Fatal("disabling twice must be fine")
	}
}
