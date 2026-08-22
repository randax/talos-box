package daemon

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
)

// recordingBridgeHelper stands in for the host helper and keeps the calls in
// the order they were made: the release has to withdraw the subnet's resolver
// registration before the link it names disappears, and order is the only way
// to assert that.
type recordingBridgeHelper struct {
	ops         []string
	subnets     []int
	removed     bool
	teardownErr error
	closed      bool
}

func (r *recordingBridgeHelper) UnregisterDNS(subnetIndex int) error {
	r.ops = append(r.ops, "dns.unregister")
	r.subnets = append(r.subnets, subnetIndex)
	return nil
}

func (r *recordingBridgeHelper) TeardownSubnet(subnetIndex int) (bool, error) {
	r.ops = append(r.ops, "net.teardown")
	r.subnets = append(r.subnets, subnetIndex)
	if r.teardownErr != nil {
		return false, r.teardownErr
	}
	return r.removed, nil
}

func (r *recordingBridgeHelper) Close() error {
	r.closed = true
	return nil
}

// forceLinuxBridgeRelease makes the destroy take the Linux bridge-release
// branch from any host, with the recording client in place of the real helper.
// That is what hostBridgeGOOS and connectBridgeHelper exist for: on macOS the
// subnet belongs to vmnet, so the release wiring would otherwise never run
// under test at all.
func forceLinuxBridgeRelease(t *testing.T, client bridgeReleaseHelper) {
	t.Helper()
	previousGOOS, previousConnect := hostBridgeGOOS, connectBridgeHelper
	hostBridgeGOOS = "linux"
	connectBridgeHelper = func() (bridgeReleaseHelper, error) { return client, nil }
	t.Cleanup(func() {
		hostBridgeGOOS = previousGOOS
		connectBridgeHelper = previousConnect
	})
}

func destroyForBridgeRelease(t *testing.T, service *Server, name string) DestroySummary {
	t.Helper()
	raw, err := json.Marshal(destroyArgs{Name: name, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	response := service.dispatch(Request{Op: "cluster.destroy", Args: raw})
	if !response.OK {
		t.Fatalf("cluster.destroy failed: %s", response.Error)
	}
	return decodeDestroySummary(t, response)
}

// A destroy that took the bridge down has to say so by name, or an operator
// cannot tell residue from design (#445).
func TestDestroyReportsTheBridgeItTookDown(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 1)
	client := &recordingBridgeHelper{removed: true}
	forceLinuxBridgeRelease(t, client)

	summary := destroyForBridgeRelease(t, service, item.Name)
	if want := cluster.LinuxBridgeName(item.SubnetIndex); summary.BridgeRemoved != want {
		t.Fatalf("summary bridgeRemoved = %q, want %q", summary.BridgeRemoved, want)
	}
	if summary.BridgeWarning != "" {
		t.Fatalf("summary bridgeWarning = %q, want none", summary.BridgeWarning)
	}
	if got := strings.Join(client.ops, ","); got != "dns.unregister,net.teardown" {
		t.Fatalf("helper calls = %q, want the DNS withdrawal before the teardown", got)
	}
	for _, subnet := range client.subnets {
		if subnet != item.SubnetIndex {
			t.Fatalf("helper call for subnet %d, want %d", subnet, item.SubnetIndex)
		}
	}
	if !client.closed {
		t.Error("helper connection was left open")
	}
}

// A failed teardown reads exactly like a host that had nothing to remove — the
// summary line is simply absent — unless the reason travels with the answer.
func TestDestroyWarnsWhenTheBridgeStaysBehind(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 1)
	client := &recordingBridgeHelper{teardownErr: errors.New("link is busy")}
	forceLinuxBridgeRelease(t, client)

	summary := destroyForBridgeRelease(t, service, item.Name)
	if summary.BridgeRemoved != "" {
		t.Fatalf("summary bridgeRemoved = %q, want none for a failed teardown", summary.BridgeRemoved)
	}
	if !strings.Contains(summary.BridgeWarning, cluster.SubnetCIDR(item.SubnetIndex)) ||
		!strings.Contains(summary.BridgeWarning, "link is busy") {
		t.Fatalf("summary bridgeWarning = %q, want the subnet and the reason", summary.BridgeWarning)
	}
}

// A host that never built the bridge is not a failure: nothing removed, and
// nothing to warn about.
func TestDestroyStaysQuietWhenThereIsNoBridgeToRemove(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 1)
	client := &recordingBridgeHelper{}
	forceLinuxBridgeRelease(t, client)

	summary := destroyForBridgeRelease(t, service, item.Name)
	if summary.BridgeRemoved != "" || summary.BridgeWarning != "" {
		t.Fatalf("summary bridgeRemoved = %q, bridgeWarning = %q, want both empty", summary.BridgeRemoved, summary.BridgeWarning)
	}
}
