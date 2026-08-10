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

func TestInheritedListenerRejectsUnexpectedFDCount(t *testing.T) {
	_, _, err := inheritedListener("ignored", os.Getpid(), func(name string) string {
		switch name {
		case "LISTEN_PID":
			return strconv.Itoa(os.Getpid())
		case "LISTEN_FDS":
			return "2"
		default:
			return ""
		}
	}, 3)
	if err == nil {
		t.Fatal("inheritedListener() accepted multiple descriptors")
	}
}
