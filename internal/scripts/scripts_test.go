package scripts

import (
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"nexora/internal/jobs"
)

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
	if err != nil {
		panic(err)
	}
	return t
}

func TestScheduleNext(t *testing.T) {
	cases := []struct {
		name string
		s    Schedule
		from string
		want string
	}{
		{"every", Schedule{Kind: KindEvery, Every: "30m"}, "2026-11-27 10:00", "2026-11-27 10:30"},
		{"daily later today", Schedule{Kind: KindDaily, At: "18:00"}, "2026-11-27 10:00", "2026-11-27 18:00"},
		{"daily tomorrow", Schedule{Kind: KindDaily, At: "09:00"}, "2026-11-27 10:00", "2026-11-28 09:00"},
		{"daily exactly now is next day", Schedule{Kind: KindDaily, At: "10:00"}, "2026-11-27 10:00", "2026-11-28 10:00"},
		// 2026-11-27 is a Friday.
		{"weekly monday", Schedule{Kind: KindWeekly, At: "09:00", Days: []int{1}}, "2026-11-27 10:00", "2026-11-30 09:00"},
		{"weekly same day later", Schedule{Kind: KindWeekly, At: "12:00", Days: []int{5}}, "2026-11-27 10:00", "2026-11-27 12:00"},
		{"weekly same day passed", Schedule{Kind: KindWeekly, At: "09:00", Days: []int{5}}, "2026-11-27 10:00", "2026-12-04 09:00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.s.validate(); err != nil {
				t.Fatal(err)
			}
			if got := c.s.Next(at(c.from)); !got.Equal(at(c.want)) {
				t.Fatalf("Next(%s) = %s, want %s", c.from, got, c.want)
			}
		})
	}
}

func TestScheduleValidate(t *testing.T) {
	bad := []Schedule{
		{Kind: "hourly"},
		{Kind: KindEvery, Every: "10s"},
		{Kind: KindEvery, Every: "soon"},
		{Kind: KindDaily, At: "25:00"},
		{Kind: KindDaily, At: "9:00"},
		{Kind: KindWeekly, At: "09:00"},
		{Kind: KindWeekly, At: "09:00", Days: []int{7}},
	}
	for _, s := range bad {
		if err := s.validate(); err == nil {
			t.Errorf("%+v: expected an error", s)
		}
	}
}

