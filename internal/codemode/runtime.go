// Package codemode runs AI-written JavaScript against the installed
// extensions, in the style of Cloudflare's "Code Mode": instead of issuing
// one tool call at a time, the model writes a script such as
//
//	const [a, b] = await Promise.all([
//	  tools.excel.readSheet({ path: q3 }),
//	  tools.excel.readSheet({ path: q4 }),
//	])
//	await tools.word.createDocument({ path: out, blocks: [...a, ...b].map(r => ({ text: r.join(" ") })) })
//	return a.length + b.length
//
// The script runs in an embedded JS engine (goja) with no filesystem,
// network or process access. Its only capability is the `tools` object,
// whose methods are proxied to the extension processes by the host.
//
// goja is single-threaded, so each run drives a small event loop: tool calls
// execute on their own goroutines (up to MaxConcurrency at once) and post
// their results back to the loop, which settles the matching promises on the
// VM goroutine. Independent calls therefore overlap.
package codemode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dop251/goja"
)

// ToolCaller is implemented by the extension manager. It must be safe for
// concurrent use.
type ToolCaller interface {
	CallTool(ctx context.Context, namespace, tool string, args json.RawMessage) (json.RawMessage, error)
}

// Namespace is the set of tools exposed as tools.<Name>.*.
type Namespace struct {
	Name  string
	Tools []string
}

type Options struct {
	Timeout time.Duration
	// MaxLogLines caps captured console output per run.
	MaxLogLines int
	// MaxConcurrency caps in-flight tool calls per run.
	MaxConcurrency int
}

type Result struct {
	Value any      `json:"value"`
	Logs  []string `json:"logs"`
	// ToolCalls is the number of tool calls the script made.
	ToolCalls int `json:"toolCalls"`
	// Tools lists the distinct tools called, as "namespace.tool", in order
	// of first use.
	Tools []string `json:"tools"`
	// Calls is a timeline of individual tool calls (the first
	// maxRecordedCalls of them).
	Calls []Call `json:"calls"`
}

// Call records one tool call for history.
type Call struct {
	Tool          string          `json:"tool"` // "namespace.tool"
	Args          json.RawMessage `json:"args,omitempty"`
	ArgsTruncated bool            `json:"argsTruncated,omitempty"`
	Status        string          `json:"status"` // pending, ok or error
	Error         string          `json:"error,omitempty"`
	StartedAt     time.Time       `json:"startedAt"`
	DurationMs    int64           `json:"durationMs"`
	ResultBytes   int             `json:"resultBytes,omitempty"`
}

const (
	maxRecordedCalls = 200
	maxRecordedArgs  = 4 << 10
)

type Runtime struct {
	caller ToolCaller
	opts   Options
}

func New(caller ToolCaller, opts Options) *Runtime {
	if opts.Timeout == 0 {
		opts.Timeout = 60 * time.Second
	}
	if opts.MaxLogLines == 0 {
		opts.MaxLogLines = 1000
	}
	if opts.MaxConcurrency == 0 {
		opts.MaxConcurrency = 16
	}
	return &Runtime{caller: caller, opts: opts}
}

// run is the state of a single script execution.
type run struct {
	*Runtime
	ctx context.Context
	vm  *goja.Runtime
	res *Result

	stringify goja.Callable
	parse     goja.Callable

	// pending counts tool calls whose promise is not yet settled. Only the
	// VM goroutine touches it.
	pending int
	// completions carries settle functions back to the VM goroutine.
	completions chan func() error
	// sem bounds concurrent tool calls.
	sem chan struct{}
	// seen dedupes Result.Tools. Only the VM goroutine touches it.
	seen map[string]bool
}

// Run executes code as the body of an async function. Each run gets a fresh
// VM so scripts cannot leak state into one another. Run returns as soon as
// the script settles; tool calls still in flight at that point are
// cancelled.
func (r *Runtime) Run(ctx context.Context, code string, namespaces []Namespace) (*Result, error) {
	return r.RunWithArgs(ctx, code, namespaces, nil)
}

