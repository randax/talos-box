package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/daemon"
)

func TestPrintStatusRendersRebootFactInTableAndQuietModes(t *testing.T) {
	rebootedAt := time.Date(2026, 8, 29, 6, 49, 28, 0, time.UTC)
	statuses := []daemon.ClusterStatus{{
		Name: "demo", Running: true,
		Nodes: []daemon.NodeStatus{{Name: "demo-cp-1", Role: cluster.RoleControlPlane, Phase: daemon.PhaseRebooted, RebootedAt: &rebootedAt}},
	}}
	for _, quiet := range []bool{false, true} {
		var output bytes.Buffer
		if err := printStatus(&output, statuses, quiet); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"rebooted",
			"cluster demo: node demo-cp-1 rebooted at 2026-08-29T06:49:28Z; Talos boot identity changed while the VM process stayed running",
		} {
			if !strings.Contains(output.String(), want) {
				t.Fatalf("quiet=%v output missing %q:\n%s", quiet, want, output.String())
			}
		}
	}
}
