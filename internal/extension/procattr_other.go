//go:build !linux

package extension

import "syscall"

func mcpProcAttr() *syscall.SysProcAttr { return nil }
