// Package tailscale exposes the Nexora MCP endpoint through Tailscale by
// talking to the local tailscaled daemon's LocalAPI in-process (the same
// API the `tailscale` CLI uses), with no subprocesses.
//
//   - funnel:  public HTTPS on https://<machine>.<tailnet>.ts.net:<port>/<path>.
//     Hosted AI services such as ChatGPT are not on your tailnet, so this is
//     the mode they need.
//   - tailnet: the same URL, reachable only from your own Tailscale devices.
//
// Only a dedicated path is mounted, plus the well-known OAuth discovery
// URLs for it (clients look for those at the root), so other `tailscale
// serve` handlers are left alone. Funnel is enabled per port, so the provider refuses to funnel
// a port that already serves other handlers (they would become public).
package tailscale

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"

	"nexora/internal/network"
)

// LocalAPI is the subset of the tailscaled LocalAPI client used here.
type LocalAPI interface {
	StatusWithoutPeers(ctx context.Context) (*ipnstate.Status, error)
	GetServeConfig(ctx context.Context) (*ipn.ServeConfig, error)
	SetServeConfig(ctx context.Context, config *ipn.ServeConfig) error
}

const (
	modeFunnel  = "funnel"
	modeTailnet = "tailnet"
)

type Provider struct {
	lc LocalAPI

	mu     sync.Mutex
	active *exposure
}

type exposure struct {
	host, path, mode, target string
	port                     uint16
	// discovery maps the root well-known OAuth metadata URLs for path to
	// the local server's equivalents for the target path.
	discovery map[string]string
}

// discoveryMounts returns the RFC 8414/9728 metadata locations for a
// resource mounted at path, proxied to the same documents for target.
func discoveryMounts(target *url.URL, path string) map[string]string {
	origin := target.Scheme + "://" + target.Host
	out := map[string]string{}
	for _, kind := range []string{"oauth-protected-resource", "oauth-authorization-server", "openid-configuration"} {
		out["/.well-known/"+kind+path] = origin + "/.well-known/" + kind + target.Path
	}
	return out
}

func (e *exposure) hostPort() ipn.HostPort {
	return ipn.HostPort(net.JoinHostPort(e.host, strconv.Itoa(int(e.port))))
}

// New uses the local tailscaled over its default socket.
func New() *Provider { return NewWithClient(&local.Client{}) }

func NewWithClient(lc LocalAPI) *Provider { return &Provider{lc: lc} }

func (p *Provider) Info() network.ProviderInfo {
	return network.ProviderInfo{
		ID:          "tailscale",
		Name:        "Tailscale",
		Description: "Reach Nexora on your *.ts.net address. Funnel makes it public for ChatGPT and other hosted AIs; tailnet keeps it to your own devices.",
		Settings: []network.Setting{
			{Key: "mode", Label: "Mode", Choices: []string{modeFunnel, modeTailnet}, Default: modeFunnel,
				Help: "funnel = public internet (needed for ChatGPT); tailnet = only your Tailscale devices"},
			{Key: "port", Label: "HTTPS port", Choices: []string{"8443", "443", "10000"}, Default: "8443",
				Help: "Funnel only supports 443, 8443 and 10000. A dedicated port avoids exposing your other services."},
			{Key: "path", Label: "Path", Default: "/nexora", Help: "URL path Nexora is mounted under"},
		},
	}
}

func (p *Provider) Available(ctx context.Context) error {
	_, err := p.status(ctx)
	return err
}

