package cloudflare

import "syscall"

// The kernel kills cloudflared if Nexora dies, so a crash never leaves an
// orphaned public tunnel behind.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}

var interruptSignal = syscall.SIGTERM
