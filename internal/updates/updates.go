// Package updates checks GitHub for a newer Nexora release, so users don't
// stay on old builds (and old security fixes). It only reports: installing
// is left to the user's download.
package updates

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultURL is the latest-release API of the app's repository (drafts and
// pre-releases are excluded by GitHub).
const DefaultURL = "https://api.github.com/repos/Diwanshumidha/nexora/releases/latest"

const (
	firstCheck = 30 * time.Second // after launch, once the network is up
	interval   = 12 * time.Hour
)

// Info is the result of the last check.
type Info struct {
	Current   string    `json:"current"`
	Latest    string    `json:"latest,omitempty"`
	Available bool      `json:"available"`
	URL       string    `json:"url,omitempty"`   // the release page
	Notes     string    `json:"notes,omitempty"` // release notes (Markdown), truncated
	CheckedAt time.Time `json:"checkedAt,omitzero"`
	Error     string    `json:"error,omitempty"`
}

type Checker struct {
	url     string
	current string
	client  *http.Client
	log     *slog.Logger

	mu   sync.Mutex
	info Info
	stop chan struct{}
}

// New checks url (DefaultURL if empty) for releases newer than current.
func New(url, current string, log *slog.Logger) *Checker {
	if url == "" {
		url = DefaultURL
	}
	return &Checker{
		url: url, current: current, client: &http.Client{Timeout: 20 * time.Second},
		log: log.With("component", "updates"), info: Info{Current: current},
	}
}

// Start checks shortly after launch and then periodically.
func (c *Checker) Start() {
	c.stop = make(chan struct{})
	go func() {
		t := time.NewTimer(firstCheck)
		defer t.Stop()
		for {
			select {
			case <-c.stop:
				return
			case <-t.C:
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			c.Check(ctx)
			cancel()
			t.Reset(interval)
		}
	}()
}

func (c *Checker) Close() {
	if c.stop != nil {
		close(c.stop)
		c.stop = nil
	}
}

// Info returns the last result.
func (c *Checker) Info() Info {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info
}

// Check asks GitHub now.
func (c *Checker) Check(ctx context.Context) Info {
	info := Info{Current: c.current, CheckedAt: time.Now()}
	latest, err := c.fetch(ctx)
	if err != nil {
		info.Error = err.Error()
		c.log.Debug("update check failed", "err", err)
	} else {
		info.Latest = strings.TrimPrefix(latest.Tag, "v")
		info.URL = latest.URL
		info.Notes = truncate(latest.Body, 2000)
		info.Available = newer(info.Latest, c.current)
		if info.Available {
			c.log.Info("update available", "current", c.current, "latest", info.Latest)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil && c.info.Latest != "" {
		// Keep the last good answer; a flaky network shouldn't hide an update.
		info.Latest, info.URL, info.Notes, info.Available = c.info.Latest, c.info.URL, c.info.Notes, c.info.Available
	}
	c.info = info
	return info
}

type release struct {
	Tag  string `json:"tag_name"`
	URL  string `json:"html_url"`
	Body string `json:"body"`
}

func (c *Checker) fetch(ctx context.Context) (release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "Nexora/"+c.current)
	resp, err := c.client.Do(req)
	if err != nil {
		return release{}, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return release{}, fmt.Errorf("no releases published yet")
	case resp.StatusCode != http.StatusOK:
		return release{}, fmt.Errorf("GitHub returned %s", resp.Status)
	}
	var r release
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&r); err != nil {
		return release{}, err
	}
	if r.Tag == "" {
		return release{}, fmt.Errorf("release without a tag")
	}
	return r, nil
}

// newer reports whether version a is newer than b (major.minor.patch;
// anything after the patch number, like "-rc1", is ignored).
func newer(a, b string) bool {
	pa, pb := parse(a), parse(b)
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func parse(v string) [3]int {
	var out [3]int
	for i, part := range strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3) {
		// Leading digits only: "3-rc1" is 3.
		n := 0
		for n < len(part) && part[n] >= '0' && part[n] <= '9' {
			n++
		}
		out[i], _ = strconv.Atoi(part[:n])
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
