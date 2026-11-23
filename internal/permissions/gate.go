package permissions

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"nexora/internal/codemode"
	"nexora/internal/jobs"
	"nexora/internal/protocol"
)

// Tools resolves a namespace and tool to its extension and spec.
type Tools interface {
	Tool(namespace, tool string) (extID string, spec protocol.ToolSpec, ok, found bool)
}

// Choice is the user's answer to an approval request.
type Choice string

const (
	AllowOnce   Choice = "once"   // run this call only
	AllowRun    Choice = "run"    // allow the tool until this script ends
	AllowAlways Choice = "always" // run it and allow the tool from now on
	DenyOnce    Choice = "deny"   // reject this call only
	DenyAlways  Choice = "never"  // reject it and block the tool from now on
)

var (
	ErrDenied     = errors.New("blocked by the user's permission settings")
	ErrRejected   = errors.New("the user declined this call")
	ErrNotDecided = errors.New("the user did not approve this call in time")
)

// Approval is a tool call waiting for the user.
type Approval struct {
	ID          string          `json:"id"`
	JobID       string          `json:"jobId,omitempty"`
	Source      string          `json:"source,omitempty"` // who ran the script: "app" or "mcp"
	ExtensionID string          `json:"extensionId"`
	Namespace   string          `json:"namespace"`
	Tool        string          `json:"tool"`
	Description string          `json:"description,omitempty"`
	Args        json.RawMessage `json:"args"`
	CreatedAt   time.Time       `json:"createdAt"`
	ExpiresAt   time.Time       `json:"expiresAt"`

	answer chan Choice
}

// Event records a permission outcome during a run, for its history.
type Event struct {
	Tool string `json:"tool"` // "namespace.tool"
	// Outcome: allowed-once, allowed-run, allowed-always, denied, denied-always,
	// timed-out, cancelled (the run stopped while asking) or blocked (by
	// policy, without asking).
	Outcome   string          `json:"outcome"`
	Args      json.RawMessage `json:"args,omitempty"`
	AskedAt   time.Time       `json:"askedAt"`
	DecidedAt time.Time       `json:"decidedAt"`
}

const maxEventsPerJob = 100

// Gate wraps a ToolCaller and enforces the permission policy on every call
// a script makes. It is the only path from code mode to extensions.
type Gate struct {
	next    codemode.ToolCaller
	tools   Tools
	store   *Store
	timeout time.Duration

	mu      sync.Mutex
	pending map[string]*Approval
	events  map[string][]Event // by job id, until TakeEvents
	// runAllow holds "allow for this run" answers: job id -> "ns.tool".
	runAllow map[string]map[string]bool
	// OnChange, if set, is called (in its own goroutine) whenever the set of
	// pending approvals changes, e.g. to show or close the ask window.
	OnChange func(pending []Approval)
}

func NewGate(next codemode.ToolCaller, tools Tools, store *Store, timeout time.Duration) *Gate {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	return &Gate{next: next, tools: tools, store: store, timeout: timeout,
		pending: map[string]*Approval{}, events: map[string][]Event{}, runAllow: map[string]map[string]bool{}}
}

// CallTool implements codemode.ToolCaller.
func (g *Gate) CallTool(ctx context.Context, ns, tool string, args json.RawMessage) (json.RawMessage, error) {
	extID, spec, ok, found := g.tools.Tool(ns, tool)
	if !ok || !found {
		// Unknown tools are rejected by the extension (or the manager) with a
		// clearer error; there is nothing to authorise.
		return g.next.CallTool(ctx, ns, tool, args)
	}
	switch g.store.Decide(extID, tool, spec.ReadOnly) {
	case Allow:
		return g.next.CallTool(ctx, ns, tool, args)
	case Deny:
		now := time.Now()
		g.record(ctx, Event{Tool: ns + "." + tool, Outcome: "blocked", AskedAt: now, DecidedAt: now})
		return nil, ErrDenied
	}
	if err := g.ask(ctx, extID, ns, spec, args); err != nil {
		return nil, err
	}
	return g.next.CallTool(ctx, ns, tool, args)
}

