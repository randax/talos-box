package helper

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestDetachKeepsAttachmentOnStopFailure(t *testing.T) {
	server := NewServer(nil)
	key := attachmentKey{cluster: "demo", node: "cp-1"}
	server.attachments[key] = 42

	original := stopInterface
	stopInterface = func(fd int) error {
		if fd != 42 {
			t.Fatalf("stopInterface fd = %d, want 42", fd)
		}
		return wrapVMNetStopError(errors.New("retry later"), true)
	}
	t.Cleanup(func() {
		stopInterface = original
	})

	err := server.detach(json.RawMessage(`{"cluster":"demo","node":"cp-1"}`))
	if err == nil {
		t.Fatal("detach() error = nil, want failure")
	}
	if _, ok := server.attachments[key]; !ok {
		t.Fatal("attachment mapping was removed on stop failure")
	}
}

func TestAttachCleanupKeepsAttachmentOnStopFailure(t *testing.T) {
	server := NewServer(nil)

	originalStart := startInterface
	originalStop := stopInterface
	startInterface = func(subnet int) (int, error) {
		if subnet != 7 {
			t.Fatalf("startInterface subnet = %d, want 7", subnet)
		}
		return 99, nil
	}
	stopInterface = func(fd int) error {
		if fd != 99 {
			t.Fatalf("stopInterface fd = %d, want 99", fd)
		}
		return wrapVMNetStopError(errors.New("retry later"), true)
	}
	t.Cleanup(func() {
		startInterface = originalStart
		stopInterface = originalStop
	})

	_, fd, cleanup, err := server.attach(json.RawMessage(`{"cluster":"demo","subnetIndex":7,"node":"cp-1"}`))
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	if fd != 99 {
		t.Fatalf("attach() fd = %d, want 99", fd)
	}
	cleanup()

	key := attachmentKey{cluster: "demo", node: "cp-1"}
	if got := server.attachments[key]; got != 99 {
		t.Fatalf("attachment mapping after cleanup = %d, want 99", got)
	}
}

func TestDetachDropsAttachmentOnTerminalStopFailure(t *testing.T) {
	server := NewServer(nil)
	key := attachmentKey{cluster: "demo", node: "cp-1"}
	server.attachments[key] = 42

	original := stopInterface
	stopInterface = func(fd int) error {
		if fd != 42 {
			t.Fatalf("stopInterface fd = %d, want 42", fd)
		}
		return wrapVMNetStopError(errors.New("released"), false)
	}
	t.Cleanup(func() {
		stopInterface = original
	})

	err := server.detach(json.RawMessage(`{"cluster":"demo","node":"cp-1"}`))
	if err == nil {
		t.Fatal("detach() error = nil, want failure")
	}
	if _, ok := server.attachments[key]; ok {
		t.Fatal("attachment mapping was retained after terminal stop failure")
	}
}
