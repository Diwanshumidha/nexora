package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// State of the active provider's exposure.
type State string

const (
	StateOff      State = "off"
	StateStarting State = "starting"
	StateUp       State = "up"
	StateDown     State = "down" // active but not reachable; retried automatically
)

const (
	exposeTimeout = 45 * time.Second // HTTPS certs may be provisioned on first use
	statusTimeout = 10 * time.Second
	closeTimeout  = 15 * time.Second
)

// Endpoint is the public view of one provider.
type Endpoint struct {
	ProviderInfo
	Active    bool  `json:"active"`
	State     State `json:"state"`
	Available bool  `json:"available"`
	// Unavailable explains why the provider cannot be used right now.
	Unavailable string            `json:"unavailable,omitempty"`
	Options     map[string]string `json:"options"`
	Exposure    *Exposure         `json:"exposure,omitempty"`
	Error       string            `json:"error,omitempty"`
}

type Options struct {
	// Target is the local MCP URL, e.g. http://127.0.0.1:7377/mcp.
	Target string
	// StorePath persists the active provider and per-provider options.
	StorePath string
	// CheckInterval is how often the exposure is health-checked.
	CheckInterval time.Duration
}

// Gateway runs at most one provider at a time.
type Gateway struct {
	providers []Provider
	target    *url.URL
	storePath string
	interval  time.Duration
	log       *slog.Logger

	// op serialises Expose/Close/Status calls, so providers never see
	// concurrent calls and switching providers is atomic.
	op sync.Mutex

	mu       sync.Mutex
	active   string                       // provider id, "" when off
	options  map[string]map[string]string // per provider, persisted
	state    State
	exposure *Exposure
	lastErr  string
	failures int
	retryAt  time.Time

	stop chan struct{}
	done chan struct{}
}

func New(providers []Provider, opts Options, log *slog.Logger) (*Gateway, error) {
	target, err := url.Parse(opts.Target)
	if err != nil {
		return nil, fmt.Errorf("network target: %w", err)
	}
	if opts.CheckInterval <= 0 {
		opts.CheckInterval = 20 * time.Second
	}
	g := &Gateway{
		providers: providers,
		target:    target,
		storePath: opts.StorePath,
		interval:  opts.CheckInterval,
		log:       log.With("component", "network"),
		options:   map[string]map[string]string{},
		state:     StateOff,
	}
	if err := g.load(); err != nil {
		return nil, err
	}
	if g.active != "" {
		g.state = StateStarting
	}
	return g, nil
}

// Start restores the persisted provider and begins health checks.
func (g *Gateway) Start() {
	g.stop = make(chan struct{})
	g.done = make(chan struct{})
	go func() {
		defer close(g.done)
		t := time.NewTicker(g.interval)
		defer t.Stop()
		for {
			g.reconcile()
			select {
			case <-g.stop:
				return
			case <-t.C:
			}
		}
	}()
}

// Close stops health checks and removes the live exposure. The active
// provider stays selected and is restored on the next start.
func (g *Gateway) Close() {
	if g.stop != nil {
		close(g.stop)
		<-g.done
		g.stop = nil
	}
	g.op.Lock()
	defer g.op.Unlock()
	if p := g.exposed(); p != nil {
		g.closeLocked(p)
	}
}

// PublicHost returns the host[:port] of the live exposure, or "" when
// nothing is exposed. The MCP guard accepts it as a Host header.
func (g *Gateway) PublicHost() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state != StateUp || g.exposure == nil {
		return ""
	}
	u, err := url.Parse(g.exposure.URL)
	if err != nil {
		return ""
	}
	return u.Host
}

// List returns all providers, the active one marked.
func (g *Gateway) List(ctx context.Context) []Endpoint {
	// Probe availability concurrently, outside the state lock.
	avail := make([]error, len(g.providers))
	var wg sync.WaitGroup
	for i, p := range g.providers {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			avail[i] = p.Available(ctx)
		})
	}
	wg.Wait()

	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Endpoint, len(g.providers))
	for i, p := range g.providers {
		info := p.Info()
		ep := Endpoint{
			ProviderInfo: info,
			State:        StateOff,
			Available:    avail[i] == nil,
			Options:      withDefaults(info, g.options[info.ID]),
		}
		if avail[i] != nil {
			ep.Unavailable = avail[i].Error()
		}
		if info.ID == g.active {
			ep.Active, ep.State, ep.Exposure, ep.Error = true, g.state, g.exposure, g.lastErr
		}
		out[i] = ep
	}
	return out
}

