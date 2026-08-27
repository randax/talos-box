package helper

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/randax/talos-box/internal/resolverset"
)

const (
	resolverPath                  = resolverset.SharedPath
	shutdownStopMaxAttempts       = 5
	shutdownStopInitialRetryDelay = 25 * time.Millisecond
)

var (
	startInterface = StartInterface
	// convergeHostNetworking is the sync-time bridge/nftables convergence; a
	// variable so tests stay off the real netlink socket.
	convergeHostNetworking = convergeNetworking
	teardownSubnet         = TeardownSubnet
)

type attachmentKey struct {
	cluster string
	node    string
}

type serverReply struct {
	response Response
	fd       int
	cleanup  func()
	finalize func()
}

// Server owns helper-created vmnet interfaces.
type Server struct {
	opMu         sync.Mutex
	attachments  map[attachmentKey]*platformAttachment
	pendingStops map[int]*platformAttachment
	speakers     map[string]bgpSpeaker
	dhcp         dhcpManager
	state        *State
	allowedUID   *uint32
	allowAnyUID  bool

	listenerMu   sync.Mutex
	listener     net.Listener
	closing      bool
	connections  map[net.Conn]struct{}
	connectionWG sync.WaitGroup
}

// NewServer creates an empty helper server over the reservation state tbxd
// pushes. A nil state serves an empty, memory-only set.
func NewServer(state *State, allowedUID *uint32, allowAnyUID ...bool) *Server {
	if state == nil {
		state = NewState("")
	}
	server := &Server{
		attachments:  make(map[attachmentKey]*platformAttachment),
		pendingStops: make(map[int]*platformAttachment),
		state:        state,
	}
	server.dhcp = newPlatformDHCPManager(state.Clusters, server.attachedSubnetIndexes)
	if allowedUID != nil {
		uid := *allowedUID
		server.allowedUID = &uid
	}
	if len(allowAnyUID) != 0 {
		server.allowAnyUID = allowAnyUID[0]
	}
	return server
}

// Listen creates the helper socket, replacing it only when stale.
func Listen(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create helper socket directory: %w", err)
	}
	listener, err := listenUnixSocket(path, net.Listen, net.DialTimeout, os.Remove)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, helperSocketMode()); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("set helper socket permissions: %w", err)
	}
	return listener, nil
}

func listenUnixSocket(
	path string,
	listen func(string, string) (net.Listener, error),
	dial func(string, string, time.Duration) (net.Conn, error),
	remove func(string) error,
) (net.Listener, error) {
	listener, bindErr := listen("unix", path)
	if bindErr == nil {
		return listener, nil
	}
	if !errors.Is(bindErr, unix.EADDRINUSE) {
		return nil, fmt.Errorf("listen on helper socket %s: %w", path, bindErr)
	}
	if connection, dialErr := dial("unix", path, 100*time.Millisecond); dialErr == nil {
		_ = connection.Close()
		return nil, fmt.Errorf("helper socket is already in use: %s", path)
	}
	if removeErr := remove(path); removeErr != nil {
		return nil, errors.Join(
			fmt.Errorf("listen on helper socket %s: %w", path, bindErr),
			fmt.Errorf("remove stale helper socket %s: %w", path, removeErr),
		)
	}
	listener, err := listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on helper socket %s after removing stale path: %w", path, err)
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
	for cluster, speaker := range s.speakers {
		speaker.Stop()
		delete(s.speakers, cluster)
	}
	result = s.dhcp.Close()
	for attempt := 1; ; attempt++ {
		var retainedResult error
		stop := func(attachment *platformAttachment) bool {
			err := attachment.close()
			if err == nil {
				return false
			}
			if stopErrorRetained(err) {
				retainedResult = errors.Join(retainedResult, err)
				return true
			}
			result = errors.Join(result, err)
			return false
		}

		for key, attachment := range s.attachments {
			if !stop(attachment) {
				delete(s.attachments, key)
			}
		}
		for fd, attachment := range s.pendingStops {
			if !stop(attachment) {
				delete(s.pendingStops, fd)
			}
		}

		if retainedResult == nil || attempt == shutdownStopMaxAttempts {
			return errors.Join(result, retainedResult)
		}
		time.Sleep(shutdownStopInitialRetryDelay * time.Duration(attempt))
	}
}

