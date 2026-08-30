package hypervisor

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/randax/talos-box/internal/helper"
)

const qemuIncomingOffset = 1 << 20

// guestAgentPortName is the virtio-serial port name the QEMU guest agent inside
// the guest waits for; it is protocol, not a tbx choice.
const guestAgentPortName = "org.qemu.guest_agent.0"

type qemuVersion struct {
	Major int
	Minor int
	Patch int
}

func (v qemuVersion) Compare(other qemuVersion) int {
	switch {
	case v.Major != other.Major:
		return compareInt(v.Major, other.Major)
	case v.Minor != other.Minor:
		return compareInt(v.Minor, other.Minor)
	default:
		return compareInt(v.Patch, other.Patch)
	}
}

func (v qemuVersion) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

type qemuSystem struct {
	Binary  string
	Machine string
}

type qemuFirmware struct {
	CodePath string
	VarsPath string
}

type qemuFirmwareCandidate struct {
	CodePath string
	VarsPath string
}

type qemuLaunchConfig struct {
	Architecture Architecture
	Machine      string
	Accelerator  string
	CPU          string
	CPUs         int
	MemoryMiB    int
	DiskPath     string
	MAC          string
	NetworkKind  helper.AttachmentKind
	NetworkFD    int
	ConsoleFD    int
	// GuestAgentFD is the inherited listening socket QEMU accepts guest-agent
	// clients on. Zero means no guest-agent channel: QEMU inherits 0-2 as its
	// standard streams, so a descriptor tbx passes is never zero.
	GuestAgentFD int
	// DisableBalloon leaves out the virtio-balloon device (#513).
	DisableBalloon bool
	QMPSocketPath  string
	Firmware       qemuFirmware
	IncomingPath   string
	IncomingOffset int64
}

type qemuWritableFile interface {
	io.Writer
	io.Closer
}

type qemuFS interface {
	Stat(string) (fs.FileInfo, error)
	ReadFile(string) ([]byte, error)
	MkdirAll(string, fs.FileMode) error
	OpenFile(string, int, fs.FileMode) (qemuWritableFile, error)
	Link(string, string) error
	Remove(string) error
}

type osQEMUFS struct{}

func (osQEMUFS) Stat(name string) (fs.FileInfo, error)        { return os.Stat(name) }
func (osQEMUFS) ReadFile(name string) ([]byte, error)         { return os.ReadFile(name) }
func (osQEMUFS) MkdirAll(path string, perm fs.FileMode) error { return os.MkdirAll(path, perm) }
func (osQEMUFS) OpenFile(name string, flag int, perm fs.FileMode) (qemuWritableFile, error) {
	return os.OpenFile(name, flag, perm)
}
func (osQEMUFS) Link(oldname, newname string) error { return os.Link(oldname, newname) }
func (osQEMUFS) Remove(name string) error           { return os.Remove(name) }

func qemuSystemForArchitecture(arch Architecture) (qemuSystem, error) {
	switch arch {
	case ArchitectureAMD64:
		return qemuSystem{Binary: "qemu-system-x86_64", Machine: "q35"}, nil
	case ArchitectureARM64:
		return qemuSystem{Binary: "qemu-system-aarch64", Machine: "virt"}, nil
	default:
		return qemuSystem{}, fmt.Errorf("%w: no QEMU system mapping for %s", ErrUnsupported, arch)
	}
}

