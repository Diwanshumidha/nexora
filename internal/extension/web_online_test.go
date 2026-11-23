package extension_test

import (
	"os"
	"strings"
	"testing"
)

// TestWebReadsAPublicPage needs internet: NEXORA_ONLINE_TESTS=1.
func TestWebReadsAPublicPage(t *testing.T) {
	if os.Getenv("NEXORA_ONLINE_TESTS") == "" {
		t.Skip("set NEXORA_ONLINE_TESTS=1 to fetch a public page")
	}
	out := string(builtin(t, "web", nil).do("readPage", obj{"url": "https://example.com/"}))
	if !strings.Contains(out, `"title":"Example Domain"`) || !strings.Contains(out, "domain") {
		t.Fatalf("readPage: %s", out)
	}
}
