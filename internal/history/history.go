// Package history records finished code-mode runs, from the desktop app and
// from external AIs, so the user can see what was done on their machine.
//
// Records are appended to a JSON Lines file readable only by the user (they
// can contain personal data read by extensions). The newest Max records are
// kept; the file is compacted once it holds twice that many lines.
package history

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"

	"nexora/internal/codemode"
	"nexora/internal/jobs"
	"nexora/internal/permissions"
)

const (
	maxCodeBytes   = 64 << 10
	maxResultBytes = 32 << 10
	maxLogLines    = 200
)

// Record is one finished run.
type Record struct {
	ID     string          `json:"id"`
	Source string          `json:"source"` // "app", "mcp" or "schedule"
	State  jobs.State      `json:"state"`
	Code   string          `json:"code"`
	Args   json.RawMessage `json:"args,omitempty"`
	// ScriptID and ScriptName identify the saved script that ran, if any.
	ScriptID   string          `json:"scriptId,omitempty"`
	ScriptName string          `json:"scriptName,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	Truncated  bool            `json:"truncated,omitempty"` // result was too large to keep
	Error      string          `json:"error,omitempty"`
	Logs       []string        `json:"logs,omitempty"`
	Tools      []string        `json:"tools,omitempty"` // "namespace.tool", first use order
	ToolCalls  int             `json:"toolCalls"`
	// Calls is the timeline of tool calls with their arguments and outcome.
	Calls []codemode.Call `json:"calls,omitempty"`
	// Approvals records permission prompts and blocks during the run.
	Approvals  []permissions.Event `json:"approvals,omitempty"`
	CreatedAt  time.Time           `json:"createdAt"`
	StartedAt  *time.Time          `json:"startedAt,omitempty"`
	FinishedAt *time.Time          `json:"finishedAt,omitempty"`
}

// FromJob converts a finished code-mode job into a record.
func FromJob(j jobs.Job) Record {
	r := Record{
		ID: j.ID, Source: j.Source, State: j.State, Error: j.Error,
		Code: truncate(j.Input, maxCodeBytes),
		Args: j.Args, ScriptID: j.ScriptID, ScriptName: j.ScriptName,
		CreatedAt: j.CreatedAt, StartedAt: j.StartedAt, FinishedAt: j.FinishedAt,
	}
	if res, ok := j.Result.(*codemode.Result); ok && res != nil {
		r.Tools, r.ToolCalls, r.Calls = res.Tools, res.ToolCalls, res.Calls
		r.Logs = res.Logs
		if len(r.Logs) > maxLogLines {
			r.Logs = append(slices.Clip(r.Logs[:maxLogLines]), fmt.Sprintf("… %d more lines", len(res.Logs)-maxLogLines))
		}
		if j.State == jobs.StateSucceeded {
			if raw, err := json.Marshal(res.Value); err == nil {
				if len(raw) > maxResultBytes {
					r.Truncated = true
				} else {
					r.Result = raw
				}
			}
		}
	}
	return r
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n// … truncated"
}

// Store is a bounded, persistent list of records.
type Store struct {
	path string
	max  int

	mu      sync.Mutex
	records []Record // oldest first
	lines   int      // lines in the file, for compaction
	f       *os.File
}

// Open loads path (creating it if needed) and keeps the newest max records.
func Open(path string, max int) (*Store, error) {
	if max <= 0 {
		max = 500
	}
	s := &Store{path: path, max: max}
	if err := s.load(); err != nil {
		return nil, err
	}
	if s.lines > s.max {
		if err := s.compactLocked(); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	s.f = f
	return s, nil
}

func (s *Store) load() error {
	f, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			s.lines++
			var rec Record
			if json.Unmarshal(line, &rec) == nil && rec.ID != "" {
				s.records = append(s.records, rec)
			}
		}
		if err != nil {
			break
		}
	}
	if len(s.records) > s.max {
		s.records = slices.Clone(s.records[len(s.records)-s.max:])
	}
	return nil
}

// Add appends a record.
func (s *Store) Add(r Record) error {
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, r)
	if len(s.records) > s.max {
		s.records = slices.Clone(s.records[len(s.records)-s.max:])
	}
	if s.f == nil {
		return errors.New("history store is closed")
	}
	if _, err := s.f.Write(append(line, '\n')); err != nil {
		return err
	}
	s.lines++
	if s.lines >= 2*s.max {
		return s.compactLocked()
	}
	return nil
}

// List returns up to limit records, newest first (limit <= 0: all).
func (s *Store) List(limit int) []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.records)
	if limit > 0 && limit < n {
		n = limit
	}
	out := make([]Record, 0, n)
	for i := len(s.records) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, s.records[i])
	}
	return out
}

func (s *Store) Get(id string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.records) - 1; i >= 0; i-- {
		if s.records[i].ID == id {
			return s.records[i], true
		}
	}
	return Record{}, false
}

// Clear deletes all history, in memory and on disk.
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = nil
	return s.compactLocked()
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

// compactLocked rewrites the file with only the retained records.
func (s *Store) compactLocked() error {
	var buf bytes.Buffer
	for _, r := range s.records {
		line, err := json.Marshal(r)
		if err != nil {
			return err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.lines = len(s.records)
	if s.f != nil {
		// The old handle points at the replaced file.
		_ = s.f.Close()
		f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			s.f = nil
			return err
		}
		s.f = f
	}
	return nil
}
