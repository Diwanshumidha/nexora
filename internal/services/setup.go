package services

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"nexora/internal/autostart"
)

// SetupService remembers whether the first-run setup was finished. The
// setup screen also comes back whenever Node.js is unusable, whatever this
// says.
type SetupService struct {
	path string // setup.json
}

func NewSetupService(dataDir string) *SetupService {
	return &SetupService{path: filepath.Join(dataDir, "setup.json")}
}

type setupState struct {
	CompletedAt time.Time `json:"completedAt"`
}

// Done reports whether the user finished (or skipped) the setup.
func (s *SetupService) Done() bool {
	_, err := os.Stat(s.path)
	return err == nil
}

// Autostart reports whether Nexora starts (in the background) at login.
func (s *SetupService) Autostart() (bool, error) { return autostart.Enabled() }

// SetAutostart turns starting at login on or off.
func (s *SetupService) SetAutostart(on bool) error { return autostart.Set(on) }

// Complete records that the setup was finished.
func (s *SetupService) Complete() error {
	raw, err := json.Marshal(setupState{CompletedAt: time.Now()})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.path, raw, 0o644)
}
