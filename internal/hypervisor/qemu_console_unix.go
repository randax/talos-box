//go:build darwin || linux

package hypervisor

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func newQEMUConsoleProxy(path string) (*consoleProxy, *os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, nil, fmt.Errorf("create QEMU console directory: %w", err)
	}
	descriptors, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("create QEMU console socketpair: %w", err)
	}
	hostFile := os.NewFile(uintptr(descriptors[0]), "qemu-console-host")
	guestFile := os.NewFile(uintptr(descriptors[1]), "qemu-console-guest")
	if hostFile == nil || guestFile == nil {
		if hostFile != nil {
			_ = hostFile.Close()
		} else {
			_ = unix.Close(descriptors[0])
		}
		if guestFile != nil {
			_ = guestFile.Close()
		} else {
			_ = unix.Close(descriptors[1])
		}
		return nil, nil, errors.New("wrap QEMU console socketpair")
	}
	hostConnection, err := net.FileConn(hostFile)
	_ = hostFile.Close()
	if err != nil {
		_ = guestFile.Close()
		return nil, nil, fmt.Errorf("open QEMU console socketpair: %w", err)
	}
	listener, err := listenUnix(path, "console")
	if err != nil {
		_ = hostConnection.Close()
		_ = guestFile.Close()
		return nil, nil, err
	}
	proxy := startConsoleProxy(listener, hostConnection, hostConnection, hostConnection.Close)
	return proxy, guestFile, nil
}
