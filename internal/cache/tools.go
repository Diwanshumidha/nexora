package cache

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"nexora/internal/protocol"
)

// Namespace is where scripts reach the store: tools.store.*.
const Namespace = "store"

// Description introduces tools.store in the generated API.
const Description = "Built-in cache that keeps JSON values between runs. Entries expire (default 1 day, at most 30 days) and the least recently used are evicted when it is full, so treat a miss as normal and recompute."

// Specs describes tools.store for the generated TypeScript API.
func Specs() []protocol.ToolSpec {
	key := `"key": {"type": "string", "description": "Up to 256 bytes. Use prefixes like \"report:\" to group keys."}`
	prefix := `"prefix": {"type": "string", "description": "Only keys starting with this."}`
	info := `{"type": "object", "properties": {"key": {"type": "string"}, "size": {"type": "number", "description": "bytes"}, "expiresAt": {"type": "string"}}, "required": ["key", "size", "expiresAt"]}`
	return []protocol.ToolSpec{
		{
			Name:         "get",
			Description:  "The value stored under key, or null if it is missing or expired.",
			InputSchema:  json.RawMessage(`{"type": "object", "properties": {` + key + `}, "required": ["key"]}`),
			OutputSchema: json.RawMessage(`{}`),
			ReadOnly:     true,
		},
		{
			Name:         "set",
			Description:  "Stores a JSON-serialisable value (up to 1 MB) under key, replacing any previous value.",
			InputSchema:  json.RawMessage(`{"type": "object", "properties": {` + key + `, "value": {"description": "Any JSON value except null."}, "ttl": {"type": ["number", "string"], "description": "How long to keep it: seconds, or a duration like \"30m\", \"12h\", \"7d\". Default 1 day, at most 30 days."}}, "required": ["key", "value"]}`),
			OutputSchema: json.RawMessage(`{"type": "object", "properties": {"key": {"type": "string"}, "expiresAt": {"type": "string"}}, "required": ["key", "expiresAt"]}`),
		},
		{
			Name:         "has",
			Description:  "Whether key is stored and unexpired.",
			InputSchema:  json.RawMessage(`{"type": "object", "properties": {` + key + `}, "required": ["key"]}`),
			OutputSchema: json.RawMessage(`{"type": "boolean"}`),
			ReadOnly:     true,
		},
		{
			Name:         "delete",
			Description:  "Removes key. Returns whether it was stored.",
			InputSchema:  json.RawMessage(`{"type": "object", "properties": {` + key + `}, "required": ["key"]}`),
			OutputSchema: json.RawMessage(`{"type": "boolean"}`),
		},
		{
			Name:         "keys",
			Description:  "Lists stored keys, sorted, with their size and expiry.",
			InputSchema:  json.RawMessage(`{"type": "object", "properties": {` + prefix + `}}`),
			OutputSchema: json.RawMessage(`{"type": "array", "items": ` + info + `}`),
			ReadOnly:     true,
		},
		{
			Name:         "clear",
			Description:  "Removes every key (or every key with prefix). Returns how many were removed.",
			InputSchema:  json.RawMessage(`{"type": "object", "properties": {` + prefix + `}}`),
			OutputSchema: json.RawMessage(`{"type": "number"}`),
		},
	}
}

// ToolNames lists tools.store's functions.
func ToolNames() []string {
	specs := Specs()
	names := make([]string, len(specs))
	for i, s := range specs {
		names[i] = s.Name
	}
	return names
}

type args struct {
	Key    string          `json:"key"`
	Prefix string          `json:"prefix"`
	Value  json.RawMessage `json:"value"`
	TTL    json.RawMessage `json:"ttl"`
}

// Call runs tools.store.<tool> with JSON arguments and returns a JSON result.
func (s *Store) Call(tool string, raw json.RawMessage) (json.RawMessage, error) {
	var a args
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, fmt.Errorf("arguments: %w", err)
		}
	}
	needKey := func() error {
		if a.Key == "" {
			return errors.New(`key is required, e.g. { key: "report:q3" }`)
		}
		return nil
	}
	switch tool {
	case "get":
		if err := needKey(); err != nil {
			return nil, err
		}
		if v, ok := s.Get(a.Key); ok {
			return v, nil
		}
		return json.RawMessage("null"), nil
	case "set":
		if err := needKey(); err != nil {
			return nil, err
		}
		if len(a.Value) == 0 || string(a.Value) == "null" {
			return nil, errors.New("value is required (use delete to remove a key)")
		}
		ttl, err := parseTTL(a.TTL)
		if err != nil {
			return nil, err
		}
		exp, err := s.Set(a.Key, a.Value, ttl)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"key": a.Key, "expiresAt": exp.UTC().Format(time.RFC3339)})
	case "has":
		if err := needKey(); err != nil {
			return nil, err
		}
		return json.Marshal(s.Has(a.Key))
	case "delete":
		if err := needKey(); err != nil {
			return nil, err
		}
		return json.Marshal(s.Delete(a.Key))
	case "keys":
		type item struct {
			Key       string `json:"key"`
			Size      int    `json:"size"`
			ExpiresAt string `json:"expiresAt"`
		}
		out := []item{}
		for _, i := range s.Keys(a.Prefix) {
			out = append(out, item{i.Key, i.Size, i.Expires.UTC().Format(time.RFC3339)})
		}
		return json.Marshal(out)
	case "clear":
		return json.Marshal(s.Clear(a.Prefix))
	}
	return nil, fmt.Errorf("unknown tool %q", tool)
}

// parseTTL accepts seconds (a number) or a duration string such as "90s",
// "30m", "12h" or "7d". Absent means the default.
func parseTTL(raw json.RawMessage) (time.Duration, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, nil
	}
	bad := errors.New(`ttl must be seconds or a duration like "30m", "12h" or "7d"`)
	var secs float64
	if err := json.Unmarshal(raw, &secs); err == nil {
		if secs <= 0 {
			return 0, bad
		}
		return time.Duration(secs * float64(time.Second)), nil
	}
	var str string
	if err := json.Unmarshal(raw, &str); err != nil {
		return 0, bad
	}
	str = strings.TrimSpace(str)
	if days, ok := strings.CutSuffix(str, "d"); ok {
		n, err := strconv.ParseFloat(days, 64)
		if err != nil || n <= 0 {
			return 0, bad
		}
		return time.Duration(n * float64(24*time.Hour)), nil
	}
	d, err := time.ParseDuration(str)
	if err != nil || d <= 0 {
		return 0, bad
	}
	return d, nil
}
