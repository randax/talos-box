package helper

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/randax/talos-box/internal/bgp"
)

func TestBGPRejectsInvalidArguments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		op      string
		args    string
		wantErr string
	}{
		{name: "enable requires cluster", op: "bgp.enable", args: `{"subnetIndex":7,"localASN":64512,"peerASN":64600}`, wantErr: "cluster is required"},
		{name: "disable requires cluster", op: "bgp.disable", args: `{}`, wantErr: "cluster is required"},
		{name: "subnet is required", op: "bgp.enable", args: `{"cluster":"demo","localASN":64512,"peerASN":64600}`, wantErr: "subnetIndex is required"},
		{name: "negative subnet", op: "bgp.enable", args: `{"cluster":"demo","subnetIndex":-1,"localASN":64512,"peerASN":64600}`, wantErr: "outside 0..255"},
		{name: "subnet above IPv4 octet", op: "bgp.enable", args: `{"cluster":"demo","subnetIndex":256,"localASN":64512,"peerASN":64600}`, wantErr: "outside 0..255"},
		{name: "local ASN is required", op: "bgp.enable", args: `{"cluster":"demo","subnetIndex":7,"peerASN":64600}`, wantErr: "ASNs must be non-zero"},
		{name: "peer ASN is required", op: "bgp.enable", args: `{"cluster":"demo","subnetIndex":7,"localASN":64512}`, wantErr: "ASNs must be non-zero"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			reply := NewServer(nil, nil).dispatch(Request{Op: test.op, Args: json.RawMessage(test.args)})
			if reply.response.OK || !strings.Contains(reply.response.Error, test.wantErr) {
				t.Fatalf("dispatch(%s) response = %+v, want error containing %q", test.op, reply.response, test.wantErr)
			}
		})
	}
}

func TestHelperInfoReturnsIdentityAcrossProtocolMismatchButOtherOpsRemainGated(t *testing.T) {
	t.Setenv(helperSocketEnv, shortSocketPath(t, "helper-info-mismatch"))

	reply := NewServer(nil, nil).dispatch(Request{
		Op:   helperInfoOp,
		Args: json.RawMessage(`{"protocolVersion":4}`),
	})
	if !reply.response.OK {
		t.Fatalf("helper.info response = %+v, want success", reply.response)
	}
	var info Info
	if err := json.Unmarshal(reply.response.Data, &info); err != nil {
		t.Fatal(err)
	}
	if info.ProtocolVersion != ProtocolVersion {
		t.Fatalf("helper.info protocol = %d, want %d", info.ProtocolVersion, ProtocolVersion)
	}
	if info.Version == "" {
		t.Fatal("helper.info omitted Version")
	}
	if info.Executable == "" {
		t.Fatal("helper.info omitted Executable")
	}
	if !filepath.IsAbs(info.Executable) {
		t.Fatalf("helper.info executable = %q, want absolute path", info.Executable)
	}
	if info.PID != os.Getpid() {
		t.Fatalf("helper.info pid = %d, want %d", info.PID, os.Getpid())
	}

	gated := NewServer(nil, nil).dispatch(Request{
		Op:   "net.attach",
		Args: json.RawMessage(`{"cluster":"demo","subnetIndex":7,"node":"demo-cp-1"}`),
	})
	if gated.response.OK {
		t.Fatalf("net.attach unexpectedly succeeded: %+v", gated.response)
	}
	if !strings.Contains(gated.response.Error, "start vmnet interface") && !strings.Contains(gated.response.Error, "operation not permitted") {
		t.Fatalf("net.attach error = %q, want normal op handling rather than protocol mismatch gating", gated.response.Error)
	}
}

