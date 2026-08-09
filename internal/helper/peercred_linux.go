//go:build linux

package helper

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

func peerUID(connection *net.UnixConn) (uint32, error) {
	rawConnection, err := connection.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("access unix socket: %w", err)
	}

	var credentials *unix.Ucred
	var credentialsErr error
	if err := rawConnection.Control(func(fd uintptr) {
		credentials, credentialsErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, fmt.Errorf("access unix socket descriptor: %w", err)
	}
	if credentialsErr != nil {
		return 0, fmt.Errorf("get SO_PEERCRED: %w", credentialsErr)
	}
	if credentials == nil {
		return 0, fmt.Errorf("get SO_PEERCRED: nil credentials")
	}
	return credentials.Uid, nil
}
