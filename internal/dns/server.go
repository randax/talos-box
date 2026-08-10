package dns

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

const (
	Port    = 5399
	Address = "127.0.0.1:5399"
)

// Server answers DNS requests over UDP.
type Server struct {
	connection net.PacketConn
	lookup     func(string) net.IP
	forward    func([]byte) ([]byte, error)
}

// NewServer serves DNS over an already-bound packet connection. This is the
// entry point used when the privileged helper passes tbxd a UDP/53 socket.
func NewServer(connection net.PacketConn, lookup func(string) net.IP, forward func([]byte) ([]byte, error)) *Server {
	return &Server{connection: connection, lookup: lookup, forward: forward}
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
	return NewServer(connection, lookup, SystemForward), nil
}

func (s *Server) Serve() error {
	buffer := make([]byte, 4096)
	for {
		size, peer, err := s.connection.ReadFrom(buffer)
		if err != nil {
			if isClosed(err) {
				return nil
			}
			return fmt.Errorf("read DNS request: %w", err)
		}
		response, err := s.response(buffer[:size])
		if err != nil {
			continue
		}
		if _, err := s.connection.WriteTo(response, peer); err != nil && !isClosed(err) {
			return fmt.Errorf("write DNS response: %w", err)
		}
	}
}

func (s *Server) response(query []byte) ([]byte, error) {
	q, err := parseQuestion(query)
	if err != nil {
		return nil, err
	}
	if isAuthoritativeName(q.name) {
		return answer(query, s.lookup)
	}
	if s.forward == nil {
		return answer(query, s.lookup)
	}
	response, err := s.forward(query)
	if err != nil {
		return errorAnswer(query, 2)
	}
	return response, nil
}

func isAuthoritativeName(name string) bool {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	return name == "k8s.test" || strings.HasSuffix(name, ".k8s.test")
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
