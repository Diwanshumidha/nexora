package codemode

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"nexora/internal/protocol"
)

// APINamespace pairs a code-mode namespace with its tool specs.
type APINamespace struct {
	Name        string
	Description string
	Tools       []protocol.ToolSpec
}

// GenerateTypes renders a TypeScript declaration of the `tools` object. It is
// handed to the model as the API it can program against.
func GenerateTypes(namespaces []APINamespace) string {
	var b strings.Builder
	b.WriteString("declare const tools: {\n")
	for _, ns := range namespaces {
		if ns.Description != "" {
			fmt.Fprintf(&b, "  /** %s */\n", comment(ns.Description))
		}
		fmt.Fprintf(&b, "  %s: {\n", ns.Name)
		for _, t := range ns.Tools {
			if t.Description != "" {
				fmt.Fprintf(&b, "    /** %s */\n", comment(t.Description))
			}
			fmt.Fprintf(&b, "    %s(input: %s): Promise<%s>;\n",
				t.Name, schemaToTS(t.InputSchema, 3), schemaToTS(t.OutputSchema, 3))
		}
		b.WriteString("  };\n")
	}
	b.WriteString("};\n")
	return b.String()
}

var identPattern = regexp.MustCompile(`^[a-zA-Z_$][a-zA-Z0-9_$]*$`)

// propName quotes object keys that are not valid identifiers.
func propName(k string) string {
	if identPattern.MatchString(k) {
		return k
	}
	return strconv.Quote(k)
}

// comment escapes text for use inside a /** */ block.
func comment(s string) string {
	s = strings.ReplaceAll(s, "*/", "*\\/")
	return strings.Join(strings.Fields(s), " ")
}

type schema struct {
	Type        any                `json:"type"`
	Description string             `json:"description"`
	Properties  map[string]*schema `json:"properties"`
	Required    []string           `json:"required"`
	Items       *schema            `json:"items"`
	Enum        []any              `json:"enum"`
}

func schemaToTS(raw json.RawMessage, depth int) string {
	if len(raw) == 0 {
		return "any"
	}
	var s schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return "any"
	}
	return s.ts(depth)
}

func (s *schema) ts(depth int) string {
	if s == nil {
		return "any"
	}
	if len(s.Enum) > 0 {
		parts := make([]string, len(s.Enum))
		for i, e := range s.Enum {
			b, _ := json.Marshal(e)
			parts[i] = string(b)
		}
		return strings.Join(parts, " | ")
	}
	switch t := s.Type.(type) {
	case []any: // e.g. ["string", "null"]
		parts := make([]string, 0, len(t))
		for _, v := range t {
			name, _ := v.(string)
			parts = append(parts, (&schema{Type: name, Properties: s.Properties, Required: s.Required, Items: s.Items}).ts(depth))
		}
		return strings.Join(parts, " | ")
	}
	typ, _ := s.Type.(string)
	switch typ {
	case "string":
		return "string"
	case "number", "integer":
		return "number"
	case "boolean":
		return "boolean"
	case "null":
		return "null"
	case "array":
		return "Array<" + s.Items.ts(depth) + ">"
	case "object":
		if len(s.Properties) == 0 {
			return "Record<string, any>"
		}
		required := map[string]bool{}
		for _, r := range s.Required {
			required[r] = true
		}
		keys := make([]string, 0, len(s.Properties))
		for k := range s.Properties {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		indent := strings.Repeat("  ", depth)
		var b strings.Builder
		b.WriteString("{\n")
		for _, k := range keys {
			p := s.Properties[k]
			if p.Description != "" {
				fmt.Fprintf(&b, "%s  /** %s */\n", indent, comment(p.Description))
			}
			opt := "?"
			if required[k] {
				opt = ""
			}
			fmt.Fprintf(&b, "%s  %s%s: %s;\n", indent, propName(k), opt, p.ts(depth+1))
		}
		b.WriteString(indent)
		b.WriteString("}")
		return b.String()
	}
	return "any"
}
