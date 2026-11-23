package services

import (
	"context"
	"errors"

	"github.com/wailsapp/wails/v3/pkg/application"

	"nexora/internal/network"
)

// NetworkEndpoint is a provider plus the full MCP URL to give an AI.
type NetworkEndpoint struct {
	network.Endpoint
	// MCPURL is the public URL including the secret token (when up).
	MCPURL string `json:"mcpUrl,omitempty"`
}

// NetworkService manages the built-in network providers (Tailscale,
// Cloudflare Tunnel) that make the local MCP endpoint reachable by remote
// AI clients. At most one provider is live at a time.
type NetworkService struct {
	gw    *network.Gateway // nil when the MCP endpoint is disabled
	token func() string    // current MCP token (it can be rotated)
}

func NewNetworkService(gw *network.Gateway, token func() string) *NetworkService {
	return &NetworkService{gw: gw, token: token}
}

// ServiceStartup restores the previously active provider.
func (s *NetworkService) ServiceStartup(context.Context, application.ServiceOptions) error {
	if s.gw != nil {
		s.gw.Start()
	}
	return nil
}

// ServiceShutdown removes the live exposure. The provider stays selected
// and is restored on the next launch.
func (s *NetworkService) ServiceShutdown() error {
	if s.gw != nil {
		s.gw.Close()
	}
	return nil
}

func (s *NetworkService) List(ctx context.Context) []NetworkEndpoint {
	if s.gw == nil {
		return []NetworkEndpoint{}
	}
	eps := s.gw.List(ctx)
	out := make([]NetworkEndpoint, len(eps))
	for i, ep := range eps {
		out[i] = s.view(ep)
	}
	return out
}

// Activate makes id the live provider, switching off any other.
func (s *NetworkService) Activate(ctx context.Context, id string, options map[string]string) (NetworkEndpoint, error) {
	if s.gw == nil {
		return NetworkEndpoint{}, errMCPDisabled
	}
	ep, err := s.gw.Activate(ctx, id, options)
	return s.view(ep), err
}

// Deactivate stops exposing the MCP endpoint.
func (s *NetworkService) Deactivate() error {
	if s.gw == nil {
		return errMCPDisabled
	}
	return s.gw.Deactivate()
}

var errMCPDisabled = errors.New("the MCP endpoint is disabled, so there is nothing to expose")

func (s *NetworkService) view(ep network.Endpoint) NetworkEndpoint {
	v := NetworkEndpoint{Endpoint: ep}
	if ep.State == network.StateUp && ep.Exposure != nil {
		v.MCPURL = ep.Exposure.URL + "/" + s.token()
	}
	return v
}
