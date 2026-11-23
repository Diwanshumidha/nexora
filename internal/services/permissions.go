package services

import (
	"fmt"

	"nexora/internal/extension"
	"nexora/internal/permissions"
)

// ToolPermission is one tool's permission as shown in the UI.
type ToolPermission struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	ReadOnly    bool   `json:"readOnly"`
	// Effective is what happens when a script calls the tool.
	Effective permissions.Decision `json:"effective"`
	// Override is the per-tool setting; empty means "follow the mode".
	Override permissions.Decision `json:"override,omitempty"`
}

type ExtensionPermissions struct {
	ExtensionID string           `json:"extensionId"`
	Mode        permissions.Mode `json:"mode"`
	Tools       []ToolPermission `json:"tools"`
}

// PermissionsService manages per-extension modes, per-tool overrides,
// strict mode and pending approvals. Approvals are answered only in the app
// (the ask window or the main window), where the arguments are visible:
// never from a notification.
type PermissionsService struct {
	store *permissions.Store
	gate  *permissions.Gate
	mgr   *extension.Manager
}

func NewPermissionsService(store *permissions.Store, gate *permissions.Gate, mgr *extension.Manager) *PermissionsService {
	return &PermissionsService{store: store, gate: gate, mgr: mgr}
}

// Strict reports whether every tool call must be approved.
func (s *PermissionsService) Strict() bool { return s.store.Strict() }

// SetStrict turns "ask before every tool call" on or off.
func (s *PermissionsService) SetStrict(on bool) error { return s.store.SetStrict(on) }

// Get returns an extension's mode and every tool's effective permission.
func (s *PermissionsService) Get(extID string) (ExtensionPermissions, error) {
	info, ok := s.mgr.Get(extID)
	if !ok {
		return ExtensionPermissions{}, fmt.Errorf("unknown extension %q", extID)
	}
	p := s.store.Get(extID)
	out := ExtensionPermissions{ExtensionID: extID, Mode: p.Mode, Tools: []ToolPermission{}}
	for _, t := range info.Tools {
		out.Tools = append(out.Tools, ToolPermission{
			Name: t.Name, Description: t.Description, ReadOnly: t.ReadOnly,
			Effective: p.Decide(t.Name, t.ReadOnly),
			Override:  p.Tools[t.Name],
		})
	}
	return out, nil
}

// Modes returns every extension's mode, for colouring the orbit.
func (s *PermissionsService) Modes() map[string]permissions.Mode {
	out := map[string]permissions.Mode{}
	for _, info := range s.mgr.List() {
		out[info.Manifest.ID] = s.store.Get(info.Manifest.ID).Mode
	}
	return out
}

func (s *PermissionsService) SetMode(extID string, mode permissions.Mode) (ExtensionPermissions, error) {
	if err := s.store.SetMode(extID, mode); err != nil {
		return ExtensionPermissions{}, err
	}
	return s.Get(extID)
}

// SetTool sets a tool's override; "" makes it follow the mode again.
func (s *PermissionsService) SetTool(extID, tool string, decision permissions.Decision) (ExtensionPermissions, error) {
	if err := s.store.SetTool(extID, tool, decision); err != nil {
		return ExtensionPermissions{}, err
	}
	return s.Get(extID)
}

// Approvals lists tool calls waiting for the user, oldest first.
func (s *PermissionsService) Approvals() []permissions.Approval {
	return s.gate.Pending()
}

func (s *PermissionsService) Resolve(id string, choice permissions.Choice) error {
	return s.gate.Resolve(id, choice)
}
