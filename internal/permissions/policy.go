// Package permissions decides, per extension and per tool, whether a tool
// call from a script may run, must be approved by the user, or is blocked.
//
// Each extension has a Mode:
//
//	trusted   every tool runs without asking
//	ask       read-only tools run; others wait for the user's approval
//	readonly  read-only tools run; others are blocked
//
// and any tool can override its mode with allow, ask or deny. Tools that do
// not declare themselves read-only are treated as writes.
package permissions

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type Mode string

const (
	ModeTrusted  Mode = "trusted"
	ModeAsk      Mode = "ask"
	ModeReadOnly Mode = "readonly"

	// DefaultMode applies to extensions the user hasn't configured.
	DefaultMode = ModeAsk
)

type Decision string

const (
	Allow Decision = "allow"
	Ask   Decision = "ask"
	Deny  Decision = "deny"
)

func (m Mode) valid() bool {
	return m == ModeTrusted || m == ModeAsk || m == ModeReadOnly
}

func (d Decision) valid() bool { return d == Allow || d == Ask || d == Deny }

// Default is the decision a mode gives a tool with no override.
func (m Mode) Default(readOnly bool) Decision {
	switch {
	case m == ModeTrusted, readOnly:
		return Allow
	case m == ModeReadOnly:
		return Deny
	default:
		return Ask
	}
}

// Policy is one extension's configuration.
type Policy struct {
	Mode Mode `json:"mode"`
	// Tools holds per-tool overrides; tools not listed follow Mode.
	Tools map[string]Decision `json:"tools,omitempty"`
}

// Decide returns the effective decision for a tool.
func (p Policy) Decide(tool string, readOnly bool) Decision {
	if d, ok := p.Tools[tool]; ok {
		return d
	}
	return p.Mode.Default(readOnly)
}

// Store persists policies in a JSON file.
type Store struct {
	path string

	mu       sync.RWMutex
	policies map[string]Policy // by extension id
	// strict makes every tool call ask, whatever the mode or overrides say
	// (blocked tools stay blocked).
	strict bool
}

func Open(path string) (*Store, error) {
	s := &Store{path: path, policies: map[string]Policy{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var file struct {
		Strict     bool              `json:"strict"`
		Extensions map[string]Policy `json:"extensions"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s.strict = file.Strict
	for id, p := range file.Extensions {
		if !p.Mode.valid() {
			p.Mode = DefaultMode
		}
		for tool, d := range p.Tools {
			if !d.valid() {
				delete(p.Tools, tool)
			}
		}
		s.policies[id] = p
	}
	return s, nil
}

// Get returns an extension's policy (the default if it has none).
func (s *Store) Get(extID string) Policy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if p, ok := s.policies[extID]; ok {
		return clonePolicy(p)
	}
	return Policy{Mode: DefaultMode}
}

func (s *Store) Decide(extID, tool string, readOnly bool) Decision {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.policies[extID]
	if !ok {
		p = Policy{Mode: DefaultMode}
	}
	d := p.Decide(tool, readOnly)
	if s.strict && d == Allow {
		return Ask
	}
	return d
}

// Strict reports whether every tool call must be approved.
func (s *Store) Strict() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.strict
}

// SetStrict turns strict mode on or off.
func (s *Store) SetStrict(on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.strict = on
	return s.saveLocked()
}

// SetMode changes an extension's mode, keeping its per-tool overrides.
func (s *Store) SetMode(extID string, mode Mode) error {
	if !mode.valid() {
		return fmt.Errorf("unknown mode %q", mode)
	}
	return s.update(extID, func(p *Policy) { p.Mode = mode })
}

// SetTool overrides one tool's decision; an empty decision removes the
// override so the tool follows the extension's mode again.
func (s *Store) SetTool(extID, tool string, d Decision) error {
	if d != "" && !d.valid() {
		return fmt.Errorf("unknown decision %q", d)
	}
	return s.update(extID, func(p *Policy) {
		if d == "" {
			delete(p.Tools, tool)
			return
		}
		if p.Tools == nil {
			p.Tools = map[string]Decision{}
		}
		p.Tools[tool] = d
	})
}

// Reset removes an extension's policy, e.g. when it is uninstalled.
func (s *Store) Reset(extID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.policies, extID)
	return s.saveLocked()
}

func (s *Store) update(extID string, fn func(*Policy)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.policies[extID]
	if !ok {
		p = Policy{Mode: DefaultMode}
	}
	p = clonePolicy(p)
	fn(&p)
	s.policies[extID] = p
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	raw, err := json.MarshalIndent(map[string]any{"strict": s.strict, "extensions": s.policies}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func clonePolicy(p Policy) Policy {
	if p.Tools != nil {
		tools := make(map[string]Decision, len(p.Tools))
		for k, v := range p.Tools {
			tools[k] = v
		}
		p.Tools = tools
	}
	return p
}
