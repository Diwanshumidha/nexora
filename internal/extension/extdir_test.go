package extension_test

import (
	"os"
	"path/filepath"
)

// extensionsDir is the extensions folder of a nexora-extensions checkout:
// $NEXORA_EXTENSIONS_REPO, or the sibling checkout ../nexora-extensions.
// Tests that need real extensions skip when it isn't built.
func extensionsDir() string {
	repo := os.Getenv("NEXORA_EXTENSIONS_REPO")
	if repo == "" {
		repo, _ = filepath.Abs("../../../nexora-extensions")
	}
	return filepath.Join(repo, "extensions")
}
