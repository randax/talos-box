//go:build darwin && cgo && e2e

package helper

import (
	"fmt"
	"testing"

	"golang.org/x/sys/unix"
)

func TestVMNetAttachmentReportsRaisedBuffers(t *testing.T) {
	env := requireHelperNetworking(t)
	attachment, err := Attach("vmnet-buffers-"+env.runID, 242, fmt.Sprintf("node-%s", env.runID))
	if err != nil {
		t.Fatalf("attach helper network: %v", err)
	}
	defer func() {
		if err := attachment.Close(); err != nil {
			t.Errorf("close attachment: %v", err)
		}
	}()

	fd := int(attachment.File.Fd())
	target := vmnetSocketBufferSize()
	for _, option := range []int{unix.SO_SNDBUF, unix.SO_RCVBUF} {
		got, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, option)
		if err != nil {
			t.Fatalf("read attachment option %d: %v", option, err)
		}
		if got < target {
			t.Errorf("attachment option %d = %d, want at least %d", option, got, target)
		}
	}
}
