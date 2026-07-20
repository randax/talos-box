package helper

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const resolverPath = "/etc/resolver/k8s.test"

type attachmentKey struct {
	cluster string
	node    string
}

type serverReply struct {
	response Response
	fd       int
	cleanup  func()
}

// Server owns helper-created vmnet interfaces.
type Server struct {
	opMu        sync.Mutex
	attachments map[attachmentKey]int

	listenerMu   sync.Mutex
	listener     net.Listener
	closing      bool
	connections  map[net.Conn]struct{}
	connectionWG sync.WaitGroup
}

// NewServer creates an empty helper server.
func NewServer() *Server {
	return &Server{attachments: make(map[attachmentKey]int)}
}

// Listen creates the helper socket, replacing it only when stale.
func Listen(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create helper socket directory: %w", err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		if connection, dialErr := net.DialTimeout("unix", path, 100*time.Millisecond); dialErr == nil {
			_ = connection.Close()
			return nil, fmt.Errorf("helper socket is already in use: %s", path)
		}
		if removeErr := os.Remove(path); removeErr != nil {
			return nil, fmt.Errorf("remove stale helper socket: %w", removeErr)
		}
		listener, err = net.Listen("unix", path)
	}
	if err != nil {
		return nil, fmt.Errorf("listen on helper socket: %w", err)
	}
	// TODO: restrict access after adding peer-credential authorization.
	if err := os.Chmod(path, 0o666); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("set helper socket permissions: %w", err)
	}
	return listener, nil
}

// Serve accepts helper connections until Shutdown closes the listener.
func (s *Server) Serve(listener net.Listener) error {
	s.listenerMu.Lock()
	s.listener = listener
	if s.connections == nil {
		s.connections = make(map[net.Conn]struct{})
	}
	closing := s.closing
	s.listenerMu.Unlock()
	if closing {
		_ = listener.Close()
		return nil
	}

	for {
		connection, err := listener.Accept()
		if err != nil {
			s.listenerMu.Lock()
			closing := s.closing
			s.listenerMu.Unlock()
			if closing || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept helper connection: %w", err)
		}
		unixConnection, ok := connection.(*net.UnixConn)
		if !ok {
			_ = connection.Close()
			continue
		}
		s.listenerMu.Lock()
		if s.closing {
			s.listenerMu.Unlock()
			_ = connection.Close()
			continue
		}
		s.connections[connection] = struct{}{}
		s.connectionWG.Add(1)
		s.listenerMu.Unlock()
		go func() {
			defer func() {
				s.listenerMu.Lock()
				delete(s.connections, connection)
				s.listenerMu.Unlock()
				s.connectionWG.Done()
			}()
			s.serveConnection(unixConnection)
		}()
	}
}

// Shutdown closes connections and stops every helper-owned interface.
func (s *Server) Shutdown() error {
	s.listenerMu.Lock()
	s.closing = true
	listener := s.listener
	connections := make([]net.Conn, 0, len(s.connections))
	for connection := range s.connections {
		connections = append(connections, connection)
	}
	s.listenerMu.Unlock()
	if listener != nil {
		_ = listener.Close()
	}
	for _, connection := range connections {
		_ = connection.Close()
	}
	s.connectionWG.Wait()

	s.opMu.Lock()
	defer s.opMu.Unlock()
	var result error
	for key, fd := range s.attachments {
		result = errors.Join(result, StopInterface(fd))
		delete(s.attachments, key)
	}
	return result
}

func (s *Server) serveConnection(connection *net.UnixConn) {
	defer func() { _ = connection.Close() }()
	decoder := json.NewDecoder(connection)
	for {
		var request Request
		if err := decoder.Decode(&request); err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) && !errors.Is(err, os.ErrClosed) {
				_ = sendResponse(connection, failure(fmt.Errorf("decode request: %w", err)), -1)
			}
			return
		}
		s.opMu.Lock()
		reply := s.dispatch(request)
		err := sendResponse(connection, reply.response, reply.fd)
		if err != nil && reply.cleanup != nil {
			reply.cleanup()
		}
		s.opMu.Unlock()
		if err != nil {
			return
		}
	}
}

