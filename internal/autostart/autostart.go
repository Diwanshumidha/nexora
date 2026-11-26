// Package autostart starts Nexora in the background when the user logs in,
// so AIs can reach it without opening the window first: an XDG autostart
// entry on Linux, a LaunchAgent on macOS and a Run key on Windows. Only
// the current user is affected.
package autostart

import (
	"os"
	"path/filepath"
)

// Arg is the flag the login entry passes: start without a window.
const Arg = "--background"

// Enabled reports whether Nexora starts at login.
func Enabled() (bool, error) { return enabled() }

// Set turns starting at login on or off, for the running executable.
func Set(on bool) error {
	if !on {
		return disable()
	}
	exe, err := executable()
	if err != nil {
		return err
	}
	return enable(exe)
}

// executable is the program to start at login. An AppImage runs from a
// temporary mount, so the AppImage file itself is used instead.
func executable() (string, error) {
	if p := os.Getenv("APPIMAGE"); p != "" {
		return p, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}
