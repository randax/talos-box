//go:build darwin

package main

import (
	"errors"
	"strings"
	"testing"

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