func sendResponse(connection *net.UnixConn, response Response, fd int) error {
	wire, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("encode helper response: %w", err)
	}
	wire = append(wire, '\n')
	if fd < 0 {
		return writeAll(connection, wire)
	}
	rights := unix.UnixRights(fd)
	n, oobn, err := connection.WriteMsgUnix(wire, rights, nil)
	if err != nil {
		return fmt.Errorf("write helper response with file descriptor: %w", err)
	}
	if oobn != len(rights) {
		return fmt.Errorf("write helper response file descriptor: wrote %d of %d control bytes", oobn, len(rights))
	}
	if n < len(wire) {
		return writeAll(connection, wire[n:])
	}
	return nil
}

func (s *Server) dispatch(request Request) serverReply {
	data, fd, cleanup, err := s.handle(request)
	if err != nil {
		return serverReply{response: failure(err), fd: -1}
	}
	return serverReply{response: success(data), fd: fd, cleanup: cleanup}
}

func (s *Server) handle(request Request) (any, int, func(), error) {
	switch request.Op {
	case "net.attach":
		return s.attach(request.Args)
	case "net.detach":
		return nil, -1, nil, s.detach(request.Args)
	case "dns.install":
		var args struct {
			Port int `json:"port"`
		}
		if err := decodeArgs(request.Args, &args); err != nil {
			return nil, -1, nil, err
		}
		return nil, -1, nil, installResolver(resolverPath, args.Port)
	case "dns.uninstall":
		err := os.Remove(resolverPath)
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		} else if err != nil {
			err = fmt.Errorf("remove resolver file: %w", err)
		}
		return nil, -1, nil, err
	case "forwarding.enable":
		return nil, -1, nil, enableForwarding()
	case "ping":
		return map[string]bool{"pong": true}, -1, nil, nil
	default:
		return nil, -1, nil, fmt.Errorf("unknown operation %q", request.Op)
	}
}

func (s *Server) attach(raw json.RawMessage) (any, int, func(), error) {
	var args struct {
		Cluster     string `json:"cluster"`
		SubnetIndex *int   `json:"subnetIndex"`
		Node        string `json:"node"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, -1, nil, err
	}
	if args.Cluster == "" || args.Node == "" {
		return nil, -1, nil, errors.New("cluster and node are required")
	}
	if args.SubnetIndex == nil {
		return nil, -1, nil, errors.New("subnetIndex is required")
	}
	if *args.SubnetIndex < 0 || *args.SubnetIndex > 255 {
		return nil, -1, nil, fmt.Errorf("subnet index %d is outside 0..255", *args.SubnetIndex)
	}
	key := attachmentKey{cluster: args.Cluster, node: args.Node}
	if _, exists := s.attachments[key]; exists {
		return nil, -1, nil, fmt.Errorf("network interface for %s/%s is already attached", args.Cluster, args.Node)
	}
	fd, err := StartInterface(*args.SubnetIndex)
	if err != nil {
		return nil, -1, nil, err
	}
	s.attachments[key] = fd
	cleanup := func() {
		if current, ok := s.attachments[key]; ok && current == fd {
			delete(s.attachments, key)
			_ = StopInterface(fd)
		}
	}
	return map[string]string{"cluster": args.Cluster, "node": args.Node}, fd, cleanup, nil
}

func (s *Server) detach(raw json.RawMessage) error {
	var args struct {
		Cluster string `json:"cluster"`
		Node    string `json:"node"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return err
	}
	if args.Cluster == "" || args.Node == "" {
		return errors.New("cluster and node are required")
	}
	key := attachmentKey{cluster: args.Cluster, node: args.Node}
	fd, ok := s.attachments[key]
	if !ok {
		// idempotent: the pump already cleaned up when the VM closed its fd
		return nil
	}
	delete(s.attachments, key)
	return StopInterface(fd)
}

func decodeArgs(raw json.RawMessage, destination any) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(raw, destination); err != nil {
		return fmt.Errorf("decode arguments: %w", err)
	}
	return nil
}

func resolverContent(port int) ([]byte, error) {
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("DNS port %d is outside 1..65535", port)
	}
	return []byte(fmt.Sprintf("nameserver 127.0.0.1\nport %d\n", port)), nil
}

func installResolver(path string, port int) error {
	content, err := resolverContent(port)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create resolver directory: %w", err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return fmt.Errorf("write resolver file: %w", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		return fmt.Errorf("set resolver permissions: %w", err)
	}
	return nil
}

func enableForwarding() error {
	output, err := exec.Command("/usr/sbin/sysctl", "-w", "net.inet.ip.forwarding=1").CombinedOutput()
	if err != nil {
		return fmt.Errorf("enable IP forwarding: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}