// RunWithArgs is Run with a JSON object exposed to the script as the global
// `args` (saved scripts' parameters). Nil or empty args give `{}`.
func (r *Runtime) RunWithArgs(ctx context.Context, code string, namespaces []Namespace, args json.RawMessage) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()

	vm := goja.New()
	x := &run{
		Runtime:     r,
		ctx:         ctx,
		vm:          vm,
		res:         &Result{Logs: []string{}, Tools: []string{}, Calls: []Call{}},
		seen:        map[string]bool{},
		completions: make(chan func() error, r.opts.MaxConcurrency),
		sem:         make(chan struct{}, r.opts.MaxConcurrency),
	}

	stop := context.AfterFunc(ctx, func() { vm.Interrupt(ctx.Err()) })
	defer stop()

	if err := x.setup(namespaces); err != nil {
		return nil, err
	}
	if err := x.installArgs(args); err != nil {
		return nil, err
	}

	val, err := vm.RunString("(async () => {\n" + code + "\n})()")
	if err != nil {
		return x.res, x.wrapErr(err)
	}
	p, ok := val.Export().(*goja.Promise)
	if !ok {
		return x.res, errors.New("script did not produce a promise")
	}

	// Event loop: settle tool promises as their results arrive, until the
	// script's own promise settles.
	for p.State() == goja.PromiseStatePending {
		if x.pending == 0 {
			return x.res, errors.New("script is awaiting something that will never resolve")
		}
		select {
		case settle := <-x.completions:
			x.pending--
			if err := settle(); err != nil {
				return x.res, x.wrapErr(err)
			}
		case <-ctx.Done():
			return x.res, x.wrapErr(ctx.Err())
		}
	}

	if p.State() == goja.PromiseStateRejected {
		// Tool failures are Go errors; their "GoError: " prefix is noise.
		return x.res, fmt.Errorf("script error: %s", strings.TrimPrefix(p.Result().String(), "GoError: "))
	}
	v, err := x.toJSON(p.Result())
	if err != nil {
		return x.res, fmt.Errorf("script result is not JSON-serialisable: %w", err)
	}
	x.res.Value = v
	return x.res, nil
}

func (x *run) wrapErr(err error) error {
	if errors.Is(x.ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("script timed out after %s", x.opts.Timeout)
	}
	if x.ctx.Err() != nil {
		return x.ctx.Err()
	}
	return err
}

func (x *run) setup(namespaces []Namespace) error {
	jsonObj := x.vm.Get("JSON").ToObject(x.vm)
	var ok bool
	if x.stringify, ok = goja.AssertFunction(jsonObj.Get("stringify")); !ok {
		return errors.New("JSON.stringify unavailable")
	}
	if x.parse, ok = goja.AssertFunction(jsonObj.Get("parse")); !ok {
		return errors.New("JSON.parse unavailable")
	}
	if err := x.installConsole(); err != nil {
		return err
	}
	return x.installTools(namespaces)
}

// installArgs sets the global `args` to a JS copy of the JSON object raw.
func (x *run) installArgs(raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage("{}")
	}
	v, err := x.fromJSON(raw)
	if err != nil {
		return fmt.Errorf("invalid script arguments: %w", err)
	}
	return x.vm.Set("args", v)
}

func (x *run) installConsole() error {
	console := x.vm.NewObject()
	logFn := func(level string) func(goja.FunctionCall) goja.Value {
		return func(call goja.FunctionCall) goja.Value {
			parts := make([]string, len(call.Arguments))
			for i, a := range call.Arguments {
				parts[i] = a.String()
			}
			switch {
			case len(x.res.Logs) < x.opts.MaxLogLines:
				x.res.Logs = append(x.res.Logs, level+": "+strings.Join(parts, " "))
			case len(x.res.Logs) == x.opts.MaxLogLines:
				x.res.Logs = append(x.res.Logs, "… further output truncated")
			}
			return goja.Undefined()
		}
	}
	for _, lvl := range []string{"log", "info", "warn", "error"} {
		if err := console.Set(lvl, logFn(lvl)); err != nil {
			return err
		}
	}
	return x.vm.Set("console", console)
}

func (x *run) installTools(namespaces []Namespace) error {
	tools := x.vm.NewObject()
	names := make([]string, 0, len(namespaces))
	for _, ns := range namespaces {
		obj := x.vm.NewObject()
		for _, tool := range ns.Tools {
			if err := obj.Set(tool, x.toolFunc(ns.Name, tool)); err != nil {
				return err
			}
		}
		strict := x.strict(obj, "tools."+ns.Name, "Its tools: ", ns.Tools)
		if err := tools.Set(ns.Name, strict); err != nil {
			return err
		}
		names = append(names, ns.Name)
	}
	return x.vm.Set("tools", x.strict(tools, "tools", "Available: ", names))
}

