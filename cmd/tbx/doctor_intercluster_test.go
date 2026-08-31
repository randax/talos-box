package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/daemon"
)

func jsonResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func liveClusterStatuses() []daemon.ClusterStatus {
	return []daemon.ClusterStatus{
		{
			Name: "qa-core", Running: true, VIP: "172.30.0.200", VIPLive: true,
			Domain: "qa-core.k8s.test", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel},
		},
		{
			Name: "qa-edge", Running: true, VIP: "172.30.1.200", VIPLive: true,
			Domain: "qa-edge.k8s.test", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel},
		},
	}
}

func liveCiliumClusterStatuses() []daemon.ClusterStatus {
	return []daemon.ClusterStatus{
		{
			Name: "qa-core", Running: true, VIP: "172.30.0.200", VIPLive: true,
			Domain: "qa-core.k8s.test", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium},
		},
		{
			Name: "qa-edge", Running: true, VIP: "172.30.1.200", VIPLive: true,
			Domain: "qa-edge.k8s.test", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium},
		},
	}
}

func TestInterClusterFinding(t *testing.T) {
	tests := []struct {
		name       string
		statuses   []daemon.ClusterStatus
		statusErr  error
		do         httpDo
		wantLevel  string
		wantDetail string
	}{
		{
			name:       "single cluster is skipped with a reason",
			statuses:   []daemon.ClusterStatus{{Name: "demo", Running: true, VIP: "172.30.0.200", VIPLive: true}},
			do:         func(*http.Request) (*http.Response, error) { return jsonResponse(`{}`), nil },
			wantLevel:  "SKIP",
			wantDetail: "1 cluster(s) running; inter-cluster paths need at least two",
		},
		{
			name: "no live VIPs is skipped with a reason",
			statuses: []daemon.ClusterStatus{
				{Name: "qa-core", Running: true},
				{Name: "qa-edge", Running: true},
			},
			do:         func(*http.Request) (*http.Response, error) { return jsonResponse(`{}`), nil },
			wantLevel:  "SKIP",
			wantDetail: "0 of 2 running cluster(s) report a live LoadBalancer VIP",
		},
		{
			name:      "every path answers",
			statuses:  liveClusterStatuses(),
			do:        func(*http.Request) (*http.Response, error) { return jsonResponse(`{"responses":["lb-probe-1"]}`), nil },
			wantLevel: "PASS",
		},
		{
			name:     "a dead sibling path names the dead direction",
			statuses: liveClusterStatuses(),
			do: func(request *http.Request) (*http.Response, error) {
				if request.URL.Host == "172.30.1.200" && request.URL.Path == "/dial" {
					return jsonResponse(`{"errors":["dial tcp 172.30.0.200:80: i/o timeout"]}`), nil
				}
				return jsonResponse(`{"responses":["lb-probe-1"]}`), nil
			},
			wantLevel:  "FAIL",
			wantDetail: "qa-edge → qa-core VIP 172.30.0.200: dial failed: dial tcp 172.30.0.200:80: i/o timeout",
		},
		{
			name:     "a dead host path names the cluster",
			statuses: liveClusterStatuses(),
			do: func(request *http.Request) (*http.Response, error) {
				if request.URL.Host == "172.30.0.200" {
					return nil, errors.New("i/o timeout")
				}
				return jsonResponse(`{"responses":["lb-probe-1"]}`), nil
			},
			wantLevel:  "FAIL",
			wantDetail: "host → qa-core VIP 172.30.0.200: i/o timeout",
		},
		{
			// #388's actual shape: the path blackholes, so the dial request
			// never comes back and our own deadline fires. The source VIP
			// answered the host a moment earlier, so this is the sibling path,
			// and doctor must fail on it rather than filing an advisory.
			name:     "a sibling dial that never answers fails and names the direction",
			statuses: liveClusterStatuses(),
			do: func(request *http.Request) (*http.Response, error) {
				if request.URL.Host == "172.30.1.200" && request.URL.Path == "/dial" {
					return nil, context.DeadlineExceeded
				}
				return jsonResponse(`{"responses":["lb-probe-1"]}`), nil
			},
			wantLevel:  "FAIL",
			wantDetail: "qa-edge → qa-core VIP 172.30.0.200: no answer within 20s from the lb-probe behind 172.30.1.200",
		},
		{
			name:     "an lb-probe without a dial endpoint warns instead of failing",
			statuses: liveClusterStatuses(),
			do: func(request *http.Request) (*http.Response, error) {
				if request.URL.Path == "/dial" {
					return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody}, nil
				}
				return jsonResponse(`{}`), nil
			},
			wantLevel:  "WARN",
			wantDetail: "could not be probed: lb-probe has no dial endpoint (HTTP 404)",
		},
		{
			name:       "unavailable cluster status is skipped",
			statusErr:  errors.New("daemon busy"),
			do:         func(*http.Request) (*http.Response, error) { return jsonResponse(`{}`), nil },
			wantLevel:  "SKIP",
			wantDetail: "cluster status unavailable: daemon busy",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			finding := interClusterFinding(tt.statuses, tt.statusErr, tt.do)
			if finding.level != tt.wantLevel {
				t.Fatalf("level = %s (%s), want %s", finding.level, finding.detail, tt.wantLevel)
			}
			if tt.wantDetail != "" && !strings.Contains(finding.detail, tt.wantDetail) {
				t.Fatalf("detail = %q, want it to contain %q", finding.detail, tt.wantDetail)
			}
		})
	}
}

