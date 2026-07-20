package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/imagecache"
	"github.com/randax/talos-box/internal/vm"
	"golang.org/x/sys/unix"
)

// Server owns all VMs started by one daemon process.
type Server struct {
	cache *imagecache.Cache

	opMu             sync.Mutex
	vms              map[string]map[string]*vm.VM
	defaultSchematic string

	listenerMu   sync.Mutex
	listener     net.Listener
	closing      bool
	connections  map[net.Conn]struct{}
	connectionWG sync.WaitGroup
}

type lockedListener struct {
	net.Listener
	lock *os.File
}

func (l *lockedListener) Close() error {
	// Keep the process lock until exit so a replacement daemon cannot bind while
	// this daemon is still stopping VMs and cleaning up its socket.
	err := l.Listener.Close()
	runtime.KeepAlive(l.lock)
	return err
}

// NewServer creates a daemon using the default image cache.
func NewServer() (*Server, error) {
	cache, err := imagecache.NewDefault()
	if err != nil {
		return nil, err
	}
	return &Server{cache: cache, vms: make(map[string]map[string]*vm.VM)}, nil
}

// Listen creates the daemon socket, replacing it only when it is stale.
func Listen(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create daemon directory: %w", err)
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open daemon lock: %w", err)
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("another daemon owns %s: %w", path, err)
	}
	closeLock := func() {
		_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
		_ = lock.Close()
	}

	listener, err := net.Listen("unix", path)
	if err != nil {
		if connection, dialErr := net.DialTimeout("unix", path, 100*time.Millisecond); dialErr == nil {
			_ = connection.Close()
			closeLock()
			return nil, fmt.Errorf("daemon socket is already in use: %s", path)
		}
		if removeErr := os.Remove(path); removeErr != nil {
			closeLock()
			return nil, fmt.Errorf("remove stale daemon socket: %w", removeErr)
		}
		listener, err = net.Listen("unix", path)
	}
	if err != nil {
		closeLock()
		return nil, fmt.Errorf("listen on daemon socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		closeLock()
		return nil, fmt.Errorf("set daemon socket permissions: %w", err)
	}
	return &lockedListener{Listener: listener, lock: lock}, nil
}

// Serve accepts daemon protocol connections until Shutdown closes listener.
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
			return fmt.Errorf("accept daemon connection: %w", err)
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
			s.serveConnection(connection)
		}()
	}
}

// Shutdown stops accepting requests and gracefully closes every running VM.
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
	var all []*vm.VM
	for _, nodes := range s.vms {
		for _, machine := range nodes {
			all = append(all, machine)
		}
	}
	err := closeVMs(all)
	s.vms = make(map[string]map[string]*vm.VM)
	return err
}

func (s *Server) serveConnection(connection net.Conn) {
	defer func() { _ = connection.Close() }()
	decoder := json.NewDecoder(connection)
	encoder := json.NewEncoder(connection)
	for {
		var request Request
		if err := decoder.Decode(&request); err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			if !errors.Is(err, net.ErrClosed) && !errors.Is(err, os.ErrClosed) {
				_ = encoder.Encode(failure(fmt.Errorf("decode request: %w", err)))
			}
			return
		}
		if err := encoder.Encode(s.dispatch(request)); err != nil {
			return
		}
	}
}

func (s *Server) dispatch(request Request) Response {
	s.opMu.Lock()
	defer s.opMu.Unlock()

	data, err := s.handle(request)
	if err != nil {
		return failure(err)
	}
	return success(data)
}

func (s *Server) handle(request Request) (any, error) {
	switch request.Op {
	case "daemon.ping":
		return map[string]bool{"pong": true}, nil
	case "up":
		return s.up(request.Args)
	case "down":
		return s.down(request.Args)
	case "cluster.create":
		return s.createCluster(request.Args)
	case "cluster.start":
		return s.startCluster(request.Args)
	case "cluster.stop":
		return s.stopCluster(request.Args)
	case "cluster.destroy":
		return s.destroyCluster(request.Args)
	case "cluster.list":
		return s.listClusters()
	case "node.add":
		return s.addNode(request.Args)
	case "node.remove":
		return s.removeNode(request.Args)
	case "status":
		return s.status(request.Args)
	case "snapshot.create":
		return s.snapshotCreate(request.Args)
	case "snapshot.restore":
		return s.snapshotRestore(request.Args)
	case "snapshot.list":
		return s.snapshotList(request.Args)
	case "snapshot.delete":
		return s.snapshotDelete(request.Args)
	case "bgp.enable":
		return s.setBGP(request.Args, true)
	case "bgp.disable":
		return s.setBGP(request.Args, false)
	case "cache.pull":
		return s.pullCache(request.Args)
	case "cache.prune":
		return s.pruneCache()
	default:
		return nil, fmt.Errorf("unknown operation %q", request.Op)
	}
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

func closeVMs(machines []*vm.VM) error {
	errorsByVM := make(chan error, len(machines))
	var wait sync.WaitGroup
	for _, machine := range machines {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errorsByVM <- machine.Close()
		}()
	}
	wait.Wait()
	close(errorsByVM)
	var result error
	for err := range errorsByVM {
		result = errors.Join(result, err)
	}
	return result
}

func removeNodeFiles(clusterName, nodeName string) error {
	dir, err := cluster.Dir(clusterName)
	if err != nil {
		return err
	}
	for _, suffix := range []string{".img", ".efi", ".console.sock"} {
		if err := os.Remove(filepath.Join(dir, nodeName+suffix)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove node file: %w", err)
		}
	}
	return nil
}

func sortedNodeNames(nodes map[string]*vm.VM) []string {
	names := make([]string, 0, len(nodes))
	for name := range nodes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
