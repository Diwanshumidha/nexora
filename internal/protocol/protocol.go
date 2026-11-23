// Package protocol defines the wire protocol between the Nexora core and
// extension processes.
//
// Transport: newline-delimited JSON-RPC 2.0 over the extension's stdin/stdout.
// stderr is captured by the host as extension logs.
//
// Lifecycle:
//
//	host -> ext  initialize   {hostVersion, extension}  -> {tools: []ToolSpec}
//	host -> ext  tools/call   {name, args}              -> any (tool result)
//	host -> ext  shutdown     {}                        -> {}
//	host -> ext  settings/update {settings}             -> {}
//	ext  -> host log          {level, message}          (notification)
package protocol

import "encoding/json"

const Version = "2.0"

// Host -> extension methods.
const (
	MethodInitialize = "initialize"
	MethodToolsCall  = "tools/call"
	MethodShutdown   = "shutdown"
	// MethodSettingsUpdate delivers new setting values to a running
	// extension (settings that don't change sandbox access).
	MethodSettingsUpdate = "settings/update"
)

// Extension -> host notifications.
const (
	MethodLog = "log"
)

// Standard JSON-RPC error codes plus Nexora specific ones.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603

	CodeToolNotFound = -32001
	CodeToolFailed   = -32002
)

// Message is a single JSON-RPC frame. A frame with Method set is a request
// (or a notification when ID is nil); otherwise it is a response.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// ToolSpec describes a single callable tool exposed by an extension. It is
// surfaced to the AI as `tools.<namespace>.<name>(input)`.
type ToolSpec struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"inputSchema,omitempty"`  // JSON Schema
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"` // JSON Schema
	// ReadOnly declares that the tool has no side effects. Tools that don't
	// declare it are treated as writes by the permission system.
	ReadOnly bool `json:"readOnly,omitempty"`
}

type InitializeParams struct {
	HostVersion string `json:"hostVersion"`
	ExtensionID string `json:"extensionId"`
	DataDir     string `json:"dataDir"` // per-extension writable storage
	// Settings are the user's values for the manifest's settings.
	Settings map[string]any `json:"settings"`
}

type SettingsUpdateParams struct {
	Settings map[string]any `json:"settings"`
}

type InitializeResult struct {
	Tools []ToolSpec `json:"tools"`
}

type ToolsCallParams struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type LogParams struct {
	Level   string `json:"level"`
	Message string `json:"message"`
}
