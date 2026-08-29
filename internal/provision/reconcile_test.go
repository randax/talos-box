package provision

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/randax/talos-box/internal/cluster"
)

func TestReclaimMinFreeKiBScalesAndClamps(t *testing.T) {
	tests := []struct {
		memoryMiB int
		wantKiB   int
	}{{512, 16384}, {1024, 32768}, {2048, 65536}, {4096, 131072}, {8192, 262144}, {16384, 262144}}
	for _, tt := range tests {
		if got := reclaimMinFreeKiB(tt.memoryMiB); got != tt.wantKiB {
			t.Errorf("reclaimMinFreeKiB(%d) = %d, want %d", tt.memoryMiB, got, tt.wantKiB)
		}
	}
}

func generatedMachineSettings(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	machine, ok := document["machine"].(map[string]any)
	if !ok {
		t.Fatalf("generated config has no machine mapping: %s", data)
	}
	return machine
}

func TestGeneratedConfigAppliesReclaimProtectionByRoleMemory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	controlPlane := cluster.NodeDefaults{MemoryMiB: 512}
	worker := cluster.NodeDefaults{MemoryMiB: 8192}
	item := cluster.Cluster{
		Name: "reclaim-by-role", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true},
		NodeDefaults: cluster.NodeDefaults{MemoryMiB: 2048}, ControlPlaneDefaults: &controlPlane, WorkerDefaults: &worker,
		Nodes: []cluster.Node{{Name: "cp-1", Role: cluster.RoleControlPlane, IP: "172.30.0.2"}, {Name: "worker-1", Role: cluster.RoleWorker, IP: "172.30.0.3"}},
	}
	result, err := generateMachineConfigs(item)
	if err != nil {
		t.Fatal(err)
	}
	for role, want := range map[cluster.Role]string{cluster.RoleControlPlane: "16384", cluster.RoleWorker: "262144"} {
		sysctls := generatedMachineSettings(t, result.configs[role])["sysctls"].(map[string]any)
		if sysctls["vm.min_free_kbytes"] != want || sysctls["vm.watermark_scale_factor"] != "200" || sysctls["vm.vfs_cache_pressure"] != "50" {
			t.Fatalf("%s sysctls = %+v, want scaled reclaim defaults", role, sysctls)
		}
	}
}

func TestGeneratedConfigAddsKubeletMemoryProtectionByDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item := cluster.Cluster{
		Name: "kubelet-protection", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true},
		NodeDefaults: cluster.NodeDefaults{MemoryMiB: 2048}, Nodes: []cluster.Node{{Name: "cp-1", Role: cluster.RoleControlPlane, IP: "172.30.0.2"}},
	}
	result, err := generateMachineConfigs(item)
	if err != nil {
		t.Fatal(err)
	}
	extra := generatedMachineSettings(t, result.configs[cluster.RoleControlPlane])["kubelet"].(map[string]any)["extraConfig"].(map[string]any)
	if got := extra["evictionHard"].(map[string]any)["memory.available"]; got != "300Mi" {
		t.Fatalf("evictionHard.memory.available = %v, want 300Mi", got)
	}
	if got := extra["systemReserved"].(map[string]any)["memory"]; got != "512Mi" {
		t.Fatalf("systemReserved.memory = %v, want 512Mi", got)
	}
}

func TestGeneratedConfigAllowsKubeletMemoryProtectionOptOut(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item := cluster.Cluster{
		Name:               "kubelet-opt-out",
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true, DisableKubeletMemoryProtection: true},
		NodeDefaults:       cluster.NodeDefaults{MemoryMiB: 2048}, Nodes: []cluster.Node{{Name: "cp-1", Role: cluster.RoleControlPlane, IP: "172.30.0.2"}},
	}
	result, err := generateMachineConfigs(item)
	if err != nil {
		t.Fatal(err)
	}
	machine := generatedMachineSettings(t, result.configs[cluster.RoleControlPlane])
	if _, ok := machine["sysctls"].(map[string]any)["vm.min_free_kbytes"]; !ok {
		t.Fatal("reclaim sysctls were removed with kubelet opt-out")
	}
	kubelet := machine["kubelet"].(map[string]any)
	if extra, ok := kubelet["extraConfig"].(map[string]any); ok {
		if eviction, ok := extra["evictionHard"].(map[string]any); ok {
			if _, found := eviction["memory.available"]; found {
				t.Fatalf("opted-out config has eviction threshold: %+v", extra)
			}
		}
		if reserved, ok := extra["systemReserved"].(map[string]any); ok {
			if _, found := reserved["memory"]; found {
				t.Fatalf("opted-out config has memory reservation: %+v", extra)
			}
		}
	}
}

