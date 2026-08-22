package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
)

// The manual step doctor prints has to be the one the daemon logs, character
// for character (internal/helper.applyResolvedRegistration), or an operator
// reading tbxd.log and an operator reading doctor get two different repairs
// (#447).
func TestResolvedManualStepMatchesTheDaemonWording(t *testing.T) {
	t.Parallel()

	steps := resolvedManualSteps([]cluster.Cluster{{Name: "qa-smoke", SubnetIndex: 0}})
	want := `sudo resolvectl dns br-tbx0 172.30.0.1 && sudo resolvectl domain br-tbx0 "~qa-smoke.k8s.test"`
	if len(steps) != 1 || steps[0] != want {
		t.Fatalf("steps = %v, want [%s]", steps, want)
	}
}

func TestResolvedDigFallbackUsesGatewayAndDomain(t *testing.T) {
	t.Parallel()

	fallbacks := resolvedDigFallbacks([]cluster.Cluster{
		{Name: "qa-smoke", SubnetIndex: 0},
		{Name: "other", Domain: "lab.test", SubnetIndex: 3},
	})
	want := []string{"dig @172.30.0.1 <node>.qa-smoke.k8s.test", "dig @172.30.3.1 <node>.lab.test"}
	if len(fallbacks) != len(want) {
		t.Fatalf("fallbacks = %v, want %v", fallbacks, want)
	}
	for index := range want {
		if fallbacks[index] != want[index] {
			t.Fatalf("fallbacks = %v, want %v", fallbacks, want)
		}
	}
}

func TestHostDNSUnavailableDetailCarriesReasonStepAndFallback(t *testing.T) {
	t.Parallel()

	detail := hostDNSUnavailableDetail(
		errors.New("sd_bus_open_system: No such file or directory"),
		[]cluster.Cluster{{Name: "qa-smoke", SubnetIndex: 0}},
	)
	for _, want := range []string{
		"systemd-resolved is unavailable (sd_bus_open_system: No such file or directory)",
		"cluster names do not resolve on this host",
		"guests and by-IP access remain available",
		`manual step: sudo resolvectl dns br-tbx0 172.30.0.1 && sudo resolvectl domain br-tbx0 "~qa-smoke.k8s.test"`,
		"fallback: dig @172.30.0.1 <node>.qa-smoke.k8s.test",
	} {
		if !strings.Contains(detail, want) {
			t.Fatalf("detail = %q, want %q", detail, want)
		}
	}
}

func TestHostDNSUnavailableDetailWithoutClusters(t *testing.T) {
	t.Parallel()

	detail := hostDNSUnavailableDetail(errors.New("boom"), nil)
	if !strings.Contains(detail, "host cluster names require resolved") ||
		strings.Contains(detail, "manual step") {
		t.Fatalf("detail = %q", detail)
	}
}
