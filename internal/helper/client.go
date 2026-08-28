package helper

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/randax/talos-box/internal/cluster"
)

// The helper only performs short admin operations, so every interaction is
// bounded: a stuck helper must not wedge daemon shutdown or cluster ops.
const (
	dialTimeout  = 5 * time.Second
	callTimeout  = 30 * time.Second
	probeTimeout = time.Second
)

// Client is a serialized connection to tbx-helper.
type Client struct {
	socketPath string
	connection *net.UnixConn
	info       Info
	mu         sync.Mutex
}

// Connect connects to the helper socket selected for this process.
func Connect() (*Client, error) {
	socketPath, err := SocketPath()
	if err != nil {
		return nil, err
	}
	connection, info, err := dialHelper(socketPath, dialTimeout, callTimeout, true)
	if err != nil {
		return nil, err
	}
	return &Client{socketPath: socketPath, connection: connection, info: info}, nil
}

// Probe returns the helper's diagnostic identity without enforcing the protocol
// match that Connect uses for operational traffic.
func Probe() (Info, error) {
	socketPath, err := SocketPath()
	if err != nil {
		return Info{}, err
	}
	connection, info, err := dialHelper(socketPath, probeTimeout, probeTimeout, false)
	if err != nil {
		return Info{}, err
	}
	_ = connection.Close()
	return info, nil
}

func dialHelper(socketPath string, dialDeadline, handshakeDeadline time.Duration, requireProtocolMatch bool) (*net.UnixConn, Info, error) {
	dialer := net.Dialer{Timeout: dialDeadline}
	connection, err := dialer.Dial("unix", socketPath)
	if err != nil {
		return nil, Info{}, fmt.Errorf("connect to helper at %s: %w", socketPath, err)
	}
	unixConnection := connection.(*net.UnixConn)
	info, err := handshakeHelper(unixConnection, handshakeDeadline, requireProtocolMatch)
	if err != nil {
		_ = unixConnection.Close()
		return nil, Info{}, err
	}
	return unixConnection, info, nil
}

// Close closes the helper connection.
func (c *Client) Close() error {
	return c.connection.Close()
}

// Info returns the connected helper's self-reported protocol and capability state.
func (c *Client) Info() (Info, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info, nil
}

// attach creates a vmnet interface and returns its datagram socket descriptor.
func (c *Client) attach(cluster string, subnetIndex int, node string) (AttachmentKind, int, error) {
	args := struct {
		Cluster     string `json:"cluster"`
		SubnetIndex int    `json:"subnetIndex"`
		Node        string `json:"node"`
	}{Cluster: cluster, SubnetIndex: subnetIndex, Node: node}
	response, fd, err := c.call("net.attach", args, true)
	if err != nil {
		return "", -1, err
	}
	var data struct {
		Kind AttachmentKind `json:"kind"`
	}
	if err := json.Unmarshal(response.Data, &data); err != nil {
		_ = unix.Close(fd)
		return "", -1, fmt.Errorf("decode attach response: %w", err)
	}
	if data.Kind == "" {
		_ = unix.Close(fd)
		return "", -1, errors.New("attach response omitted attachment kind")
	}
	return data.Kind, fd, nil
}

// Detach stops and removes a node's vmnet interface.
func (c *Client) Detach(cluster, node string) error {
	_, _, err := c.call("net.detach", map[string]string{"cluster": cluster, "node": node}, false)
	return err
}

// TeardownSubnet removes the host bridge for a subnet whose last cluster is
// gone, so the next create reuses the freed index instead of climbing to a new
// one. It reports whether a bridge was there to remove.
func (c *Client) TeardownSubnet(subnetIndex int) (bool, error) {
	response, _, err := c.call("net.teardown", map[string]int{"subnetIndex": subnetIndex}, false)
	if err != nil {
		return false, err
	}
	var data struct {
		Removed bool `json:"removed"`
	}
	if err := json.Unmarshal(response.Data, &data); err != nil {
		return false, fmt.Errorf("decode teardown response: %w", err)
	}
	return data.Removed, nil
}

// Sync pushes the cluster reservations tbxd owns to the helper, which persists
// them and reconverges the host networking they describe. It is the helper's
// only source of cluster state.
func (c *Client) Sync(clusters []cluster.Cluster) error {
	synced := make([]SyncedCluster, 0, len(clusters))
	for _, item := range clusters {
		converted := SyncedCluster{
			Name:        item.Name,
			SubnetIndex: item.SubnetIndex,
			Nodes:       make([]SyncedNode, 0, len(item.Nodes)),
		}
		for _, node := range item.Nodes {
			converted.Nodes = append(converted.Nodes, SyncedNode{Name: node.Name, MAC: node.MAC, IP: node.IP})
		}
		synced = append(synced, converted)
	}
	_, _, err := c.call("net.sync", map[string]any{"clusters": synced}, false)
	return err
}

