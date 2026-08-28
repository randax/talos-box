package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/randax/talos-box/internal/daemon"
	"github.com/randax/talos-box/internal/helper"
	"github.com/randax/talos-box/internal/resolverset"
)

const (
	helperLaunchdLabel = "dev.talosbox.helper"
	helperPlistPath    = "/Library/LaunchDaemons/dev.talosbox.helper.plist"
)

func (c cli) runSystem(args []string) error {
	if len(args) == 0 {
		return errors.New(groupUsages["system"])
	}
	switch args[0] {
	// restart and status manage the per-user daemon, so they never need root
	case "restart":
		force := false
		if len(args) == 2 && args[1] == "--force" {
			force = true
		} else if len(args) != 1 {
			return errors.New("usage: tbx system restart [--force]")
		}
		return c.restartDaemon(force)
	case "status":
		if len(args) != 1 {
			return errors.New("usage: tbx system status")
		}
		return c.daemonStatus()
	// `tbx logs` is the short form; both spellings read the same daemon log.
	case "logs":
		return c.runLogs(args[1:])
	case "install":
		if len(args) > 2 {
			return errors.New("usage: tbx system install [absolute-helper-path]")
		}
		if len(args) == 2 && !filepath.IsAbs(args[1]) {
			return errors.New("tbx-helper path must be absolute")
		}
	case "uninstall":
		if len(args) != 1 {
			return errors.New("usage: tbx system uninstall")
		}
	default:
		return unknownVerbError("system", args[0])
	}

	if os.Geteuid() != 0 {
		if _, err := fmt.Fprintf(c.err, "tbx system %s requires root; re-running with sudo\n", args[0]); err != nil {
			return err
		}
		return reexecSystemWithSudo(args)
	}
	if args[0] == "uninstall" {
		return c.uninstallSystem()
	}
	return c.installSystem(args[1:])
}

