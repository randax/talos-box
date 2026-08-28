//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
	"github.com/randax/talos-box/internal/helper"
)

type fakeRuntimeIdentityCommandResult struct {
	output []byte
	err    error
}

func fakeRuntimeIdentityCommandOutput(results map[string]fakeRuntimeIdentityCommandResult) commandOutput {
	return func(name string, args ...string) ([]byte, error) {
		key := strings.TrimSpace(name + " " + strings.Join(args, " "))
		result, ok := results[key]
		if !ok {
			return nil, fmt.Errorf("unexpected command %q", key)
		}
		return result.output, result.err
	}
}

func TestLinuxRuntimeIdentitySkipsSocketActivatedDialsWhenInactive(t *testing.T) {
	t.Parallel()

	var daemonDialed, helperDialed bool
	identity := collectRuntimeIdentity(context.Background(), runtimeIdentityDeps{
		executable: func() (string, error) { return "/opt/current/tbx", nil },
		command: fakeRuntimeIdentityCommandOutput(map[string]fakeRuntimeIdentityCommandResult{
			"systemctl --user show -p LoadState,ActiveState tbxd.service": {output: []byte("LoadState=loaded\nActiveState=inactive\n")},
			"systemctl show -p LoadState,ActiveState tbx-helper.service":  {output: []byte("LoadState=loaded\nActiveState=inactive\n")},
		}),
		daemonDial: func(context.Context) (daemon.Info, int, error) {
			daemonDialed = true
			return daemon.Info{}, 0, errors.New("dialed inactive daemon")
		},
		helperDial: func(context.Context) (helper.Info, error) {
			helperDialed = true
			return helper.Info{}, errors.New("dialed inactive helper")
		},
		helperConfiguredPath: func() configuredComponentPath {
			return configuredComponentPath{Path: "/usr/bin/tbx-helper", Source: "systemd"}
		},
	})

	if daemonDialed || helperDialed {
		t.Fatalf("inactive services still dialed: daemon=%v helper=%v", daemonDialed, helperDialed)
	}
	var output bytes.Buffer
	if err := renderRuntimeIdentity(&output, identity); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"daemon: not running (tbxd.service inactive; socket-activated)",
		"helper: installed, inactive (tbx-helper.service; socket-activated; configured path from systemd: /usr/bin/tbx-helper)",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("runtime output missing %q:\n%s", want, output.String())
		}
	}
}

func TestLinuxRuntimeIdentityDialsOnlyActiveSocketActivatedServices(t *testing.T) {
	t.Parallel()

	var daemonDialed, helperDialed bool
	identity := collectRuntimeIdentity(context.Background(), runtimeIdentityDeps{
		executable: func() (string, error) { return "/opt/current/tbx", nil },
		command: fakeRuntimeIdentityCommandOutput(map[string]fakeRuntimeIdentityCommandResult{
			"systemctl --user show -p LoadState,ActiveState tbxd.service": {output: []byte("LoadState=loaded\nActiveState=active\n")},
			"systemctl show -p LoadState,ActiveState tbx-helper.service":  {output: []byte("LoadState=loaded\nActiveState=active\n")},
		}),
		daemonDial: func(context.Context) (daemon.Info, int, error) {
			daemonDialed = true
			return daemon.Info{ProtocolVersion: daemon.ProtocolVersion, Version: "0.1.3", Executable: "/opt/current/tbxd", PID: 1234}, 1234, nil
		},
		helperDial: func(context.Context) (helper.Info, error) {
			helperDialed = true
			return helper.Info{ProtocolVersion: helper.ProtocolVersion, Version: "0.1.3", Executable: "/opt/current/tbx-helper", PID: 2345}, nil
		},
	})

	if !daemonDialed || !helperDialed {
		t.Fatalf("active services were not dialed: daemon=%v helper=%v", daemonDialed, helperDialed)
	}
	if !identity.Daemon.Available || identity.Daemon.Path != "/opt/current/tbxd" {
		t.Fatalf("daemon identity = %+v", identity.Daemon)
	}
	if !identity.Helper.Available || identity.Helper.Path != "/opt/current/tbx-helper" {
		t.Fatalf("helper identity = %+v", identity.Helper)
	}
}

func TestLinuxRuntimeIdentityDialsWhenUnitsAreNotLoadedAndReportsNotInstalled(t *testing.T) {
	// `systemctl is-active` would print "inactive" for a unit that does not
	// exist; `show` reports LoadState=not-found, which must fall through to
	// the dial so an absent helper is "not installed", never "installed,
	// inactive".
	var helperDialed bool
	deps := runtimeIdentityDeps{
		executable: func() (string, error) { return "/opt/current/tbx", nil },
		command: fakeRuntimeIdentityCommandOutput(map[string]fakeRuntimeIdentityCommandResult{
			"systemctl --user show -p LoadState,ActiveState tbxd.service": {output: []byte("LoadState=not-found\nActiveState=inactive\n")},
			"systemctl show -p LoadState,ActiveState tbx-helper.service":  {output: []byte("LoadState=not-found\nActiveState=inactive\n")},
			"systemctl cat tbx-helper.service":                            {err: errors.New("No files found for tbx-helper.service.")},
		}),
		daemonDial: func(context.Context) (daemon.Info, int, error) {
			return daemon.Info{}, 0, os.ErrNotExist
		},
		helperDial: func(context.Context) (helper.Info, error) {
			helperDialed = true
			return helper.Info{}, os.ErrNotExist
		},
	}
	runtimeIdentityPlatformDeps(&deps)
	identity := collectRuntimeIdentity(context.Background(), deps)
	if !helperDialed {
		t.Fatal("helper was not dialed although no unit is loaded")
	}
	var output bytes.Buffer
	if err := renderRuntimeIdentity(&output, identity); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); !strings.Contains(got, "helper: not installed") || strings.Contains(got, "installed, inactive") {
		t.Fatalf("runtime output = %q, want helper reported as not installed", got)
	}
}

func TestLinuxSystemdServiceActiveDistrustsUnrecognisedReplies(t *testing.T) {
	for name, result := range map[string]fakeRuntimeIdentityCommandResult{
		"command failure": {err: errors.New("Failed to connect to bus")},
		"garbage":         {output: []byte("something unexpected\n")},
		"not loaded":      {output: []byte("LoadState=not-found\nActiveState=inactive\n")},
	} {
		command := fakeRuntimeIdentityCommandOutput(map[string]fakeRuntimeIdentityCommandResult{
			"systemctl show -p LoadState,ActiveState tbx-helper.service": result,
		})
		if _, known := linuxSystemdServiceActive(command, false, runtimeIdentityHelperServiceUnit); known {
			t.Fatalf("%s: reply was trusted as an authoritative activity state", name)
		}
	}
}
