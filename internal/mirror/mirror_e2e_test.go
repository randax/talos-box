//go:build e2e

package mirror

import (
	"os/exec"
	"strings"
	"testing"
)

// TestMirrorPullE2E proves a Talos node pulls through the running mirror on
// this machine — where a direct pull is RST by the corporate agent (gate G4).
// Requires: tbxd running with mirrors up, a configured node whose machine
// config points registryMirrors at the gateway (via `tbx manifests <c> talos`).
// The daemon-side wiring is exercised by the manual/live procedure recorded on
// issue #34; this test asserts the mirror endpoint itself serves a real image.
func TestMirrorPullE2E(t *testing.T) {
	// the mirror answers the registry v2 ping locally without upstream auth
	out, err := exec.Command("curl", "-s", "-o", "/dev/null", "-w", "%{http_code}",
		"--max-time", "10", "http://127.0.0.1:5058/v2/").Output()
	if err != nil {
		t.Skipf("mirror not reachable (is tbxd running?): %v", err)
	}
	if strings.TrimSpace(string(out)) != "200" {
		t.Fatalf("registry.k8s.io mirror /v2/ = %s, want 200", out)
	}

	// pull a real manifest through the mirror (server-side token + fetch)
	out, err = exec.Command("curl", "-sL", "--max-time", "30",
		"-H", "Accept: application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json",
		"http://127.0.0.1:5058/v2/pause/manifests/3.10").Output()
	if err != nil {
		t.Fatalf("manifest pull: %v", err)
	}
	if !strings.Contains(string(out), "mediaType") && !strings.Contains(string(out), "manifests") {
		t.Fatalf("manifest through mirror looks wrong: %s", firstN(string(out), 200))
	}
}

func firstN(s string, n int) string {
	if len(s) < n {
		return s
	}
	return s[:n]
}