// strict wraps obj so reading a member it doesn't have throws a TypeError
// naming what does exist, instead of yielding undefined and failing later
// with "Cannot read property 'x' of undefined".
func (x *run) strict(obj *goja.Object, path, listLabel string, members []string) goja.Proxy {
	return x.vm.NewProxy(obj, &goja.ProxyTrapConfig{
		Get: func(target *goja.Object, property string, _ goja.Value) goja.Value {
			if v := target.Get(property); v != nil {
				return v
			}
			if property == "then" { // probed by await and Promise.resolve
				return goja.Undefined()
			}
			list := "none"
			if len(members) > 0 {
				list = strings.Join(members, ", ")
			}
			panic(x.vm.NewTypeError("%s.%s does not exist. %s%s", path, property, listLabel, list))
		},
	})
}

// toolFunc returns the JS function for tools.<ns>.<tool>. It runs on the VM
// goroutine, starts the call in the background and returns a promise.
func (x *run) toolFunc(ns, tool string) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		promise, resolve, reject := x.vm.NewPromise()
		x.res.ToolCalls++
		if name := ns + "." + tool; !x.seen[name] {
			x.seen[name] = true
			x.res.Tools = append(x.res.Tools, name)
		}

		arg := call.Argument(0)
		if goja.IsUndefined(arg) || goja.IsNull(arg) {
			arg = x.vm.NewObject()
		}
		args, err := x.jsonBytes(arg)
		if err != nil {
			_ = reject(x.vm.NewGoError(fmt.Errorf("tools.%s.%s: arguments: %w", ns, tool, err)))
			return x.vm.ToValue(promise)
		}

		// Record the call; settle (on this goroutine) fills in the outcome.
		rec := -1
		if len(x.res.Calls) < maxRecordedCalls {
			c := Call{Tool: ns + "." + tool, Status: "pending", StartedAt: time.Now()}
			if len(args) <= maxRecordedArgs {
				c.Args = json.RawMessage(args)
			} else {
				c.ArgsTruncated = true
			}
			x.res.Calls = append(x.res.Calls, c)
			rec = len(x.res.Calls) - 1
		}

		x.pending++
		go func() {
			out, err := x.callTool(ns, tool, args)
			settle := func() error {
				if rec >= 0 {
					c := &x.res.Calls[rec]
					c.DurationMs = time.Since(c.StartedAt).Milliseconds()
					c.ResultBytes = len(out)
					if err != nil {
						c.Status, c.Error = "error", err.Error()
					} else {
						c.Status = "ok"
					}
				}
				if err != nil {
					return reject(x.vm.NewGoError(fmt.Errorf("tools.%s.%s: %w", ns, tool, err)))
				}
				v, perr := x.fromJSON(out)
				if perr != nil {
					return reject(x.vm.NewGoError(fmt.Errorf("tools.%s.%s: invalid result: %w", ns, tool, perr)))
				}
				return resolve(v)
			}
			select {
			case x.completions <- settle:
			case <-x.ctx.Done(): // Run has returned or is returning
			}
		}()
		return x.vm.ToValue(promise)
	}
}

func (x *run) callTool(ns, tool string, args []byte) (json.RawMessage, error) {
	select {
	case x.sem <- struct{}{}:
		defer func() { <-x.sem }()
	case <-x.ctx.Done():
		return nil, x.ctx.Err()
	}
	return x.caller.CallTool(x.ctx, ns, tool, args)
}

// jsonBytes runs the VM's JSON.stringify, so values follow JSON semantics
// (Dates become strings, functions and undefined are dropped). An undefined
// result is reported as "null".
func (x *run) jsonBytes(v goja.Value) ([]byte, error) {
	out, err := x.stringify(goja.Undefined(), v)
	if err != nil {
		return nil, err
	}
	if goja.IsUndefined(out) {
		return []byte("null"), nil
	}
	return []byte(out.String()), nil
}

// fromJSON turns a tool result into a native JS value with JSON.parse. This
// yields real JS objects and arrays (unlike wrapping Go maps) and skips an
// intermediate Go decode.
func (x *run) fromJSON(raw json.RawMessage) (goja.Value, error) {
	if len(raw) == 0 {
		return goja.Undefined(), nil
	}
	return x.parse(goja.Undefined(), x.vm.ToValue(string(raw)))
}

// toJSON converts a JS value to plain Go data via JSON, so results always
// serialise cleanly to the frontend or an MCP client.
func (x *run) toJSON(v goja.Value) (any, error) {
	raw, err := x.jsonBytes(v)
	if err != nil {
		return nil, err
	}
	var out any
	return out, json.Unmarshal(raw, &out)
}
