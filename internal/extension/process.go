package extension

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"nexora/internal/protocol"
)

var ErrNotRunning = errors.New("extension process is not running")

// Process is a running, isolated extension speaking JSON-RPC over stdio.
type Process struct {
	manifest *Manifest
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	log      *slog.Logger

	writeMu sync.Mutex
	nextID  atomic.Int64

	mu      sync.Mutex
	pending map[int64]chan *protocol.Message
	closed  bool

	done    chan struct{}
	exitErr error // set before done is closed
}

type spawnOptions struct {
	Dir      string // absolute extension directory
	DataDir  string // absolute private data directory
	NodePath string
	// Folders granted by the user through folder settings.
	GrantRead, GrantWrite []string
}

func spawn(m *Manifest, opts spawnOptions, log *slog.Logger) (*Process, error) {
	cmd, err := buildCommand(m, opts)
	if err != nil {
		return nil, err
	}
	cmd.Dir = opts.Dir

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", m.ID, err)
	}

	p := &Process{
		manifest: m,
		cmd:      cmd,
		stdin:    stdin,
		log:      log.With("extension", m.ID),
		pending:  map[int64]chan *protocol.Message{},
		done:     make(chan struct{}),
	}

	var readers sync.WaitGroup
	readers.Go(func() { p.readLoop(stdout) })
	readers.Go(func() { p.logLoop(stderr) })
	go func() {
		// Wait must only be called once all reads from the pipes are done.
		readers.Wait()
		err := cmd.Wait()
		p.log.Debug("process exited", "err", err)
		p.exitErr = err
		p.fail(ErrNotRunning)
		close(p.done)
	}()
	return p, nil
}

// buildCommand turns a manifest into an exec.Cmd, applying sandboxing.
func buildCommand(m *Manifest, opts spawnOptions) (*exec.Cmd, error) {
	switch m.Runtime {
	case RuntimeNode:
		args := nodePermissionFlags(m.Permissions, opts)
		args = append(args, filepath.Join(opts.Dir, m.Main))
		cmd := exec.Command(opts.NodePath, args...)
		cmd.Env = minimalEnv(nil)
		return cmd, nil
	case RuntimeMCP:
		// TODO: MCP servers speak a different JSON-RPC dialect; route them
		// through internal/mcp's client adapter instead of this process.
		return nil, fmt.Errorf("extension %s: mcp runtime not implemented yet", m.ID)
	}
	return nil, fmt.Errorf("unknown runtime %q", m.Runtime)
}

// nodePermissionFlags maps manifest permissions onto the Node.js permission
// model so an extension can only touch what it declared. Directory grants
// are recursive.
func nodePermissionFlags(p Permissions, opts spawnOptions) []string {
	home, _ := os.UserHomeDir()
	expand := func(s string) string {
		s = strings.ReplaceAll(s, "$HOME", home)
		return strings.ReplaceAll(s, "$DATA", opts.DataDir)
	}
	flags := []string{
		"--permission",
		"--disable-warning=ExperimentalWarning",
		// The extension may always read its own code and use its data dir.
		"--allow-fs-read=" + opts.Dir,
		"--allow-fs-read=" + opts.DataDir,
		"--allow-fs-write=" + opts.DataDir,
	}
	for _, r := range p.FSRead {
		flags = append(flags, "--allow-fs-read="+expand(r))
	}
	for _, w := range p.FSWrite {
		flags = append(flags, "--allow-fs-write="+expand(w))
	}
	for _, r := range opts.GrantRead {
		flags = append(flags, "--allow-fs-read="+r)
	}
	for _, w := range opts.GrantWrite {
		flags = append(flags, "--allow-fs-write="+w)
	}
	if p.ChildProcess {
		flags = append(flags, "--allow-child-process")
	}
	if p.Network {
		flags = append(flags, "--allow-net")
	}
	return flags
}

func minimalEnv(extra map[string]string) []string {
	env := []string{"NODE_ENV=production"}
	for _, k := range []string{"PATH", "HOME", "LANG", "TMPDIR"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

// Done is closed once the process has exited.
func (p *Process) Done() <-chan struct{} { return p.done }

// ExitErr is the process exit error; only valid after Done is closed.
func (p *Process) ExitErr() error { return p.exitErr }

// Call sends a request and decodes the response into result (if non-nil).
func (p *Process) Call(ctx context.Context, method string, params, result any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	out, err := p.CallRaw(ctx, method, raw)
	if err != nil || result == nil || len(out) == 0 {
		return err
	}
	return json.Unmarshal(out, result)
}

// CallRaw sends a request whose params are already JSON-encoded and returns
// the raw JSON result. This is the hot path for tool calls: params and
// results pass through without being re-encoded or re-parsed. params may be
// split into parts that are concatenated on the wire, which avoids copying
// large payloads just to add an envelope around them.
func (p *Process) CallRaw(ctx context.Context, method string, params ...[]byte) (json.RawMessage, error) {
	id := p.nextID.Add(1)
	ch := make(chan *protocol.Message, 1)

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrNotRunning
	}
	p.pending[id] = ch
	p.mu.Unlock()

	if err := p.writeRequest(id, method, params); err != nil {
		p.forget(id)
		return nil, fmt.Errorf("write to extension: %w", err)
	}

	select {
	case <-ctx.Done():
		p.forget(id)
		return nil, ctx.Err()
	case msg := <-ch:
		if msg.Error != nil {
			return nil, msg.Error
		}
		return msg.Result, nil
	}
}

func (p *Process) forget(id int64) {
	p.mu.Lock()
	delete(p.pending, id)
	p.mu.Unlock()
}

// take removes and returns the waiter for id, so each waiter receives at
// most one message and sends never block.
func (p *Process) take(id int64) chan *protocol.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	ch := p.pending[id]
	delete(p.pending, id)
	return ch
}

