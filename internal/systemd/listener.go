package systemd

import (
	"fmt"
	"net"
	"os"
	"strconv"
)

const inheritedFDStart = 3

// InheritedListener returns the systemd-activated listener, if present.
func InheritedListener(name string) (net.Listener, bool, error) {
	return inheritedListener(name, os.Getpid(), os.Getenv, inheritedFDStart)
}

func inheritedListener(name string, pid int, getenv func(string) string, fd int) (net.Listener, bool, error) {
	listenPID := getenv("LISTEN_PID")
	listenFDS := getenv("LISTEN_FDS")
	if listenPID == "" && listenFDS == "" {
		return nil, false, nil
	}
	if listenPID == "" || listenFDS == "" {
		return nil, false, fmt.Errorf("incomplete systemd socket activation environment")
	}
	activatedPID, err := strconv.Atoi(listenPID)
	if err != nil {
		return nil, false, fmt.Errorf("parse LISTEN_PID: %w", err)
	}
	if activatedPID != pid {
		return nil, false, nil
	}
	count, err := strconv.Atoi(listenFDS)
	if err != nil {
		return nil, false, fmt.Errorf("parse LISTEN_FDS: %w", err)
	}
	if count == 0 {
		return nil, false, nil
	}
	if count != 1 {
		return nil, false, fmt.Errorf("expected exactly one activated listener for %s, got %d", name, count)
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		return nil, false, fmt.Errorf("open activated listener fd %d for %s", fd, name)
	}
	listener, err := net.FileListener(file)
	_ = file.Close()
	if err != nil {
		return nil, false, fmt.Errorf("use activated listener for %s: %w", name, err)
	}
	return listener, true, nil
}
