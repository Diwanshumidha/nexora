package extension

import "syscall"

// The kernel stops a local MCP server if Nexora dies, so a crash never
// leaves it running.
func mcpProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}