func TestProbeVIPFromHostSetsCiliumWildcardHost(t *testing.T) {
	var sawHost string
	err := probeVIPFromHost(func(request *http.Request) (*http.Response, error) {
		sawHost = request.Host
		return jsonResponse(`{}`), nil
	}, vipTarget{vip: "172.30.0.200", cni: "cilium", domain: "qa-core.k8s.test"})
	if err != nil {
		t.Fatal(err)
	}
	if sawHost != "probe.qa-core.k8s.test" {
		t.Fatalf("Host header = %q, want probe.qa-core.k8s.test", sawHost)
	}
}

func TestProbeVIPFromClusterUsesSourceAndSiblingDomainsForCilium(t *testing.T) {
	source := vipTarget{vip: "172.30.0.200", cni: "cilium", domain: "qa-core.k8s.test"}
	sibling := vipTarget{vip: "172.30.1.200", cni: "cilium", domain: "qa-edge.k8s.test"}
	err := probeVIPFromCluster(func(request *http.Request) (*http.Response, error) {
		if request.Host != "probe.qa-core.k8s.test" {
			t.Fatalf("outer Host header = %q", request.Host)
		}
		if request.URL.String() != "http://172.30.0.200/dial?host=probe.qa-edge.k8s.test&port=80&protocol=http&request=hostname&tries=1" {
			t.Fatalf("dial URL = %s", request.URL.String())
		}
		return jsonResponse(`{"responses":["lb-probe-1"]}`), nil
	}, source, sibling)
	if err != nil {
		t.Fatal(err)
	}
}

func TestInterClusterFindingCarriesCiliumDomainsIntoBothProbeLegs(t *testing.T) {
	var seen []string
	finding := interClusterFinding(liveCiliumClusterStatuses(), nil, func(request *http.Request) (*http.Response, error) {
		seen = append(seen, request.Host+" "+request.URL.String())
		return jsonResponse(`{"responses":["lb-probe-1"]}`), nil
	})
	if finding.level != "PASS" {
		t.Fatalf("level = %s (%s), want PASS", finding.level, finding.detail)
	}
	for _, want := range []string{
		"probe.qa-core.k8s.test http://172.30.0.200/",
		"probe.qa-edge.k8s.test http://172.30.1.200/",
		"probe.qa-core.k8s.test http://172.30.0.200/dial?host=probe.qa-edge.k8s.test&port=80&protocol=http&request=hostname&tries=1",
		"probe.qa-edge.k8s.test http://172.30.1.200/dial?host=probe.qa-core.k8s.test&port=80&protocol=http&request=hostname&tries=1",
	} {
		found := false
		for _, got := range seen {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing probe request %q in %v", want, seen)
		}
	}
}

// The egress client honours HTTP_PROXY, which is right for factory.talos.dev
// and wrong for a cluster VIP: those addresses are host-local, so a proxied
// probe reports a dead path on a healthy host. The VIP client must never
// consult the environment.
func TestDoctorVIPHTTPClientIgnoresTheProxyEnvironment(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1/")
	t.Setenv("http_proxy", "http://127.0.0.1:1/")
	transport, ok := newDoctorVIPHTTPClient().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("VIP client transport = %T, want *http.Transport", newDoctorVIPHTTPClient().Transport)
	}
	if transport.Proxy != nil {
		request := &http.Request{Method: http.MethodGet, URL: &url.URL{Scheme: "http", Host: "172.30.0.200", Path: "/"}}
		proxy, err := transport.Proxy(request)
		t.Fatalf("VIP client proxies http://172.30.0.200/ via %v (err %v); it must dial the VIP directly", proxy, err)
	}
}