func TestResolveArgs(t *testing.T) {
	sc := Script{Params: []Param{
		{Name: "path", Type: TypeString, Required: true},
		{Name: "limit", Type: TypeNumber, Default: json.RawMessage("10")},
		{Name: "dry", Type: TypeBoolean},
	}}
	got, err := sc.ResolveArgs(json.RawMessage(`{"path":"/tmp/a"}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"limit":10,"path":"/tmp/a"}` {
		t.Fatalf("got %s", got)
	}
	for _, in := range []string{`{}`, `{"path":1}`, `{"path":"x","other":1}`, `[1]`} {
		if _, err := sc.ResolveArgs(json.RawMessage(in)); err == nil {
			t.Errorf("%s: expected an error", in)
		}
	}
}

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "scripts.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStoreSaveAndReload(t *testing.T) {
	s := newStore(t)
	sc, err := s.Save(Script{Name: "Daily digest", Code: "return 1"}, "app")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sc.ID, "scr_") || sc.CreatedBy != "app" {
		t.Fatalf("unexpected %+v", sc)
	}
	if _, err := s.Save(Script{Name: "daily DIGEST", Code: "return 2"}, "mcp"); err == nil {
		t.Fatal("duplicate name accepted")
	}
	sc.Code = "return 3"
	sc.CreatedBy = "mcp" // ignored: kept from the stored script
	if _, err := s.Save(sc, "mcp"); err != nil {
		t.Fatal(err)
	}

	again, err := OpenStore(s.path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := again.Get("daily digest")
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != "return 3" || got.CreatedBy != "app" {
		t.Fatalf("reloaded %+v", got)
	}
	if err := again.Delete(got.ID); err != nil {
		t.Fatal(err)
	}
	if len(again.List()) != 0 {
		t.Fatal("not deleted")
	}
}

func TestScheduledScriptNeedsArgs(t *testing.T) {
	s := newStore(t)
	_, err := s.Save(Script{
		Name: "x", Code: "return args.path",
		Params:   []Param{{Name: "path", Type: TypeString, Required: true}},
		Schedule: &Schedule{Enabled: true, Kind: KindDaily, At: "09:00"},
	}, "app")
	if err == nil {
		t.Fatal("schedule without its required argument accepted")
	}
}

// fakeQueue records submitted specs and lets tests finish jobs.
type fakeQueue struct {
	mu    sync.Mutex
	specs []jobs.Spec
	jobs  map[string]jobs.Job
}

func (q *fakeQueue) submit(spec jobs.Spec) (jobs.Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.jobs == nil {
		q.jobs = map[string]jobs.Job{}
	}
	q.specs = append(q.specs, spec)
	j := jobs.Job{ID: "job_" + string(rune('a'+len(q.specs))), State: jobs.StateQueued, ScriptID: spec.ScriptID, CreatedAt: time.Now()}
	q.jobs[j.ID] = j
	return j, nil
}

func (q *fakeQueue) get(id string) (jobs.Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	j, ok := q.jobs[id]
	if !ok {
		return jobs.Job{}, jobs.ErrNotFound
	}
	return j, nil
}

func (q *fakeQueue) finish(id string) jobs.Job {
	q.mu.Lock()
	defer q.mu.Unlock()
	j := q.jobs[id]
	j.State = jobs.StateSucceeded
	q.jobs[id] = j
	return j
}

func TestSchedulerFiresOnceAndSkipsOverlap(t *testing.T) {
	s := newStore(t)
	clock := at("2026-11-27 08:59")
	s.now = func() time.Time { return clock }
	q := &fakeQueue{}
	m := NewManager(s, q.submit, q.get, slog.New(slog.NewTextHandler(io.Discard, nil)))

	sc, err := m.Save(Script{
		Name: "digest", Code: "return args.n",
		Params:       []Param{{Name: "n", Type: TypeNumber, Required: true}},
		Schedule:     &Schedule{Enabled: true, Kind: KindEvery, Every: "1h"},
		ScheduleArgs: json.RawMessage(`{"n":2}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	m.fireDue(at("2026-11-27 09:30")) // not due until 09:59
	if len(q.specs) != 0 {
		t.Fatal("fired early")
	}
	m.fireDue(at("2026-11-27 10:00"))
	if len(q.specs) != 1 || string(q.specs[0].Args) != `{"n":2}` || q.specs[0].Source != SourceSchedule {
		t.Fatalf("specs %+v", q.specs)
	}
	got, _ := s.Get(sc.ID)
	if !got.NextRun.Equal(at("2026-11-27 11:00")) {
		t.Fatalf("next run %s", got.NextRun)
	}

	// Still running at the next slot: skipped, and the schedule moves on.
	m.fireDue(at("2026-11-27 11:00"))
	if len(q.specs) != 1 {
		t.Fatal("overlapping run started")
	}
	m.Finished(q.finish(got.LastRun.JobID))
	m.fireDue(at("2026-11-27 12:00"))
	if len(q.specs) != 2 {
		t.Fatal("did not fire after the previous run finished")
	}
}

func TestStartSkipsMissedRunsUnlessCatchUp(t *testing.T) {
	s := newStore(t)
	clock := at("2026-11-27 08:00")
	s.now = func() time.Time { return clock }
	q := &fakeQueue{}
	m := NewManager(s, q.submit, q.get, slog.New(slog.NewTextHandler(io.Discard, nil)))
	skip, _ := m.Save(Script{Name: "skip", Code: "1", Schedule: &Schedule{Enabled: true, Kind: KindDaily, At: "09:00"}})
	catch, _ := m.Save(Script{Name: "catch", Code: "1", Schedule: &Schedule{Enabled: true, Kind: KindDaily, At: "09:00", CatchUp: true}})

	clock = at("2026-11-29 12:00") // Nexora was closed for two days
	m.Start(time.Hour)
	defer m.Close()
	m.fireDue(clock)
	if len(q.specs) != 1 || q.specs[0].ScriptID != catch.ID {
		t.Fatalf("specs %+v", q.specs)
	}
	got, _ := s.Get(skip.ID)
	if !got.NextRun.Equal(at("2026-11-30 09:00")) {
		t.Fatalf("skipped script next run %s", got.NextRun)
	}
}
