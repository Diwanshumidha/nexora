package tailscale

import (
	"context"
	"os"
	"testing"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
)

// TestLiveReadOnly checks the real tailscaled without changing anything:
// NEXORA_LIVE_TAILSCALE=1 go test -run Live -v ./internal/network/tailscale
func TestLiveReadOnly(t *testing.T) {
	if os.Getenv("NEXORA_LIVE_TAILSCALE") == "" {
		t.Skip("set NEXORA_LIVE_TAILSCALE=1 to check the local tailscaled")
	}
	ctx := context.Background()
	lc := &local.Client{}
	p := NewWithClient(lc)
	if err := p.Available(ctx); err != nil {
		t.Fatalf("available: %v", err)
	}
	st, _ := lc.StatusWithoutPeers(ctx)
	t.Logf("node: %s  https certs: %v", st.Self.DNSName, st.CertDomains)
	for _, port := range []uint16{443, 8443, 10000} {
		t.Logf("funnel on %d: %v", port, errOrOK(ipn.CheckFunnelAccess(port, st.Self)))
	}
	sc, err := lc.GetServeConfig(ctx)
	if err != nil {
		t.Fatalf("read serve config: %v", explain(err))
	}
	t.Logf("serve config readable; %d web hosts configured", len(sc.Web))
}

func errOrOK(err error) string {
	if err != nil {
		return err.Error()
	}
	return "allowed"
}
