//go:build linux

package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const fallbackDaemonUnitPrefix = "talos-box-tbxd-fallback-"

var (
	hasSystemd                         = hasSystemdLive
	queryUserLinger                    = queryUserLingerLive
	runSystemdDaemon                   = runSystemdDaemonLive
	startDetachedDaemon                = startDetachedDaemonLive
	systemdRunCombinedOutput           = systemdRunCombinedOutputLive
	daemonLaunchStderr       io.Writer = os.Stderr
)

func launchDaemonLive(daemonPath string, logFile *os.File) error {
	if !hasSystemd() {
		return startDetachedDaemon(daemonPath, logFile)
	}
	if !filepath.IsAbs(daemonPath) {
		return fmt.Errorf("start tbxd with systemd-run: daemon path %q is not absolute", daemonPath)
	}

	linger, err := queryUserLinger(os.Getuid())
	warnLinger := err != nil || !linger
	pid := os.Getpid()
	if err := runSystemdDaemon(transientDaemonArgs(daemonPath, pid, os.Getenv)); err != nil {
		return fmt.Errorf("start tbxd with systemd-run: %w; inspect the transient unit with: %s; enable the packaged socket with: systemctl --user enable --now tbxd.socket", err, fallbackDaemonJournalctl(pid))
	}
	if warnLinger {
		_, _ = fmt.Fprintln(daemonLaunchStderr, "warning: tbxd fallback guests will stop at logout because user lingering is not enabled or could not be verified; run loginctl enable-linger, or install the packaged tbxd.socket unit")
	}

	return nil
}

func hasSystemdLive() bool {
	info, err := os.Stat("/run/systemd/system")

	return err == nil && info.IsDir()
}

func queryUserLingerLive(uid int) (bool, error) {
	output, err := exec.Command(
		"loginctl",
		"show-user",
		strconv.Itoa(uid),
		"--property=Linger",
		"--value",
	).Output()
	if err != nil {
		return false, fmt.Errorf("query user lingering: %w", err)
	}

	return parseUserLinger(output), nil
}

func parseUserLinger(output []byte) bool {
	return strings.TrimSpace(string(output)) == "yes"
}

func transientDaemonArgs(daemonPath string, pid int, getenv func(string) string) []string {
	args := []string{
		"--user",
		"--unit=" + fallbackDaemonUnitName(pid),
		"--collect",
		"--service-type=exec",
		"--slice=app.slice",
		"--quiet",
	}
	for _, name := range []string{
		"PATH",
		"TBX_HELPER_SOCKET",
		"TBX_BALLOON_RESERVE_MIB",
		"TBXD_K8S_WARNINGS",
	} {
		if value := getenv(name); value != "" {
			args = append(args, "--setenv="+name+"="+value)
		}
	}

	return append(args, "--", daemonPath)
}

func runSystemdDaemonLive(args []string) error {
	output, err := systemdRunCombinedOutput("systemd-run", args...)
	if err == nil {
		return nil
	}
	stderr := strings.TrimSpace(string(output))
	if stderr == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, stderr)
}

func systemdRunCombinedOutputLive(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

func fallbackDaemonUnitName(pid int) string {
	return fallbackDaemonUnitPrefix + strconv.Itoa(pid) + ".service"
}

func fallbackDaemonJournalctl(pid int) string {
	return "journalctl --user -u " + fallbackDaemonUnitName(pid)
}

func daemonSpawnFailureHint() string {
	if !hasSystemd() {
		return ""
	}
	return "; under systemd fallback also inspect " + fallbackDaemonJournalctl(os.Getpid())
}

func startDetachedDaemonLive(daemonPath string, logFile *os.File) error {
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
