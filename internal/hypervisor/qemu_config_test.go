package hypervisor

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestQEMUVersionCompareAndString(t *testing.T) {
	t.Parallel()

	v := qemuVersion{Major: 8, Minor: 2, Patch: 1}
	if got := v.String(); got != "8.2.1" {
		t.Fatalf("String() = %q, want %q", got, "8.2.1")
	}
	if got := v.Compare(qemuVersion{Major: 8, Minor: 2, Patch: 0}); got != 1 {
		t.Fatalf("Compare(older) = %d, want 1", got)
	}
	if got := v.Compare(qemuVersion{Major: 8, Minor: 2, Patch: 1}); got != 0 {
		t.Fatalf("Compare(equal) = %d, want 0", got)
	}
	if got := v.Compare(qemuVersion{Major: 9, Minor: 0, Patch: 0}); got != -1 {
		t.Fatalf("Compare(newer) = %d, want -1", got)
	}
}

func TestQEMUSystemForArchitecture(t *testing.T) {
	t.Parallel()

	amd64, err := qemuSystemForArchitecture(ArchitectureAMD64)
	if err != nil {
		t.Fatalf("qemuSystemForArchitecture(amd64): %v", err)
	}
	if amd64.Binary != "qemu-system-x86_64" || amd64.Machine != "q35" {
		t.Fatalf("amd64 mapping = %+v, want qemu-system-x86_64/q35", amd64)
	}

	arm64, err := qemuSystemForArchitecture(ArchitectureARM64)
	if err != nil {
		t.Fatalf("qemuSystemForArchitecture(arm64): %v", err)
	}
	if arm64.Binary != "qemu-system-aarch64" || arm64.Machine != "virt" {
		t.Fatalf("arm64 mapping = %+v, want qemu-system-aarch64/virt", arm64)
	}
}

func TestDiscoverQEMUFirmwareFindsFirstMatchingPair(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	codeOnly := filepath.Join(dir, "OVMF_CODE.fd")
	varsOnly := filepath.Join(dir, "OVMF_VARS.fd")
	matchCode := filepath.Join(dir, "AAVMF_CODE.fd")
	matchVars := filepath.Join(dir, "AAVMF_VARS.fd")
	writeFile(t, codeOnly, "code only")
	writeFile(t, varsOnly, "vars only")
	writeFile(t, matchCode, "match code")
	writeFile(t, matchVars, "match vars")

	got, err := discoverQEMUFirmware(osQEMUFS{}, ArchitectureARM64, []qemuFirmwareCandidate{
		{CodePath: codeOnly, VarsPath: filepath.Join(dir, "missing-vars.fd")},
		{CodePath: filepath.Join(dir, "missing-code.fd"), VarsPath: varsOnly},
		{CodePath: matchCode, VarsPath: matchVars},
	})
	if err != nil {
		t.Fatalf("discoverQEMUFirmware() error = %v", err)
	}
	if got != (qemuFirmware{CodePath: matchCode, VarsPath: matchVars}) {
		t.Fatalf("discoverQEMUFirmware() = %+v, want %+v", got, qemuFirmware{CodePath: matchCode, VarsPath: matchVars})
	}
}

func TestEnsureQEMUVarsCopiesOnceWithoutOverwrite(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	templatePath := filepath.Join(dir, "templates", "OVMF_VARS.fd")
	vmVarsPath := filepath.Join(dir, "vm", "node.vars.fd")
	writeFile(t, templatePath, "template")

	if err := ensureQEMUVars(osQEMUFS{}, templatePath, vmVarsPath); err != nil {
		t.Fatalf("ensureQEMUVars(first): %v", err)
	}
	if got := readFile(t, vmVarsPath); got != "template" {
		t.Fatalf("first vars content = %q, want %q", got, "template")
	}

	writeFile(t, vmVarsPath, "customized")
	writeFile(t, templatePath, "updated template")

	if err := ensureQEMUVars(osQEMUFS{}, templatePath, vmVarsPath); err != nil {
		t.Fatalf("ensureQEMUVars(second): %v", err)
	}
	if got := readFile(t, vmVarsPath); got != "customized" {
		t.Fatalf("second vars content = %q, want existing contents preserved", got)
	}

	entries, err := filepath.Glob(filepath.Join(filepath.Dir(vmVarsPath), ".node.vars.fd.*.tmp"))
	if err != nil {
		t.Fatalf("Glob() error = %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary vars files left behind: %v", entries)
	}
}

