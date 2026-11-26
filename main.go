package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"nexora/internal/cache"
	"nexora/internal/config"
	"nexora/internal/extension"
	"nexora/internal/history"
	"nexora/internal/jobs"
	"nexora/internal/mcp"
	"nexora/internal/network"
	"nexora/internal/network/cloudflare"
	"nexora/internal/network/tailscale"
	"nexora/internal/permissions"
	"nexora/internal/registry"
	"nexora/internal/runner"
	"nexora/internal/services"
)

//go:embed all:frontend/dist
var assets embed.FS

//go:embed build/tray.png
var trayIcon []byte

// Nexora runs like Discord: one process per user, a single main window,
// and closing the window only hides it. The backend (MCP endpoint,
// extensions, network provider, approvals) keeps working until "Quit
// Nexora" is chosen from the tray. Launching it again just brings the
// window back.
func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	// --background starts without a window (for launching at login).
	startHidden := slices.Contains(os.Args[1:], "--background")

	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}

	// The window is created later; second instances ask for it to be shown.
	var showMain func()
	var quitting atomic.Bool
	var app *application.App

	// Created first: a second launch is detected here, tells this instance to
	// show its window, and exits before touching ports, extensions or files.
	app = application.New(application.Options{
		Name:        "Nexora",
		Description: "Plugin-based personal AI assistant",
		Logger:      logger,
		SingleInstance: &application.SingleInstanceOptions{
			// One instance per profile (config folder).
			UniqueID: "io.codedecoders.nexora." + profileID(cfg.DataDir),
			OnSecondInstanceLaunch: func(d application.SecondInstanceData) {
				// `nexora --quit` stops the running instance (useful where
				// there is no tray); any other launch shows the window.
				if slices.Contains(d.Args, "--quit") {
					quitting.Store(true)
					app.Quit()
					return
				}
				if showMain != nil {
					showMain()
				}
			},
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		// Keep running with no visible windows: the tray quits the app.
		Linux:      application.LinuxOptions{DisableQuitOnLastWindowClosed: true},
		Windows:    application.WindowsOptions{DisableQuitOnLastWindowClosed: true},
		Mac:        application.MacOptions{ApplicationShouldTerminateAfterLastWindowClosed: false},
		OnShutdown: func() { quitting.Store(true) },
	})

	mgr := extension.NewManager(cfg, logger)
	hist, err := history.Open(cfg.HistoryFile, 500)
	if err != nil {
		log.Fatal(err)
	}
	defer hist.Close()
	// The queue reports finished jobs to history, which needs the runner's
	// permission events; the hook only fires once jobs exist, after run is set.
	var run *runner.Runner
	queue := jobs.New(jobs.Options{
		Workers:    cfg.Workers,
		MaxQueued:  cfg.MaxQueued,
		MaxRuntime: cfg.JobTimeout,
		OnFinish: func(j jobs.Job) {
			rec := history.FromJob(j)
			rec.Approvals = run.Gate().TakeEvents(j.ID)
			if err := hist.Add(rec); err != nil {
				logger.Warn("recording history", "err", err)
			}
		},
	})
	defer queue.Close()
	perms, err := permissions.Open(cfg.PermissionsFile)
	if err != nil {
		log.Fatal(err)
	}
	store, err := cache.Open(cache.Options{Path: cfg.CacheFile})
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	run = runner.New(mgr, queue, perms, store, cfg.ApprovalTimeout)

	var (
		gateway *network.Gateway
		guard   *mcp.Guard
		baseURL string
	)
	if cfg.MCPAddr != "" {
		guard, err = mcp.OpenGuard(cfg.MCPTokenFile, cfg.MCPAllowedOrigins)
		if err != nil {
			log.Fatal(err)
		}
		srv := mcp.NewServer(run, mcp.Options{InlineWait: cfg.MCPInlineWait})
		handler := mcp.AccessLog(mcp.NewHandler(srv, guard), guard, filepath.Join(cfg.DataDir, "mcp-access.log"))
		httpSrv, err := mcp.Listen(cfg.MCPAddr, handler, logger)
		if err != nil {
			// Something else holds the port; the desktop app still works.
			logger.Error("MCP endpoint disabled", "err", err)
		} else {
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = httpSrv.Shutdown(ctx)
			}()
			baseURL = "http://" + httpSrv.Addr() + "/mcp"
			logger.Info("MCP endpoint listening", "url", baseURL)

			// Built-in network providers forward public traffic to this local URL.
			providers := []network.Provider{tailscale.New(), cloudflare.New()}
			gateway, err = network.New(providers, network.Options{
				Target:    baseURL,
				StorePath: cfg.NetworkFile,
			}, logger)
			if err != nil {
				log.Fatal(err)
			}
			guard.SetPublicHost(gateway.PublicHost)
		}
	}

	// Services start in this order and stop in reverse.
	for _, s := range []application.Service{
		application.NewService(services.NewExtensionService(mgr, perms, logger)),
		application.NewService(services.NewCatalogService(mgr, registry.New(cfg.RegistryURL), filepath.Join(cfg.DataDir, "catalog.json"), logger)),
		application.NewService(services.NewPermissionsService(perms, run.Gate(), mgr)),
		application.NewService(services.NewAskWindow(run.Gate(), logger)),
		application.NewService(services.NewCodeModeService(run)),
		application.NewService(services.NewStorageService(store)),
		application.NewService(services.NewHistoryService(hist, run)),
		application.NewService(services.NewConnectService(baseURL, connectGuard(guard, baseURL))),
		application.NewService(services.NewNetworkService(gateway, tokenOf(guard))),
	} {
		app.RegisterService(s)
	}
	// NEXORA_PERF=1 runs the frontend's scroll benchmark and logs the result.
	perf := os.Getenv("NEXORA_PERF") != ""
	if perf {
		app.RegisterService(application.NewService(services.NewPerfProbe(logger)))
	}
	startURL := "/"
	if perf {
		startURL = "/#perf-" + os.Getenv("NEXORA_PERF")
	}

	mainWin := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:   "main",
		Title:  "Nexora",
		Width:  1200,
		Height: 780,
		Hidden: startHidden,
		Mac: application.MacWindow{
			InvisibleTitleBarHeight: 50,
			Backdrop:                application.MacBackdropTranslucent,
			TitleBar:                application.MacTitleBarHiddenInset,
		},
		BackgroundColour: application.NewRGB(14, 14, 18),
		URL:              startURL,
	})
	// Closing the window hides it; the process keeps serving AIs.
	mainWin.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		if quitting.Load() {
			return
		}
		e.Cancel()
		mainWin.Hide()
		logger.Info("window hidden; Nexora keeps running in the tray")
	})
	showMain = func() {
		mainWin.Show()
		mainWin.UnMinimise()
		mainWin.Focus()
	}

	tray := app.SystemTray.New()
	tray.SetIcon(trayIcon)
	tray.SetTooltip("Nexora")
	menu := app.NewMenu()
	menu.Add("Open Nexora").OnClick(func(*application.Context) { showMain() })
	menu.AddSeparator()
	menu.Add("Quit Nexora").OnClick(func(*application.Context) {
		quitting.Store(true)
		app.Quit()
	})
	tray.SetMenu(menu)
	tray.OnClick(showMain)

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}

// connectGuard returns the guard only if the endpoint is actually listening.
func connectGuard(g *mcp.Guard, baseURL string) *mcp.Guard {
	if baseURL == "" {
		return nil
	}
	return g
}

func tokenOf(g *mcp.Guard) func() string {
	return func() string {
		if g == nil {
			return ""
		}
		return g.Token()
	}
}

// profileID is a short stable id for a data folder, so separate profiles
// (e.g. XDG_CONFIG_HOME) run as separate single instances.
func profileID(dataDir string) string {
	sum := sha256.Sum256([]byte(dataDir))
	return "p" + hex.EncodeToString(sum[:6]) // D-Bus name elements must not start with a digit
}