func (p *Provider) Expose(ctx context.Context, target *url.URL, opts map[string]string) (network.Exposure, error) {
	ex, err := p.plan(ctx, target, opts)
	if err != nil {
		return network.Exposure{}, err
	}
	err = p.updateServeConfig(ctx, func(sc *ipn.ServeConfig) error {
		if err := checkConflicts(sc, ex); err != nil {
			return err
		}
		sc.SetWebHandler(&ipn.HTTPHandler{Proxy: ex.target}, ex.host, ex.port, ex.path, true, "")
		for mount, proxy := range ex.discovery {
			// Best effort: a mount someone else uses is left alone; clients
			// then find the metadata under path instead.
			if web := sc.Web[ex.hostPort()]; web != nil && web.Handlers[mount] != nil && web.Handlers[mount].Proxy != proxy {
				continue
			}
			sc.SetWebHandler(&ipn.HTTPHandler{Proxy: proxy}, ex.host, ex.port, mount, true, "")
		}
		sc.SetFunnel(ex.host, ex.port, ex.mode == modeFunnel)
		return nil
	})
	if err != nil {
		return network.Exposure{}, err
	}

	p.mu.Lock()
	p.active = ex
	p.mu.Unlock()

	u := "https://" + ex.host
	if ex.port != 443 {
		u += ":" + strconv.Itoa(int(ex.port))
	}
	out := network.Exposure{URL: u + ex.path, Public: ex.mode == modeFunnel}
	if out.Public {
		out.Message = "Public via Tailscale Funnel: reachable by ChatGPT and other hosted AI services."
	} else {
		out.Message = "Tailnet only: reachable from your own Tailscale devices, not by hosted services like ChatGPT."
	}
	return out, nil
}

// plan validates the environment and options before touching any config.
func (p *Provider) plan(ctx context.Context, target *url.URL, opts map[string]string) (*exposure, error) {
	st, err := p.status(ctx)
	if err != nil {
		return nil, err
	}
	host := strings.TrimSuffix(st.Self.DNSName, ".")
	if host == "" {
		return nil, errors.New("this machine has no MagicDNS name; enable MagicDNS in the Tailscale admin console (https://login.tailscale.com/admin/dns)")
	}
	if len(st.CertDomains) == 0 {
		return nil, errors.New("HTTPS certificates are not enabled for your tailnet; enable them at https://login.tailscale.com/admin/dns")
	}

	ex := &exposure{host: host, mode: opts["mode"], target: target.String()}
	if ex.mode != modeFunnel && ex.mode != modeTailnet {
		return nil, fmt.Errorf("unknown mode %q (use %s or %s)", ex.mode, modeFunnel, modeTailnet)
	}
	port, err := strconv.ParseUint(opts["port"], 10, 16)
	if err != nil || port == 0 {
		return nil, fmt.Errorf("invalid port %q", opts["port"])
	}
	ex.port = uint16(port)
	if ex.path, err = normalisePath(opts["path"]); err != nil {
		return nil, err
	}
	ex.discovery = discoveryMounts(target, ex.path)
	if ex.mode == modeFunnel {
		// Checks the node's funnel/https capabilities and allowed ports;
		// its errors link to the relevant admin settings.
		if err := ipn.CheckFunnelAccess(ex.port, st.Self); err != nil {
			return nil, fmt.Errorf("tailscale funnel: %w", err)
		}
	}
	return ex, nil
}

// checkConflicts refuses changes that would break or expose the user's
// other serve config.
func checkConflicts(sc *ipn.ServeConfig, ex *exposure) error {
	hp := ex.hostPort()
	if tcp := sc.TCP[ex.port]; tcp != nil && tcp.TCPForward != "" {
		return fmt.Errorf("port %d already forwards TCP to %s; choose another port", ex.port, tcp.TCPForward)
	}
	var others []string
	if web := sc.Web[hp]; web != nil {
		for mount, h := range web.Handlers {
			switch {
			case mount == ex.path && h.Proxy == ex.target:
				// Ours, e.g. left over from a crash: safe to overwrite.
			case mount == ex.path:
				return fmt.Errorf("path %s on port %d is already used by another service; choose another path", ex.path, ex.port)
			case ex.discovery[mount] != "" && ex.discovery[mount] == h.Proxy:
				// Our discovery mount.
			default:
				others = append(others, mount)
			}
		}
	}
	if len(others) > 0 && ex.mode == modeFunnel && !sc.AllowFunnel[hp] {
		return fmt.Errorf("port %d already serves %s privately; Funnel would make it public. Choose another port (443, 8443 or 10000)",
			ex.port, strings.Join(others, ", "))
	}
	if ex.mode == modeTailnet && sc.AllowFunnel[hp] {
		return fmt.Errorf("port %d is publicly funnelled by another service, so Nexora would be public too; choose another port", ex.port)
	}
	return nil
}

