//go:build linux

package main

import (
	"context"
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

// linuxSystemdServiceActive answers whether a socket-activated unit's service
// is running right now. It uses `systemctl show` rather than `is-active`
// because the latter prints "inactive" (exit 3) for a unit that does not
// exist at all, which would render an absent helper as "installed, inactive".
// known is false when the answer cannot be trusted — no systemd, no such
// unit, or an unrecognised reply — and the caller then dials: with no unit
// loaded there is nothing a dial could activate.
func linuxSystemdServiceActive(command commandOutput, user bool, unit string) (active bool, known bool) {
	if command == nil {
		return false, false
	}
	args := []string{"show", "-p", "LoadState,ActiveState", unit}
	if user {
		args = append([]string{"--user"}, args...)
	}
	output, err := command("systemctl", args...)
	if err != nil {
		return false, false
	}
	properties := map[string]string{}
	for _, line := range strings.Split(string(output), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			properties[key] = value
		}
	}
	if properties["LoadState"] != "loaded" {
		return false, false
	}
	switch properties["ActiveState"] {
	case "active":
		return true, true
	case "inactive", "activating", "deactivating", "failed", "reloading":
		return false, true
	}
	return false, false
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
