package provision

import (
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
)

func TestRenderInspectionMatchesCiliumProvisioningInputs(t *testing.T) {
	item := cluster.Cluster{Name: "demo", SubnetIndex: 4, ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true, BGP: true, Hubble: true}}
	objects, err := renderCilium(item)
	if err != nil {
		t.Fatal(err)
	}
	namespaces, chart, extras, probe := partitionCiliumObjects(objects)
	wantObjects, err := encodeInspectionObjects(append(namespaces, chart...))
	if err != nil {
		t.Fatal(err)
	}
	wantExtras, err := encodeInspectionObjects(append(extras, probe...))
	if err != nil {
		t.Fatal(err)
	}
	for section, want := range map[string]string{"objects": wantObjects, "extras": wantExtras} {
		got, err := RenderInspection(item, section)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("Cilium %s differs from reconciler render", section)
		}
	}
	machine, err := RenderInspection(item, "machine")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"name: none", "proxy:\n    disabled: true", "RegistryMirrorConfig", "http://172.30.4.1:5059"} {
		if !strings.Contains(machine, want) {
			t.Fatalf("machine patch missing %q:\n%s", want, machine)
		}
	}
}

func TestRenderInspectionMatchesMetalLBProvisioningInputs(t *testing.T) {
	item := cluster.Cluster{Name: "demo", SubnetIndex: 5, ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true}}
	objects, err := renderMetalLB(item)
	if err != nil {
		t.Fatal(err)
	}
	namespaces, chart, crds, extras, probe := partitionMetalLBObjects(objects)
	wantObjects, err := encodeInspectionObjects(append(append(namespaces, crds...), chart...))
	if err != nil {
		t.Fatal(err)
	}
	wantExtras, err := encodeInspectionObjects(append(extras, probe...))
	if err != nil {
		t.Fatal(err)
	}
	for section, want := range map[string]string{"objects": wantObjects, "extras": wantExtras} {
		got, err := RenderInspection(item, section)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("MetalLB %s differs from reconciler render", section)
		}
	}
	machine, err := RenderInspection(item, "machine")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(machine, "name: flannel") || strings.Contains(machine, "proxy:") {
		t.Fatalf("flannel machine patch = %q", machine)
	}
}

func TestRenderInspectionFlannelWithoutLoadBalancerIsHandApplicable(t *testing.T) {
	item := cluster.Cluster{Name: "demo", SubnetIndex: 6, ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel}}
	all, err := RenderInspection(item, "all")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(all, "name: flannel") || strings.Contains(all, "metallb") || strings.Contains(all, "cilium") {
		t.Fatalf("flannel non-LB inspection = %s", all)
	}
}

func TestPinnedChartRenderingIsStableAcrossRepeatedInspections(t *testing.T) {
	for _, item := range []cluster.Cluster{
		{Name: "cilium", SubnetIndex: 7, ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true, Hubble: true}},
		{Name: "flannel", SubnetIndex: 8, ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true}},
	} {
		first, err := RenderInspection(item, "objects")
		if err != nil {
			t.Fatal(err)
		}
		second, err := RenderInspection(item, "objects")
		if err != nil {
			t.Fatal(err)
		}
		if first != second {
			t.Fatalf("%s chart render is not stable", item.CNI)
		}
	}
}

func TestRenderInspectionPreservesLegacySections(t *testing.T) {
	cilium := cluster.Cluster{Name: "demo", SubnetIndex: 9, ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true, BGP: true}}
	for section, want := range map[string]string{
		"mirrors": "RegistryMirrorConfig",
		"lb-pool": "CiliumLoadBalancerIPPool",
		"bgp":     "CiliumBGPClusterConfig",
	} {
		got, err := RenderInspection(cilium, section)
		if err != nil {
			t.Fatalf("legacy section %s: %v", section, err)
		}
		if !strings.Contains(got, want) {
			t.Fatalf("legacy section %s missing %q:\n%s", section, want, got)
		}
	}
	if _, err := RenderInspection(cilium, "balloon"); err == nil || !strings.Contains(err.Error(), "deprecated") {
		t.Fatalf("legacy balloon error = %v, want targeted deprecation", err)
	}
}

func TestRenderInspectionIndependentSections(t *testing.T) {
	item := cluster.Cluster{Name: "demo", SubnetIndex: 2, ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true}}
	machine, err := RenderInspection(item, "machine")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(machine, "DaemonSet") || !strings.Contains(machine, "RegistryMirrorConfig") {
		t.Fatalf("machine section unexpectedly contains chart output:\n%s", machine)
	}
	values, err := RenderInspection(item, "values")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(values, "apiVersion:") || !strings.Contains(values, "kubeProxyReplacement") {
		t.Fatalf("values section unexpectedly contains rendered objects:\n%s", values)
	}
}

func TestInspectionAllWarnsThatMixedOutputIsNotDirectlyApplicable(t *testing.T) {
	item := cluster.Cluster{Name: "demo", SubnetIndex: 2, ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true}}
	all, err := RenderInspection(item, "all")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Inspection bundle only", "Do not pipe it wholesale", "run the command above each section"} {
		if !strings.Contains(all, want) {
			t.Fatalf("all inspection missing safety guidance %q:\n%s", want, all)
		}
	}
}

func TestProvisioningNarrationUsesExactInspectionSections(t *testing.T) {
	item := cluster.Cluster{Name: "demo", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true}}
	for stage, narration := range map[string][]string{
		"Cilium":  ciliumNarration(item, true),
		"MetalLB": metalLBNarration(item),
	} {
		joined := strings.Join(narration, "\n")
		for _, want := range []string{
			"tbx manifests demo objects | kubectl apply --server-side -f -",
			"tbx manifests demo extras | kubectl apply --server-side -f -",
		} {
			if !strings.Contains(joined, want) {
				t.Fatalf("%s narration missing exact manual equivalent %q:\n%s", stage, want, joined)
			}
		}
		if strings.Contains(joined, "helm template") {
			t.Fatalf("%s narration bypasses curated values:\n%s", stage, joined)
		}
	}
}
