package helper

import (
	"encoding/json"
	"net"
	"testing"
)

func TestClientListenDNSReceivesBoundDatagramSocket(t *testing.T) {
	t.Parallel()

	serverConnection, clientConnection := unixSocketpair(t)
	defer func() { _ = serverConnection.Close() }()
	defer func() { _ = clientConnection.Close() }()

	listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	file, err := listener.File()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()

	serverErrors := make(chan error, 1)
	go func() {
		var request Request
		if err := json.NewDecoder(serverConnection).Decode(&request); err != nil {
			serverErrors <- err
			return
		}
		if request.Op != "dns.listen" {
			serverErrors <- &unexpectedDNSRequestError{op: request.Op}
			return
		}
		serverErrors <- sendResponse(serverConnection, success(DNSRegistration{
			Registered: true,
			Domain:     "~demo.k8s.test",
		}), int(file.Fd()))
	}()

	client := &Client{connection: clientConnection}
	connection, registration, err := client.ListenDNS("demo", 7)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	if err := <-serverErrors; err != nil {
		t.Fatal(err)
	}
	if !registration.Registered || registration.Domain != "~demo.k8s.test" {
		t.Fatalf("registration = %#v", registration)
	}
	if connection.LocalAddr().String() != listener.LocalAddr().String() {
		t.Fatalf("received socket address = %s, want %s", connection.LocalAddr(), listener.LocalAddr())
	}
}

type unexpectedDNSRequestError struct{ op string }

func (e *unexpectedDNSRequestError) Error() string { return "unexpected helper operation " + e.op }
