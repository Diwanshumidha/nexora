// Package jobs is a bounded FIFO execution queue with a fixed worker pool.
//
// Every code-mode run goes through it, whether it comes from the UI or from
// an external AI over MCP, so concurrent requests are executed fairly and
// the machine is never oversubscribed. Callers can wait for a job inline
// for a bounded time and fall back to polling by job ID. This keeps
// requests under proxy timeouts such as Cloudflare's 100 s limit.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime/debug"
	"sort"
	"sync"
	"time"
)

type State string

const (
	StateQueued    State = "queued"
	StateRunning   State = "running"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"
)

// Finished reports whether the job has reached a terminal state.
func (s State) Finished() bool {
	return s == StateSucceeded || s == StateFailed || s == StateCancelled
}

var (
	ErrQueueFull = errors.New("job queue is full, try again shortly")
	ErrNotFound  = errors.New("job not found (it may have expired)")
	ErrClosed    = errors.New("job queue is shut down")
)

// Info identifies the job a task is running for. Tasks can read it from
// their context with FromContext.
type Info struct {
	ID     string
	Source string
}

type ctxKey struct{}

// FromContext returns the job a task's context belongs to.
func FromContext(ctx context.Context) (Info, bool) {
	info, ok := ctx.Value(ctxKey{}).(Info)
	return info, ok
}

// Task is the work a job performs. It must honour ctx cancellation.
type Task func(ctx context.Context) (any, error)

// Spec describes a job being submitted.
type Spec struct {
	Label string
	// Source identifies the caller, e.g. "app" or "mcp".
	Source string
	// Input is the job's input (e.g. the script), kept for display/history.
	Input string
}

