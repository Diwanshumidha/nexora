//go:build !linux

package cloudflare

import (
	"os"
	"syscall"
)

func sysProcAttr() *syscall.SysProcAttr { return nil }

var interruptSignal = os.Interrupt