// restartDaemon replaces the running tbxd with one spawned from this build, so
// a protocol skew has a named recovery command instead of a pid hunt (#290).
// A restart stops every VM the daemon runs, so running clusters are named and
// the restart only proceeds with an explicit --force.
func (c cli) restartDaemon(force bool) error {
	socketPath, err := daemon.SocketPath()
	if err != nil {
		return err
	}
	info, pid, err := daemonHandshake(socketPath)
	if err != nil {
		var connectionError dialError
		if !errors.As(err, &connectionError) {
			var busy busyDaemonError
			if !errors.As(err, &busy) {
				return err
			}
			// a busy daemon still owns the socket and is still ours to
			// replace; it just could not answer daemon.info in time
			info, pid = daemon.Info{}, busy.pid
		} else {
			if _, err := spawnDaemonProcess(); err != nil {
				return err
			}
			started, startedPID, err := waitForCurrentDaemon(socketPath)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(c.out, "started tbxd (pid %d, protocol %d)\n", startedPID, started.ProtocolVersion)
			return err
		}
	}
	if err := refuseSupervisedRestart(force); err != nil {
		return err
	}
	// the cluster query runs only after the supervision and force checks, and
	// under a deadline: it is served under the daemon's operation lock, so a
	// long suspend or destroy must never be able to hang --force
	activity, activityErr := daemonClusterActivity(socketPath)
	if activityErr != nil {
		// A daemon too busy to answer cluster.list is exactly the one whose
		// shutdown takes minutes, so the stop-wait must not collapse to the
		// base timeout. On-disk state still names every configured node; the
		// count over-estimates (stopped clusters included), which only ever
		// lengthens the wait (#319).
		activity.runningVMs = onDiskVMCount()
	}
	if !force {
		if activityErr != nil {
			return fmt.Errorf("tbx could not tell whether clusters are running (%v); restarting tbxd stops every running cluster — re-run: tbx system restart --force", activityErr)
		}
		if !activity.empty() {
			return errors.New(restartRefusal(activity))
		}
	}
	// activity carries the VM count the stop-wait is scaled to; an unknown
	// activity leaves the base wait, which is the pre-#319 behaviour.
	restarted, restartedPID, err := replaceDaemon(socketPath, info, pid, activity, c.err)
	if err != nil {
		return err
	}
	switch {
	case activityErr != nil:
		if _, err := fmt.Fprintln(c.out, "stopped clusters: unknown (state query failed)"); err != nil {
			return err
		}
	case !activity.empty():
		if _, err := fmt.Fprintf(c.out, "stopped clusters: %s\n", activity.describe()); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(c.out, "restarted tbxd (pid %d, protocol %d)\n", restartedPID, restarted.ProtocolVersion)
	return err
}

// refuseSupervisedRestart decides whether tbx may replace this daemon. A
// supervisor that confirms it owns an active tbxd is never overridden. A merely
// inferred unit file — every packaged install ships one, whether or not it is
// in use — is refused without --force but yields to it, so the recovery chain
// does not dead-end on a file that proves nothing.
func refuseSupervisedRestart(force bool) error {
	state, reason := supervisedDaemon()
	if state == supervisionConfirmed || (state == supervisionInferred && !force) {
		// supervisionRefusal is shared with the protocol gate in client.go, so
		// the two callers cannot name different ways out of the same state
		return errors.New(supervisionRefusal(state, reason))
	}
	return nil
}

// daemonStatus reports the running daemon without spawning one, so an operator
// can see a protocol skew before it bites.
func (c cli) daemonStatus() error {
	identity := c.collectRuntimeIdentity(context.Background())
	runtimeBlock := identity
	runtimeBlock.Findings = nil
	if err := renderRuntimeIdentity(c.out, runtimeBlock); err != nil {
		return err
	}
	compatHint := false
	for _, finding := range identity.Findings {
		if finding.check == "runtime-compat" {
			compatHint = true
		}
		if finding.detail == "" {
			if _, err := fmt.Fprintf(c.err, "%s %s\n", finding.level, finding.check); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintf(c.err, "%s %s: %s\n", finding.level, finding.check, finding.detail); err != nil {
			return err
		}
	}
	if compatHint {
		if _, err := fmt.Fprintln(c.err, "warning: run: tbx system restart"); err != nil {
			return err
		}
	}
	return nil
}

func (c cli) installSystem(args []string) error {
	helperPath, err := resolveHelperPath(args)
	if err != nil {
		return err
	}
	if err := validateHelperBinary(helperPath); err != nil {
		return err
	}
	allowedUID, err := allowedUIDFromSudoEnv(os.LookupEnv)
	if err != nil {
		return err
	}
	if allowedUID == nil {
		if _, err := fmt.Fprintln(c.err, "warning: SUDO_UID is not set; only root will be able to use tbx-helper; re-run `sudo tbx system install` from your account to authorize it"); err != nil {
			return err
		}
	}
	plist, err := renderLaunchdPlist(helperPath, allowedUID)
	if err != nil {
		return err
	}

	if _, err := os.Stat(helperPlistPath); err == nil {
		_ = runLaunchctl("bootout", "system", helperPlistPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect helper plist: %w", err)
	}
	if err := os.WriteFile(helperPlistPath, plist, 0o644); err != nil {
		return fmt.Errorf("write helper plist: %w", err)
	}
	if err := os.Chmod(helperPlistPath, 0o644); err != nil {
		return fmt.Errorf("set helper plist permissions: %w", err)
	}
	if err := os.Chown(helperPlistPath, 0, 0); err != nil {
		return fmt.Errorf("set helper plist ownership: %w", err)
	}
	if err := runLaunchctl("bootstrap", "system", helperPlistPath); err != nil {
		return err
	}
	_, err = fmt.Fprintf(c.out, "installed %s using %s\n", helperLaunchdLabel, helperPath)
	return err
}

// dnsUninstaller is the helper surface the uninstall path needs.
type dnsUninstaller interface {
	UninstallDNS() error
	Close() error
}

func connectHelperForUninstall() (dnsUninstaller, error) {
	return helper.Connect()
}

// removeScopedResolver drops the tbx-managed resolver files while the helper
// is still loaded. An unreachable helper (never installed, or already booted
// out) falls back to removing the files directly — uninstallSystem only runs
// as root — so the uninstall promise holds even without a live helper. The
// helper and the fallback both report missing files as success, so repeated
// uninstalls stay idempotent. A cleanup failure is returned (after a stderr
// warning) so the caller can finish the teardown and still exit non-zero.
func (c cli) removeScopedResolver(connect func() (dnsUninstaller, error), fallback func() error) error {
	client, err := connect()
	if err != nil {
		return c.warnResolverFailure(fallback())
	}
	defer func() { _ = client.Close() }()
	if err := client.UninstallDNS(); err != nil {
		// The helper is about to be booted out, so this is the last chance to
		// honor the uninstall promise: retry directly as root.
		return c.warnResolverFailure(fallback())
	}
	return nil
}

func (c cli) warnResolverFailure(err error) error {
	if err == nil {
		return nil
	}
	_, printErr := fmt.Fprintf(c.err, "warning: could not remove resolver files: %v\n", err)
	return errors.Join(fmt.Errorf("remove resolver files: %w", err), printErr)
}

// removeResolverFilesDirectly is the root fallback for an unreachable helper:
// remove the shared k8s.test resolver plus any marker-managed per-domain
// files, never touching unmanaged files, then HUP mDNSResponder if anything
// changed. Missing files and a missing /etc/resolver are success.
func removeResolverFilesDirectly() error {
	removed, err := removeResolverFiles(resolverset.SharedPath)
	if removed {
		// Best effort: resolver-file pickup is undocumented; a failed HUP
		// only delays mDNSResponder noticing the removal.
		_ = exec.Command("/usr/bin/killall", "-HUP", "mDNSResponder").Run()
	}
	return err
}

func removeResolverFiles(sharedPath string) (bool, error) {
	removed := false
	var sweepErr error
	if err := os.Remove(sharedPath); err == nil {
		removed = true
	} else if !errors.Is(err, os.ErrNotExist) {
		// Keep sweeping: one stuck file must not strand the others.
		sweepErr = errors.Join(sweepErr, fmt.Errorf("remove %s: %w", sharedPath, err))
	}
	directory := filepath.Dir(sharedPath)
	entries, err := os.ReadDir(directory)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return removed, errors.Join(sweepErr, fmt.Errorf("read resolver directory: %w", err))
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		content, err := os.ReadFile(path)
		if err != nil {
			// An unreadable file cannot be classified, so it may be a managed
			// file left behind — that must fail the sweep, not pass silently.
			// A file that vanished since ReadDir is simply gone.
			if !errors.Is(err, os.ErrNotExist) {
				sweepErr = errors.Join(sweepErr, fmt.Errorf("read resolver file %s: %w", entry.Name(), err))
			}
			continue
		}
		if !resolverset.Managed(content) {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			sweepErr = errors.Join(sweepErr, fmt.Errorf("remove resolver file %s: %w", entry.Name(), err))
			continue
		}
		removed = true
	}
	return removed, sweepErr
}

func (c cli) uninstallSystem() error {
	// Ask before teardown, while the helper can still service the request. A
	// failure is carried to the end: the helper teardown still runs, and the
	// command exits non-zero because the cleanup promise was not honored.
	resolverErr := c.removeScopedResolver(connectHelperForUninstall, removeResolverFilesDirectly)
	if _, err := os.Stat(helperPlistPath); err == nil {
		if err := runLaunchctl("bootout", "system", helperPlistPath); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect helper plist: %w", err)
	} else {
		// A loaded job can outlive a manually removed plist.
		_ = runLaunchctl("bootout", "system/"+helperLaunchdLabel)
	}
	if err := os.Remove(helperPlistPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove helper plist: %w", err)
	}
	if resolverErr != nil {
		return resolverErr
	}
	_, err := fmt.Fprintln(c.out, "uninstalled "+helperLaunchdLabel)
	return err
}

func resolveHelperPath(args []string) (string, error) {
	if len(args) == 1 {
		if !filepath.IsAbs(args[0]) {
			return "", errors.New("tbx-helper path must be absolute")
		}
		return filepath.Clean(args[0]), nil
	}
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("find tbx executable: %w", err)
	}
	// Joining from the executable itself with ../ resolves to its sibling.
	return filepath.Clean(filepath.Join(executable, "..", "tbx-helper")), nil
}

func validateHelperBinary(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect tbx-helper binary: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("tbx-helper path is not a regular file: %s", path)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("tbx-helper path is not executable: %s", path)
	}
	return nil
}

