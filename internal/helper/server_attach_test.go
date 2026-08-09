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

func TestAttachCleanupDropsAttachmentOnRetainedStopFailure(t *testing.T) {
	server := NewServer(nil)
	startCalls := 0
	stopCalls := make(map[int]int)

	originalStart := startInterface
	originalStop := stopInterface
	startInterface = func(subnet int) (int, error) {
		if subnet != 7 {
			t.Fatalf("startInterface subnet = %d, want 7", subnet)
		}
		startCalls++
		return 98 + startCalls, nil
	}
	stopInterface = func(fd int) error {
		stopCalls[fd]++
		switch fd {
		case 99:
			if stopCalls[fd] == 1 {
				return wrapVMNetStopError(errors.New("retry later"), true)
			}
			return nil
		case 100:
			return nil
		default:
			t.Fatalf("unexpected stopInterface fd = %d", fd)
			return nil
		}
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
	if _, ok := server.attachments[key]; ok {
		t.Fatal("attachment mapping retained after failed response cleanup")
	}
	if _, ok := server.pendingStops[99]; !ok {
		t.Fatal("retained stop was not recorded for shutdown retry")
	}

	_, retryFD, retryCleanup, err := server.attach(json.RawMessage(`{"cluster":"demo","subnetIndex":7,"node":"cp-1"}`))
	if err != nil {
		t.Fatalf("retry attach() error = %v", err)
	}
	if retryFD != 100 {
		t.Fatalf("retry attach fd = %d, want 100", retryFD)
	}

	retryCleanup()
	if err := server.Shutdown(); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if got := stopCalls[99]; got != 2 {
		t.Fatalf("stopInterface calls for retained fd = %d, want 2", got)
	}
	if len(server.pendingStops) != 0 {
		t.Fatalf("pending stops after successful shutdown retry = %v, want empty", server.pendingStops)
	}
}

func TestShutdownRetriesRetainedAttachmentStops(t *testing.T) {
	server := NewServer(nil)
	key := attachmentKey{cluster: "demo", node: "cp-1"}
	server.attachments[key] = 42
	stopCalls := 0

	original := stopInterface
	stopInterface = func(fd int) error {
		if fd != 42 {
			t.Fatalf("stopInterface fd = %d, want 42", fd)
		}
		stopCalls++
		if stopCalls == 1 {
			return wrapVMNetStopError(errors.New("retry later"), true)
		}
		return nil
	}
	t.Cleanup(func() {
		stopInterface = original
	})

	if err := server.Shutdown(); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if stopCalls != 2 {
		t.Fatalf("stopInterface calls = %d, want 2", stopCalls)
	}
	if _, ok := server.attachments[key]; ok {
		t.Fatal("attachment retained after successful shutdown retry")
	}
}

func TestShutdownBoundsRetainedStopRetries(t *testing.T) {
	server := NewServer(nil)
	server.pendingStops[42] = struct{}{}
	stopCalls := 0
	stopErr := errors.New("retry later")

	original := stopInterface
	stopInterface = func(fd int) error {
		if fd != 42 {
			t.Fatalf("stopInterface fd = %d, want 42", fd)
		}
		stopCalls++
		return wrapVMNetStopError(stopErr, true)
	}
	t.Cleanup(func() {
		stopInterface = original
	})

	err := server.Shutdown()
	if !errors.Is(err, stopErr) {
		t.Fatalf("Shutdown() error = %v, want %v", err, stopErr)
	}
	if stopCalls != shutdownStopMaxAttempts {
		t.Fatalf("stopInterface calls = %d, want %d", stopCalls, shutdownStopMaxAttempts)
	}
	if _, ok := server.pendingStops[42]; !ok {
		t.Fatal("retained stop removed after exhausting shutdown retries")
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
