package services

import (
	"context"
	"time"

	"nexora/internal/jobs"
	"nexora/internal/runner"
)

// CodeModeService exposes code-mode execution and the job queue to the UI.
type CodeModeService struct {
	runner *runner.Runner
}

func NewCodeModeService(r *runner.Runner) *CodeModeService {
	return &CodeModeService{runner: r}
}

// Types returns the TypeScript declaration of `tools` for running extensions.
func (s *CodeModeService) Types() string {
	return s.runner.API()
}

// Run queues a script and waits for it to finish. The UI has no proxy in
// between, so it can wait for the full job runtime.
func (s *CodeModeService) Run(ctx context.Context, code string) (jobs.Job, error) {
	return s.runner.Run(ctx, code, runner.SourceApp, 24*time.Hour)
}

// Jobs lists recent jobs from every caller (UI and MCP), newest first.
func (s *CodeModeService) Jobs() []jobs.Job {
	return s.runner.Queue().List()
}

func (s *CodeModeService) CancelJob(id string) (jobs.Job, error) {
	return s.runner.Queue().Cancel(id)
}

func (s *CodeModeService) QueueStats() jobs.Stats {
	return s.runner.Queue().Stats()
}
