package dns

import (
	"errors"
	"fmt"
	"net"
	"time"
)

const (
	Port    = 5399
	Address = "127.0.0.1:5399"
)

// Server answers DNS requests over UDP.
type Server struct {
	connection *net.UDPConn
	lookup     func(string) net.IP
}

func Listen(address string, lookup func(string) net.IP) (*Server, error) {
	udpAddress, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return nil, fmt.Errorf("resolve DNS address: %w", err)
	}
	connection, err := net.ListenUDP("udp", udpAddress)
	if err != nil {
		return nil, fmt.Errorf("listen for DNS: %w", err)
	}
	return &Server{connection: connection, lookup: lookup}, nil
}

func (s *Server) Serve() error {
	buffer := make([]byte, 4096)
	for {
		size, peer, err := s.connection.ReadFromUDP(buffer)
		if err != nil {
			if isClosed(err) {
				return nil
			}
			return fmt.Errorf("read DNS request: %w", err)
		}
		response, err := answer(buffer[:size], s.lookup)
		if err != nil {
			continue
		}
		if _, err := s.connection.WriteToUDP(response, peer); err != nil && !isClosed(err) {
			return fmt.Errorf("write DNS response: %w", err)
		}
	}
}

func (s *Server) Close() error {
	return s.connection.Close()
}

// Probe verifies that address returns a syntactically valid response.
func Probe(address string) error {
	server, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return err
	}
	connection, err := net.DialUDP("udp", nil, server)
	if err != nil {
		return err
	}
	defer func() { _ = connection.Close() }()
	if err := connection.SetDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	const id = 0x7462
	query, err := encodeQuery("doctor.invalid.k8s.test", id)
	if err != nil {
		return err
	}
	if _, err := connection.Write(query); err != nil {
		return err
	}
	buffer := make([]byte, 4096)
	size, err := connection.Read(buffer)
	if err != nil {
		return err
	}
	_, rcode, err := parseAnswerIP(buffer[:size], id)
	if err != nil {
		return err
	}
	if rcode != 0 && rcode != 3 {
		return fmt.Errorf("DNS response code is %d", rcode)
	}
	return nil
}

func isClosed(err error) bool {
	return errors.Is(err, net.ErrClosed)
}
