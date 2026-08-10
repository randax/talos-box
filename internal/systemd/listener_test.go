package systemd

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"testing"
)

func TestInheritedListenerUsesActivatedFD(t *testing.T) {
	socketPath := fmt.Sprintf("/tmp/tbxd-%d.sock", os.Getpid())
	original, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = original.Close() })

	file, err := original.(*net.UnixListener).File()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()

	listener, activated, err := inheritedListener(socketPath, os.Getpid(), func(name string) string {
		switch name {
		case "LISTEN_PID":
			return strconv.Itoa(os.Getpid())
		case "LISTEN_FDS":
			return "1"
		default:
			return ""
		}
	}, int(file.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if !activated {
		t.Fatal("inheritedListener() did not activate")
	}
	defer func() { _ = listener.Close() }()

	accepted := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			accepted <- err
			return
		}
		_ = connection.Close()
		accepted <- nil
	}()

	connection, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.Close()
	if err := <-accepted; err != nil {
		t.Fatal(err)
	}
}

func TestInheritedListenerIgnoresMismatchedPID(t *testing.T) {
	listener, activated, err := inheritedListener("ignored", os.Getpid(), func(name string) string {
		switch name {
		case "LISTEN_PID":
			return strconv.Itoa(os.Getpid() + 1)
		case "LISTEN_FDS":
			return "1"
		default:
			return ""
		}
	}, 99)
	if err != nil {
		t.Fatal(err)
	}
	if activated || listener != nil {
		t.Fatalf("inheritedListener() = %#v, %t; want nil, false", listener, activated)
	}
}

func TestInheritedListenerIgnoresInvalidActivationMetadata(t *testing.T) {
	t.Parallel()

	pid := strconv.Itoa(os.Getpid())
	tests := []struct {
		name string
		env  map[string]string
	}{
		{name: "only LISTEN_PID", env: map[string]string{"LISTEN_PID": pid}},
		{name: "only LISTEN_FDS", env: map[string]string{"LISTEN_FDS": "1"}},
		{name: "malformed LISTEN_PID", env: map[string]string{"LISTEN_PID": "not-a-pid", "LISTEN_FDS": "1"}},
		{name: "malformed LISTEN_FDS", env: map[string]string{"LISTEN_PID": pid, "LISTEN_FDS": "not-a-count"}},
		{name: "zero descriptors", env: map[string]string{"LISTEN_PID": pid, "LISTEN_FDS": "0"}},
		{name: "multiple descriptors", env: map[string]string{"LISTEN_PID": pid, "LISTEN_FDS": "2"}},
		{name: "negative descriptor count", env: map[string]string{"LISTEN_PID": pid, "LISTEN_FDS": "-1"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			listener, activated, err := inheritedListener("ignored", os.Getpid(), func(name string) string {
				return test.env[name]
			}, 3)
			if err != nil {
				t.Fatalf("inheritedListener() error = %v, want normal-listener fallback", err)
			}
			if activated || listener != nil {
				t.Fatalf("inheritedListener() = %#v, %t; want nil, false", listener, activated)
			}
		})
	}
}
