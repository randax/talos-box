package dns

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

func TestExchangeDNSRetriesTruncatedUDPResponseOverTCP(t *testing.T) {
	t.Parallel()

	tcpListener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tcpListener.Close() }()
	address := tcpListener.Addr().String()
	udpAddress, err := net.ResolveUDPAddr("udp4", address)
	if err != nil {
		t.Fatal(err)
	}
	udpListener, err := net.ListenUDP("udp4", udpAddress)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = udpListener.Close() }()

	upstreamErrors := make(chan error, 2)
	go func() {
		buffer := make([]byte, 4096)
		size, peer, err := udpListener.ReadFromUDP(buffer)
		if err != nil {
			upstreamErrors <- err
			return
		}
		response, err := errorAnswer(buffer[:size], 0)
		if err == nil {
			binary.BigEndian.PutUint16(response[2:], binary.BigEndian.Uint16(response[2:])|0x0200)
			_, err = udpListener.WriteToUDP(response, peer)
		}
		upstreamErrors <- err
	}()
	go func() {
		connection, err := tcpListener.AcceptTCP()
		if err != nil {
			upstreamErrors <- err
			return
		}
		defer func() { _ = connection.Close() }()
		length := make([]byte, 2)
		if _, err := io.ReadFull(connection, length); err != nil {
			upstreamErrors <- err
			return
		}
		query := make([]byte, binary.BigEndian.Uint16(length))
		if _, err := io.ReadFull(connection, query); err != nil {
			upstreamErrors <- err
			return
		}
		response, err := answer(query, func(string) net.IP { return net.IPv4(203, 0, 113, 9) }, "")
		if err == nil {
			frame := make([]byte, 2+len(response))
			binary.BigEndian.PutUint16(frame, uint16(len(response)))
			copy(frame[2:], response)
			err = writeFull(connection, frame)
		}
		upstreamErrors <- err
	}()

	const id = 31
	query, err := encodeQuery("example.com", id)
	if err != nil {
		t.Fatal(err)
	}
	response, err := exchangeDNS(query, address)
	if err != nil {
		t.Fatal(err)
	}
	ip, rcode, err := parseAnswerIP(response, id)
	if err != nil {
		t.Fatal(err)
	}
	if rcode != 0 || !ip.Equal(net.IPv4(203, 0, 113, 9)) {
		t.Fatalf("answer = %v rcode %d", ip, rcode)
	}
	for range 2 {
		if err := <-upstreamErrors; err != nil {
			t.Fatal(err)
		}
	}
}

func TestServerForwardsPublicQueryThroughUDPSocket(t *testing.T) {
	t.Parallel()

	upstream, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = upstream.Close() }()
	upstreamErr := make(chan error, 1)
	go func() {
		buffer := make([]byte, 4096)
		size, peer, err := upstream.ReadFromUDP(buffer)
		if err != nil {
			upstreamErr <- err
			return
		}
		response, err := answer(buffer[:size], func(string) net.IP { return net.IPv4(198, 51, 100, 4) }, "")
		if err == nil {
			_, err = upstream.WriteToUDP(response, peer)
		}
		upstreamErr <- err
	}()

	listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(listener, func(string) net.IP { return nil }, Authority(nil), func(query []byte) ([]byte, error) {
		return exchangeDNS(query, upstream.LocalAddr().String())
	})
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve() }()

	client, err := net.DialUDP("udp4", nil, listener.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	if err := client.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	const id = 37
	query, err := encodeQuery("example.com", id)
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
	if rcode != 0 || !ip.Equal(net.IPv4(198, 51, 100, 4)) {
		t.Fatalf("answer = %v rcode %d", ip, rcode)
	}
	if err := <-upstreamErr; err != nil {
		t.Fatal(err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-serveErr; err != nil {
		t.Fatal(err)
	}
}

func BenchmarkExchangeUDP(b *testing.B) {
	upstream, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = upstream.Close() }()

	query, err := encodeQuery("example.com", 41)
	if err != nil {
		b.Fatal(err)
	}
	response, err := answer(query, func(string) net.IP { return net.IPv4(192, 0, 2, 8) }, "")
	if err != nil {
		b.Fatal(err)
	}
	go func() {
		buffer := make([]byte, 4096)
		for {
			_, peer, err := upstream.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			if _, err := upstream.WriteToUDP(response, peer); err != nil {
				return
			}
		}
	}()

	address := upstream.LocalAddr().String()
	if _, err := exchangeUDP(query, address); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := exchangeUDP(query, address); err != nil {
			b.Fatal(err)
		}
	}
}
