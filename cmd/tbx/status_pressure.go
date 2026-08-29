package main

import (
	"fmt"
	"io"

	"github.com/randax/talos-box/internal/daemon"
	"github.com/randax/talos-box/internal/hostpressure"
)

var statusMemorySnapshot = hostpressure.SystemMemorySnapshot

func runningStatusExists(statuses []daemon.ClusterStatus) bool {
	for _, status := range statuses {
		if status.Running {
			return true
		}
	}
	return false
}

func printStatusPressureNotice(output io.Writer, snapshot hostpressure.Snapshot, probeErr error) error {
	if probeErr != nil {
		return nil
	}
	finding, ok := hostpressure.SteadySwapFinding(snapshot)
	if !ok {
		return nil
	}
	_, err := fmt.Fprintf(output, "warning: %s; %s\n", finding.Detail, finding.Remedy)
	return err
}
