package helper

import (
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestListenUnixSocketPreservesNonCollisionBindError(t *testing.T) {
	t.Parallel()

	removeCalled := false
	_, err := listenUnixSocket(
		"/restricted/tbx-helper.sock",
		func(string, string) (net.Listener, error) { return nil, os.ErrPermission },
		func(string, string, time.Duration) (net.Conn, error) {
			t.Fatal("dial called for permission error")
			return nil, nil
		},
		func(string) error { removeCalled = true; return nil },
	)
	if !errors.Is(err, os.ErrPermission) || removeCalled {
		t.Fatalf("listenUnixSocket() error = %v, removeCalled = %t; want original permission error only", err, removeCalled)
	}
}

func TestListenUnixSocketRetainsBindErrorWhenStaleRemovalFails(t *testing.T) {
	t.Parallel()

	removeErr := errors.New("remove denied")
	_, err := listenUnixSocket(
		"/run/tbx-helper.sock",
		func(string, string) (net.Listener, error) { return nil, unix.EADDRINUSE },
		func(string, string, time.Duration) (net.Conn, error) { return nil, errors.New("connection refused") },
		func(string) error { return removeErr },
	)
	if !errors.Is(err, unix.EADDRINUSE) || !errors.Is(err, removeErr) {
		t.Fatalf("listenUnixSocket() error = %v, want original bind and cleanup errors", err)
	}
}

func TestIsAuthorizedUID(t *testing.T) {
	t.Parallel()

	allowedUID := uint32(501)
	tests := []struct {
		name       string
		uid        uint32
		allowedUID *uint32
		allowAny   bool
		want       bool
	}{
		{name: "allowed uid", uid: 501, allowedUID: &allowedUID, want: true},
		{name: "root", uid: 0, allowedUID: &allowedUID, want: true},
		{name: "other uid", uid: 502, allowedUID: &allowedUID, want: false},
		{name: "unset allows root", uid: 0, want: true},
		{name: "unset rejects user", uid: 501, want: false},
		{name: "socket-admitted group peer", uid: 501, allowAny: true, want: true},
		{name: "explicit uid remains authoritative for activated socket", uid: 502, allowedUID: &allowedUID, allowAny: true, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := isAuthorizedUID(test.uid, test.allowedUID, test.allowAny); got != test.want {
				t.Fatalf("isAuthorizedUID(%d) = %t, want %t", test.uid, got, test.want)
			}
		})
	}
}
