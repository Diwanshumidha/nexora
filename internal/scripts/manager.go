package scripts

import (
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nexora/internal/jobs"
)

// Sources of a saved script's runs, besides the runner's "app" and "mcp".
const SourceSchedule = "schedule"

// tick is how often the scheduler checks for due scripts. It compares wall
// clock times instead of sleeping until the next run, so a run due while
// the computer slept starts soon after it wakes.
const tick = 15 * time.Second

// Manager runs saved scripts, by hand or on their schedules.
type Manager struct {
	*Store
	submit func(jobs.Spec) (jobs.Job, error)
	job    func(id string) (jobs.Job, error)
	log    *slog.Logger

	kick      chan struct{}
	stop      chan struct{}
	done      chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once
	started   atomic.Bool
}

// NewManager runs scripts through submit (the runner) and looks jobs up
// with job (the queue), to avoid overlapping scheduled runs.
func NewManager(store *Store, submit func(jobs.Spec) (jobs.Job, error), job func(string) (jobs.Job, error), log *slog.Logger) *Manager {
	return &Manager{
		Store: store, submit: submit, job: job, log: log,
		kick: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}),
	}
}

// Run starts a script with args (a JSON object, merged over the defaults).
func (m *Manager) Run(idOrName string, args json.RawMessage, source string) (jobs.Job, error) {
	sc, err := m.Get(idOrName)
	if err != nil {
		return jobs.Job{}, err
	}
	resolved, err := sc.ResolveArgs(args)
	if err != nil {
		return jobs.Job{}, err
	}
	job, err := m.submit(jobs.Spec{
		Label: "script", Source: source, Input: sc.Code, Args: resolved,
		ScriptID: sc.ID, ScriptName: sc.Name,
	})
	if err != nil {
		return jobs.Job{}, err
	}
	_ = m.update(sc.ID, func(s *Script) {
		s.LastRun = &LastRun{JobID: job.ID, Source: source, State: job.State, At: job.CreatedAt}
	})
	return job, nil
}

// Finished records the outcome of a job; jobs that didn't run a saved
// script are ignored. Hook it to the queue's OnFinish.
func (m *Manager) Finished(j jobs.Job) {
	if j.ScriptID == "" {
		return
	}
	_ = m.update(j.ScriptID, func(s *Script) {
		if s.LastRun != nil && s.LastRun.JobID != j.ID {
			return // a newer run has started since
		}
		s.LastRun = &LastRun{JobID: j.ID, Source: j.Source, State: j.State, Error: firstLine(j.Error), At: j.CreatedAt}
	})
}

// Save stores a script from the app, which alone may schedule scripts.
func (m *Manager) Save(in Script) (Script, error) {
	sc, err := m.Store.Save(in, "app")
	if err == nil {
		m.wake()
	}
	return sc, err
}

// Start begins checking schedules. Runs missed while Nexora was closed
// either run once (CatchUp) or are skipped. The first check waits delay,
// so extensions have started before a caught-up run needs them.
func (m *Manager) Start(delay time.Duration) {
	m.startOnce.Do(func() {
		now := m.now()
		for _, sc := range m.List() {
			if sc.Scheduled() && sc.NextRun != nil && sc.NextRun.Before(now) && !sc.Schedule.CatchUp {
				_ = m.update(sc.ID, func(s *Script) { s.NextRun = nextAfter(s, now) })
			}
		}
		m.started.Store(true)
		go m.loop(delay)
	})
}

// Close stops the scheduler. Runs already started are left to the queue.
func (m *Manager) Close() {
	m.stopOnce.Do(func() { close(m.stop) })
	if m.started.Load() {
		<-m.done
	}
}

func (m *Manager) wake() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

func (m *Manager) loop(delay time.Duration) {
	defer close(m.done)
	select {
	case <-time.After(delay):
	case <-m.stop:
		return
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		m.fireDue(m.now())
		select {
		case <-t.C:
		case <-m.kick:
		case <-m.stop:
			return
		}
	}
}

// fireDue starts every scheduled script whose next run has come.
func (m *Manager) fireDue(now time.Time) {
	for _, sc := range m.List() {
		if !sc.Scheduled() || sc.NextRun == nil || sc.NextRun.After(now) {
			continue
		}
		if m.busy(sc) {
			m.log.Info("scheduled script skipped: its previous run is still going", "script", sc.Name)
		} else if _, err := m.Run(sc.ID, sc.ScheduleArgs, SourceSchedule); err != nil {
			m.log.Warn("scheduled script did not start", "script", sc.Name, "err", err)
			_ = m.update(sc.ID, func(s *Script) {
				s.LastRun = &LastRun{Source: SourceSchedule, State: jobs.StateFailed, Error: "did not start: " + err.Error(), At: now}
			})
		}
		_ = m.update(sc.ID, func(s *Script) { s.NextRun = nextAfter(s, now) })
	}
}

// busy reports whether the script's last run is still queued or running.
func (m *Manager) busy(sc Script) bool {
	if sc.LastRun == nil || sc.LastRun.JobID == "" || sc.LastRun.State.Finished() {
		return false
	}
	j, err := m.job(sc.LastRun.JobID)
	if errors.Is(err, jobs.ErrNotFound) || err != nil {
		return false
	}
	return !j.State.Finished()
}

func nextAfter(s *Script, now time.Time) *time.Time {
	if !s.Scheduled() {
		return nil
	}
	next := s.Schedule.Next(now)
	return &next
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	if len(line) > 300 {
		line = line[:300] + "…"
	}
	return line
}
