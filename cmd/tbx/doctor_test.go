//go:build darwin

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"syscall"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
	"github.com/randax/talos-box/internal/hostpressure"
)

func TestParseDSCacheAddresses(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   []string
	}{
		{
			name: "IPv4 answer",
			output: `name: doctor-probe.demo.k8s.test
ip_address: 172.30.7.200
`,
			want: []string{"172.30.7.200"},
		},
		{
			name: "multiple records and unrelated fields",
			output: `name: example.test
ipv6_address: 2001:db8::10
ip_address: 192.0.2.10
alias: ignored.example.test
ip_address: 192.0.2.11
`,
			want: []string{"2001:db8::10", "192.0.2.10", "192.0.2.11"},
		},
		{name: "empty answer", output: "", want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addresses := parseDSCacheAddresses([]byte(tt.output))
			got := make([]string, 0, len(addresses))
			for _, address := range addresses {
				got = append(got, address.String())
			}
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Fatalf("parseDSCacheAddresses() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseRouteInterface(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		want    string
		wantErr bool
	}{
		{
			name: "bridge route",
			output: `   route to: 172.30.3.1
destination: 172.30.3.1
  interface: bridge100
      flags: <UP,HOST,DONE,LLINFO,CLONING,IFSCOPE,IFREF>
`,
			want: "bridge100",
		},
		{
			name: "VPN route",
			output: `   route to: 172.30.3.2
destination: 172.30.3.0
       mask: 255.255.255.0
  interface: utun6
`,
			want: "utun6",
		},
		{name: "missing interface", output: "route to: 172.30.3.1\n", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseRouteInterface([]byte(tt.output))
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseRouteInterface() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("parseRouteInterface() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClassifyEgressError(t *testing.T) {
	timeout := &net.DNSError{Err: "i/o timeout", IsTimeout: true}
	tests := []struct {
		name string
		err  error
		want egressErrorKind
	}{
		{name: "unknown authority", err: &url.Error{Err: x509.UnknownAuthorityError{}}, want: egressUnknownAuthority},
		{name: "timeout", err: &url.Error{Err: timeout}, want: egressTimeout},
		{name: "deadline", err: context.DeadlineExceeded, want: egressTimeout},
		{name: "connection reset", err: &url.Error{Err: &net.OpError{Err: syscall.ECONNRESET}}, want: egressConnectionReset},
		{name: "textual reset", err: errors.New("read: connection reset by peer"), want: egressConnectionReset},
		{name: "other", err: errors.New("no route to host"), want: egressOther},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyEgressError(tt.err); got != tt.want {
				t.Fatalf("classifyEgressError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEgressFindingMessages(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "pass", want: "PASS egress"},
		{name: "unknown authority", err: x509.UnknownAuthorityError{}, want: "WARN egress: TLS interception certificate is signed by an unknown authority; install the trusted corporate CA in the System keychain"},
		{name: "timeout", err: context.DeadlineExceeded, want: "WARN egress: connection timed out (likely proxy-only egress); HTTPS_PROXY must be set in the shell that starts tbx"},
		{name: "reset", err: syscall.ECONNRESET, want: "WARN egress: connection reset during the TLS handshake (TLS filtered)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			finding := egressFinding(tt.err)
			got := finding.level + " " + finding.check
			if finding.detail != "" {
				got += ": " + finding.detail
			}
			if got != tt.want {
				t.Fatalf("egressFinding() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestProbeFactoryEgressIgnoresPostHandshakeFailure(t *testing.T) {
	do := func(request *http.Request) (*http.Response, error) {
		trace := httptrace.ContextClientTrace(request.Context())
		if trace == nil || trace.TLSHandshakeDone == nil {
			t.Fatal("request has no TLS handshake trace")
		}
		trace.TLSHandshakeDone(tls.ConnectionState{}, nil)
		return nil, context.DeadlineExceeded
	}
	if err := probeFactoryEgress(do); err != nil {
		t.Fatalf("probeFactoryEgress() = %v after successful TLS handshake", err)
	}
}

func TestParseActivatedSystemExtensions(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   []string
	}{
		{
			name: "activated extensions only",
			output: `3 extension(s)
--- com.apple.system_extension.network_extension (Go to 'System Settings > General > Login Items & Extensions > Network Extensions' to modify these system extension(s))
enabled active teamID bundleID (version) name [state]
* * 9PTGMPNXZ2 com.microsoft.wdav.netext (101.25082.0003/101.25082.0003) Microsoft Defender Network Extension [activated enabled]
* - PXPZ95SK77 com.example.disabled (1.0/1.0) Disabled Extension [terminated waiting to uninstall on reboot]
* * ZMCG7MLDV9 com.crowdstrike.falcon.Agent (7.20/7.20) Falcon Sensor [activated enabled]
* * ABCDE12345 com.example.unknown-filter (2.0/2.0) Unknown Filter [activated enabled]
			`,
			want: []string{
				"com.microsoft.wdav.netext",
				"com.crowdstrike.falcon.Agent",
				"com.example.unknown-filter",
			},
		},
		{name: "no extensions", output: "0 extension(s)\n", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseActivatedSystemExtensions([]byte(tt.output))
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Fatalf("parseActivatedSystemExtensions() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSecurityExtensionWarning(t *testing.T) {
	tests := []struct {
		bundleID string
		want     string
	}{
		{"com.paloaltonetworks.GlobalProtect.client.extension", "guest TLS will be reset; registry mirrors are required"},
		{"com.zscaler.network-extension", "may filter local/guest traffic or DNS"},
		{"com.netskope.client.network-extension", "may filter local/guest traffic or DNS"},
		{"com.cisco.anyconnect.macos.acsockext", "may filter local/guest traffic or DNS"},
		{"com.cisco.secureclient.networkextension", "may filter local/guest traffic or DNS"},
		{"com.crowdstrike.falcon.Agent", "EDR present; ad-hoc-signed binaries may be blocked"},
		{"com.microsoft.wdav.netext", "EDR present; ad-hoc-signed binaries may be blocked"},
		{"com.sentinelone.network-monitoring", "EDR present; ad-hoc-signed binaries may be blocked"},
		{"io.tailscale.ipn.macsys.network-extension", "VPN present; check route capture"},
		{"ch.protonvpn.mac.WireGuard-Extension", "VPN present; check route capture"},
		{"com.wireguard.macos.network-extension", "VPN present; check route capture"},
		{"com.example.unknown", ""},
	}

	for _, tt := range tests {
		t.Run(tt.bundleID, func(t *testing.T) {
			if got := securityExtensionWarning(tt.bundleID); got != tt.want {
				t.Fatalf("securityExtensionWarning() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRunDoctorContinuesAfterFailures(t *testing.T) {
	var calls []string
	recordError := func(name string) func() error {
		return func() error {
			calls = append(calls, name)
			return errors.New("broken")
		}
	}
	deps := doctorDependencies{
		checkHelper:     recordError("helper"),
		checkResolver:   recordError("resolver"),
		checkDirectDNS:  recordError("DNS"),
		checkForwarding: recordError("forwarding"),
		listClusters: func() ([]daemon.ClusterSummary, error) {
			calls = append(calls, "cluster.list")
			return nil, dialError{err: errors.New("connection refused")}
		},
		getStatus: func() ([]daemon.ClusterStatus, error) {
			t.Fatal("status should not be requested without clusters")
			return nil, nil
		},
		command: func(string, ...string) ([]byte, error) {
			calls = append(calls, "extensions")
			return nil, nil
		},
		doHTTP: func(*http.Request) (*http.Response, error) {
			calls = append(calls, "egress")
			return &http.Response{Body: http.NoBody}, nil
		},
	}
	var output strings.Builder
	err := (cli{out: &output}).runDoctorWithDependencies(nil, deps)
	if err == nil {
		t.Fatal("runDoctorWithDependencies() succeeded despite failing checks")
	}
	for _, name := range []string{"helper", "resolver", "DNS", "forwarding", "cluster.list", "egress", "extensions"} {
		if !strings.Contains(fmt.Sprint(calls), name) {
			t.Errorf("%s was not run; calls = %v", name, calls)
		}
	}
	for _, line := range []string{
		"FAIL helper: broken",
		"FAIL resolver: broken",
		"FAIL DNS: broken",
		"FAIL forwarding: broken",
		"SKIP system-dns: daemon unavailable: connection refused",
		"SKIP routes: daemon unavailable: connection refused",
		"PASS egress",
		"INFO security-inventory: no activated system extensions found",
	} {
		if !strings.Contains(output.String(), line) {
			t.Errorf("output missing %q:\n%s", line, output.String())
		}
	}
}

func TestRunDoctorRunsLaterChecksAfterSystemDNSFailure(t *testing.T) {
	var calls []string
	pass := func() error { return nil }
	deps := doctorDependencies{
		checkHelper:     pass,
		checkResolver:   pass,
		checkDirectDNS:  pass,
		checkForwarding: pass,
		listClusters: func() ([]daemon.ClusterSummary, error) {
			return []daemon.ClusterSummary{{Name: "demo", SubnetIndex: 3, Running: true}}, nil
		},
		getStatus: func() ([]daemon.ClusterStatus, error) {
			return []daemon.ClusterStatus{{Name: "demo", Running: true}}, nil
		},
		command: func(name string, args ...string) ([]byte, error) {
			switch name {
			case "/usr/bin/dscacheutil":
				calls = append(calls, "system-dns")
				return []byte("ip_address: 203.0.113.10\n"), nil
			case "/sbin/route":
				calls = append(calls, "routes")
				return []byte("interface: bridge100\n"), nil
			case "/usr/bin/systemextensionsctl":
				calls = append(calls, "security-inventory")
				return nil, nil
			default:
				t.Fatalf("unexpected command %s %v", name, args)
				return nil, nil
			}
		},
		doHTTP: func(*http.Request) (*http.Response, error) {
			calls = append(calls, "egress")
			return &http.Response{Body: http.NoBody}, nil
		},
	}
	var output strings.Builder
	err := (cli{out: &output}).runDoctorWithDependencies(nil, deps)
	if err == nil {
		t.Fatal("runDoctorWithDependencies() succeeded despite system DNS failure")
	}
	for _, name := range []string{"system-dns", "routes", "egress", "security-inventory"} {
		if !strings.Contains(fmt.Sprint(calls), name) {
			t.Errorf("%s was not run; calls = %v", name, calls)
		}
	}
	for _, line := range []string{
		"FAIL system-dns: demo: scoped resolver is being bypassed (DNS filtering agent or browser/system DoH)",
		"PASS routes",
		"PASS egress",
		"INFO security-inventory",
	} {
		if !strings.Contains(output.String(), line) {
			t.Errorf("output missing %q:\n%s", line, output.String())
		}
	}
}

func TestRunDoctorNoClustersSkipsAndEgressWarnIsNonFatal(t *testing.T) {
	deps := passingDoctorDependencies()
	deps.listClusters = func() ([]daemon.ClusterSummary, error) { return nil, nil }
	deps.doHTTP = func(*http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	}
	var output strings.Builder
	if err := (cli{out: &output}).runDoctorWithDependencies(nil, deps); err != nil {
		t.Fatalf("runDoctorWithDependencies() = %v for WARN-only diagnostics", err)
	}
	for _, line := range []string{
		"SKIP system-dns: no clusters exist",
		"SKIP routes: no clusters exist",
		"WARN egress: connection timed out",
	} {
		if !strings.Contains(output.String(), line) {
			t.Errorf("output missing %q:\n%s", line, output.String())
		}
	}
}

func TestRunDoctorIncludesMirrorHealth(t *testing.T) {
	deps := passingDoctorDependencies()
	deps.listClusters = func() ([]daemon.ClusterSummary, error) {
		return []daemon.ClusterSummary{{Name: "demo", SubnetIndex: 3, Running: true}}, nil
	}
	deps.getStatus = func() ([]daemon.ClusterStatus, error) {
		return []daemon.ClusterStatus{{Name: "demo", Running: true, Nodes: []daemon.NodeStatus{{Name: "demo-cp-1", IP: "172.30.3.2"}}}}, nil
	}
	deps.command = func(name string, args ...string) ([]byte, error) {
		switch name {
		case "/usr/bin/dscacheutil":
			return []byte("ip_address: 172.30.3.200\n"), nil
		case "/sbin/route":
			return []byte("interface: bridge100\n"), nil
		default:
			return nil, nil
		}
	}
	deps.listCache = func() (daemon.CacheListResult, error) {
		return daemon.CacheListResult{
			MirrorBoundGatewayIPs: []string{"172.30.3.1"},
			MirrorTotal: daemon.MirrorCacheTotals{
				BlobCount:     2,
				BlobBytes:     20,
				ManifestCount: 1,
				ManifestBytes: 7,
			},
		}, nil
	}

	var output strings.Builder
	if err := (cli{out: &output}).runDoctorWithDependencies(nil, deps); err != nil {
		t.Fatalf("runDoctorWithDependencies() = %v", err)
	}
	if !strings.Contains(output.String(), "PASS mirror-health: mirror serving on 1 gateway(s) [172.30.3.1], cache 27 bytes (2 blob(s), 1 manifest(s))") {
		t.Fatalf("output missing mirror health line:\n%s", output.String())
	}
}

func TestRunDoctorSkipsMirrorHealthWhenNoClustersAreRunning(t *testing.T) {
	deps := passingDoctorDependencies()
	deps.listClusters = func() ([]daemon.ClusterSummary, error) {
		return []daemon.ClusterSummary{{Name: "demo", SubnetIndex: 3, Running: false}}, nil
	}
	deps.command = func(name string, args ...string) ([]byte, error) {
		if name == "/usr/bin/dscacheutil" {
			return []byte("ip_address: 172.30.3.200\n"), nil
		}
		return nil, nil
	}
	deps.listCache = func() (daemon.CacheListResult, error) {
		return daemon.CacheListResult{
			MirrorTotal: daemon.MirrorCacheTotals{
				BlobCount:     2,
				BlobBytes:     20,
				ManifestCount: 1,
				ManifestBytes: 7,
			},
		}, nil
	}

	var output strings.Builder
	if err := (cli{out: &output}).runDoctorWithDependencies(nil, deps); err != nil {
		t.Fatalf("runDoctorWithDependencies() = %v", err)
	}
	if !strings.Contains(output.String(), "SKIP mirror-health: no clusters are running; cache 27 bytes (2 blob(s), 1 manifest(s))") {
		t.Fatalf("output missing mirror idle line:\n%s", output.String())
	}
}

func TestRunDoctorFailsMirrorHealthWhenClusterIsRunningButNotBound(t *testing.T) {
	deps := passingDoctorDependencies()
	deps.listClusters = func() ([]daemon.ClusterSummary, error) {
		return []daemon.ClusterSummary{{Name: "demo", SubnetIndex: 3, Running: true}}, nil
	}
	deps.getStatus = func() ([]daemon.ClusterStatus, error) {
		return []daemon.ClusterStatus{{Name: "demo", Running: true, Nodes: []daemon.NodeStatus{{Name: "demo-cp-1", IP: "172.30.3.2"}}}}, nil
	}
	deps.command = func(name string, args ...string) ([]byte, error) {
		switch name {
		case "/usr/bin/dscacheutil":
			return []byte("ip_address: 172.30.3.200\n"), nil
		case "/sbin/route":
			return []byte("interface: bridge100\n"), nil
		default:
			return nil, nil
		}
	}
	deps.listCache = func() (daemon.CacheListResult, error) {
		return daemon.CacheListResult{
			MirrorTotal: daemon.MirrorCacheTotals{
				BlobCount:     2,
				BlobBytes:     20,
				ManifestCount: 1,
				ManifestBytes: 7,
			},
		}, nil
	}

	var output strings.Builder
	err := (cli{out: &output}).runDoctorWithDependencies(nil, deps)
	if err == nil {
		t.Fatal("runDoctorWithDependencies() succeeded despite missing mirror listeners")
	}
	if !strings.Contains(output.String(), "FAIL mirror-health: no mirror listeners bound for running cluster(s); expected [172.30.3.1]; cache 27 bytes (2 blob(s), 1 manifest(s))") {
		t.Fatalf("output missing mirror health line:\n%s", output.String())
	}
}

func TestRunDoctorFailsMirrorHealthWhenOnlySomeRunningClustersAreBound(t *testing.T) {
	deps := passingDoctorDependencies()
	deps.listClusters = func() ([]daemon.ClusterSummary, error) {
		return []daemon.ClusterSummary{
			{Name: "demo-a", SubnetIndex: 3, Running: true},
			{Name: "demo-b", SubnetIndex: 4, Running: true},
		}, nil
	}
	deps.getStatus = func() ([]daemon.ClusterStatus, error) {
		return []daemon.ClusterStatus{
			{Name: "demo-a", Running: true, Nodes: []daemon.NodeStatus{{Name: "demo-a-cp-1", IP: "172.30.3.2"}}},
			{Name: "demo-b", Running: true, Nodes: []daemon.NodeStatus{{Name: "demo-b-cp-1", IP: "172.30.4.2"}}},
		}, nil
	}
	deps.command = func(name string, args ...string) ([]byte, error) {
		switch name {
		case "/usr/bin/dscacheutil":
			if strings.Contains(args[len(args)-1], ".demo-a.") {
				return []byte("ip_address: 172.30.3.200\n"), nil
			}
			return []byte("ip_address: 172.30.4.200\n"), nil
		case "/sbin/route":
			return []byte("interface: bridge100\n"), nil
		default:
			return nil, nil
		}
	}
	deps.listCache = func() (daemon.CacheListResult, error) {
		return daemon.CacheListResult{
			MirrorBoundGatewayIPs: []string{"172.30.3.1"},
			MirrorTotal: daemon.MirrorCacheTotals{
				BlobCount:     2,
				BlobBytes:     20,
				ManifestCount: 1,
				ManifestBytes: 7,
			},
		}, nil
	}

	var output strings.Builder
	err := (cli{out: &output}).runDoctorWithDependencies(nil, deps)
	if err == nil {
		t.Fatal("runDoctorWithDependencies() succeeded despite partial mirror listener coverage")
	}
	if !strings.Contains(output.String(), "FAIL mirror-health: mirror listeners bound on [172.30.3.1], expected [172.30.3.1 172.30.4.1]; cache 27 bytes (2 blob(s), 1 manifest(s))") {
		t.Fatalf("output missing partial mirror health line:\n%s", output.String())
	}
}

func TestRunDoctorFailsMirrorHealthWhenBoundGatewayIPIsWrong(t *testing.T) {
	deps := passingDoctorDependencies()
	deps.listClusters = func() ([]daemon.ClusterSummary, error) {
		return []daemon.ClusterSummary{{Name: "demo", SubnetIndex: 3, Running: true}}, nil
	}
	deps.getStatus = func() ([]daemon.ClusterStatus, error) {
		return []daemon.ClusterStatus{{Name: "demo", Running: true, Nodes: []daemon.NodeStatus{{Name: "demo-cp-1", IP: "172.30.3.2"}}}}, nil
	}
	deps.command = func(name string, args ...string) ([]byte, error) {
		switch name {
		case "/usr/bin/dscacheutil":
			return []byte("ip_address: 172.30.3.200\n"), nil
		case "/sbin/route":
			return []byte("interface: bridge100\n"), nil
		default:
			return nil, nil
		}
	}
	deps.listCache = func() (daemon.CacheListResult, error) {
		return daemon.CacheListResult{
			MirrorBoundGatewayIPs: []string{"172.30.99.1"},
			MirrorTotal: daemon.MirrorCacheTotals{
				BlobCount:     2,
				BlobBytes:     20,
				ManifestCount: 1,
				ManifestBytes: 7,
			},
		}, nil
	}

	var output strings.Builder
	err := (cli{out: &output}).runDoctorWithDependencies(nil, deps)
	if err == nil {
		t.Fatal("runDoctorWithDependencies() succeeded despite wrong mirror binding")
	}
	if !strings.Contains(output.String(), "FAIL mirror-health: mirror listeners bound on [172.30.99.1], expected [172.30.3.1]; cache 27 bytes (2 blob(s), 1 manifest(s))") {
		t.Fatalf("output missing wrong-ip mirror health line:\n%s", output.String())
	}
}

func TestRunDoctorSkipsMirrorHealthWhenClusterStateIsUnavailableButGatewaysAreBound(t *testing.T) {
	deps := passingDoctorDependencies()
	deps.listClusters = func() ([]daemon.ClusterSummary, error) {
		return nil, errors.New("cluster state unavailable")
	}
	deps.listCache = func() (daemon.CacheListResult, error) {
		return daemon.CacheListResult{
			MirrorBoundGatewayIPs: []string{"172.30.3.1"},
			MirrorTotal: daemon.MirrorCacheTotals{
				BlobCount:     2,
				BlobBytes:     20,
				ManifestCount: 1,
				ManifestBytes: 7,
			},
		}, nil
	}

	var output strings.Builder
	err := (cli{out: &output}).runDoctorWithDependencies(nil, deps)
	if err == nil {
		t.Fatal("runDoctorWithDependencies() succeeded despite unavailable cluster state")
	}
	if !strings.Contains(output.String(), "SKIP mirror-health: cluster state unavailable; cache 27 bytes (2 blob(s), 1 manifest(s))") {
		t.Fatalf("output missing unavailable-cluster mirror health line:\n%s", output.String())
	}
}

func TestRunDoctorReportsRuntimeDependentChecksAsSkipped(t *testing.T) {
	deps := passingDoctorDependencies()
	deps.checkResolver = func() error {
		return skippedDoctorCheck{detail: "daemon unavailable: connection refused"}
	}
	deps.checkDirectDNS = func() error {
		return skippedDoctorCheck{detail: "daemon unavailable: connection refused"}
	}

	var output strings.Builder
	if err := (cli{out: &output}).runDoctorWithDependencies(nil, deps); err != nil {
		t.Fatalf("runDoctorWithDependencies() = %v for skipped checks", err)
	}
	for _, line := range []string{
		"SKIP resolver: daemon unavailable: connection refused",
		"SKIP DNS: daemon unavailable: connection refused",
	} {
		if !strings.Contains(output.String(), line) {
			t.Errorf("output missing %q:\n%s", line, output.String())
		}
	}
}

// Sticky swap blocks nothing, but doctor must not call 90% swap a PASS: the
// same reading turns into a create refusal the moment pressure ticks up (#284).
func TestRunDoctorWarnsHostPressureForStickySwap(t *testing.T) {
	deps := passingDoctorDependencies()
	deps.hostPressure = func() (hostpressure.Snapshot, error) {
		return hostpressure.Snapshot{
			Swap:           hostpressure.Usage{TotalBytes: 10 << 30, AvailableBytes: 1 << 30},
			MemoryPressure: hostpressure.MemoryPressureNormal,
		}, nil
	}
	var output strings.Builder
	if err := (cli{out: &output}).runDoctorWithDependencies(nil, deps); err != nil {
		t.Fatalf("runDoctorWithDependencies() = %v for sticky swap with normal pressure", err)
	}
	for _, fragment := range []string{
		"WARN host-pressure: host swap is 90% used (9.0 GiB of 10.0 GiB) while memory pressure is normal",
		"`tbx down`",
	} {
		if !strings.Contains(output.String(), fragment) {
			t.Errorf("output missing %q:\n%s", fragment, output.String())
		}
	}
}

// Doctor fails on exactly what the create gate refuses, with the same numbers
// and a runnable remediation line.
func TestRunDoctorFailsOnBlockingHostPressure(t *testing.T) {
	deps := passingDoctorDependencies()
	deps.hostPressure = func() (hostpressure.Snapshot, error) {
		return hostpressure.Snapshot{
			Swap:       hostpressure.Usage{TotalBytes: 10 << 30, AvailableBytes: 1 << 30},
			DataVolume: hostpressure.Usage{TotalBytes: 100 << 30, AvailableBytes: 5 << 30},
		}, nil
	}
	var output strings.Builder
	if err := (cli{out: &output}).runDoctorWithDependencies(nil, deps); err == nil {
		t.Fatal("runDoctorWithDependencies() succeeded despite host pressure that blocks cluster create")
	}
	for _, line := range []string{
		"FAIL host-pressure: host swap is 90% used (9.0 GiB of 10.0 GiB) and memory pressure could not be measured",
		"FAIL host-pressure: talosbox data volume is 95% used (5.0 GiB free)",
		"`tbx down`",
		"`tbx cache prune --all`",
		"--force",
	} {
		if !strings.Contains(output.String(), line) {
			t.Errorf("output missing %q:\n%s", line, output.String())
		}
	}
}

func TestRunDoctorClusterListOperationErrorFailsChecks(t *testing.T) {
	deps := passingDoctorDependencies()
	deps.listClusters = func() ([]daemon.ClusterSummary, error) {
		return nil, errors.New("decode daemon result")
	}
	var output strings.Builder
	if err := (cli{out: &output}).runDoctorWithDependencies(nil, deps); err == nil {
		t.Fatal("runDoctorWithDependencies() succeeded after cluster.list operation error")
	}
	for _, line := range []string{
		"FAIL system-dns: list clusters: decode daemon result",
		"FAIL routes: list clusters: decode daemon result",
	} {
		if !strings.Contains(output.String(), line) {
			t.Errorf("output missing %q:\n%s", line, output.String())
		}
	}
}

func TestRunDoctorStatusErrorFailsRoutes(t *testing.T) {
	deps := passingDoctorDependencies()
	deps.listClusters = func() ([]daemon.ClusterSummary, error) {
		return []daemon.ClusterSummary{{Name: "demo", SubnetIndex: 3, Running: true}}, nil
	}
	deps.getStatus = func() ([]daemon.ClusterStatus, error) {
		return nil, errors.New("status unavailable")
	}
	deps.command = func(name string, args ...string) ([]byte, error) {
		switch name {
		case "/usr/bin/dscacheutil":
			return []byte("ip_address: 172.30.3.200\n"), nil
		case "/sbin/route":
			return []byte("interface: bridge100\n"), nil
		case "/usr/bin/systemextensionsctl":
			return nil, nil
		default:
			t.Fatalf("unexpected command %s %v", name, args)
			return nil, nil
		}
	}
	var output strings.Builder
	if err := (cli{out: &output}).runDoctorWithDependencies(nil, deps); err == nil {
		t.Fatal("runDoctorWithDependencies() succeeded without node route status")
	}
	if !strings.Contains(output.String(), "FAIL routes: cluster status unavailable; node routes could not be checked") {
		t.Fatalf("routes failure missing from output:\n%s", output.String())
	}
	if strings.Contains(output.String(), "PASS routes") {
		t.Fatalf("routes incorrectly passed:\n%s", output.String())
	}
}

func TestCheckSystemDNSChecksEveryCluster(t *testing.T) {
	clusters := []daemon.ClusterSummary{
		{Name: "alpha", SubnetIndex: 2},
		{Name: "beta", SubnetIndex: 9},
	}
	var names []string
	command := func(name string, args ...string) ([]byte, error) {
		if name != "/usr/bin/dscacheutil" || len(args) != 5 ||
			args[0] != "-q" || args[1] != "host" || args[2] != "-a" || args[3] != "name" {
			t.Fatalf("unexpected resolver command: %s %v", name, args)
		}
		probeName := args[4]
		names = append(names, probeName)
		subnet := 2
		if strings.Contains(probeName, ".beta.") {
			subnet = 9
		}
		return []byte(fmt.Sprintf("name: %s\nip_address: 172.30.%d.200\n", probeName, subnet)), nil
	}
	if err := checkSystemDNS(clusters, command); err != nil {
		t.Fatalf("checkSystemDNS() = %v", err)
	}
	want := []string{"doctor-probe.alpha.k8s.test", "doctor-probe.beta.k8s.test"}
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Fatalf("lookups = %v, want %v", names, want)
	}
}

func TestCheckClusterRoutesChecksGatewayAndNodeForEveryCluster(t *testing.T) {
	clusters := []daemon.ClusterSummary{
		{Name: "alpha", SubnetIndex: 2, Running: true},
		{Name: "beta", SubnetIndex: 9, Running: true},
	}
	statuses := []daemon.ClusterStatus{
		{Name: "alpha", Running: true, Nodes: []daemon.NodeStatus{{IP: "172.30.2.7"}}},
		{Name: "beta", Running: true, Nodes: []daemon.NodeStatus{{IP: "172.30.9.4"}}},
	}
	var targets []string
	command := func(name string, args ...string) ([]byte, error) {
		if name != "/sbin/route" || len(args) != 3 || args[0] != "-n" || args[1] != "get" {
			t.Fatalf("unexpected route command: %s %v", name, args)
		}
		targets = append(targets, args[2])
		return []byte("interface: vmnet8\n"), nil
	}
	if err := checkClusterRoutes(clusters, statuses, command); err != nil {
		t.Fatalf("checkClusterRoutes() = %v", err)
	}
	want := []string{"172.30.2.1", "172.30.2.7", "172.30.9.1", "172.30.9.4"}
	if fmt.Sprint(targets) != fmt.Sprint(want) {
		t.Fatalf("route targets = %v, want %v", targets, want)
	}
}

func TestCheckClusterRoutesSkipsStoppedClustersAndNodes(t *testing.T) {
	clusters := []daemon.ClusterSummary{
		{Name: "running", SubnetIndex: 2, Running: true},
		{Name: "stopped", SubnetIndex: 9},
	}
	statuses := []daemon.ClusterStatus{
		{Name: "running", Running: true, Nodes: []daemon.NodeStatus{
			{Name: "cp-1", IP: "172.30.2.7", Phase: daemon.PhaseStopped}, // stale lease
			{Name: "cp-2", IP: "172.30.2.8", Phase: daemon.PhaseConfigured},
		}},
		{Name: "stopped", Nodes: []daemon.NodeStatus{{IP: "172.30.9.4"}}},
	}
	var targets []string
	command := func(_ string, args ...string) ([]byte, error) {
		targets = append(targets, args[len(args)-1])
		return []byte("interface: bridge100\n"), nil
	}
	if err := checkClusterRoutes(clusters, statuses, command); err != nil {
		t.Fatalf("checkClusterRoutes() = %v", err)
	}
	want := []string{"172.30.2.1", "172.30.2.8"}
	if fmt.Sprint(targets) != fmt.Sprint(want) {
		t.Fatalf("route targets = %v, want %v (stopped cluster and stopped node must be skipped)", targets, want)
	}
}

func TestRunDoctorSkipsRoutesWhenNoClustersRunning(t *testing.T) {
	deps := passingDoctorDependencies()
	deps.listClusters = func() ([]daemon.ClusterSummary, error) {
		return []daemon.ClusterSummary{{Name: "demo", SubnetIndex: 3}}, nil
	}
	deps.getStatus = func() ([]daemon.ClusterStatus, error) {
		t.Fatal("status should not be requested when no clusters are running")
		return nil, nil
	}
	deps.command = func(name string, args ...string) ([]byte, error) {
		if name == "/sbin/route" {
			t.Fatalf("route probe ran for a stopped cluster: %s %v", name, args)
		}
		if name == "/usr/bin/dscacheutil" {
			return []byte("ip_address: 172.30.3.200\n"), nil
		}
		return nil, nil
	}
	var output strings.Builder
	if err := (cli{out: &output}).runDoctorWithDependencies(nil, deps); err != nil {
		t.Fatalf("runDoctorWithDependencies() = %v", err)
	}
	if !strings.Contains(output.String(), "SKIP routes: no clusters are running") {
		t.Fatalf("output missing routes SKIP:\n%s", output.String())
	}
}

func passingDoctorDependencies() doctorDependencies {
	pass := func() error { return nil }
	return doctorDependencies{
		checkHelper:     pass,
		checkResolver:   pass,
		checkDirectDNS:  pass,
		checkForwarding: pass,
		listClusters:    func() ([]daemon.ClusterSummary, error) { return nil, nil },
		getStatus:       func() ([]daemon.ClusterStatus, error) { return nil, nil },
		listCache:       func() (daemon.CacheListResult, error) { return daemon.CacheListResult{}, nil },
		hostPressure:    func() (hostpressure.Snapshot, error) { return hostpressure.Snapshot{}, nil },
		command: func(string, ...string) ([]byte, error) {
			return nil, nil
		},
		doHTTP: func(*http.Request) (*http.Response, error) {
			return &http.Response{Body: http.NoBody}, nil
		},
	}
}

func TestCheckRoutesAllowsLoopbackGatewayButNotLoopbackNode(t *testing.T) {
	clusters := []daemon.ClusterSummary{{Name: "demo", SubnetIndex: 0, Running: true}}
	statuses := []daemon.ClusterStatus{{
		Name:    "demo",
		Running: true,
		Nodes:   []daemon.NodeStatus{{Name: "demo-cp-1", IP: "172.30.0.2"}},
	}}
	gatewayLocal := func(_ string, args ...string) ([]byte, error) {
		iface := "bridge100"
		if args[len(args)-1] == "172.30.0.1" {
			iface = "lo0"
		}
		return []byte("interface: " + iface + "\n"), nil
	}
	if err := checkClusterRoutes(clusters, statuses, gatewayLocal); err != nil {
		t.Fatalf("checkClusterRoutes() = %v; lo0 gateway must be healthy", err)
	}

	nodeLocal := func(_ string, _ ...string) ([]byte, error) {
		return []byte("interface: lo0\n"), nil
	}
	if err := checkClusterRoutes(clusters, statuses, nodeLocal); err == nil {
		t.Fatal("checkClusterRoutes() succeeded for a node route via lo0")
	}
}

func TestCheckRoutesDetectsCapturedSubnet(t *testing.T) {
	clusters := []daemon.ClusterSummary{{Name: "demo", SubnetIndex: 3, Running: true}}
	statuses := []daemon.ClusterStatus{{
		Name:    "demo",
		Running: true,
		Nodes:   []daemon.NodeStatus{{Name: "demo-cp-1", IP: "172.30.3.2"}},
	}}
	command := func(_ string, args ...string) ([]byte, error) {
		iface := "bridge100"
		if args[len(args)-1] == "172.30.3.2" {
			iface = "utun6"
		}
		return []byte("interface: " + iface + "\n"), nil
	}

	err := checkClusterRoutes(clusters, statuses, command)
	if err == nil {
		t.Fatal("checkClusterRoutes() succeeded for VPN-captured node route")
	}
	for _, fragment := range []string{"demo", "172.30.3.2", "utun6", "VPN/ZTNA client has captured the cluster subnet"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("error %q missing %q", err, fragment)
		}
	}
}
