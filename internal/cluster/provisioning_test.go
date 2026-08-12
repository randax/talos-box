package cluster

import (
	"strings"
	"testing"
)

func boolPtr(value bool) *bool { return &value }

func TestParseProvisioningIntent(t *testing.T) {
	tests := []struct {
		name    string
		cni     string
		lb      *bool
		bgp     *bool
		hubble  *bool
		want    ProvisioningIntent
		wantErr string
	}{
		{
			name: "cilium defaults load balancer on",
			cni:  "cilium",
			want: ProvisioningIntent{CNI: CNICilium, LB: true},
		},
		{
			name: "flannel preserves explicit load balancer off",
			cni:  "flannel",
			lb:   boolPtr(false),
			want: ProvisioningIntent{CNI: CNIFlannel},
		},
		{
			name:   "cilium preserves optional features",
			cni:    "cilium",
			bgp:    boolPtr(true),
			hubble: boolPtr(true),
			want:   ProvisioningIntent{CNI: CNICilium, LB: true, BGP: true, Hubble: true},
		},
		{
			name:    "unknown cni",
			cni:     "calico",
			wantErr: "cni must be one of cilium or flannel",
		},
		{
			name:    "load balancer without cni even when false",
			lb:      boolPtr(false),
			wantErr: "lb requires cni",
		},
		{
			name:    "bgp without cni",
			bgp:     boolPtr(true),
			wantErr: "bgp requires cni",
		},
		{
			name:    "hubble without cni",
			hubble:  boolPtr(false),
			wantErr: "hubble requires cni",
		},
		{
			name:    "bgp requires load balancer",
			cni:     "cilium",
			lb:      boolPtr(false),
			bgp:     boolPtr(true),
			wantErr: "bgp requires lb: true",
		},
		{
			name:    "bgp requires cilium",
			cni:     "flannel",
			bgp:     boolPtr(true),
			wantErr: "bgp requires cni: cilium",
		},
		{
			name:    "hubble requires cilium",
			cni:     "flannel",
			hubble:  boolPtr(true),
			wantErr: "hubble requires cni: cilium",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseProvisioningIntent(tt.cni, tt.lb, tt.bgp, tt.hubble)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ParseProvisioningIntent() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("ParseProvisioningIntent() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
