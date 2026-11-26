package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"nexora/internal/protocol"
)

// mcpInitTimeout is longer than initTimeout: launchers such as npx or uvx
// may download the server on first start.
const mcpInitTimeout = 90 * time.Second

// mcpConn adapts an external MCP server (stdio or streamable HTTP) to the
// conn interface, so its tools appear under tools.<namespace>.* like any
// other extension's.
//
// Unlike Node extensions, a local MCP server is not sandboxed: it runs with
// the user's privileges and only gets a minimal environment.
type mcpConn struct {
	session *mcp.ClientSession
	cmd     *exec.Cmd // nil for remote servers
	log     *slog.Logger

	// names maps JS tool names to the server's tool names, which need not be
	// JS identifiers ("get-issue" is exposed as get_issue).
	names atomic.Pointer[map[string]string]

	done    chan struct{}
	exitErr error // set before done is closed
}

var _ conn = (*mcpConn)(nil)

type mcpOptions struct {
	Dir, DataDir string
	Specs        []SettingSpec
	Values       Values
	// OnToolsChanged receives the new tool list when the server announces
	// that it changed.
	OnToolsChanged func([]protocol.ToolSpec)
}

// connectMCP starts or connects to the server described by m, performs the
// MCP handshake and lists its tools.
func connectMCP(ctx context.Context, m *Manifest, opts mcpOptions, log *slog.Logger) (*mcpConn, []protocol.ToolSpec, error) {
	for _, s := range opts.Specs {
		if s.Required && isEmptySetting(opts.Values[s.Key]) {
			return nil, nil, fmt.Errorf("%s is not set; fill it in on the extension's panel", s.Label)
		}
	}
	x := newExpander(opts)
	c := &mcpConn{log: log.With("extension", m.ID), done: make(chan struct{})}

	var transport mcp.Transport
	if m.URL != "" {
		headers := http.Header{}
		for k, v := range m.Headers {
			headers.Set(k, x.str(v))
		}
		transport = &mcp.StreamableClientTransport{
			Endpoint:   x.str(m.URL),
			HTTPClient: &http.Client{Transport: headerTransport{headers: headers, base: http.DefaultTransport}},
		}
	} else {
		env := map[string]string{}
		for k, v := range m.Env {
			env[k] = x.str(v)
		}
		cmd := exec.Command(x.str(m.Command), x.args(m.Args)...)
		cmd.Dir = opts.Dir
		cmd.Env = mcpEnv(env)
		cmd.Stderr = &lineLogger{log: c.log}
		cmd.SysProcAttr = mcpProcAttr()
		// Don't hang on grandchildren that keep stderr open after the
		// server exits.
		cmd.WaitDelay = 2 * time.Second
		c.cmd = cmd
		transport = &mcp.CommandTransport{Command: cmd, TerminateDuration: 2 * time.Second}
	}

	var refreshing atomic.Bool
	client := mcp.NewClient(&mcp.Implementation{Name: "nexora", Version: HostVersion}, &mcp.ClientOptions{
		// No roots, sampling or elicitation: Nexora only calls tools.
		Capabilities: &mcp.ClientCapabilities{},
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			// Calling back into the session from a notification handler
			// could block its read loop, so refresh asynchronously.
			if opts.OnToolsChanged == nil || !refreshing.CompareAndSwap(false, true) {
				return
			}
			go func() {
				defer refreshing.Store(false)
				ctx, cancel := context.WithTimeout(context.Background(), initTimeout)
				defer cancel()
				tools, err := c.listTools(ctx)
				if err != nil {
					c.log.Warn("refreshing MCP tools", "err", err)
					return
				}
				opts.OnToolsChanged(tools)
			}()
		},
	})

	ctx, cancel := context.WithTimeout(ctx, mcpInitTimeout)
	defer cancel()
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, nil, mcpStartError(err, ctx)
	}
	c.session = session
	go func() {
		waitErr := session.Wait()
		// Close reaps the server process (if any) so its exit status is known.
		closeErr := session.Close()
		c.exitErr = c.describeExit(waitErr, closeErr)
		close(c.done)
	}()

	tools, err := c.listTools(ctx)
	if err != nil {
		stopCtx, stop := context.WithTimeout(context.Background(), stopTimeout)
		defer stop()
		_ = c.Stop(stopCtx)
		return nil, nil, fmt.Errorf("list tools: %w", mcpStartError(err, ctx))
	}
	return c, tools, nil
}

