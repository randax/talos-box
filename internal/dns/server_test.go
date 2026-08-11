package dns

import (
	"net"
	"testing"
	"time"
)

func TestServerServesInjectedPacketConnection(t *testing.T) {
	t.Parallel()

	listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(listener, func(string) net.IP { return net.IPv4(172, 30, 7, 2) }, Authority(nil), nil)
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- server.Serve() }()

	client, err := net.DialUDP("udp4", nil, listener.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	if err := client.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	const id = 0x9090
	query, err := encodeQuery("node.demo.k8s.test", id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write(query); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, 4096)
	size, err := client.Read(response)
	if err != nil {
		t.Fatal(err)
	}
	ip, rcode, err := parseAnswerIP(response[:size], id)
	if err != nil {
		t.Fatal(err)
	}
	if rcode != 0 || !ip.Equal(net.IPv4(172, 30, 7, 2)) {
		t.Fatalf("answer = %v rcode %d", ip, rcode)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-serveErrors; err != nil {
		t.Fatal(err)
	}
}

func TestServerForwardsPublicQuery(t *testing.T) {
	t.Parallel()

	query, err := encodeQuery("example.com", 17)
	if err != nil {
		t.Fatal(err)
	}
	want, err := answer(query, func(string) net.IP { return net.IPv4(203, 0, 113, 9) })
	if err != nil {
		t.Fatal(err)
	}
	forwardCalls := 0
	server := NewServer(nil, func(string) net.IP { return nil }, Authority(nil), func(got []byte) ([]byte, error) {
		forwardCalls++
		if string(got) != string(query) {
			t.Fatalf("forwarded query changed")
		}
		return want, nil
	})

	got, err := server.response(query)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("response differs from upstream answer")
	}
	if forwardCalls != 1 {
		t.Fatalf("forward calls = %d, want 1", forwardCalls)
	}
}

func TestServerNeverForwardsAuthoritativeMiss(t *testing.T) {
	t.Parallel()

	query, err := encodeQuery("missing.demo.k8s.test", 19)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(nil, func(string) net.IP { return nil }, Authority(nil), func([]byte) ([]byte, error) {
		t.Fatal("authoritative query was forwarded")
		return nil, nil
	})

	response, err := server.response(query)
	if err != nil {
		t.Fatal(err)
	}
	_, rcode, err := parseAnswerIP(response, 19)
	if err != nil {
		t.Fatal(err)
	}
	if rcode != 3 {
		t.Fatalf("rcode = %d, want NXDOMAIN", rcode)
	}
}

func TestAuthorityFollowsLiveDomainSet(t *testing.T) {
	t.Parallel()

	domains := func() []string { return []string{"lab.internal"} }
	authority := Authority(domains)
	tests := []struct {
		name string
		want bool
	}{
		{"app.lab.internal", true},
		{"lab.internal", true},
		{"demo.k8s.test", true}, // default suffix is always ours
		{"k8s.test.", true},
		{"App.Lab.Internal.", true},
		{"example.com", false},
		{"internal", false},
		{"notlab.internal", false}, // suffix match is label-wise, not string-wise
	}
	for _, tt := range tests {
		if got := authority(tt.name); got != tt.want {
			t.Errorf("authority(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestServerDoesNotForwardCustomDomainQueries(t *testing.T) {
	t.Parallel()

	query, err := encodeQuery("missing.lab.internal", 23)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(nil, func(string) net.IP { return nil }, Authority(func() []string { return []string{"lab.internal"} }), func([]byte) ([]byte, error) {
		t.Fatal("authoritative query was forwarded")
		return nil, nil
	})
	response, err := server.response(query)
	if err != nil {
		t.Fatal(err)
	}
	_, rcode, err := parseAnswerIP(response, 23)
	if err != nil {
		t.Fatal(err)
	}
	if rcode != 3 {
		t.Fatalf("rcode = %d, want NXDOMAIN", rcode)
	}
}

func TestServerReturnsServFailWhenForwardingFails(t *testing.T) {
	t.Parallel()

	query, err := encodeQuery("example.com", 23)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(nil, func(string) net.IP { return nil }, Authority(nil), func([]byte) ([]byte, error) {
		return nil, net.ErrClosed
	})

	response, err := server.response(query)
	if err != nil {
		t.Fatal(err)
	}
	_, rcode, err := parseAnswerIP(response, 23)
	if err != nil {
		t.Fatal(err)
	}
	if rcode != 2 {
		t.Fatalf("rcode = %d, want SERVFAIL", rcode)
	}
}