func (p *Provider) Status(ctx context.Context) error {
	p.mu.Lock()
	ex := p.active
	p.mu.Unlock()
	if ex == nil {
		return errors.New("not exposed")
	}
	if _, err := p.status(ctx); err != nil {
		return err
	}
	sc, err := p.lc.GetServeConfig(ctx)
	if err != nil {
		return explain(err)
	}
	hp := ex.hostPort()
	web := sc.Web[hp]
	if web == nil || web.Handlers[ex.path] == nil || web.Handlers[ex.path].Proxy != ex.target {
		return errors.New("the Tailscale serve config was changed outside Nexora")
	}
	if ex.mode == modeFunnel && !sc.AllowFunnel[hp] {
		return errors.New("Funnel was turned off for this port outside Nexora")
	}
	return nil
}

func (p *Provider) Close(ctx context.Context) error {
	p.mu.Lock()
	ex := p.active
	p.active = nil
	p.mu.Unlock()
	if ex == nil {
		return nil
	}
	return p.updateServeConfig(ctx, func(sc *ipn.ServeConfig) error {
		hp := ex.hostPort()
		web := sc.Web[hp]
		if web == nil || web.Handlers[ex.path] == nil || web.Handlers[ex.path].Proxy != ex.target {
			return nil // already gone, or replaced by someone else
		}
		mounts := []string{ex.path}
		for mount, proxy := range ex.discovery {
			if h := web.Handlers[mount]; h != nil && h.Proxy == proxy {
				mounts = append(mounts, mount)
			}
		}
		sc.RemoveWebHandler(ex.host, ex.port, mounts, true)
		if ex.mode == modeFunnel {
			// We turned Funnel on, so turn it off even if other handlers were
			// added to the port meanwhile: they were never meant to be public.
			sc.SetFunnel(ex.host, ex.port, false)
		}
		return nil
	})
}

// updateServeConfig applies mutate with optimistic concurrency, retrying
// if the config changed between read and write (e.g. a concurrent CLI call).
func (p *Provider) updateServeConfig(ctx context.Context, mutate func(*ipn.ServeConfig) error) error {
	var err error
	for range 3 {
		var sc *ipn.ServeConfig
		if sc, err = p.lc.GetServeConfig(ctx); err != nil {
			return explain(err)
		}
		if err = mutate(sc); err != nil {
			return err
		}
		if err = p.lc.SetServeConfig(ctx, sc); err == nil || !local.IsPreconditionsFailedError(err) {
			break
		}
	}
	return explain(err)
}

func (p *Provider) status(ctx context.Context) (*ipnstate.Status, error) {
	st, err := p.lc.StatusWithoutPeers(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot reach tailscaled (is Tailscale installed and running?): %w", err)
	}
	switch {
	case st.BackendState == ipn.Running.String() && st.Self != nil:
		return st, nil
	case st.AuthURL != "":
		return nil, fmt.Errorf("Tailscale needs you to log in: %s", st.AuthURL)
	default:
		return nil, fmt.Errorf("Tailscale is %s; run `tailscale up`", st.BackendState)
	}
}

// explain turns LocalAPI errors into actionable messages.
func explain(err error) error {
	if err != nil && local.IsAccessDeniedError(err) {
		return fmt.Errorf("tailscaled denied access to serve config; allow your user once with `sudo tailscale set --operator=$USER` (%w)", err)
	}
	return err
}

func normalisePath(p string) (string, error) {
	p = "/" + strings.Trim(p, "/")
	if p == "/" {
		return "", errors.New("path must not be the root; it would take over the whole port")
	}
	if strings.ContainsAny(p, " ?#") {
		return "", fmt.Errorf("invalid path %q", p)
	}
	return p, nil
}
