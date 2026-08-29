//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func launchDaemonLive(daemonPath string, logFile *os.File) error {
	command := exec.Command(daemonPath)
	command.Stdout = logFile
	command.Stderr = logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return fmt.Errorf("start %s: %w", daemonPath, err)
	}
	if err := command.Process.Release(); err != nil {
		return fmt.Errorf("detach tbxd: %w", err)
	}

	return nil
}
