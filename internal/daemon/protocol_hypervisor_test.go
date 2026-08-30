package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/hypervisor"
)

func TestInfoHypervisorFeatureStatusJSONRoundTrip(t *testing.T) {
	t.Parallel()

	want := Info{
		CompiledDefaultHypervisor: hypervisor.NameVZ,
		Hypervisors: []HypervisorInfo{{
			Name:                    "qemu",
			Available:               true,
			AvailabilityRemediation: "install the hypervisor",
			BalloonReadback:         FeatureStatusInfo{Supported: true},
			Suspend:                 &FeatureStatusInfo{Reason: "save unavailable"},
			GuestAgent:              FeatureStatusInfo{Reason: "no channel"},
		}},
	}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(raw)
	for _, fragment := range []string{
		`"balloonReadback":{"supported":true}`,
		`"suspend":{"reason":"save unavailable"}`,
		`"guestAgent":{"reason":"no channel"}`,
		`"availabilityRemediation":"install the hypervisor"`,
		`"compiledDefaultHypervisor":"vz"`,
	} {
		if !strings.Contains(encoded, fragment) {
			t.Fatalf("JSON = %s, want camelCase fragment %s", encoded, fragment)
		}
	}

	var got Info
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Hypervisors) != 1 || !got.Hypervisors[0].BalloonReadback.Supported || got.Hypervisors[0].Suspend.Reason != "save unavailable" || got.Hypervisors[0].GuestAgent.Reason != "no channel" || got.Hypervisors[0].AvailabilityRemediation != "install the hypervisor" || got.CompiledDefaultHypervisor != hypervisor.NameVZ {
		t.Fatalf("round trip = %+v, want feature statuses preserved", got)
	}

	var old Info
	if err := json.Unmarshal([]byte(`{"protocolVersion":20}`), &old); err != nil {
		t.Fatal(err)
	}
	if len(old.Hypervisors) != 0 || old.DefaultHypervisor != "" || old.DefaultHypervisorSource != "" || old.CompiledDefaultHypervisor != "" {
		t.Fatalf("old daemon info = %+v, want empty hypervisor inventory", old)
	}
}

func TestClusterStatusHypervisorJSONCompatibility(t *testing.T) {
	t.Parallel()
	want := ClusterStatus{Name: "demo", Hypervisor: hypervisor.NameQEMU}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"hypervisor":"qemu"`) {
		t.Fatalf("JSON = %s, want hypervisor field", raw)
	}
	var got ClusterStatus
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Hypervisor != hypervisor.NameQEMU {
		t.Fatalf("round-trip hypervisor = %q, want %q", got.Hypervisor, hypervisor.NameQEMU)
	}

	var old ClusterStatus
	if err := json.Unmarshal([]byte(`{"name":"legacy"}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.Hypervisor != "" {
		t.Fatalf("old status hypervisor = %q, want empty", old.Hypervisor)
	}
	empty, err := json.Marshal(ClusterStatus{Name: "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(empty), "hypervisor") {
		t.Fatalf("empty status JSON includes hypervisor: %s", empty)
	}
	emptyInfo, err := json.Marshal(HypervisorInfo{Name: hypervisor.NameQEMU})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(emptyInfo), "availabilityRemediation") {
		t.Fatalf("empty hypervisor info includes remediation: %s", emptyInfo)
	}
}
