package cluster

import (
	"strings"
	"testing"
)

func boolPtr(value bool) *bool { return &value }

func TestParseProvisioningIntent(t *testing.T) {
	tests := []struct {
		name                    string
		cni                     string
		csi                     string
		lb                      *bool
		bgp                     *bool
		hubble                  *bool
		kubeletMemoryProtection *bool
		want                    ProvisioningIntent
		wantErr                 []string
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
			wantErr: []string{"cni must be one of cilium or flannel"},
		},
		{
			name: "longhorn storage intent",
			cni:  "cilium",
			csi:  "longhorn",
			want: ProvisioningIntent{CNI: CNICilium, CSI: CSILonghorn, LB: true},
		},
		{
			name: "local path storage intent",
			cni:  "flannel",
			csi:  "local-path",
			want: ProvisioningIntent{CNI: CNIFlannel, CSI: CSILocalPath, LB: true},
		},
		{
			name:    "unknown csi lists curated engines",
			cni:     "cilium",
			csi:     "rook",
			wantErr: []string{"csi must be one of longhorn | local-path", `got "rook"`},
		},
		{
			name: "csi without cni names both remedies",
			csi:  "longhorn",
			wantErr: []string{
				"csi requires cni",
				"add cni:",
				"install storage yourself from the printed manifests",
			},
		},
		{
			name:    "load balancer without cni even when false",
			lb:      boolPtr(false),
			wantErr: []string{"lb requires cni"},
		},
		{
			name:    "bgp without cni",
			bgp:     boolPtr(true),
			wantErr: []string{"bgp requires cni"},
		},
		{
			name:    "hubble without cni",
			hubble:  boolPtr(false),
			wantErr: []string{"hubble requires cni"},
		},
		{
			name:                    "kubelet memory protection without cni",
			kubeletMemoryProtection: boolPtr(false),
			wantErr:                 []string{"kubeletMemoryProtection requires cni"},
		},
		{
			name:    "bgp requires load balancer",
			cni:     "cilium",
			lb:      boolPtr(false),
			bgp:     boolPtr(true),
			wantErr: []string{"bgp requires lb: true"},
		},
		{
			name:    "bgp requires cilium",
			cni:     "flannel",
			bgp:     boolPtr(true),
			wantErr: []string{"bgp requires cni: cilium"},
		},
		{
			name:    "hubble requires cilium",
			cni:     "flannel",
			hubble:  boolPtr(true),
			wantErr: []string{"hubble requires cni: cilium"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseProvisioningIntent(tt.cni, tt.csi, tt.lb, tt.bgp, tt.hubble, tt.kubeletMemoryProtection)
			if len(tt.wantErr) > 0 {
				if err == nil {
					t.Fatalf("ParseProvisioningIntent() error = nil, want containing %q", tt.wantErr)
				}
				for _, part := range tt.wantErr {
					if !strings.Contains(err.Error(), part) {
						t.Fatalf("ParseProvisioningIntent() error = %v, want containing %q", err, part)
					}
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

func TestParseProvisioningIntentDefaultsKubeletMemoryProtectionOn(t *testing.T) {
	intent, err := (ProvisioningIntentInput{CNI: string(CNICilium)}).Intent()
	if err != nil {
		t.Fatal(err)
	}
	if intent.DisableKubeletMemoryProtection {
		t.Fatalf("default provisioning intent = %+v, want kubelet memory protection enabled", intent)
	}
}

func TestParseProvisioningIntentAllowsKubeletMemoryProtectionOptOut(t *testing.T) {
	intent, err := (ProvisioningIntentInput{
		CNI:                     string(CNICilium),
		KubeletMemoryProtection: boolPtr(false),
	}).Intent()
	if err != nil {
		t.Fatal(err)
	}
	if !intent.DisableKubeletMemoryProtection {
		t.Fatalf("opt-out provisioning intent = %+v, want kubelet memory protection disabled", intent)
	}
	input := intent.Input()
	if input.KubeletMemoryProtection == nil || *input.KubeletMemoryProtection {
		t.Fatalf("durable opt-out input = %+v, want explicit kubeletMemoryProtection false", input)
	}
}
