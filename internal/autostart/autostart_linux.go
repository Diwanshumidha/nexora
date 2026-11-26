package autostart

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func desktopFile() (string, error) {
	dir, err := os.UserConfigDir() // honours XDG_CONFIG_HOME
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "autostart", "nexora.desktop"), nil
}

func enabled() (bool, error) {
	p, err := desktopFile()
	if err != nil {
		return false, err
	}
	raw, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !strings.Contains(string(raw), "Hidden=true"), nil
}

func enable(exe string) error {
	p, err := desktopFile()
	if err != nil {
		return err
	}
	entry := strings.Join([]string{
		"[Desktop Entry]",
		"Type=Application",
		"Name=Nexora",
		"Comment=Keeps Nexora running so your AIs can reach it",
		"Exec=" + desktopQuote(exe) + " " + Arg,
		"Icon=nexora",
		"Terminal=false",
		"X-GNOME-Autostart-enabled=true",
		"",
	}, "\n")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(entry), 0o644)
}

func disable() error {
	p, err := desktopFile()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// desktopQuote quotes an Exec argument per the Desktop Entry spec.
func desktopQuote(s string) string {
	if !strings.ContainsAny(s, " \t\"'\\$`") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\\\`, `"`, `\\"`, "`", "\\\\`", `$`, `\\$`)
	return `"` + r.Replace(s) + `"`
}
