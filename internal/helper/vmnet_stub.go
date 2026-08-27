//go:build !darwin && !linux

package helper

import "errors"

var errVMNetUnsupported = errors.New("vmnet.framework is only available on macOS with cgo enabled")

// StartInterface is unavailable outside macOS cgo builds.
func StartInterface([]int, int, string, string) (*platformAttachment, error) {
	return nil, errVMNetUnsupported
}
