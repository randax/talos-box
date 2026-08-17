package daemon

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hypervisor"
)

// ownBridgeSubnetSources models a host where the cluster's own bridge still
// holds the subnet gateway under a vmnet-assigned name, as it does after
// suspend or an unclean stop. extraInterfaces adds foreign squatters.
func ownBridgeSubnetSources(t *testing.T, extraInterfaces ...cluster.HostInterface) cluster.SubnetSources {
	t.Helper()
	_, network, err := net.ParseCIDR("172.30.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	own := cluster.HostInterface{
		Name:  "bridge101",
		Addrs: []net.Addr{&net.IPNet{IP: net.ParseIP("172.30.0.1"), Mask: network.Mask}},
	}
	interfaces := append([]cluster.HostInterface{own}, extraInterfaces...)
	return cluster.SubnetSources{
		Interfaces: func() ([]cluster.HostInterface, error) { return interfaces, nil },
		Route: func(net.IP) (cluster.HostRoute, error) {
			return cluster.HostRoute{Interface: "bridge101", Network: network}, nil
		},
	}
}

func foreignSubnetInterface(t *testing.T) cluster.HostInterface {
	t.Helper()
	_, network, err := net.ParseCIDR("172.30.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	return cluster.HostInterface{
		Name:  "utun7",
		Addrs: []net.Addr{&net.IPNet{IP: net.ParseIP("172.30.0.5"), Mask: network.Mask}},
	}
}

func savedClusterForSubnetTest(t *testing.T, name string, workers int) cluster.Cluster {
	t.Helper()
	item, err := cluster.New(name, 0, 1, workers, cluster.NodeDefaults{CPUs: 1, MemoryMiB: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	return item
}

// A suspended cluster leaves its own bridge up on its subnet. Start must not
// re-run the create-time collision guard against it, or the cluster is
// unrecoverable except by destroy (#271).
func TestStartClusterAcceptsOwnBridgeOccupyingTheSubnet(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item := savedClusterForSubnetTest(t, "own-bridge-start", 0)

	service := &Server{
		hypervisor:    &fakeHypervisor{},
		vms:           make(map[string]map[string]hypervisor.Machine),
		hostPressure:  noHostPressure,
		subnetSources: ownBridgeSubnetSources(t),
	}
	raw, err := json.Marshal(startArgs{Name: item.Name})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.startCluster(raw)
	if err != nil {
		t.Fatalf("startCluster() error = %v, want the cluster's own bridge to be accepted", err)
	}
	if strings.Contains(result.Warning, "conflicts") {
		t.Fatalf("ClusterSummary.Warning = %q, did not want an own-bridge conflict", result.Warning)
	}
}

// A foreign squatter cannot be resolved by starting a cluster whose subnet is
// already fixed, so it is reported rather than fatal.
func TestStartClusterWarnsOnForeignSubnetConflict(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item := savedClusterForSubnetTest(t, "foreign-start", 0)

	service := &Server{
		hypervisor:    &fakeHypervisor{},
		vms:           make(map[string]map[string]hypervisor.Machine),
		hostPressure:  noHostPressure,
		subnetSources: ownBridgeSubnetSources(t, foreignSubnetInterface(t)),
	}
	raw, err := json.Marshal(startArgs{Name: item.Name})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.startCluster(raw)
	if err != nil {
		t.Fatalf("startCluster() error = %v, want a warning instead", err)
	}
	if !strings.Contains(result.Warning, "utun7") || !strings.Contains(result.Warning, "conflicts") {
		t.Fatalf("ClusterSummary.Warning = %q, want a foreign-conflict warning naming utun7", result.Warning)
	}
}

// Resume runs against the bridge suspend deliberately left up, so the strict
// guard would refuse every suspended cluster (#271).
func TestResumeClusterAcceptsOwnBridgeOccupyingTheSubnet(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item := savedClusterForSubnetTest(t, "own-bridge-resume", 0)
	writeSavedState(t, item)

	service := &Server{
		hypervisor:    &fakeHypervisor{},
		vms:           make(map[string]map[string]hypervisor.Machine),
		subnetSources: ownBridgeSubnetSources(t),
	}
	raw, err := json.Marshal(nameArgs{Name: item.Name})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.resumeCluster(raw)
	if err != nil {
		t.Fatalf("resumeCluster() error = %v, want the cluster's own bridge to be accepted", err)
	}
	if strings.Contains(result.Warning, "conflicts") {
		t.Fatalf("ClusterSummary.Warning = %q, did not want an own-bridge conflict", result.Warning)
	}
}

func TestResumeClusterWarnsOnForeignSubnetConflict(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item := savedClusterForSubnetTest(t, "foreign-resume", 0)
	writeSavedState(t, item)

	service := &Server{
		hypervisor:    &fakeHypervisor{},
		vms:           make(map[string]map[string]hypervisor.Machine),
		subnetSources: ownBridgeSubnetSources(t, foreignSubnetInterface(t)),
	}
	raw, err := json.Marshal(nameArgs{Name: item.Name})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.resumeCluster(raw)
	if err != nil {
		t.Fatalf("resumeCluster() error = %v, want a warning instead", err)
	}
	if !strings.Contains(result.Warning, "utun7") || !strings.Contains(result.Warning, "conflicts") {
		t.Fatalf("ClusterSummary.Warning = %q, want a foreign-conflict warning naming utun7", result.Warning)
	}
}

func writeSavedState(t *testing.T, item cluster.Cluster) {
	t.Helper()
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range item.Nodes {
		path := filepath.Join(dir, node.Name+".vzstate")
		if err := os.WriteFile(path, []byte("saved"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
