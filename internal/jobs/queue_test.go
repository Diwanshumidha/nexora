package jobs

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestInlineVersusPolling(t *testing.T) {
	q := New(Options{Workers: 1})
	defer q.Close()

	fast, _ := q.Submit(Spec{Label: "fast"}, func(context.Context) (any, error) { return 42, nil })
	j, err := q.Wait(context.Background(), fast.ID, time.Second)
	if err != nil || j.State != StateSucceeded || j.Result != 42 {
		t.Fatalf("fast job: %+v %v", j, err)
	}

	release := make(chan struct{})
	slow, _ := q.Submit(Spec{Label: "slow"}, func(context.Context) (any, error) { <-release; return "done", nil })
	j, _ = q.Wait(context.Background(), slow.ID, 50*time.Millisecond)
	if j.State != StateRunning {
		t.Fatalf("expected running after inline wait, got %s", j.State)
	}
	close(release)
	j, _ = q.Wait(context.Background(), slow.ID, time.Second)
	if j.State != StateSucceeded || j.Result != "done" {
		t.Fatalf("slow job: %+v", j)
	}
}

func TestFIFOAndWorkerLimit(t *testing.T) {
	q := New(Options{Workers: 2})
	defer q.Close()

	var running, peak atomic.Int32
	var mu sync.Mutex
	var order []int
	release := make(chan struct{})
	var ids []string
	for i := range 6 {
		j, err := q.Submit(Spec{Label: ""}, func(context.Context) (any, error) {
			n := running.Add(1)
			defer running.Add(-1)
			for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
			}
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			<-release
			return nil, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, j.ID)
	}

	waitUntil(t, func() bool { return q.Stats().Running == 2 })
	if j, _ := q.Get(ids[4]); j.State != StateQueued || j.Position != 3 {
		t.Fatalf("job 5 should be queued at position 3, got %s/%d", j.State, j.Position)
	}
	close(release)
	for _, id := range ids {
		if j, _ := q.Wait(context.Background(), id, time.Second); j.State != StateSucceeded {
			t.Fatalf("job %s: %s", id, j.State)
		}
	}
	if peak.Load() != 2 {
		t.Fatalf("peak concurrency %d, want 2", peak.Load())
	}
	// Two workers race for the first pair, but start order is otherwise FIFO.
	for i := 2; i < len(order); i++ {
		if order[i] < order[i-2] {
			t.Fatalf("not FIFO: %v", order)
		}
	}
}

func TestQueueFull(t *testing.T) {
	q := New(Options{Workers: 1, MaxQueued: 1})
	defer q.Close()
	block := func(ctx context.Context) (any, error) { <-ctx.Done(); return nil, ctx.Err() }
	q.Submit(Spec{Label: ""}, block)
	waitUntil(t, func() bool { return q.Stats().Running == 1 })
	if _, err := q.Submit(Spec{Label: ""}, block); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Submit(Spec{Label: ""}, block); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("expected ErrQueueFull, got %v", err)
	}
}

func TestCancelQueuedAndRunning(t *testing.T) {
	q := New(Options{Workers: 1})
	defer q.Close()
	block := func(ctx context.Context) (any, error) { <-ctx.Done(); return nil, ctx.Err() }
	running, _ := q.Submit(Spec{Label: ""}, block)
	queued, _ := q.Submit(Spec{Label: ""}, block)
	waitUntil(t, func() bool { j, _ := q.Get(running.ID); return j.State == StateRunning })

	if j, _ := q.Cancel(queued.ID); j.State != StateCancelled {
		t.Fatalf("queued job: %s", j.State)
	}
	q.Cancel(running.ID)
	if j, _ := q.Wait(context.Background(), running.ID, time.Second); j.State != StateCancelled {
		t.Fatalf("running job: %s", j.State)
	}
}

func TestMaxRuntimeAndPanic(t *testing.T) {
	q := New(Options{Workers: 2, MaxRuntime: 50 * time.Millisecond})
	defer q.Close()
	slow, _ := q.Submit(Spec{Label: ""}, func(ctx context.Context) (any, error) { <-ctx.Done(); return nil, ctx.Err() })
	bad, _ := q.Submit(Spec{Label: ""}, func(context.Context) (any, error) { panic("kaboom") })

	if j, _ := q.Wait(context.Background(), slow.ID, time.Second); j.State != StateFailed {
		t.Fatalf("slow job: %+v", j)
	}
	if j, _ := q.Wait(context.Background(), bad.ID, time.Second); j.State != StateFailed {
		t.Fatalf("panicking job: %+v", j)
	}
	// Workers survive a panic.
	ok, _ := q.Submit(Spec{Label: ""}, func(context.Context) (any, error) { return 1, nil })
	if j, _ := q.Wait(context.Background(), ok.ID, time.Second); j.State != StateSucceeded {
		t.Fatalf("follow-up job: %+v", j)
	}
}

func TestUnknownJob(t *testing.T) {
	q := New(Options{})
	defer q.Close()
	if _, err := q.Wait(context.Background(), "job_nope", time.Millisecond); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestOnFinishAndSpec(t *testing.T) {
	var mu sync.Mutex
	var finished []Job
	q := New(Options{Workers: 1, OnFinish: func(j Job) {
		mu.Lock()
		finished = append(finished, j)
		mu.Unlock()
	}})
	defer q.Close()

	j, _ := q.Submit(Spec{Label: "run", Source: "mcp", Input: "return 1"}, func(context.Context) (any, error) { return 1, nil })
	if j.Source != "mcp" || j.Input != "return 1" {
		t.Fatalf("spec not recorded: %+v", j)
	}
	block := func(ctx context.Context) (any, error) { <-ctx.Done(); return nil, ctx.Err() }
	running, _ := q.Submit(Spec{}, block)
	queued, _ := q.Submit(Spec{}, block)
	q.Cancel(queued.ID)
	q.Cancel(running.ID)

	waitUntil(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(finished) == 3 })
	mu.Lock()
	defer mu.Unlock()
	states := map[State]int{}
	for _, f := range finished {
		states[f.State]++
	}
	if states[StateSucceeded] != 1 || states[StateCancelled] != 2 {
		t.Fatalf("unexpected finish notifications: %v", states)
	}
}

func TestTaskSeesJobInfo(t *testing.T) {
	q := New(Options{Workers: 1})
	defer q.Close()
	j, _ := q.Submit(Spec{Source: "mcp"}, func(ctx context.Context) (any, error) {
		info, ok := FromContext(ctx)
		if !ok {
			return nil, errors.New("no job info in context")
		}
		return info, nil
	})
	got, _ := q.Wait(context.Background(), j.ID, time.Second)
	if info, _ := got.Result.(Info); info.ID != j.ID || info.Source != "mcp" {
		t.Fatalf("got %+v (%s)", got.Result, got.Error)
	}
}
