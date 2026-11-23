package permissions

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"nexora/internal/jobs"
	"nexora/internal/protocol"
)

func TestModeDefaults(t *testing.T) {
	for _, tc := range []struct {
		mode     Mode
		readOnly bool
		want     Decision
	}{
		{ModeTrusted, false, Allow}, {ModeTrusted, true, Allow},
		{ModeAsk, true, Allow}, {ModeAsk, false, Ask},
		{ModeReadOnly, true, Allow}, {ModeReadOnly, false, Deny},
	} {
		if got := tc.mode.Default(tc.readOnly); got != tc.want {
			t.Errorf("%s readOnly=%v: got %s, want %s", tc.mode, tc.readOnly, got, tc.want)
		}
	}
}

func TestStoreOverridesAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "permissions.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if d := s.Decide("excel", "writeSheet", false); d != Ask {
		t.Fatalf("unconfigured extensions default to ask-first, got %s", d)
	}
	must(t, s.SetMode("excel", ModeReadOnly))
	must(t, s.SetTool("excel", "writeSheet", Ask))
	must(t, s.SetTool("excel", "readSheet", Deny))
	if s.Decide("excel", "writeSheet", false) != Ask || s.Decide("excel", "readSheet", true) != Deny ||
		s.Decide("excel", "deleteSheet", false) != Deny {
		t.Fatal("overrides must win over the mode")
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if p := s2.Get("excel"); p.Mode != ModeReadOnly || p.Tools["writeSheet"] != Ask {
		t.Fatalf("not persisted: %+v", p)
	}
	must(t, s2.SetTool("excel", "writeSheet", "")) // back to the mode
	if s2.Decide("excel", "writeSheet", false) != Deny {
		t.Fatal("clearing an override should restore the mode default")
	}
	must(t, s2.Reset("excel"))
	if s2.Get("excel").Mode != DefaultMode {
		t.Fatal("reset should forget the policy")
	}
	if s2.SetMode("excel", "yolo") == nil || s2.SetTool("excel", "x", "maybe") == nil {
		t.Fatal("invalid values must be rejected")
	}
}

// fakeTools: namespace "x" belongs to extension "ext" with read and write tools.
type fakeTools struct{}

func (fakeTools) Tool(ns, tool string) (string, protocol.ToolSpec, bool, bool) {
	if ns != "x" {
		return "", protocol.ToolSpec{}, false, false
	}
	switch tool {
	case "read":
		return "ext", protocol.ToolSpec{Name: "read", ReadOnly: true}, true, true
	case "write":
		return "ext", protocol.ToolSpec{Name: "write"}, true, true
	}
	return "ext", protocol.ToolSpec{}, true, false
}

type countingCaller struct{ calls atomic.Int32 }

func (c *countingCaller) CallTool(context.Context, string, string, json.RawMessage) (json.RawMessage, error) {
	c.calls.Add(1)
	return json.RawMessage(`"ok"`), nil
}

func newGate(t *testing.T, timeout time.Duration) (*Gate, *Store, *countingCaller) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "p.json"))
	must(t, err)
	c := &countingCaller{}
	return NewGate(c, fakeTools{}, s, timeout), s, c
}

