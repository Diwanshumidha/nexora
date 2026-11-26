package extension

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// MCPServerSpec is what the user enters to add an MCP server.
type MCPServerSpec struct {
	Name string `json:"name"`
	// Namespace under tools; derived from Name if empty.
	Namespace string `json:"namespace,omitempty"`
	// CommandLine starts a local server over stdio, e.g.
	// `npx -y @modelcontextprotocol/server-github`. Quotes group arguments.
	CommandLine string `json:"commandLine,omitempty"`
	// URL connects to a remote server over streamable HTTP instead.
	URL string `json:"url,omitempty"`
	// Env and Headers become settings, so their values (often tokens) are
	// kept in the settings store and can be changed later.
	Env     map[string]string `json:"env,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

var secretName = regexp.MustCompile(`(?i)token|key|secret|password|passwd|auth|credential|cookie`)

// NewMCPManifest turns spec into a manifest plus initial setting values.
func NewMCPManifest(spec MCPServerSpec) (*Manifest, Values, error) {
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		return nil, nil, errors.New("give the server a name")
	}
	m := &Manifest{
		ID: mcpID(name), Name: name, Version: "1.0.0", Runtime: RuntimeMCP, Icon: "plug",
		Namespace: strings.TrimSpace(spec.Namespace),
	}
	if m.Namespace == "" {
		m.Namespace = mcpNamespace(name)
	}
	cmdLine, rawURL := strings.TrimSpace(spec.CommandLine), strings.TrimSpace(spec.URL)
	switch {
	case cmdLine != "" && rawURL != "":
		return nil, nil, errors.New("enter a command or a URL, not both")
	case cmdLine != "":
		args, err := SplitCommandLine(cmdLine)
		if err != nil {
			return nil, nil, err
		}
		m.Command, m.Args = args[0], args[1:]
		m.Description = "Local MCP server: " + cmdLine
	case rawURL != "":
		u, err := url.Parse(rawURL)
		if err != nil || u.Host == "" {
			return nil, nil, fmt.Errorf("%q is not a valid URL", rawURL)
		}
		m.URL = rawURL
		m.Description = "Remote MCP server at " + u.Host
	default:
		return nil, nil, errors.New("enter the command that starts the server, or its URL")
	}

	values := Values{}
	addSetting := func(key, label, value string) (string, error) {
		if !identPattern.MatchString(key) {
			return "", fmt.Errorf("%q is not a valid name", label)
		}
		if slices.ContainsFunc(m.Settings, func(s SettingSpec) bool { return s.Key == key }) {
			return "", fmt.Errorf("%q is listed twice", label)
		}
		typ := SettingString
		if secretName.MatchString(label) {
			typ = SettingSecret
		}
		m.Settings = append(m.Settings, SettingSpec{Key: key, Type: typ, Label: label, Required: true})
		values[key] = value
		return "${" + key + "}", nil
	}
	for _, k := range sortedKeys(spec.Env) {
		ref, err := addSetting(k, k, spec.Env[k])
		if err != nil {
			return nil, nil, err
		}
		if m.Env == nil {
			m.Env = map[string]string{}
		}
		m.Env[k] = ref
	}
	for _, k := range sortedKeys(spec.Headers) {
		if m.URL == "" {
			return nil, nil, errors.New("headers only apply to a remote server (URL)")
		}
		ref, err := addSetting("header_"+jsToolName(k), k+" header", spec.Headers[k])
		if err != nil {
			return nil, nil, err
		}
		if m.Headers == nil {
			m.Headers = map[string]string{}
		}
		m.Headers[k] = ref
	}
	if err := m.Validate(); err != nil {
		return nil, nil, err
	}
	return m, values, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		if strings.TrimSpace(k) != "" {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}

// mcpID derives an extension id ("GitHub Issues" -> "github-issues").
func mcpID(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	id := strings.TrimRight(b.String(), "-")
	if id == "" || id[0] < 'a' || id[0] > 'z' || len(id) < 2 {
		id = "mcp-" + id
	}
	return strings.TrimRight(id[:min(len(id), 64)], "-")
}

// mcpNamespace derives a JS identifier ("GitHub Issues" -> "githubIssues").
func mcpNamespace(name string) string {
	var b strings.Builder
	for i, word := range strings.FieldsFunc(name, func(r rune) bool { return r > unicode.MaxASCII || !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		word = strings.ToLower(word)
		if i > 0 {
			word = strings.ToUpper(word[:1]) + word[1:]
		}
		b.WriteString(word)
	}
	ns := b.String()
	if ns == "" || unicode.IsDigit(rune(ns[0])) {
		ns = "mcp" + ns
	}
	return ns
}

// SplitCommandLine splits a command line into arguments. Single quotes are
// literal, double quotes allow \" and \\, and outside quotes a backslash
// only escapes a space, quote or backslash, so Windows paths survive.
func SplitCommandLine(s string) ([]string, error) {
	var (
		args  []string
		cur   strings.Builder
		inArg bool
		quote rune
	)
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case quote == '"':
			switch {
			case r == '"':
				quote = 0
			case r == '\\' && i+1 < len(rs) && (rs[i+1] == '"' || rs[i+1] == '\\'):
				i++
				cur.WriteRune(rs[i])
			default:
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, inArg = r, true
		case r == '\\' && i+1 < len(rs) && strings.ContainsRune(` '"\`, rs[i+1]):
			i++
			cur.WriteRune(rs[i])
			inArg = true
		case unicode.IsSpace(r):
			if inArg {
				args = append(args, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteRune(r)
			inArg = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unclosed quote in command")
	}
	if inArg {
		args = append(args, cur.String())
	}
	if len(args) == 0 {
		return nil, errors.New("empty command")
	}
	return args, nil
}
