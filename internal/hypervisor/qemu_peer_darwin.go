//go:build darwin

package hypervisor

import (
	"errors"
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

func verifyQMPPeerDarwin(connection net.Conn, process *qemuProcess) error {
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("QMP connection has type %T, want Unix socket", connection)
	}
	if process == nil || process.process == nil {
		return errors.New("QEMU process identity is unavailable")
	}
	raw, err := unixConnection.SyscallConn()
	if err != nil {
		return fmt.Errorf("access QMP socket: %w", err)
	}
	var peerPID int
	var credentials *unix.Xucred
	var credentialErr error
	if err := raw.Control(func(fd uintptr) {
		peerPID, credentialErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
		if credentialErr == nil {
			credentials, credentialErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		}
	}); err != nil {
		return fmt.Errorf("inspect QMP peer: %w", err)
	}
	if credentialErr != nil {
		return fmt.Errorf("inspect QMP peer credentials: %w", credentialErr)
	}
	if peerPID != process.process.Pid || credentials.Uid != uint32(os.Getuid()) {
		return fmt.Errorf(
			"QMP peer pid=%d uid=%d does not match QEMU pid=%d uid=%d",
			peerPID,
			credentials.Uid,
			process.process.Pid,
			os.Getuid(),
		)
	}
	return nil
}
