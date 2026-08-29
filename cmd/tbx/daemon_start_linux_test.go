//go:build linux

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestTransientDaemonArgsUseUserServiceNotScope(t *testing.T) {
	daemonPath := "/opt/talos-box/bin/tbxd"
	args := transientDaemonArgs(daemonPath, 4242, func(string) string { return "" })

	for _, want := range []string{
		"--user",
		"--unit=talos-box-tbxd-fallback-4242.service",
		"--collect",
		"--service-type=exec",
		"--slice=app.slice",
		"--quiet",
		"--",
		daemonPath,
	} {
		if !slices.Contains(args, want) {
			t.Errorf("transientDaemonArgs() = %q, missing %q", args, want)
		}
	}
	for _, forbidden := range []string{"--scope", "--pipe", "--wait", "sh", "bash", "zsh"} {
		if slices.Contains(args, forbidden) {
			t.Errorf("transientDaemonArgs() = %q, contains forbidden argument %q", args, forbidden)
		}
	}
	if !filepath.IsAbs(args[len(args)-1]) {
		t.Errorf("transientDaemonArgs() daemon path = %q, want absolute path", args[len(args)-1])
	}
}

func TestTransientDaemonArgsForwardOnlyDaemonEnvironment(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want []string
	}{
		{name: "unset", env: map[string]string{}},
		{
			name: "set",
			env: map[string]string{
				"PATH":                    "/custom/bin:/usr/bin",
				"TBX_HELPER_SOCKET":       "/run/user/1000/helper.sock",
				"TBX_BALLOON_RESERVE_MIB": "4096",
				"TBXD_K8S_WARNINGS":       "1",
				"AWS_SECRET_ACCESS_KEY":   "do-not-forward",
			},
			want: []string{
				"--setenv=PATH=/custom/bin:/usr/bin",
				"--setenv=TBX_HELPER_SOCKET=/run/user/1000/helper.sock",
				"--setenv=TBX_BALLOON_RESERVE_MIB=4096",
				"--setenv=TBXD_K8S_WARNINGS=1",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			args := transientDaemonArgs("/opt/talos-box/bin/tbxd", 99, func(name string) string {
				return test.env[name]
			})
			for _, want := range test.want {
				if !slices.Contains(args, want) {
					t.Errorf("transientDaemonArgs() = %q, missing %q", args, want)
				}
			}
			joined := strings.Join(args, " ")
			if strings.Contains(joined, "AWS_SECRET_ACCESS_KEY") || strings.Contains(joined, "do-not-forward") {
				t.Errorf("transientDaemonArgs() = %q, forwarded unrelated environment", args)
			}
			if test.name == "unset" && strings.Contains(joined, "--setenv=") {
				t.Errorf("transientDaemonArgs() = %q, forwarded an unset variable", args)
			}
		})
	}
}

func TestLinuxDaemonLaunchWarnsAndStillLaunchesWithoutLinger(t *testing.T) {
	state := pinLinuxDaemonLaunchTest(t)
	hasSystemd = func() bool { return true }
	queryUserLinger = func(int) (bool, error) { return false, nil }

	if err := launchDaemonLive("/opt/talos-box/bin/tbxd", nil); err != nil {
		t.Fatalf("launchDaemonLive() error = %v", err)
	}
	if state.systemdRuns != 1 {
		t.Fatalf("systemd launcher calls = %d, want 1", state.systemdRuns)
	}
	assertLingerWarning(t, state.stderr.String())
}

func TestLinuxDaemonLaunchWarnsWhenLingerCannotBeVerified(t *testing.T) {
	tests := []struct {
		name  string
		query func(int) (bool, error)
	}{
		{name: "command error", query: func(int) (bool, error) { return false, errors.New("loginctl unavailable") }},
		{name: "malformed output", query: func(int) (bool, error) { return false, nil }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := pinLinuxDaemonLaunchTest(t)
			hasSystemd = func() bool { return true }
			queryUserLinger = test.query

			if err := launchDaemonLive("/opt/talos-box/bin/tbxd", nil); err != nil {
				t.Fatalf("launchDaemonLive() error = %v", err)
			}
			if state.systemdRuns != 1 {
				t.Fatalf("systemd launcher calls = %d, want 1", state.systemdRuns)
			}
			assertLingerWarning(t, state.stderr.String())
		})
	}
}

func TestLinuxDaemonLaunchReportsSystemdRunFailure(t *testing.T) {
	state := pinLinuxDaemonLaunchTest(t)
	hasSystemd = func() bool { return true }
	queryUserLinger = func(int) (bool, error) { return true, nil }
	runSystemdDaemon = func([]string) error { return errors.New("user bus unavailable") }

	err := launchDaemonLive("/opt/talos-box/bin/tbxd", nil)
	if err == nil {
		t.Fatal("launchDaemonLive() error = nil, want systemd-run failure")
	}
	for _, want := range []string{"user bus unavailable", "systemctl --user enable --now tbxd.socket"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("launchDaemonLive() error = %q, missing %q", err, want)
		}
	}
	if state.stderr.Len() != 0 {
		t.Errorf("launchDaemonLive() stderr = %q, want no linger warning", state.stderr.String())
	}
}

