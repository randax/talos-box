package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/daemon"
)

type dialError struct{ err error }

func (e dialError) Error() string { return e.err.Error() }

const (
	legacyProvisioningIntentProtocolVersion = 1
	csiProvisioningIntentProtocolVersion    = 2
	cacheWarmProtocolVersion                = 3
	nodeRemoveGateProtocolVersion           = 4
	perClusterTalosProtocolVersion          = 5
	snapshotRestoreGateProtocolVersion      = 6
	snapshotCreateWarningProtocolVersion    = 7
	nodeRunStateProtocolVersion             = 8
	bgpReconcileProtocolVersion             = 10
	bgpStatusProtocolVersion                = 14
)

func requiresProvisioningIntentHandshake(input cluster.ProvisioningIntentInput) bool {
	return input.CNI != "" || input.CSI != "" || input.LB != nil || input.BGP != nil || input.Hubble != nil
}

func minimumProvisioningIntentProtocol(input cluster.ProvisioningIntentInput) int {
	if input.CSI != "" {
		return csiProvisioningIntentProtocolVersion
	}
	return legacyProvisioningIntentProtocolVersion
}

// ensureProtocolAtLeast performs the daemon.info handshake and refuses to
// proceed when either side is too old for the named feature.
func (c cli) ensureProtocolAtLeast(minimum int, feature string) error {
	var info daemon.Info
	if err := c.call("daemon.info", struct{}{}, &info); err != nil {
		if strings.Contains(err.Error(), "unknown operation") {
			return fmt.Errorf("tbxd is too old to use %s; run: tbx system restart (add --force if it refuses; on a supervised install restart the tbxd service instead)", feature)
		}
		return err
	}
	if info.ProtocolVersion < minimum {
		return fmt.Errorf("tbxd protocol %d is too old to use %s; run: tbx system restart (add --force if it refuses; on a supervised install restart the tbxd service instead)", info.ProtocolVersion, feature)
	}
	if info.ProtocolVersion > daemon.ProtocolVersion {
		return fmt.Errorf("tbx is too old: protocol %d does not support tbxd protocol %d; upgrade tbx to use %s", daemon.ProtocolVersion, info.ProtocolVersion, feature)
	}
	return nil
}

func (c cli) ensureProvisioningIntentSupport(input cluster.ProvisioningIntentInput) error {
	if !requiresProvisioningIntentHandshake(input) {
		return nil
	}
	return c.ensureProtocolAtLeast(minimumProvisioningIntentProtocol(input), "--cni/--csi/--hubble/--lb/--bgp")
}

// ensureNodeRemoveSupport refuses to send node.remove to a daemon that would
// ignore its force field and delete the node's disk ungated.
func (c cli) ensureNodeRemoveSupport() error {
	return c.ensureProtocolAtLeast(nodeRemoveGateProtocolVersion, "node remove")
}

// ensureNodeRunStateSupport refuses to send node.start/node.stop to a daemon
// that does not serve them; an older daemon answers "unknown operation", which
// says nothing about how to fix it.
func (c cli) ensureNodeRunStateSupport(verb string) error {
	return c.ensureProtocolAtLeast(nodeRunStateProtocolVersion, "node "+verb)
}

// ensureSnapshotRestoreSupport refuses to send snapshot.restore to a daemon
// that would ignore its force field, delete the nodes the snapshot never
// captured ungated, and answer with the pre-gate response shape.
func (c cli) ensureSnapshotRestoreSupport() error {
	return c.ensureProtocolAtLeast(snapshotRestoreGateProtocolVersion, "snapshot restore")
}

// ensureSnapshotCreateSupport refuses to send snapshot.create to a daemon that
// answers with the bare snapshot list, dropping the restart's warning.
func (c cli) ensureSnapshotCreateSupport() error {
	return c.ensureProtocolAtLeast(snapshotCreateWarningProtocolVersion, "snapshot create")
}

// ensurePerClusterTalosSupport refuses to send an up request with per-cluster
// talos settings to a daemon that would silently apply the file-level
// version/schematic to every created cluster and drop extensions entirely.
func (c cli) ensurePerClusterTalosSupport() error {
	return c.ensureProtocolAtLeast(perClusterTalosProtocolVersion, "per-cluster talos settings")
}

// ensureBGPReconcileSupport refuses to send a mode change to a daemon that only
// moves the host speaker: it would report success while Cilium keeps announcing
// the old way, with the BGP control plane disabled and its CRDs absent (#344).
func (c cli) ensureBGPReconcileSupport(verb string) error {
	return c.ensureProtocolAtLeast(bgpReconcileProtocolVersion, "bgp "+verb)
}

// ensureBGPStatusSupport refuses to ask a daemon that has no bgp.status for
// one: it would answer with an unknown-operation error naming an internal op.
func (c cli) ensureBGPStatusSupport() error {
	return c.ensureProtocolAtLeast(bgpStatusProtocolVersion, "bgp status")
}

func (c cli) ensureCacheWarmSupport() error {
	return c.ensureProtocolAtLeast(cacheWarmProtocolVersion, "cache warm/check")
}

