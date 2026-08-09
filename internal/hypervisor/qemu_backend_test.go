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
			return &helper.Attachment{Kind: helper.AttachmentDatagramFD, File: read}, nil
		},
	})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("newMachine() = %v, want ErrUnsupported", err)
	}
	if _, err := read.Stat(); err == nil {
		t.Fatal("rejected network attachment remained open")
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

func TestQEMUSuspendUsesFileMigrationAndRetainsMachine(t *testing.T) {
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
			var arguments struct {
				Channels []struct {
					Address struct {
						Transport string `json:"transport"`
						Filename  string `json:"filename"`
						Offset    string `json:"offset"`
					} `json:"addr"`
				} `json:"channels"`
			}
			if err := json.Unmarshal(request.Arguments, &arguments); err != nil {
				t.Errorf("decode migrate arguments: %v", err)
			} else if len(arguments.Channels) != 1 || arguments.Channels[0].Address.Transport != "file" || arguments.Channels[0].Address.Offset != "0x100000" {
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
	if err := validateQEMUSave(metadata, "8.2.2", ArchitectureAMD64, "q35"); err != nil {
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