func TestLinuxDaemonLaunchRunsTheAbsoluteSibling(t *testing.T) {
	state := pinLinuxDaemonLaunchTest(t)
	hasSystemd = func() bool { return true }
	queryUserLinger = func(int) (bool, error) { return true, nil }
	daemonPath := "/opt/talos-box/bin/tbxd"

	if err := launchDaemonLive(daemonPath, nil); err != nil {
		t.Fatalf("launchDaemonLive() error = %v", err)
	}
	separator := slices.Index(state.systemdArgs, "--")
	if separator == -1 || separator+1 != len(state.systemdArgs)-1 {
		t.Fatalf("systemd-run args = %q, want one command after --", state.systemdArgs)
	}
	if got := state.systemdArgs[separator+1]; got != daemonPath {
		t.Fatalf("systemd-run command = %q, want %q", got, daemonPath)
	}
}

func TestLinuxDaemonLaunchUsesUniqueUnitPerCallerPID(t *testing.T) {
	first := transientDaemonArgs("/opt/talos-box/bin/tbxd", 101, func(string) string { return "" })
	second := transientDaemonArgs("/opt/talos-box/bin/tbxd", 202, func(string) string { return "" })
	firstUnit := "--unit=" + fallbackDaemonUnitPrefix + strconv.Itoa(101) + ".service"
	secondUnit := "--unit=" + fallbackDaemonUnitPrefix + strconv.Itoa(202) + ".service"

	if !slices.Contains(first, firstUnit) || !slices.Contains(second, secondUnit) || firstUnit == secondUnit {
		t.Fatalf("transient units are not unique: first=%q second=%q", first, second)
	}
}

func TestLinuxDaemonLaunchFallsBackToForkWithoutSystemd(t *testing.T) {
	state := pinLinuxDaemonLaunchTest(t)
	hasSystemd = func() bool { return false }
	queryUserLinger = func(int) (bool, error) {
		t.Fatal("queryUserLinger called without systemd")

		return false, nil
	}
	runSystemdDaemon = func([]string) error {
		t.Fatal("runSystemdDaemon called without systemd")

		return nil
	}

	if err := launchDaemonLive("/opt/talos-box/bin/tbxd", nil); err != nil {
		t.Fatalf("launchDaemonLive() error = %v", err)
	}
	if state.detachedRuns != 1 {
		t.Fatalf("detached launcher calls = %d, want 1", state.detachedRuns)
	}
	if state.systemdRuns != 0 {
		t.Fatalf("systemd launcher calls = %d, want 0", state.systemdRuns)
	}
}

func TestParseUserLingerAcceptsExactlyYes(t *testing.T) {
	for _, test := range []struct {
		output string
		want   bool
	}{
		{output: "yes\n", want: true},
		{output: "no\n", want: false},
		{output: "\n", want: false},
		{output: "enabled\n", want: false},
	} {
		if got := parseUserLinger([]byte(test.output)); got != test.want {
			t.Errorf("parseUserLinger(%q) = %t, want %t", test.output, got, test.want)
		}
	}
}

type linuxDaemonLaunchState struct {
	stderr       bytes.Buffer
	systemdArgs  []string
	systemdRuns  int
	detachedRuns int
}

func pinLinuxDaemonLaunchTest(t *testing.T) *linuxDaemonLaunchState {
	t.Helper()

	originalHasSystemd := hasSystemd
	originalQueryUserLinger := queryUserLinger
	originalRunSystemdDaemon := runSystemdDaemon
	originalStartDetachedDaemon := startDetachedDaemon
	originalStderr := daemonLaunchStderr
	state := &linuxDaemonLaunchState{}
	hasSystemd = func() bool { return true }
	queryUserLinger = func(int) (bool, error) { return true, nil }
	runSystemdDaemon = func(args []string) error {
		state.systemdRuns++
		state.systemdArgs = slices.Clone(args)

		return nil
	}
	startDetachedDaemon = func(string, *os.File) error {
		state.detachedRuns++

		return nil
	}
	daemonLaunchStderr = &state.stderr
	t.Cleanup(func() {
		hasSystemd = originalHasSystemd
		queryUserLinger = originalQueryUserLinger
		runSystemdDaemon = originalRunSystemdDaemon
		startDetachedDaemon = originalStartDetachedDaemon
		daemonLaunchStderr = originalStderr
	})

	return state
}

func assertLingerWarning(t *testing.T, warning string) {
	t.Helper()

	if strings.Count(warning, "\n") != 1 {
		t.Errorf("linger warning = %q, want exactly one line", warning)
	}
	for _, want := range []string{"guests will stop at logout", "loginctl enable-linger", "tbxd.socket"} {
		if !strings.Contains(warning, want) {
			t.Errorf("linger warning = %q, missing %q", warning, want)
		}
	}
}