func TestBuildQEMUArgv(t *testing.T) {
	t.Parallel()

	got, err := buildQEMUArgv(qemuLaunchConfig{
		Architecture:  ArchitectureAMD64,
		CPUs:          4,
		MemoryMiB:     8192,
		DiskPath:      "/var/lib/talos/node,disk.img",
		MAC:           "02:00:00:00:00:01",
		TapFD:         9,
		ConsoleFD:     7,
		QMPSocketPath: "/run/talos/qmp,node.sock",
		Firmware: qemuFirmware{
			CodePath: "/usr/share/OVMF/OVMF_CODE,4M.fd",
			VarsPath: "/var/lib/talos/node,VARS.fd",
		},
		IncomingPath: "/var/lib/talos/save state.bin",
	})
	if err != nil {
		t.Fatalf("buildQEMUArgv() error = %v", err)
	}

	want := []string{
		"qemu-system-x86_64",
		"-nodefaults",
		"-display", "none",
		"-no-reboot",
		"-S",
		"-machine", "q35",
		"-accel", "kvm",
		"-cpu", "host",
		"-smp", "4",
		"-m", "8192",
		"-drive", "if=pflash,format=raw,unit=0,readonly=on,file=/usr/share/OVMF/OVMF_CODE,,4M.fd",
		"-drive", "if=pflash,format=raw,unit=1,file=/var/lib/talos/node,,VARS.fd",
		"-blockdev", "driver=file,node-name=osdisk-file,filename=/var/lib/talos/node,,disk.img",
		"-blockdev", "driver=raw,node-name=osdisk,file=osdisk-file",
		"-device", "virtio-blk-pci,drive=osdisk",
		"-netdev", "tap,id=net0,fd=9",
		"-device", "virtio-net-pci,netdev=net0,mac=02:00:00:00:00:01",
		"-object", "rng-random,id=rng0,filename=/dev/urandom",
		"-device", "virtio-rng-pci,rng=rng0",
		"-device", "virtio-balloon-pci,deflate-on-oom=on,free-page-reporting=on",
		"-chardev", "socket,id=charconsole,fd=7",
		// Without a guest agent the serial topology stays exactly one port, so
		// clusters that never requested it keep their existing device graph.
		"-device", "virtio-serial-pci,id=virtioconsole0,max_ports=1",
		"-device", "virtconsole,chardev=charconsole",
		"-qmp", "unix:/run/talos/qmp,,node.sock,server=on,wait=off",
		"-incoming", "file:/var/lib/talos/save state.bin,offset=1048576",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildQEMUArgv() = %#v, want %#v", got, want)
	}
}

func TestBuildQEMUArgvAddsGuestAgentPort(t *testing.T) {
	t.Parallel()

	got, err := buildQEMUArgv(qemuLaunchConfig{
		Architecture:  ArchitectureAMD64,
		CPUs:          2,
		MemoryMiB:     2048,
		DiskPath:      "/var/lib/talos/node.img",
		MAC:           "02:00:00:00:00:01",
		TapFD:         3,
		ConsoleFD:     4,
		GuestAgentFD:  5,
		QMPSocketPath: "/run/talos/node.qmp.sock",
		Firmware: qemuFirmware{
			CodePath: "/usr/share/OVMF/OVMF_CODE.fd",
			VarsPath: "/var/lib/talos/node.VARS.fd",
		},
	})
	if err != nil {
		t.Fatalf("buildQEMUArgv() error = %v", err)
	}

	want := []string{
		"-chardev", "socket,id=charconsole,fd=4",
		"-chardev", "socket,id=charqga,fd=5,server=on,wait=off",
		"-device", "virtio-serial-pci,id=virtioconsole0,max_ports=2",
		"-device", "virtconsole,chardev=charconsole",
		"-device", "virtserialport,chardev=charqga,name=org.qemu.guest_agent.0",
		"-qmp", "unix:/run/talos/node.qmp.sock,server=on,wait=off",
	}
	if !containsSequence(got, want) {
		t.Fatalf("buildQEMUArgv() = %#v, want serial block %#v", got, want)
	}
}

func containsSequence(got, want []string) bool {
	for start := 0; start+len(want) <= len(got); start++ {
		if reflect.DeepEqual(got[start:start+len(want)], want) {
			return true
		}
	}
	return false
}

func TestBuildQEMUArgvRejectsUnsafePaths(t *testing.T) {
	t.Parallel()

	_, err := buildQEMUArgv(qemuLaunchConfig{
		Architecture:  ArchitectureAMD64,
		CPUs:          1,
		MemoryMiB:     1024,
		DiskPath:      "/var/lib/talos/bad\npath.img",
		MAC:           "02:00:00:00:00:01",
		TapFD:         3,
		ConsoleFD:     4,
		QMPSocketPath: "/run/qmp.sock",
		Firmware: qemuFirmware{
			CodePath: "/usr/share/OVMF/OVMF_CODE.fd",
			VarsPath: "/var/lib/talos/node.vars.fd",
		},
	})
	if err == nil {
		t.Fatal("buildQEMUArgv() succeeded for an unsafe path")
	}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