// daemonSession memoizes the connect-time protocol handshake for one tbx
// process, so the gate costs a single extra exchange no matter how many verbs
// a command sends. A cli without a session skips the gate, which is how tests
// drive call() against a scripted daemon.
//
// Only definitive outcomes are memoized. A transient handshake failure (a
// draining daemon's EOF, a busy daemon that cannot answer within the handshake
// deadline) must not freeze the whole process into an error call() would
// otherwise have retried past, so it skips the gate for that attempt instead —
// unless the caller settles the session (see resolveDaemonProtocol), which
// records the skip so a long call cannot re-handshake underneath itself.
type daemonSession struct {
	mu       sync.Mutex
	resolved bool
	err      error
}

func newDaemonSession() *daemonSession { return &daemonSession{} }

// transientDaemonError marks a handshake outcome that says nothing about the
// daemon's protocol: the gate is skipped for this attempt and not remembered.
type transientDaemonError struct{ err error }

func (e transientDaemonError) Error() string { return e.err.Error() }
func (e transientDaemonError) Unwrap() error { return e.err }

// busyDaemonError marks a daemon that owns the socket but did not answer
// daemon.info within the handshake deadline — daemon.info is served under the
// daemon's operation lock, so a long suspend or destroy blocks it.
type busyDaemonError struct {
	pid int
	err error
}

func (e busyDaemonError) Error() string {
	if e.pid > 0 {
		return fmt.Sprintf("tbxd (pid %d) is busy: %v", e.pid, e.err)
	}
	return fmt.Sprintf("tbxd is busy: %v", e.err)
}

func (e busyDaemonError) Unwrap() error { return e.err }

// ensureDaemonProtocol gates every verb on the daemon speaking this CLI's
// protocol: a skew that only surfaces on a gated verb lets a stale daemon serve
// cluster lifecycle all session (#290). A stale daemon that tbx owns is
// restarted in place; anything else fails with a recovery command.
func (c cli) ensureDaemonProtocol() error { return c.resolveDaemonProtocol(false) }

// resolveDaemonProtocol runs the gate. A transient outcome is retried once with
// a longer deadline before the gate is skipped, so a briefly busy daemon does
// not quietly re-open the skew hole (#290), and the skip is always announced.
//
// settle is set by callers that must not hand the verb a second handshake: a
// long call under a heartbeat would otherwise re-handshake from call() while
// the ticker writes to stderr. Those callers remember the skip for the process.
func (c cli) resolveDaemonProtocol(settle bool) error {
	if c.daemon == nil {
		return nil
	}
	c.daemon.mu.Lock()
	defer c.daemon.mu.Unlock()
	if c.daemon.resolved {
		return c.daemon.err
	}
	err := c.handshakeDaemon(daemonHandshakeTimeout)
	var transient transientDaemonError
	if errors.As(err, &transient) {
		err = c.handshakeDaemon(daemonHandshakeRetryTimeout)
	}
	if errors.As(err, &transient) {
		// nothing was learned about the daemon's protocol, so the gate is
		// skipped for this verb — say so rather than silently proceeding
		if _, noticeErr := fmt.Fprintf(c.err, "%s (%v)\n", unverifiedProtocolNotice, err); noticeErr != nil {
			return noticeErr
		}
		if settle {
			c.daemon.resolved, c.daemon.err = true, nil
		}
		return nil
	}
	c.daemon.resolved, c.daemon.err = true, err
	return err
}

// unverifiedProtocolNotice is printed whenever the protocol gate is skipped, so
// an unverified daemon is never a silent condition.
const unverifiedProtocolNotice = "warning: tbxd busy; protocol not verified for this command"

func (c cli) handshakeDaemon(timeout time.Duration) error {
	socketPath, err := daemon.SocketPath()
	if err != nil {
		return err
	}
	info, pid, err := daemonHandshakeWithin(socketPath, timeout)
	if err != nil {
		var connectionError dialError
		if errors.As(err, &connectionError) {
			// no daemon is running; call spawns one from this build
			return nil
		}
		return transientDaemonError{err: err}
	}
	switch {
	case info.ProtocolVersion == daemon.ProtocolVersion:
		return nil
	case info.ProtocolVersion > daemon.ProtocolVersion:
		return fmt.Errorf("tbxd protocol %d is newer than tbx protocol %d; upgrade tbx", info.ProtocolVersion, daemon.ProtocolVersion)
	}
	skew := fmt.Sprintf("tbxd protocol %d is older than tbx protocol %d", info.ProtocolVersion, daemon.ProtocolVersion)
	if state, reason := supervisedDaemon(); state != supervisionNone {
		// supervisionRefusal is shared with tbx system restart, so the gate can
		// never name a way out the restart itself refuses (or the other way
		// round)
		return fmt.Errorf("%s and could not be restarted: %s", skew, supervisionRefusal(state, reason))
	}
	// restarting tbxd stops every VM it runs, and suspended memory does not
	// survive it — never do that behind a read-only verb's back (#290)
	activity, activityErr := daemonClusterActivity(socketPath)
	if activityErr != nil {
		return fmt.Errorf("%s and tbx could not tell whether clusters are running (%v); run: tbx system restart --force to restart anyway", skew, activityErr)
	}
	if !activity.empty() {
		return fmt.Errorf("%s, and %s", skew, restartRefusal(activity))
	}
	if _, _, err := replaceDaemon(socketPath, info, pid, activity, c.err); err != nil {
		return fmt.Errorf("%s and could not be restarted (%w); run: tbx system restart", skew, err)
	}
	_, err = fmt.Fprintf(c.err, "restarted stale tbxd (protocol %d < %d)\n", info.ProtocolVersion, daemon.ProtocolVersion)
	return err
}

