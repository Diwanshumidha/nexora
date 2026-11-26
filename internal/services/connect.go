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
	oauth   *mcp.OAuth // nil when the endpoint is disabled
}

func NewConnectService(baseURL string, guard *mcp.Guard, oauth *mcp.OAuth) *ConnectService {
	return &ConnectService{baseURL: baseURL, guard: guard, oauth: oauth}
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

// ConnectionRequests lists AI clients waiting for the user to approve their
// sign-in (OAuth), oldest first.
func (s *ConnectService) ConnectionRequests() []mcp.ConsentRequest {
	if s.oauth == nil {
		return []mcp.ConsentRequest{}
	}
	if p := s.oauth.Pending(); p != nil {
		return p
	}
	return []mcp.ConsentRequest{}
}

// AnswerConnection approves or denies a sign-in. Like tool approvals, it is
// answered only in the app, where the matching code is visible.
func (s *ConnectService) AnswerConnection(id string, allow bool) error {
	if s.oauth == nil {
		return errors.New("the MCP endpoint is disabled")
	}
	return s.oauth.Answer(id, allow)
}

// Connections lists AI clients signed in with OAuth.
func (s *ConnectService) Connections() []mcp.Connection {
	if s.oauth == nil {
		return []mcp.Connection{}
	}
	return s.oauth.Connections()
}

// RevokeConnection signs a client out; it must be approved again.
func (s *ConnectService) RevokeConnection(id string) error {
	if s.oauth == nil {
		return errors.New("the MCP endpoint is disabled")
	}
	return s.oauth.Revoke(id)
}