func discoverQEMUFirmware(fsys qemuFS, arch Architecture, candidates []qemuFirmwareCandidate) (qemuFirmware, error) {
	if fsys == nil {
		fsys = osQEMUFS{}
	}
	if len(candidates) == 0 {
		candidates = qemuFirmwareCandidates(arch)
	}
	for _, candidate := range candidates {
		if err := validateQEMUPath(candidate.CodePath); err != nil {
			return qemuFirmware{}, fmt.Errorf("firmware code path: %w", err)
		}
		if err := validateQEMUPath(candidate.VarsPath); err != nil {
			return qemuFirmware{}, fmt.Errorf("firmware vars path: %w", err)
		}
		if _, err := fsys.Stat(candidate.CodePath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return qemuFirmware{}, fmt.Errorf("stat firmware code %q: %w", candidate.CodePath, err)
		}
		if _, err := fsys.Stat(candidate.VarsPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return qemuFirmware{}, fmt.Errorf("stat firmware vars %q: %w", candidate.VarsPath, err)
		}
		return qemuFirmware(candidate), nil
	}
	return qemuFirmware{}, fmt.Errorf("%w: no matching EFI firmware pair found for %s", ErrUnsupported, arch)
}

func qemuFirmwareCandidates(arch Architecture) []qemuFirmwareCandidate {
	switch arch {
	case ArchitectureAMD64:
		return []qemuFirmwareCandidate{
			{CodePath: "/usr/share/OVMF/OVMF_CODE_4M.fd", VarsPath: "/usr/share/OVMF/OVMF_VARS_4M.fd"},
			{CodePath: "/usr/share/OVMF/OVMF_CODE.fd", VarsPath: "/usr/share/OVMF/OVMF_VARS.fd"},
			{CodePath: "/usr/share/edk2/ovmf/OVMF_CODE.fd", VarsPath: "/usr/share/edk2/ovmf/OVMF_VARS.fd"},
			{CodePath: "/usr/share/edk2/x64/OVMF_CODE.4m.fd", VarsPath: "/usr/share/edk2/x64/OVMF_VARS.4m.fd"},
			{CodePath: "/run/current-system/sw/share/qemu/edk2-x86_64-code.fd", VarsPath: "/run/current-system/sw/share/qemu/edk2-i386-vars.fd"},
		}
	case ArchitectureARM64:
		return []qemuFirmwareCandidate{
			{CodePath: "/usr/share/AAVMF/AAVMF_CODE_4M.fd", VarsPath: "/usr/share/AAVMF/AAVMF_VARS_4M.fd"},
			{CodePath: "/usr/share/AAVMF/AAVMF_CODE.fd", VarsPath: "/usr/share/AAVMF/AAVMF_VARS.fd"},
			{CodePath: "/usr/share/edk2/aarch64/QEMU_EFI.fd", VarsPath: "/usr/share/edk2/aarch64/vars-template-pflash.raw"},
			{CodePath: "/usr/share/edk2/aarch64/QEMU_CODE.fd", VarsPath: "/usr/share/edk2/aarch64/QEMU_VARS.fd"},
			{CodePath: "/run/current-system/sw/share/qemu/edk2-aarch64-code.fd", VarsPath: "/run/current-system/sw/share/qemu/edk2-arm-vars.fd"},
		}
	default:
		return nil
	}
}

func ensureQEMUVars(fsys qemuFS, templatePath, vmVarsPath string) error {
	if fsys == nil {
		fsys = osQEMUFS{}
	}
	if err := validateQEMUPath(templatePath); err != nil {
		return fmt.Errorf("template vars path: %w", err)
	}
	if err := validateQEMUPath(vmVarsPath); err != nil {
		return fmt.Errorf("VM vars path: %w", err)
	}
	if _, err := fsys.Stat(vmVarsPath); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect VM vars path: %w", err)
	}

	data, err := fsys.ReadFile(templatePath)
	if err != nil {
		return fmt.Errorf("read firmware vars template: %w", err)
	}
	if err := fsys.MkdirAll(filepath.Dir(vmVarsPath), 0o755); err != nil {
		return fmt.Errorf("create VM vars directory: %w", err)
	}

	tmp, tmpPath, err := createQEMUTempFile(fsys, filepath.Dir(vmVarsPath), filepath.Base(vmVarsPath))
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			_ = fsys.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write VM vars temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close VM vars temp file: %w", err)
	}
	if err := fsys.Link(tmpPath, vmVarsPath); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return fmt.Errorf("publish VM vars file: %w", err)
	}
	published = true
	if err := fsys.Remove(tmpPath); err != nil {
		return fmt.Errorf("remove VM vars temp file: %w", err)
	}
	return nil
}

