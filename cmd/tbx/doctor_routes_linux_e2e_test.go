//go:build linux && e2e

package main

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
)

func TestLinuxDoctorExitCodeWithRunningClusterRoutes(t *testing.T) {
	clusterName := os.Getenv("TBX_E2E_CLUSTER")
	if clusterName == "" {
		t.Skip("TBX_E2E_CLUSTER is not set; a live e2e daemon and cluster are required")
	}

	client := cli{}
	listClusters := func() ([]daemon.ClusterSummary, error) {
		var all []daemon.ClusterSummary
		if err := client.doctorCall("cluster.list", struct{}{}, &all); err != nil {
			return nil, err
		}
		for _, item := range all {
			if item.Name == clusterName {
				return []daemon.ClusterSummary{item}, nil
			}
		}
		return nil, nil
	}
	getStatus := func() ([]daemon.ClusterStatus, error) {
		var all []daemon.ClusterStatus
		if err := client.doctorCall("status", map[string]string{"cluster": clusterName}, &all); err != nil {
			return nil, err
		}
		for _, status := range all {
			if status.Name != clusterName {
				continue
			}
			for i := range status.Nodes {
				status.Nodes[i].Services = nil
				status.Nodes[i].StalledServices = nil
				status.Nodes[i].RebootedAt = nil
			}
			return []daemon.ClusterStatus{status}, nil
		}
		return nil, nil
	}

	clusters, err := listClusters()
	if err != nil {
		t.Fatalf("list cluster %q: %v", clusterName, err)
	}
	if len(clusters) != 1 || !clusters[0].Running {
		t.Fatalf("cluster %q is not running: %+v", clusterName, clusters)
	}
	statuses, err := getStatus()
	if err != nil {
		t.Fatalf("get cluster %q status: %v", clusterName, err)
	}
	if len(statuses) != 1 || !statuses[0].Running {
		t.Fatalf("cluster %q has no running status: %+v", clusterName, statuses)
	}
	liveNodeIP := false
	for _, node := range statuses[0].Nodes {
		if node.IP != "" && !node.Phase.Stopped() {
			liveNodeIP = true
			break
		}
	}
	if !liveNodeIP {
		t.Fatalf("cluster %q has no live node IP: %+v", clusterName, statuses[0].Nodes)
	}
	pass := func() error { return nil }
	// Only the route lookup runs for real: the system-DNS check shares the
	// command seam, and this test asserts doctor's exit status for #502 alone,
	// not that the runner's resolver answers cluster names.
	routeOnly := func(name string, args ...string) ([]byte, error) {
		if name != "ip" {
			return nil, fmt.Errorf("%s is not exercised by this test", name)
		}
		return execCombinedOutput(name, args...)
	}
	deps := doctorDependencies{
		checkHelper:     pass,
		checkResolver:   pass,
		checkDirectDNS:  pass,
		checkForwarding: pass,
		listClusters:    listClusters,
		getStatus:       getStatus,
		command:         routeOnly,
		doHTTP: func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
		},
	}

	var output strings.Builder
	if err := (cli{out: &output}).runDoctorWithDependencies(nil, deps); err != nil {
		t.Fatalf("doctor returned a failing exit status: %v\n%s", err, output.String())
	}
	if !strings.Contains(output.String(), "PASS routes") {
		t.Fatalf("doctor output missing successful routes check:\n%s", output.String())
	}
}