func TestBGPStatusReportsObservedSpeakerOwnership(t *testing.T) {
	server := NewServer(nil, nil)
	announced := []bgp.Route{
		{Prefix: "172.30.7.200/32", Nexthop: "172.30.7.2"},
		{Prefix: "172.30.7.201/32", Nexthop: "172.30.7.3"},
	}
	server.speakers = map[string]bgpSpeaker{
		"demo":   &reportingSpeaker{routes: announced},
		"silent": &shutdownSpeaker{},
	}
	for _, test := range []struct {
		name       string
		args       string
		active     bool
		wantRoutes []BGPRoute
	}{
		{
			name:   "active speaker reports its announced routes",
			args:   `{"cluster":"demo"}`,
			active: true,
			wantRoutes: []BGPRoute{
				{Prefix: "172.30.7.200/32", Nexthop: "172.30.7.2"},
				{Prefix: "172.30.7.201/32", Nexthop: "172.30.7.3"},
			},
		},
		{name: "speaker that cannot report routes", args: `{"cluster":"silent"}`, active: true},
		{name: "no speaker", args: `{"cluster":"missing"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			reply := server.dispatch(Request{Op: "bgp.status", Args: json.RawMessage(test.args)})
			if !reply.response.OK {
				t.Fatalf("bgp.status response = %+v", reply.response)
			}
			var data BGPState
			if err := json.Unmarshal(reply.response.Data, &data); err != nil {
				t.Fatal(err)
			}
			if data.Active != test.active {
				t.Fatalf("bgp.status active = %t, want %t", data.Active, test.active)
			}
			if !reflect.DeepEqual(data.Routes, test.wantRoutes) {
				t.Fatalf("bgp.status routes = %v, want %v", data.Routes, test.wantRoutes)
			}
		})
	}
}

type shutdownSpeaker struct{ stops int }

func (s *shutdownSpeaker) Stop() { s.stops++ }

// reportingSpeaker stands in for the real *bgp.Speaker, which reports the
// routes it has installed in the host FIB.
type reportingSpeaker struct {
	shutdownSpeaker
	routes []bgp.Route
}

func (s *reportingSpeaker) Routes() []bgp.Route { return s.routes }

func TestShutdownStopsEveryBGPSpeaker(t *testing.T) {
	server := NewServer(nil, nil)
	first, second := &shutdownSpeaker{}, &shutdownSpeaker{}
	server.speakers = map[string]bgpSpeaker{"first": first, "second": second}
	if err := server.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if first.stops != 1 || second.stops != 1 || len(server.speakers) != 0 {
		t.Fatalf("BGP shutdown = first:%d second:%d remaining:%d", first.stops, second.stops, len(server.speakers))
	}
}

func TestListenUnixSocketPreservesNonCollisionBindError(t *testing.T) {
	t.Parallel()

	removeCalled := false
	_, err := listenUnixSocket(
		"/restricted/tbx-helper.sock",
		func(string, string) (net.Listener, error) { return nil, os.ErrPermission },
		func(string, string, time.Duration) (net.Conn, error) {
			t.Fatal("dial called for permission error")
			return nil, nil
		},
		func(string) error { removeCalled = true; return nil },
	)
	if !errors.Is(err, os.ErrPermission) || removeCalled {
		t.Fatalf("listenUnixSocket() error = %v, removeCalled = %t; want original permission error only", err, removeCalled)
	}
}

func TestListenUnixSocketRetainsBindErrorWhenStaleRemovalFails(t *testing.T) {
	t.Parallel()

	removeErr := errors.New("remove denied")
	_, err := listenUnixSocket(
		"/run/tbx-helper.sock",
		func(string, string) (net.Listener, error) { return nil, unix.EADDRINUSE },
		func(string, string, time.Duration) (net.Conn, error) { return nil, errors.New("connection refused") },
		func(string) error { return removeErr },
	)
	if !errors.Is(err, unix.EADDRINUSE) || !errors.Is(err, removeErr) {
		t.Fatalf("listenUnixSocket() error = %v, want original bind and cleanup errors", err)
	}
}

func TestIsAuthorizedUID(t *testing.T) {
	t.Parallel()

	allowedUID := uint32(501)
	tests := []struct {
		name       string
		uid        uint32
		allowedUID *uint32
		allowAny   bool
		want       bool
	}{
		{name: "allowed uid", uid: 501, allowedUID: &allowedUID, want: true},
		{name: "root", uid: 0, allowedUID: &allowedUID, want: true},
		{name: "other uid", uid: 502, allowedUID: &allowedUID, want: false},
		{name: "unset allows root", uid: 0, want: true},
		{name: "unset rejects user", uid: 501, want: false},
		{name: "socket-admitted group peer", uid: 501, allowAny: true, want: true},
		{name: "explicit uid remains authoritative for activated socket", uid: 502, allowedUID: &allowedUID, allowAny: true, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := isAuthorizedUID(test.uid, test.allowedUID, test.allowAny); got != test.want {
				t.Fatalf("isAuthorizedUID(%d) = %t, want %t", test.uid, got, test.want)
			}
		})
	}
}

type recordingDHCPManager struct {
	converged [][]int
	released  []int
	subnets   func() []int
}

func (m *recordingDHCPManager) Converge() error {
	m.converged = append(m.converged, m.subnets())
	return nil
}

func (m *recordingDHCPManager) Release(subnetIndex int) error {
	m.released = append(m.released, subnetIndex)
	return nil
}

func (m *recordingDHCPManager) Close() error { return nil }

func TestAttachConvergesDHCPForASubnetTheSyncedStateDoesNotCover(t *testing.T) {
	server := NewServer(nil, nil)
	manager := &recordingDHCPManager{subnets: server.attachedSubnetIndexes}
	server.dhcp = manager

	originalStart := startInterface
	startInterface = func([]int, int, string, string) (*platformAttachment, error) {
		return testPlatformAttachment(41, func(int) error { return nil }), nil
	}
	t.Cleanup(func() { startInterface = originalStart })

	if _, _, _, err := server.attach(0, json.RawMessage(`{"cluster":"demo","subnetIndex":5,"node":"cp-1"}`)); err != nil {
		t.Fatal(err)
	}
	if len(manager.converged) != 1 {
		t.Fatalf("DHCP converges = %d, want 1", len(manager.converged))
	}
	if got := manager.converged[0]; len(got) != 1 || got[0] != 5 {
		t.Fatalf("converged subnets = %v, want [5]", got)
	}
}

func TestSyncReplacesStateAndConvergesDHCP(t *testing.T) {
	state := NewState(t.TempDir())
	server := NewServer(state, nil)
	manager := &recordingDHCPManager{subnets: server.desiredSubnetIndexes}
	server.dhcp = manager
	var convergedHost [][]int
	originalConverge := convergeHostNetworking
	convergeHostNetworking = func(subnets []int) error {
		convergedHost = append(convergedHost, subnets)
		return nil
	}
	t.Cleanup(func() { convergeHostNetworking = originalConverge })

	reply := server.dispatch(Request{Op: "net.sync", Args: json.RawMessage(
		`{"clusters":[{"name":"demo","subnetIndex":7,"nodes":[{"name":"demo-cp-1","mac":"52:54:00:00:07:01","ip":"172.30.7.11"}]}]}`,
	)})
	if !reply.response.OK {
		t.Fatalf("net.sync response = %+v", reply.response)
	}
	if got := state.SubnetIndexes(); len(got) != 1 || got[0] != 7 {
		t.Fatalf("state subnets = %v, want [7]", got)
	}
	if len(manager.converged) != 1 || len(manager.converged[0]) != 1 || manager.converged[0][0] != 7 {
		t.Fatalf("DHCP converges = %v, want one converge over [7]", manager.converged)
	}
	if len(convergedHost) != 1 || len(convergedHost[0]) != 1 || convergedHost[0][0] != 7 {
		t.Fatalf("host converges = %v, want one converge over [7]", convergedHost)
	}
}

func TestSyncRejectsInconsistentReservationsAndKeepsState(t *testing.T) {
	state := NewState(t.TempDir())
	server := NewServer(state, nil)
	manager := &recordingDHCPManager{subnets: server.desiredSubnetIndexes}
	server.dhcp = manager
	if err := state.Replace(0, []SyncedCluster{{
		Name:        "demo",
		SubnetIndex: 7,
		Nodes:       []SyncedNode{{Name: "demo-cp-1", MAC: "52:54:00:00:07:01", IP: "172.30.7.11"}},
	}}); err != nil {
		t.Fatal(err)
	}

	reply := server.dispatch(Request{Op: "net.sync", Args: json.RawMessage(
		`{"clusters":[{"name":"broken","subnetIndex":1,"nodes":[{"name":"a","mac":"not-a-mac","ip":"172.30.1.11"}]}]}`,
	)})
	if reply.response.OK {
		t.Fatal("net.sync accepted an invalid reservation set")
	}
	if got := state.SubnetIndexes(); len(got) != 1 || got[0] != 7 {
		t.Fatalf("state subnets = %v, want the previous [7]", got)
	}
	if len(manager.converged) != 0 {
		t.Fatalf("DHCP converges = %v, want none after a rejected sync", manager.converged)
	}
}

// net.sync carries the peer's uid: a second user's push must not erase the
// first user's reservations, and the converge covers both.
func TestSyncFromTwoPeersConvergesOverBothPartitions(t *testing.T) {
	server := NewServer(NewState(t.TempDir()), nil)
	manager := &recordingDHCPManager{subnets: server.desiredSubnetIndexes}
	server.dhcp = manager
	originalConverge := convergeHostNetworking
	convergeHostNetworking = func([]int) error { return nil }
	t.Cleanup(func() { convergeHostNetworking = originalConverge })

	alice := Request{Op: "net.sync", Args: json.RawMessage(
		`{"clusters":[{"name":"alpha","subnetIndex":2,"nodes":[{"name":"alpha-cp-1","mac":"52:54:00:00:02:01","ip":"172.30.2.11"}]}]}`)}
	bob := Request{Op: "net.sync", Args: json.RawMessage(
		`{"clusters":[{"name":"beta","subnetIndex":9,"nodes":[{"name":"beta-cp-1","mac":"52:54:00:00:09:01","ip":"172.30.9.11"}]}]}`)}
	if reply := server.dispatchFrom(1000, alice); !reply.response.OK {
		t.Fatalf("alice net.sync = %+v", reply.response)
	}
	if reply := server.dispatchFrom(1001, bob); !reply.response.OK {
		t.Fatalf("bob net.sync = %+v", reply.response)
	}
	last := manager.converged[len(manager.converged)-1]
	if len(last) != 2 || last[0] != 2 || last[1] != 9 {
		t.Fatalf("DHCP converge after both synced = %v, want [2 9]", last)
	}
}

// Two users may each run a cluster called "demo": their attachments are
// addressed by peer uid, so one cannot block or release the other's.
func TestAttachmentsFromTwoPeersWithTheSameNamesStayApart(t *testing.T) {
	server := NewServer(nil, nil)
	server.dhcp = &recordingDHCPManager{subnets: server.attachedSubnetIndexes}
	stops := make(map[int]int)
	next := 50
	originalStart := startInterface
	startInterface = func([]int, int, string, string) (*platformAttachment, error) {
		next++
		fd := next
		return testPlatformAttachment(fd, func(int) error { stops[fd]++; return nil }), nil
	}
	t.Cleanup(func() { startInterface = originalStart })

	args := json.RawMessage(`{"cluster":"demo","subnetIndex":2,"node":"demo-cp-1"}`)
	if _, fd, _, err := server.attach(1000, args); err != nil || fd != 51 {
		t.Fatalf("alice attach = fd %d, %v", fd, err)
	}
	if _, fd, _, err := server.attach(1001, args); err != nil || fd != 52 {
		t.Fatalf("bob attach = fd %d, %v; want an independent attachment", fd, err)
	}
	if err := server.detach(1000, json.RawMessage(`{"cluster":"demo","node":"demo-cp-1"}`)); err != nil {
		t.Fatal(err)
	}
	if stops[51] != 1 || stops[52] != 0 {
		t.Fatalf("stops after alice detached = %v, want only her fd 51 closed", stops)
	}
	if _, ok := server.attachments[attachmentKey{owner: 1001, cluster: "demo", node: "demo-cp-1"}]; !ok {
		t.Fatal("bob's attachment vanished with alice's detach")
	}
}

// A push the host cannot converge is rolled back: the owner's previous
// partition is restored and converged again, so helper and daemon both stand
// at the last committed set.
func TestSyncRestoresThePreviousPartitionWhenConvergenceFails(t *testing.T) {
	state := NewState(t.TempDir())
	server := NewServer(state, nil)
	manager := &recordingDHCPManager{subnets: server.desiredSubnetIndexes}
	server.dhcp = manager
	failNext := false
	originalConverge := convergeHostNetworking
	convergeHostNetworking = func([]int) error {
		if failNext {
			failNext = false
			return errors.New("nft: table busy")
		}
		return nil
	}
	t.Cleanup(func() { convergeHostNetworking = originalConverge })

	committed := json.RawMessage(`{"clusters":[{"name":"demo","subnetIndex":3,"nodes":[{"name":"demo-cp-1","mac":"52:54:00:00:03:01","ip":"172.30.3.11"}]}]}`)
	if reply := server.dispatchFrom(1000, Request{Op: "net.sync", Args: committed}); !reply.response.OK {
		t.Fatalf("committed sync = %+v", reply.response)
	}
	proposed := json.RawMessage(`{"clusters":[{"name":"demo","subnetIndex":3,"nodes":[{"name":"demo-cp-1","mac":"52:54:00:00:03:01","ip":"172.30.3.11"},{"name":"demo-worker-1","mac":"52:54:00:00:03:02","ip":"172.30.3.12"}]}]}`)
	failNext = true
	reply := server.dispatchFrom(1000, Request{Op: "net.sync", Args: proposed})
	if reply.response.OK || !strings.Contains(reply.response.Error, "table busy") {
		t.Fatalf("proposed sync = %+v, want the convergence failure", reply.response)
	}
	if got := state.Clusters(); len(got) != 1 || len(got[0].Nodes) != 1 {
		t.Fatalf("clusters after failed sync = %+v, want the committed single-node set", got)
	}
	reloaded := NewState(state.path[:len(state.path)-len("/"+reservationsFileName)])
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Clusters(); len(got) != 1 || len(got[0].Nodes) != 1 {
		t.Fatalf("persisted clusters after failed sync = %+v, want the committed set", got)
	}
	if n := len(manager.converged); n != 2 {
		t.Fatalf("DHCP converges = %d, want 2 (committed sync, then the reconverge after rollback)", n)
	}
}
