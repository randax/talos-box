//go:build e2e

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/balloon"
	"github.com/randax/talos-box/internal/daemon"
	"github.com/randax/talos-box/internal/hypervisor"
)

const (
	e2eCommandTimeout = 30 * time.Minute
	e2eCleanupTimeout = 3 * time.Minute
)

var e2eOwnedClusters = struct {
	sync.Mutex
	names map[string]struct{}
}{names: make(map[string]struct{})}

func requireE2EHypervisor(t *testing.T) e2eHypervisorEntry {
	t.Helper()
	_ = binPath(t, "tbxd")
	requireE2ERuntime(t)
	output, doctorErr := runTBXCommand(t, nil, e2eCommandTimeout, "doctor")
	inventory, err := parseDoctorHypervisorInventory(output, doctorErr)
	if err != nil {
		t.Fatalf("parse `tbx doctor` hypervisor inventory: %v\nfull doctor output:\n%s", err, output)
	}
	selected, err := selectE2EHypervisor(os.Getenv("TBX_E2E_HYPERVISOR"), inventory)
	if err != nil {
		t.Fatalf("invalid e2e hypervisor selection: %v", err)
	}
	if err := validateE2EDoctorPreconditions(output); err != nil {
		t.Fatalf("%v\nfull doctor output:\n%s", err, output)
	}
	entry, err := selectedE2EHypervisor(inventory, selected)
	if err == nil {
		return entry
	}
	var unavailable e2eUnavailableError
	if errors.As(err, &unavailable) {
		t.Skipf("selected %s", unavailable.Error())
	}
	t.Fatalf("select e2e hypervisor: %v\nfull doctor output:\n%s", err, output)
	return e2eHypervisorEntry{}
}

func requireE2ERuntime(t *testing.T) {
	t.Helper()
	output, err := runTBXCommand(t, nil, 30*time.Second, "system", "status")
	if err != nil {
		t.Fatalf("tbx system status: %v\n%s", err, output)
	}
	for _, line := range strings.Split(output, "\n") {
		switch {
		case strings.HasPrefix(line, "FAIL runtime-compat:"):
			t.Fatalf("e2e runtime precondition: %s\nfull system status:\n%s", line, output)
		case strings.HasPrefix(strings.TrimSpace(line), "helper: not installed"):
			t.Skipf("tbx-helper is not installed; install it with `sudo %s system install` and rerun the e2e tests", binPath(t, "tbx"))
		case strings.HasPrefix(strings.TrimSpace(line), "helper: unreachable:"),
			strings.HasPrefix(strings.TrimSpace(line), "helper: installed, inactive"):
			t.Fatalf("e2e runtime precondition: %s\nfull system status:\n%s", strings.TrimSpace(line), output)
		}
	}
}

