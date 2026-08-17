package daemon

import "golang.org/x/sys/unix"

func socketPeerPID(fd uintptr) (int, error) {
	return unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
}
