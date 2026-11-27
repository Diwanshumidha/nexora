// Package scripts keeps saved code-mode scripts: named, reusable scripts
// with typed parameters (the script's global `args`) and an optional
// schedule that runs them by themselves while Nexora is running.
//
// Scripts live in one JSON file readable only by the user. Every run, by
// hand, from an AI or on schedule, goes through the shared job queue, so
// trust settings and approvals apply exactly as for any other script.
package scripts

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"nexora/internal/jobs"
)

const (
	maxScripts   = 500
	maxNameLen   = 80
	maxCodeBytes = 64 << 10
	maxParams    = 32
)

var ErrNotFound = errors.New("script not found")

// Parameter types.
const (
	TypeString  = "string"
	TypeNumber  = "number"
	TypeBoolean = "boolean"
	TypeJSON    = "json" // any JSON value
)

// Param is one input of a script, read by the script as args.<Name>.
type Param struct {
	Name        string          `json:"name"`
	Type        string          `json:"type"`
	Description string          `json:"description,omitempty"`
	Required    bool            `json:"required,omitempty"`
	Default     json.RawMessage `json:"default,omitempty"`
}

// LastRun is the most recent run of a script, from anywhere.
type LastRun struct {
	JobID  string     `json:"jobId"`
	Source string     `json:"source"`
	State  jobs.State `json:"state"`
	Error  string     `json:"error,omitempty"`
	At     time.Time  `json:"at"`
}

// Script is a saved script.
type Script struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description,omitempty"`
	Code        string  `json:"code"`
	Params      []Param `json:"params"`
	// Schedule is nil for scripts that only run when asked.
	Schedule *Schedule `json:"schedule,omitempty"`
	// ScheduleArgs are the arguments scheduled runs use (a JSON object).
	ScheduleArgs json.RawMessage `json:"scheduleArgs,omitempty"`
	// CreatedBy is "app" or "mcp" (saved by an AI).
	CreatedBy string     `json:"createdBy"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
	LastRun   *LastRun   `json:"lastRun,omitempty"`
	NextRun   *time.Time `json:"nextRun,omitempty"` // set while the schedule is enabled
}

// Scheduled reports whether the script runs by itself.
func (s *Script) Scheduled() bool { return s.Schedule != nil && s.Schedule.Enabled }

// Validate returns the script as it would be stored, or why it can't be.
func (s Script) Validate() (Script, error) {
	c := clone(&s)
	err := c.normalize()
	return c, err
}

var identRe = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

// normalize trims and checks a script before it is stored.
func (s *Script) normalize() error {
	s.Name = strings.TrimSpace(s.Name)
	s.Description = strings.TrimSpace(s.Description)
	switch {
	case s.Name == "":
		return errors.New("give the script a name")
	case len(s.Name) > maxNameLen:
		return fmt.Errorf("the name is longer than %d characters", maxNameLen)
	case strings.TrimSpace(s.Code) == "":
		return errors.New("the script is empty")
	case len(s.Code) > maxCodeBytes:
		return fmt.Errorf("the script is larger than %d KB", maxCodeBytes>>10)
	case len(s.Params) > maxParams:
		return fmt.Errorf("a script can have at most %d parameters", maxParams)
	}
	if s.Params == nil {
		s.Params = []Param{}
	}
	seen := map[string]bool{}
	for i := range s.Params {
		p := &s.Params[i]
		p.Name = strings.TrimSpace(p.Name)
		p.Description = strings.TrimSpace(p.Description)
		if !identRe.MatchString(p.Name) {
			return fmt.Errorf("parameter %q must be a JavaScript name (letters, digits, _ or $)", p.Name)
		}
		if seen[p.Name] {
			return fmt.Errorf("parameter %q is listed twice", p.Name)
		}
		seen[p.Name] = true
		switch p.Type {
		case TypeString, TypeNumber, TypeBoolean, TypeJSON:
		case "":
			p.Type = TypeString
		default:
			return fmt.Errorf("parameter %q: unknown type %q", p.Name, p.Type)
		}
		if isNull(p.Default) {
			p.Default = nil
		} else if err := checkType(*p, p.Default); err != nil {
			return fmt.Errorf("default of %s", err)
		}
	}
	if s.Schedule != nil {
		if err := s.Schedule.validate(); err != nil {
			return err
		}
	}
	if isNull(s.ScheduleArgs) {
		s.ScheduleArgs = nil
	}
	if s.Scheduled() {
		// Checked now, so a schedule never fires into a missing argument.
		if _, err := s.ResolveArgs(s.ScheduleArgs); err != nil {
			return fmt.Errorf("scheduled runs: %w", err)
		}
	}
	return nil
}

// ResolveArgs merges given (a JSON object, or empty) over the parameters'
// defaults and checks the result against the parameters.
func (s *Script) ResolveArgs(given json.RawMessage) (json.RawMessage, error) {
	in := map[string]json.RawMessage{}
	if !isNull(given) {
		if err := json.Unmarshal(given, &in); err != nil {
			return nil, errors.New("arguments must be a JSON object")
		}
	}
	out := map[string]json.RawMessage{}
	known := map[string]bool{}
	for _, p := range s.Params {
		known[p.Name] = true
		v, ok := in[p.Name]
		if !ok || isNull(v) {
			v = p.Default
		}
		if isNull(v) {
			if p.Required {
				return nil, fmt.Errorf("%q is required", p.Name)
			}
			continue
		}
		if err := checkType(p, v); err != nil {
			return nil, err
		}
		out[p.Name] = v
	}
	for k := range in {
		if !known[k] {
			return nil, fmt.Errorf("the script has no parameter %q", k)
		}
	}
	return json.Marshal(out)
}

func checkType(p Param, v json.RawMessage) error {
	var x any
	if err := json.Unmarshal(v, &x); err != nil {
		return fmt.Errorf("%q is not valid JSON", p.Name)
	}
	ok := true
	switch p.Type {
	case TypeString:
		_, ok = x.(string)
	case TypeNumber:
		_, ok = x.(float64)
	case TypeBoolean:
		_, ok = x.(bool)
	}
	if !ok {
		return fmt.Errorf("%q must be a %s", p.Name, p.Type)
	}
	return nil
}

func isNull(v json.RawMessage) bool {
	v = bytes.TrimSpace(v)
	return len(v) == 0 || string(v) == "null"
}

// Store is the set of saved scripts, persisted to one file.
type Store struct {
	path string

	mu      sync.Mutex
	scripts []*Script // in creation order
	now     func() time.Time
}

// OpenStore loads path, creating it on the first save.
func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, now: time.Now}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var file struct {
		Scripts []*Script `json:"scripts"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		// Keep the damaged file for the user instead of overwriting it.
		_ = os.Rename(path, path+".damaged")
		return s, nil
	}
	s.scripts = file.Scripts
	return s, nil
}

