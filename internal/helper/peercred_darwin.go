//go:build darwin

package helper

import (
	"errors"
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

func peerUID(connection *net.UnixConn) (uint32, error) {
	rawConnection, err := connection.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("access unix socket: %w", err)
	}

	var credentials *unix.Xucred
	var credentialsErr error
	if err := rawConnection.Control(func(fd uintptr) {
		credentials, credentialsErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return 0, fmt.Errorf("access unix socket descriptor: %w", err)
	}
	if credentialsErr != nil {
		return 0, fmt.Errorf("get LOCAL_PEERCRED: %w", credentialsErr)
	}
	if credentials == nil {
		return 0, errors.New("get LOCAL_PEERCRED: nil credentials")
	}
	return credentials.Uid, nil
}
