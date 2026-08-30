//go:build darwin || linux

package hypervisor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewQEMUConsoleProxyCreatesNestedDirectory(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "tbx-qemu-console-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "nested", "console", "node.sock")
	proxy, guest, err := newQEMUConsoleProxy(path)
	if err != nil {
		t.Fatal(err)
	}
	proxy.close()
	if err := guest.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatalf("console directory was not created: %v", err)
	}
}
