//go:build darwin

package hypervisor

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestDarwinQEMUAvailabilityProbe(t *testing.T) {
	t.Parallel()

	architecture := Architecture(runtime.GOARCH)
	system, err := qemuSystemForArchitecture(architecture)
	if err != nil {
		t.Fatal(err)
	}
	probeErr := errors.New("probe failed")
	tests := []struct {
		name            string
		change          func(*qemuDarwinProbeDeps)
		wantReason      string
		wantRemediation string
		wantWrapped     error
	}{
		{
			name: "binary missing",
			change: func(deps *qemuDarwinProbeDeps) {
				deps.lookPath = func(string) (string, error) { return "", probeErr }
			},
			wantReason:      system.Binary + " was not found on PATH",
			wantRemediation: "install QEMU: brew install qemu, then restart tbxd",
			wantWrapped:     probeErr,
		},
		{
			name: "hvf absent",
			change: func(deps *qemuDarwinProbeDeps) {
				deps.accelHelp = func(context.Context, string) ([]byte, error) { return []byte("tcg\n"), nil }
			},
			wantReason:      "QEMU does not list the hvf accelerator",
			wantRemediation: "HVF not built in: Homebrew builds without HVF on macOS 14; upgrade to macOS 15+ and reinstall QEMU",
		},
		{
			name: "sysctl zero",
			change: func(deps *qemuDarwinProbeDeps) {
				deps.sysctl = func(string) (uint32, error) { return 0, nil }
			},
			wantReason:      "HVF denied: kern.hv_support is not 1",
			wantRemediation: "use a Mac with Hypervisor.framework support enabled",
		},
		{
			name: "entitlement false",
			change: func(deps *qemuDarwinProbeDeps) {
				deps.entitled = func(context.Context, string) (bool, error) { return false, nil }
			},
			wantReason:      "HVF denied: /test/bin/" + system.Binary + " lacks com.apple.security.hypervisor",
			wantRemediation: "install or reinstall a signed Homebrew/nixpkgs QEMU; do not re-sign it without the hypervisor entitlement",
		},
		{
			name: "capability probe failed",
			change: func(deps *qemuDarwinProbeDeps) {
				deps.probe = func(context.Context, string, qemuPeerVerifier) (qemuProbe, error) {
					return qemuProbe{}, probeErr
				}
			},
			wantReason:      "probe QEMU: probe failed",
			wantRemediation: "upgrade QEMU to 6.2 or newer: brew upgrade qemu, then restart tbxd",
			wantWrapped:     probeErr,
		},
		{
			name: "QEMU too old",
			change: func(deps *qemuDarwinProbeDeps) {
				deps.probe = func(context.Context, string, qemuPeerVerifier) (qemuProbe, error) {
					return qemuProbe{Version: qemuVersion{Major: 6, Minor: 1}, Machines: []string{system.Machine}}, nil
				}
			},
			wantReason:      "hypervisor feature unsupported: QEMU >= 6.2 is required (found 6.1.0)",
			wantRemediation: "upgrade QEMU to 6.2 or newer: brew upgrade qemu, then restart tbxd",
			wantWrapped:     ErrUnsupported,
		},
		{
			name: "binary path resolution failed",
			change: func(deps *qemuDarwinProbeDeps) {
				deps.evalSymlinks = func(string) (string, error) { return "", probeErr }
			},
			wantReason:      "resolve QEMU binary path: probe failed",
			wantRemediation: "upgrade QEMU to 6.2 or newer: brew upgrade qemu, then restart tbxd",
			wantWrapped:     probeErr,
		},
		{
			name: "firmware absent",
			change: func(deps *qemuDarwinProbeDeps) {
				deps.fs = missingQEMUFirmwareFS{}
			},
			wantReason:      "no matching EFI firmware pair found for " + string(architecture),
			wantRemediation: "reinstall QEMU so its edk2 firmware is present",
			wantWrapped:     ErrUnsupported,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			deps := passingDarwinQEMUProbeDeps(t, architecture)
			test.change(&deps)
			_, err := newQEMUWith(context.Background(), deps)
			var unavailable unavailableError
			if !errors.As(err, &unavailable) {
				t.Fatalf("newQEMUWith() error = %v, want unavailableError", err)
			}
			if unavailable.reason != test.wantReason || unavailable.remediation != test.wantRemediation {
				t.Fatalf("availability = reason %q remediation %q, want %q / %q", unavailable.reason, unavailable.remediation, test.wantReason, test.wantRemediation)
			}
			if test.wantWrapped != nil && !errors.Is(err, test.wantWrapped) {
				t.Fatalf("newQEMUWith() error = %v, want wrapped %v", err, test.wantWrapped)
			}
		})
	}

	t.Run("all gates pass", func(t *testing.T) {
		t.Parallel()
		backend, err := newQEMUWith(context.Background(), passingDarwinQEMUProbeDeps(t, architecture))
		if err != nil {
			t.Fatal(err)
		}
		if backend == nil {
			t.Fatal("newQEMUWith() returned a nil hypervisor")
		}
	})
}

