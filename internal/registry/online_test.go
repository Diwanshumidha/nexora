package registry

import (
	"context"
	"os"
	"testing"

	"nexora/internal/config"
)

// Downloads from the published catalog. Run with NEXORA_ONLINE_TESTS=1.
func TestPublishedCatalog(t *testing.T) {
	if os.Getenv("NEXORA_ONLINE_TESTS") == "" {
		t.Skip("set NEXORA_ONLINE_TESTS=1")
	}
	c := New(config.DefaultRegistryURL)
	cat, err := c.Fetch(context.Background())
	if err != nil || len(cat.Extensions) == 0 {
		t.Fatalf("fetch: %v", err)
	}
	smallest := cat.Extensions[0]
	for _, e := range cat.Extensions {
		if e.Size < smallest.Size {
			smallest = e
		}
	}
	_, cleanup, err := c.Download(context.Background(), smallest)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	t.Logf("%d extensions; downloaded and verified %s %s", len(cat.Extensions), smallest.ID, smallest.Version)
}
