package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
)

// fakeRouteProbe answers with a fixed interface per address so the shared
// route logic can be exercised on every GOOS without execing a host tool.
func fakeRouteProbe(t *testing.T, ifaces map[string]string, asked *[]string) routeProbe {
	t.Helper()
	return routeProbe{
		iface: func(ip string) (string, error) {
			if asked != nil {
				*asked = append(*asked, ip)
			}
			iface, ok := ifaces[ip]
			if !ok {
				return "", errors.New("no route")
			}
			return iface, nil
		},
		clusterIface: func(iface string) bool { return strings.HasPrefix(iface, "tbx-cluster") },
		loopback:     "tbx-lo",
	}
}

func TestCheckClusterRoutesAcceptsTheProbesClusterInterface(t *testing.T) {
	clusters := []daemon.ClusterSummary{{Name: "demo", SubnetIndex: 3, Running: true}}
	statuses := []daemon.ClusterStatus{{
		Name:    "demo",
		Running: true,
		Nodes:   []daemon.NodeStatus{{Name: "demo-cp-1", IP: "172.30.3.2"}},
	}}
	var asked []string
	probe := fakeRouteProbe(t, map[string]string{
		"172.30.3.1": "tbx-cluster0",
		"172.30.3.2": "tbx-cluster0",
	}, &asked)
	if err := checkClusterRoutes(clusters, statuses, probe); err != nil {
		t.Fatalf("checkClusterRoutes() = %v", err)
	}
	if want := []string{"172.30.3.1", "172.30.3.2"}; fmt.Sprint(asked) != fmt.Sprint(want) {
		t.Fatalf("probed %v, want %v", asked, want)
	}
}

func TestCheckClusterRoutesFailsForAForeignInterface(t *testing.T) {
	clusters := []daemon.ClusterSummary{{Name: "demo", SubnetIndex: 3, Running: true}}
	statuses := []daemon.ClusterStatus{{
		Name:    "demo",
		Running: true,
		Nodes:   []daemon.NodeStatus{{Name: "demo-cp-1", IP: "172.30.3.2"}},
	}}
	probe := fakeRouteProbe(t, map[string]string{
		"172.30.3.1": "tbx-cluster0",
		"172.30.3.2": "wlan0",
	}, nil)
	err := checkClusterRoutes(clusters, statuses, probe)
	if err == nil {
		t.Fatal("checkClusterRoutes() succeeded for a captured node route")
	}
	for _, fragment := range []string{"demo", "172.30.3.2", "wlan0", "VPN/ZTNA client has captured the cluster subnet"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("error %q missing %q", err, fragment)
		}
	}
}

func TestCheckClusterRoutesAllowsTheProbesLoopbackForTheGatewayOnly(t *testing.T) {
	clusters := []daemon.ClusterSummary{{Name: "demo", SubnetIndex: 0, Running: true}}
	statuses := []daemon.ClusterStatus{{
		Name:    "demo",
		Running: true,
		Nodes:   []daemon.NodeStatus{{Name: "demo-cp-1", IP: "172.30.0.2"}},
	}}
	gatewayLocal := fakeRouteProbe(t, map[string]string{
		"172.30.0.1": "tbx-lo",
		"172.30.0.2": "tbx-cluster0",
	}, nil)
	if err := checkClusterRoutes(clusters, statuses, gatewayLocal); err != nil {
		t.Fatalf("checkClusterRoutes() = %v; a loopback gateway must be healthy", err)
	}

	nodeLocal := fakeRouteProbe(t, map[string]string{
		"172.30.0.1": "tbx-lo",
		"172.30.0.2": "tbx-lo",
	}, nil)
	if err := checkClusterRoutes(clusters, statuses, nodeLocal); err == nil {
		t.Fatal("checkClusterRoutes() succeeded for a node route via the loopback")
	}
}
