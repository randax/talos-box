package hypervisor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/randax/talos-box/internal/helper"
)

const qemuStartupTimeout = 10 * time.Second

type qemuConsoleFactory func(string) (*consoleProxy, *os.File, error)

type qemuHypervisor struct {
	architecture Architecture
	system       qemuSystem
	binary       string
	firmware     qemuFirmware
	version      qemuVersion
	capabilities Capabilities
	newConsole   qemuConsoleFactory
	command      func(string, ...string) *exec.Cmd
	verifyPeer   func(net.Conn, *qemuProcess) error

	savedMu sync.Mutex
	saved   map[string]*qemuMachine
}

func (h *qemuHypervisor) Capabilities() Capabilities { return h.capabilities }

func (h *qemuHypervisor) Architecture() Architecture { return h.architecture }

func (h *qemuHypervisor) Launch(ctx context.Context, spec Spec) (Machine, error) {
	if err := validateSpec(spec); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("launch QEMU VM: %w", err)
	}

	var incoming string
	if spec.Restore != nil && spec.Restore.Path != "" {
		metadata, err := readQEMUSave(spec.Restore.Path)
		if err == nil {
			err = validateQEMUSave(metadata, h.version.String(), h.architecture, h.system.Machine)
		}
		if err != nil {
			reportRestoreFallback(spec.Restore, err)
		} else if !h.capabilities.Suspend.Supported {
			err = fmt.Errorf("%w: %s", ErrIncompatibleSave, h.capabilities.Suspend.Reason)
			reportRestoreFallback(spec.Restore, err)
		} else {
			incoming = spec.Restore.Path
		}
	}

	machine := (*qemuMachine)(nil)
	if spec.Restore != nil && spec.Restore.Path != "" {
		machine = h.takeSaved(spec.Restore.Path)
	}
	if machine == nil {
		var err error
		machine, err = h.newMachine(spec)
		if err != nil {
			return nil, err
		}
	} else {
		machine.opMu.Lock()
		machine.spec = spec
		machine.opMu.Unlock()
	}

	started := false
	defer func() {
		if !started {
			_ = machine.Close()
		}
	}()

	if incoming != "" {
		if err := machine.start(ctx, incoming); err == nil {
			started = true
			return machine, nil
		} else {
			restoreErr := fmt.Errorf("%w: restore QEMU state: %v", ErrIncompatibleSave, err)
			reportRestoreFallback(spec.Restore, restoreErr)
			_ = machine.stopProcess()
		}
	}
	if err := machine.start(ctx, ""); err != nil {
		return nil, err
	}
	started = true
	return machine, nil
}

func reportRestoreFallback(restore *Restore, err error) {
	if restore != nil && restore.Fallback != nil {
		restore.Fallback(err)
	}
}

func (h *qemuHypervisor) newMachine(spec Spec) (*qemuMachine, error) {
	if spec.Network == nil {
		return nil, errors.New("network attachment provider is required")
	}
	if err := ensureQEMUVars(osQEMUFS{}, h.firmware.VarsPath, spec.EFIVarsPath); err != nil {
		return nil, err
	}
	attachment, err := spec.Network()
	if err != nil {
		return nil, fmt.Errorf("attach QEMU network: %w", err)
	}
	if attachment == nil {
		return nil, errors.New("network attachment provider returned nil")
	}
	owned := false
	defer func() {
		if !owned {
			_ = attachment.Close()
		}
	}()
	if attachment.Kind != helper.AttachmentTapFD {
		return nil, fmt.Errorf("%w: QEMU requires %q network attachment, got %q", ErrUnsupported, helper.AttachmentTapFD, attachment.Kind)
	}
	if attachment.File == nil {
		return nil, errors.New("network attachment has no descriptor")
	}
	if h.newConsole == nil {
		return nil, errors.New("QEMU console factory is unavailable")
	}
	console, consoleGuest, err := h.newConsole(spec.ConsoleSocketPath)
	if err != nil {
		return nil, err
	}
	owned = true
	return &qemuMachine{
		owner:        h,
		spec:         spec,
		attachment:   attachment,
		console:      console,
		consoleGuest: consoleGuest,
	}, nil
}

