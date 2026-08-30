package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hypervisor"
)

// A narrated status names every node it is about to probe, so a client's
// silence deadline is re-armed per probe rather than covering the whole walk.
func TestDispatchStatusNarratesEachNodeProbe(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item := cluster.Cluster{Name: "demo", SubnetIndex: 1, Nodes: []cluster.Node{
		{Name: "cp-1", Role: "controlplane", MAC: "02:00:00:00:01:01"},
		{Name: "w-1", Role: "worker", MAC: "02:00:00:00:01:02"},
	}}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	service := &Server{
		hypervisors:  singleFakeRegistry(&fakeHypervisor{}),
		vms:          map[string]map[string]hypervisor.Machine{},
		nodeIPLookup: func(string, int) string { return "" },
		nodeProbe:    func(string) ProbeResult { return ProbeResult{} },
	}
	var stages []string
	response := service.dispatchStatus(Request{Op: "status", Args: json.RawMessage(`{"cluster":"demo"}`)}, func(stage string) {
		stages = append(stages, stage)
	})
	if !response.OK {
		t.Fatalf("status failed: %s", response.Error)
	}
	joined := strings.Join(stages, "\n")
	for _, want := range []string{"probing node demo/cp-1", "probing node demo/w-1"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("stages %q missing %q", stages, want)
		}
	}
	if unnarrated := service.dispatchStatus(Request{Op: "status", Args: json.RawMessage(`{"cluster":"demo"}`)}, nil); !unnarrated.OK {
		t.Fatalf("unnarrated status failed: %s", unnarrated.Error)
	}
}
