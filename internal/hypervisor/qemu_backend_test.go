package hypervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/helper"
)

func TestQEMULaunchValidatesBeforeAcquiringNetwork(t *testing.T) {
	backend := &qemuHypervisor{}
	acquired := false
	_, err := backend.Launch(context.Background(), Spec{
		Network: func() (*helper.Attachment, error) {
			acquired = true
			return nil, nil
		},
	})
	if err == nil {
		t.Fatal("Launch() accepted invalid spec")
	}
	if acquired {
		t.Fatal("Launch() acquired network before validating the spec")
	}
}

func TestQEMULaunchRefusesSaveIdentityMismatchBeforeStarting(t *testing.T) {
	currentVersion := qemuVersion{Major: 8, Minor: 2, Patch: 2}
	currentArchitecture := ArchitectureAMD64
	currentMachine := "q35"
	compatible := qemuSaveMetadata{
		Schema:       qemuSaveSchema,
		Backend:      qemuSaveBackend,
		QEMUVersion:  currentVersion.String(),
		Architecture: currentArchitecture,
		Machine:      currentMachine,
	}
	tests := []struct {
		name     string
		metadata qemuSaveMetadata
	}{
		{name: "backend", metadata: func() qemuSaveMetadata {
			metadata := compatible
			metadata.Backend = "vz"
			return metadata
		}()},
		{name: "architecture", metadata: func() qemuSaveMetadata {
			metadata := compatible
			metadata.Architecture = ArchitectureARM64
			return metadata
		}()},
		{name: "machine", metadata: func() qemuSaveMetadata {
			metadata := compatible
			metadata.Machine = "virt"
			return metadata
		}()},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			savePath := filepath.Join(dir, "node.vzstate")
			encoded, err := json.Marshal(test.metadata)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(savePath, append(encoded, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
			varsTemplate := filepath.Join(dir, "OVMF_VARS.fd")
			if err := os.WriteFile(varsTemplate, []byte("vars"), 0o600); err != nil {
				t.Fatal(err)
			}

			fallbackCalled := false
			networkAcquired := false
			backend := &qemuHypervisor{
				architecture: currentArchitecture,
				system:       qemuSystem{Machine: currentMachine},
				version:      currentVersion,
				capabilities: qemuCapabilities(currentVersion),
				firmware:     qemuFirmware{VarsPath: varsTemplate},
			}
			_, err = backend.Launch(context.Background(), Spec{
				CPUs:              1,
				MemoryMiB:         1024,
				DiskPath:          filepath.Join(dir, "node.img"),
				MAC:               "02:00:00:00:00:01",
				EFIVarsPath:       filepath.Join(dir, "node.efi"),
				ConsoleSocketPath: filepath.Join(dir, "node.console.sock"),
				Network: func() (*helper.Attachment, error) {
					networkAcquired = true
					return nil, errors.New("cold boot attempted")
				},
				Restore: &Restore{Path: savePath, Fallback: func(error) {
					fallbackCalled = true
				}},
			})
			if !errors.Is(err, ErrIncompatibleSave) {
				t.Fatalf("Launch() = %v, want ErrIncompatibleSave", err)
			}
			if fallbackCalled {
				t.Fatal("identity mismatch was reported as a cold-boot fallback")
			}
			if networkAcquired {
				t.Fatal("identity mismatch acquired the network for a cold boot")
			}
			if _, err := os.Stat(savePath); err != nil {
				t.Fatalf("identity mismatch removed retryable save: %v", err)
			}
		})
	}
}

