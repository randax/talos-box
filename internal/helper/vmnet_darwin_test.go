//go:build darwin && cgo

package helper

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestVMNetSocketpairBuffersRaisedOnBothEnds(t *testing.T) {
	sockets, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatalf("create socketpair: %v", err)
	}
	for _, fd := range sockets {
		defer func() { _ = unix.Close(fd) }()
	}

	if err := configureVMNetSocketpairBuffers(sockets[0], sockets[1]); err != nil {
		t.Fatalf("configure socketpair buffers: %v", err)
	}

	target := vmnetSocketBufferSize()
	for index, fd := range sockets {
		for _, option := range []int{unix.SO_SNDBUF, unix.SO_RCVBUF} {
			got, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, option)
			if err != nil {
				t.Fatalf("read socket %d option %d: %v", index, option, err)
			}
			if got < target {
				t.Errorf("socket %d option %d = %d, want at least %d", index, option, got, target)
			}
		}
	}
}