func mcpStartError(err error, ctx context.Context) error {
	if ctx.Err() != nil {
		return fmt.Errorf("MCP server did not respond within %s: %w", mcpInitTimeout, err)
	}
	return err
}

func (c *mcpConn) describeExit(waitErr, closeErr error) error {
	if c.cmd != nil && c.cmd.ProcessState != nil && !c.cmd.ProcessState.Success() {
		return errors.New(c.cmd.ProcessState.String())
	}
	if waitErr != nil && !errors.Is(waitErr, mcp.ErrConnectionClosed) {
		return waitErr
	}
	if closeErr != nil {
		return closeErr
	}
	return errors.New("MCP server disconnected")
}

// listTools fetches every tool and refreshes the name map.
func (c *mcpConn) listTools(ctx context.Context) ([]protocol.ToolSpec, error) {
	var specs []protocol.ToolSpec
	names := map[string]string{}
	for t, err := range c.session.Tools(ctx, nil) {
		if err != nil {
			return nil, err
		}
		spec, err := toolSpecFromMCP(t)
		if err != nil {
			c.log.Warn("ignoring MCP tool", "tool", t.Name, "err", err)
			continue
		}
		if _, dup := names[spec.Name]; dup {
			c.log.Warn("ignoring MCP tool with a clashing name", "tool", t.Name, "as", spec.Name)
			continue
		}
		names[spec.Name] = t.Name
		specs = append(specs, spec)
	}
	c.names.Store(&names)
	return specs, nil
}

func toolSpecFromMCP(t *mcp.Tool) (protocol.ToolSpec, error) {
	spec := protocol.ToolSpec{Name: jsToolName(t.Name), Description: t.Description}
	if spec.Description == "" && t.Title != "" {
		spec.Description = t.Title
	}
	if t.Annotations != nil {
		spec.ReadOnly = t.Annotations.ReadOnlyHint
	}
	if t.InputSchema != nil {
		raw, err := json.Marshal(t.InputSchema)
		if err != nil {
			return spec, fmt.Errorf("input schema: %w", err)
		}
		spec.InputSchema = raw
	}
	if t.OutputSchema != nil {
		raw, err := json.Marshal(t.OutputSchema)
		if err != nil {
			return spec, fmt.Errorf("output schema: %w", err)
		}
		spec.OutputSchema = raw
	}
	return spec, nil
}

// jsToolName turns an MCP tool name into a JS identifier: characters that
// aren't allowed become "_", and a leading digit gets a "_" prefix.
func jsToolName(name string) string {
	var b strings.Builder
	for i, r := range name {
		switch {
		case r == '_' || r == '$' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			if i == 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

func (c *mcpConn) CallTool(ctx context.Context, tool string, args json.RawMessage) (json.RawMessage, error) {
	name, ok := (*c.names.Load())[tool]
	if !ok {
		return nil, &protocol.Error{Code: protocol.CodeToolNotFound, Message: "unknown tool " + tool}
	}
	res, err := c.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		select {
		case <-c.done:
			return nil, ErrNotRunning
		default:
		}
		return nil, err
	}
	return mcpResult(res)
}

// mcpResult maps an MCP tool result onto a code-mode value: structured
// content as is, text as a string, anything else (images, resources) as the
// list of MCP content blocks. A tool error becomes a thrown error.
func mcpResult(res *mcp.CallToolResult) (json.RawMessage, error) {
	text, allText := contentText(res.Content)
	if res.IsError {
		if text == "" {
			text = "tool failed"
		}
		return nil, &protocol.Error{Code: protocol.CodeToolFailed, Message: text}
	}
	switch {
	case res.StructuredContent != nil:
		return json.Marshal(res.StructuredContent)
	case len(res.Content) == 0:
		return json.RawMessage("null"), nil
	case allText:
		return json.Marshal(text)
	}
	return json.Marshal(res.Content)
}

func contentText(content []mcp.Content) (text string, allText bool) {
	parts := make([]string, 0, len(content))
	allText = true
	for _, c := range content {
		if t, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, t.Text)
		} else {
			allText = false
		}
	}
	return strings.Join(parts, "\n"), allText
}

// UpdateSettings always restarts: settings are baked into the server's
// command line, environment or headers.
func (c *mcpConn) UpdateSettings(context.Context, Values) error { return errRestartRequired }

