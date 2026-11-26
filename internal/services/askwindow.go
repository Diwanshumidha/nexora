package services

import (
	"context"
	"log/slog"
	"sync"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"nexora/internal/mcp"
	"nexora/internal/permissions"
)

// AskTitle is the ask window's title. On tiling window managers it can be
// matched by a rule, e.g. Hyprland: windowrule = float, title:^(Nexora · Approval needed)$
const AskTitle = "Nexora · Approval needed"

// AskWindow shows a small, separate approval window whenever a tool call or
// an AI client's sign-in is waiting for the user, and closes it when nothing
// is pending.
//
// The window is created fresh for each burst of requests rather than
// hidden and re-shown: a newly mapped window opens on the workspace the
// user is currently on (Linux), and is created always-on-top and focused
// (Windows, macOS).
type AskWindow struct {
	gate  *permissions.Gate
	oauth *mcp.OAuth // nil when the MCP endpoint is disabled
	log   *slog.Logger

	mu       sync.Mutex
	win      *application.WebviewWindow
	lastSeen int // pending count at the last sync, to raise only on new requests
}

func NewAskWindow(gate *permissions.Gate, oauth *mcp.OAuth, log *slog.Logger) *AskWindow {
	return &AskWindow{gate: gate, oauth: oauth, log: log}
}

// ServiceStartup subscribes to approval changes.
func (a *AskWindow) ServiceStartup(context.Context, application.ServiceOptions) error {
	a.gate.OnChange = func([]permissions.Approval) { a.sync() }
	if a.oauth != nil {
		a.oauth.OnChange = a.sync
	}
	return nil
}

// sync opens, raises or closes the window to match the pending approvals.
// It re-reads the gate instead of trusting the callback's snapshot, because
// change notifications can arrive out of order.
func (a *AskWindow) sync() {
	app := application.Get()
	if app == nil {
		return
	}
	n := len(a.gate.Pending())
	if a.oauth != nil {
		n += len(a.oauth.Pending())
	}

	a.mu.Lock()
	grew := n > a.lastSeen
	a.lastSeen = n
	cur := a.win
	if n == 0 {
		a.win = nil
	}
	a.mu.Unlock()

	// Window calls happen outside the lock: closing fires WindowClosing,
	// whose handler takes it.
	switch {
	case n == 0 && cur != nil:
		cur.Close()
	case n > 0 && cur == nil && grew:
		w := a.open(app)
		a.mu.Lock()
		if a.win == nil {
			a.win = w
			w = nil
		}
		a.mu.Unlock()
		if w != nil { // lost a race with another sync: keep just one window
			w.Close()
		}
	case n > 0 && cur != nil && grew:
		// Another request while the window is open: bring it forward.
		cur.Show()
		cur.Focus()
	}
}

func (a *AskWindow) open(app *application.App) *application.WebviewWindow {
	w := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:                "nexora-ask",
		Title:               AskTitle,
		Width:               460,
		Height:              640,
		URL:                 "/#/ask",
		AlwaysOnTop:         true,
		DisableResize:       true, // fixed-size windows float on most tiling WMs
		InitialPosition:     application.WindowCentered,
		MinimiseButtonState: application.ButtonHidden,
		MaximiseButtonState: application.ButtonHidden,
		BackgroundColour:    application.NewRGB(17, 19, 24),
		Mac: application.MacWindow{
			Backdrop: application.MacBackdropTranslucent,
			TitleBar: application.MacTitleBarHiddenInset,
		},
	})
	w.OnWindowEvent(events.Common.WindowClosing, func(*application.WindowEvent) {
		// Closed by the user without answering: the requests stay pending
		// (the main window still shows them) and the next one reopens it.
		a.mu.Lock()
		if a.win == w {
			a.win = nil
		}
		a.mu.Unlock()
	})
	w.Focus()
	a.log.Info("approval needed: opened the ask window")
	return w
}
