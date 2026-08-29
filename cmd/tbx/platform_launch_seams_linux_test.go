//go:build linux

package main

import (
	"fmt"
	"io"
	"os"
)

func pinPlatformLaunchSeams() {
	hasSystemd = func() bool { return false }
	queryUserLinger = func(int) (bool, error) {
		return false, fmt.Errorf("queryUserLinger is not stubbed in this test")
	}
	runSystemdDaemon = func([]string) error {
		return fmt.Errorf("runSystemdDaemon is not stubbed in this test")
	}
	startDetachedDaemon = func(string, *os.File) error {
		return fmt.Errorf("startDetachedDaemon is not stubbed in this test")
	}
	systemdRunCombinedOutput = func(string, ...string) ([]byte, error) {
		return nil, fmt.Errorf("systemdRunCombinedOutput is not stubbed in this test")
	}
	daemonLaunchStderr = io.Discard
}
