package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"nexora/internal/runner"
	"nexora/internal/scripts"
)

// ListScriptsInput is the input of list_scripts.
type ListScriptsInput struct{}

// Param is a saved script's parameter as AIs see it (scripts.Param keeps
// the default as raw JSON, which has no useful JSON Schema).
type Param struct {
	Name        string `json:"name" jsonschema:"JavaScript identifier; the script reads args.<name>"`
	Type        string `json:"type" jsonschema:"string, number, boolean or json"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Default     any    `json:"default,omitempty"`
}

func toParams(in []scripts.Param) []Param {
	out := make([]Param, len(in))
	for i, p := range in {
		out[i] = Param{Name: p.Name, Type: p.Type, Description: p.Description, Required: p.Required}
		if len(p.Default) > 0 {
			_ = json.Unmarshal(p.Default, &out[i].Default)
		}
	}
	return out
}

func fromParams(in []Param) ([]scripts.Param, error) {
	out := make([]scripts.Param, len(in))
	for i, p := range in {
		out[i] = scripts.Param{Name: p.Name, Type: p.Type, Description: p.Description, Required: p.Required}
		if p.Default != nil {
			raw, err := json.Marshal(p.Default)
			if err != nil {
				return nil, err
			}
			out[i].Default = raw
		}
	}
	return out, nil
}

// ScriptInfo describes a saved script to an AI.
type ScriptInfo struct {
	Name        string  `json:"name"`
	Description string  `json:"description,omitempty"`
	Params      []Param `json:"params" jsonschema:"Arguments the script reads as args.<name>"`
	Schedule    string  `json:"schedule,omitempty" jsonschema:"When it runs by itself, if scheduled"`
	SavedBy     string  `json:"savedBy" jsonschema:"'user' or 'ai'"`
}

type ListScriptsOutput struct {
	Scripts []ScriptInfo `json:"scripts"`
}

// RunScriptInput is the input of run_script.
type RunScriptInput struct {
	Name string         `json:"name" jsonschema:"Name of a saved script, from list_scripts"`
	Args map[string]any `json:"args,omitempty" jsonschema:"Arguments by parameter name; missing ones use their defaults"`
}

// SaveScriptInput is the input of save_script.
type SaveScriptInput struct {
	Name        string  `json:"name" jsonschema:"Short, unique name, e.g. 'Summarise inbox'"`
	Description string  `json:"description,omitempty" jsonschema:"What the script does and returns"`
	Code        string  `json:"code" jsonschema:"JavaScript async function body, as for run_code. Read inputs from the global 'args'."`
	Params      []Param `json:"params,omitempty" jsonschema:"Inputs the script reads from the global 'args'"`
}

type SaveScriptOutput struct {
	Name string `json:"name"`
	Next string `json:"next"`
}

func (s *server) addScriptTools(srv *mcp.Server, readOnly *mcp.ToolAnnotations) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_scripts",
		Description: "List the user's saved scripts with their parameters. Run one with run_script.",
		Annotations: readOnly,
	}, s.listScripts)
	mcp.AddTool(srv, &mcp.Tool{
		Name: "run_script",
		Description: fmt.Sprintf("Run a saved script by name with arguments. Returns the result directly if it finishes within %d seconds; "+
			"otherwise returns a jobId to poll with get_job.", int(s.opts.InlineWait.Seconds())),
	}, s.runScript)
	mcp.AddTool(srv, &mcp.Tool{
		Name: "save_script",
		Description: "Save a new script for the user to run again later, from you or from the Nexora app. " +
			"It cannot replace an existing script; only the user can edit or schedule scripts.",
	}, s.saveScript)
}

func (s *server) listScripts(_ context.Context, _ *mcp.CallToolRequest, _ ListScriptsInput) (*mcp.CallToolResult, ListScriptsOutput, error) {
	out := ListScriptsOutput{Scripts: []ScriptInfo{}}
	for _, sc := range s.opts.Scripts.List() {
		info := ScriptInfo{Name: sc.Name, Description: sc.Description, Params: toParams(sc.Params), SavedBy: "user"}
		if sc.CreatedBy == runner.SourceMCP {
			info.SavedBy = "ai"
		}
		if sc.Scheduled() {
			info.Schedule = describeSchedule(*sc.Schedule)
		}
		out.Scripts = append(out.Scripts, info)
	}
	return nil, out, nil
}

func (s *server) runScript(ctx context.Context, req *mcp.CallToolRequest, in RunScriptInput) (*mcp.CallToolResult, JobOutput, error) {
	if strings.TrimSpace(in.Name) == "" {
		return nil, JobOutput{}, errors.New("name is required")
	}
	var args json.RawMessage
	if len(in.Args) > 0 {
		raw, err := json.Marshal(in.Args)
		if err != nil {
			return nil, JobOutput{}, err
		}
		args = raw
	}
	job, err := s.opts.Scripts.Run(in.Name, args, runner.SourceMCP)
	if err != nil {
		return nil, JobOutput{}, err
	}
	return s.wait(ctx, req, job.ID, s.opts.InlineWait)
}

func (s *server) saveScript(_ context.Context, _ *mcp.CallToolRequest, in SaveScriptInput) (*mcp.CallToolResult, SaveScriptOutput, error) {
	params, err := fromParams(in.Params)
	if err != nil {
		return nil, SaveScriptOutput{}, err
	}
	// Create only, never scheduled: an AI must not change what runs
	// unattended, or the code of a script the user relies on.
	sc, err := s.opts.Scripts.Store.Save(scripts.Script{
		Name: in.Name, Description: in.Description, Code: in.Code, Params: params,
	}, runner.SourceMCP)
	if err != nil {
		return nil, SaveScriptOutput{}, err
	}
	return nil, SaveScriptOutput{
		Name: sc.Name,
		Next: fmt.Sprintf("Saved. Run it with run_script {\"name\": %q}. The user can edit or schedule it in Nexora.", sc.Name),
	}, nil
}

var weekdays = []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}

func describeSchedule(s scripts.Schedule) string {
	switch s.Kind {
	case scripts.KindEvery:
		return "every " + s.Every
	case scripts.KindDaily:
		return "daily at " + s.At
	default:
		days := make([]string, len(s.Days))
		for i, d := range s.Days {
			days[i] = weekdays[d]
		}
		return strings.Join(days, ", ") + " at " + s.At
	}
}
