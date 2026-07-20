package helper

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"reflect"
	"testing"

	"golang.org/x/sys/unix"
)

func TestProtocolRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value any
		into  any
	}{
		{
			name:  "request",
			value: Request{Op: "net.attach", Args: json.RawMessage("{\"cluster\":\"demo\",\"subnetIndex\":1,\"node\":\"demo-cp-1\"}")},
			into:  &Request{},
		},
		{
			name:  "success response",
			value: Response{OK: true, Data: json.RawMessage("{\"pong\":true}")},
			into:  &Response{},
		},
		{
			name:  "error response",
			value: Response{OK: false, Error: "not found"},
			into:  &Response{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var wire bytes.Buffer
			if err := json.NewEncoder(&wire).Encode(test.value); err != nil {
				t.Fatal(err)
			}
			if wire.Len() == 0 || wire.Bytes()[wire.Len()-1] != '\n' {
				t.Fatalf("encoded message is not newline-delimited: %q", wire.Bytes())
			}
			if err := json.NewDecoder(&wire).Decode(test.into); err != nil {
				t.Fatal(err)
			}
			got := reflect.ValueOf(test.into).Elem().Interface()
			if !reflect.DeepEqual(got, test.value) {
				t.Fatalf("round trip = %#v, want %#v", got, test.value)
			}
		})
	}
}

func TestSCMRightsResponse(t *testing.T) {
	t.Parallel()

	left, right := unixSocketpair(t)
	defer func() { _ = left.Close() }()
	defer func() { _ = right.Close() }()

	file, err := os.CreateTemp(t.TempDir(), "handoff")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	const content = "descriptor handoff"
	if _, err := file.WriteString(content); err != nil {
		t.Fatal(err)
	}

	sendErrors := make(chan error, 1)
	go func() {
		sendErrors <- sendResponse(left, success(map[string]string{"node": "demo-cp-1"}), int(file.Fd()))
	}()

	response, fd, err := receiveResponse(right, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-sendErrors; err != nil {
		t.Fatal(err)
	}
	if !response.OK {
		t.Fatalf("response = %#v", response)
	}
	received := os.NewFile(uintptr(fd), "received")
	if received == nil {
		t.Fatal("received descriptor is invalid")
	}
	defer func() { _ = received.Close() }()
	buffer := make([]byte, len(content))
	if _, err := received.ReadAt(buffer, 0); err != nil {
		t.Fatal(err)
	}
	if string(buffer) != content {
		t.Fatalf("received content = %q, want %q", buffer, content)
	}
}

func TestSCMRightsResponseRequiresDescriptor(t *testing.T) {
	t.Parallel()

	left, right := unixSocketpair(t)
	defer func() { _ = left.Close() }()
	defer func() { _ = right.Close() }()

	sendErrors := make(chan error, 1)
	go func() {
		sendErrors <- sendResponse(left, success(struct{}{}), -1)
	}()
	if _, _, err := receiveResponse(right, true); err == nil {
		t.Fatal("receiveResponse accepted a response without a descriptor")
	}
	if err := <-sendErrors; err != nil {
		t.Fatal(err)
	}
}

func unixSocketpair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	connections := make([]*net.UnixConn, 0, 2)
	for index, fd := range fds {
		file := os.NewFile(uintptr(fd), "socketpair")
		connection, err := net.FileConn(file)
		_ = file.Close()
		if err != nil {
			for _, existing := range connections {
				_ = existing.Close()
			}
			t.Fatalf("convert socketpair endpoint %d: %v", index, err)
		}
		unixConnection, ok := connection.(*net.UnixConn)
		if !ok {
			_ = connection.Close()
			t.Fatalf("socketpair endpoint %d is %T, want *net.UnixConn", index, connection)
		}
		connections = append(connections, unixConnection)
	}
	return connections[0], connections[1]
}

func TestResolverContent(t *testing.T) {
	t.Parallel()

	got, err := resolverContent(1053)
	if err != nil {
		t.Fatal(err)
	}
	const want = "nameserver 127.0.0.1\nport 1053\n"
	if string(got) != want {
		t.Fatalf("resolver content = %q, want %q", got, want)
	}
}

func TestResolverContentRejectsInvalidPort(t *testing.T) {
	t.Parallel()

	for _, port := range []int{0, 65536} {
		if _, err := resolverContent(port); err == nil {
			t.Fatalf("resolverContent(%d) succeeded", port)
		}
	}
}

func TestInstallResolver(t *testing.T) {
	t.Parallel()

	path := t.TempDir() + "/resolver/k8s.test"
	if err := installResolver(path, 5353); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const want = "nameserver 127.0.0.1\nport 5353\n"
	if string(content) != want {
		t.Fatalf("resolver file = %q, want %q", content, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("resolver permissions = %#o, want 0644", got)
	}
}

func TestAttachRequiresSubnetIndex(t *testing.T) {
	t.Parallel()

	server := NewServer()
	_, _, _, err := server.attach(json.RawMessage("{\"cluster\":\"demo\",\"node\":\"demo-cp-1\"}"))
	if err == nil {
		t.Fatal("attach accepted a missing subnetIndex")
	}
}

func TestDetachRequiresNames(t *testing.T) {
	t.Parallel()

	if err := NewServer().detach(json.RawMessage("{}")); err == nil {
		t.Fatal("detach accepted missing cluster and node")
	}
}
