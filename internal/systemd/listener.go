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
	// Ignore activation metadata unless it forms the exact contract this
	// process expects, preserving normal startup under inherited shell state.
	if listenPID == "" && listenFDS == "" {
		return nil, false, nil
	}
	if listenPID == "" || listenFDS == "" {
		return nil, false, nil
	}
	activatedPID, err := strconv.Atoi(listenPID)
	if err != nil || activatedPID != pid {
		return nil, false, nil
	}
	count, err := strconv.Atoi(listenFDS)
	if err != nil || count != 1 {
		return nil, false, nil
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
