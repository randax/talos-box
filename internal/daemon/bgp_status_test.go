package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/helper"
)

// A refused enable is a pure validation failure: nothing is started, so nothing
// may be narrated ahead of the refusal (#399).
func TestBGPEnableRefusesWithoutNarratingTheSpeaker(t *testing.T) {
	service, item, client := runningCiliumClusterForBGP(t, false)
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	recordCiliumReconciles(service)
	progress, stages := recordStages()

	raw, err := json.Marshal(nameArgs{Name: item.Name})
	if err != nil {
		t.Fatal(err)
	}
	response := service.dispatchWithProgress(Request{Op: "bgp.enable", Args: raw}, progress)
	if response.OK {
		t.Fatal("bgp.enable succeeded on a flannel cluster")
	}
	if !strings.Contains(response.Error, "bgp requires cni: cilium") {
		t.Fatalf("bgp.enable error = %q, want the CNI precondition", response.Error)
	}
	if len(*stages) != 0 {
		t.Fatalf("refused bgp.enable narrated %q, want nothing", *stages)
	}
	if len(client.enabled) != 0 {
		t.Fatalf("refused bgp.enable started the speaker for %q", client.enabled)
	}
}

// `bgp status` reports the announcement mode as it stands: the recorded intent,
// the host speaker behind it, where it binds and what it announces (#399).
func TestBGPStatusReportsSpeakerStateAndRoutes(t *testing.T) {
	service, item, client := runningCiliumClusterForBGP(t, true)
	client.routes = []helper.BGPRoute{{Prefix: "172.30.0.200/32", Nexthop: "172.30.0.2"}}

	response := dispatchBGPRequest(t, service, "bgp.status", item.Name)
	if !response.OK {
		t.Fatalf("bgp.status failed: %s", response.Error)
	}
	var status BGPStatus
	if err := json.Unmarshal(response.Data, &status); err != nil {
		t.Fatal(err)
	}
	if status.Name != item.Name || !status.BGP || !status.Speaker {
		t.Fatalf("bgp.status = %+v, want bgp intent with a running speaker", status)
	}
	if status.BindAddress != cluster.Gateway(item.SubnetIndex) || status.Port != hostBGPPort {
		t.Fatalf("bgp.status bind = %s:%d, want %s:%d",
			status.BindAddress, status.Port, cluster.Gateway(item.SubnetIndex), hostBGPPort)
	}
	if len(status.Routes) != 1 || status.Routes[0].Prefix != "172.30.0.200/32" {
		t.Fatalf("bgp.status routes = %+v, want the announced VIP", status.Routes)
	}
}

// A stopped speaker is a fact, not an error: the report says so and leaves the
// recorded intent visible beside it.
func TestBGPStatusReportsAStoppedSpeaker(t *testing.T) {
	service, item, client := runningCiliumClusterForBGP(t, false)
	client.active = false

	response := dispatchBGPRequest(t, service, "bgp.status", item.Name)
	if !response.OK {
		t.Fatalf("bgp.status failed: %s", response.Error)
	}
	var status BGPStatus
	if err := json.Unmarshal(response.Data, &status); err != nil {
		t.Fatal(err)
	}
	if status.BGP || status.Speaker || len(status.Routes) != 0 {
		t.Fatalf("bgp.status = %+v, want l2 with no speaker and no routes", status)
	}
}