func createQEMUTempFile(fsys qemuFS, dir, base string) (qemuWritableFile, string, error) {
	for attempt := 0; attempt < 16; attempt++ {
		name := fmt.Sprintf(".%s.%d.%d.tmp", base, time.Now().UnixNano(), attempt)
		path := filepath.Join(dir, name)
		file, err := fsys.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, "", fmt.Errorf("create VM vars temp file: %w", err)
		}
		return file, path, nil
	}
	return nil, "", errors.New("create VM vars temp file: exhausted unique names")
}

func buildQEMUArgv(cfg qemuLaunchConfig) ([]string, error) {
	system, err := qemuSystemForArchitecture(cfg.Architecture)
	if err != nil {
		return nil, err
	}
	if err := validateQEMULaunchConfig(cfg); err != nil {
		return nil, err
	}
	var netdev string
	switch cfg.NetworkKind {
	case helper.AttachmentTapFD:
		netdev = qemuOption("tap", "id=net0", "fd="+strconv.Itoa(cfg.NetworkFD))
	case helper.AttachmentDatagramFD:
		netdev = qemuOption("dgram", "id=net0", "local.type=fd", "local.str="+strconv.Itoa(cfg.NetworkFD))
	}

	args := []string{
		system.Binary,
		"-nodefaults",
		"-display", "none",
		"-no-reboot",
		"-S",
		"-machine", cfg.Machine,
		"-accel", cfg.Accelerator,
		"-cpu", cfg.CPU,
		"-smp", strconv.Itoa(cfg.CPUs),
		"-m", strconv.Itoa(cfg.MemoryMiB),
		"-drive", qemuOption(
			"if=pflash",
			"format=raw",
			"unit=0",
			"readonly=on",
			"file="+qemuOptionValue(cfg.Firmware.CodePath),
		),
		"-drive", qemuOption(
			"if=pflash",
			"format=raw",
			"unit=1",
			"file="+qemuOptionValue(cfg.Firmware.VarsPath),
		),
		"-blockdev", qemuOption(
			"driver=file",
			"node-name=osdisk-file",
			"filename="+qemuOptionValue(cfg.DiskPath),
		),
		"-blockdev", qemuOption(
			"driver=raw",
			"node-name=osdisk",
			"file=osdisk-file",
		),
		"-device", qemuOption(
			"virtio-blk-pci",
			"drive=osdisk",
		),
		"-netdev", netdev,
		"-device", qemuOption(
			"virtio-net-pci",
			"netdev=net0",
			"mac="+qemuOptionValue(cfg.MAC),
		),
		"-object", qemuOption(
			"rng-random",
			"id=rng0",
			"filename=/dev/urandom",
		),
		"-device", qemuOption(
			"virtio-rng-pci",
			"rng=rng0",
		),
	}
	if !cfg.DisableBalloon {
		args = append(args, "-device", qemuOption(
			"virtio-balloon-pci",
			"deflate-on-oom=on",
			"free-page-reporting=on",
		))
	}
	args = append(args, "-chardev", qemuOption(
		"socket",
		"id=charconsole",
		"fd="+strconv.Itoa(cfg.ConsoleFD),
	))
	// The virtio-serial controller carries the console on port 0; the guest
	// agent needs a second port, and only clusters that asked for it get the
	// wider controller so every other device graph stays byte-identical.
	serialPorts := 1
	if cfg.GuestAgentFD != 0 {
		serialPorts = 2
		args = append(args, "-chardev", qemuOption(
			"socket",
			"id=charqga",
			"fd="+strconv.Itoa(cfg.GuestAgentFD),
			"server=on",
			"wait=off",
		))
	}
	args = append(args,
		"-device", qemuOption(
			"virtio-serial-pci",
			"id=virtioconsole0",
			"max_ports="+strconv.Itoa(serialPorts),
		),
		"-device", qemuOption(
			"virtconsole",
			"chardev=charconsole",
		),
	)
	if cfg.GuestAgentFD != 0 {
		args = append(args, "-device", qemuOption(
			"virtserialport",
			"chardev=charqga",
			"name="+guestAgentPortName,
		))
	}
	args = append(args,
		"-qmp", qemuOption(
			"unix:"+qemuOptionValue(cfg.QMPSocketPath),
			"server=on",
			"wait=off",
		),
	)
	if cfg.IncomingPath != "" {
		offset := cfg.IncomingOffset
		if offset == 0 {
			offset = qemuIncomingOffset
		}
		args = append(args, "-incoming", fmt.Sprintf("file:%s,offset=%d", qemuOptionValue(cfg.IncomingPath), offset))
	}
	return args, nil
}

