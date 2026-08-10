//go:build linux

package helper

import "testing"

func TestParseEffectiveCapabilities(t *testing.T) {
	t.Parallel()

	status := []byte("Name:\ttbx-helper\nCapEff:\t0000000000003400\n")
	got, err := parseEffectiveCapabilities(status)
	if err != nil {
		t.Fatal(err)
	}
	if got != requiredLinuxCapabilityMask {
		t.Fatalf("parseEffectiveCapabilities() = %#x, want %#x", got, requiredLinuxCapabilityMask)
	}
}

func TestCapabilityNames(t *testing.T) {
	t.Parallel()

	got := capabilityNames(requiredLinuxCapabilityMask)
	want := []string{"CAP_NET_BIND_SERVICE", "CAP_NET_ADMIN", "CAP_NET_RAW"}
	if len(got) != len(want) {
		t.Fatalf("capabilityNames() = %v, want %v", got, want)
	}
	for index, name := range want {
		if got[index] != name {
			t.Fatalf("capabilityNames()[%d] = %q, want %q", index, got[index], name)
		}
	}
}
