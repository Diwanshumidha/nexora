package services

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"nexora/internal/extension"
	"nexora/internal/registry"
)

// CatalogService lists the extension catalog and installs or updates
// extensions from it, or from a package file.
//
// The last catalog fetched is kept in memory and on disk, so the "+" panel
// opens instantly, works offline (showing the saved copy) and can flag
// updates without a network round trip. It is refreshed in the background
// every few hours.
type CatalogService struct {
	mgr       *extension.Manager
	reg       *registry.Client
	cachePath string
	log       *slog.Logger

	// busy serialises installs so two clicks can't race on one folder.
	busy sync.Mutex

	mu        sync.Mutex
	cat       *registry.Catalog
	fetchedAt time.Time
	fetchErr  string // last refresh failure, cleared by a success
	stop      context.CancelFunc
}

const (
	// catalogMaxAge is how long a fetched catalog counts as current: older
	// copies are shown but flagged Stale, and installs fetch afresh.
	catalogMaxAge = 15 * time.Minute
	// updateCheckEvery is how often the catalog is refreshed in the
	// background to notice updates.
	updateCheckEvery = 6 * time.Hour
)

func NewCatalogService(mgr *extension.Manager, reg *registry.Client, cachePath string, log *slog.Logger) *CatalogService {
	return &CatalogService{mgr: mgr, reg: reg, cachePath: cachePath, log: log}
}

func (s *CatalogService) ServiceStartup(context.Context, application.ServiceOptions) error {
	s.loadSaved()
	if s.reg.IndexURL == "" {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.stop = cancel
	go func() {
		// Let startup settle before touching the network.
		wait := 10 * time.Second
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			wait = updateCheckEvery
			if _, err := s.refresh(ctx); err != nil && ctx.Err() == nil {
				s.log.Info("catalog update check failed", "err", err)
			}
		}
	}()
	return nil
}

func (s *CatalogService) ServiceShutdown() error {
	if s.stop != nil {
		s.stop()
	}
	return nil
}

// CatalogItem is one catalog entry and how it relates to what's installed.
type CatalogItem struct {
	registry.Entry
	// State: "available", "installed", "update", "dev" (loaded from a
	// development folder, managed there) or "incompatible".
	State            string `json:"state"`
	InstalledVersion string `json:"installedVersion,omitempty"`
}

type CatalogView struct {
	Configured bool          `json:"configured"`
	Source     string        `json:"source"`
	Items      []CatalogItem `json:"items"`
	// FetchedAt is when the catalog shown was downloaded (zero if never).
	FetchedAt time.Time `json:"fetchedAt"`
	// Offline means the latest fetch failed and Items come from the saved
	// copy; Error says why.
	Offline bool   `json:"offline"`
	Error   string `json:"error,omitempty"`
	// Stale means the saved copy is older than catalogMaxAge; the UI shows
	// it and refreshes in the background.
	Stale bool `json:"stale"`
	// Updates counts installed extensions with a newer catalog version.
	Updates int `json:"updates"`
}

// List returns the catalog. Without refresh it answers from the saved copy
// when there is one, however old, so the panel opens instantly; Stale then
// tells the UI to ask again with refresh. With refresh (or no saved copy)
// it fetches; if that fails but a saved copy exists, the copy is returned
// with Offline set. When no catalog is configured it returns
// Configured=false rather than an error, so the UI can explain.
func (s *CatalogService) List(ctx context.Context, refresh bool) (CatalogView, error) {
	view := CatalogView{Configured: s.reg.IndexURL != "", Source: s.reg.IndexURL, Items: []CatalogItem{}}
	if !view.Configured {
		return view, nil
	}
	cat, fetchedAt := s.saved()
	if refresh || cat == nil {
		fresh, err := s.refresh(ctx)
		switch {
		case err == nil:
			cat, fetchedAt = fresh, time.Now()
		case cat == nil:
			return view, err
		default:
			view.Offline, view.Error = true, err.Error()
		}
	}
	view.FetchedAt = fetchedAt
	view.Stale = !view.Offline && time.Since(fetchedAt) > catalogMaxAge
	view.Items = s.items(cat)
	for _, it := range view.Items {
		if it.State == "update" {
			view.Updates++
		}
	}
	return view, nil
}

