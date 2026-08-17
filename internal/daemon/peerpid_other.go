//go:build !darwin && !linux

package daemon

import "errors"

func socketPeerPID(uintptr) (int, error) {
	return 0, errors.New("unix socket peer pid lookup is unsupported on this platform")
}
