package main

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/daemon"
	"github.com/randax/talos-box/internal/wsl"
)

// `tbx doctor --help` used to print the bare usage line, leaving the exit-code
// contract — the command's most automation-relevant fact — documented only in
// docs/macos.md (#419).
func TestDoctorHelpDescribesChecksAndExitCodes(t *testing.T) {
	platformNames := linuxPlatformNamesForHelp(wsl.NotWSL)
	for _, flag := range []string{"-h", "--help"} {
		t.Run(flag, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			command := cli{out: &stdout, err: &stderr}
			deps := doctorDependencies{platformCheckNames: func() []string { return platformNames }}
			if err := command.runDoctorWithDependencies([]string{flag}, deps); err != nil {
				t.Fatalf("doctor %s = %v, want nil", flag, err)
			}
			help := stdout.String()
			want := []string{
				"usage: tbx doctor",
				"exits non-zero",
				"runtime identity",
				"current host sysctl, read directly by this client",
				// the output shape a script parses: doctor prints one line per
				// finding, and host-pressure and security-inventory each report
				// several in a single run
				"One line per finding",
				"may report several",
				"WARN",
				"INFO",
				// every portable check name is listed
				"helper", "resolver", "DNS", "forwarding (host)", "host-pressure",
				"system-dns", "routes", "inter-cluster", "guest-agent",
				"talos-services",
				"mirror-health", "mirror-offline", "image-cache", "egress",
				"security-inventory", "runtime-compat", "installations",
			}
			for _, substring := range want {
				if !strings.Contains(help, substring) {
					t.Errorf("doctor %s output missing %q:\n%s", flag, substring, help)
				}
			}
			for _, name := range platformNames {
				if !strings.Contains(help, name) {
					t.Errorf("doctor %s output missing platform check %q:\n%s", flag, name, help)
				}
			}
		})
	}
}

// The help text claims to name every check the command can report, so a check
// added to doctor without a line in the Checks: block breaks that claim
// silently — which is how `inter-cluster` and `mirror-offline` went missing.
// Running doctor with no dependencies makes every check report SKIP, which is
// enough to enumerate the vocabulary without probing the host.
func TestDoctorHelpNamesEveryCheckDoctorReports(t *testing.T) {
	skip := func() error { return skippedDoctorCheck{detail: "probe unavailable"} }
	deps := doctorDependencies{
		checkHelper:     skip,
		checkResolver:   skip,
		checkDirectDNS:  skip,
		checkForwarding: skip,
		listClusters:    func() ([]daemon.ClusterSummary, error) { return nil, nil },
		listConfig:      func() ([]cluster.Cluster, error) { return nil, nil },
		listCache:       func() (daemon.CacheListResult, error) { return daemon.CacheListResult{}, nil },
		// The egress probe reaches the network; this one answers it locally so
		// the enumeration stays hermetic.
		doHTTP:  func(*http.Request) (*http.Response, error) { return nil, errors.New("probe unavailable") },
		command: func(string, ...string) ([]byte, error) { return nil, errors.New("probe unavailable") },
	}
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}
	if err := command.runDoctorWithDependencies(nil, deps); err != nil {
		t.Fatalf("doctor = %v, want nil with every probe unavailable", err)
	}
	help := doctorHelpForPlatform(linuxPlatformNamesForHelp(wsl.NotWSL))
	seen := 0
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if strings.HasPrefix(line, "runtime:") || strings.HasPrefix(line, "  ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		seen++
		check := strings.TrimSuffix(fields[1], ":")
		if !strings.Contains(help, check) {
			t.Errorf("doctor reports check %q that `tbx doctor --help` never names:\n%s", check, help)
		}
	}
	if seen < 12 {
		t.Fatalf("enumerated %d doctor findings, want the whole check vocabulary — the run reported almost nothing:\n%s", seen, stdout.String())
	}

	t.Run("WSL vocabulary", func(t *testing.T) {
		wslDeps := deps
		finding, ok := wslDoctorFinding(completeWSLIdentity())
		if !ok {
			t.Fatal("complete WSL identity produced no finding")
		}
		wslDeps.platform = func() []doctorFinding { return []doctorFinding{finding} }
		var output bytes.Buffer
		if err := (cli{out: &output}).runDoctorWithDependencies(nil, wslDeps); err != nil {
			t.Fatalf("doctor = %v", err)
		}
		help := doctorHelpForPlatform(linuxPlatformNamesForHelp(wsl.WSL2))
		if !strings.Contains(output.String(), "INFO wsl:") || !strings.Contains(help, "wsl                 INFO-only") {
			t.Fatalf("WSL output/help vocabulary disagree:\noutput:\n%s\nhelp:\n%s", output.String(), help)
		}
	})
}

func TestDoctorHelpAddsWSLCheckOnlyForWSLPlatform(t *testing.T) {
	t.Parallel()

	bareNames := linuxPlatformNamesForHelp(wsl.NotWSL)
	wslNames := linuxPlatformNamesForHelp(wsl.WSL2)
	bare := doctorHelpForPlatform(bareNames)
	wslHelp := doctorHelpForPlatform(wslNames)
	description := "wsl                 INFO-only WSL generation/version, distro, Windows build,"
	if strings.Contains(bare, description) || strings.Contains(bare, "On this platform doctor also checks: wsl") {
		t.Fatalf("bare-Linux help advertises WSL:\n%s", bare)
	}
	if strings.Count(wslHelp, description) != 1 || strings.Count(wslHelp, "On this platform doctor also checks: wsl,") != 1 {
		t.Fatalf("WSL help must advertise the check exactly once:\n%s", wslHelp)
	}
}

// Keep conditional-help tests untagged: the production Linux helper is
// build-tagged, but this pure name construction is the cross-platform contract.
func linuxPlatformNamesForHelp(generation wsl.Generation) []string {
	names := []string{
		"kvm", "qemu", "bridge-netfilter", "bridge-stp", "rp-filter",
		"port-53", "port-67", "port-179", "helper-unit", "helper-access", "helper-capabilities",
	}
	if generation == wsl.NotWSL {
		return names
	}
	return append([]string{"wsl"}, names...)
}

// A help flag must never be mistaken for an argument, and a real argument must
// still be refused.
func TestDoctorRejectsArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}
	err := command.runDoctorWithDependencies([]string{"cluster"}, doctorDependencies{})
	if err == nil || !strings.Contains(err.Error(), "usage: tbx doctor") {
		t.Fatalf("doctor cluster = %v, want a usage error", err)
	}
}
