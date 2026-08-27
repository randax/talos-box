//go:build darwin && !cgo

package helper

import "errors"

var errVMNetUnsupported = errors.New("vmnet.framework is only available on macOS with cgo enabled")

// StartInterface is unavailable on macOS builds without cgo support.
func StartInterface([]int, int, string, string) (*platformAttachment, error) {
	return nil, errVMNetUnsupported
}
