package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
			return fmt.Errorf("tbxd is too old; restart or upgrade tbxd to use %s", feature)
		}
		return err
	}
	if info.ProtocolVersion < minimum {
		return fmt.Errorf("tbxd protocol %d is too old; restart or upgrade tbxd to use %s", info.ProtocolVersion, feature)
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

func (c cli) ensureCacheWarmSupport() error {
	return c.ensureProtocolAtLeast(cacheWarmProtocolVersion, "cache warm/check")
}

func (c cli) call(op string, args, destination any) error {
	socketPath, err := daemon.SocketPath()
	if err != nil {
		return err
	}
	response, err := exchange(socketPath, op, args)
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
			response, err = exchange(socketPath, op, args)
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
	connection, err := net.DialTimeout("unix", socketPath, 250*time.Millisecond)
	if err != nil {
		return daemon.Response{}, dialError{err: err}
	}
	defer func() { _ = connection.Close() }()

	rawArgs, err := json.Marshal(args)
	if err != nil {
		return daemon.Response{}, fmt.Errorf("encode request arguments: %w", err)
	}
	request := daemon.Request{Op: op, Args: rawArgs}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return daemon.Response{}, fmt.Errorf("write daemon request: %w", err)
	}
	var response daemon.Response
	if err := json.NewDecoder(connection).Decode(&response); err != nil {
		return daemon.Response{}, fmt.Errorf("read daemon response: %w", err)
	}
	return response, nil
}

// daemonWaitTimeout bounds how long a just-spawned daemon may take to serve.
const daemonWaitTimeout = 20 * time.Second

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
	return filepath.Join(home, ".talosbox", "tbxd.log"), nil
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
