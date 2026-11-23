package services

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"nexora/internal/extension"
	"nexora/internal/registry"
)

// CatalogService lists the extension catalog and installs or updates
// extensions from it.
type CatalogService struct {
	mgr *extension.Manager
	reg *registry.Client
	log *slog.Logger

	// busy serialises installs so two clicks can't race on one folder.
	busy sync.Mutex
}

func NewCatalogService(mgr *extension.Manager, reg *registry.Client, log *slog.Logger) *CatalogService {
	return &CatalogService{mgr: mgr, reg: reg, log: log}
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
}

// List fetches the catalog. When none is configured it returns
// Configured=false rather than an error, so the UI can explain.
func (s *CatalogService) List(ctx context.Context) (CatalogView, error) {
	view := CatalogView{Configured: s.reg.IndexURL != "", Source: s.reg.IndexURL, Items: []CatalogItem{}}
	if !view.Configured {
		return view, nil
	}
	cat, err := s.reg.Fetch(ctx)
	if err != nil {
		return view, err
	}
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
		view.Items = append(view.Items, item)
	}
	return view, nil
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

func (s *CatalogService) apply(ctx context.Context, id string, update bool) (*extension.Info, error) {
	s.busy.Lock()
	defer s.busy.Unlock()

	cat, err := s.reg.Fetch(ctx)
	if err != nil {
		return nil, err
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

	if update {
		info, err := s.mgr.Update(ctx, dir)
		if err == nil {
			s.log.Info("extension updated", "id", id, "version", entry.Version)
		}
		return info, err
	}
	info, err := s.mgr.Install(dir)
	if err != nil {
		return nil, err
	}
	s.log.Info("extension installed", "id", id, "version", entry.Version)
	if err := s.mgr.Start(ctx, id); err != nil {
		return info, err
	}
	info, _ = s.mgr.Get(id)
	return info, nil
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