func validateQEMULaunchConfig(cfg qemuLaunchConfig) error {
	switch {
	case cfg.Machine == "":
		return errors.New("QEMU machine is required")
	case cfg.Accelerator == "":
		return errors.New("QEMU accelerator is required")
	case cfg.CPU == "":
		return errors.New("QEMU CPU is required")
	case cfg.CPUs <= 0:
		return errors.New("QEMU CPUs must be greater than zero")
	case cfg.MemoryMiB <= 0:
		return errors.New("QEMU memory must be greater than zero")
	case cfg.NetworkFD < 0:
		return errors.New("QEMU network FD must be non-negative")
	case cfg.ConsoleFD < 0:
		return errors.New("QEMU console FD must be non-negative")
	case cfg.GuestAgentFD < 0:
		return errors.New("QEMU guest-agent FD must be non-negative")
	case cfg.Firmware.CodePath == "":
		return errors.New("QEMU firmware code path is required")
	case cfg.Firmware.VarsPath == "":
		return errors.New("QEMU firmware vars path is required")
	}
	switch cfg.NetworkKind {
	case helper.AttachmentTapFD, helper.AttachmentDatagramFD:
	default:
		return fmt.Errorf("%w: QEMU cannot use network attachment kind %q", ErrUnsupported, cfg.NetworkKind)
	}
	if _, err := net.ParseMAC(cfg.MAC); err != nil {
		return fmt.Errorf("parse QEMU MAC address: %w", err)
	}
	for _, item := range []struct {
		label string
		path  string
	}{
		{label: "disk", path: cfg.DiskPath},
		{label: "QMP socket", path: cfg.QMPSocketPath},
		{label: "firmware code", path: cfg.Firmware.CodePath},
		{label: "firmware vars", path: cfg.Firmware.VarsPath},
	} {
		if err := validateQEMUPath(item.path); err != nil {
			return fmt.Errorf("%s path: %w", item.label, err)
		}
	}
	if cfg.IncomingPath != "" {
		if err := validateQEMUPath(cfg.IncomingPath); err != nil {
			return fmt.Errorf("incoming path: %w", err)
		}
		if cfg.IncomingOffset < 0 {
			return errors.New("QEMU incoming offset must be non-negative")
		}
	}
	return nil
}

func validateQEMUPath(path string) error {
	switch {
	case path == "":
		return errors.New("path is required")
	case strings.ContainsRune(path, 0):
		return errors.New("path contains NUL")
	case strings.ContainsAny(path, "\r\n"):
		return errors.New("path contains a line break")
	default:
		return nil
	}
}

func qemuOption(parts ...string) string {
	return strings.Join(parts, ",")
}

func qemuOptionValue(value string) string {
	return strings.ReplaceAll(value, ",", ",,")
}

func compareInt(left, right int) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}
