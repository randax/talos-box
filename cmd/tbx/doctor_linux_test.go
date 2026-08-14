//go:build linux

package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
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

func TestLinuxResolverChecksPerLinkState(t *testing.T) {
	t.Parallel()

	var calls []string
	err := checkLinuxResolver(
		func() ([]cluster.Cluster, error) { return []cluster.Cluster{{Name: "demo", SubnetIndex: 7}}, nil },
		func(string) ([]byte, error) { return []byte("5\n"), nil },
		func(name string, args ...string) ([]byte, error) {
			calls = append(calls, name+" "+strings.Join(args, " "))
			if name != "resolvectl" || fmt.Sprint(args) != "[status br-tbx7]" {
				t.Fatalf("unexpected command %s %v", name, args)
			}
			return []byte("Link 5 (br-tbx7)\n    DNS Servers: 172.30.8.1\n    DNS Domain: ~other.k8s.test\n"), nil
		},
	)
	level, detail := classifyResolverFailure(err)
	if level != "WARN" {
		t.Fatalf("level = %q, want WARN (%v)", level, err)
	}
	if !strings.Contains(detail, "DNS server 172.30.7.1") || !strings.Contains(detail, "~demo.k8s.test") {
		t.Fatalf("detail = %q", detail)
	}
	if fmt.Sprint(calls) != "[resolvectl status br-tbx7]" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestLinuxResolverSkipsStoppedClusterWithoutBridge(t *testing.T) {
	t.Parallel()

	err := checkLinuxResolver(
		func() ([]cluster.Cluster, error) { return nil, nil },
		func(string) ([]byte, error) {
			t.Fatal("bridge inspected for stopped cluster")
			return nil, nil
		},
		func(string, ...string) ([]byte, error) {
			t.Fatal("resolvectl called for stopped cluster")
			return nil, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestLinuxDirectDNSSkipsStoppedClusterWithoutBridge(t *testing.T) {
	t.Parallel()

	err := checkLinuxDirectDNS(
		func() ([]cluster.Cluster, error) { return nil, nil },
		func(string) ([]byte, error) {
			t.Fatal("bridge inspected for stopped cluster")
			return nil, nil
		},
		func(string) error {
			t.Fatal("DNS probed for stopped cluster")
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestRunningLinuxClustersFiltersStoppedConfigs(t *testing.T) {
	t.Parallel()

	got, err := runningLinuxClusters(
		func() ([]cluster.Cluster, error) {
			return []cluster.Cluster{{Name: "running", SubnetIndex: 1}, {Name: "stopped", SubnetIndex: 2}}, nil
		},
		func() ([]daemon.ClusterSummary, error) {
			return []daemon.ClusterSummary{{Name: "running", Running: true}, {Name: "stopped", Running: false}}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "running" {
		t.Fatalf("runningLinuxClusters() = %+v, want only running", got)
	}
}

func TestLinuxPlatformDoctorFindingsKVMMissing(t *testing.T) {
	t.Parallel()

	finding := linuxKVMFinding(func(string) error { return os.ErrNotExist })
	if finding.level != "FAIL" || !strings.Contains(finding.detail, "/dev/kvm is missing") {
		t.Fatalf("finding = %+v", finding)
	}
}

func TestLinuxPlatformDoctorFindingsKVMAccessHint(t *testing.T) {
	t.Parallel()

	finding := linuxKVMFinding(func(string) error { return errors.New("permission denied") })
	if finding.level != "FAIL" || !strings.Contains(finding.detail, doctorKVMGroupFix) {
		t.Fatalf("finding = %+v", finding)
	}
}

func TestLinuxPlatformDoctorFindingsQEMUMissingRequiredMachine(t *testing.T) {
	t.Parallel()

	system, err := doctorQEMUSystemForArchitecture(runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	finding := linuxQEMUFinding(func(name string, args ...string) ([]byte, error) {
		switch {
		case name == system.binary && fmt.Sprint(args) == "[--version]":
			return []byte("QEMU emulator version 8.2.2\n"), nil
		case name == system.binary && fmt.Sprint(args) == "[-machine help]":
			return []byte("unsupported-machine  Test machine\n"), nil
		default:
			t.Fatalf("unexpected command %s %v", name, args)
			return nil, nil
		}
	})
	if finding.level != "FAIL" || !strings.Contains(finding.detail, fmt.Sprintf("required machine type %q", system.machine)) {
		t.Fatalf("finding = %+v", finding)
	}
}

func TestLinuxPlatformDoctorFindingsQEMUInstallHint(t *testing.T) {
	t.Parallel()

	system, err := doctorQEMUSystemForArchitecture(runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	finding := linuxQEMUFinding(func(name string, args ...string) ([]byte, error) {
		if name != system.binary || fmt.Sprint(args) != "[--version]" {
			t.Fatalf("unexpected command %s %v", name, args)
		}
		return nil, errors.New("executable file not found")
	})
	if finding.level != "FAIL" || !strings.Contains(finding.detail, "install the QEMU package") {
		t.Fatalf("finding = %+v", finding)
	}
}

func TestLinuxPlatformDoctorFindingsBridgeNetfilterSignature(t *testing.T) {
	t.Parallel()

	finding := linuxBridgeNetfilterFinding(
		func(path string) ([]byte, error) {
			if path != doctorBridgeNFCall {
				t.Fatalf("unexpected read %s", path)
			}
			return []byte("1\n"), nil
		},
		func(name string, args ...string) ([]byte, error) {
			if name != "iptables" || fmt.Sprint(args) != "[-S FORWARD]" {
				t.Fatalf("unexpected command %s %v", name, args)
			}
			return []byte("-P FORWARD DROP\n-A FORWARD -j DOCKER-USER\n"), nil
		},
	)
	if finding.level != "FAIL" || !strings.Contains(finding.detail, "physdev") {
		t.Fatalf("finding = %+v", finding)
	}
}

func TestLinuxPlatformDoctorFindingsBridgeSTPSkipsMissingBridge(t *testing.T) {
	t.Parallel()

	finding := linuxBridgeSTPFinding(
		func() ([]cluster.Cluster, error) { return []cluster.Cluster{{Name: "demo", SubnetIndex: 7}}, nil },
		func(path string) ([]byte, error) {
			if path != "/sys/class/net/br-tbx7/bridge/stp_state" {
				t.Fatalf("unexpected read %s", path)
			}
			return nil, os.ErrNotExist
		},
	)
	if finding.level != "PASS" {
		t.Fatalf("finding = %+v, want PASS for absent stopped-cluster bridge", finding)
	}
}

func TestLinuxPlatformDoctorFindingsBridgeSTPFailure(t *testing.T) {
	t.Parallel()

	finding := linuxBridgeSTPFinding(
		func() ([]cluster.Cluster, error) { return []cluster.Cluster{{Name: "demo", SubnetIndex: 7}}, nil },
		func(path string) ([]byte, error) {
			if path != "/sys/class/net/br-tbx7/bridge/stp_state" {
				t.Fatalf("unexpected read %s", path)
			}
			return []byte("1\n"), nil
		},
	)
	if finding.level != "FAIL" || !strings.Contains(finding.detail, "stp_state 0") {
		t.Fatalf("finding = %+v", finding)
	}
}

func TestLinuxPlatformDoctorFindingsRPFilterDetectsVPNWithoutDefaultRoute(t *testing.T) {
	t.Parallel()

	finding := linuxRPFilterFinding(
		func(name string, args ...string) ([]byte, error) {
			switch {
			case name == "ip" && fmt.Sprint(args) == "[-o route show default]":
				return []byte("default via 192.0.2.1 dev eth0\n"), nil
			case name == "ip" && fmt.Sprint(args) == "[-o link show up]":
				return []byte("1: lo: <LOOPBACK,UP> mtu 65536\n2: eth0: <BROADCAST,UP> mtu 1500\n7: wg0: <POINTOPOINT,UP> mtu 1420\n"), nil
			default:
				t.Fatalf("unexpected command %s %v", name, args)
				return nil, nil
			}
		},
		func(path string) ([]byte, error) {
			switch path {
			case "/proc/sys/net/ipv4/conf/all/rp_filter":
				return []byte("1\n"), nil
			case "/proc/sys/net/ipv4/conf/eth0/rp_filter":
				return []byte("0\n"), nil
			case "/proc/sys/net/ipv4/conf/wg0/rp_filter":
				return []byte("0\n"), nil
			default:
				t.Fatalf("unexpected read %s", path)
				return nil, nil
			}
		},
	)
	if finding.level != "FAIL" || !strings.Contains(finding.detail, "multi-homed/VPN host") {
		t.Fatalf("finding = %+v", finding)
	}
}

func TestLinuxPlatformDoctorFindingsPortIgnoresTalosBoxOwner(t *testing.T) {
	t.Parallel()

	finding := linuxPortFinding(
		53,
		"udp",
		func() ([]cluster.Cluster, error) { return []cluster.Cluster{{Name: "demo", SubnetIndex: 7}}, nil },
		func(path string) ([]byte, error) {
			if path != "/sys/class/net/br-tbx7/ifindex" {
				t.Fatalf("unexpected read %s", path)
			}
			return []byte("5\n"), nil
		},
		func(name string, args ...string) ([]byte, error) {
			if name != "ss" {
				t.Fatalf("unexpected command %s %v", name, args)
			}
			return []byte("UNCONN 0 0 172.30.7.1:53 0.0.0.0:* users:((\"tbx-helper\",pid=1,fd=3))\n"), nil
		},
		nil,
		nil,
	)
	if finding.level != "PASS" {
		t.Fatalf("finding = %+v", finding)
	}
}

func TestLinuxPlatformDoctorFindingsPortForeignConflict(t *testing.T) {
	t.Parallel()

	finding := linuxPortFinding(
		67,
		"udp",
		func() ([]cluster.Cluster, error) { return []cluster.Cluster{{Name: "demo", SubnetIndex: 7}}, nil },
		func(path string) ([]byte, error) {
			if path != "/sys/class/net/br-tbx7/ifindex" {
				t.Fatalf("unexpected read %s", path)
			}
			return []byte("5\n"), nil
		},
		func(name string, args ...string) ([]byte, error) {
			if name != "ss" {
				t.Fatalf("unexpected command %s %v", name, args)
			}
			return []byte("UNCONN 0 0 172.30.7.1:67 0.0.0.0:* users:((\"dhcpd\",pid=2,fd=4))\n"), nil
		},
		nil,
		nil,
	)
	if finding.level != "FAIL" || !strings.Contains(finding.detail, "dhcpd") {
		t.Fatalf("finding = %+v", finding)
	}
}

func TestLinuxPlatformDoctorFindingsPortDetectsWildcardConflict(t *testing.T) {
	t.Parallel()

	finding := linuxPortFinding(
		53,
		"udp",
		func() ([]cluster.Cluster, error) { return []cluster.Cluster{{Name: "demo", SubnetIndex: 7}}, nil },
		func(string) ([]byte, error) { return []byte("5\n"), nil },
		func(string, ...string) ([]byte, error) {
			return []byte("UNCONN 0 0 0.0.0.0:53 0.0.0.0:* users:((\"dnsmasq\",pid=2,fd=4))\n"), nil
		},
		nil,
		nil,
	)
	if finding.level != "FAIL" || !strings.Contains(finding.detail, "dnsmasq") {
		t.Fatalf("finding = %+v", finding)
	}
}

func TestFindSocketListenerRequiresExactPort(t *testing.T) {
	t.Parallel()

	output := []byte("UNCONN 0 0 172.30.7.1:5300 0.0.0.0:* users:((\"dnsmasq\",pid=2,fd=4))\n")
	if got := findSocketListener(output, "172.30.7.1", 53); got.line != "" {
		t.Fatalf("findSocketListener() = %+v, want no :53 match for :5300", got)
	}
}

func TestLinuxPlatformDoctorFindingsPortUsesBindPreflight(t *testing.T) {
	t.Parallel()

	var bindCalls []string
	finding := linuxPortFinding(
		179,
		"tcp",
		func() ([]cluster.Cluster, error) { return []cluster.Cluster{{Name: "demo", SubnetIndex: 7}}, nil },
		func(path string) ([]byte, error) {
			if path != "/sys/class/net/br-tbx7/ifindex" {
				t.Fatalf("unexpected read %s", path)
			}
			return []byte("5\n"), nil
		},
		func(name string, args ...string) ([]byte, error) {
			if name != "ss" {
				t.Fatalf("unexpected command %s %v", name, args)
			}
			return nil, nil
		},
		nil,
		func(network, address string) (io.Closer, error) {
			bindCalls = append(bindCalls, network+" "+address)
			return noopCloser{}, nil
		},
	)
	if finding.level != "PASS" {
		t.Fatalf("finding = %+v", finding)
	}
	if fmt.Sprint(bindCalls) != "[tcp4 172.30.7.1:179]" {
		t.Fatalf("bindCalls = %v", bindCalls)
	}
}

func TestLinuxPlatformDoctorFindingsPortSkipsAbsentBridge(t *testing.T) {
	t.Parallel()

	finding := linuxPortFinding(
		53,
		"udp",
		func() ([]cluster.Cluster, error) { return []cluster.Cluster{{Name: "demo", SubnetIndex: 7}}, nil },
		func(path string) ([]byte, error) {
			if path != "/sys/class/net/br-tbx7/ifindex" {
				t.Fatalf("unexpected read %s", path)
			}
			return nil, os.ErrNotExist
		},
		func(name string, args ...string) ([]byte, error) {
			if name != "ss" {
				t.Fatalf("unexpected command %s %v", name, args)
			}
			return nil, nil
		},
		nil,
		nil,
	)
	if finding.level != "SKIP" || !strings.Contains(finding.detail, "no cluster bridges exist") {
		t.Fatalf("finding = %+v", finding)
	}
}

func TestLinuxPlatformDoctorFindingsHelperUnitDisabled(t *testing.T) {
	t.Parallel()

	finding := linuxHelperUnitFinding(func(name string, args ...string) ([]byte, error) {
		if name != "systemctl" || fmt.Sprint(args) != "[is-enabled tbx-helper.socket]" {
			t.Fatalf("unexpected command %s %v", name, args)
		}
		return []byte("disabled\n"), errors.New("exit status 1")
	})
	if finding.level != "FAIL" || !strings.Contains(finding.detail, "enable --now") {
		t.Fatalf("finding = %+v", finding)
	}
}

func TestLinuxPlatformDoctorFindingsHelperAccessSuggestsUsermod(t *testing.T) {
	t.Parallel()

	finding := linuxHelperAccessFinding(func(name string, args ...string) ([]byte, error) {
		if name != "id" || fmt.Sprint(args) != "[-Gn]" {
			t.Fatalf("unexpected command %s %v", name, args)
		}
		return []byte("wheel kvm\n"), nil
	})
	if finding.level != "FAIL" || !strings.Contains(finding.detail, doctorHelperGroupFix) {
		t.Fatalf("finding = %+v", finding)
	}
}

func TestLinuxPlatformDoctorFindingsHelperCapabilitiesMustMatchExactly(t *testing.T) {
	t.Parallel()

	finding := linuxHelperCapabilitiesFinding(func() (helperCapabilityReport, error) {
		return helperCapabilityReport{
			Effective:      doctorHelperCapabilityMask | 1,
			EffectiveNames: []string{"CAP_NET_BIND_SERVICE", "CAP_NET_ADMIN", "CAP_NET_RAW"},
		}, nil
	})
	if finding.level != "FAIL" || !strings.Contains(finding.detail, "want exactly") {
		t.Fatalf("finding = %+v", finding)
	}
}

func TestLinuxPlatformDoctorFindingsHelperCapabilitiesPassExactMask(t *testing.T) {
	t.Parallel()

	finding := linuxHelperCapabilitiesFinding(func() (helperCapabilityReport, error) {
		return helperCapabilityReport{Effective: doctorHelperCapabilityMask}, nil
	})
	if finding.level != "PASS" {
		t.Fatalf("finding = %+v", finding)
	}
}

func TestLinuxPlatformDoctorFindingsPortIgnoresUnprivilegedBindFailure(t *testing.T) {
	t.Parallel()

	finding := linuxPortFinding(
		53,
		"udp",
		func() ([]cluster.Cluster, error) { return []cluster.Cluster{{Name: "demo", SubnetIndex: 7}}, nil },
		func(string) ([]byte, error) { return []byte("5\n"), nil },
		func(string, ...string) ([]byte, error) { return nil, nil },
		func(string, string) (net.PacketConn, error) { return nil, os.ErrPermission },
		nil,
	)
	if finding.level != "PASS" {
		t.Fatalf("finding = %+v", finding)
	}
}

func TestRPFilterRelevantInterfacesIgnoresUnrelatedVirtualLinks(t *testing.T) {
	t.Parallel()

	got := rpFilterRelevantInterfaces(
		[]byte("default via 192.0.2.1 dev eth0\n"),
		[]byte("2: eth0: <UP> mtu 1500\n3: docker0: <UP> mtu 1500\n4: veth1234@if5: <UP> mtu 1500\n"),
	)
	if fmt.Sprint(got) != "[eth0]" {
		t.Fatalf("interfaces = %v, want [eth0]", got)
	}
}

type noopCloser struct{}

func (noopCloser) Close() error { return nil }
