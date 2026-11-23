package extension

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// SettingType is the kind of value a setting holds.
type SettingType string

const (
	SettingString  SettingType = "string"
	SettingNumber  SettingType = "number"
	SettingBoolean SettingType = "boolean"
	SettingSelect  SettingType = "select"
	SettingSecret  SettingType = "secret"  // stored like a string; masked in the UI
	SettingFolder  SettingType = "folder"  // one absolute directory
	SettingFolders SettingType = "folders" // a list of absolute directories
)

// Grant is the sandbox access a folder setting gives the extension.
type Grant string

const (
	GrantNone      Grant = ""
	GrantRead      Grant = "read"
	GrantReadWrite Grant = "readwrite"
)

// SettingSpec declares one user setting in nexora.json:
//
//	"settings": [
//	  {"key": "folders", "type": "folders", "label": "Trusted folders",
//	   "description": "Excel can open and edit workbooks here", "grant": "readwrite"}
//	]
type SettingSpec struct {
	Key         string      `json:"key"`
	Type        SettingType `json:"type"`
	Label       string      `json:"label"`
	Description string      `json:"description,omitempty"`
	Default     any         `json:"default,omitempty"`
	Choices     []string    `json:"choices,omitempty"` // select only
	Required    bool        `json:"required,omitempty"`
	// Grant makes the chosen folders accessible inside the extension's
	// sandbox (folder and folders only).
	Grant Grant `json:"grant,omitempty"`
}

func (s SettingSpec) validate() error {
	if !identPattern.MatchString(s.Key) {
		return fmt.Errorf("setting key %q must be an identifier", s.Key)
	}
	switch s.Type {
	case SettingString, SettingNumber, SettingBoolean, SettingSecret:
	case SettingSelect:
		if len(s.Choices) == 0 {
			return fmt.Errorf("setting %q: select needs choices", s.Key)
		}
	case SettingFolder, SettingFolders:
	default:
		return fmt.Errorf("setting %q: unknown type %q", s.Key, s.Type)
	}
	switch s.Grant {
	case GrantNone:
	case GrantRead, GrantReadWrite:
		if s.Type != SettingFolder && s.Type != SettingFolders {
			return fmt.Errorf("setting %q: grant is only allowed on folder settings", s.Key)
		}
	default:
		return fmt.Errorf("setting %q: unknown grant %q", s.Key, s.Grant)
	}
	return nil
}

// grants reports whether the setting changes sandbox access.
func (s SettingSpec) grants() bool { return s.Grant != GrantNone }

// Values are an extension's setting values, keyed by setting key.
type Values map[string]any

// resolve returns specs' values with defaults filled in and normalised types.
func resolve(specs []SettingSpec, stored Values) Values {
	out := Values{}
	for _, s := range specs {
		v, ok := stored[s.Key]
		if !ok || v == nil {
			v = s.Default
		}
		out[s.Key] = normalise(s, v)
	}
	return out
}

func normalise(s SettingSpec, v any) any {
	switch s.Type {
	case SettingNumber:
		if f, ok := toFloat(v); ok {
			return f
		}
		return float64(0)
	case SettingBoolean:
		b, _ := v.(bool)
		return b
	case SettingFolders:
		var out []string
		switch t := v.(type) {
		case []string:
			out = t
		case []any:
			for _, x := range t {
				if str, ok := x.(string); ok {
					out = append(out, str)
				}
			}
		}
		if out == nil {
			out = []string{}
		}
		return out
	default:
		str, _ := v.(string)
		if s.Type == SettingSelect && !slices.Contains(s.Choices, str) && len(s.Choices) > 0 {
			return s.Choices[0]
		}
		return str
	}
}

// SettingsPolicy guards which folders may be granted.
type SettingsPolicy struct {
	// Protected directories (and anything containing them) can never be
	// granted: Nexora's own data holds the MCP token and every extension's
	// private data.
	Protected []string
}