func TestGeneratedConfigSkipsKubeletMemoryProtectionOnSmallNodes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item := cluster.Cluster{
		Name:               "small-node",
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true},
		NodeDefaults:       cluster.NodeDefaults{MemoryMiB: 1024}, Nodes: []cluster.Node{{Name: "cp-1", Role: cluster.RoleControlPlane, IP: "172.30.0.2"}},
	}
	result, err := generateMachineConfigs(item)
	if err != nil {
		t.Fatal(err)
	}
	machine := generatedMachineSettings(t, result.configs[cluster.RoleControlPlane])
	if got := machine["sysctls"].(map[string]any)["vm.min_free_kbytes"]; got != "32768" {
		t.Fatalf("vm.min_free_kbytes = %v, want 32768 on a 1 GiB node", got)
	}
	if extra, ok := machine["kubelet"].(map[string]any)["extraConfig"].(map[string]any); ok {
		if _, found := extra["evictionHard"]; found {
			t.Fatalf("1 GiB node received kubelet eviction defaults: %+v", extra)
		}
		if _, found := extra["systemReserved"]; found {
			t.Fatalf("1 GiB node received kubelet reservation defaults: %+v", extra)
		}
	}
}

func TestReclaimDefaultsDoNotClobberExistingMachineSettings(t *testing.T) {
	input := []byte(`machine:
  sysctls:
    vm.min_free_kbytes: "12345"
    net.ipv4.ip_forward: "1"
  kubelet:
    extraMounts:
      - destination: /var/lib/example
        type: bind
        source: /var/lib/example
    extraConfig:
      evictionHard:
        memory.available: 100Mi
        nodefs.available: 5%
      systemReserved:
        memory: 256Mi
        cpu: 200m
      serializeImagePulls: false
`)
	got, err := withReclaimProtection(input, 4096, true)
	if err != nil {
		t.Fatal(err)
	}
	machine := generatedMachineSettings(t, got)
	sysctls := machine["sysctls"].(map[string]any)
	if sysctls["vm.min_free_kbytes"] != "12345" || sysctls["net.ipv4.ip_forward"] != "1" {
		t.Fatalf("existing sysctls changed: %+v", sysctls)
	}
	extra := machine["kubelet"].(map[string]any)["extraConfig"].(map[string]any)
	wantEviction := map[string]any{"memory.available": "100Mi", "nodefs.available": "5%"}
	if !reflect.DeepEqual(extra["evictionHard"], wantEviction) {
		t.Fatalf("evictionHard = %+v, want %+v", extra["evictionHard"], wantEviction)
	}
	wantReserved := map[string]any{"memory": "256Mi", "cpu": "200m"}
	if !reflect.DeepEqual(extra["systemReserved"], wantReserved) || extra["serializeImagePulls"] != false {
		t.Fatalf("existing kubelet config changed: %+v", extra)
	}
	if _, ok := machine["kubelet"].(map[string]any)["extraMounts"]; !ok {
		t.Fatal("existing kubelet extra mounts were removed")
	}
}

// TestGeneratedConfigOpensUserNamespacesForGVisor pins the curated promise:
// requesting gvisor must relax Talos's user-namespace hardening on every
// node, or runsc fails at sandbox create with a misleading ENOSPC.
func TestGeneratedConfigOpensUserNamespacesForGVisor(t *testing.T) {
	tests := []struct {
		name       string
		extensions []string
		want       bool
	}{
		{name: "gvisor requested", extensions: []string{"gvisor"}, want: true},
		{name: "no extensions", extensions: nil, want: false},
		{name: "other extension", extensions: []string{"nfs-utils"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			item := cluster.Cluster{
				Name:            "gvisor-config",
				TalosExtensions: tt.extensions,
				Nodes: []cluster.Node{
					{Name: "cp-1", Role: cluster.RoleControlPlane, IP: "172.30.0.2"},
					{Name: "worker-1", Role: cluster.RoleWorker, IP: "172.30.0.3"},
				},
			}
			result, err := generateMachineConfigs(item)
			if err != nil {
				t.Fatal(err)
			}
			for _, role := range []cluster.Role{cluster.RoleControlPlane, cluster.RoleWorker} {
				config := string(result.configs[role])
				got := strings.Contains(config, "user.max_user_namespaces")
				if got != tt.want {
					t.Fatalf("%s config contains user.max_user_namespaces = %t, want %t", role, got, tt.want)
				}
				for _, reclaimSysctl := range []string{"vm.min_free_kbytes", "vm.watermark_scale_factor", "vm.vfs_cache_pressure"} {
					if !strings.Contains(config, reclaimSysctl) {
						t.Fatalf("%s config lost %s while applying extension settings", role, reclaimSysctl)
					}
				}
			}
		})
	}
}
