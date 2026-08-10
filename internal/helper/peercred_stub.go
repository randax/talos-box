//go:build !darwin && !linux

package helper

import (
	"errors"
	"net"
)

func peerUID(*net.UnixConn) (uint32, error) {
	return 0, errors.New("LOCAL_PEERCRED is only available on macOS")
}
