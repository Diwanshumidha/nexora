package extension

import (
	"context"
	"encoding/json"
	"errors"

	"nexora/internal/protocol"
)

// errRestartRequired is returned by conn.UpdateSettings when new settings
// only take effect on a fresh start.
var errRestartRequired = errors.New("restart required to apply settings")

// conn is a live connection to a running extension, whatever its runtime:
// a Node.js process speaking the Nexora protocol, or an MCP server.
type conn interface {
	// CallTool invokes tool with args (valid JSON) and returns its JSON result.
	CallTool(ctx context.Context, tool string, args json.RawMessage) (json.RawMessage, error)
	// UpdateSettings delivers new setting values, or returns
	// errRestartRequired if the extension must be restarted to apply them.
	UpdateSettings(ctx context.Context, values Values) error
	// Stop shuts the extension down, forcibly once ctx is done. It always
	// waits for it to exit.
	Stop(ctx context.Context) error
	// Done is closed once the extension has exited or disconnected.
	Done() <-chan struct{}
	// ExitErr explains why; only valid after Done is closed.
	ExitErr() error
}

var _ conn = (*Process)(nil)

// CallTool sends tools/call. Params are {"name":<tool>,"args":<args>}, sent
// in parts so args are never copied.
func (p *Process) CallTool(ctx context.Context, tool string, args json.RawMessage) (json.RawMessage, error) {
	name, err := json.Marshal(tool)
	if err != nil {
		return nil, err
	}
	head := append(append([]byte(`{"name":`), name...), `,"args":`...)
	return p.CallRaw(ctx, protocol.MethodToolsCall, head, args, []byte("}"))
}

func (p *Process) UpdateSettings(ctx context.Context, values Values) error {
	err := p.Call(ctx, protocol.MethodSettingsUpdate, protocol.SettingsUpdateParams{Settings: values}, nil)
	var rpcErr *protocol.Error
	if errors.As(err, &rpcErr) && rpcErr.Code == protocol.CodeMethodNotFound {
		// Older SDK without live updates.
		return errRestartRequired
	}
	return err
}