func (h *qemuHypervisor) retain(path string, machine *qemuMachine) error {
	h.savedMu.Lock()
	defer h.savedMu.Unlock()
	if h.saved == nil {
		h.saved = make(map[string]*qemuMachine)
	}
	if existing := h.saved[path]; existing != nil && existing != machine {
		return fmt.Errorf("saved state %q is already owned by another VM", path)
	}
	h.saved[path] = machine
	return nil
}

func (h *qemuHypervisor) takeSaved(path string) *qemuMachine {
	h.savedMu.Lock()
	defer h.savedMu.Unlock()
	machine := h.saved[path]
	delete(h.saved, path)
	return machine
}

func (h *qemuHypervisor) forget(machine *qemuMachine) {
	h.savedMu.Lock()
	defer h.savedMu.Unlock()
	for path, saved := range h.saved {
		if saved == machine {
			delete(h.saved, path)
		}
	}
}

type qemuMachine struct {
	owner        *qemuHypervisor
	spec         Spec
	attachment   *helper.Attachment
	console      *consoleProxy
	consoleGuest *os.File

	opMu    sync.Mutex
	process *qemuProcess
	qmp     *qmpClient
	qmpPath string

	closeMu sync.Mutex
	closed  bool
}

func (m *qemuMachine) start(ctx context.Context, incoming string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if m.closed {
		return errors.New("QEMU machine is closed")
	}
	if m.process != nil && m.process.active() {
		return errors.New("QEMU machine is already active")
	}
	if m.qmp != nil {
		_ = m.qmp.close()
		m.qmp = nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	qmpPath := qemuSocketPath(m.spec.ConsoleSocketPath)
	if err := removeStaleQMPSocket(qmpPath); err != nil {
		return err
	}
	argv, err := buildQEMUArgv(qemuLaunchConfig{
		Architecture:   m.owner.architecture,
		CPUs:           m.spec.CPUs,
		MemoryMiB:      m.spec.MemoryMiB,
		DiskPath:       m.spec.DiskPath,
		MAC:            m.spec.MAC,
		TapFD:          3,
		ConsoleFD:      4,
		QMPSocketPath:  qmpPath,
		Firmware:       qemuFirmware{CodePath: m.owner.firmware.CodePath, VarsPath: m.spec.EFIVarsPath},
		IncomingPath:   incoming,
		IncomingOffset: qemuSaveOffset,
	})
	if err != nil {
		return fmt.Errorf("build QEMU command: %w", err)
	}
	commandFactory := m.owner.command
	if commandFactory == nil {
		commandFactory = exec.Command
	}
	command := commandFactory(m.owner.binary, argv[1:]...)
	command.ExtraFiles = []*os.File{m.attachment.File, m.consoleGuest}
	command.Stdout = os.Stderr
	command.Stderr = os.Stderr
	process, err := startQEMUProcess(command)
	if err != nil {
		return fmt.Errorf("start QEMU VM: %w", err)
	}
	m.process = process
	m.qmpPath = qmpPath

	startupCtx, cancel := context.WithTimeout(ctx, qemuStartupTimeout)
	defer cancel()
	connection, err := dialQMPSocket(startupCtx, qmpPath, process)
	if err != nil {
		_ = process.kill()
		<-process.done
		return fmt.Errorf("connect QEMU monitor: %w", err)
	}
	if m.owner.verifyPeer != nil {
		if err := m.owner.verifyPeer(connection, process); err != nil {
			_ = connection.Close()
			_ = process.kill()
			<-process.done
			return fmt.Errorf("authenticate QEMU monitor: %w", err)
		}
	}
	client, err := newQMPClient(startupCtx, connection)
	if err != nil {
		_ = connection.Close()
		_ = process.kill()
		<-process.done
		return fmt.Errorf("handshake QEMU monitor: %w", err)
	}
	m.qmp = client
	if incoming != "" {
		if err := waitQEMUIncoming(startupCtx, client); err != nil {
			_ = client.close()
			_ = process.kill()
			<-process.done
			return err
		}
	}
	if err := client.execute(startupCtx, "cont", nil, nil); err != nil {
		_ = client.close()
		_ = process.kill()
		<-process.done
		return fmt.Errorf("start QEMU CPUs: %w", err)
	}
	if !process.active() {
		exitErr := process.waitError()
		if exitErr == nil {
			exitErr = errors.New("QEMU exited without an error status")
		}
		return fmt.Errorf("QEMU exited while starting: %w", exitErr)
	}
	return nil
}

func waitQEMUIncoming(ctx context.Context, client *qmpClient) error {
	if err := waitQEMUMigration(ctx, client); err != nil {
		return fmt.Errorf("%w: incoming migration: %v", ErrIncompatibleSave, err)
	}
	var result struct {
		Status string `json:"status"`
	}
	if err := client.execute(ctx, "query-status", nil, &result); err != nil {
		return fmt.Errorf("query QEMU restore status: %w", err)
	}
	switch result.Status {
	case "paused", "prelaunch", "postmigrate", "running":
		return nil
	default:
		return fmt.Errorf("%w: unexpected QEMU restore status %q", ErrIncompatibleSave, result.Status)
	}
}

func (m *qemuMachine) Active() bool {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	return !m.closed && m.process != nil && m.process.active()
}

func (m *qemuMachine) SetMemoryTargetMiB(targetMiB int) error {
	if targetMiB <= 0 {
		return errors.New("memory target must be greater than zero")
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if !m.activeLocked() || m.qmp == nil {
		return ErrDeviceNotActive
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return m.qmp.execute(ctx, "balloon", map[string]any{"value": int64(targetMiB) * 1024 * 1024}, nil)
}

func (m *qemuMachine) queryBalloon(ctx context.Context) (int64, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if !m.activeLocked() || m.qmp == nil {
		return 0, ErrDeviceNotActive
	}
	var result struct {
		Actual int64 `json:"actual"`
	}
	if err := m.qmp.execute(ctx, "query-balloon", nil, &result); err != nil {
		return 0, err
	}
	return result.Actual, nil
}

func (m *qemuMachine) Stop(ctx context.Context) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if !m.activeLocked() {
		return nil
	}
	requestErr := m.qmp.execute(ctx, "system_powerdown", nil, nil)
	if requestErr != nil {
		return m.forceStopLocked(requestErr)
	}
	select {
	case <-m.process.done:
		return nil
	case <-ctx.Done():
		return m.forceStopLocked(ctx.Err())
	}
}

func (m *qemuMachine) Suspend(ctx context.Context, savePath string) (result error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if !m.owner.capabilities.Suspend.Supported {
		return fmt.Errorf("%w: %s", ErrUnsupported, m.owner.capabilities.Suspend.Reason)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !m.activeLocked() || m.qmp == nil {
		return ErrDeviceNotActive
	}
	if savePath == "" {
		return errors.New("QEMU save path is required")
	}
	if err := m.owner.retain(savePath, m); err != nil {
		return err
	}
	saved := false
	defer func() {
		if !saved {
			m.owner.forget(m)
		}
	}()

	temporary, err := prepareQEMUSave(savePath, qemuSaveMetadata{
		QEMUVersion:  m.owner.version.String(),
		Architecture: m.owner.architecture,
		Machine:      m.owner.system.Machine,
	})
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(temporary)
		}
	}()

	if err := m.qmp.execute(ctx, "stop", nil, nil); err != nil {
		return fmt.Errorf("pause QEMU VM: %w", err)
	}
	resumeOnError := true
	defer func() {
		if result != nil && resumeOnError && m.activeLocked() {
			resumeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = m.qmp.execute(resumeCtx, "cont", nil, nil)
		}
	}()
	if err := m.qmp.execute(ctx, "migrate", map[string]any{
		"channels": []any{map[string]any{
			"channel-type": "main",
			"addr": map[string]any{
				"transport": "file",
				"filename":  temporary,
				"offset":    fmt.Sprintf("0x%x", qemuSaveOffset),
			},
		}},
	}, nil); err != nil {
		return fmt.Errorf("start QEMU file migration: %w", err)
	}
	if err := waitQEMUMigration(ctx, m.qmp); err != nil {
		return err
	}
	resumeOnError = false
	if err := commitQEMUSave(temporary, savePath); err != nil {
		return err
	}
	committed = true
	if err := m.quitLocked(ctx); err != nil {
		return fmt.Errorf("stop QEMU after suspend: %w", err)
	}
	// The retained machine keeps its console and network attachment, but the
	// QMP connection belongs to the QEMU process that just exited. Close it now
	// instead of retaining the descriptor until a future resume or Close.
	m.cleanupExitedQMP()
	saved = true
	return nil
}

func (m *qemuMachine) cleanupExitedQMP() {
	if m.qmp != nil {
		if err := m.qmp.close(); err == nil {
			m.qmp = nil
		}
	}
	if m.qmpPath != "" {
		if err := os.Remove(m.qmpPath); err == nil || errors.Is(err, os.ErrNotExist) {
			m.qmpPath = ""
		}
	}
}

func waitQEMUMigration(ctx context.Context, client *qmpClient) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		var result struct {
			Status    string `json:"status"`
			ErrorDesc string `json:"error-desc"`
		}
		if err := client.execute(ctx, "query-migrate", nil, &result); err != nil {
			return fmt.Errorf("query QEMU migration: %w", err)
		}
		switch result.Status {
		case "completed":
			return nil
		case "failed", "cancelled":
			if result.ErrorDesc == "" {
				result.ErrorDesc = "migration did not complete"
			}
			return fmt.Errorf("QEMU migration %s: %s", result.Status, result.ErrorDesc)
		case "setup", "active", "postcopy-active", "device", "wait-unplug", "pre-switchover":
		default:
			return fmt.Errorf("QEMU migration returned unexpected status %q", result.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (m *qemuMachine) Close() error {
	m.closeMu.Lock()
	defer m.closeMu.Unlock()
	m.opMu.Lock()
	if m.closed {
		m.opMu.Unlock()
		return nil
	}
	stopErr := m.forceStopLocked(nil)
	if m.qmp != nil {
		stopErr = errors.Join(stopErr, m.qmp.close())
		m.qmp = nil
	}
	if stopErr != nil {
		m.opMu.Unlock()
		return stopErr
	}
	m.closed = true
	m.opMu.Unlock()
	m.owner.forget(m)
	if m.console != nil {
		m.console.close()
	}
	if m.consoleGuest != nil {
		_ = m.consoleGuest.Close()
	}
	if m.attachment != nil {
		if err := m.attachment.Close(); err != nil {
			return err
		}
	}
	if m.qmpPath != "" {
		_ = os.Remove(m.qmpPath)
	}
	return nil
}

func (m *qemuMachine) activeLocked() bool {
	return !m.closed && m.process != nil && m.process.active()
}

func (m *qemuMachine) quitLocked(ctx context.Context) error {
	if !m.activeLocked() {
		return nil
	}
	quitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := m.qmp.execute(quitCtx, "quit", nil, nil)
	select {
	case <-m.process.done:
		return nil
	case <-quitCtx.Done():
		killErr := m.process.kill()
		<-m.process.done
		return errors.Join(err, killErr)
	}
}

func (m *qemuMachine) forceStopLocked(prior error) error {
	if !m.activeLocked() {
		return nil
	}
	forceCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	quitErr := error(nil)
	if m.qmp != nil {
		quitErr = m.qmp.execute(forceCtx, "quit", nil, nil)
	}
	select {
	case <-m.process.done:
		return nil
	case <-forceCtx.Done():
		killErr := m.process.kill()
		<-m.process.done
		if killErr != nil {
			return errors.Join(prior, quitErr, fmt.Errorf("kill QEMU process: %w", killErr))
		}
		return nil
	}
}

func (m *qemuMachine) stopProcess() error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	return m.forceStopLocked(nil)
}

func qemuSocketPath(consolePath string) string {
	base := strings.TrimSuffix(consolePath, ".console.sock")
	if base == consolePath {
		base = strings.TrimSuffix(consolePath, filepath.Ext(consolePath))
	}
	return base + ".qmp.sock"
}

func removeStaleQMPSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect QMP socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refuse to replace non-socket QMP path %q", path)
	}
	connection, dialErr := net.DialTimeout("unix", path, 100*time.Millisecond)
	if dialErr == nil {
		_ = connection.Close()
		return fmt.Errorf("QMP socket is already in use: %s", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale QMP socket: %w", err)
	}
	return nil
}