// InstallDNS installs the k8s.test scoped resolver.
func (c *Client) InstallDNS(port int) error {
	_, _, err := c.call("dns.install", map[string]int{"port": port}, false)
	return err
}

// UninstallDNS removes the k8s.test scoped resolver and any remaining
// marker-managed per-domain resolver files.
func (c *Client) UninstallDNS() error {
	_, _, err := c.call("dns.uninstall", struct{}{}, false)
	return err
}

// SyncDomainResolvers converges the per-domain macOS resolver files to the
// given set of custom cluster domains.
func (c *Client) SyncDomainResolvers(domains []string, port int) error {
	_, _, err := c.call("dns.syncDomains", map[string]any{
		"domains": domains, "port": port,
	}, false)
	return err
}

// ListenDNS asks the helper to bind a cluster gateway's UDP/53 socket and
// transfers ownership of the bound descriptor to the caller. domain is the
// cluster's effective domain; empty means the default, <cluster>.k8s.test.
func (c *Client) ListenDNS(cluster, domain string, subnetIndex int) (*net.UDPConn, DNSRegistration, error) {
	response, fd, err := c.call("dns.listen", map[string]any{
		"cluster": cluster, "domain": domain, "subnetIndex": subnetIndex,
	}, true)
	if err != nil {
		return nil, DNSRegistration{}, err
	}
	var registration DNSRegistration
	if err := json.Unmarshal(response.Data, &registration); err != nil {
		_ = unix.Close(fd)
		return nil, DNSRegistration{}, fmt.Errorf("decode DNS registration: %w", err)
	}
	file := os.NewFile(uintptr(fd), fmt.Sprintf("%s.dns", cluster))
	if file == nil {
		_ = unix.Close(fd)
		return nil, DNSRegistration{}, fmt.Errorf("wrap DNS descriptor %d", fd)
	}
	connection, err := net.FilePacketConn(file)
	_ = file.Close()
	if err != nil {
		return nil, DNSRegistration{}, fmt.Errorf("open passed DNS descriptor: %w", err)
	}
	udp, ok := connection.(*net.UDPConn)
	if !ok {
		_ = connection.Close()
		return nil, DNSRegistration{}, fmt.Errorf("passed DNS descriptor is %T, want UDP", connection)
	}
	return udp, registration, nil
}

// RegisterDNS re-asserts the systemd-resolved route-only domain for a
// cluster. Registration failure is represented in the returned status.
func (c *Client) RegisterDNS(cluster, domain string, subnetIndex int) (DNSRegistration, error) {
	response, _, err := c.call("dns.register", map[string]any{
		"cluster": cluster, "domain": domain, "subnetIndex": subnetIndex,
	}, false)
	if err != nil {
		return DNSRegistration{}, err
	}
	var registration DNSRegistration
	if err := json.Unmarshal(response.Data, &registration); err != nil {
		return DNSRegistration{}, fmt.Errorf("decode DNS registration: %w", err)
	}
	return registration, nil
}

// UnregisterDNS removes a cluster's systemd-resolved per-link configuration.
func (c *Client) UnregisterDNS(subnetIndex int) error {
	_, _, err := c.call("dns.unregister", map[string]int{"subnetIndex": subnetIndex}, false)
	return err
}

// EnableForwarding enables IPv4 forwarding on the host.
func (c *Client) EnableForwarding() error {
	_, _, err := c.call("forwarding.enable", struct{}{}, false)
	return err
}

// EnableBGP starts the host BGP speaker for a cluster.
func (c *Client) EnableBGP(cluster string, subnetIndex int, localASN, peerASN uint32) error {
	_, _, err := c.call("bgp.enable", map[string]any{
		"cluster": cluster, "subnetIndex": subnetIndex, "localASN": localASN, "peerASN": peerASN,
	}, false)
	return err
}

// DisableBGP stops a cluster's host BGP speaker.
func (c *Client) DisableBGP(cluster string) error {
	_, _, err := c.call("bgp.disable", map[string]any{"cluster": cluster}, false)
	return err
}

// HasBGP reports whether the helper currently owns a BGP speaker for cluster.
func (c *Client) HasBGP(cluster string) (bool, error) {
	state, err := c.BGPStatus(cluster)
	return state.Active, err
}

