// Package cloudflare exposes the Nexora MCP endpoint through a cloudflared
// quick tunnel: a random https://<words>.trycloudflare.com URL with no
// Cloudflare account needed. The URL changes each time the tunnel starts.
// Cloudflare ends requests after 100 s, which the MCP server's inline wait
// (run_code / get_job) stays under.
package cloudflare

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"nexora/internal/network"
)

var urlPattern = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

type Provider struct {
	// Binary is the cloudflared executable (default: looked up on PATH).
	Binary string

	mu     sync.Mutex
	cmd    *exec.Cmd
	exited chan struct{} // closed when cmd exits
}

func New() *Provider { return &Provider{Binary: "cloudflared"} }

func (p *Provider) Info() network.ProviderInfo {
	return network.ProviderInfo{
		ID:          "cloudflare",
		Name:        "Cloudflare Tunnel",
		Description: "Public trycloudflare.com URL through a cloudflared quick tunnel. No account needed; the URL changes on every start.",
	}
}

func (p *Provider) Available(context.Context) error {
	if _, err := exec.LookPath(p.Binary); err != nil {
		return errors.New("cloudflared is not installed: https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/")
	}
	return nil
}

func (p *Provider) Expose(ctx context.Context, target *url.URL, _ map[string]string) (network.Exposure, error) {
	if err := p.Available(ctx); err != nil {
		return network.Exposure{}, err
	}
	if err := p.Close(ctx); err != nil {
		return network.Exposure{}, err
	}

	origin := (&url.URL{Scheme: target.Scheme, Host: target.Host}).String()
	// Not CommandContext: the tunnel must outlive this call's ctx.
	cmd := exec.Command(p.Binary, "tunnel", "--no-autoupdate", "--url", origin)
	cmd.SysProcAttr = sysProcAttr()
	out, err := cmd.StderrPipe() // cloudflared logs to stderr
	if err != nil {
		return network.Exposure{}, err
	}
	cmd.Stdout = cmd.Stderr
	if err := cmd.Start(); err != nil {
		return network.Exposure{}, fmt.Errorf("start cloudflared: %w", err)
	}

	exited := make(chan struct{})
	found := make(chan string, 1)
	var tail lastLines
	go func() {
		// Keep draining so cloudflared never blocks on a full pipe.
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			line := sc.Text()
			tail.add(line)
			if u := urlPattern.FindString(line); u != "" {
				select {
				case found <- u:
				default:
				}
			}
		}
		_, _ = io.Copy(io.Discard, out)
	}()
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()

	select {
	case u := <-found:
		p.mu.Lock()
		p.cmd, p.exited = cmd, exited
		p.mu.Unlock()
		return network.Exposure{
			URL:     u + strings.TrimSuffix(target.Path, "/"),
			Public:  true,
			Message: "Public quick tunnel. The URL changes whenever the tunnel restarts.",
		}, nil
	case <-exited:
		return network.Exposure{}, fmt.Errorf("cloudflared exited before the tunnel was ready:\n%s", tail.String())
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-exited
		return network.Exposure{}, fmt.Errorf("cloudflared did not report a tunnel URL in time:\n%s", tail.String())
	}
}

func (p *Provider) Status(context.Context) error {
	p.mu.Lock()
	exited := p.exited
	p.mu.Unlock()
	if exited == nil {
		return errors.New("not exposed")
	}
	select {
	case <-exited:
		return errors.New("cloudflared exited")
	default:
		return nil
	}
}

func (p *Provider) Close(ctx context.Context) error {
	p.mu.Lock()
	cmd, exited := p.cmd, p.exited
	p.cmd, p.exited = nil, nil
	p.mu.Unlock()
	if cmd == nil {
		return nil
	}
	_ = cmd.Process.Signal(interruptSignal)
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-exited
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-exited
	}
	return nil
}

// lastLines keeps the tail of cloudflared's output for error messages.
type lastLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *lastLines) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, s)
	if len(l.lines) > 10 {
		l.lines = l.lines[1:]
	}
}

func (l *lastLines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}