func (s *Server) serveConnection(connection *net.UnixConn) {
	defer func() { _ = connection.Close() }()
	uid, err := peerUID(connection)
	if err != nil {
		authorizationErr := fmt.Errorf("read helper peer credentials: %w", err)
		log.Printf("reject helper connection: %v", authorizationErr)
		_ = sendResponse(connection, failure(authorizationErr), -1)
		return
	}
	if !isAuthorizedPeer(uid, s.allowedUID, s.allowAnyUID) {
		authorizationErr := fmt.Errorf("unauthorized uid %d", uid)
		log.Printf("reject helper connection: %v", authorizationErr)
		_ = sendResponse(connection, failure(authorizationErr), -1)
		return
	}

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
		if reply.finalize != nil {
			reply.finalize()
		}
		s.opMu.Unlock()
		if err != nil {
			return
		}
	}
}

// desiredSubnetIndexes is every subnet the helper must keep host networking
// for: the synced reservations plus the live attachments. The caller holds
// opMu.
func (s *Server) desiredSubnetIndexes() []int {
	indexes := append(s.state.SubnetIndexes(), s.attachedSubnetIndexes()...)
	slices.Sort(indexes)
	return slices.Compact(indexes)
}

// attachedSubnetIndexes reports the subnets live attachments occupy. DHCP must
// serve them even when the synced state does not name them yet — a node cannot
// boot on a subnet with no listener. The caller holds opMu; this must not take
// it again.
func (s *Server) attachedSubnetIndexes() []int {
	indexes := make([]int, 0, len(s.attachments))
	for _, attachment := range s.attachments {
		indexes = append(indexes, attachment.subnetIndex)
	}
	slices.Sort(indexes)
	return slices.Compact(indexes)
}

func isAuthorizedUID(uid uint32, allowedUID *uint32, allowAnyUID bool) bool {
	return uid == 0 || allowedUID != nil && uid == *allowedUID || allowAnyUID && allowedUID == nil
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
	if request.Op == "dns.listen" {
		return s.dnsListenerReply(request.Args)
	}
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
	case "net.teardown":
		return s.teardownNetwork(request.Args)
	case "net.sync":
		return s.sync(request.Args)
	case "dns.install":
		var args struct {
			Port int `json:"port"`
		}
		if err := decodeArgs(request.Args, &args); err != nil {
			return nil, -1, nil, err
		}
		return nil, -1, nil, installHostResolver(args.Port)
	case "dns.uninstall":
		return nil, -1, nil, uninstallHostResolver()
	case "dns.syncDomains":
		var args struct {
			Domains []string `json:"domains"`
			Port    int      `json:"port"`
		}
		if err := decodeArgs(request.Args, &args); err != nil {
			return nil, -1, nil, err
		}
		return nil, -1, nil, syncDomainResolvers(args.Domains, args.Port)
	case "dns.register":
		clusterName, subnetIndex, err := decodeDNSIdentity(request.Args)
		if err != nil {
			return nil, -1, nil, err
		}
		return registerDNS(clusterName, subnetIndex), -1, nil, nil
	case "dns.unregister":
		subnetIndex, err := decodeDNSSubnet(request.Args)
		if err != nil {
			return nil, -1, nil, err
		}
		return nil, -1, nil, unregisterDNS(subnetIndex)
	case "forwarding.enable":
		return nil, -1, nil, enableForwarding()
	case "bgp.enable":
		return nil, -1, nil, s.enableBGP(request.Args)
	case "bgp.disable":
		return nil, -1, nil, s.disableBGP(request.Args)
	case "bgp.status":
		var args struct {
			Cluster string `json:"cluster"`
		}
		if err := decodeArgs(request.Args, &args); err != nil {
			return nil, -1, nil, err
		}
		if err := validateBGPCluster(args.Cluster); err != nil {
			return nil, -1, nil, err
		}
		speaker, active := s.speakers[args.Cluster]
		return bgpState(speaker, active), -1, nil, nil
	case helperInfoOp:
		var args struct {
			ProtocolVersion int `json:"protocolVersion"`
		}
		if err := decodeArgs(request.Args, &args); err != nil {
			return nil, -1, nil, err
		}
		if args.ProtocolVersion != protocolVersion {
			return nil, -1, nil, protocolMismatchError(args.ProtocolVersion, protocolVersion)
		}
		return currentHelperInfo()
	case "ping":
		return map[string]bool{"pong": true}, -1, nil, nil
	default:
		return nil, -1, nil, fmt.Errorf("unknown operation %q", request.Op)
	}
}