func runTBX(t *testing.T, args ...string) string {
	t.Helper()
	output, err := runTBXCommand(t, nil, e2eCommandTimeout, args...)
	if err != nil {
		t.Fatalf("tbx %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return output
}

func runTBXWithEnv(t *testing.T, env []string, args ...string) string {
	t.Helper()
	output, err := runTBXCommand(t, env, e2eCommandTimeout, args...)
	if err != nil {
		t.Fatalf("tbx %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return output
}

func runTBXFailure(t *testing.T, args ...string) string {
	t.Helper()
	output, err := runTBXCommand(t, nil, e2eCommandTimeout, args...)
	if err == nil {
		t.Fatalf("tbx %s unexpectedly succeeded\n%s", strings.Join(args, " "), output)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("tbx %s timed out instead of returning an expected failure\n%s", strings.Join(args, " "), output)
	}
	return output
}

func runTBXFailureWithEnv(t *testing.T, env []string, args ...string) string {
	t.Helper()
	output, err := runTBXCommand(t, env, e2eCommandTimeout, args...)
	if err == nil {
		t.Fatalf("tbx %s unexpectedly succeeded\n%s", strings.Join(args, " "), output)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("tbx %s timed out instead of returning an expected failure\n%s", strings.Join(args, " "), output)
	}
	return output
}

func runTBXCommand(t *testing.T, env []string, timeout time.Duration, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, binPath(t, "tbx"), args...)
	if env != nil {
		command.Env = mergeE2EEnv(os.Environ(), env)
	}
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		return string(output), ctx.Err()
	}
	return string(output), err
}

func mergeE2EEnv(base, overrides []string) []string {
	values := make(map[string]string, len(base)+len(overrides))
	for _, item := range append(append([]string{}, base...), overrides...) {
		key, _, ok := strings.Cut(item, "=")
		if ok {
			values[key] = item
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, values[key])
	}
	return result
}

// e2eDaemonEnvState captures the running daemon's environment-derived
// settings so a test that replaces the daemon can carry them through its own
// restarts and put them back afterwards, instead of silently stripping a
// TBX_HYPERVISOR default or a custom balloon reserve the user's daemon was
// started with.
type e2eDaemonEnvState struct {
	reserveMiB     int
	reserveDefault bool
	hypervisorEnv  string // TBX_HYPERVISOR value to carry, "" for none
}

func captureE2EDaemonEnvState(t *testing.T) e2eDaemonEnvState {
	t.Helper()
	socketPath, err := daemon.SocketPath()
	if err != nil {
		t.Fatalf("resolve daemon socket path: %v", err)
	}
	info, _, err := daemonHandshake(socketPath)
	if err != nil {
		t.Fatalf("read running daemon info from %s: %v", socketPath, err)
	}
	state := e2eDaemonEnvState{reserveMiB: info.BalloonReserveMiB}
	compiled := compiledBalloonReserveMiB()
	if state.reserveMiB == 0 {
		state.reserveMiB = compiled
	}
	state.reserveDefault = state.reserveMiB == compiled
	if info.DefaultHypervisorSource == hypervisor.DefaultSourceEnvironment {
		state.hypervisorEnv = string(info.DefaultHypervisor)
	}
	return state
}

// compiledBalloonReserveMiB is balloon.DefaultConfig's reserve with the
// TBX_BALLOON_RESERVE_MIB override stripped: DefaultConfig reads the CURRENT
// process environment, so a test shell sharing the daemon's custom reserve
// would otherwise be misread as the compiled default. Safe because these
// tests never run in parallel.
func compiledBalloonReserveMiB() int {
	if v, ok := os.LookupEnv("TBX_BALLOON_RESERVE_MIB"); ok {
		_ = os.Unsetenv("TBX_BALLOON_RESERVE_MIB")
		defer func() { _ = os.Setenv("TBX_BALLOON_RESERVE_MIB", v) }()
	}
	return balloon.DefaultConfig().ReserveMiB
}

// env renders the captured daemon environment as restart overrides, with any
// extra overrides appended so they win in mergeE2EEnv. An empty value means
// "unset" to both consumers: the registry treats an empty TBX_HYPERVISOR as
// no override, and the balloon manager treats an empty reserve as default.
func (s e2eDaemonEnvState) env(overrides ...string) []string {
	env := []string{"TBX_HYPERVISOR=" + s.hypervisorEnv, "TBX_BALLOON_RESERVE_MIB="}
	if !s.reserveDefault {
		env[1] = "TBX_BALLOON_RESERVE_MIB=" + strconv.Itoa(s.reserveMiB)
	}
	return append(env, overrides...)
}

func binPath(t *testing.T, name string) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	path := filepath.Join(wd, "..", "..", "bin", name)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("required bin/%s is unavailable (run make build): %v", name, err)
	}
	return path
}

func captureTBXDLogOffset(t *testing.T) int64 {
	t.Helper()
	path := tbxdLogPath(t)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat tbxd log %s: %v", path, err)
	}
	return info.Size()
}