// List returns copies of all scripts, sorted by name.
func (s *Store) List() []Script {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Script, len(s.scripts))
	for i, sc := range s.scripts {
		out[i] = clone(sc)
	}
	slices.SortFunc(out, func(a, b Script) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	return out
}

// Get finds a script by ID, or else by name (case-insensitive).
func (s *Store) Get(idOrName string) (Script, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sc := s.findLocked(idOrName)
	if sc == nil {
		return Script{}, fmt.Errorf("%w: %q", ErrNotFound, idOrName)
	}
	return clone(sc), nil
}

func (s *Store) findLocked(idOrName string) *Script {
	for _, sc := range s.scripts {
		if sc.ID == idOrName {
			return sc
		}
	}
	for _, sc := range s.scripts {
		if strings.EqualFold(sc.Name, strings.TrimSpace(idOrName)) {
			return sc
		}
	}
	return nil
}

// Save creates in (empty ID) or updates the script with in's ID. Run state
// and creation details are kept from the stored script; the next run is
// recomputed if the schedule changed.
func (s *Store) Save(in Script, createdBy string) (Script, error) {
	if err := in.normalize(); err != nil {
		return Script{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sc := range s.scripts {
		if sc.ID != in.ID && strings.EqualFold(sc.Name, in.Name) {
			return Script{}, fmt.Errorf("another script is already called %q", in.Name)
		}
	}
	now := s.now()
	in.UpdatedAt = now
	var old *Script
	if in.ID == "" {
		if len(s.scripts) >= maxScripts {
			return Script{}, fmt.Errorf("at most %d scripts can be saved", maxScripts)
		}
		in.ID, in.CreatedAt, in.CreatedBy = newID(), now, createdBy
		in.LastRun, in.NextRun = nil, nil
	} else {
		if old = s.findByIDLocked(in.ID); old == nil {
			return Script{}, fmt.Errorf("%w: %q", ErrNotFound, in.ID)
		}
		in.CreatedAt, in.CreatedBy, in.LastRun, in.NextRun = old.CreatedAt, old.CreatedBy, old.LastRun, old.NextRun
	}
	switch {
	case !in.Scheduled():
		in.NextRun = nil
	case old == nil || in.NextRun == nil || !sameSchedule(old.Schedule, in.Schedule):
		next := in.Schedule.Next(now)
		in.NextRun = &next
	}
	stored := clone(&in)
	if old != nil {
		*old = stored
	} else {
		s.scripts = append(s.scripts, &stored)
	}
	return clone(&stored), s.persistLocked()
}

func (s *Store) findByIDLocked(id string) *Script {
	for _, sc := range s.scripts {
		if sc.ID == id {
			return sc
		}
	}
	return nil
}

func sameSchedule(a, b *Schedule) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Enabled == b.Enabled && a.Kind == b.Kind && a.Every == b.Every && a.At == b.At && slices.Equal(a.Days, b.Days)
}

// Delete removes a script.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, sc := range s.scripts {
		if sc.ID == id {
			s.scripts = slices.Delete(s.scripts, i, i+1)
			return s.persistLocked()
		}
	}
	return fmt.Errorf("%w: %q", ErrNotFound, id)
}

// update applies fn to the stored script with id and saves the file.
func (s *Store) update(id string, fn func(*Script)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sc := s.findByIDLocked(id)
	if sc == nil {
		return fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	fn(sc)
	return s.persistLocked()
}

func (s *Store) persistLocked() error {
	raw, err := json.MarshalIndent(struct {
		Scripts []*Script `json:"scripts"`
	}{s.scripts}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// clone deep-copies the parts of a script that are shared by reference.
func clone(sc *Script) Script {
	c := *sc
	c.Params = slices.Clone(sc.Params)
	if sc.Schedule != nil {
		sch := *sc.Schedule
		sch.Days = slices.Clone(sc.Schedule.Days)
		c.Schedule = &sch
	}
	if sc.LastRun != nil {
		lr := *sc.LastRun
		c.LastRun = &lr
	}
	if sc.NextRun != nil {
		n := *sc.NextRun
		c.NextRun = &n
	}
	return c
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "scr_" + hex.EncodeToString(b[:])
}