// sync adopts the reservations tbxd pushes and reconverges the host state they
// describe. tbxd owns cluster state; the helper only holds this copy, so it can
// serve DHCP and rebuild bridges without ever reading a user's home.
func (s *Server) sync(raw json.RawMessage) (any, int, func(), error) {
	var args struct {
		Clusters []SyncedCluster `json:"clusters"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, -1, nil, err
	}
	if err := s.state.Replace(args.Clusters); err != nil {
		return nil, -1, nil, err
	}
	if err := convergeHostNetworking(s.desiredSubnetIndexes()); err != nil {
		if !isSubnetPreflightError(err) {
			return nil, -1, nil, fmt.Errorf("converge helper networking: %w", err)
		}
		// A captured subnet is that cluster's problem, reported by its own
		// attach; the sync must not fail the unrelated cluster that sent it.
		log.Printf("net.sync: %v", err)
	}
	if err := s.dhcp.Converge(); err != nil {
		return nil, -1, nil, err
	}
	return struct{}{}, -1, nil, nil
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
	attachment, err := startInterface(s.desiredSubnetIndexes(), *args.SubnetIndex, args.Cluster, args.Node)
	if err != nil {
		return nil, -1, nil, err
	}
	attachment.subnetIndex = *args.SubnetIndex
	s.attachments[key] = attachment
	if err := s.dhcp.Converge(); err != nil {
		delete(s.attachments, key)
		return nil, -1, nil, errors.Join(err, attachment.close())
	}
	cleanup := func() {
		if current, ok := s.attachments[key]; ok && current == attachment {
			err := attachment.close()
			// The response (and therefore the descriptor) never reached the
			// caller. Always release the logical attachment so its natural retry
			// cannot be wedged by a teardown-only retained vmnet handle.
			delete(s.attachments, key)
			if err != nil {
				if stopErrorRetained(err) {
					s.pendingStops[attachment.FD] = attachment
				}
				log.Printf(
					"clean up undelivered network attachment %s/%s fd %d (retained=%t): %v",
					args.Cluster,
					args.Node,
					attachment.FD,
					stopErrorRetained(err),
					err,
				)
			}
		}
	}
	return map[string]any{"cluster": args.Cluster, "node": args.Node, "kind": attachment.Kind}, attachment.FD, cleanup, nil
}

// teardownNetwork removes the host networking a subnet's last cluster leaves
// behind. The synced set is tbxd's word, not proof — destroy syncs after the
// state is gone, and a stale copy must not pin a bridge — so the guarantee
// is the primitive's own: DeleteBridge refuses a bridge that still has links
// enslaved (a live VM's only path), and an absent bridge is success. The
// destroy path stops the cluster's VMs, which detaches their taps, before it
// asks for this.
//
// The subnet's DHCP server goes with the bridge: its socket is bound to that
// bridge's ifindex, so a bridge rebuilt under the same name — the point of
// removing it, subnet indexes are reused — would be served by a socket
// listening on an interface that no longer exists. The release only follows a
// bridge that is gone (removed, or already absent); a refusal means a live VM
// is still enslaved, and that cluster keeps its DHCP.
func (s *Server) teardownNetwork(raw json.RawMessage) (any, int, func(), error) {
	var args struct {
		SubnetIndex *int `json:"subnetIndex"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, -1, nil, err
	}
	if args.SubnetIndex == nil {
		return nil, -1, nil, errors.New("subnetIndex is required")
	}
	if *args.SubnetIndex < 0 || *args.SubnetIndex > 255 {
		return nil, -1, nil, fmt.Errorf("subnet index %d is outside 0..255", *args.SubnetIndex)
	}
	removed, err := teardownSubnet(*args.SubnetIndex)
	if err != nil {
		return nil, -1, nil, err
	}
	if err := s.dhcp.Release(*args.SubnetIndex); err != nil {
		return nil, -1, nil, err
	}
	return map[string]bool{"removed": removed}, -1, nil, nil
}

func validateBGPEnableArgs(cluster string, subnetIndex *int, localASN, peerASN uint32) error {
	if err := validateBGPCluster(cluster); err != nil {
		return err
	}
	if subnetIndex == nil {
		return errors.New("subnetIndex is required")
	}
	if *subnetIndex < 0 || *subnetIndex > 255 {
		return fmt.Errorf("subnet index %d is outside 0..255", *subnetIndex)
	}
	if localASN == 0 || peerASN == 0 {
		return errors.New("BGP ASNs must be non-zero")
	}
	return nil
}

func validateBGPCluster(cluster string) error {
	if cluster == "" {
		return errors.New("cluster is required")
	}
	return nil
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
	attachment, ok := s.attachments[key]
	if !ok {
		// Idempotent: the descriptor owner may already have released a
		// non-persistent tap, or the helper may have restarted since attach.
		return nil
	}
	if err := attachment.close(); err != nil {
		if !stopErrorRetained(err) {
			delete(s.attachments, key)
		}
		return err
	}
	delete(s.attachments, key)
	return nil
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
	// Writing follows symlinks; as root that must never happen.
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("resolver path %s exists but is not a regular file; remove it manually", path)
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