// Updates lists installed extensions with a newer version in the saved
// catalog. It never touches the network, so the UI can poll it.
func (s *CatalogService) Updates() []CatalogItem {
	out := []CatalogItem{}
	cat, _ := s.saved()
	if cat == nil {
		return out
	}
	for _, it := range s.items(cat) {
		if it.State == "update" {
			out = append(out, it)
		}
	}
	return out
}

func (s *CatalogService) items(cat *registry.Catalog) []CatalogItem {
	items := make([]CatalogItem, 0, len(cat.Extensions))
	for _, e := range cat.Extensions {
		item := CatalogItem{Entry: e, State: "available"}
		if info, ok := s.mgr.Get(e.ID); ok {
			item.InstalledVersion = info.Manifest.Version
			switch {
			case !s.mgr.Installed(e.ID):
				item.State = "dev"
			case compareVersions(e.Version, info.Manifest.Version) > 0:
				item.State = "update"
			default:
				item.State = "installed"
			}
		}
		if e.Protocol > extension.HostProtocol && item.State != "installed" && item.State != "dev" {
			item.State = "incompatible"
		}
		items = append(items, item)
	}
	return items
}

func (s *CatalogService) saved() (*registry.Catalog, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cat, s.fetchedAt
}

// refresh fetches the catalog and saves it.
func (s *CatalogService) refresh(ctx context.Context) (*registry.Catalog, error) {
	cat, err := s.reg.Fetch(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.fetchErr = err.Error()
		return nil, err
	}
	s.cat, s.fetchedAt, s.fetchErr = cat, time.Now(), ""
	s.saveLocked()
	return cat, nil
}

// savedCatalog is the on-disk copy of the last catalog fetched.
type savedCatalog struct {
	Source    string            `json:"source"`
	FetchedAt time.Time         `json:"fetchedAt"`
	Catalog   *registry.Catalog `json:"catalog"`
}

func (s *CatalogService) loadSaved() {
	if s.cachePath == "" {
		return
	}
	raw, err := os.ReadFile(s.cachePath)
	if err != nil {
		return
	}
	var v savedCatalog
	// A copy of a different catalog (the URL changed) is not this one.
	if json.Unmarshal(raw, &v) != nil || v.Catalog == nil || v.Source != s.reg.IndexURL {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cat == nil {
		s.cat, s.fetchedAt = v.Catalog, v.FetchedAt
	}
}

func (s *CatalogService) saveLocked() {
	if s.cachePath == "" {
		return
	}
	raw, err := json.Marshal(savedCatalog{Source: s.reg.IndexURL, FetchedAt: s.fetchedAt, Catalog: s.cat})
	if err == nil {
		tmp := s.cachePath + ".tmp"
		if err = os.WriteFile(tmp, raw, 0o644); err == nil {
			err = os.Rename(tmp, s.cachePath)
		}
	}
	if err != nil {
		s.log.Warn("saving catalog", "err", err)
	}
}

// Install downloads, verifies and installs id, then starts it.
func (s *CatalogService) Install(ctx context.Context, id string) (*extension.Info, error) {
	return s.apply(ctx, id, false)
}

// Update replaces an installed extension with the catalog's version,
// keeping its settings, data and permissions.
func (s *CatalogService) Update(ctx context.Context, id string) (*extension.Info, error) {
	return s.apply(ctx, id, true)
}

// UpdateResult is the outcome of one update in UpdateAll.
type UpdateResult struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Error   string `json:"error,omitempty"`
}

