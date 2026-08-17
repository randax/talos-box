package daemon

import (
	"errors"
	"fmt"
	"net"
)

// PeerPID reports the pid of the process serving a unix socket connection. The
// pid comes from the kernel rather than the protocol, so a daemon too old to
// describe itself can still be identified — and replaced — by the CLI.
func PeerPID(connection net.Conn) (int, error) {
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return 0, errors.New("connection is not a unix socket")
	}
	raw, err := unixConnection.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("access unix socket: %w", err)
	}
	var pid int
	var lookupErr error
	if err := raw.Control(func(fd uintptr) { pid, lookupErr = socketPeerPID(fd) }); err != nil {
		return 0, fmt.Errorf("inspect unix socket: %w", err)
	}
	if lookupErr != nil {
		return 0, fmt.Errorf("read unix socket peer: %w", lookupErr)
	}
	if pid <= 0 {
		return 0, errors.New("unix socket peer pid is unavailable")
	}
	return pid, nil
}