func waitForTBXDLog(t *testing.T, offset int64, pattern *regexp.Regexp, deadline time.Duration) string {
	t.Helper()
	expires := time.Now().Add(deadline)
	for {
		tail, err := readTBXDLogFrom(offset)
		if err != nil {
			t.Fatalf("read tbxd log from offset %d: %v", offset, err)
		}
		if pattern.MatchString(tail) {
			return tail
		}
		if time.Now().After(expires) {
			t.Fatalf("timed out after %s waiting for tbxd.log to match %q; new log tail:\n%s", deadline, pattern, tail)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func readTBXDLogFrom(offset int64) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	file, err := os.Open(filepath.Join(home, ".talosbox", "tbxd.log"))
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if offset > info.Size() {
		return "", fmt.Errorf("tbxd.log shrank from offset %d to %d bytes", offset, info.Size())
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return "", err
	}
	data, err := io.ReadAll(file)
	return string(data), err
}

func tbxdLogPath(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve home directory: %v", err)
	}
	return filepath.Join(home, ".talosbox", "tbxd.log")
}

func writeE2EConfig(t *testing.T, yaml string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "talosbox.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write e2e config %s: %v", path, err)
	}
	return path
}

func registerE2EClusterCleanup(t *testing.T, name string, cleanupOutput *strings.Builder) {
	t.Helper()
	e2eOwnedClusters.Lock()
	e2eOwnedClusters.names[name] = struct{}{}
	e2eOwnedClusters.Unlock()
	t.Cleanup(func() {
		output, err := runTBXCommand(t, nil, e2eCleanupTimeout, "cluster", "destroy", name, "--force")
		if cleanupOutput != nil {
			fmt.Fprintf(cleanupOutput, "$ tbx cluster destroy %s --force\n%s", name, output)
		}
		e2eOwnedClusters.Lock()
		delete(e2eOwnedClusters.names, name)
		e2eOwnedClusters.Unlock()
		if err != nil && !strings.Contains(output, "does not exist") {
			t.Errorf("cleanup cluster %q: %v\n%s", name, err, output)
		}
	})
}

func requireNoForeignClusters(t *testing.T) {
	t.Helper()
	output := runTBX(t, "cluster", "list", "-o", "json")
	var clusters []daemon.ClusterSummary
	if err := json.Unmarshal([]byte(output), &clusters); err != nil {
		t.Fatalf("decode `tbx cluster list -o json`: %v\n%s", err, output)
	}
	e2eOwnedClusters.Lock()
	owned := make(map[string]struct{}, len(e2eOwnedClusters.names))
	for name := range e2eOwnedClusters.names {
		owned[name] = struct{}{}
	}
	e2eOwnedClusters.Unlock()
	var offenders []string
	for _, item := range clusters {
		if _, ok := owned[item.Name]; ok {
			continue
		}
		state, remediation := "stopped", "destroy stopped"
		switch {
		case item.Running:
			state, remediation = "running", "stop-or-destroy running"
		case item.Suspended:
			state, remediation = "suspended", "resume-or-destroy suspended"
		}
		offenders = append(offenders, fmt.Sprintf("%s (%s: %s)", item.Name, state, remediation))
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Skipf("foreign clusters prevent isolated e2e execution: %s; use a clean host after applying the state-aware remediation", strings.Join(offenders, ", "))
	}
}

func registerE2EFailureDiagnostics(t *testing.T, logOffset int64) {
	t.Helper()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		status, statusErr := runTBXCommand(t, nil, 30*time.Second, "status", "-o", "json")
		logTail, logErr := readTBXDLogFrom(logOffset)
		var diagnostic bytes.Buffer
		fmt.Fprintf(&diagnostic, "e2e failure diagnostics:\n$ tbx status -o json\n%s", status)
		if statusErr != nil {
			fmt.Fprintf(&diagnostic, "\nstatus diagnostic error: %v", statusErr)
		}
		fmt.Fprintf(&diagnostic, "\ntbxd.log from offset %d:\n%s", logOffset, logTail)
		if logErr != nil {
			fmt.Fprintf(&diagnostic, "\nlog diagnostic error: %v", logErr)
		}
		t.Log(diagnostic.String())
	})
}
