//go:build darwin

package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
)

// The whole point of #388: routes and forwarding pass while the data path is
// dead, so doctor must fail on the path itself.
func TestRunDoctorFailsOnADeadInterClusterPath(t *testing.T) {
	deps := passingDoctorDependencies()
	deps.listClusters = func() ([]daemon.ClusterSummary, error) {
		return []daemon.ClusterSummary{
			{Name: "qa-core", SubnetIndex: 0, Running: true},
			{Name: "qa-edge", SubnetIndex: 1, Running: true},
		}, nil
	}
	deps.getStatus = func() ([]daemon.ClusterStatus, error) { return liveClusterStatuses(), nil }
	deps.command = func(name string, args ...string) ([]byte, error) {
		switch name {
		case "/usr/bin/dscacheutil":
			if strings.Contains(args[len(args)-1], ".qa-core.") {
				return []byte("ip_address: 172.30.0.200\n"), nil
			}
			return []byte("ip_address: 172.30.1.200\n"), nil
		case "/sbin/route":
			return []byte("interface: bridge100\n"), nil
		default:
			return nil, nil
		}
	}
	deps.doVIPHTTP = func(request *http.Request) (*http.Response, error) {
		if request.URL.Host == "172.30.1.200" && request.URL.Path == "/dial" {
			return jsonResponse(`{"errors":["dial tcp 172.30.0.200:80: i/o timeout"]}`), nil
		}
		return jsonResponse(`{"responses":["lb-probe-1"]}`), nil
	}

	var output strings.Builder
	err := (cli{out: &output}).runDoctorWithDependencies(nil, deps)
	if err == nil {
		t.Fatal("runDoctorWithDependencies() succeeded while a cross-cluster VIP path was dead")
	}
	if !strings.Contains(output.String(), "PASS routes") {
		t.Fatalf("routes should still pass; output:\n%s", output.String())
	}
	if !strings.Contains(output.String(), "FAIL inter-cluster: qa-edge → qa-core VIP 172.30.0.200") {
		t.Fatalf("output missing the inter-cluster failure:\n%s", output.String())
	}
}