func TestQEMULaunchMalformedSaveKeepsColdBootFallback(t *testing.T) {
	dir := t.TempDir()
	savePath := filepath.Join(dir, "node.vzstate")
	if err := os.WriteFile(savePath, []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	varsTemplate := filepath.Join(dir, "OVMF_VARS.fd")
	if err := os.WriteFile(varsTemplate, []byte("vars"), 0o600); err != nil {
		t.Fatal(err)
	}
	currentVersion := qemuVersion{Major: 8, Minor: 2, Patch: 2}
	backend := &qemuHypervisor{
		architecture: ArchitectureAMD64,
		system:       qemuSystem{Machine: "q35"},
		version:      currentVersion,
		capabilities: qemuCapabilities(currentVersion),
		firmware:     qemuFirmware{VarsPath: varsTemplate},
	}
	coldBootErr := errors.New("cold boot attempted")
	var fallbackErr error
	_, err := backend.Launch(context.Background(), Spec{
		CPUs:              1,
		MemoryMiB:         1024,
		DiskPath:          filepath.Join(dir, "node.img"),
		MAC:               "02:00:00:00:00:01",
		EFIVarsPath:       filepath.Join(dir, "node.efi"),
		ConsoleSocketPath: filepath.Join(dir, "node.console.sock"),
		Network: func() (*helper.Attachment, error) {
			return nil, coldBootErr
		},
		Restore: &Restore{Path: savePath, Fallback: func(err error) {
			fallbackErr = err
		}},
	})
	if !errors.Is(err, coldBootErr) {
		t.Fatalf("Launch() = %v, want cold-boot attempt", err)
	}
	if !errors.Is(fallbackErr, ErrIncompatibleSave) {
		t.Fatalf("fallback = %v, want malformed save reported as ErrIncompatibleSave", fallbackErr)
	}
}

func TestQEMULaunchVersionMismatchKeepsColdBootFallback(t *testing.T) {
	dir := t.TempDir()
	savePath := filepath.Join(dir, "node.vzstate")
	metadata := qemuSaveMetadata{Schema: qemuSaveSchema, Backend: qemuSaveBackend, QEMUVersion: "8.2.1", Architecture: ArchitectureAMD64, Machine: "q35"}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(savePath, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	varsTemplate := filepath.Join(dir, "OVMF_VARS.fd")
	if err := os.WriteFile(varsTemplate, []byte("vars"), 0o600); err != nil {
		t.Fatal(err)
	}
	currentVersion := qemuVersion{Major: 8, Minor: 2, Patch: 2}
	backend := &qemuHypervisor{
		architecture: ArchitectureAMD64,
		system:       qemuSystem{Machine: "q35"},
		version:      currentVersion,
		capabilities: qemuCapabilities(currentVersion),
		firmware:     qemuFirmware{VarsPath: varsTemplate},
	}
	coldBootErr := errors.New("cold boot attempted")
	var fallbackErr error
	_, err = backend.Launch(context.Background(), Spec{
		CPUs:              1,
		MemoryMiB:         1024,
		DiskPath:          filepath.Join(dir, "node.img"),
		MAC:               "02:00:00:00:00:01",
		EFIVarsPath:       filepath.Join(dir, "node.efi"),
		ConsoleSocketPath: filepath.Join(dir, "node.console.sock"),
		Network: func() (*helper.Attachment, error) {
			return nil, coldBootErr
		},
		Restore: &Restore{Path: savePath, Fallback: func(err error) {
			fallbackErr = err
		}},
	})
	if !errors.Is(err, coldBootErr) {
		t.Fatalf("Launch() = %v, want cold-boot attempt after a QEMU upgrade", err)
	}
	if !errors.Is(fallbackErr, ErrIncompatibleSave) || !strings.Contains(fallbackErr.Error(), "8.2.1") {
		t.Fatalf("fallback = %v, want version mismatch reported as ErrIncompatibleSave", fallbackErr)
	}
}

func TestQEMUNewMachineClosesRejectedAttachment(t *testing.T) {
	dir := t.TempDir()
	varsTemplate := filepath.Join(dir, "OVMF_VARS.fd")
	if err := os.WriteFile(varsTemplate, []byte("vars"), 0o600); err != nil {
		t.Fatal(err)
	}
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = write.Close() }()
	backend := &qemuHypervisor{
		firmware: qemuFirmware{VarsPath: varsTemplate},
	}
	_, err = backend.newMachine(Spec{
		CPUs:              1,
		MemoryMiB:         1024,
		DiskPath:          filepath.Join(dir, "node.img"),
		MAC:               "02:00:00:00:00:01",
		EFIVarsPath:       filepath.Join(dir, "node.efi"),
		ConsoleSocketPath: filepath.Join(dir, "node.console.sock"),
		Network: func() (*helper.Attachment, error) {
			return &helper.Attachment{Kind: helper.AttachmentKind("unknown-fd"), File: read}, nil
		},
	})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("newMachine() = %v, want ErrUnsupported", err)
	}
	if _, err := read.Stat(); err == nil {
		t.Fatal("rejected network attachment remained open")
	}
}

