//go:build e2e

package hypervisor

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRealMigrateRequiresUint64FileOffset(t *testing.T) {
	qemuPath, err := exec.LookPath("qemu-system-aarch64")
	if err != nil {
		t.Skip("qemu-system-aarch64 is not on PATH")
	}

	dir, err := os.MkdirTemp("/tmp", "tbx-qmp-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	socketPath := filepath.Join(dir, "qmp.sock")
	var output bytes.Buffer
	command := exec.Command(qemuPath,
		"-machine", "virt,accel=tcg",
		"-cpu", "max",
		"-m", "128M",
		"-S",
		"-nodefaults",
		"-display", "none",
		"-serial", "none",
		"-qmp", "unix:"+socketPath+",server,nowait",
	)
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatalf("start QEMU: %v", err)
	}
	processDone := make(chan error, 1)
	go func() {
		processDone <- command.Wait()
	}()
	exited := false
	defer func() {
		if exited {
			return
		}
		_ = command.Process.Kill()
		select {
		case <-processDone:
		case <-time.After(time.Second):
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := dialRealQMPSocket(ctx, socketPath)
	if err != nil {
		select {
		case processErr := <-processDone:
			exited = true
			t.Fatalf("connect QMP socket: %v; QEMU exit: %v\nQEMU output:\n%s", err, processErr, output.String())
		default:
			t.Fatalf("connect QMP socket: %v\nQEMU output:\n%s", err, output.String())
		}
	}
	client, err := newQMPClient(ctx, conn)
	if err != nil {
		t.Fatalf("connect QMP: %v\nQEMU output:\n%s", err, output.String())
	}
	defer func() { _ = client.close() }()

	stringOffset := map[string]any{
		"channels": []any{map[string]any{
			"channel-type": "main",
			"addr": map[string]any{
				"transport": "file",
				"filename":  filepath.Join(dir, "string-offset.state"),
				"offset":    "0x100000",
			},
		}},
	}
	err = client.execute(ctx, "migrate", stringOffset, nil)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "uint64") {
		t.Fatalf("migrate with string offset error = %v, want error mentioning uint64", err)
	}

	numericOffset := map[string]any{
		"channels": []any{map[string]any{
			"channel-type": "main",
			"addr": map[string]any{
				"transport": "file",
				"filename":  filepath.Join(dir, "numeric-offset.state"),
				"offset":    uint64(qemuSaveOffset),
			},
		}},
	}
	if err := client.execute(ctx, "migrate", numericOffset, nil); err != nil {
		t.Fatalf("migrate with numeric offset: %v", err)
	}
	if err := client.execute(ctx, "quit", nil, nil); err != nil {
		t.Fatalf("quit QEMU: %v", err)
	}
	select {
	case err := <-processDone:
		exited = true
		if err != nil {
			t.Fatalf("QEMU exit: %v\nQEMU output:\n%s", err, output.String())
		}
	case <-ctx.Done():
		t.Fatalf("wait for QEMU exit: %v\nQEMU output:\n%s", ctx.Err(), output.String())
	}
}

func dialRealQMPSocket(ctx context.Context, socketPath string) (net.Conn, error) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		conn, err := net.DialTimeout("unix", socketPath, 100*time.Millisecond)
		if err == nil {
			return conn, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}
