//go:build darwin

package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
)

// These tests pin the macOS probe's command syntax and parser (/sbin/route -n
// get, "interface:" lines, bridge*/vmnet*/lo0). The shared route logic is
// covered platform-neutrally in doctor_routes_test.go with a fake probe.

func TestParseRouteInterface(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		want    string
		wantErr bool
	}{
		{
			name: "bridge route",
			output: `   route to: 172.30.3.1
destination: 172.30.3.1
  interface: bridge100
      flags: <UP,HOST,DONE,LLINFO,CLONING,IFSCOPE,IFREF>
`,
			want: "bridge100",
		},
		{
			name: "VPN route",
			output: `   route to: 172.30.3.2
destination: 172.30.3.0
       mask: 255.255.255.0
  interface: utun6
`,
			want: "utun6",
		},
		{name: "missing interface", output: "route to: 172.30.3.1\n", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseRouteInterface([]byte(tt.output))
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseRouteInterface() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("parseRouteInterface() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCheckClusterRoutesChecksGatewayAndNodeForEveryCluster(t *testing.T) {
	clusters := []daemon.ClusterSummary{
		{Name: "alpha", SubnetIndex: 2, Running: true},
		{Name: "beta", SubnetIndex: 9, Running: true},
	}
	statuses := []daemon.ClusterStatus{
		{Name: "alpha", Running: true, Nodes: []daemon.NodeStatus{{IP: "172.30.2.7"}}},
		{Name: "beta", Running: true, Nodes: []daemon.NodeStatus{{IP: "172.30.9.4"}}},
	}
	var targets []string
	command := func(name string, args ...string) ([]byte, error) {
		if name != "/sbin/route" || len(args) != 3 || args[0] != "-n" || args[1] != "get" {
			t.Fatalf("unexpected route command: %s %v", name, args)
		}
		targets = append(targets, args[2])
		return []byte("interface: vmnet8\n"), nil
	}
	if err := checkClusterRoutes(clusters, statuses, platformRouteProbe(command)); err != nil {
		t.Fatalf("checkClusterRoutes() = %v", err)
	}
	want := []string{"172.30.2.1", "172.30.2.7", "172.30.9.1", "172.30.9.4"}
	if fmt.Sprint(targets) != fmt.Sprint(want) {
		t.Fatalf("route targets = %v, want %v", targets, want)
	}
}

func TestCheckClusterRoutesSkipsStoppedClustersAndNodes(t *testing.T) {
	clusters := []daemon.ClusterSummary{
		{Name: "running", SubnetIndex: 2, Running: true},
		{Name: "stopped", SubnetIndex: 9},
	}
	statuses := []daemon.ClusterStatus{
		{Name: "running", Running: true, Nodes: []daemon.NodeStatus{
			{Name: "cp-1", IP: "172.30.2.7", Phase: daemon.PhaseStopped}, // stale lease
			{Name: "cp-2", IP: "172.30.2.8", Phase: daemon.PhaseConfigured},
		}},
		{Name: "stopped", Nodes: []daemon.NodeStatus{{IP: "172.30.9.4"}}},
	}
	var targets []string
	command := func(_ string, args ...string) ([]byte, error) {
		targets = append(targets, args[len(args)-1])
		return []byte("interface: bridge100\n"), nil
	}
	if err := checkClusterRoutes(clusters, statuses, platformRouteProbe(command)); err != nil {
		t.Fatalf("checkClusterRoutes() = %v", err)
	}
	want := []string{"172.30.2.1", "172.30.2.8"}
	if fmt.Sprint(targets) != fmt.Sprint(want) {
		t.Fatalf("route targets = %v, want %v (stopped cluster and stopped node must be skipped)", targets, want)
	}
}

func TestCheckRoutesAllowsLoopbackGatewayButNotLoopbackNode(t *testing.T) {
	clusters := []daemon.ClusterSummary{{Name: "demo", SubnetIndex: 0, Running: true}}
	statuses := []daemon.ClusterStatus{{
		Name:    "demo",
		Running: true,
		Nodes:   []daemon.NodeStatus{{Name: "demo-cp-1", IP: "172.30.0.2"}},
	}}
	gatewayLocal := func(_ string, args ...string) ([]byte, error) {
		iface := "bridge100"
		if args[len(args)-1] == "172.30.0.1" {
			iface = "lo0"
		}
		return []byte("interface: " + iface + "\n"), nil
	}
	if err := checkClusterRoutes(clusters, statuses, platformRouteProbe(gatewayLocal)); err != nil {
		t.Fatalf("checkClusterRoutes() = %v; lo0 gateway must be healthy", err)
	}

	nodeLocal := func(_ string, _ ...string) ([]byte, error) {
		return []byte("interface: lo0\n"), nil
	}
	if err := checkClusterRoutes(clusters, statuses, platformRouteProbe(nodeLocal)); err == nil {
		t.Fatal("checkClusterRoutes() succeeded for a node route via lo0")
	}
}

func TestCheckRoutesDetectsCapturedSubnet(t *testing.T) {
	clusters := []daemon.ClusterSummary{{Name: "demo", SubnetIndex: 3, Running: true}}
	statuses := []daemon.ClusterStatus{{
		Name:    "demo",
		Running: true,
		Nodes:   []daemon.NodeStatus{{Name: "demo-cp-1", IP: "172.30.3.2"}},
	}}
	command := func(_ string, args ...string) ([]byte, error) {
		iface := "bridge100"
		if args[len(args)-1] == "172.30.3.2" {
			iface = "utun6"
		}
		return []byte("interface: " + iface + "\n"), nil
	}

	err := checkClusterRoutes(clusters, statuses, platformRouteProbe(command))
	if err == nil {
		t.Fatal("checkClusterRoutes() succeeded for VPN-captured node route")
	}
	for _, fragment := range []string{"demo", "172.30.3.2", "utun6", "VPN/ZTNA client has captured the cluster subnet"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("error %q missing %q", err, fragment)
		}
	}
}