func TestQEMUNewMachineAcceptsBothNetworkAttachmentKinds(t *testing.T) {
	tests := []struct {
		name       string
		kind       helper.AttachmentKind
		guestAgent bool
	}{
		{name: "tap without guest agent", kind: helper.AttachmentTapFD},
		{name: "tap with guest agent", kind: helper.AttachmentTapFD, guestAgent: true},
		{name: "datagram without guest agent", kind: helper.AttachmentDatagramFD},
		{name: "datagram with guest agent", kind: helper.AttachmentDatagramFD, guestAgent: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir, err := os.MkdirTemp("/tmp", "tbx-qemu-network-test-")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = os.RemoveAll(dir) }()
			varsTemplate := filepath.Join(dir, "OVMF_VARS.fd")
			if err := os.WriteFile(varsTemplate, []byte("vars"), 0o600); err != nil {
				t.Fatal(err)
			}
			network, peer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = peer.Close() }()
			backend := &qemuHypervisor{
				architecture: ArchitectureAMD64,
				system:       qemuSystem{Machine: "q35"},
				accelerator:  "kvm",
				cpu:          "host",
				firmware:     qemuFirmware{VarsPath: varsTemplate},
				newConsole: func(string) (*consoleProxy, *os.File, error) {
					file, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
					return nil, file, err
				},
			}
			guestAgentPath := ""
			if test.guestAgent {
				guestAgentPath = filepath.Join(dir, "node.qga.sock")
			}
			machine, err := backend.newMachine(Spec{
				CPUs:                 1,
				MemoryMiB:            1024,
				DiskPath:             filepath.Join(dir, "node.img"),
				MAC:                  "02:00:00:00:00:01",
				EFIVarsPath:          filepath.Join(dir, "node.efi"),
				ConsoleSocketPath:    filepath.Join(dir, "node.console.sock"),
				GuestAgentSocketPath: guestAgentPath,
				Network: func() (*helper.Attachment, error) {
					return &helper.Attachment{Kind: test.kind, File: network}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			files := machine.extraFiles()
			wantFiles := 2
			if test.guestAgent {
				wantFiles = 3
			}
			if len(files) != wantFiles || files[0] != network || files[1] != machine.consoleGuest {
				t.Fatalf("extraFiles() = %v, want network then console with length %d", files, wantFiles)
			}
			cfg := machine.launchConfig("/run/qmp.sock", "")
			if cfg.NetworkKind != test.kind || cfg.NetworkFD != 3 {
				t.Fatalf("launch network = %q fd %d, want %q fd 3", cfg.NetworkKind, cfg.NetworkFD, test.kind)
			}
			if cfg.Machine != "q35" || cfg.Accelerator != "kvm" || cfg.CPU != "host" {
				t.Fatalf("launch platform = machine %q accelerator %q CPU %q", cfg.Machine, cfg.Accelerator, cfg.CPU)
			}
			if test.guestAgent && (cfg.GuestAgentFD != 5 || files[2] != machine.guestAgent) {
				t.Fatalf("guest-agent launch mapping = fd %d files %v", cfg.GuestAgentFD, files)
			}
			if err := machine.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestQEMUNewMachineBindsGuestAgentSocket(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "tbx-qga-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	varsTemplate := filepath.Join(dir, "OVMF_VARS.fd")
	if err := os.WriteFile(varsTemplate, []byte("vars"), 0o600); err != nil {
		t.Fatal(err)
	}
	tapRead, tapWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tapWrite.Close() }()
	backend := &qemuHypervisor{
		firmware: qemuFirmware{VarsPath: varsTemplate},
		newConsole: func(string) (*consoleProxy, *os.File, error) {
			file, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
			return nil, file, err
		},
	}
	guestAgentPath := filepath.Join(dir, "node.qga.sock")
	machine, err := backend.newMachine(Spec{
		CPUs:                 1,
		MemoryMiB:            1024,
		DiskPath:             filepath.Join(dir, "node.img"),
		MAC:                  "02:00:00:00:00:01",
		EFIVarsPath:          filepath.Join(dir, "node.efi"),
		ConsoleSocketPath:    filepath.Join(dir, "node.console.sock"),
		GuestAgentSocketPath: guestAgentPath,
		Network: func() (*helper.Attachment, error) {
			return &helper.Attachment{Kind: helper.AttachmentTapFD, File: tapRead}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(guestAgentPath)
	if err != nil {
		t.Fatalf("guest-agent socket not bound: %v", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("guest-agent path mode = %v, want a socket", info.Mode())
	}
	if got, want := len(machine.extraFiles()), 3; got != want {
		t.Fatalf("extraFiles() length = %d, want %d", got, want)
	}
	if got := machine.extraFiles()[2]; got != machine.guestAgent {
		t.Fatalf("extraFiles()[2] = %v, want the guest-agent descriptor %v", got, machine.guestAgent)
	}
	if got, want := machine.launchConfig("/run/qmp.sock", "").GuestAgentFD, 5; got != want {
		t.Fatalf("launchConfig().GuestAgentFD = %d, want %d", got, want)
	}
	if cfg := machine.launchConfig("/run/qmp.sock", ""); cfg.NetworkKind != helper.AttachmentTapFD || cfg.NetworkFD != 3 {
		t.Fatalf("launchConfig() network = %q fd %d, want tap fd 3", cfg.NetworkKind, cfg.NetworkFD)
	}
	if err := machine.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(guestAgentPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Close() left the guest-agent socket behind: %v", err)
	}
}

func TestQEMUNewMachineWithoutGuestAgent(t *testing.T) {
	dir := t.TempDir()
	varsTemplate := filepath.Join(dir, "OVMF_VARS.fd")
	if err := os.WriteFile(varsTemplate, []byte("vars"), 0o600); err != nil {
		t.Fatal(err)
	}
	tapRead, tapWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tapWrite.Close() }()
	backend := &qemuHypervisor{
		firmware: qemuFirmware{VarsPath: varsTemplate},
		newConsole: func(string) (*consoleProxy, *os.File, error) {
			file, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
			return nil, file, err
		},
	}
	machine, err := backend.newMachine(Spec{
		CPUs:              1,
		MemoryMiB:         1024,
		DiskPath:          filepath.Join(dir, "node.img"),
		MAC:               "02:00:00:00:00:01",
		EFIVarsPath:       filepath.Join(dir, "node.efi"),
		ConsoleSocketPath: filepath.Join(dir, "node.console.sock"),
		Network: func() (*helper.Attachment, error) {
			return &helper.Attachment{Kind: helper.AttachmentTapFD, File: tapRead}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = machine.Close() }()
	if got, want := len(machine.extraFiles()), 2; got != want {
		t.Fatalf("extraFiles() length = %d, want %d", got, want)
	}
	if got := machine.launchConfig("/run/qmp.sock", "").GuestAgentFD; got != 0 {
		t.Fatalf("launchConfig().GuestAgentFD = %d, want 0", got)
	}
	if cfg := machine.launchConfig("/run/qmp.sock", ""); cfg.NetworkKind != helper.AttachmentTapFD || cfg.NetworkFD != 3 {
		t.Fatalf("launchConfig() network = %q fd %d, want tap fd 3", cfg.NetworkKind, cfg.NetworkFD)
	}
}

func TestQEMUBackendHelper(t *testing.T) {
	if os.Getenv("TBX_QEMU_BACKEND_HELPER") != "1" {
		return
	}
	path := os.Getenv("TBX_QEMU_BACKEND_QMP")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	connection, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	encoder := json.NewEncoder(connection)
	decoder := json.NewDecoder(connection)
	if err := encoder.Encode(map[string]any{"QMP": map[string]any{"capabilities": []string{}}}); err != nil {
		t.Fatal(err)
	}
	for {
		var request qmpTestRequest
		if err := decoder.Decode(&request); err != nil {
			t.Fatal(err)
		}
		if err := encoder.Encode(map[string]any{"return": map[string]any{}, "id": request.ID}); err != nil {
			t.Fatal(err)
		}
		if request.Execute == "system_powerdown" {
			return
		}
	}
}

func TestQEMULaunchStopAndCloseLifecycle(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "tbx-qemu-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	varsTemplate := filepath.Join(dir, "OVMF_VARS.fd")
	if err := os.WriteFile(varsTemplate, []byte("vars"), 0o600); err != nil {
		t.Fatal(err)
	}
	tapRead, tapWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tapWrite.Close() }()
	backend := &qemuHypervisor{
		architecture: ArchitectureAMD64,
		system:       qemuSystem{Binary: "qemu-system-x86_64", Machine: "q35"},
		binary:       os.Args[0],
		accelerator:  "kvm",
		cpu:          "host",
		firmware:     qemuFirmware{CodePath: filepath.Join(dir, "OVMF_CODE.fd"), VarsPath: varsTemplate},
		version:      qemuVersion{Major: 8, Minor: 2, Patch: 2},
		capabilities: qemuCapabilities(qemuVersion{Major: 8, Minor: 2, Patch: 2}),
		saved:        make(map[string]*qemuMachine),
		newConsole: func(string) (*consoleProxy, *os.File, error) {
			file, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
			return nil, file, err
		},
		command: func(_ string, arguments ...string) *exec.Cmd {
			var qmpPath string
			for index, argument := range arguments {
				if argument == "-qmp" && index+1 < len(arguments) {
					value := strings.TrimPrefix(arguments[index+1], "unix:")
					qmpPath, _, _ = strings.Cut(value, ",server=on")
					qmpPath = strings.ReplaceAll(qmpPath, ",,", ",")
					break
				}
			}
			command := exec.Command(os.Args[0], "-test.run=^TestQEMUBackendHelper$")
			command.Env = append(os.Environ(), "TBX_QEMU_BACKEND_HELPER=1", "TBX_QEMU_BACKEND_QMP="+qmpPath)
			return command
		},
	}
	if backend.Architecture() != ArchitectureAMD64 || !backend.Capabilities().Suspend.Supported {
		t.Fatal("backend did not expose probed architecture/capabilities")
	}
	machine, err := backend.Launch(context.Background(), Spec{
		CPUs:              1,
		MemoryMiB:         1024,
		DiskPath:          filepath.Join(dir, "node.img"),
		MAC:               "02:00:00:00:00:01",
		EFIVarsPath:       filepath.Join(dir, "node.efi"),
		ConsoleSocketPath: filepath.Join(dir, "node.console.sock"),
		Network: func() (*helper.Attachment, error) {
			return &helper.Attachment{Kind: helper.AttachmentTapFD, File: tapRead}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !machine.Active() {
		t.Fatal("launched QEMU machine is not active")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := machine.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if machine.Active() {
		t.Fatal("stopped QEMU machine remains active")
	}
	if err := machine.Close(); err != nil {
		t.Fatal(err)
	}
	if err := machine.Close(); err != nil {
		t.Fatalf("second Close() = %v, want idempotent success", err)
	}
	if _, err := tapRead.Stat(); err == nil {
		t.Fatal("Close() left tap attachment open")
	}
}

func TestQEMUBalloonSetAndReadback(t *testing.T) {
	client, server := qmpClientForTest(t, func(request qmpTestRequest) (any, bool) {
		switch request.Execute {
		case "balloon":
			var arguments struct {
				Value int64 `json:"value"`
			}
			if err := json.Unmarshal(request.Arguments, &arguments); err != nil {
				t.Errorf("decode balloon arguments: %v", err)
			}
			if arguments.Value != 1536*1024*1024 {
				t.Errorf("balloon value = %d, want %d", arguments.Value, int64(1536*1024*1024))
			}
			return map[string]any{}, false
		case "query-balloon":
			return map[string]any{"actual": int64(1536 * 1024 * 1024)}, true
		default:
			t.Errorf("unexpected QMP command %q", request.Execute)
			return map[string]any{}, true
		}
	})
	defer func() { _ = client.close() }()
	defer func() { _ = server.Close() }()
	machine := &qemuMachine{
		owner:   &qemuHypervisor{},
		process: &qemuProcess{done: make(chan struct{})},
		qmp:     client,
	}
	if err := machine.SetMemoryTargetMiB(1536); err != nil {
		t.Fatal(err)
	}
	actual, err := machine.queryBalloon(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if actual != 1536*1024*1024 {
		t.Fatalf("queryBalloon() = %d, want %d", actual, int64(1536*1024*1024))
	}
}

type qmpMigrateTestArguments struct {
	Channels []struct {
		Address struct {
			Transport string `json:"transport"`
			Filename  string `json:"filename"`
			Offset    uint64 `json:"offset"`
		} `json:"addr"`
	} `json:"channels"`
}

func TestQEMUSuspendSendsUint64FileMigrationOffset(t *testing.T) {
	dir := t.TempDir()
	savePath := filepath.Join(dir, "node.vzstate")
	process := &qemuProcess{done: make(chan struct{})}
	var commands []string
	var migrationPath string
	queries := 0
	client, server := qmpClientForTest(t, func(request qmpTestRequest) (any, bool) {
		commands = append(commands, request.Execute)
		switch request.Execute {
		case "stop":
			return map[string]any{}, false
		case "migrate":
			var arguments qmpMigrateTestArguments
			if err := json.Unmarshal(request.Arguments, &arguments); err != nil {
				t.Errorf("decode migrate arguments: %v", err)

				return map[string]any{}, false
			}
			if len(arguments.Channels) != 1 || arguments.Channels[0].Address.Transport != "file" || arguments.Channels[0].Address.Offset != uint64(qemuSaveOffset) {
				t.Errorf("migrate arguments = %+v", arguments)
			} else {
				migrationPath = arguments.Channels[0].Address.Filename
			}
			return map[string]any{}, false
		case "query-migrate":
			queries++
			if queries == 1 {
				return map[string]any{"status": "active"}, false
			}
			return map[string]any{"status": "completed"}, false
		case "quit":
			close(process.done)
			return map[string]any{}, true
		default:
			t.Errorf("unexpected QMP command %q", request.Execute)
			return map[string]any{}, true
		}
	})
	defer func() { _ = client.close() }()
	defer func() { _ = server.Close() }()
	backend := &qemuHypervisor{
		architecture: ArchitectureAMD64,
		system:       qemuSystem{Machine: "q35"},
		version:      qemuVersion{Major: 8, Minor: 2, Patch: 2},
		capabilities: qemuCapabilities(qemuVersion{Major: 8, Minor: 2, Patch: 2}),
		saved:        make(map[string]*qemuMachine),
	}
	machine := &qemuMachine{owner: backend, process: process, qmp: client}
	if err := machine.Suspend(context.Background(), savePath); err != nil {
		t.Fatal(err)
	}
	wantCommands := []string{"stop", "migrate", "query-migrate", "query-migrate", "quit"}
	if !reflect.DeepEqual(commands, wantCommands) {
		t.Fatalf("commands = %v, want %v", commands, wantCommands)
	}
	if migrationPath == "" || migrationPath == savePath {
		t.Fatalf("migration path = %q, want atomic temporary path", migrationPath)
	}
	metadata, err := readQEMUSave(savePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateQEMUSave(metadata, ArchitectureAMD64, "q35"); err != nil {
		t.Fatal(err)
	}
	if err := validateQEMUSaveVersion(metadata, "8.2.2"); err != nil {
		t.Fatal(err)
	}
	if backend.saved[savePath] != machine {
		t.Fatal("suspended machine was not retained for same-daemon resume")
	}
	if machine.qmp != nil {
		t.Fatal("suspended machine retained the exited QEMU process's QMP client")
	}
	if machine.Active() {
		t.Fatal("machine remained active after suspend")
	}
}

func TestQMPMigrateArgumentsRejectStringOffset(t *testing.T) {
	var arguments qmpMigrateTestArguments
	if err := json.Unmarshal([]byte(`{"channels":[{"addr":{"offset":1048576}}]}`), &arguments); err != nil {
		t.Fatalf("decode numeric offset: %v", err)
	}
	if got := arguments.Channels[0].Address.Offset; got != uint64(qemuSaveOffset) {
		t.Fatalf("numeric offset = %d, want %d", got, uint64(qemuSaveOffset))
	}

	err := json.Unmarshal([]byte(`{"channels":[{"addr":{"offset":"0x100000"}}]}`), &arguments)
	var typeError *json.UnmarshalTypeError
	if !errors.As(err, &typeError) {
		t.Fatalf("decode string offset error = %v, want JSON type error", err)
	}
}

func TestCleanupExitedQMPPreservesFailedCleanup(t *testing.T) {
	closeErr := errors.New("close QMP connection")
	client := &qmpClient{conn: closeErrorConn{err: closeErr}}
	qmpPath := filepath.Join(t.TempDir(), "qmp.sock")
	if err := os.Mkdir(qmpPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(qmpPath, "keep"), []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	machine := &qemuMachine{qmp: client, qmpPath: qmpPath}

	machine.cleanupExitedQMP()

	if machine.qmp != client {
		t.Fatal("cleanup discarded QMP client after close failure")
	}
	if machine.qmpPath != qmpPath {
		t.Fatal("cleanup discarded QMP socket path after removal failure")
	}
	if err := machine.qmp.close(); !errors.Is(err, closeErr) {
		t.Fatalf("retained QMP client close error = %v, want %v", err, closeErr)
	}
}

func TestCleanupExitedQMPClearsSuccessfulAndMissingCleanup(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer func() { _ = serverConn.Close() }()
	machine := &qemuMachine{
		qmp:     &qmpClient{conn: clientConn},
		qmpPath: filepath.Join(t.TempDir(), "missing", "qmp.sock"),
	}

	machine.cleanupExitedQMP()

	if machine.qmp != nil {
		t.Fatal("cleanup retained successfully closed QMP client")
	}
	if machine.qmpPath != "" {
		t.Fatalf("cleanup retained missing QMP socket path %q", machine.qmpPath)
	}
}

func TestWaitQEMUIncomingRejectsFailedMigration(t *testing.T) {
	client, server := qmpClientForTest(t, func(request qmpTestRequest) (any, bool) {
		if request.Execute != "query-migrate" {
			t.Errorf("unexpected QMP command %q", request.Execute)
		}
		return map[string]any{"status": "failed", "error-desc": "bad stream"}, true
	})
	defer func() { _ = client.close() }()
	defer func() { _ = server.Close() }()
	err := waitQEMUIncoming(context.Background(), client)
	if !errors.Is(err, ErrIncompatibleSave) || !strings.Contains(err.Error(), "bad stream") {
		t.Fatalf("waitQEMUIncoming() = %v, want ErrIncompatibleSave with migration failure", err)
	}
}

type closeErrorConn struct {
	net.Conn
	err error
}

func (c closeErrorConn) Close() error {
	return c.err
}

func qmpClientForTest(t *testing.T, respond func(qmpTestRequest) (any, bool)) (*qmpClient, net.Conn) {
	t.Helper()
	clientConnection, serverConnection := net.Pipe()
	ready := make(chan struct{})
	go func() {
		reader := bufio.NewReader(serverConnection)
		_ = writeJSONLine(serverConnection, map[string]any{"QMP": map[string]any{"capabilities": []string{}}})
		handshake, err := readQMPTestRequest(reader, serverConnection)
		if err != nil {
			t.Errorf("read handshake: %v", err)
			close(ready)
			return
		}
		_ = writeJSONLine(serverConnection, map[string]any{"return": map[string]any{}, "id": handshake.ID})
		close(ready)
		for {
			request, err := readQMPTestRequest(reader, serverConnection)
			if err != nil {
				return
			}
			result, done := respond(request)
			_ = writeJSONLine(serverConnection, map[string]any{"return": result, "id": request.ID})
			if done {
				return
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, err := newQMPClient(ctx, clientConnection)
	if err != nil {
		t.Fatal(err)
	}
	<-ready
	return client, serverConnection
}
