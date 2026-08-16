//go:build linux

package hypervisor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"golang.org/x/sys/unix"
)

// New probes the host QEMU/KVM backend once before the daemon accepts work.
func New(ctx context.Context) (Hypervisor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	architecture := Architecture(runtime.GOARCH)
	system, err := qemuSystemForArchitecture(architecture)
	if err != nil {
		return nil, err
	}
	binary, err := exec.LookPath(system.Binary)
	if err != nil {
		return nil, fmt.Errorf("%w: find %s: %v", ErrUnsupported, system.Binary, err)
	}
	if err := probeKVM("/dev/kvm"); err != nil {
		return nil, err
	}
	probe, err := probeQEMU(ctx, binary)
	if err != nil {
		return nil, fmt.Errorf("probe QEMU: %w", err)
	}
	if err := validateQEMUProbe(probe, system.Machine); err != nil {
		return nil, err
	}
	firmware, err := discoverQEMUFirmware(osQEMUFS{}, architecture, nil)
	if err != nil {
		return nil, fmt.Errorf("probe QEMU firmware: %w", err)
	}
	return &qemuHypervisor{
		architecture: architecture,
		system:       system,
		binary:       binary,
		firmware:     firmware,
		version:      probe.Version,
		capabilities: qemuCapabilities(probe.Version),
		newConsole:   newQEMUConsoleProxy,
		verifyPeer:   verifyQMPPeer,
		saved:        make(map[string]*qemuMachine),
	}, nil
}

// GuestAgentSupport reports the host's guest-agent capability without probing a
// backend, so `tbx doctor` can explain the gate with the daemon down. QEMU can
// always carry the channel; only the extension itself is optional.
func GuestAgentSupport() FeatureStatus {
	return FeatureStatus{Supported: true}
}

func probeKVM(path string) error {
	return probeKVMWith(path, func(fd uintptr) (int, error) {
		version, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, uintptr(0xae00), 0) // KVM_GET_API_VERSION
		if errno != 0 {
			return 0, errno
		}
		return int(version), nil
	})
}

func probeKVMWith(path string, apiVersion func(uintptr) (int, error)) error {
	device, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("%w: KVM device %s is not usable: %v", ErrUnsupported, path, err)
	}
	version, versionErr := apiVersion(device.Fd())
	closeErr := device.Close()
	if versionErr != nil {
		return fmt.Errorf("%w: query KVM API on %s: %v", ErrUnsupported, path, versionErr)
	}
	if version != 12 {
		return fmt.Errorf("%w: KVM API version 12 is required (found %d)", ErrUnsupported, version)
	}
	if closeErr != nil {
		return fmt.Errorf("close KVM device %s: %w", path, closeErr)
	}
	return nil
}

func verifyQMPPeer(connection net.Conn, process *qemuProcess) error {
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("QMP connection has type %T, want Unix socket", connection)
	}
	if process == nil || process.process == nil {
		return errors.New("QEMU process identity is unavailable")
	}
	raw, err := unixConnection.SyscallConn()
	if err != nil {
		return fmt.Errorf("access QMP socket: %w", err)
	}
	var credentials *unix.Ucred
	var credentialErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, credentialErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return fmt.Errorf("inspect QMP peer: %w", err)
	}
	if credentialErr != nil {
		return fmt.Errorf("inspect QMP peer credentials: %w", credentialErr)
	}
	if credentials.Pid != int32(process.process.Pid) || credentials.Uid != uint32(os.Getuid()) {
		return fmt.Errorf(
			"QMP peer pid=%d uid=%d does not match QEMU pid=%d uid=%d",
			credentials.Pid,
			credentials.Uid,
			process.process.Pid,
			os.Getuid(),
		)
	}
	return nil
}

func newQEMUConsoleProxy(path string) (*consoleProxy, *os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, nil, fmt.Errorf("create QEMU console directory: %w", err)
	}
	descriptors, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("create QEMU console socketpair: %w", err)
	}
	hostFile := os.NewFile(uintptr(descriptors[0]), "qemu-console-host")
	guestFile := os.NewFile(uintptr(descriptors[1]), "qemu-console-guest")
	if hostFile == nil || guestFile == nil {
		if hostFile != nil {
			_ = hostFile.Close()
		} else {
			_ = unix.Close(descriptors[0])
		}
		if guestFile != nil {
			_ = guestFile.Close()
		} else {
			_ = unix.Close(descriptors[1])
		}
		return nil, nil, errors.New("wrap QEMU console socketpair")
	}
	hostConnection, err := net.FileConn(hostFile)
	_ = hostFile.Close()
	if err != nil {
		_ = guestFile.Close()
		return nil, nil, fmt.Errorf("open QEMU console socketpair: %w", err)
	}
	listener, err := listenUnix(path, "console")
	if err != nil {
		_ = hostConnection.Close()
		_ = guestFile.Close()
		return nil, nil, err
	}
	proxy := startConsoleProxy(listener, hostConnection, hostConnection, hostConnection.Close)
	return proxy, guestFile, nil
}
