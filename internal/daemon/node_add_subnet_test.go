package daemon

import (
	"context"
	"encoding/json"
	"net"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/provision"
)

// A running cluster's own bridge occupies its subnet, and the host names that
// bridge as it pleases. Node add must not re-run the create-time collision
// guard against it, or it would be refused for as long as the cluster runs.
func TestNodeAddAcceptsOwnBridgeOccupyingTheSubnet(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 0)
	convergedClusterForFastNoop(t, item.Name)
	service.subnetSources = cluster.SubnetSources{
		Interfaces: func() ([]cluster.HostInterface, error) {
			_, network, err := net.ParseCIDR("172.30.0.1/24")
			if err != nil {
				return nil, err
			}
			address := &net.IPNet{IP: net.ParseIP("172.30.0.1"), Mask: network.Mask}
			return []cluster.HostInterface{{Name: "bridge101", Addrs: []net.Addr{address}}}, nil
		},
		Route: func(net.IP) (cluster.HostRoute, error) {
			_, network, err := net.ParseCIDR("172.30.0.0/24")
			if err != nil {
				return cluster.HostRoute{}, err
			}
			return cluster.HostRoute{Interface: "bridge101", Network: network}, nil
		},
	}
	service.provisionReconcile = func(context.Context, provision.Request) (provision.Result, error) {
		return provision.Result{StoragePhase: provision.StoragePhaseLive, StorageLive: true}, nil
	}

	raw, err := json.Marshal(nodeArgs{Cluster: item.Name, Name: "demo-worker-1", Role: cluster.RoleWorker})
	if err != nil {
		t.Fatal(err)
	}
	response := service.dispatch(Request{Op: "node.add", Args: raw})

	if !response.OK {
		t.Fatalf("node.add failed: %s", response.Error)
	}
}