// ask blocks until the user answers, the request expires or ctx ends.
func (g *Gate) ask(ctx context.Context, extID, ns string, spec protocol.ToolSpec, args json.RawMessage) error {
	info, hasJob := jobs.FromContext(ctx)
	if hasJob {
		g.mu.Lock()
		allowed := g.runAllow[info.ID][ns+"."+spec.Name]
		g.mu.Unlock()
		if allowed {
			return nil
		}
	}
	now := time.Now()
	a := &Approval{
		ID: newID(), ExtensionID: extID, Namespace: ns, Tool: spec.Name, Description: spec.Description,
		Args: args, CreatedAt: now, ExpiresAt: now.Add(g.timeout),
		answer: make(chan Choice, 1),
	}
	if hasJob {
		a.JobID, a.Source = info.ID, info.Source
	}

	g.mu.Lock()
	g.pending[a.ID] = a
	g.mu.Unlock()
	g.changed()
	defer func() {
		g.mu.Lock()
		delete(g.pending, a.ID)
		g.mu.Unlock()
		g.changed()
	}()

	outcome, err := g.wait(ctx, a, extID, spec.Name)
	g.record(ctx, Event{Tool: ns + "." + spec.Name, Outcome: outcome, Args: args, AskedAt: a.CreatedAt, DecidedAt: time.Now()})
	return err
}

func (g *Gate) wait(ctx context.Context, a *Approval, extID, tool string) (outcome string, err error) {
	timer := time.NewTimer(g.timeout)
	defer timer.Stop()
	select {
	case c := <-a.answer:
		switch c {
		case AllowOnce:
			return "allowed-once", nil
		case AllowRun:
			if a.JobID != "" {
				g.mu.Lock()
				if g.runAllow[a.JobID] == nil {
					g.runAllow[a.JobID] = map[string]bool{}
				}
				g.runAllow[a.JobID][a.Namespace+"."+tool] = true
				g.mu.Unlock()
			}
			return "allowed-run", nil
		case AllowAlways:
			return "allowed-always", g.store.SetTool(extID, tool, Allow)
		case DenyAlways:
			if err := g.store.SetTool(extID, tool, Deny); err != nil {
				return "denied-always", err
			}
			return "denied-always", ErrRejected
		default:
			return "denied", ErrRejected
		}
	case <-timer.C:
		return "timed-out", ErrNotDecided
	case <-ctx.Done():
		return "cancelled", ctx.Err()
	}
}

func (g *Gate) record(ctx context.Context, e Event) {
	info, ok := jobs.FromContext(ctx)
	if !ok {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.events[info.ID]) < maxEventsPerJob {
		g.events[info.ID] = append(g.events[info.ID], e)
	}
}

// Events returns the permission events recorded so far for a running job.
func (g *Gate) Events(jobID string) []Event {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]Event(nil), g.events[jobID]...)
}

// TakeEvents returns and forgets the permission events recorded for a job.
func (g *Gate) TakeEvents(jobID string) []Event {
	g.mu.Lock()
	defer g.mu.Unlock()
	ev := g.events[jobID]
	delete(g.events, jobID)
	delete(g.runAllow, jobID) // the run is over
	return ev
}

func (g *Gate) changed() {
	if g.OnChange != nil {
		go g.OnChange(g.Pending())
	}
}

// Resolve answers a pending approval.
func (g *Gate) Resolve(id string, c Choice) error {
	switch c {
	case AllowOnce, AllowRun, AllowAlways, DenyOnce, DenyAlways:
	default:
		return fmt.Errorf("unknown choice %q", c)
	}
	g.mu.Lock()
	a, ok := g.pending[id]
	if ok {
		delete(g.pending, id) // answer at most once
	}
	g.mu.Unlock()
	if !ok {
		return errors.New("this request is no longer waiting (it was answered, expired or cancelled)")
	}
	a.answer <- c
	return nil
}

// Pending lists approvals waiting for the user, oldest first.
func (g *Gate) Pending() []Approval {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Approval, 0, len(g.pending))
	for _, a := range g.pending {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// PendingFor lists the tools a job is waiting on, as "namespace.tool".
func (g *Gate) PendingFor(jobID string) []string {
	var out []string
	for _, a := range g.Pending() {
		if a.JobID == jobID {
			out = append(out, a.Namespace+"."+a.Tool)
		}
	}
	return out
}

func newID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "apr_" + hex.EncodeToString(b[:])
}
