package daemon

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestInfoHypervisorFeatureStatusJSONRoundTrip(t *testing.T) {
	t.Parallel()

	want := Info{Hypervisors: []HypervisorInfo{{
		Name:            "qemu",
		Available:       true,
		BalloonReadback: FeatureStatusInfo{Supported: true},
		GuestAgent:      FeatureStatusInfo{Reason: "no channel"},
	}}}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(raw)
	for _, fragment := range []string{
		`"balloonReadback":{"supported":true}`,
		`"guestAgent":{"reason":"no channel"}`,
	} {
		if !strings.Contains(encoded, fragment) {
			t.Fatalf("JSON = %s, want camelCase fragment %s", encoded, fragment)
		}
	}

	var got Info
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Hypervisors) != 1 || !got.Hypervisors[0].BalloonReadback.Supported || got.Hypervisors[0].GuestAgent.Reason != "no channel" {
		t.Fatalf("round trip = %+v, want feature statuses preserved", got)
	}

	var old Info
	if err := json.Unmarshal([]byte(`{"protocolVersion":20}`), &old); err != nil {
		t.Fatal(err)
	}
	if len(old.Hypervisors) != 0 || old.DefaultHypervisor != "" || old.DefaultHypervisorSource != "" {
		t.Fatalf("old daemon info = %+v, want empty hypervisor inventory", old)
	}
}