// Validate checks values against specs and returns the normalised values.
func (p SettingsPolicy) Validate(specs []SettingSpec, in Values) (Values, error) {
	out := Values{}
	for _, s := range specs {
		raw, present := in[s.Key]
		if !present || raw == nil {
			raw = s.Default
		}
		switch s.Type {
		case SettingNumber:
			if _, ok := toFloat(raw); !ok && raw != nil {
				return nil, fmt.Errorf("%s must be a number", s.Label)
			}
		case SettingBoolean:
			if _, ok := raw.(bool); !ok && raw != nil {
				return nil, fmt.Errorf("%s must be on or off", s.Label)
			}
		case SettingSelect:
			if str, _ := raw.(string); raw != nil && !slices.Contains(s.Choices, str) {
				return nil, fmt.Errorf("%s must be one of %s", s.Label, strings.Join(s.Choices, ", "))
			}
		case SettingFolders, SettingFolder:
		default:
			if _, ok := raw.(string); !ok && raw != nil {
				return nil, fmt.Errorf("%s must be text", s.Label)
			}
		}
		v := normalise(s, raw)

		switch s.Type {
		case SettingFolder:
			if str := v.(string); str != "" {
				clean, err := p.checkFolder(s, str)
				if err != nil {
					return nil, err
				}
				v = clean
			}
		case SettingFolders:
			var clean []string
			for _, f := range v.([]string) {
				c, err := p.checkFolder(s, f)
				if err != nil {
					return nil, err
				}
				if !slices.Contains(clean, c) {
					clean = append(clean, c)
				}
			}
			if clean == nil {
				clean = []string{}
			}
			v = clean
		}
		if s.Required && isEmpty(v) {
			return nil, fmt.Errorf("%s is required", s.Label)
		}
		out[s.Key] = v
	}
	return out, nil
}

func (p SettingsPolicy) checkFolder(s SettingSpec, dir string) (string, error) {
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("%s: %q must be an absolute path", s.Label, dir)
	}
	clean := filepath.Clean(dir)
	fi, err := os.Stat(clean)
	if err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%s: %q is not a folder", s.Label, dir)
	}
	if s.grants() {
		if clean == "/" {
			return "", fmt.Errorf("%s: granting the whole disk is not allowed; pick a specific folder", s.Label)
		}
		for _, prot := range p.Protected {
			if prot != "" && (within(prot, clean) || within(clean, prot)) {
				return "", fmt.Errorf("%s: %q contains Nexora's private data, so it can't be shared; pick a narrower folder", s.Label, dir)
			}
		}
	}
	return clean, nil
}

// within reports whether path is dir or inside it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// toFloat accepts any Go number (JSON gives float64; Go callers may pass ints).
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func isEmpty(v any) bool {
	switch t := v.(type) {
	case string:
		return t == ""
	case []string:
		return len(t) == 0
	}
	return v == nil
}

// grantedFolders lists the folders values grant, split by access level.
func grantedFolders(specs []SettingSpec, values Values) (read, write []string) {
	for _, s := range specs {
		if !s.grants() {
			continue
		}
		var dirs []string
		switch v := values[s.Key].(type) {
		case string:
			if v != "" {
				dirs = []string{v}
			}
		case []string:
			dirs = v
		}
		read = append(read, dirs...)
		if s.Grant == GrantReadWrite {
			write = append(write, dirs...)
		}
	}
	return read, write
}

// settingsStore persists values, one 0600 JSON file per extension.
type settingsStore struct {
	dir string
	mu  sync.Mutex
}

func (s *settingsStore) load(id string) (Values, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(filepath.Join(s.dir, id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return Values{}, nil
	}
	if err != nil {
		return nil, err
	}
	var v Values
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return v, nil
}

func (s *settingsStore) save(id string, v Values) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir, id+".json")
	if err := os.WriteFile(path+".tmp", raw, 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func (s *settingsStore) remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(filepath.Join(s.dir, id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
