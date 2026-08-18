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
		"defaultClassReplicaCount: 2",
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

func TestRunManifestsSubstrateOnlyRendersMachineAndMirrors(t *testing.T) {
	for section, wants := range map[string][]string{
		"machine": {"name: none", "RegistryMirrorConfig", "http://172.30.5.1:5059"},
		"mirrors": {"RegistryMirrorConfig", "skipFallback: true"},
	} {
		stdout := runCLIWithResponse(t,
			`[{"name":"demo","subnetIndex":5,"controlPlanes":1,"workers":2}]`,
			func(command cli) error { return command.runManifests([]string{"demo", section}) },
		)
		for _, want := range wants {
			if !strings.Contains(stdout, want) {
				t.Errorf("substrate-only %s output missing %q:\n%s", section, want, stdout)
			}
		}
	}
}

func TestRunManifestsCNIDerivedSectionNamesTheFlag(t *testing.T) {
	var err error
	runCLIWithResponse(t,
		`[{"name":"demo","subnetIndex":5,"controlPlanes":1,"workers":2}]`,
		func(command cli) error {
			err = command.runManifests([]string{"demo", "objects"})
			return nil
		},
	)
	if err == nil || !strings.Contains(err.Error(), "--cni cilium") {
		t.Fatalf("substrate-only objects error = %v, want the --cni remedy", err)
	}
}

// TestRunManifestsRefusalsExitNonZero: every manifests refusal has to reach the
// shell as a failure, because the documented pipe pattern (`tbx manifests <c>
// objects | kubectl apply --server-side -f -`) otherwise feeds kubectl the
// error text instead of aborting, and scripted callers cannot detect it (#354).
func TestRunManifestsRefusalsExitNonZero(t *testing.T) {
	for _, section := range []string{"values", "objects", "extras", "k8s", "lb-pool", "l2", "cilium-values", "metallb-values", "bgp", "balloon"} {
		var err error
		stdout := runCLIWithResponse(t,
			`[{"name":"demo","subnetIndex":5,"controlPlanes":1,"workers":2}]`,
			func(command cli) error {
				err = command.runManifests([]string{"demo", section})
				return nil
			},
		)
		if err == nil {
			t.Errorf("tbx manifests demo %s = nil error, want a refusal", section)
		}
		if stdout != "" {
			t.Errorf("tbx manifests demo %s stdout = %q, want nothing for a pipe to consume", section, stdout)
		}
	}
}

func TestRunManifestsCNIFlagRendersTheNamedCuratedPath(t *testing.T) {
	stdout := runCLIWithResponse(t,
		`[{"name":"demo","subnetIndex":5,"controlPlanes":1,"workers":2}]`,
		func(command cli) error { return command.runManifests([]string{"demo", "values", "--cni", "cilium"}) },
	)
	if !strings.Contains(stdout, "kubeProxyReplacement: true") {
		t.Fatalf("tbx manifests --cni cilium values missing Cilium values:\n%s", stdout)
	}
}

// TestRunManifestsImagesRoundTripsThroughTheWarmList is the composability
// promise: `tbx manifests demo images | tbx cache warm -` must parse, so every
// emitted line is checked with the warm list's own parser.
func TestRunManifestsImagesRoundTripsThroughTheWarmList(t *testing.T) {
	stdout := runCLIWithResponse(t,
		`[{"name":"demo","subnetIndex":3,"controlPlanes":1,"workers":2,"talosVersion":"v1.13.6","schematic":"cafe1234","cni":"cilium","csi":"longhorn"}]`,
		func(command cli) error { return command.runManifests([]string{"demo", "images"}) },
	)
	entries, problems := parseWarmListSource("stdin", strings.NewReader(stdout))
	if len(problems) != 0 {
		t.Fatalf("images output is not a valid warm list: %v", problems)
	}
	if len(entries) == 0 {
		t.Fatalf("images output produced no warm entries:\n%s", stdout)
	}
	for _, want := range []string{
		"factory.talos.dev/metal-installer/cafe1234:v1.13.6",
		"docker.io/longhornio/longhorn-manager:v1.12.0",
	} {
		found := false
		for _, entry := range entries {
			if entry.Ref == want {
				found = true
			}
		}
		if !found {
			t.Errorf("images output missing %q:\n%s", want, stdout)
		}
	}
}

// TestRunManifestsImagesFollowsTheDeclaredIntent guards the export against
// claiming images an intent never installs.
func TestRunManifestsImagesFollowsTheDeclaredIntent(t *testing.T) {
	stdout := runCLIWithResponse(t,
		`[{"name":"demo","subnetIndex":3,"controlPlanes":1,"workers":2,"talosVersion":"v1.13.6","schematic":"cafe1234"}]`,
		func(command cli) error { return command.runManifests([]string{"demo", "images"}) },
	)
	if strings.Contains(stdout, "longhornio/") || strings.Contains(stdout, "quay.io/cilium/") {
		t.Fatalf("substrate-only images output claims curated images:\n%s", stdout)
	}
	if !strings.Contains(stdout, "registry.k8s.io/kube-proxy:") {
		t.Fatalf("substrate-only images output missing kube-proxy:\n%s", stdout)
	}
}
