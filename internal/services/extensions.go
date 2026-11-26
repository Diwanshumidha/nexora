// Package services contains the Go services bound to the Wails frontend.
// Keep them thin: they adapt internal packages to a frontend-friendly API.
package services

import (
	"context"
	"log/slog"

	"github.com/wailsapp/wails/v3/pkg/application"

	"nexora/internal/extension"
	"nexora/internal/permissions"
	"nexora/internal/protocol"
)

// ExtensionService lets the UI list, add and remove extensions.
type ExtensionService struct {
	mgr   *extension.Manager
	perms *permissions.Store
	log   *slog.Logger
}

func NewExtensionService(mgr *extension.Manager, perms *permissions.Store, log *slog.Logger) *ExtensionService {
	return &ExtensionService{mgr: mgr, perms: perms, log: log}
}

// ServiceStartup is called by Wails when the app starts.
func (s *ExtensionService) ServiceStartup(ctx context.Context, _ application.ServiceOptions) error {
	if err := s.mgr.Discover(); err != nil {
		return err
	}
	s.mgr.SetURLOpener(func(url string) error { return application.Get().Browser.OpenURL(url) })
	s.mgr.StartAll(ctx)
	return nil
}

// ServiceShutdown is called by Wails when the app quits.
func (s *ExtensionService) ServiceShutdown() error {
	s.mgr.StopAll(context.Background())
	return nil
}

// List returns every extension for the UI, without tool JSON schemas: the
// UI never shows them and they dominate the payload of a frequent poll.
// (Code mode gets them via the runner.)
func (s *ExtensionService) List() []extension.Info {
	list := s.mgr.List()
	for i := range list {
		tools := make([]protocol.ToolSpec, len(list[i].Tools))
		for j, t := range list[i].Tools {
			tools[j] = protocol.ToolSpec{Name: t.Name, Description: t.Description, ReadOnly: t.ReadOnly}
		}
		list[i].Tools = tools
	}
	return list
}

// Install adds an extension from a local folder containing nexora.json.
func (s *ExtensionService) Install(ctx context.Context, dir string) (*extension.Info, error) {
	info, err := s.mgr.Install(dir)
	if err != nil {
		return nil, err
	}
	if err := s.mgr.Start(ctx, info.Manifest.ID); err != nil {
		return info, err
	}
	info, _ = s.mgr.Get(info.Manifest.ID)
	return info, nil
}

// AddMCPServer adds an external MCP server as an extension and starts it.
// A server that fails to start is still added, so the user can fix its
// settings on its panel, where the error is shown.
func (s *ExtensionService) AddMCPServer(ctx context.Context, spec extension.MCPServerSpec) (*extension.Info, error) {
	man, values, err := extension.NewMCPManifest(spec)
	if err != nil {
		return nil, err
	}
	info, err := s.mgr.Create(man, values)
	if err != nil {
		return nil, err
	}
	s.log.Info("MCP server added", "id", man.ID, "remote", man.URL != "")
	if err := s.mgr.Start(ctx, man.ID); err != nil {
		s.log.Warn("MCP server failed to start", "id", man.ID, "err", err)
	}
	info, _ = s.mgr.Get(info.Manifest.ID)
	return info, nil
}

// SignIn connects a remote MCP server that requires OAuth, sending the user
// through its sign-in in the browser. It returns when they finish or cancel.
func (s *ExtensionService) SignIn(ctx context.Context, id string) error {
	err := s.mgr.SignIn(ctx, id)
	if err != nil {
		s.log.Warn("MCP sign-in failed", "id", id, "err", err)
	}
	return err
}

// CancelSignIn stops waiting for a sign-in started with SignIn.
func (s *ExtensionService) CancelSignIn(id string) { s.mgr.CancelSignIn(id) }

// SignOut forgets a remote MCP server's sign-in and disconnects it.
func (s *ExtensionService) SignOut(ctx context.Context, id string) error {
	return s.mgr.SignOut(ctx, id)
}

// Remove uninstalls an extension and forgets its permissions, so a later
// reinstall starts from the safe default.
func (s *ExtensionService) Remove(ctx context.Context, id string) error {
	if err := s.mgr.Remove(ctx, id); err != nil {
		return err
	}
	return s.perms.Reset(id)
}

func (s *ExtensionService) Restart(ctx context.Context, id string) error {
	if err := s.mgr.Stop(ctx, id); err != nil {
		return err
	}
	return s.mgr.Start(ctx, id)
}

// SettingsView is an extension's settings form: specs plus current values.
// Secret values are never sent to the UI; SecretsSet says which are filled.
type SettingsView struct {
	Specs      []extension.SettingSpec `json:"specs"`
	Values     map[string]any          `json:"values"`
	SecretsSet map[string]bool         `json:"secretsSet"`
}

func (s *ExtensionService) Settings(id string) (SettingsView, error) {
	specs, values, err := s.mgr.Settings(id)
	if err != nil {
		return SettingsView{}, err
	}
	view := SettingsView{Specs: specs, Values: map[string]any{}, SecretsSet: map[string]bool{}}
	if view.Specs == nil {
		view.Specs = []extension.SettingSpec{}
	}
	for _, spec := range specs {
		v := values[spec.Key]
		if spec.Type == extension.SettingSecret {
			view.SecretsSet[spec.Key] = v != ""
			v = ""
		}
		view.Values[spec.Key] = v
	}
	return view, nil
}

// SetSettings saves new values. A secret sent as null keeps its stored
// value, so the UI never needs to know it.
func (s *ExtensionService) SetSettings(ctx context.Context, id string, values map[string]any) (SettingsView, error) {
	specs, current, err := s.mgr.Settings(id)
	if err != nil {
		return SettingsView{}, err
	}
	in := extension.Values{}
	for k, v := range values {
		in[k] = v
	}
	for _, spec := range specs {
		if spec.Type == extension.SettingSecret && in[spec.Key] == nil {
			in[spec.Key] = current[spec.Key]
		}
	}
	if _, err := s.mgr.SetSettings(ctx, id, in); err != nil {
		s.log.Warn("settings not saved", "extension", id, "err", err)
		return SettingsView{}, err
	}
	s.log.Info("settings saved", "extension", id)
	return s.Settings(id)
}

// PickFolder opens the native folder chooser; "" means cancelled.
func (s *ExtensionService) PickFolder(title string) (string, error) {
	app := application.Get()
	if app == nil {
		return "", nil
	}
	s.log.Info("opening folder picker", "title", title)
	return app.Dialog.OpenFile().
		SetTitle(title).
		CanChooseDirectories(true).
		CanChooseFiles(false).
		CanCreateDirectories(true).
		PromptForSingleSelection()
}
