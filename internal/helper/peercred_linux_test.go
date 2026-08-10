//go:build linux

package helper

import (
	"fmt"
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

func TestLinuxSocketAuthorizationAlwaysUsesPeerUID(t *testing.T) {
	t.Parallel()

	uid := uint32(os.Geteuid())
	if uid != 0 && isAuthorizedPeer(uid, nil, false) {
		t.Fatal("non-root peer was authorized without an allowed UID")
	}
	if !isAuthorizedPeer(0, nil, false) {
		t.Fatal("root peer was rejected")
	}
}

func TestLinuxHelperSocketModeAllowsPeerCredentialGate(t *testing.T) {
	t.Parallel()

	if got := helperSocketMode().Perm(); got != 0o666 {
		t.Fatalf("helper socket mode = %#o, want 0666 so SO_PEERCRED can authorize clients", got)
	}
}

func TestLinuxUnauthorizedPeerReceivesServerError(t *testing.T) {
	t.Parallel()

	uid := uint32(os.Geteuid())
	if uid == 0 {
		t.Skip("root is always authorized")
	}
	allowedUID := uid + 1
	server := NewServer(&allowedUID)
	left, right := unixSocketpair(t)
	defer func() { _ = right.Close() }()

	done := make(chan struct{})
	go func() {
		server.serveConnection(left)
		close(done)
	}()

	client := &Client{connection: right}
	err := client.Ping()
	if err == nil {
		t.Fatal("unauthorized peer received no error")
	}
	want := fmt.Sprintf("unauthorized uid %d", uid)
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
	<-done
}
