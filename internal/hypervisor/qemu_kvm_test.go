//go:build linux && kvm

package hypervisor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/helper"
	"golang.org/x/sys/unix"
)

// TestQEMUKVMBootToTalosMaintenance is intentionally behind the kvm build
// tag. Run it natively on both amd64 and arm64 with a disposable Talos raw
// disk supplied in TBX_KVM_TALOS_<ARCH>_DISK. The hard /dev/kvm and tap gates
// make a misconfigured hardware runner fail immediately instead of timing out.
func TestQEMUKVMBootToTalosMaintenance(t *testing.T) {
	kvm, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("KVM is required and must be writable: %v", err)
	}
	_ = kvm.Close()

	environment := "TBX_KVM_TALOS_" + strings.ToUpper(runtime.GOARCH) + "_DISK"
	diskPath := os.Getenv(environment)
	if diskPath == "" {
		t.Fatalf("%s must name a disposable Talos raw disk", environment)
	}
	if _, err := os.Stat(diskPath); err != nil {
		t.Fatalf("inspect Talos disk %q: %v", diskPath, err)
	}

	backend, err := New(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("/tmp", "tbx-kvm-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	machine, err := backend.Launch(context.Background(), Spec{
		CPUs:              2,
		MemoryMiB:         2048,
		DiskPath:          diskPath,
		MAC:               "02:00:00:00:00:87",
		EFIVarsPath:       filepath.Join(dir, "node.efi"),
		ConsoleSocketPath: filepath.Join(dir, "node.console.sock"),
		Network: func() (*helper.Attachment, error) {
			tap, err := openTestTap()
			if err != nil {
				return nil, err
			}
			return &helper.Attachment{Kind: helper.AttachmentTapFD, File: tap}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = machine.Close() }()

	connection, err := dialConsoleUntil(context.Background(), filepath.Join(dir, "node.console.sock"), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	if err := waitForMaintenance(connection, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
}

func openTestTap() (*os.File, error) {
	file, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/net/tun (CAP_NET_ADMIN is required): %w", err)
	}
	request, err := unix.NewIfreq("tbx-kvm%d")
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	request.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI)
	if err := unix.IoctlIfreq(int(file.Fd()), unix.TUNSETIFF, request); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("create KVM test tap (CAP_NET_ADMIN is required): %w", err)
	}
	return file, nil
}

func dialConsoleUntil(ctx context.Context, path string, timeout time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		connection, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
		if err == nil {
			return connection, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("connect console: %w", ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func waitForMaintenance(reader io.Reader, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	buffer := make([]byte, 32*1024)
	var output bytes.Buffer
	for time.Now().Before(deadline) {
		if connection, ok := reader.(net.Conn); ok {
			_ = connection.SetReadDeadline(time.Now().Add(time.Second))
		}
		count, err := reader.Read(buffer)
		if count > 0 {
			_, _ = output.Write(buffer[:count])
			text := strings.ToLower(output.String())
			if strings.Contains(text, "maintenance mode") || strings.Contains(text, "talos maintenance") {
				return nil
			}
			if output.Len() > 2*1024*1024 {
				data := output.Bytes()
				output.Reset()
				_, _ = output.Write(data[len(data)-1024*1024:])
			}
		}
		if err != nil {
			if networkError, ok := err.(net.Error); ok && networkError.Timeout() {
				continue
			}
			return fmt.Errorf("read Talos console: %w", err)
		}
	}
	return fmt.Errorf("Talos did not reach maintenance mode; console tail: %q", output.String())
}
