//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
			"systemctl --user is-active tbxd.service": {output: []byte("inactive\n"), err: errors.New("exit status 3")},
			"systemctl is-active tbx-helper.service":  {output: []byte("inactive\n"), err: errors.New("exit status 3")},
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
			"systemctl --user is-active tbxd.service": {output: []byte("active\n")},
			"systemctl is-active tbx-helper.service":  {output: []byte("active\n")},
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