func (p *Process) write(msg *protocol.Message) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return p.writeFrame(append(b, '\n'))
}

// largeFrame is the size above which frame parts are written separately
// rather than copied into one buffer.
const largeFrame = 64 << 10

// writeRequest assembles a request frame by hand. Marshalling a Message with
// a RawMessage field would re-validate and copy params, which dominates the
// cost for large tool arguments. The params parts must concatenate to valid
// JSON (no parts means {}).
func (p *Process) writeRequest(id int64, method string, params [][]byte) error {
	quotedMethod, err := json.Marshal(method)
	if err != nil {
		return err
	}
	var headBuf [96]byte
	head := headBuf[:0]
	head = append(head, `{"jsonrpc":"2.0","id":`...)
	head = strconv.AppendInt(head, id, 10)
	head = append(head, `,"method":`...)
	head = append(head, quotedMethod...)
	head = append(head, `,"params":`...)
	if len(params) == 0 {
		params = [][]byte{[]byte("{}")}
	}
	size := len(head) + 2
	for _, part := range params {
		size += len(part)
	}

	if size <= largeFrame {
		// One small buffer, one syscall.
		buf := make([]byte, 0, size)
		buf = append(buf, head...)
		for _, part := range params {
			buf = append(buf, part...)
		}
		return p.writeFrame(append(buf, "}\n"...))
	}

	frame := make(net.Buffers, 0, len(params)+2)
	frame = append(frame, head)
	frame = append(frame, params...)
	frame = append(frame, []byte("}\n"))
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	_, err = frame.WriteTo(p.stdin)
	return err
}

func (p *Process) writeFrame(frame []byte) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	_, err := p.stdin.Write(frame)
	return err
}

// readLoop uses bufio.Reader rather than Scanner so arbitrarily large frames
// are accepted; stopping early would stall the extension on a full pipe.
func (p *Process) readLoop(r io.Reader) {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			p.handleFrame(line)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
				p.log.Warn("reading extension stdout", "err", err)
			}
			return
		}
	}
}

func (p *Process) handleFrame(line []byte) {
	var msg protocol.Message
	if err := json.Unmarshal(line, &msg); err != nil {
		p.log.Warn("invalid frame from extension", "err", err)
		return
	}
	switch {
	case msg.Method != "" && msg.ID != nil:
		// Extension -> host requests are not part of the protocol yet.
		_ = p.write(&protocol.Message{JSONRPC: protocol.Version, ID: msg.ID, Error: &protocol.Error{
			Code: protocol.CodeMethodNotFound, Message: "unknown method " + msg.Method,
		}})
	case msg.Method != "":
		p.handleNotification(&msg)
	case msg.ID != nil:
		if ch := p.take(*msg.ID); ch != nil {
			ch <- &msg
		}
	}
}

func (p *Process) handleNotification(msg *protocol.Message) {
	switch msg.Method {
	case protocol.MethodLog:
		var lp protocol.LogParams
		_ = json.Unmarshal(msg.Params, &lp)
		p.log.Log(context.Background(), logLevel(lp.Level), lp.Message)
	default:
		p.log.Debug("unhandled notification", "method", msg.Method)
	}
}

func logLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

func (p *Process) logLoop(r io.Reader) {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := br.ReadString('\n')
		if line = strings.TrimRight(line, "\r\n"); line != "" {
			p.log.Info(line, "stream", "stderr")
		}
		if err != nil {
			return
		}
	}
}

// fail rejects all in-flight calls and refuses new ones.
func (p *Process) fail(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for id, ch := range p.pending {
		delete(p.pending, id)
		ch <- &protocol.Message{Error: &protocol.Error{Code: protocol.CodeInternalError, Message: err.Error()}}
	}
}

// Stop asks the extension to shut down gracefully, killing it if it has not
// exited by the time ctx is done. It always waits for the process to exit.
func (p *Process) Stop(ctx context.Context) error {
	select {
	case <-p.done:
		return nil
	default:
	}
	_ = p.Call(ctx, protocol.MethodShutdown, struct{}{}, nil)
	_ = p.stdin.Close()
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
		<-p.done
		return nil
	}
}