func (c *mcpConn) Stop(ctx context.Context) error {
	closed := make(chan struct{})
	go func() {
		_ = c.session.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-ctx.Done():
		if c.cmd != nil && c.cmd.Process != nil {
			if err := c.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				return err
			}
		}
	}
	<-c.done
	return nil
}

func (c *mcpConn) Done() <-chan struct{} { return c.done }
func (c *mcpConn) ExitErr() error        { return c.exitErr }

// mcpEnv is the environment of a local MCP server: the essentials to find
// and run programs, plus the manifest's variables. Secrets in the user's
// shell environment are not passed through.
func mcpEnv(extra map[string]string) []string {
	var env []string
	for _, k := range []string{
		"PATH", "HOME", "USER", "LANG", "TMPDIR",
		// Windows needs these to run node, npx, python, ...
		"SystemRoot", "SystemDrive", "ComSpec", "PATHEXT", "TEMP", "TMP",
		"USERPROFILE", "APPDATA", "LOCALAPPDATA", "ProgramFiles", "ProgramData",
	} {
		if v, ok := os.LookupEnv(k); ok {
			if _, override := extra[k]; !override {
				env = append(env, k+"="+v)
			}
		}
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

type headerTransport struct {
	headers http.Header
	base    http.RoundTripper
}

func (t headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, v := range t.headers {
		req.Header[k] = v
	}
	return t.base.RoundTrip(req)
}

// lineLogger logs a server's stderr line by line.
type lineLogger struct {
	log *slog.Logger
	mu  sync.Mutex
	buf []byte
}

func (l *lineLogger) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		if line := strings.TrimRight(string(l.buf[:i]), "\r"); line != "" {
			l.log.Info(line, "stream", "stderr")
		}
		l.buf = l.buf[i+1:]
	}
	if len(l.buf) > 64<<10 { // a runaway line without newlines
		l.log.Info(string(l.buf), "stream", "stderr")
		l.buf = l.buf[:0]
	}
	return len(p), nil
}

// templateVar matches ${name} references in MCP manifest values.
var templateVar = regexp.MustCompile(`\$\{([A-Za-z_$][A-Za-z0-9_$]*)\}`)

// Built-in template variables, besides setting keys.
var builtinVars = []string{"HOME", "DATA", "DIR"}

// expander substitutes ${name} with setting values and built-ins.
type expander map[string][]string

func newExpander(opts mcpOptions) expander {
	home, _ := os.UserHomeDir()
	x := expander{}
	for _, s := range opts.Specs {
		x[s.Key] = settingStrings(opts.Values[s.Key])
	}
	// Built-ins win, so a setting can't redirect ${DATA}.
	x["HOME"] = []string{home}
	x["DATA"] = []string{opts.DataDir}
	x["DIR"] = []string{opts.Dir}
	return x
}

// str expands every reference in s. A list value (folders) is joined with
// the OS path list separator.
func (x expander) str(s string) string {
	return templateVar.ReplaceAllStringFunc(s, func(ref string) string {
		return strings.Join(x[ref[2:len(ref)-1]], string(os.PathListSeparator))
	})
}

// args expands each argument. An argument that is exactly one reference to
// a list setting becomes one argument per item (none if the list is empty).
func (x expander) args(in []string) []string {
	out := make([]string, 0, len(in))
	for _, a := range in {
		if m := templateVar.FindStringSubmatch(a); m != nil && m[0] == a {
			out = append(out, x[m[1]]...)
			continue
		}
		out = append(out, x.str(a))
	}
	return out
}

func settingStrings(v any) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		return []string{t}
	case []string:
		return t
	case float64:
		return []string{strconv.FormatFloat(t, 'f', -1, 64)}
	case bool:
		return []string{strconv.FormatBool(t)}
	}
	return []string{fmt.Sprint(v)}
}

func isEmptySetting(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []string:
		return len(t) == 0
	}
	return false
}

// mcpTemplateRefs returns the variables referenced by an MCP manifest.
func mcpTemplateRefs(m *Manifest) []string {
	var refs []string
	add := func(s string) {
		for _, match := range templateVar.FindAllStringSubmatch(s, -1) {
			refs = append(refs, match[1])
		}
	}
	add(m.Command)
	add(m.URL)
	for _, a := range m.Args {
		add(a)
	}
	for _, v := range m.Env {
		add(v)
	}
	for _, v := range m.Headers {
		add(v)
	}
	return refs
}