// daemonHandshake reports the running daemon's protocol and pid. The pid comes
// from the socket peer rather than the response, so a daemon too old to answer
// daemon.info at all can still be identified and replaced.
func daemonHandshake(socketPath string) (daemon.Info, int, error) {
	return daemonHandshakeWithin(socketPath, daemonHandshakeTimeout)
}

func daemonHandshakeWithin(socketPath string, timeout time.Duration) (daemon.Info, int, error) {
	connection, err := net.DialTimeout("unix", socketPath, 250*time.Millisecond)
	if err != nil {
		return daemon.Info{}, 0, dialError{err: err}
	}
	defer func() { _ = connection.Close() }()
	pid, pidErr := daemon.PeerPID(connection)
	if pidErr != nil {
		pid = 0
	}
	// daemon.info is served under the daemon's operation lock, so a running
	// suspend or destroy would block the gate forever without a deadline
	if err := connection.SetDeadline(time.Now().Add(timeout)); err != nil {
		return daemon.Info{}, pid, fmt.Errorf("set daemon handshake deadline: %w", err)
	}

	request := daemon.Request{Op: "daemon.info", Args: json.RawMessage(`{}`)}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return daemon.Info{}, pid, handshakeIOError(pid, "write daemon request", err)
	}
	var response daemon.Response
	if err := json.NewDecoder(connection).Decode(&response); err != nil {
		return daemon.Info{}, pid, handshakeIOError(pid, "read daemon response", err)
	}
	if !response.OK {
		if strings.Contains(response.Error, "unknown operation") {
			return daemon.Info{}, pid, nil
		}
		return daemon.Info{}, pid, errors.New(response.Error)
	}
	var info daemon.Info
	if len(response.Data) > 0 {
		if err := json.Unmarshal(response.Data, &info); err != nil {
			return daemon.Info{}, pid, fmt.Errorf("decode daemon result: %w", err)
		}
	}
	return info, pid, nil
}

// handshakeIOError classifies a handshake exchange failure. A deadline expiry
// means the daemon is alive but busy under its operation lock, which is not a
// protocol answer.
func handshakeIOError(pid int, stage string, err error) error {
	if isTimeout(err) {
		return busyDaemonError{pid: pid, err: err}
	}
	return fmt.Errorf("%s: %w", stage, err)
}

func isTimeout(err error) bool {
	return os.IsTimeout(err) || errors.Is(err, os.ErrDeadlineExceeded)
}

// clusterActivity names the clusters a tbxd restart would disturb. Suspended
// clusters are kept apart from running ones because they lose more than power:
// their saved memory does not survive the daemon that wrote it.
type clusterActivity struct {
	running   []string
	suspended []string
	// runningVMs is how many VMs the daemon has to power off before it can
	// exit. Stopping them is what makes a restart slow, so it is what the
	// stop-wait is scaled against (#319).
	runningVMs int
}

func (a clusterActivity) empty() bool { return len(a.running)+len(a.suspended) == 0 }

func (a clusterActivity) describe() string {
	var parts []string
	if len(a.running) > 0 {
		parts = append(parts, strings.Join(a.running, ", "))
	}
	if len(a.suspended) > 0 {
		parts = append(parts, "suspended: "+strings.Join(a.suspended, ", ")+
			" (suspended clusters lose their saved memory)")
	}
	return strings.Join(parts, "; ")
}

// suspendedRecovery names the commands that clear a suspension. It matters most
// when the suspension is not real: a .vzstate orphaned by a crashed daemon reads
// exactly like live saved memory, and without a named way out the disk scan
// dead-ends every automatic restart forever (#284 review round 3).
func (a clusterActivity) suspendedRecovery() string {
	if len(a.suspended) == 0 {
		return ""
	}
	resumes := make([]string, 0, len(a.suspended))
	for _, name := range a.suspended {
		resumes = append(resumes, "tbx cluster resume "+name)
	}
	return "clear the saved state with: " + strings.Join(resumes, ", ") +
		" (or tbx cluster destroy <name> to discard it)"
}

// restartRefusal is the one refusal both the protocol gate and `tbx system
// restart` print when a restart would disturb clusters: it names what was
// found, how to clear a suspension, and the force that proceeds regardless.
func restartRefusal(activity clusterActivity) string {
	message := "restarting tbxd stops these clusters: " + activity.describe()
	if recovery := activity.suspendedRecovery(); recovery != "" {
		message += "; " + recovery
	}
	return message + "; re-run: tbx system restart --force to restart anyway"
}

// supervisionRefusal is the one refusal both the protocol gate and `tbx system
// restart` print for a supervised daemon, so the two can never name different
// ways out (#290 review round 3).
//
// A confirmed supervisor owns the process at any force level, so only its own
// restart command helps. A merely inferred unit file may be an orphan whose
// supervisor is absent — that command may then not exist at all — so the escape
// that always works, `tbx system restart --force`, is named first, with the
// supervisor's command as the alternative for when a supervisor really owns it.
func supervisionRefusal(state supervision, reason string) string {
	switch state {
	case supervisionConfirmed:
		return fmt.Sprintf("%s; tbx will not restart a supervised tbxd — run: %s", reason, supervisorRestartCommand())
	case supervisionInferred:
		return fmt.Sprintf("%s; re-run: tbx system restart --force to replace it in place, or restart it through its supervisor: %s",
			reason, supervisorRestartCommand())
	default:
		return ""
	}
}

