// Package runner executes code-mode scripts through the job queue. It is
// the single entry point shared by the desktop UI and the MCP server, so
// all callers share one queue and one concurrency budget.
package runner

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"nexora/internal/cache"
	"nexora/internal/codemode"
	"nexora/internal/extension"
	"nexora/internal/jobs"
	"nexora/internal/permissions"
	"nexora/internal/protocol"
	"nexora/internal/scripts"
)

type Runner struct {
	mgr   *extension.Manager
	rt    *codemode.Runtime
	queue *jobs.Queue
	perms *permissions.Store
	gate  *permissions.Gate
	store *cache.Store
}

// New wires scripts to extensions through the permission gate, and to
// store (tools.store) directly: the cache is Nexora's own scratch space, so
// it needs no approval. A nil store leaves tools.store out.
func New(mgr *extension.Manager, queue *jobs.Queue, perms *permissions.Store, store *cache.Store, approvalTimeout time.Duration) *Runner {
	gate := permissions.NewGate(mgr, mgr, perms, approvalTimeout)
	return &Runner{
		mgr: mgr,
		// The queue enforces the overall runtime limit (jobs.Options.MaxRuntime),
		// so a script's own timeout only needs to match it.
		rt:    codemode.New(builtins{next: gate, store: store}, codemode.Options{Timeout: 24 * time.Hour}),
		queue: queue,
		perms: perms,
		gate:  gate,
		store: store,
	}
}

// builtins routes tools.store to the cache and everything else to next.
type builtins struct {
	next  codemode.ToolCaller
	store *cache.Store
}

func (b builtins) CallTool(ctx context.Context, ns, tool string, args json.RawMessage) (json.RawMessage, error) {
	if ns == cache.Namespace && b.store != nil {
		return b.store.Call(tool, args)
	}
	return b.next.CallTool(ctx, ns, tool, args)
}

// Store is the cache behind tools.store (nil if disabled).
func (r *Runner) Store() *cache.Store { return r.store }

func (r *Runner) Gate() *permissions.Gate { return r.gate }

func (r *Runner) Permissions() *permissions.Store { return r.perms }

// WaitingFor lists the tool calls a job is waiting for the user to approve.
func (r *Runner) WaitingFor(jobID string) []string { return r.gate.PendingFor(jobID) }

func (r *Runner) Queue() *jobs.Queue { return r.queue }

// API returns the TypeScript declaration of `tools` for running extensions.
// Blocked tools are left out; tools that ask first say so, so the model can
// tell the user an approval is coming.
func (r *Runner) API() string {
	var api []codemode.APINamespace
	if r.store != nil {
		api = append(api, codemode.APINamespace{Name: cache.Namespace, Description: cache.Description, Tools: cache.Specs()})
	}
	for _, info := range r.running() {
		var tools []protocol.ToolSpec
		for _, t := range info.Tools {
			switch r.perms.Decide(info.Manifest.ID, t.Name, t.ReadOnly) {
			case permissions.Deny:
				continue
			case permissions.Ask:
				t.Description = strings.TrimSpace(t.Description + " (Asks the user for approval in Nexora before running.)")
			}
			tools = append(tools, t)
		}
		if len(tools) == 0 {
			continue
		}
		api = append(api, codemode.APINamespace{
			Name:        info.Manifest.Namespace,
			Description: info.Manifest.Description,
			Tools:       tools,
		})
	}
	return codemode.GenerateTypes(api)
}

// Sources of a run, recorded in history.
const (
	SourceApp      = "app"                  // the desktop UI
	SourceMCP      = "mcp"                  // an external AI client
	SourceSchedule = scripts.SourceSchedule // a saved script's schedule
)

// Submit enqueues a script. The job's Result is a *codemode.Result.
func (r *Runner) Submit(code, source string) (jobs.Job, error) {
	return r.SubmitSpec(jobs.Spec{Label: "run_code", Source: source, Input: code})
}

// SubmitSpec enqueues spec.Input as a script, with spec.Args as its global
// `args`. Saved scripts use it to carry their arguments and identity.
func (r *Runner) SubmitSpec(spec jobs.Spec) (jobs.Job, error) {
	return r.queue.Submit(spec, func(ctx context.Context) (any, error) {
		// The result is kept even on failure: its logs are often how the AI
		// debugs a script.
		res, err := r.rt.RunWithArgs(ctx, spec.Input, r.namespaces(), spec.Args)
		return res, err
	})
}

// Run enqueues a script and waits up to maxWait for it to finish.
func (r *Runner) Run(ctx context.Context, code, source string, maxWait time.Duration) (jobs.Job, error) {
	job, err := r.Submit(code, source)
	if err != nil {
		return jobs.Job{}, err
	}
	return r.queue.Wait(ctx, job.ID, maxWait)
}

// Extensions returns the namespaces of running extensions.
func (r *Runner) Extensions() []string {
	out := []string{}
	for _, info := range r.running() {
		out = append(out, info.Manifest.Namespace)
	}
	return out
}

func (r *Runner) namespaces() []codemode.Namespace {
	var out []codemode.Namespace
	if r.store != nil {
		out = append(out, codemode.Namespace{Name: cache.Namespace, Tools: cache.ToolNames()})
	}
	for _, info := range r.running() {
		ns := codemode.Namespace{Name: info.Manifest.Namespace}
		for _, t := range info.Tools {
			// Blocked tools are absent from the sandbox, so scripts fail
			// fast with "not a function" instead of reaching the gate.
			if r.perms.Decide(info.Manifest.ID, t.Name, t.ReadOnly) != permissions.Deny {
				ns.Tools = append(ns.Tools, t.Name)
			}
		}
		out = append(out, ns)
	}
	return out
}

func (r *Runner) running() []extension.Info {
	var out []extension.Info
	for _, info := range r.mgr.List() {
		if info.Status == extension.StatusRunning {
			out = append(out, info)
		}
	}
	return out
}
