package daemon

import (
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/vm"
)

func TestHostSubnetSourcesMergesPartialOverrides(t *testing.T) {
	interfacesCalled := false
	service := &Server{
		subnetSources: cluster.SubnetSources{
			Interfaces: func() ([]cluster.HostInterface, error) {
				interfacesCalled = true
				return nil, nil
			},
		},
	}
	sources := service.hostSubnetSources()
	if sources.Route == nil {
		t.Fatal("hostSubnetSources() left Route nil for a partial override")
	}
	if _, err := sources.Interfaces(); err != nil || !interfacesCalled {
		t.Fatalf("hostSubnetSources() did not keep the injected interface source (err %v)", err)
	}
}

func TestStartClusterAttachesSubnetWarning(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	item, err := cluster.New("vpn-warning", 0, 0, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	_, vpnRoute, err := net.ParseCIDR("172.16.0.0/12")
	if err != nil {
		t.Fatal(err)
	}
	service := &Server{
		vms: make(map[string]map[string]*vm.VM),
		subnetSources: cluster.SubnetSources{
			Interfaces: func() ([]cluster.HostInterface, error) { return nil, nil },
			Route: func(net.IP) (cluster.HostRoute, error) {
				return cluster.HostRoute{Interface: "utun7", Network: vpnRoute}, nil
			},
		},
	}
	raw, err := json.Marshal(startArgs{Name: item.Name})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.startCluster(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Warning, "utun7") || !strings.Contains(result.Warning, "capture cluster traffic") {
		t.Fatalf("ClusterSummary.Warning = %q, want VPN interface and risk", result.Warning)
	}
}
