//go:build linux

package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/daemon"
)

func TestLinuxSystemDNSUsesResolvedAndGetent(t *testing.T) {
	t.Parallel()

	clusters := []daemon.ClusterSummary{{Name: "demo", SubnetIndex: 7}}
	var calls []string
	command := func(name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		switch name {
		case "resolvectl":
			return []byte("Global\n"), nil
		case "getent":
			return []byte("172.30.7.200 STREAM doctor-probe.demo.k8s.test\n"), nil
		default:
			return nil, fmt.Errorf("unexpected command %s", name)
		}
	}
	if err := checkSystemDNS(clusters, command); err != nil {
		t.Fatal(err)
	}
	want := []string{"resolvectl status", "getent ahostsv4 doctor-probe.demo.k8s.test"}
	if fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestLinuxResolvedAbsenceIsAnActionableWarning(t *testing.T) {
	t.Parallel()

	err := checkSystemDNS(
		[]daemon.ClusterSummary{{Name: "demo", SubnetIndex: 7}},
		func(string, ...string) ([]byte, error) { return nil, errors.New("not found") },
	)
	level, detail := classifySystemDNSFailure(err)
	if level != "WARN" {
		t.Fatalf("level = %q, want WARN (%v)", level, err)
	}
	if !strings.Contains(detail, "guests and by-IP access remain available") {
		t.Fatalf("detail = %q", detail)
	}
}

func TestLinuxResolvedManualStepUsesClusterBridgeAndDomain(t *testing.T) {
	t.Parallel()

	steps := resolvedManualSteps([]cluster.Cluster{{Name: "demo", SubnetIndex: 7}})
	if len(steps) != 1 ||
		!strings.Contains(steps[0], "resolvectl dns br-tbx7 172.30.7.1") ||
		!strings.Contains(steps[0], "resolvectl domain br-tbx7 \"~demo.k8s.test\"") {
		t.Fatalf("manual steps = %v", steps)
	}
}
