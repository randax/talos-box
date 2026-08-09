//go:build linux

package helper

import (
	"os"
	"testing"
)

func TestLinuxPeerUID(t *testing.T) {
	t.Parallel()

	left, right := unixSocketpair(t)
	defer func() { _ = left.Close() }()
	defer func() { _ = right.Close() }()

	got, err := peerUID(left)
	if err != nil {
		t.Fatal(err)
	}
	if want := uint32(os.Geteuid()); got != want {
		t.Fatalf("peer uid = %d, want %d", got, want)
	}
}

func TestLinuxSocketAuthorizedPeerUsesFilesystemGate(t *testing.T) {
	t.Parallel()

	if !isAuthorizedPeer(uint32(os.Geteuid()), nil) {
		t.Fatal("peer admitted by the Linux socket permissions was rejected")
	}
}