// BGPStatus reports the helper's speaker state for cluster: whether it is
// running and, when it is, the routes it has installed in the host FIB.
func (c *Client) BGPStatus(cluster string) (BGPState, error) {
	response, _, err := c.call("bgp.status", map[string]any{"cluster": cluster}, false)
	if err != nil {
		return BGPState{}, err
	}
	var state BGPState
	if err := json.Unmarshal(response.Data, &state); err != nil {
		return BGPState{}, fmt.Errorf("decode BGP status: %w", err)
	}
	return state, nil
}

// Ping verifies that the helper is responsive.
func (c *Client) Ping() error {
	response, _, err := c.call("ping", struct{}{}, false)
	if err != nil {
		return err
	}
	var data struct {
		Pong bool `json:"pong"`
	}
	if err := json.Unmarshal(response.Data, &data); err != nil {
		return fmt.Errorf("decode ping response: %w", err)
	}
	if !data.Pong {
		return errors.New("helper did not return pong")
	}
	return nil
}

func (c *Client) call(op string, args any, wantFD bool) (Response, int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	response, fd, err := c.callLocked(op, args, wantFD)
	if err == nil {
		return response, fd, nil
	}
	if !shouldReconnect(op, err) {
		return Response{}, -1, err
	}
	if reconnectErr := c.reconnectLocked(); reconnectErr != nil {
		return Response{}, -1, errors.Join(err, reconnectErr)
	}
	return c.callLocked(op, args, wantFD)
}

func (c *Client) callLocked(op string, args any, wantFD bool) (Response, int, error) {
	if err := c.connection.SetDeadline(time.Now().Add(callTimeout)); err != nil {
		return Response{}, -1, fmt.Errorf("set helper call deadline: %w", err)
	}
	defer func() { _ = c.connection.SetDeadline(time.Time{}) }()

	rawArgs, err := json.Marshal(args)
	if err != nil {
		return Response{}, -1, fmt.Errorf("encode request arguments: %w", err)
	}
	wire, err := json.Marshal(Request{Op: op, Args: rawArgs})
	if err != nil {
		return Response{}, -1, fmt.Errorf("encode request: %w", err)
	}
	wire = append(wire, '\n')
	if err := writeAll(c.connection, wire); err != nil {
		if responseErr := earlyHelperResponse(c.connection, err); responseErr != nil {
			return Response{}, -1, responseErr
		}
		return Response{}, -1, fmt.Errorf("write helper request: %w", err)
	}
	return receiveResponse(c.connection, wantFD)
}

func (c *Client) reconnectLocked() error {
	_ = c.connection.Close()
	connection, info, err := dialHelper(c.socketPath, dialTimeout, callTimeout, true)
	if err != nil {
		return fmt.Errorf("reconnect to helper at %s: %w", c.socketPath, err)
	}
	c.connection = connection
	c.info = info
	return nil
}

func handshakeHelper(connection *net.UnixConn, timeout time.Duration, requireProtocolMatch bool) (Info, error) {
	response, _, err := exchangeRequest(connection, helperInfoOp, map[string]int{"protocolVersion": ProtocolVersion}, false, timeout)
	if err != nil {
		return Info{}, err
	}
	if !response.OK {
		return Info{}, protocolHandshakeFailure(response.Error)
	}
	var info Info
	if err := json.Unmarshal(response.Data, &info); err != nil {
		return Info{}, fmt.Errorf("decode helper info: %w", err)
	}
	if requireProtocolMatch && info.ProtocolVersion != ProtocolVersion {
		return Info{}, protocolMismatchError(ProtocolVersion, info.ProtocolVersion)
	}
	return info, nil
}

func exchangeRequest(connection *net.UnixConn, op string, args any, wantFD bool, timeout time.Duration) (Response, int, error) {
	if err := connection.SetDeadline(time.Now().Add(timeout)); err != nil {
		return Response{}, -1, fmt.Errorf("set helper call deadline: %w", err)
	}
	defer func() { _ = connection.SetDeadline(time.Time{}) }()

	rawArgs, err := json.Marshal(args)
	if err != nil {
		return Response{}, -1, fmt.Errorf("encode request arguments: %w", err)
	}
	wire, err := json.Marshal(Request{Op: op, Args: rawArgs})
	if err != nil {
		return Response{}, -1, fmt.Errorf("encode request: %w", err)
	}
	wire = append(wire, '\n')
	if err := writeAll(connection, wire); err != nil {
		if responseErr := earlyHelperResponse(connection, err); responseErr != nil {
			return Response{}, -1, responseErr
		}
		return Response{}, -1, fmt.Errorf("write helper request: %w", err)
	}
	return receiveRawResponse(connection, wantFD)
}

