package main

import (
	"fmt"
	"strings"

	"github.com/randax/talos-box/internal/cluster"
)

// resolvedManualSteps repeats, verbatim, the manual step the helper hands the
// daemon when systemd-resolved registration fails (see
// internal/helper.applyResolvedRegistration), so doctor and tbxd.log tell an
// operator to run the same commands instead of two different ones (#447).
func resolvedManualSteps(clusters []cluster.Cluster) []string {
	steps := make([]string, 0, len(clusters))
	for _, item := range clusters {
		bridge := cluster.LinuxBridgeName(item.SubnetIndex)
		steps = append(steps, fmt.Sprintf(
			"sudo resolvectl dns %s %s && sudo resolvectl domain %s %q",
			bridge, cluster.Gateway(item.SubnetIndex), bridge, "~"+item.EffectiveDomain(),
		))
	}
	return steps
}

// resolvedDigFallbacks names the lookup that keeps working without
// systemd-resolved: the daemon's own resolver answers on the cluster gateway.
// docs/qa/smoke-linux.md sends the operator to "the doctor-printed fallback",
// so doctor has to print it.
func resolvedDigFallbacks(clusters []cluster.Cluster) []string {
	fallbacks := make([]string, 0, len(clusters))
	for _, item := range clusters {
		fallbacks = append(fallbacks, fmt.Sprintf(
			"dig @%s <node>.%s", cluster.Gateway(item.SubnetIndex), item.EffectiveDomain(),
		))
	}
	return fallbacks
}

// hostDNSUnavailableDetail phrases the one condition doctor must never report
// as PASS: cluster names do not resolve on this host because systemd-resolved
// is unavailable. Guests and by-IP access are unaffected, so the finding is a
// WARN carrying the repair, not a FAIL (#447).
func hostDNSUnavailableDetail(cause error, clusters []cluster.Cluster) string {
	if len(clusters) == 0 {
		return fmt.Sprintf(
			"systemd-resolved is unavailable (%v); host cluster names require resolved, while guests and by-IP access remain available",
			cause,
		)
	}
	return fmt.Sprintf(
		"systemd-resolved is unavailable (%v); cluster names do not resolve on this host, while guests and by-IP access remain available; manual step: %s; fallback: %s",
		cause,
		strings.Join(resolvedManualSteps(clusters), "; "),
		strings.Join(resolvedDigFallbacks(clusters), "; "),
	)
}
