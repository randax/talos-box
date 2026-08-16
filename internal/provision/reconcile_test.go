package provision

import (
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
)

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
				got := strings.Contains(string(result.configs[role]), "user.max_user_namespaces")
				if got != tt.want {
					t.Fatalf("%s config contains user.max_user_namespaces = %t, want %t", role, got, tt.want)
				}
			}
		})
	}
}