// answer waits for an approval to appear and resolves it.
func answer(t *testing.T, g *Gate, c Choice) Approval {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if p := g.Pending(); len(p) > 0 {
			must(t, g.Resolve(p[0].ID, c))
			return p[0]
		}
		if time.Now().After(deadline) {
			t.Fatal("no approval appeared")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestGateAllowDenyAsk(t *testing.T) {
	g, s, c := newGate(t, time.Minute)
	ctx := context.Background()

	// Reads pass in the default mode.
	if _, err := g.CallTool(ctx, "x", "read", nil); err != nil {
		t.Fatal(err)
	}

	// Writes wait; approving once runs this call only.
	done := make(chan error, 1)
	go func() { _, err := g.CallTool(ctx, "x", "write", json.RawMessage(`{"path":"/tmp/a"}`)); done <- err }()
	a := answer(t, g, AllowOnce)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if a.Tool != "write" || a.ExtensionID != "ext" || string(a.Args) != `{"path":"/tmp/a"}` {
		t.Fatalf("approval missing details: %+v", a)
	}
	if s.Get("ext").Tools["write"] != "" {
		t.Fatal("allow once must not change the policy")
	}

	// Denying once rejects without running.
	before := c.calls.Load()
	go func() { _, err := g.CallTool(ctx, "x", "write", nil); done <- err }()
	answer(t, g, DenyOnce)
	if err := <-done; !errors.Is(err, ErrRejected) || c.calls.Load() != before {
		t.Fatalf("got %v", err)
	}

	// "Always" answers become overrides.
	go func() { _, err := g.CallTool(ctx, "x", "write", nil); done <- err }()
	answer(t, g, AllowAlways)
	must(t, <-done)
	if s.Decide("ext", "write", false) != Allow {
		t.Fatal("always allow should persist")
	}
	if _, err := g.CallTool(ctx, "x", "write", nil); err != nil || len(g.Pending()) != 0 {
		t.Fatal("an allowed tool must not ask again")
	}

	must(t, s.SetTool("ext", "write", Ask))
	go func() { _, err := g.CallTool(ctx, "x", "write", nil); done <- err }()
	answer(t, g, DenyAlways)
	if err := <-done; !errors.Is(err, ErrRejected) {
		t.Fatal(err)
	}
	if _, err := g.CallTool(ctx, "x", "write", nil); !errors.Is(err, ErrDenied) {
		t.Fatalf("never should block from now on, got %v", err)
	}
}

func TestGateTimeoutCancelAndJobInfo(t *testing.T) {
	g, _, c := newGate(t, 50*time.Millisecond)
	if _, err := g.CallTool(context.Background(), "x", "write", nil); !errors.Is(err, ErrNotDecided) {
		t.Fatalf("expected a timeout, got %v", err)
	}
	if len(g.Pending()) != 0 || c.calls.Load() != 0 {
		t.Fatal("expired requests must be cleaned up and not run")
	}

	g, _, _ = newGate(t, time.Minute)
	q := jobs.New(jobs.Options{Workers: 1})
	defer q.Close()
	job, _ := q.Submit(jobs.Spec{Source: "mcp"}, func(ctx context.Context) (any, error) {
		return g.CallTool(ctx, "x", "write", nil)
	})
	deadline := time.Now().Add(3 * time.Second)
	for len(g.PendingFor(job.ID)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("approval should be tagged with its job")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if p := g.Pending()[0]; p.Source != "mcp" || g.PendingFor(job.ID)[0] != "x.write" {
		t.Fatalf("got %+v", p)
	}
	q.Cancel(job.ID) // cancelling the run withdraws the request
	if j, _ := q.Wait(context.Background(), job.ID, time.Second); j.State != jobs.StateCancelled || len(g.Pending()) != 0 {
		t.Fatalf("state %s, pending %d", j.State, len(g.Pending()))
	}
	ev := g.TakeEvents(job.ID)
	if len(ev) != 1 || ev[0].Outcome != "cancelled" || ev[0].Tool != "x.write" {
		t.Fatalf("events: %+v", ev)
	}
	if len(g.TakeEvents(job.ID)) != 0 {
		t.Fatal("events are handed over once")
	}
	if g.Resolve("apr_gone", AllowOnce) == nil {
		t.Fatal("resolving an unknown request must fail")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestAllowForRunIsScopedToTheRun(t *testing.T) {
	g, s, c := newGate(t, time.Minute)
	q := jobs.New(jobs.Options{Workers: 2})
	defer q.Close()

	// One run calls the write tool three times; one approval covers them all.
	runA, _ := q.Submit(jobs.Spec{Source: "mcp"}, func(ctx context.Context) (any, error) {
		for range 3 {
			if _, err := g.CallTool(ctx, "x", "write", nil); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	answer(t, g, AllowRun)
	if j, _ := q.Wait(context.Background(), runA.ID, 3*time.Second); j.State != jobs.StateSucceeded {
		t.Fatalf("run A: %s %s", j.State, j.Error)
	}
	if c.calls.Load() != 3 || s.Get("ext").Tools["write"] != "" {
		t.Fatal("allow-for-run should cover the run without changing the policy")
	}
	if ev := g.TakeEvents(runA.ID); len(ev) != 1 || ev[0].Outcome != "allowed-run" {
		t.Fatalf("events: %+v", ev)
	}

	// A different run asks again.
	runB, _ := q.Submit(jobs.Spec{}, func(ctx context.Context) (any, error) { return g.CallTool(ctx, "x", "write", nil) })
	answer(t, g, DenyOnce)
	if j, _ := q.Wait(context.Background(), runB.ID, 3*time.Second); j.State != jobs.StateFailed {
		t.Fatalf("run B should have asked and been denied, got %s", j.State)
	}
}

func TestStrictModeAsksForEverything(t *testing.T) {
	g, s, _ := newGate(t, time.Minute)
	must(t, s.SetMode("ext", ModeTrusted))
	must(t, s.SetTool("ext", "write", Deny))
	must(t, s.SetStrict(true))

	done := make(chan error, 1)
	go func() { _, err := g.CallTool(context.Background(), "x", "read", nil); done <- err }()
	answer(t, g, AllowOnce) // even a read-only tool of a trusted extension asks
	must(t, <-done)
	if _, err := g.CallTool(context.Background(), "x", "write", nil); !errors.Is(err, ErrDenied) {
		t.Fatal("blocked tools stay blocked in strict mode")
	}

	reopened, err := Open(s.path)
	must(t, err)
	if !reopened.Strict() {
		t.Fatal("strict mode should persist")
	}
}

func TestOnChangeReportsPending(t *testing.T) {
	g, _, _ := newGate(t, time.Minute)
	var seen atomic.Int32
	g.OnChange = func([]Approval) { seen.Add(1) }
	done := make(chan error, 1)
	go func() { _, err := g.CallTool(context.Background(), "x", "write", nil); done <- err }()
	answer(t, g, AllowOnce)
	must(t, <-done)
	deadline := time.Now().Add(time.Second)
	for seen.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if seen.Load() < 2 {
		t.Fatalf("expected a change when the request appeared and when it was answered, got %d", seen.Load())
	}
}
