package helper

import (
	"os"
	"testing"
)

func TestAttachmentCloseReleasesDescriptorAndNetworkOnce(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = write.Close() }()

	releases := 0
	attachment := newAttachment(AttachmentDatagramFD, read, func() error {
		releases++
		return nil
	})

	if err := attachment.Close(); err != nil {
		t.Fatal(err)
	}
	if err := attachment.Close(); err != nil {
		t.Fatalf("second Close() = %v, want idempotent success", err)
	}
	if releases != 1 {
		t.Fatalf("release calls = %d, want 1", releases)
	}
	if _, err := read.Stat(); err == nil {
		t.Fatal("attachment descriptor remains open after Close")
	}
}
