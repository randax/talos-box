//go:build !linux && !darwin

package main

import (
	"fmt"
	"os"
	"runtime"
)

func launchDaemonLive(string, *os.File) error {
	return fmt.Errorf("automatic tbxd launch is unsupported on %s", runtime.GOOS)
}