func renderLaunchdPlist(helperPath string, allowedUID *uint32) ([]byte, error) {
	if !filepath.IsAbs(helperPath) {
		return nil, errors.New("tbx-helper path must be absolute")
	}
	var escaped bytes.Buffer
	if err := xml.EscapeText(&escaped, []byte(helperPath)); err != nil {
		return nil, fmt.Errorf("escape helper path: %w", err)
	}
	uidArgs := ""
	if allowedUID != nil {
		uidArgs = fmt.Sprintf("    <string>--allowed-uid</string>\n    <string>%d</string>\n", *allowedUID)
	}
	const template = "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n" +
		"<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n" +
		"<plist version=\"1.0\">\n" +
		"<dict>\n" +
		"  <key>Label</key>\n" +
		"  <string>%s</string>\n" +
		"  <key>ProgramArguments</key>\n" +
		"  <array>\n" +
		"    <string>%s</string>\n" +
		"%s" +
		"  </array>\n" +
		"  <key>RunAtLoad</key>\n" +
		"  <true/>\n" +
		"  <key>KeepAlive</key>\n" +
		"  <true/>\n" +
		"</dict>\n" +
		"</plist>\n"
	return []byte(fmt.Sprintf(template, helperLaunchdLabel, escaped.String(), uidArgs)), nil
}

// allowedUIDFromSudoEnv returns nil when SUDO_UID is absent (e.g. a direct
// root shell), which installs the helper in root-only mode.
func allowedUIDFromSudoEnv(lookupEnv func(string) (string, bool)) (*uint32, error) {
	value, ok := lookupEnv("SUDO_UID")
	if !ok || value == "" {
		return nil, nil
	}
	uid, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("invalid SUDO_UID %q; run `sudo tbx system install` from your own account: %w", value, err)
	}
	allowed := uint32(uid)
	return &allowed, nil
}

func reexecSystemWithSudo(args []string) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find tbx executable: %w", err)
	}
	commandArgs := append([]string{executable, "system"}, args...)
	command := exec.Command("sudo", commandArgs...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("run sudo: %w", err)
	}
	return nil
}

func runLaunchctl(args ...string) error {
	output, err := exec.Command("/bin/launchctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}
