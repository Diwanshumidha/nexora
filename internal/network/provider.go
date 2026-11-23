// Package network is the layer between remote AI clients and the local MCP
// endpoint. The endpoint only listens on localhost; a built-in provider
// (Tailscale, Cloudflare Tunnel, ...) makes it reachable and reports the
// public URL.
//
//	ChatGPT ──https──▶ provider (tailscaled / cloudflared) ──▶ 127.0.0.1:7377/mcp ──▶ MCP server
//
// Providers are compiled in and run in-process. At most one provider is
// live at a time. The Gateway owns that choice (persisted in network.json),
// keeps the exposure healthy, and closes it on shutdown.
package network

import (
	"context"
	"net/url"
)

// Provider makes a local URL reachable from outside the machine.
//
// Contract: after Expose returns url, a request to url + "/x" must reach
// target + "/x". The MCP access token is appended by the caller, so
// providers never handle it. The gateway serialises calls to a provider.
type Provider interface {
	Info() ProviderInfo
	// Available reports whether the provider can be used right now (e.g.
	// the daemon is running), with an actionable error if not.
	Available(ctx context.Context) error
	Expose(ctx context.Context, target *url.URL, options map[string]string) (Exposure, error)
	// Status returns an error if a previous exposure stopped working.
	Status(ctx context.Context) error
	// Close removes the exposure. It must be safe to call when nothing is
	// exposed.
	Close(ctx context.Context) error
}

type ProviderInfo struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Settings    []Setting `json:"settings,omitempty"`
}

// Setting describes one user option. The UI renders a form from these.
type Setting struct {
	Key     string   `json:"key"`
	Label   string   `json:"label"`
	Help    string   `json:"help,omitempty"`
	Choices []string `json:"choices,omitempty"` // a select when set, otherwise free text
	Default string   `json:"default"`
}

type Exposure struct {
	// URL is the public base URL, without a trailing slash.
	URL string `json:"url"`
	// Public is true when anyone on the internet can reach URL (needed for
	// hosted AI services such as ChatGPT); false for private networks.
	Public  bool   `json:"public"`
	Message string `json:"message,omitempty"`
}

// withDefaults fills unset options from the provider's settings and drops
// unknown keys.
func withDefaults(info ProviderInfo, options map[string]string) map[string]string {
	out := make(map[string]string, len(info.Settings))
	for _, s := range info.Settings {
		v, ok := options[s.Key]
		if !ok || v == "" {
			v = s.Default
		}
		out[s.Key] = v
	}
	return out
}
