//go:build darwin

package helper

import (
	"fmt"
	"os"
	"testing"
)

func TestPeerUID(t *testing.T) {
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

func TestUnauthorizedPeerReceivesServerError(t *testing.T) {
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
