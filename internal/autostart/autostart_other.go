//go:build !linux && !darwin && !windows

package autostart

import "errors"

var errUnsupported = errors.New("starting at login is not supported on this system")

func enabled() (bool, error) { return false, nil }
func enable(string) error    { return errUnsupported }
func disable() error         { return nil }