// addSuspended merges names the daemon did not report, skipping anything
// already named.
func (a *clusterActivity) addSuspended(names ...string) {
	for _, name := range names {
		if slices.Contains(a.running, name) || slices.Contains(a.suspended, name) {
			continue
		}
		a.suspended = append(a.suspended, name)
	}
}

// daemonClusterActivity asks the daemon what it is running, then adds any
// suspended clusters found on disk.
//
// The disk scan is not redundant: ClusterSummary.Suspended is newer than the
// daemons this gate exists for, so a stale daemon answers cluster.list without
// it and suspension is invisible over that protocol. Every caller here is
// deciding whether to restart a daemon, so an unreported suspension would cost
// the operator the saved memory outright.
func daemonClusterActivity(socketPath string) (clusterActivity, error) {
	activity, err := runningClustersQuery(socketPath)
	if err != nil {
		return activity, err
	}
	suspended, savedErr := savedStateClustersQuery()
	if savedErr != nil {
		// An unreadable scan means suspension state is unknown; treating it
		// as "none" would risk exactly the saved-memory loss described above.
		return activity, fmt.Errorf("scan for suspended clusters: %w", savedErr)
	}
	activity.addSuspended(suspended...)
	return activity, nil
}

// runningClusters reports the clusters the running daemon still has VMs for,
// plus the ones it reports as suspended. It speaks cluster.list, which every
// protocol tbx has ever shipped serves, and decodes only the fields it needs so
// an older result shape still answers.
//
// The exchange is deadlined: cluster.list is served under the daemon's
// operation lock, so a long suspend or destroy would otherwise block every
// caller — including `tbx system restart` — forever.
func runningClusters(socketPath string) (clusterActivity, error) {
	response, err := exchangeWithin(socketPath, "cluster.list", struct{}{}, daemonHandshakeTimeout)
	if err != nil {
		if isTimeout(err) {
			return clusterActivity{}, fmt.Errorf("tbxd did not answer cluster.list within %s", daemonHandshakeTimeout)
		}
		return clusterActivity{}, err
	}
	if !response.OK {
		if response.Error == "" {
			return clusterActivity{}, errors.New("cluster.list failed")
		}
		return clusterActivity{}, errors.New(response.Error)
	}
	var summaries []struct {
		Name          string `json:"name"`
		Running       bool   `json:"running"`
		Suspended     bool   `json:"suspended"`
		ControlPlanes int    `json:"controlPlanes"`
		Workers       int    `json:"workers"`
	}
	if len(response.Data) > 0 {
		if err := json.Unmarshal(response.Data, &summaries); err != nil {
			return clusterActivity{}, fmt.Errorf("decode cluster list: %w", err)
		}
	}
	var activity clusterActivity
	for _, summary := range summaries {
		switch {
		case summary.Running:
			activity.running = append(activity.running, summary.Name)
			activity.runningVMs += summary.ControlPlanes + summary.Workers
		case summary.Suspended:
			activity.suspended = append(activity.suspended, summary.Name)
		}
	}
	return activity, nil
}

// savedStateClusters names clusters with suspended VM memory on disk, which is
// the only suspension signal that survives both a daemon restart and a daemon
// too old to report it.
func savedStateClusters() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("find home directory: %w", err)
	}
	root := filepath.Join(home, ".talosbox", "clusters")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read cluster directory: %w", err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		matches, err := filepath.Glob(filepath.Join(root, entry.Name(), "*"+savedStateSuffix))
		if err == nil && len(matches) > 0 {
			names = append(names, entry.Name())
		}
	}
	return names, nil
}

// savedStateSuffix mirrors the daemon's suspend artifacts.
const savedStateSuffix = ".vzstate"

// onDiskVMEstimate counts every configured node of every cluster on disk. It
// answers when the daemon is too busy to serve cluster.list — the restarts
// whose shutdown is longest — and deliberately over-counts: nodes of stopped
// clusters inflate the wait budget, never shrink it.
func onDiskVMEstimate() int {
	items, err := cluster.List()
	if err != nil {
		return 0
	}
	total := 0
	for _, item := range items {
		total += len(item.Nodes)
	}
	return total
}

// process-level seams: the restart path signals, inspects and spawns real
// processes, which tests replace with a scripted daemon.
var (
	terminateDaemonProcess  = func(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }
	spawnDaemonProcess      = startDaemon
	supervisedDaemon        = supervisedDaemonUnit
	daemonProcessAlive      = processAlive
	daemonLockFree          = socketLockFree
	daemonHandshakeProbe    = daemonHandshake
	onDiskVMCount           = onDiskVMEstimate
	runningClustersQuery    = runningClusters
	savedStateClustersQuery = savedStateClusters
	runSupervisorCommand    = func(name string, args ...string) error {
		return exec.Command(name, args...).Run()
	}
)