// Activate makes id the live provider with the given options, closing any
// other provider first. The choice is persisted.
func (g *Gateway) Activate(ctx context.Context, id string, options map[string]string) (Endpoint, error) {
	p := g.provider(id)
	if p == nil {
		return Endpoint{}, fmt.Errorf("unknown network provider %q", id)
	}
	g.op.Lock()
	if prev := g.exposed(); prev != nil {
		g.closeLocked(prev)
	}

	g.mu.Lock()
	g.active = id
	g.options[id] = withDefaults(p.Info(), options)
	g.failures, g.retryAt, g.lastErr = 0, time.Time{}, ""
	err := g.saveLocked()
	g.mu.Unlock()
	if err != nil {
		g.op.Unlock()
		return Endpoint{}, err
	}

	g.exposeLocked(ctx, p)
	g.op.Unlock()
	return g.endpoint(ctx, id), nil
}

// Deactivate turns networking off: nothing is exposed until a provider is
// activated again.
func (g *Gateway) Deactivate() error {
	g.op.Lock()
	defer g.op.Unlock()
	if p := g.exposed(); p != nil {
		g.closeLocked(p)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active, g.state, g.exposure, g.lastErr = "", StateOff, nil, ""
	return g.saveLocked()
}

// exposed returns the active provider if it may have a live exposure.
// Close is idempotent, so a "down" provider is included to clean up any
// partial state.
func (g *Gateway) exposed() Provider {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state == StateOff {
		return nil
	}
	return g.provider(g.active)
}

func (g *Gateway) endpoint(ctx context.Context, id string) Endpoint {
	for _, ep := range g.List(ctx) {
		if ep.ID == id {
			return ep
		}
	}
	return Endpoint{}
}

// reconcile exposes the active provider if needed and health-checks it.
func (g *Gateway) reconcile() {
	g.op.Lock()
	defer g.op.Unlock()

	g.mu.Lock()
	p := g.provider(g.active)
	state, retryAt := g.state, g.retryAt
	g.mu.Unlock()
	if p == nil {
		return
	}

	if state == StateUp {
		ctx, cancel := context.WithTimeout(context.Background(), statusTimeout)
		err := p.Status(ctx)
		cancel()
		if err == nil {
			return
		}
		g.log.Warn("exposure lost, re-exposing", "provider", p.Info().ID, "err", err)
		g.mu.Lock()
		g.state, g.exposure, g.lastErr = StateDown, nil, err.Error()
		g.mu.Unlock()
	} else if time.Now().Before(retryAt) {
		return
	}
	g.exposeLocked(context.Background(), p)
}

// exposeLocked runs Expose and records the outcome. Caller holds g.op.
func (g *Gateway) exposeLocked(ctx context.Context, p Provider) {
	id := p.Info().ID
	g.mu.Lock()
	g.state = StateStarting
	options := g.options[id]
	g.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, exposeTimeout)
	defer cancel()
	ex, err := p.Expose(ctx, g.target, options)

	g.mu.Lock()
	defer g.mu.Unlock()
	if err != nil {
		g.failures++
		g.state, g.exposure, g.lastErr = StateDown, nil, err.Error()
		g.retryAt = time.Now().Add(backoff(g.failures))
		g.log.Warn("expose failed", "provider", id, "err", err, "retryIn", backoff(g.failures))
		return
	}
	g.failures, g.retryAt = 0, time.Time{}
	g.state, g.exposure, g.lastErr = StateUp, &ex, ""
	g.log.Info("MCP endpoint exposed", "provider", id, "url", ex.URL, "public", ex.Public)
}

// closeLocked closes p's exposure. Caller holds g.op.
func (g *Gateway) closeLocked(p Provider) {
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	if err := p.Close(ctx); err != nil {
		g.log.Warn("close failed", "provider", p.Info().ID, "err", err)
	}
	g.mu.Lock()
	g.exposure = nil
	g.state = StateStarting // restored by the next reconcile if still active
	g.mu.Unlock()
}

func (g *Gateway) provider(id string) Provider {
	i := slices.IndexFunc(g.providers, func(p Provider) bool { return p.Info().ID == id })
	if i < 0 {
		return nil
	}
	return g.providers[i]
}

// backoff returns the delay before retrying after n consecutive failures:
// 20s, 40s, 80s... capped at 5 minutes.
func backoff(n int) time.Duration {
	return min(20*time.Second<<min(n-1, 4), 5*time.Minute)
}

type store struct {
	Active  string                       `json:"active,omitempty"`
	Options map[string]map[string]string `json:"options,omitempty"`
}

func (g *Gateway) load() error {
	raw, err := os.ReadFile(g.storePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var s store
	if err := json.Unmarshal(raw, &s); err != nil {
		g.log.Warn("ignoring unreadable network config", "path", g.storePath, "err", err)
		return nil
	}
	if g.provider(s.Active) != nil {
		g.active = s.Active
	}
	for id, o := range s.Options {
		g.options[id] = o
	}
	return nil
}

func (g *Gateway) saveLocked() error {
	raw, err := json.MarshalIndent(store{Active: g.active, Options: g.options}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(g.storePath), 0o755); err != nil {
		return err
	}
	tmp := g.storePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, g.storePath)
}
