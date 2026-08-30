//go:build darwin

package hypervisor

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	qemuDarwinInstallRemediation     = "install QEMU: brew install qemu, then restart tbxd"
	qemuDarwinHVFBuildRemediation    = "HVF not built in: Homebrew builds without HVF on macOS 14; upgrade to macOS 15+ and reinstall QEMU"
	qemuDarwinHostRemediation        = "use a Mac with Hypervisor.framework support enabled"
	qemuDarwinEntitlementRemediation = "install or reinstall a signed Homebrew/nixpkgs QEMU; do not re-sign it without the hypervisor entitlement"
	qemuDarwinFirmwareRemediation    = "reinstall QEMU so its edk2 firmware is present"
	qemuDarwinUpgradeRemediation     = "upgrade QEMU to 6.2 or newer: brew upgrade qemu, then restart tbxd"
)

type qemuDarwinProbeDeps struct {
	lookPath     func(string) (string, error)
	accelHelp    func(context.Context, string) ([]byte, error)
	sysctl       func(string) (uint32, error)
	entitled     func(context.Context, string) (bool, error)
	probe        func(context.Context, string, qemuPeerVerifier) (qemuProbe, error)
	evalSymlinks func(string) (string, error)
	homeDir      func() (string, error)
	user         func() string
	fs           qemuFS
}

type qemuDarwinCommand func(context.Context, string, ...string) ([]byte, error)

func defaultQEMUDarwinProbeDeps() qemuDarwinProbeDeps {
	return qemuDarwinProbeDeps{
		lookPath: exec.LookPath,
		accelHelp: func(ctx context.Context, binary string) ([]byte, error) {
			return runQEMUDarwinCommand(ctx, binary, "-accel", "help")
		},
		sysctl:       unix.SysctlUint32,
		entitled:     qemuDarwinEntitled,
		probe:        probeQEMU,
		evalSymlinks: filepath.EvalSymlinks,
		homeDir:      os.UserHomeDir,
		user:         func() string { return os.Getenv("USER") },
		fs:           osQEMUFS{},
	}
}

func probeDarwinQEMUAccelerator(ctx context.Context, binary string, command qemuDarwinCommand) (bool, error) {
	output, err := command(ctx, binary, "-accel", "help")
	if err != nil {
		return false, err
	}
	return hasDarwinQEMUAccelerator(output, "hvf"), nil
}

func hasDarwinQEMUAccelerator(output []byte, accelerator string) bool {
	for _, line := range strings.Split(string(output), "\n") {
		if strings.TrimSpace(line) == accelerator {
			return true
		}
	}
	return false
}

func runQEMUDarwinCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("run %s: %w (%s)", filepath.Base(name), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func qemuDarwinEntitled(ctx context.Context, binary string) (bool, error) {
	output, err := exec.CommandContext(ctx, "/usr/bin/codesign", "-d", "--entitlements", "-", "--xml", binary).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("inspect QEMU entitlements: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return parseDarwinQEMUEntitlements(output)
}

func parseDarwinQEMUEntitlements(output []byte) (bool, error) {
	start := strings.Index(string(output), "<?xml")
	if start < 0 {
		start = strings.Index(string(output), "<plist")
	}
	if start < 0 {
		return false, errors.New("codesign output did not contain an entitlement plist")
	}
	decoder := xml.NewDecoder(strings.NewReader(string(output[start:])))
	const entitlementKey = "com.apple.security.hypervisor"
	wantValue := false
	entitled := false
	for {
		token, err := decoder.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return entitled, nil
			}
			return false, fmt.Errorf("decode QEMU entitlement plist: %w", err)
		}
		switch value := token.(type) {
		case xml.StartElement:
			switch value.Name.Local {
			case "key":
				var key string
				if err := decoder.DecodeElement(&key, &value); err != nil {
					return false, fmt.Errorf("decode QEMU entitlement key: %w", err)
				}
				wantValue = strings.TrimSpace(key) == entitlementKey
			case "true":
				if wantValue {
					entitled = true
				}
				wantValue = false
			case "false":
				wantValue = false
			default:
				if wantValue {
					wantValue = false
				}
			}
		}
	}
}

func darwinQEMUFirmwareCandidates(resolvedBinary, homeDir, user string, arch Architecture) []qemuFirmwareCandidate {
	roots := []string{
		filepath.Clean(filepath.Join(filepath.Dir(resolvedBinary), "..", "share", "qemu")),
		"/opt/homebrew/opt/qemu/share/qemu",
		"/usr/local/opt/qemu/share/qemu",
		"/run/current-system/sw/share/qemu",
	}
	if homeDir != "" {
		roots = append(roots, filepath.Join(homeDir, ".nix-profile", "share", "qemu"))
	}
	if user != "" {
		roots = append(roots, filepath.Join("/etc/profiles/per-user", user, "share", "qemu"))
	}
	roots = append(roots, "/nix/var/nix/profiles/default/share/qemu")

	var codeName, varsName string
	switch arch {
	case ArchitectureARM64:
		codeName, varsName = "edk2-aarch64-code.fd", "edk2-arm-vars.fd"
	case ArchitectureAMD64:
		codeName, varsName = "edk2-x86_64-code.fd", "edk2-i386-vars.fd"
	default:
		return nil
	}
	seen := make(map[string]bool, len(roots))
	candidates := make([]qemuFirmwareCandidate, 0, len(roots))
	for _, root := range roots {
		root = filepath.Clean(root)
		if seen[root] {
			continue
		}
		seen[root] = true
		candidates = append(candidates, qemuFirmwareCandidate{
			CodePath: filepath.Join(root, codeName),
			VarsPath: filepath.Join(root, varsName),
		})
	}
	return candidates
}
