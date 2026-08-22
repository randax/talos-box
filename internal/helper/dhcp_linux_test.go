//go:build linux

package helper

import (
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/insomniacslk/dhcp/dhcpv4/server4"
	"github.com/randax/talos-box/internal/cluster"
)

type fakeDHCPListener struct {
	closed int
	done   chan struct{}
}

func newFakeDHCPListener() *fakeDHCPListener {
	return &fakeDHCPListener{done: make(chan struct{})}
}

func (l *fakeDHCPListener) Serve() error {
	<-l.done
	return net.ErrClosed
}

func (l *fakeDHCPListener) Close() error {
	l.closed++
	if l.closed == 1 {
		close(l.done)
		return nil
	}
	return net.ErrClosed
}

func testDHCPManagerOnSubnet(t *testing.T, subnetIndex int, ifindexes map[string]int) (*linuxDHCPManager, *[]*fakeDHCPListener) {
	t.Helper()
	item, err := cluster.New("dhcp", subnetIndex, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	listeners := make([]*fakeDHCPListener, 0, 2)
	manager := &linuxDHCPManager{
		servers: make(map[int]dhcpEntry),
		load:    func() ([]cluster.Cluster, error) { return []cluster.Cluster{item}, nil },
		listen: func(int, server4.Handler) (dhcpListener, error) {
			listener := newFakeDHCPListener()
			listeners = append(listeners, listener)
			return listener, nil
		},
		ifindex: func(name string) (int, error) {
			index, ok := ifindexes[name]
			if !ok {
				return 0, errors.New("no such device")
			}
			return index, nil
		},
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager, &listeners
}

func TestConvergeRebindsDHCPWhenTheBridgeWasRecreated(t *testing.T) {
	ifindexes := map[string]int{bridgeNameForSubnet(0): 11}
	manager, listeners := testDHCPManagerOnSubnet(t, 0, ifindexes)

	if err := manager.Converge(); err != nil {
		t.Fatal(err)
	}
	if len(*listeners) != 1 {
		t.Fatalf("listeners after first converge = %d, want 1", len(*listeners))
	}
	first := (*listeners)[0]

	ifindexes[bridgeNameForSubnet(0)] = 12
	if err := manager.Converge(); err != nil {
		t.Fatal(err)
	}
	if len(*listeners) != 2 {
		t.Fatalf("listeners after recreated bridge = %d, want 2", len(*listeners))
	}
	if first.closed != 1 {
		t.Fatalf("stale listener closes = %d, want 1", first.closed)
	}
	entry, exists := manager.servers[0]
	if !exists || entry.listener != (*listeners)[1] || entry.ifindex != 12 {
		t.Fatalf("entry after rebind = %+v, want the new listener on ifindex 12", entry)
	}
}

func TestConvergeKeepsDHCPWhenTheBridgeIsUnchanged(t *testing.T) {
	manager, listeners := testDHCPManagerOnSubnet(t, 4, map[string]int{bridgeNameForSubnet(4): 9})

	if err := manager.Converge(); err != nil {
		t.Fatal(err)
	}
	if err := manager.Converge(); err != nil {
		t.Fatal(err)
	}
	if len(*listeners) != 1 {
		t.Fatalf("listeners after second converge = %d, want 1", len(*listeners))
	}
	if (*listeners)[0].closed != 0 {
		t.Fatalf("listener closes = %d, want 0", (*listeners)[0].closed)
	}
	if entry := manager.servers[4]; entry.listener != (*listeners)[0] || entry.ifindex != 9 {
		t.Fatalf("entry after no-op converge = %+v, want the original listener on ifindex 9", entry)
	}
}

func TestConvergeRebindsDHCPWhenTheBridgeIsMissing(t *testing.T) {
	ifindexes := map[string]int{bridgeNameForSubnet(2): 7}
	manager, listeners := testDHCPManagerOnSubnet(t, 2, ifindexes)

	if err := manager.Converge(); err != nil {
		t.Fatal(err)
	}
	delete(ifindexes, bridgeNameForSubnet(2))
	if err := manager.Converge(); err != nil {
		t.Fatal(err)
	}
	if len(*listeners) != 2 {
		t.Fatalf("listeners after missing bridge = %d, want 2", len(*listeners))
	}
	if (*listeners)[0].closed != 1 {
		t.Fatalf("stale listener closes = %d, want 1", (*listeners)[0].closed)
	}
	// An unknown ifindex stays stale, so the entry rebinds again once the
	// bridge is back.
	if entry := manager.servers[2]; entry.ifindex != 0 {
		t.Fatalf("entry ifindex after missing bridge = %d, want 0", entry.ifindex)
	}
}

func TestConvergeSurfacesAFailedListenOnARecreatedBridge(t *testing.T) {
	ifindexes := map[string]int{bridgeNameForSubnet(1): 3}
	manager, listeners := testDHCPManagerOnSubnet(t, 1, ifindexes)

	if err := manager.Converge(); err != nil {
		t.Fatal(err)
	}
	manager.listen = func(int, server4.Handler) (dhcpListener, error) {
		return nil, errors.New("no such device")
	}
	ifindexes[bridgeNameForSubnet(1)] = 4

	err := manager.Converge()
	if err == nil {
		t.Fatal("Converge() = nil, want a listen error")
	}
	if want := "listen for DHCP on " + bridgeNameForSubnet(1); !strings.Contains(err.Error(), want) {
		t.Fatalf("Converge() error = %v, want it to mention %q", err, want)
	}
	if (*listeners)[0].closed != 1 {
		t.Fatalf("stale listener closes = %d, want 1", (*listeners)[0].closed)
	}
	if _, exists := manager.servers[1]; exists {
		t.Fatal("failed rebind kept the stale entry")
	}
}

func TestReleaseStopsTheSubnetListenerAndIsIdempotent(t *testing.T) {
	manager, listeners := testDHCPManagerOnSubnet(t, 5, map[string]int{bridgeNameForSubnet(5): 6})

	if err := manager.Converge(); err != nil {
		t.Fatal(err)
	}
	if err := manager.Release(5); err != nil {
		t.Fatal(err)
	}
	if (*listeners)[0].closed != 1 {
		t.Fatalf("listener closes = %d, want 1", (*listeners)[0].closed)
	}
	if _, exists := manager.servers[5]; exists {
		t.Fatal("Release() kept the subnet entry")
	}
	if err := manager.Release(5); err != nil {
		t.Fatalf("second Release() = %v, want nil", err)
	}
}

func TestReleasedSubnetRebindsOnTheNextConverge(t *testing.T) {
	ifindexes := map[string]int{bridgeNameForSubnet(0): 21}
	manager, listeners := testDHCPManagerOnSubnet(t, 0, ifindexes)

	if err := manager.Converge(); err != nil {
		t.Fatal(err)
	}
	if err := manager.Release(0); err != nil {
		t.Fatal(err)
	}
	ifindexes[bridgeNameForSubnet(0)] = 22
	if err := manager.Converge(); err != nil {
		t.Fatal(err)
	}
	if len(*listeners) != 2 {
		t.Fatalf("listeners after release and converge = %d, want 2", len(*listeners))
	}
	if entry := manager.servers[0]; entry.ifindex != 22 {
		t.Fatalf("entry ifindex after rebind = %d, want 22", entry.ifindex)
	}
}