func receiveRawResponse(connection *net.UnixConn, wantFD bool) (Response, int, error) {
	return receiveDecodedResponse(connection, wantFD, false)
}

func shouldReconnect(op string, err error) bool {
	if !safeRetryOperation(op) {
		return false
	}
	return errors.Is(err, unix.EPIPE) ||
		errors.Is(err, unix.ECONNRESET) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed)
}

func safeRetryOperation(op string) bool {
	switch op {
	// net.teardown is idempotent: a retry that finds the bridge already gone
	// succeeds, reporting only that it removed nothing. The ambiguity is
	// benign — a retry after a lost response reports removed:false for a
	// bridge the first attempt did remove, so the destroy summary omits the
	// bridge line rather than inventing one.
	// net.sync replaces the helper's whole reservation set, so a retry after a
	// lost response converges on the same state.
	case helperInfoOp, "ping", "net.sync", "net.detach", "net.teardown", "dns.install", "dns.uninstall", "dns.syncDomains", "dns.register", "dns.unregister", "forwarding.enable", "bgp.enable", "bgp.disable":
		return true
	default:
		return false
	}
}

func earlyHelperResponse(connection *net.UnixConn, writeErr error) error {
	if !errors.Is(writeErr, unix.EPIPE) && !errors.Is(writeErr, unix.ECONNRESET) {
		return nil
	}
	_, _, err := receiveResponse(connection, false)
	if err == nil {
		return nil
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return nil
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func receiveResponse(connection *net.UnixConn, wantFD bool) (Response, int, error) {
	return receiveDecodedResponse(connection, wantFD, true)
}

func receiveDecodedResponse(connection *net.UnixConn, wantFD, failOnResponseError bool) (Response, int, error) {
	const maxResponseSize = 1 << 20
	var payload bytes.Buffer
	rights := make([]int, 0, 1)
	closeRights := func() {
		for _, fd := range rights {
			_ = unix.Close(fd)
		}
	}

	buffer := make([]byte, 4096)
	oob := make([]byte, unix.CmsgSpace(64*4))
	for !bytes.Contains(payload.Bytes(), []byte{'\n'}) {
		n, oobn, flags, _, err := connection.ReadMsgUnix(buffer, oob)
		if err != nil {
			closeRights()
			return Response{}, -1, fmt.Errorf("read helper response: %w", err)
		}
		if payload.Len()+n > maxResponseSize {
			closeRights()
			return Response{}, -1, errors.New("helper response is too large")
		}
		_, _ = payload.Write(buffer[:n])
		if oobn > 0 {
			messages, parseErr := unix.ParseSocketControlMessage(oob[:oobn])
			if parseErr != nil {
				closeRights()
				return Response{}, -1, fmt.Errorf("parse helper control message: %w", parseErr)
			}
			for _, message := range messages {
				fds, rightsErr := unix.ParseUnixRights(&message)
				if rightsErr != nil {
					closeRights()
					return Response{}, -1, fmt.Errorf("parse helper file descriptors: %w", rightsErr)
				}
				rights = append(rights, fds...)
			}
		}
		if flags&unix.MSG_CTRUNC != 0 {
			closeRights()
			return Response{}, -1, errors.New("helper response control data was truncated")
		}
		if n == 0 && oobn == 0 {
			closeRights()
			return Response{}, -1, io.ErrUnexpectedEOF
		}
	}

	line, _, _ := bytes.Cut(payload.Bytes(), []byte{'\n'})
	var response Response
	if err := json.Unmarshal(line, &response); err != nil {
		closeRights()
		return Response{}, -1, fmt.Errorf("decode helper response: %w", err)
	}
	if !response.OK && failOnResponseError {
		closeRights()
		if response.Error == "" {
			return response, -1, errors.New("helper operation failed")
		}
		return response, -1, errors.New(response.Error)
	}
	if wantFD {
		if len(rights) != 1 {
			closeRights()
			return response, -1, fmt.Errorf("helper returned %d file descriptors, want 1", len(rights))
		}
		return response, rights[0], nil
	}
	if len(rights) != 0 {
		closeRights()
		return response, -1, fmt.Errorf("helper unexpectedly returned %d file descriptors", len(rights))
	}
	return response, -1, nil
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
