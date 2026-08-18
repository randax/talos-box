//go:build darwin

package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/daemon"
)

func TestPlatformDoctorDNSSkipsWhenDaemonUnavailable(t *testing.T) {
	t.Parallel()

	deps := doctorDependencies{
		checkDirectDNS: func() error {
			return errors.New("read udp 127.0.0.1:5399: read: connection refused")
		},
		listClusters: func() ([]daemon.ClusterSummary, error) {
			return nil, dialError{err: errors.New("dial unix ~/.talosbox/tbxd.sock: connect: no such file or directory")}
		},
	}
	platformDoctorDependencies(&deps)

	err := deps.checkDirectDNS()
	var skipped skippedDoctorCheck
	if !errors.As(err, &skipped) {
		t.Fatalf("checkDirectDNS() = %v, want skippedDoctorCheck", err)
	}
	if !strings.Contains(skipped.Error(), "daemon unavailable") {
		t.Fatalf("skip detail = %q, want daemon unavailable reason", skipped.Error())
	}
}

func TestPlatformDoctorDNSStillFailsWhenDaemonIsUp(t *testing.T) {
	t.Parallel()

	probeErr := errors.New("read udp 127.0.0.1:5399: read: connection refused")
	deps := doctorDependencies{
		checkDirectDNS: func() error { return probeErr },
		listClusters: func() ([]daemon.ClusterSummary, error) {
			return nil, nil
		},
	}
	platformDoctorDependencies(&deps)

	err := deps.checkDirectDNS()
	var skipped skippedDoctorCheck
	if errors.As(err, &skipped) {
		t.Fatalf("checkDirectDNS() = %v, want probe failure, not SKIP", err)
	}
	if !errors.Is(err, probeErr) {
		t.Fatalf("checkDirectDNS() = %v, want %v", err, probeErr)
	}
}

func TestPlatformDoctorDNSDoesNotContactDaemonWhenProbeSucceeds(t *testing.T) {
	t.Parallel()

	contacted := false
	deps := doctorDependencies{
		checkDirectDNS: func() error { return nil },
		listClusters: func() ([]daemon.ClusterSummary, error) {
			contacted = true
			return nil, nil
		},
	}
	platformDoctorDependencies(&deps)

	if err := deps.checkDirectDNS(); err != nil {
		t.Fatalf("checkDirectDNS() = %v", err)
	}
	if contacted {
		t.Fatal("checkDirectDNS() contacted the daemon despite a healthy probe")
	}
}

// macOS had no port checks at all, so a stray listener on the BGP port — the
// `nc -l 179` in #345 — was invisible to doctor while Linux caught its own
// (#359). The inventory comes from netstat because an unprivileged lsof cannot
// see a root-owned socket on macOS.
func TestDarwinBGPPortFindingReportsAWildcardSquatter(t *testing.T) {
	t.Parallel()

	var ran []string
	finding := darwinBGPPortFinding(179,
		func() ([]cluster.Cluster, error) { return []cluster.Cluster{{Name: "qa-a", SubnetIndex: 0}}, nil },
		func(name string, args ...string) ([]byte, error) {
			ran = append([]string{name}, args...)
			return []byte("tcp4       0      0  *.179                  *.*                    LISTEN\n"), nil
		},
	)

	if finding.check != "port-179" {
		t.Fatalf("check = %q, want port-179", finding.check)
	}
	if finding.level != "WARN" {
		t.Fatalf("level = %q, want WARN for a squatted port", finding.level)
	}
	if !strings.Contains(finding.detail, "*.179") {
		t.Fatalf("detail = %q, want the listener quoted", finding.detail)
	}
	if !strings.Contains(finding.detail, cluster.Gateway(0)) {
		t.Fatalf("detail = %q, want the speaker's bind named", finding.detail)
	}
	if !strings.Contains(finding.detail, "sudo lsof -nP -iTCP:179 -sTCP:LISTEN") {
		t.Fatalf("detail = %q, want the way to identify the owner", finding.detail)
	}
	if len(ran) == 0 || ran[0] != "netstat" {
		t.Fatalf("probe command = %q, want netstat: lsof cannot see root-owned sockets unprivileged", ran)
	}
}

// The host speaker binds one cluster gateway each, so its own listener — and a
// second cluster's — must not be reported as a conflict.
func TestDarwinBGPPortFindingPassesOnTheSpeakersOwnBinds(t *testing.T) {
	t.Parallel()

	output := "tcp4       0      0  " + cluster.Gateway(0) + ".179         *.*                    LISTEN\n" +
		"tcp4       0      0  " + cluster.Gateway(1) + ".179         *.*                    LISTEN\n"
	finding := darwinBGPPortFinding(179,
		func() ([]cluster.Cluster, error) {
			return []cluster.Cluster{{Name: "qa-a", SubnetIndex: 0}, {Name: "qa-b", SubnetIndex: 1}}, nil
		},
		func(string, ...string) ([]byte, error) { return []byte(output), nil },
	)

	if finding.level != "PASS" {
		t.Fatalf("finding = %+v, want PASS for the speaker's own binds", finding)
	}
}

// With no clusters there is no gateway to bind and nothing to conflict with,
// which is how the Linux check reports the same case.
func TestDarwinBGPPortFindingSkipsWithoutClusters(t *testing.T) {
	t.Parallel()

	finding := darwinBGPPortFinding(179,
		func() ([]cluster.Cluster, error) { return nil, nil },
		func(string, ...string) ([]byte, error) {
			t.Fatal("inspected sockets without a cluster")
			return nil, nil
		},
	)

	if finding.level != "SKIP" {
		t.Fatalf("finding = %+v, want SKIP without clusters", finding)
	}
}

// The port check is registered, not merely available: doctor renders whatever
// platformDoctorDependencies puts on deps.platform.
func TestPlatformDoctorRegistersTheBGPPortCheck(t *testing.T) {
	t.Parallel()

	deps := doctorDependencies{
		listConfig: func() ([]cluster.Cluster, error) { return []cluster.Cluster{{Name: "qa-a"}}, nil },
		command:    func(string, ...string) ([]byte, error) { return nil, nil },
	}
	platformDoctorDependencies(&deps)

	if deps.platform == nil {
		t.Fatal("platformDoctorDependencies() registered no platform findings")
	}
	for _, finding := range deps.platform() {
		if finding.check == "port-179" {
			return
		}
	}
	t.Fatalf("platform findings = %+v, want a port-179 check", deps.platform())
}