// processAlive reports whether a pid still names a live process. A signal 0
// that is refused (EPERM) still proves the process exists.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// supervision grades how sure tbx is that a service manager owns tbxd.
type supervision int

const (
	supervisionNone supervision = iota
	// supervisionInferred: a unit file is installed, but no supervisor
	// confirmed it is running tbxd. Every packaged Linux/Nix install ships
	// that file, so on its own it must not dead-end the recovery chain —
	// otherwise the gate says "run tbx system restart" and the restart
	// refuses, leaving no way forward.
	supervisionInferred
	// supervisionConfirmed: the supervisor itself answered that it owns an
	// active tbxd unit. That daemon is never tbx's to kill.
	supervisionConfirmed
)

// supervisedDaemonUnit reports whether a service manager owns tbxd. Such a
// daemon is not tbx's to replace: the supervisor decides which binary comes
// back — and under systemd socket activation the supervisor, not tbxd, owns
// the socket — so tbx reports instead of killing it.
//
// The supervisor itself is asked first, because a unit can live anywhere; the
// installed unit paths are only a weaker, inferred fallback for when the
// supervisor CLI is missing or unusable.
func supervisedDaemonUnit() (supervision, string) {
	if supervised, reason := supervisorOwnsDaemon(); supervised {
		return supervisionConfirmed, reason
	}
	for _, path := range supervisionUnitPaths() {
		if _, err := os.Stat(path); err == nil {
			return supervisionInferred, "tbxd may be managed by " + path +
				" (no supervisor confirmed it is running tbxd)"
		}
	}
	return supervisionNone, ""
}

// supervisorRestartCommand is the command an operator can actually run to
// restart a supervised tbxd. `tbx system restart` is not that command: it
// refuses a supervised daemon by design.
func supervisorRestartCommand() string {
	switch runtime.GOOS {
	case "linux":
		return "systemctl --user restart tbxd.service"
	case "darwin":
		return fmt.Sprintf("launchctl kickstart -k gui/%d/%s", os.Getuid(), daemonLaunchdLabel)
	}
	return "tbx system restart"
}

// supervisorOwnsDaemon asks the platform service manager whether it is running
// tbxd. A missing or failing supervisor CLI answers "unknown", which falls
// through to the unit-path scan.
func supervisorOwnsDaemon() (bool, string) {
	switch runtime.GOOS {
	case "linux":
		for _, unit := range []string{"tbxd.socket", "tbxd.service"} {
			if err := runSupervisorCommand("systemctl", "--user", "is-active", "--quiet", unit); err == nil {
				return true, "tbxd is managed by systemd (--user unit " + unit + ")"
			}
		}
	case "darwin":
		label := fmt.Sprintf("gui/%d/%s", os.Getuid(), daemonLaunchdLabel)
		if err := runSupervisorCommand("launchctl", "print", label); err == nil {
			return true, "tbxd is managed by launchd (" + label + ")"
		}
	}
	return false, ""
}

const daemonLaunchdLabel = "dev.talosbox.tbxd"

// supervisionUnitPaths lists every location a packaged install can put a tbxd
// unit: the deb/rpm and Nix systemd user units, the system-wide overrides, and
// the launchd plists.
func supervisionUnitPaths() []string {
	paths := []string{
		"/usr/lib/systemd/user/tbxd.service",
		"/usr/lib/systemd/user/tbxd.socket",
		"/usr/local/lib/systemd/user/tbxd.service",
		"/usr/local/lib/systemd/user/tbxd.socket",
		"/etc/systemd/user/tbxd.service",
		"/etc/systemd/user/tbxd.socket",
		"/etc/systemd/system/tbxd.service",
		"/etc/systemd/system/tbxd.socket",
		"/Library/LaunchDaemons/" + daemonLaunchdLabel + ".plist",
		"/Library/LaunchAgents/" + daemonLaunchdLabel + ".plist",
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths,
			filepath.Join(home, "Library", "LaunchAgents", daemonLaunchdLabel+".plist"),
			filepath.Join(home, ".config", "systemd", "user", "tbxd.service"),
			filepath.Join(home, ".config", "systemd", "user", "tbxd.socket"))
	}
	return paths
}

// replaceDaemon replaces the running daemon with one spawned from this build
// and returns the replacement's info and pid. An unidentifiable or supervised
// daemon is left alone: killing a process tbx cannot prove it owns is worse
// than reporting the skew.
//
// The old daemon stops every VM it runs before it exits, so the wait is scaled
// to that workload and the replacement is only spawned once the socket lock is
// actually free — otherwise the new daemon dies on "another daemon owns
// tbxd.sock" while the old one is still cleaning up (#319).
func replaceDaemon(socketPath string, info daemon.Info, pid int, activity clusterActivity, progress io.Writer) (daemon.Info, int, error) {
	// only a confirmed supervisor is refused here; callers decide what to do
	// about a merely inferred unit file, which a forced restart may override
	if state, reason := supervisedDaemon(); state == supervisionConfirmed {
		return info, pid, errors.New(reason)
	}
	if pid <= 0 {
		return info, pid, errors.New("the running tbxd could not be identified")
	}
	if err := terminateDaemonProcess(pid); err != nil {
		return info, pid, fmt.Errorf("stop tbxd (pid %d): %w", pid, err)
	}
	if err := waitForDaemonExit(pid, activity, progress); err != nil {
		return info, pid, err
	}
	// The daemon holds its socket lock deliberately past listener close, so an
	// observed process exit is not yet permission to bind.
	serving, servingPID, err := waitForDaemonLock(socketPath, progress)
	if err != nil {
		return info, pid, err
	}
	if servingPID > 0 {
		// A concurrent tbx already spawned a current-protocol daemon in the
		// window between the old exit and our spawn; the restart's goal is met.
		return serving, servingPID, nil
	}
	if _, err := spawnDaemonProcess(); err != nil {
		return info, pid, err
	}
	return waitForCurrentDaemon(socketPath)
}

