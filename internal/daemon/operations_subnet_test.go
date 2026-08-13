package daemon

import (
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hostpressure"
	"github.com/randax/talos-box/internal/hypervisor"
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
		vms:          make(map[string]map[string]hypervisor.Machine),
		hostPressure: noHostPressure,
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

func TestStartClusterRefusesExtremeHostPressure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	item, err := cluster.New("pressure", 0, 0, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	service := &Server{
		vms: make(map[string]map[string]hypervisor.Machine),
		hostPressure: func(string) (hostpressure.Snapshot, error) {
			return hostpressure.Snapshot{
				Swap: hostpressure.Usage{TotalBytes: 10 << 30, AvailableBytes: 1 << 30},
			}, nil
		},
		subnetSources: cluster.SubnetSources{
			Interfaces: func() ([]cluster.HostInterface, error) { return nil, nil },
			Route:      func(net.IP) (cluster.HostRoute, error) { return cluster.HostRoute{}, nil },
		},
	}
	raw, err := json.Marshal(startArgs{Name: item.Name})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.startCluster(raw)
	if err == nil {
		t.Fatal("startCluster() succeeded under extreme host pressure without force")
	}
	for _, fragment := range []string{"host swap is 90% used", "use --force to override"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("startCluster() error = %q, missing %q", err, fragment)
		}
	}
}

func TestStartClusterForceSurfacesExtremeHostPressureWarning(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	item, err := cluster.New("pressure-forced", 0, 0, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	service := &Server{
		vms: make(map[string]map[string]hypervisor.Machine),
		hostPressure: func(string) (hostpressure.Snapshot, error) {
			return hostpressure.Snapshot{
				DataVolume: hostpressure.Usage{TotalBytes: 100 << 30, AvailableBytes: 5 << 30},
			}, nil
		},
		subnetSources: cluster.SubnetSources{
			Interfaces: func() ([]cluster.HostInterface, error) { return nil, nil },
			Route:      func(net.IP) (cluster.HostRoute, error) { return cluster.HostRoute{}, nil },
		},
	}
	raw, err := json.Marshal(startArgs{Name: item.Name, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.startCluster(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"data volume is 95% used", "(forced)"} {
		if !strings.Contains(result.Warning, fragment) {
			t.Errorf("ClusterSummary.Warning = %q, missing %q", result.Warning, fragment)
		}
	}
}

func TestStartClusterSurfacesHostPressureProbeFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	item, err := cluster.New("pressure-probe-failure", 0, 0, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	service := &Server{
		vms: make(map[string]map[string]hypervisor.Machine),
		hostPressure: func(string) (hostpressure.Snapshot, error) {
			return hostpressure.Snapshot{}, errors.New("sysctl unavailable")
		},
		subnetSources: cluster.SubnetSources{
			Interfaces: func() ([]cluster.HostInterface, error) { return nil, nil },
			Route:      func(net.IP) (cluster.HostRoute, error) { return cluster.HostRoute{}, nil },
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
	if !strings.Contains(result.Warning, "host-pressure probe failed: sysctl unavailable") {
		t.Fatalf("ClusterSummary.Warning = %q, want probe failure", result.Warning)
	}
}

func TestStartClusterSoftWarnsForLonghornMemoryPressure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	item, err := cluster.New("longhorn-memory-warning", 0, 0, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: cluster.CSILonghorn}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	service := &Server{
		vms:            make(map[string]map[string]hypervisor.Machine),
		hostPressure:   noHostPressure,
		hostFreeMemory: func() (int, error) { return 1024, nil },
		subnetSources: cluster.SubnetSources{
			Interfaces: func() ([]cluster.HostInterface, error) { return nil, nil },
			Route:      func(net.IP) (cluster.HostRoute, error) { return cluster.HostRoute{}, nil },
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
	if !strings.Contains(result.Warning, "Longhorn") || !strings.Contains(result.Warning, "memory-tight") {
		t.Fatalf("ClusterSummary.Warning = %q, want soft Longhorn memory warning", result.Warning)
	}
}

func TestStartClusterWarnsForLonghornCustomSchematic(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	item, err := cluster.New("longhorn-custom-schematic", 0, 0, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: cluster.CSILonghorn}
	item.Schematic = "custom-schematic"
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	service := &Server{
		vms:              make(map[string]map[string]hypervisor.Machine),
		defaultSchematic: "generated-default-schematic",
		hostPressure:     noHostPressure,
		hostFreeMemory:   func() (int, error) { return 8192, nil },
		subnetSources: cluster.SubnetSources{
			Interfaces: func() ([]cluster.HostInterface, error) { return nil, nil },
			Route:      func(net.IP) (cluster.HostRoute, error) { return cluster.HostRoute{}, nil },
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
	if !strings.Contains(result.Warning, "iscsi-tools") || !strings.Contains(result.Warning, "util-linux-tools") {
		t.Fatalf("ClusterSummary.Warning = %q, want Longhorn custom schematic warning", result.Warning)
	}
}

func TestStartClusterSkipsLonghornCustomSchematicWarningForGeneratedDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	item, err := cluster.New("longhorn-generated-schematic", 0, 0, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: cluster.CSILonghorn}
	item.Schematic = "generated-default-schematic"
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	service := &Server{
		vms:              make(map[string]map[string]hypervisor.Machine),
		defaultSchematic: item.Schematic,
		hostPressure:     noHostPressure,
		hostFreeMemory:   func() (int, error) { return 8192, nil },
		subnetSources: cluster.SubnetSources{
			Interfaces: func() ([]cluster.HostInterface, error) { return nil, nil },
			Route:      func(net.IP) (cluster.HostRoute, error) { return cluster.HostRoute{}, nil },
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
	if strings.Contains(result.Warning, "iscsi-tools") || strings.Contains(result.Warning, "util-linux-tools") {
		t.Fatalf("ClusterSummary.Warning = %q, did not want Longhorn custom schematic warning", result.Warning)
	}
}

func TestStartClusterDoesNotWarnThatCuratedCSIIsUnsupportedForCilium(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	item, err := cluster.New("cilium-csi-warning", 0, 0, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNICilium, CSI: cluster.CSILonghorn}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	service := &Server{
		vms:            make(map[string]map[string]hypervisor.Machine),
		hostPressure:   noHostPressure,
		hostFreeMemory: func() (int, error) { return 8192, nil },
		subnetSources: cluster.SubnetSources{
			Interfaces: func() ([]cluster.HostInterface, error) { return nil, nil },
			Route:      func(net.IP) (cluster.HostRoute, error) { return cluster.HostRoute{}, nil },
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
	if strings.Contains(result.Warning, "will not install storage") || strings.Contains(result.Warning, "not implemented") {
		t.Fatalf("ClusterSummary.Warning = %q, curated CSI now runs on the generalized CNI path", result.Warning)
	}
}
