package main

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

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
		{Name: "qa-core", Running: true, VIP: "172.30.0.200", VIPLive: true},
		{Name: "qa-edge", Running: true, VIP: "172.30.1.200", VIPLive: true},
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
