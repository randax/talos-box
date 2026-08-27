//go:build linux

package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
)

func TestParseIPRouteInterface(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		want    string
		wantErr bool
	}{
		{
			name:   "bridge route",
			output: "172.30.3.2 dev br-tbx3 src 172.30.3.1 uid 1000 \\    cache \n",
			want:   "br-tbx3",
		},
		{
			name:   "local gateway",
			output: "local 172.30.3.1 dev lo src 172.30.3.1 uid 1000 \\    cache <local> \n",
			want:   "lo",
		},
		{
			name:   "vpn capture",
			output: "172.30.3.2 via 10.0.0.1 dev utun6 src 10.0.0.7 uid 1000 \\    cache \n",
			want:   "utun6",
		},
		{name: "no device", output: "172.30.3.2 \n", wantErr: true},
		{name: "empty", output: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseIPRouteInterface([]byte(tt.output))
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseIPRouteInterface() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("parseIPRouteInterface() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The Linux probe must never exec a macOS binary: it asks `ip route get` and
// accepts the talosbox bridge, not `bridge*`/`vmnet*` (#468).
func TestLinuxRouteProbeUsesIPRouteAndTheBridge(t *testing.T) {
	clusters := []daemon.ClusterSummary{{Name: "demo", SubnetIndex: 3, Running: true}}
	statuses := []daemon.ClusterStatus{{
		Name:    "demo",
		Running: true,
		Nodes:   []daemon.NodeStatus{{Name: "demo-cp-1", IP: "172.30.3.2"}},
	}}
	var calls []string
	command := func(name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if name != "ip" {
			t.Fatalf("route probe execed %q; Linux must use `ip route get`", name)
		}
		target := args[len(args)-1]
		if target == "172.30.3.1" {
			return []byte("local 172.30.3.1 dev lo src 172.30.3.1 \n"), nil
		}
		return []byte(fmt.Sprintf("%s dev br-tbx3 src 172.30.3.1 \n", target)), nil
	}
	if err := checkClusterRoutes(clusters, statuses, platformRouteProbe(command)); err != nil {
		t.Fatalf("checkClusterRoutes() = %v", err)
	}
	want := []string{"ip -o route get 172.30.3.1", "ip -o route get 172.30.3.2"}
	if fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Fatalf("route commands = %v, want %v", calls, want)
	}

	captured := func(name string, args ...string) ([]byte, error) {
		_ = name
		return []byte(fmt.Sprintf("%s via 10.0.0.1 dev wlan0 src 10.0.0.7 \n", args[len(args)-1])), nil
	}
	if err := checkClusterRoutes(clusters, statuses, platformRouteProbe(captured)); err == nil {
		t.Fatal("checkClusterRoutes() succeeded for a route leaving via wlan0")
	}
}