func TestDarwinQEMUAcceleratorProbeUsesAccelHelp(t *testing.T) {
	t.Parallel()

	var gotName string
	var gotArgs []string
	found, err := probeDarwinQEMUAccelerator(context.Background(), "/test/qemu", func(_ context.Context, name string, args ...string) ([]byte, error) {
		gotName = name
		gotArgs = append([]string(nil), args...)
		return []byte("Accelerators supported in QEMU binary:\n hvf\n tcg\n"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("probe did not find hvf")
	}
	if gotName != "/test/qemu" || !reflect.DeepEqual(gotArgs, []string{"-accel", "help"}) {
		t.Fatalf("command = %q %q, want %q", gotName, gotArgs, []string{"-accel", "help"})
	}
}

func TestDarwinQEMUAcceleratorProbeRequiresWholeLine(t *testing.T) {
	t.Parallel()

	for _, output := range []string{"whvf\n", "hvf accelerator\n", "tcg, hvf\n"} {
		found, err := probeDarwinQEMUAccelerator(context.Background(), "qemu", func(context.Context, string, ...string) ([]byte, error) {
			return []byte(output), nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if found {
			t.Fatalf("probe accepted non-line hvf in %q", output)
		}
	}
}

func TestDarwinQEMUEntitlementParser(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		xml     string
		want    bool
		wantErr bool
	}{
		{name: "true key", xml: plistWithHypervisor("<true/>"), want: true},
		{name: "false key", xml: plistWithHypervisor("<false/>")},
		{name: "missing key", xml: `<?xml version="1.0"?><plist><dict><key>com.apple.security.network.client</key><true/></dict></plist>`},
		{name: "malformed XML", xml: `<?xml version="1.0"?><plist><dict><key>com.apple.security.hypervisor</key><true/>`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseDarwinQEMUEntitlements([]byte("Executable=/test/qemu\n" + test.xml))
			if (err != nil) != test.wantErr {
				t.Fatalf("parseDarwinQEMUEntitlements() error = %v, wantErr %v", err, test.wantErr)
			}
			if got != test.want {
				t.Fatalf("parseDarwinQEMUEntitlements() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestDarwinQEMUFirmwareCandidates(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "Users", "test-user")
	resolved := filepath.Join(string(filepath.Separator), "nix", "store", "qemu-hash", "bin", "qemu-system-aarch64")
	candidates := darwinQEMUFirmwareCandidates(resolved, home, "test-user", ArchitectureARM64)
	wantRoots := []string{
		filepath.Join(string(filepath.Separator), "nix", "store", "qemu-hash", "share", "qemu"),
		"/opt/homebrew/opt/qemu/share/qemu",
		"/usr/local/opt/qemu/share/qemu",
		"/run/current-system/sw/share/qemu",
		filepath.Join(home, ".nix-profile", "share", "qemu"),
		"/etc/profiles/per-user/test-user/share/qemu",
		"/nix/var/nix/profiles/default/share/qemu",
	}
	if len(candidates) != len(wantRoots) {
		t.Fatalf("candidate count = %d, want %d: %+v", len(candidates), len(wantRoots), candidates)
	}
	for index, root := range wantRoots {
		want := qemuFirmwareCandidate{
			CodePath: filepath.Join(root, "edk2-aarch64-code.fd"),
			VarsPath: filepath.Join(root, "edk2-arm-vars.fd"),
		}
		if candidates[index] != want {
			t.Fatalf("candidate[%d] = %+v, want %+v", index, candidates[index], want)
		}
		if strings.Contains(candidates[index].CodePath, ".json") || strings.Contains(candidates[index].VarsPath, ".json") {
			t.Fatalf("candidate includes descriptor JSON: %+v", candidates[index])
		}
	}

	amd64 := darwinQEMUFirmwareCandidates(resolved, home, "test-user", ArchitectureAMD64)
	if !slices.Contains(amd64, qemuFirmwareCandidate{
		CodePath: "/opt/homebrew/opt/qemu/share/qemu/edk2-x86_64-code.fd",
		VarsPath: "/opt/homebrew/opt/qemu/share/qemu/edk2-i386-vars.fd",
	}) {
		t.Fatalf("amd64 candidates do not contain the Homebrew firmware pair: %+v", amd64)
	}
}

func TestDarwinQEMUUsesInjectedUserForFirmwareCandidates(t *testing.T) {
	t.Parallel()

	deps := passingDarwinQEMUProbeDeps(t, Architecture(runtime.GOARCH))
	called := false
	deps.user = func() string {
		called = true
		return "test-user"
	}
	if _, err := newQEMUWith(context.Background(), deps); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("newQEMUWith did not use the injected user dependency")
	}
}

func TestDarwinQEMUFirmwareVarsPersist(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	templatePath := filepath.Join(dir, "share", "qemu", "edk2-arm-vars.fd")
	vmVarsPath := filepath.Join(dir, "vm", "node.efi")
	writeFile(t, templatePath, "template")
	if err := ensureQEMUVars(osQEMUFS{}, templatePath, vmVarsPath); err != nil {
		t.Fatal(err)
	}
	writeFile(t, vmVarsPath, "persistent")
	writeFile(t, templatePath, "new template")
	if err := ensureQEMUVars(osQEMUFS{}, templatePath, vmVarsPath); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, vmVarsPath); got != "persistent" {
		t.Fatalf("vars contents = %q, want persistent", got)
	}
}

func passingDarwinQEMUProbeDeps(t *testing.T, architecture Architecture) qemuDarwinProbeDeps {
	t.Helper()
	dir := t.TempDir()
	system, err := qemuSystemForArchitecture(architecture)
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join("/test/bin", system.Binary)
	resolved := filepath.Join(dir, "bin", system.Binary)
	firmwareRoot := filepath.Join(dir, "share", "qemu")
	var codeName, varsName string
	switch architecture {
	case ArchitectureARM64:
		codeName, varsName = "edk2-aarch64-code.fd", "edk2-arm-vars.fd"
	case ArchitectureAMD64:
		codeName, varsName = "edk2-x86_64-code.fd", "edk2-i386-vars.fd"
	default:
		t.Fatalf("unsupported test architecture %q", architecture)
	}
	writeFile(t, filepath.Join(firmwareRoot, codeName), "code")
	writeFile(t, filepath.Join(firmwareRoot, varsName), "vars")
	return qemuDarwinProbeDeps{
		lookPath:  func(string) (string, error) { return binary, nil },
		accelHelp: func(context.Context, string) ([]byte, error) { return []byte("hvf\ntcg\n"), nil },
		sysctl:    func(string) (uint32, error) { return 1, nil },
		entitled:  func(context.Context, string) (bool, error) { return true, nil },
		probe: func(_ context.Context, _ string, verifyPeer qemuPeerVerifier) (qemuProbe, error) {
			if verifyPeer == nil {
				t.Fatal("Darwin QEMU probe did not receive a peer verifier")
			}
			return qemuProbe{Version: qemuVersion{Major: 11, Minor: 1, Patch: 1}, Machines: []string{system.Machine}}, nil
		},
		evalSymlinks: func(string) (string, error) { return resolved, nil },
		homeDir:      func() (string, error) { return filepath.Join(dir, "home"), nil },
		user:         func() string { return "test-user" },
		fs:           osQEMUFS{},
	}
}

func plistWithHypervisor(value string) string {
	return `<?xml version="1.0"?><plist><dict><key>com.apple.security.hypervisor</key>` + value + `</dict></plist>`
}

type missingQEMUFirmwareFS struct{ qemuFS }

func (missingQEMUFirmwareFS) Stat(string) (fs.FileInfo, error) { return nil, os.ErrNotExist }
