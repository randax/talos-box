package daemon

import "golang.org/x/sys/unix"

func socketPeerPID(fd uintptr) (int, error) {
	credentials, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return 0, err
	}
	return int(credentials.Pid), nil
}