// Job is an immutable snapshot of a job's state.
type Job struct {
	ID         string     `json:"id"`
	Label      string     `json:"label,omitempty"`
	Source     string     `json:"source,omitempty"`
	Input      string     `json:"input,omitempty"`
	State      State      `json:"state"`
	Position   int        `json:"position,omitempty"` // 1-based place in the queue while queued
	Result     any        `json:"result,omitempty"`
	Error      string     `json:"error,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
}

type Options struct {
	// Workers is the number of jobs executed concurrently.
	Workers int
	// MaxQueued caps jobs waiting to start; Submit fails beyond it.
	MaxQueued int
	// MaxRuntime bounds a single job's execution time.
	MaxRuntime time.Duration
	// Retention is how long finished jobs stay retrievable.
	Retention time.Duration
	// OnFinish, if set, is called (outside any lock) once per job that
	// reaches a terminal state.
	OnFinish func(Job)
}

type job struct {
	snap   Job
	task   Task
	cancel context.CancelFunc // set while running
	done   chan struct{}      // closed on reaching a terminal state
}

type Queue struct {
	opts Options

	mu      sync.Mutex
	cond    *sync.Cond
	jobs    map[string]*job
	waiting []*job // FIFO of queued jobs
	closed  bool

	workers sync.WaitGroup
	stopGC  chan struct{}
}

func New(opts Options) *Queue {
	if opts.Workers <= 0 {
		opts.Workers = 4
	}
	if opts.MaxQueued <= 0 {
		opts.MaxQueued = 100
	}
	if opts.MaxRuntime <= 0 {
		opts.MaxRuntime = 15 * time.Minute
	}
	if opts.Retention <= 0 {
		opts.Retention = time.Hour
	}
	q := &Queue{opts: opts, jobs: map[string]*job{}, stopGC: make(chan struct{})}
	q.cond = sync.NewCond(&q.mu)
	for range opts.Workers {
		q.workers.Go(q.worker)
	}
	go q.gc()
	return q
}

// Submit enqueues a task and returns its initial snapshot.
func (q *Queue) Submit(spec Spec, task Task) (Job, error) {
	j := &job{
		snap: Job{
			ID: newID(), Label: spec.Label, Source: spec.Source, Input: spec.Input,
			State: StateQueued, CreatedAt: time.Now(),
		},
		task: task,
		done: make(chan struct{}),
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return Job{}, ErrClosed
	}
	if len(q.waiting) >= q.opts.MaxQueued {
		return Job{}, ErrQueueFull
	}
	q.jobs[j.snap.ID] = j
	q.waiting = append(q.waiting, j)
	q.cond.Signal()
	return q.snapshotLocked(j), nil
}

// Get returns the current snapshot of a job.
func (q *Queue) Get(id string) (Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	j, ok := q.jobs[id]
	if !ok {
		return Job{}, ErrNotFound
	}
	return q.snapshotLocked(j), nil
}

// Wait blocks until the job finishes, maxWait elapses or ctx is done, then
// returns the job's snapshot at that moment. Not finishing within maxWait is
// not an error: check Job.State.
func (q *Queue) Wait(ctx context.Context, id string, maxWait time.Duration) (Job, error) {
	q.mu.Lock()
	j, ok := q.jobs[id]
	q.mu.Unlock()
	if !ok {
		return Job{}, ErrNotFound
	}
	timer := time.NewTimer(maxWait)
	defer timer.Stop()
	select {
	case <-j.done:
	case <-timer.C:
	case <-ctx.Done():
		return Job{}, ctx.Err()
	}
	return q.Get(id)
}

// Cancel stops a queued or running job. Cancelling a finished job is a no-op.
func (q *Queue) Cancel(id string) (Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	j, ok := q.jobs[id]
	if !ok {
		return Job{}, ErrNotFound
	}
	switch j.snap.State {
	case StateQueued:
		q.removeWaitingLocked(j)
		q.finishLocked(j, nil, context.Canceled, StateCancelled)
		q.notify(j.snap)
	case StateRunning:
		j.cancel() // the worker records the cancelled state
	}
	return q.snapshotLocked(j), nil
}

// notify runs OnFinish without blocking the caller, which may hold q.mu.
func (q *Queue) notify(j Job) {
	if q.opts.OnFinish != nil {
		go q.opts.OnFinish(j)
	}
}

// List returns all retained jobs, newest first.
func (q *Queue) List() []Job {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]Job, 0, len(q.jobs))
	for _, j := range q.jobs {
		out = append(out, q.snapshotLocked(j))
	}
	sort.Slice(out, func(a, b int) bool { return out[a].CreatedAt.After(out[b].CreatedAt) })
	return out
}

// Stats is a point-in-time summary of the queue.
type Stats struct {
	Workers int `json:"workers"`
	Running int `json:"running"`
	Queued  int `json:"queued"`
}

func (q *Queue) Stats() Stats {
	q.mu.Lock()
	defer q.mu.Unlock()
	s := Stats{Workers: q.opts.Workers, Queued: len(q.waiting)}
	for _, j := range q.jobs {
		if j.snap.State == StateRunning {
			s.Running++
		}
	}
	return s
}

// Close cancels all queued and running jobs and stops the workers.
func (q *Queue) Close() {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.closed = true
	for _, j := range q.waiting {
		q.finishLocked(j, nil, ErrClosed, StateCancelled)
		q.notify(j.snap)
	}
	q.waiting = nil
	for _, j := range q.jobs {
		if j.snap.State == StateRunning {
			j.cancel()
		}
	}
	q.cond.Broadcast()
	q.mu.Unlock()

	close(q.stopGC)
	q.workers.Wait()
}

func (q *Queue) worker() {
	for {
		q.mu.Lock()
		for len(q.waiting) == 0 && !q.closed {
			q.cond.Wait()
		}
		if q.closed {
			q.mu.Unlock()
			return
		}
		j := q.waiting[0]
		q.waiting[0] = nil
		q.waiting = q.waiting[1:]

		ctx, cancel := context.WithTimeout(
			context.WithValue(context.Background(), ctxKey{}, Info{ID: j.snap.ID, Source: j.snap.Source}),
			q.opts.MaxRuntime)
		now := time.Now()
		j.cancel = cancel
		j.snap.State = StateRunning
		j.snap.StartedAt = &now
		q.mu.Unlock()

		result, err := runTask(ctx, j.task)

		q.mu.Lock()
		state := StateSucceeded
		switch {
		case err == nil:
		case errors.Is(ctx.Err(), context.Canceled):
			state, err = StateCancelled, context.Canceled
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			state, err = StateFailed, fmt.Errorf("job exceeded max runtime of %s", q.opts.MaxRuntime)
		default:
			state = StateFailed
		}
		q.finishLocked(j, result, err, state)
		snap := j.snap
		q.mu.Unlock()
		cancel()
		if q.opts.OnFinish != nil {
			q.opts.OnFinish(snap)
		}
	}
}

func runTask(ctx context.Context, task Task) (result any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("job panicked: %v\n%s", r, debug.Stack())
		}
	}()
	return task(ctx)
}

func (q *Queue) finishLocked(j *job, result any, err error, state State) {
	now := time.Now()
	j.snap.State = state
	j.snap.Result = result
	if err != nil {
		j.snap.Error = err.Error()
	}
	j.snap.FinishedAt = &now
	j.task = nil
	close(j.done)
}

func (q *Queue) removeWaitingLocked(target *job) {
	for i, j := range q.waiting {
		if j == target {
			q.waiting = append(q.waiting[:i], q.waiting[i+1:]...)
			return
		}
	}
}

func (q *Queue) snapshotLocked(j *job) Job {
	s := j.snap
	if s.State == StateQueued {
		for i, w := range q.waiting {
			if w == j {
				s.Position = i + 1
				break
			}
		}
	}
	return s
}

// gc drops finished jobs once they pass the retention window.
func (q *Queue) gc() {
	interval := min(q.opts.Retention/2, time.Minute)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-q.stopGC:
			return
		case now := <-t.C:
			q.mu.Lock()
			for id, j := range q.jobs {
				if j.snap.FinishedAt != nil && now.Sub(*j.snap.FinishedAt) > q.opts.Retention {
					delete(q.jobs, id)
				}
			}
			q.mu.Unlock()
		}
	}
}

// newID returns an unguessable job ID: a job ID is the only credential
// needed to read a job's result.
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "job_" + hex.EncodeToString(b[:])
}