// UpdateAll updates every installed extension that has a newer catalog
// version. One failure doesn't stop the others.
func (s *CatalogService) UpdateAll(ctx context.Context) ([]UpdateResult, error) {
	view, err := s.List(ctx, true)
	if err != nil {
		return nil, err
	}
	if view.Offline {
		return nil, fmt.Errorf("can't reach the catalog: %s", view.Error)
	}
	out := []UpdateResult{}
	for _, it := range view.Items {
		if it.State != "update" {
			continue
		}
		r := UpdateResult{ID: it.ID, Name: it.Name, Version: it.Version}
		if _, err := s.apply(ctx, it.ID, true); err != nil {
			r.Error = err.Error()
		}
		out = append(out, r)
	}
	return out, nil
}

func (s *CatalogService) apply(ctx context.Context, id string, update bool) (*extension.Info, error) {
	s.busy.Lock()
	defer s.busy.Unlock()

	// Use the saved catalog if it is recent: it is what the user just saw.
	cat, fetchedAt := s.saved()
	if cat == nil || time.Since(fetchedAt) > catalogMaxAge {
		var err error
		if cat, err = s.refresh(ctx); err != nil {
			return nil, err
		}
	}
	var entry *registry.Entry
	for i := range cat.Extensions {
		if cat.Extensions[i].ID == id {
			entry = &cat.Extensions[i]
		}
	}
	if entry == nil {
		return nil, fmt.Errorf("%q is not in the catalog", id)
	}
	dir, cleanup, err := s.reg.Download(ctx, *entry)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return s.installDir(ctx, dir, entry.ID, entry.Version, update)
}

// installDir installs (and starts) or updates the unpacked package in dir.
func (s *CatalogService) installDir(ctx context.Context, dir, id, version string, update bool) (*extension.Info, error) {
	if update {
		info, err := s.mgr.Update(ctx, dir)
		if err == nil {
			s.log.Info("extension updated", "id", id, "version", version)
		}
		return info, err
	}
	info, err := s.mgr.Install(dir)
	if err != nil {
		return nil, err
	}
	s.log.Info("extension installed", "id", id, "version", version)
	if err := s.mgr.Start(ctx, id); err != nil {
		return info, err
	}
	info, _ = s.mgr.Get(id)
	return info, nil
}

// InstallFile installs an extension package (.nxext or .zip) from disk. If
// that extension is already installed it is updated in place, unless the
// installed version is newer.
func (s *CatalogService) InstallFile(ctx context.Context, path string) (*extension.Info, error) {
	s.busy.Lock()
	defer s.busy.Unlock()

	dir, man, cleanup, err := registry.OpenArchive(path)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	info, exists := s.mgr.Get(man.ID)
	if exists {
		if !s.mgr.Installed(man.ID) {
			return nil, fmt.Errorf("%s is loaded from a development folder (%s); rebuild it there instead", man.Name, info.Dir)
		}
		if compareVersions(man.Version, info.Manifest.Version) < 0 {
			return nil, fmt.Errorf("%s %s is already installed, which is newer than this package (%s)", man.Name, info.Manifest.Version, man.Version)
		}
	}
	return s.installDir(ctx, dir, man.ID, man.Version, exists)
}

// PickPackage opens the native file chooser for an extension package; ""
// means cancelled.
func (s *CatalogService) PickPackage() (string, error) {
	app := application.Get()
	if app == nil {
		return "", nil
	}
	return app.Dialog.OpenFile().
		SetTitle("Install an extension package").
		CanChooseFiles(true).
		CanChooseDirectories(false).
		AddFilter("Nexora extension", "*.nxext;*.zip").
		PromptForSingleSelection()
}

// compareVersions compares dotted numeric versions ("1.10.0" > "1.9.2");
// a pre-release suffix sorts before the release.
func compareVersions(a, b string) int {
	split := func(v string) ([]string, string) {
		v = strings.TrimPrefix(v, "v")
		core, pre, _ := strings.Cut(v, "-")
		return strings.Split(core, "."), pre
	}
	pa, preA := split(a)
	pb, preB := split(b)
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			if x > y {
				return 1
			}
			return -1
		}
	}
	switch {
	case preA == preB:
		return 0
	case preA == "":
		return 1
	case preB == "":
		return -1
	}
	return strings.Compare(preA, preB)
}
