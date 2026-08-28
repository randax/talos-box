//go:build linux

package main

import (
	"context"
	"errors"
	"os/exec"
	"strings"

	"github.com/randax/talos-box/internal/daemon"
	"github.com/randax/talos-box/internal/helper"
)

const (
	runtimeIdentityDaemonServiceUnit = "tbxd.service"
	runtimeIdentityHelperServiceUnit = "tbx-helper.service"
)

func runtimeIdentityPlatformDeps(deps *runtimeIdentityDeps) {
	if deps == nil {
		return
	}
	if deps.command == nil {
		deps.command = execCombinedOutput
	}
	if deps.helperConfiguredPath == nil && deps.helperProbe == nil {
		deps.helperConfiguredPath = func() configuredComponentPath {
			return linuxHelperConfiguredPath(deps.command)
		}
	}
	if deps.daemonProbe == nil && deps.daemonDial != nil {
		deps.daemonProbe = linuxActivationSafeDaemonProbe(deps.command, deps.daemonDial)
	}
	if deps.helperProbe == nil && deps.helperDial != nil {
		deps.helperProbe = linuxActivationSafeHelperProbe(deps.command, deps.helperDial, deps.helperConfiguredPath)
	}
}

func linuxActivationSafeDaemonProbe(command commandOutput, dial func(context.Context) (daemon.Info, int, error)) func(context.Context) (daemon.Info, int, error) {
	return func(ctx context.Context) (daemon.Info, int, error) {
		active, known := linuxSystemdServiceActive(command, true, runtimeIdentityDaemonServiceUnit)
		if known && !active {
			return daemon.Info{}, 0, runtimeIdentityInactiveError{detail: "not running (" + runtimeIdentityDaemonServiceUnit + " inactive; socket-activated)"}
		}
		return dial(ctx)
	}
}

func linuxActivationSafeHelperProbe(command commandOutput, dial func(context.Context) (helper.Info, error), configuredPath func() configuredComponentPath) func(context.Context) (helper.Info, error) {
	return func(ctx context.Context) (helper.Info, error) {
		active, known := linuxSystemdServiceActive(command, false, runtimeIdentityHelperServiceUnit)
		if known && !active {
			detail := "installed, inactive (" + runtimeIdentityHelperServiceUnit + "; socket-activated)"
			if configured := configuredPath(); configured.Path != "" {
				detail = "installed, inactive (" + runtimeIdentityHelperServiceUnit + "; socket-activated; configured path from systemd: " + configured.Path + ")"
			}
			return helper.Info{}, runtimeIdentityInactiveError{detail: detail}
		}
		return dial(ctx)
	}
}

func linuxSystemdServiceActive(command commandOutput, user bool, unit string) (active bool, known bool) {
	if command == nil {
		return false, false
	}
	args := []string{"is-active", unit}
	if user {
		args = append([]string{"--user"}, args...)
	}
	output, err := command("systemctl", args...)
	if err == nil {
		return strings.TrimSpace(string(output)) == "active", true
	}
	if errors.Is(err, exec.ErrNotFound) || looksNonSystemdHost(output, err) {
		return false, false
	}
	return strings.TrimSpace(string(output)) == "active", true
}

func looksNonSystemdHost(output []byte, err error) bool {
	text := strings.TrimSpace(string(output))
	if text == "" && err != nil {
		text = err.Error()
	}
	return strings.Contains(text, "System has not been booted with systemd") ||
		strings.Contains(text, "Failed to connect to bus")
}

func linuxHelperConfiguredPath(command commandOutput) configuredComponentPath {
	if command == nil {
		return configuredComponentPath{}
	}
	data, err := command("systemctl", "cat", runtimeIdentityHelperServiceUnit)
	if err != nil {
		return configuredComponentPath{}
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ExecStart=") {
			continue
		}
		execStart := strings.TrimSpace(strings.TrimPrefix(line, "ExecStart="))
		if execStart == "" {
			return configuredComponentPath{}
		}
		fields := strings.Fields(execStart)
		if len(fields) == 0 {
			return configuredComponentPath{}
		}
		return configuredComponentPath{Path: fields[0], Source: "systemd"}
	}
	return configuredComponentPath{}
}
