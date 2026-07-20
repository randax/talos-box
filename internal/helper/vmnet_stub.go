//go:build !darwin || !cgo

package helper

import "errors"

var errVMNetUnsupported = errors.New("vmnet.framework is only available on macOS with cgo enabled")

// StartInterface is unavailable outside macOS cgo builds.
func StartInterface(int) (int, error) {
	return -1, errVMNetUnsupported
}

// StopInterface is unavailable outside macOS cgo builds.
func StopInterface(int) error {
	return errVMNetUnsupported
}
