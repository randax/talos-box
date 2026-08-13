package main

import (
	"strings"
	"testing"
)

func TestRunManifestsCiliumInspectionSurface(t *testing.T) {
	stdout := runCLIWithResponse(t,
		`[{"name":"demo","subnetIndex":3,"cni":"cilium","lb":true,"bgp":true,"hubble":true}]`,
		func(command cli) error { return command.runManifests([]string{"demo"}) },
	)
	for _, want := range []string{
		"# Machine-config prerequisite patch",
		"# Pinned Helm values used by tbx (release cilium in namespace kube-system):",
		"# Exact rendered chart objects tbx applies server-side (cilium/cilium 1.19.6, release cilium, namespace kube-system):",
		"# Exact LoadBalancer/BGP extras and probe tbx applies:",
		"kubeProxyReplacement: true",
		"kind: CiliumBGPClusterConfig",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("tbx manifests cilium output missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "metallb/metallb") {
		t.Fatalf("tbx manifests cilium output leaked MetalLB metadata:\n%s", stdout)
	}
}

func TestRunManifestsFlannelMetalLBInspectionSurface(t *testing.T) {
	stdout := runCLIWithResponse(t,
		`[{"name":"demo","subnetIndex":5,"cni":"flannel","lb":true}]`,
		func(command cli) error { return command.runManifests([]string{"demo"}) },
	)
	for _, want := range []string{
		"# Machine-config prerequisite patch",
		"# Pinned Helm values used by tbx (release metallb in namespace metallb-system):",
		"# Exact rendered chart objects tbx applies server-side (metallb/metallb 0.16.1, release metallb, namespace metallb-system):",
		"# Exact LoadBalancer/BGP extras and probe tbx applies:",
		"kind: CustomResourceDefinition",
		"kind: IPAddressPool",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("tbx manifests flannel output missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "cilium/cilium") || strings.Contains(stdout, "kubeProxyReplacement") {
		t.Fatalf("tbx manifests flannel output leaked Cilium metadata:\n%s", stdout)
	}
}

func TestRunManifestsStorageInspectionUsesClusterTopology(t *testing.T) {
	stdout := runCLIWithResponse(t,
		`[{"name":"demo","subnetIndex":5,"controlPlanes":1,"workers":2,"cni":"flannel","csi":"longhorn"}]`,
		func(command cli) error { return command.runManifests([]string{"demo", "storage"}) },
	)
	for _, want := range []string{
		"# Storage machine-config prerequisite patch",
		"defaultClassReplicaCount: 3",
		"kind: StorageClass",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("tbx manifests storage output missing %q:\n%s", want, stdout)
		}
	}
}

func TestRunManifestsSubstrateOnlyIncludesStorageGuidance(t *testing.T) {
	stdout := runCLIWithResponse(t,
		`[{"name":"demo","subnetIndex":5,"controlPlanes":1,"workers":2}]`,
		func(command cli) error { return command.runManifests([]string{"demo"}) },
	)
	for _, want := range []string{
		"# Storage machine-config prerequisite patch",
		"destination: /var/local-path-provisioner",
		"destination: /var/lib/longhorn",
		"kubectl label namespace <your-csi-namespace>",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("tbx manifests substrate-only output missing %q:\n%s", want, stdout)
		}
	}
}
