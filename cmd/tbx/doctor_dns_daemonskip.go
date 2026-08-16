//go:build !linux

package main

import "github.com/randax/talos-box/internal/daemon"

// directDNSDaemonSkip probes the resolver embedded in tbxd; when the
// on-demand daemon is simply not running, that is the same condition the
// daemon-dependent checks SKIP on, not a machine fault worth a FAIL.
// Linux reaches the same verdict through checkLinuxDirectDNS instead.
func directDNSDaemonSkip(probe func() error, listClusters func() ([]daemon.ClusterSummary, error)) error {
	err := probe()
	if err == nil {
		return nil
	}
	if _, daemonErr := listClusters(); isDaemonUnavailable(daemonErr) {
		return skippedDoctorCheck{detail: daemonUnavailableDetail(daemonErr)}
	}
	return err
}
