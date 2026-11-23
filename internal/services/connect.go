package services

import (
	"errors"

	"nexora/internal/mcp"
)

// ConnectInfo tells the user how to connect an external AI client.
type ConnectInfo struct {
	Enabled bool   `json:"enabled"`
	URL     string `json:"url"`     // local URL including the secret token
	Token   string `json:"token"`   // for clients that send a bearer header
	BaseURL string `json:"baseUrl"` // local URL without the token
}

// ConnectService exposes MCP connection details and endpoint security to
// the UI.
type ConnectService struct {
	baseURL string     // "" when the endpoint is disabled
	guard   *mcp.Guard // nil when the endpoint is disabled
}

func NewConnectService(baseURL string, guard *mcp.Guard) *ConnectService {
	return &ConnectService{baseURL: baseURL, guard: guard}
}

func (s *ConnectService) Info() ConnectInfo {
	if s.guard == nil {
		return ConnectInfo{}
	}
	tok := s.guard.Token()
	return ConnectInfo{Enabled: true, BaseURL: s.baseURL, URL: s.baseURL + "/" + tok, Token: tok}
}

// RotateToken replaces the secret token. Every existing URL, including
// public ones, stops working; connected AIs need the new URL.
func (s *ConnectService) RotateToken() (ConnectInfo, error) {
	if s.guard == nil {
		return ConnectInfo{}, errors.New("the MCP endpoint is disabled")
	}
	if _, err := s.guard.Rotate(); err != nil {
		return ConnectInfo{}, err
	}
	return s.Info(), nil
}

// Security reports refused requests since start.
func (s *ConnectService) Security() mcp.GuardStats {
	if s.guard == nil {
		return mcp.GuardStats{}
	}
	return s.guard.Stats()
}
