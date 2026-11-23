package history

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nexora/internal/codemode"
	"nexora/internal/jobs"
)

func rec(id string) Record {
	return Record{ID: id, Source: "app", State: jobs.StateSucceeded, CreatedAt: time.Now()}
}

func TestPersistOrderAndLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	s, err := Open(path, 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c", "d"} {
		if err := s.Add(rec(id)); err != nil {
			t.Fatal(err)
		}
	}
	got := s.List(0)
	if len(got) != 3 || got[0].ID != "d" || got[2].ID != "b" {
		t.Fatalf("want newest 3 (d,c,b), got %v", ids(got))
	}
	if l := s.List(1); len(l) != 1 || l[0].ID != "d" {
		t.Fatalf("limit: %v", ids(l))
	}
	s.Close()

	s2, err := Open(path, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got := s2.List(0); len(got) != 3 || got[0].ID != "d" {
		t.Fatalf("reload: %v", ids(got))
	}
	if _, ok := s2.Get("c"); !ok {
		t.Fatal("get")
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("history must be private, got %v", info.Mode().Perm())
	}
}

func TestCompaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	s, _ := Open(path, 5)
	defer s.Close()
	for i := range 23 {
		s.Add(rec(string(rune('a' + i))))
	}
	raw, _ := os.ReadFile(path)
	if lines := strings.Count(string(raw), "\n"); lines >= 10 {
		t.Fatalf("file should be compacted below 2*max lines, has %d", lines)
	}
	if got := s.List(0); len(got) != 5 || got[0].ID != "w" {
		t.Fatalf("got %v", ids(got))
	}
	// Appends keep working after compaction swapped the file.
	s.Add(rec("z"))
	s.Close()
	s2, _ := Open(path, 5)
	defer s2.Close()
	if got := s2.List(1); got[0].ID != "z" {
		t.Fatalf("got %v", ids(got))
	}
}

func TestClear(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	s, _ := Open(path, 5)
	s.Add(rec("a"))
	if err := s.Clear(); err != nil {
		t.Fatal(err)
	}
	s.Add(rec("b"))
	s.Close()
	s2, _ := Open(path, 5)
	defer s2.Close()
	if got := s2.List(0); len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("got %v", ids(got))
	}
}

func TestFromJob(t *testing.T) {
	now := time.Now()
	r := FromJob(jobs.Job{
		ID: "job_1", Source: "mcp", State: jobs.StateSucceeded, Input: "return 1", CreatedAt: now, FinishedAt: &now,
		Result: &codemode.Result{Value: map[string]any{"n": 1}, Logs: []string{"log: hi"}, ToolCalls: 2, Tools: []string{"excel.readSheet"},
			Calls: []codemode.Call{{Tool: "excel.readSheet", Status: "ok"}}},
	})
	if len(r.Calls) != 1 || r.Calls[0].Tool != "excel.readSheet" {
		t.Fatalf("calls not kept: %+v", r.Calls)
	}
	if r.Source != "mcp" || r.Code != "return 1" || string(r.Result) != `{"n":1}` || r.Tools[0] != "excel.readSheet" || r.ToolCalls != 2 {
		t.Fatalf("got %+v", r)
	}

	big := FromJob(jobs.Job{ID: "job_2", State: jobs.StateSucceeded,
		Result: &codemode.Result{Value: strings.Repeat("x", maxResultBytes+1)}})
	if !big.Truncated || big.Result != nil {
		t.Fatal("large results should be dropped and flagged")
	}

	failed := FromJob(jobs.Job{ID: "job_3", State: jobs.StateFailed, Error: "boom", Result: &codemode.Result{Value: 1}})
	if failed.Result != nil || failed.Error != "boom" {
		t.Fatalf("failed runs keep the error, not a value: %+v", failed)
	}
}

func ids(rs []Record) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}
