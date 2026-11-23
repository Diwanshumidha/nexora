// Package registry downloads extensions from a catalog: an index.json plus
// one zip per extension, as produced by the nexora-extensions repo's
// `pnpm release` (normally published as GitHub release assets).
//
// Every download is checked against the catalog's SHA-256 and size, unpacked
// with zip-slip and size limits, and its manifest must match the catalog
// entry before the app installs it.
package registry

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"nexora/internal/extension"
)

const (
	maxIndexBytes   = 4 << 20
	maxZipBytes     = 200 << 20
	maxUnpackBytes  = 500 << 20
	maxZipEntries   = 5000
	downloadTimeout = 5 * time.Minute
)

// Entry is one extension in the catalog.
type Entry struct {
	ID          string                `json:"id"`
	Name        string                `json:"name"`
	Version     string                `json:"version"`
	Protocol    int                   `json:"protocol"`
	Description string                `json:"description"`
	Author      string                `json:"author,omitempty"`
	Icon        string                `json:"icon,omitempty"`
	Permissions extension.Permissions `json:"permissions"`
	URL         string                `json:"url"`
	Size        int64                 `json:"size"`
	SHA256      string                `json:"sha256"`
}

// Catalog is index.json.
type Catalog struct {
	Schema     int     `json:"schema"`
	Extensions []Entry `json:"extensions"`
}

// Client reads one catalog.
type Client struct {
	// IndexURL is https://…/index.json, a file:// URL, or http:// on
	// loopback (for local testing).
	IndexURL string
	HTTP     *http.Client
}

func New(indexURL string) *Client {
	return &Client{IndexURL: indexURL, HTTP: &http.Client{Timeout: downloadTimeout}}
}

// ErrNotConfigured means no catalog URL is set.
var ErrNotConfigured = errors.New("no extension catalog configured (set NEXORA_REGISTRY_URL)")

// Fetch downloads and validates the catalog.
func (c *Client) Fetch(ctx context.Context) (*Catalog, error) {
	if c.IndexURL == "" {
		return nil, ErrNotConfigured
	}
	raw, err := c.get(ctx, c.IndexURL, maxIndexBytes)
	if err != nil {
		return nil, fmt.Errorf("extension catalog: %w", err)
	}
	var cat Catalog
	if err := json.Unmarshal(raw, &cat); err != nil {
		return nil, fmt.Errorf("extension catalog: %w", err)
	}
	if cat.Schema != 1 {
		return nil, fmt.Errorf("extension catalog: unsupported schema %d (update Nexora)", cat.Schema)
	}
	return &cat, nil
}

// Download fetches entry's zip, verifies it and unpacks it into a new
// temporary folder. The caller installs from dir and then calls cleanup.
func (c *Client) Download(ctx context.Context, e Entry) (dir string, cleanup func(), err error) {
	if e.Size <= 0 || e.Size > maxZipBytes {
		return "", nil, fmt.Errorf("%s: invalid size %d in catalog", e.ID, e.Size)
	}
	want, err := hex.DecodeString(e.SHA256)
	if err != nil || len(want) != sha256.Size {
		return "", nil, fmt.Errorf("%s: invalid sha256 in catalog", e.ID)
	}
	u, err := c.resolve(e.URL)
	if err != nil {
		return "", nil, err
	}
	data, err := c.get(ctx, u, e.Size)
	if err != nil {
		return "", nil, fmt.Errorf("download %s: %w", e.ID, err)
	}
	if got := sha256.Sum256(data); int64(len(data)) != e.Size || !bytes.Equal(got[:], want) {
		return "", nil, fmt.Errorf("download %s: checksum mismatch; refusing to install", e.ID)
	}

	dir, err = os.MkdirTemp("", "nexora-ext-"+e.ID+"-")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	if err := unzip(data, dir); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("unpack %s: %w", e.ID, err)
	}
	man, err := extension.LoadManifest(dir)
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("%s: %w", e.ID, err)
	}
	if man.ID != e.ID || man.Version != e.Version {
		cleanup()
		return "", nil, fmt.Errorf("%s: package contains %s %s, catalog says %s %s", e.ID, man.ID, man.Version, e.ID, e.Version)
	}
	return dir, cleanup, nil
}

// resolve makes a catalog URL absolute, relative to the index.
func (c *Client) resolve(ref string) (string, error) {
	base, err := url.Parse(c.IndexURL)
	if err != nil {
		return "", err
	}
	r, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	return base.ResolveReference(r).String(), nil
}

// get reads u (https, loopback http, or file) up to limit bytes.
func (c *Client) get(ctx context.Context, u string, limit int64) ([]byte, error) {
	parsed, err := url.Parse(u)
	if err != nil {
		return nil, err
	}
	var body io.ReadCloser
	switch parsed.Scheme {
	case "file":
		f, err := os.Open(parsed.Path)
		if err != nil {
			return nil, err
		}
		body = f
	case "http", "https":
		if parsed.Scheme == "http" && !isLoopback(parsed.Hostname()) {
			return nil, fmt.Errorf("refusing plain http for %s; use https", parsed.Host)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("%s: %s", u, resp.Status)
		}
		body = resp.Body
	default:
		return nil, fmt.Errorf("unsupported URL scheme %q", parsed.Scheme)
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than expected", u)
	}
	return data, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// unzip extracts regular files only, refusing paths that escape dst and
// archives that unpack past the limits (headers can lie, so real bytes are
// counted).
func unzip(data []byte, dst string) error {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	if len(zr.File) > maxZipEntries {
		return fmt.Errorf("%d entries (limit %d)", len(zr.File), maxZipEntries)
	}
	var total int64
	for _, f := range zr.File {
		name := strings.ReplaceAll(f.Name, "\\", "/")
		if strings.HasSuffix(name, "/") {
			continue
		}
		clean := path.Clean(name)
		if !filepath.IsLocal(clean) || strings.Contains(clean, "\x00") {
			return fmt.Errorf("unsafe entry %q", f.Name)
		}
		if !f.Mode().IsRegular() {
			continue // symlinks and devices never come from a release build
		}
		target := filepath.Join(dst, filepath.FromSlash(clean))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
		if err != nil {
			rc.Close()
			return err
		}
		n, err := io.Copy(out, io.LimitReader(rc, maxUnpackBytes-total+1))
		rc.Close()
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		if total += n; total > maxUnpackBytes {
			return fmt.Errorf("unpacks to more than %d MB", maxUnpackBytes>>20)
		}
	}
	return nil
}