// daemonExitTimeout is how long the old daemon gets to exit. Stopping a VM is
// bounded by the daemon's own 30s per-machine timeout and the machines are
// retired one cluster at a time, so a fixed 20s gives up on the very restarts
// that need the wait most (#319).
func daemonExitTimeout(activity clusterActivity) time.Duration {
	return daemonWaitTimeout + time.Duration(activity.runningVMs)*machineStopBudget
}

// machineStopBudget mirrors the daemon's per-machine stop timeout. It is a var
// only so tests can shorten it.
var machineStopBudget = 30 * time.Second

// daemonExitProgressInterval is how often a long wait says what it is waiting
// for. It is a var only so tests can shorten it.
var daemonExitProgressInterval = 5 * time.Second

// waitForDaemonExit waits for the daemon process itself to go away. It must
// never poll the socket: under systemd socket activation the supervisor owns
// the listener, so every dial would respawn the daemon this call is trying to
// retire.
func waitForDaemonExit(pid int, activity clusterActivity, progress io.Writer) error {
	timeout := daemonExitTimeout(activity)
	start := time.Now()
	deadline := start.Add(timeout)
	nextReport := start.Add(daemonExitProgressInterval)
	for {
		if !daemonProcessAlive(pid) {
			return nil
		}
		now := time.Now()
		if now.After(deadline) {
			return fmt.Errorf("tbxd (pid %d) is still stopping %s and did not exit within %s; "+
				"wait for it to finish and re-run: tbx system restart --force",
				pid, describeStoppingVMs(activity), timeout)
		}
		if !now.Before(nextReport) {
			reportProgress(progress, "waiting for tbxd (pid %d) to exit (stopping %s) (%s elapsed)\n",
				pid, describeStoppingVMs(activity), now.Sub(start).Round(time.Second))
			nextReport = now.Add(daemonExitProgressInterval)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitForDaemonLock waits until the socket's flock can be taken, which is the
// only proof the old daemon has finished with the socket: it keeps the lock
// until the process is really gone, past listener close.
//
// A held lock can also mean a *fresh* daemon: another tbx may auto-spawn one in
// the window after the old exit. That is not a failure — it is the restart's
// goal already met — so a lock-holder that answers the handshake with the
// current protocol is returned (non-zero pid) instead of waited out.
func waitForDaemonLock(socketPath string, progress io.Writer) (daemon.Info, int, error) {
	start := time.Now()
	deadline := start.Add(daemonWaitTimeout)
	nextReport := start.Add(daemonExitProgressInterval)
	for {
		if daemonLockFree(socketPath) {
			return daemon.Info{}, 0, nil
		}
		// Unlike waitForDaemonExit, dialing here is safe from the socket-
		// activation respawn hazard: the probe only runs while the flock is
		// held, so a listener that answers is a daemon that already exists —
		// and a current-protocol one is the restart's goal met, whoever
		// spawned it.
		if info, pid, err := daemonHandshakeProbe(socketPath); err == nil && info.ProtocolVersion == daemon.ProtocolVersion {
			return info, pid, nil
		}
		now := time.Now()
		if now.After(deadline) {
			return daemon.Info{}, 0, fmt.Errorf("the old tbxd still owns %s after %s; it is finishing its shutdown — re-run: tbx system restart --force",
				socketPath, daemonWaitTimeout)
		}
		if !now.Before(nextReport) {
			reportProgress(progress, "waiting for the old tbxd to release %s (%s elapsed)\n",
				socketPath, now.Sub(start).Round(time.Second))
			nextReport = now.Add(daemonExitProgressInterval)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// socketLockFree reports whether the daemon socket lock can be taken right now.
// It takes the lock non-blocking and releases it immediately, so it only ever
// answers the question.
func socketLockFree(socketPath string) bool {
	lock, err := os.OpenFile(socketPath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		// A missing parent directory means no daemon can be holding the lock;
		// the spawn creates it and reports any real problem. Any other open
		// failure (permissions, I/O) proves nothing about the holder, so it
		// keeps the wait going rather than racing a daemon that may be there.
		return os.IsNotExist(err)
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return false
	}
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return true
}

// describeStoppingVMs counts the configured VMs of every running cluster,
// which is an upper bound rather than a census: individual nodes can be stopped
// with `tbx node stop`. The wait budget is happy to over-estimate, but the
// narration must not claim more VMs are running than there are.
func describeStoppingVMs(activity clusterActivity) string {
	if activity.runningVMs == 0 {
		return "its clusters"
	}
	if activity.runningVMs == 1 {
		return "up to 1 VM"
	}
	return fmt.Sprintf("up to %d VMs", activity.runningVMs)
}

// reportProgress writes one progress line, ignoring a write failure: a restart
// must not fail because its narration could not be printed.
func reportProgress(progress io.Writer, format string, args ...any) {
	if progress == nil {
		return
	}
	_, _ = fmt.Fprintf(progress, format, args...)
}

func waitForCurrentDaemon(socketPath string) (daemon.Info, int, error) {
	deadline := time.Now().Add(daemonWaitTimeout)
	for {
		info, pid, err := daemonHandshake(socketPath)
		if err == nil && info.ProtocolVersion == daemon.ProtocolVersion {
			return info, pid, nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return info, pid, fmt.Errorf("the replacement tbxd did not serve %s within %s: %w", socketPath, daemonWaitTimeout, err)
			}
			return info, pid, fmt.Errorf("the replacement tbxd still speaks protocol %d, not %d", info.ProtocolVersion, daemon.ProtocolVersion)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (c cli) call(op string, args, destination any) error {
	return c.callNarrated(op, args, destination, nil)
}

// stages is the narration sink for a verb that has no heartbeat to share the
// stream with: one goroutine writes, so no narrator is needed. A quiet verb
// gets nil, which also stops the daemon from sending stages at all (#273).
func (c cli) stages(quiet bool) func(string) {
	if quiet {
		return nil
	}
	return func(stage string) { _, _ = fmt.Fprintln(c.err, stage) }
}

// callNarrated is call with a narration sink: a non-nil onStage asks the daemon
// to report the operation's stages while it runs, and each one is handed over
// as it arrives. A nil sink is the silent exchange every other verb makes
// (#273).
func (c cli) callNarrated(op string, args, destination any, onStage func(string)) error {
	return c.callNarratedWithin(op, args, destination, onStage, 0)
}

// callNarratedWithin is callNarrated with an overall bound on the exchange. A
// zero timeout is the unbounded lifecycle call; a blocking verb passes the
// budget it narrated plus its grace, so a daemon-side gate that never returns
// fails the verb instead of hanging it forever (#392).
func (c cli) callNarratedWithin(op string, args, destination any, onStage func(string), timeout time.Duration) error {
	if err := c.ensureDaemonProtocol(); err != nil {
		return err
	}
	socketPath, err := daemon.SocketPath()
	if err != nil {
		return err
	}
	response, err := exchangeDeadlined(socketPath, op, args, timeout, onStage)
	var connectionError dialError
	if errors.As(err, &connectionError) {
		logOffset, startErr := startDaemon()
		if startErr != nil {
			return startErr
		}
		// a daemon shutting down with live VMs can hold the lock for >10s before a
		// replacement can bind — retry long enough to outlast that
		deadline := time.Now().Add(daemonWaitTimeout)
		backoff := 50 * time.Millisecond
		for {
			response, err = exchangeDeadlined(socketPath, op, args, timeout, onStage)
			if !errors.As(err, &connectionError) || time.Now().After(deadline) {
				break
			}
			time.Sleep(backoff)
			if backoff < 500*time.Millisecond {
				backoff *= 2
			}
		}
		if errors.As(err, &connectionError) {
			logPath, pathErr := daemonLogPath()
			if pathErr != nil {
				logPath = ""
			}
			return daemonSpawnFailure(err, logPath, logOffset)
		}
	}
	if err != nil {
		return err
	}
	if !response.OK {
		if response.Error == "" {
			return errors.New("daemon operation failed")
		}
		return errors.New(response.Error)
	}
	if destination != nil && len(response.Data) > 0 {
		if err := json.Unmarshal(response.Data, destination); err != nil {
			return fmt.Errorf("decode daemon result: %w", err)
		}
	}
	return nil
}

func exchange(socketPath, op string, args any) (daemon.Response, error) {
	// no deadline: a lifecycle verb legitimately runs for minutes
	return exchangeWithin(socketPath, op, args, 0)
}

// exchangeWithin runs one request/response with an optional overall deadline,
// which the restart paths need: their queries are served under the daemon's
// operation lock and must never outlast a courtesy check.
func exchangeWithin(socketPath, op string, args any, timeout time.Duration) (daemon.Response, error) {
	return exchangeDeadlined(socketPath, op, args, timeout, nil)
}

func exchangeDeadlined(socketPath, op string, args any, timeout time.Duration, onStage func(string)) (daemon.Response, error) {
	connection, err := net.DialTimeout("unix", socketPath, 250*time.Millisecond)
	if err != nil {
		return daemon.Response{}, dialError{err: err}
	}
	defer func() { _ = connection.Close() }()
	// The deadline bounds silence, not the call: it is re-armed below on every
	// stage the daemon narrates, so a request that is still reporting progress
	// — a cold image fetch, a slow reconcile — is never mistaken for a hang,
	// while one that stops talking altogether still fails (#392).
	arm := func() error {
		if timeout <= 0 {
			return nil
		}
		if err := connection.SetDeadline(time.Now().Add(timeout)); err != nil {
			return fmt.Errorf("set daemon deadline: %w", err)
		}
		return nil
	}
	if err := arm(); err != nil {
		return daemon.Response{}, err
	}

	rawArgs, err := json.Marshal(args)
	if err != nil {
		return daemon.Response{}, fmt.Errorf("encode request arguments: %w", err)
	}
	request := daemon.Request{Op: op, Args: rawArgs, Progress: onStage != nil}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return daemon.Response{}, fmt.Errorf("write daemon request: %w", err)
	}
	// A narrated request is answered by a run of stage responses followed by
	// the single result; an unnarrated one gets the result straight away.
	decoder := json.NewDecoder(connection)
	for {
		var response daemon.Response
		if err := decoder.Decode(&response); err != nil {
			return daemon.Response{}, fmt.Errorf("read daemon response: %w", err)
		}
		if response.IsProgress() {
			if onStage != nil {
				onStage(response.Stage)
			}
			if err := arm(); err != nil {
				return daemon.Response{}, err
			}
			continue
		}
		return response, nil
	}
}

// daemonWaitTimeout bounds how long a just-spawned daemon may take to serve,
// and is the base a stop-wait is scaled from. It is a var only so tests can
// shorten it.
var daemonWaitTimeout = 20 * time.Second

// daemonHandshakeTimeout bounds the whole daemon.info exchange. The gate is a
// courtesy check, so a daemon busy under its operation lock must not hang it.
// It is a var only so tests can shorten it.
var daemonHandshakeTimeout = 3 * time.Second

// daemonHandshakeRetryTimeout bounds the single retry the gate makes before it
// gives up and skips: a daemon that is merely slow must not silently re-open
// the protocol-skew hole the gate exists to close (#290).
var daemonHandshakeRetryTimeout = 10 * time.Second

// daemonSpawnFailure explains a daemon that was spawned but never served: the
// bare dial error hides that tbxd itself crashed, and the cause is in its log.
// An empty logPath (home directory unknown) falls back to a display-only path.
// logOffset is the log size before the spawn, so only lines this daemon wrote
// are quoted — the log is append-only across runs, and a stale line from a
// previous run would mislead.
func daemonSpawnFailure(dialErr error, logPath string, logOffset int64) error {
	quoted := ""
	if logPath == "" {
		logPath = "~/.talosbox/tbxd.log"
	} else {
		quoted = lastLogLine(logPath, logOffset)
	}
	message := fmt.Sprintf("tbxd was started but did not serve its socket within %s (%v); see %s",
		daemonWaitTimeout, dialErr, logPath)
	if quoted != "" {
		message = fmt.Sprintf("%s (last log line: %s)", message, quoted)
	}
	return errors.New(message)
}

// lastLogLine best-effort reads the last non-empty line of the daemon log
// written at or after fromOffset. The log is append-only and can be large, so
// only its tail is read.
func lastLogLine(logPath string, fromOffset int64) string {
	file, err := os.Open(logPath)
	if err != nil {
		return ""
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || info.Size() <= fromOffset {
		return ""
	}
	const tailBytes = 4096
	offset := max(info.Size()-tailBytes, fromOffset)
	content := make([]byte, info.Size()-offset)
	if _, err := file.ReadAt(content, offset); err != nil {
		return ""
	}
	lines := strings.Split(string(content), "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		if line := strings.TrimSpace(lines[index]); line != "" {
			return line
		}
	}
	return ""
}

func daemonLogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return filepath.Join(home, ".talosbox", daemon.LogFile), nil
}

// startDaemon spawns tbxd and returns the daemon log's size before the spawn,
// so a later failure report can quote only what this daemon wrote.
func startDaemon() (int64, error) {
	executable, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("find tbx executable: %w", err)
	}
	daemonPath := filepath.Join(filepath.Dir(executable), "tbxd")
	logPath, err := daemonLogPath()
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return 0, fmt.Errorf("create daemon directory: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, fmt.Errorf("open daemon log: %w", err)
	}
	defer func() { _ = logFile.Close() }()
	logInfo, err := logFile.Stat()
	if err != nil {
		return 0, fmt.Errorf("inspect daemon log: %w", err)
	}

	command := exec.Command(daemonPath)
	command.Stdout = logFile
	command.Stderr = logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return 0, fmt.Errorf("start %s: %w", daemonPath, err)
	}
	if err := command.Process.Release(); err != nil {
		return 0, fmt.Errorf("detach tbxd: %w", err)
	}
	return logInfo.Size(), nil
}

func parseInterspersed(flags *flag.FlagSet, args []string) ([]string, error) {
	var flagArgs, positionals []string
	for i := 0; i < len(args); i++ {
		argument := args[i]
		if argument == "--" {
			positionals = append(positionals, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(argument, "-") || argument == "-" {
			positionals = append(positionals, argument)
			continue
		}
		flagArgs = append(flagArgs, argument)
		nameValue := strings.TrimLeft(argument, "-")
		name, _, hasValue := strings.Cut(nameValue, "=")
		definition := flags.Lookup(name)
		if definition == nil || hasValue {
			continue
		}
		boolean, isBoolean := definition.Value.(interface{ IsBoolFlag() bool })
		if isBoolean && boolean.IsBoolFlag() {
			continue
		}
		if i+1 >= len(args) {
			return nil, fmt.Errorf("flag needs an argument: %s", argument)
		}
		i++
		flagArgs = append(flagArgs, args[i])
	}
	if err := flags.Parse(flagArgs); err != nil {
		return nil, err
	}
	return positionals, nil
}
