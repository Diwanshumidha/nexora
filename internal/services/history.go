package services

import (
	"strings"
	"time"

	"nexora/internal/history"
	"nexora/internal/jobs"
	"nexora/internal/runner"
)

// HistoryService exposes recorded runs (plus ones still queued or running)
// to the UI.
type HistoryService struct {
	store  *history.Store
	runner *runner.Runner
}

func NewHistoryService(store *history.Store, r *runner.Runner) *HistoryService {
	return &HistoryService{store: store, runner: r}
}

// List returns live jobs first, then up to limit finished runs, newest first.
func (s *HistoryService) List(limit int) []history.Record {
	var out []history.Record
	seen := map[string]bool{}
	for _, j := range s.runner.Queue().List() {
		if !j.State.Finished() {
			out = append(out, s.live(j))
			seen[j.ID] = true
		}
	}
	for _, r := range s.store.List(limit) {
		if !seen[r.ID] {
			out = append(out, r)
		}
	}
	return out
}

// Summary is the light view of a run used by lists and the orbit: no
// script, result, logs or call arguments.
type Summary struct {
	ID         string     `json:"id"`
	Source     string     `json:"source"`
	State      jobs.State `json:"state"`
	Tools      []string   `json:"tools"`
	ToolCalls  int        `json:"toolCalls"`
	Error      string     `json:"error,omitempty"`
	Title      string     `json:"title"` // first meaningful line of the script
	CreatedAt  string     `json:"createdAt"`
	StartedAt  string     `json:"startedAt,omitempty"`
	FinishedAt string     `json:"finishedAt,omitempty"`
}

// Summaries is List without the heavy fields, for frequent polling.
func (s *HistoryService) Summaries(limit int) []Summary {
	recs := s.List(limit)
	out := make([]Summary, len(recs))
	for i, r := range recs {
		out[i] = Summary{
			ID: r.ID, Source: r.Source, State: r.State, Tools: r.Tools, ToolCalls: r.ToolCalls,
			Error: firstLine(r.Error), Title: scriptTitle(r.Code), CreatedAt: stamp(&r.CreatedAt),
			StartedAt: stamp(r.StartedAt), FinishedAt: stamp(r.FinishedAt),
		}
		if out[i].Tools == nil {
			out[i].Tools = []string{}
		}
	}
	return out
}

func stamp(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339Nano)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	if len(line) > 200 {
		line = line[:200] + "…"
	}
	return line
}

func scriptTitle(code string) string {
	for _, l := range strings.Split(code, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "//") {
			return firstLine(l)
		}
	}
	return "(empty script)"
}

// Get returns a run, live or recorded.
func (s *HistoryService) Get(id string) (*history.Record, error) {
	if j, err := s.runner.Queue().Get(id); err == nil && !j.State.Finished() {
		r := s.live(j)
		return &r, nil
	}
	if r, ok := s.store.Get(id); ok {
		return &r, nil
	}
	return nil, jobs.ErrNotFound
}

// live builds a record for a job still queued or running.
func (s *HistoryService) live(j jobs.Job) history.Record {
	r := history.FromJob(j)
	r.Approvals = s.runner.Gate().Events(j.ID)
	return r
}

// Rerun submits a recorded run's script again from the app.
func (s *HistoryService) Rerun(id string) (jobs.Job, error) {
	r, err := s.Get(id)
	if err != nil {
		return jobs.Job{}, err
	}
	return s.runner.Submit(r.Code, runner.SourceApp)
}

func (s *HistoryService) Cancel(id string) (jobs.Job, error) {
	return s.runner.Queue().Cancel(id)
}

// Clear deletes all recorded history.
func (s *HistoryService) Clear() error {
	return s.store.Clear()
}
