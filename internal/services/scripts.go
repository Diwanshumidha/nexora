package services

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"nexora/internal/jobs"
	"nexora/internal/runner"
	"nexora/internal/scripts"
)

// ScriptsService exposes saved scripts and their schedules to the UI.
type ScriptsService struct {
	mgr *scripts.Manager
}

func NewScriptsService(m *scripts.Manager) *ScriptsService {
	return &ScriptsService{mgr: m}
}

func (s *ScriptsService) List() []scripts.Script {
	return s.mgr.List()
}

func (s *ScriptsService) Get(id string) (scripts.Script, error) {
	return s.mgr.Get(id)
}

// ScriptInput is what the editor changes; creation details and run state
// are kept by the store.
type ScriptInput struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	Code         string            `json:"code"`
	Params       []scripts.Param   `json:"params"`
	Schedule     *scripts.Schedule `json:"schedule"`
	ScheduleArgs json.RawMessage   `json:"scheduleArgs"`
}

// Save creates (empty ID) or updates a script.
func (s *ScriptsService) Save(in ScriptInput) (scripts.Script, error) {
	return s.mgr.Save(scripts.Script{
		ID: in.ID, Name: in.Name, Description: in.Description, Code: in.Code,
		Params: in.Params, Schedule: in.Schedule, ScheduleArgs: in.ScheduleArgs,
	})
}

func (s *ScriptsService) Delete(id string) error {
	return s.mgr.Delete(id)
}

// SetScheduleEnabled pauses or resumes a script's schedule.
func (s *ScriptsService) SetScheduleEnabled(id string, enabled bool) (scripts.Script, error) {
	sc, err := s.mgr.Get(id)
	if err != nil {
		return scripts.Script{}, err
	}
	if sc.Schedule == nil {
		return scripts.Script{}, errors.New("this script has no schedule")
	}
	sc.Schedule.Enabled = enabled
	return s.mgr.Save(sc)
}

// Run starts a script from the app. argsJSON is a JSON object (or empty)
// merged over the parameters' defaults. The run shows up in History.
func (s *ScriptsService) Run(id, argsJSON string) (jobs.Job, error) {
	var args json.RawMessage
	if strings.TrimSpace(argsJSON) != "" {
		args = json.RawMessage(argsJSON)
	}
	return s.mgr.Run(id, args, runner.SourceApp)
}

// PreviewSchedule checks a schedule and returns its next n run times
// (RFC 3339), for the editor to show before saving.
func (s *ScriptsService) PreviewSchedule(sch scripts.Schedule, n int) ([]string, error) {
	sch.Enabled = true
	probe := scripts.Script{Name: "preview", Code: "preview", Schedule: &sch}
	probe, err := probe.Validate()
	if err != nil {
		return nil, err
	}
	n = min(max(n, 1), 10)
	out := make([]string, 0, n)
	t := time.Now()
	for range n {
		t = probe.Schedule.Next(t)
		out = append(out, t.Format(time.RFC3339))
	}
	return out, nil
}
