package talosversion

import (
	"os"
	"strings"
	"testing"
)

// The KVM e2e harness pins one talosctl checksum per bootable Talos version and
// the release-gating lane picks the floor. Both are literals in shell and YAML, so
// nothing but this test keeps them from drifting when the window moves here.
func TestCILanesCoverTheSupportedVersionWindow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		path    string
		needles []string
	}{
		{
			name: "harness",
			path: "../../scripts/ci/linux-kvm-e2e.sh",
			needles: []string{
				"${TBX_E2E_TALOS_VERSION:-" + Default + "}",
				"  " + Default + ")",
				"  " + Min + ")",
			},
		},
		{
			name:    "workflow",
			path:    "../../.github/workflows/floor-e2e.yml",
			needles: []string{"TBX_E2E_TALOS_VERSION: " + Min},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile(test.path)
			if err != nil {
				t.Fatalf("read %s: %v", test.path, err)
			}
			for _, needle := range test.needles {
				if !strings.Contains(string(data), needle) {
					t.Errorf("%s does not pin %q, want it present", test.path, needle)
				}
			}
		})
	}
}
