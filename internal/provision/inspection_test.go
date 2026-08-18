package provision

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/manifests"
	"go.yaml.in/yaml/v4"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
	for _, want := range []string{"name: none", "proxy:\n    disabled: true", "- name: virtio_balloon", "RegistryMirrorConfig", "http://172.30.4.1:5059"} {
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

func TestRenderInspectionStoragePrerequisitesAreAvailableWithoutCuratedIntent(t *testing.T) {
	item := cluster.Cluster{Name: "demo", SubnetIndex: 6}
	storage, err := RenderInspection(item, "storage")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# Storage machine-config prerequisite patch",
		manifests.StoragePrerequisiteKubeletMounts(),
		"talosctl gen config demo https://<cp-ip>:6443 --config-patch @storage-machine.yaml",
		"talosctl apply-config --insecure",
		"talosctl patch mc -p @storage-machine.yaml --nodes <node-ip>",
		"kubectl create namespace <your-csi-namespace> --dry-run=client -o yaml | kubectl apply -f -",
		"pod-security.kubernetes.io/enforce=privileged",
		"<your-csi-namespace>",
	} {
		if !strings.Contains(storage, want) {
			t.Fatalf("substrate-only storage inspection missing %q:\n%s", want, storage)
		}
	}
	if strings.Index(storage, "talosctl gen config") > strings.Index(storage, "talosctl patch mc") {
		t.Fatalf("storage inspection does not put the unconfigured-node branch before the patch branch:\n%s", storage)
	}
	if strings.Index(storage, "talosctl patch mc") > strings.Index(storage, "kubectl create namespace") || strings.Index(storage, "kubectl create namespace") > strings.Index(storage, "kubectl label namespace") {
		t.Fatalf("storage inspection does not order node patching, namespace creation, and PSA labeling:\n%s", storage)
	}
	all, err := RenderInspection(item, "all")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(all, storage) {
		t.Fatalf("all inspection omits substrate-only storage section:\n%s", all)
	}
}

// TestRenderInspectionSubstrateSectionsNeedNoCuratedCNI is the fork surface the
// inspection exists for: the machine patch and the catch-all registry mirror are
// substrate, so a hand-bootstrapping user gets them without declaring a CNI.
func TestRenderInspectionSubstrateSectionsNeedNoCuratedCNI(t *testing.T) {
	item := cluster.Cluster{Name: "demo", SubnetIndex: 7}
	for section, wants := range map[string][]string{
		"machine": {"name: none", "- name: virtio_balloon", "RegistryMirrorConfig", "http://172.30.7.1:5059", "skipFallback: true"},
		"talos":   {"name: none", "RegistryMirrorConfig"},
		"mirrors": {"RegistryMirrorConfig", `name: "*"`, "http://172.30.7.1:5059", "skipFallback: true"},
	} {
		got, err := RenderInspection(item, section)
		if err != nil {
			t.Fatalf("RenderInspection(%q) on a substrate-only cluster: %v", section, err)
		}
		for _, want := range wants {
			if !strings.Contains(got, want) {
				t.Errorf("substrate-only %s section missing %q:\n%s", section, want, got)
			}
		}
	}
	machine, err := RenderInspection(item, "machine")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(machine, "proxy:") {
		t.Fatalf("substrate-only machine patch disables kube-proxy without a curated CNI:\n%s", machine)
	}
}

// TestRenderInspectionMachineSectionCarriesBalloonModule pins SPEC §8: the
// printed config snippets MUST include the virtio_balloon kernel module. The
// `balloon` section is deprecated and redirects here, so the `machine` section
// (and the `all` bundle that embeds it) has to carry the snippet itself.
func TestRenderInspectionMachineSectionCarriesBalloonModule(t *testing.T) {
	for _, item := range []cluster.Cluster{
		{Name: "demo", SubnetIndex: 4, ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true}},
		{Name: "demo", SubnetIndex: 5, ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel}},
		{Name: "demo", SubnetIndex: 7},
	} {
		for _, section := range []string{"machine", "all"} {
			got, err := RenderInspection(item, section)
			if err != nil {
				t.Fatalf("RenderInspection(%q) for cni %q: %v", section, item.CNI, err)
			}
			for _, want := range []string{"machine:", "kernel:", "modules:", "- name: virtio_balloon"} {
				if !strings.Contains(got, want) {
					t.Errorf("cni %q %s section missing %q:\n%s", item.CNI, section, want, got)
				}
			}
		}
	}

	// The deprecation redirect stays: `balloon` errors and names `machine`.
	if _, err := RenderInspection(cluster.Cluster{Name: "demo"}, "balloon"); err == nil || !strings.Contains(err.Error(), "use machine") {
		t.Fatalf("RenderInspection(balloon) error = %v, want it to redirect to machine", err)
	}
}

// TestRenderInspectionMachineSectionIsValidYAML guards the merge of the balloon
// snippet into the machine patch: every printed document must still parse.
func TestRenderInspectionMachineSectionIsValidYAML(t *testing.T) {
	item := cluster.Cluster{Name: "demo", SubnetIndex: 4, ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium}}
	machine, err := RenderInspection(item, "machine")
	if err != nil {
		t.Fatal(err)
	}
	decoder := yaml.NewDecoder(strings.NewReader(machine))
	documents := 0
	for {
		var document map[string]any
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("machine section document %d does not parse: %v\n%s", documents, err, machine)
		}
		documents++
	}
	if documents != 2 {
		t.Fatalf("machine section = %d documents, want 2 (patch + mirror):\n%s", documents, machine)
	}
}

// TestRenderInspectionCNIDerivedSectionsNameTheRemedy keeps the refusal for the
// sections that genuinely come out of a curated CNI's charts, and makes it point
// at the flag that renders them.
func TestRenderInspectionCNIDerivedSectionsNameTheRemedy(t *testing.T) {
	item := cluster.Cluster{Name: "demo"}
	for _, section := range []string{
		"values", "objects", "extras", "k8s",
		"cilium-values", "metallb-values", "metallb-extras",
		// bgp is deliberately absent: --cni cannot render it, so it names
		// its own remedy (see TestRenderInspectionBGPSectionNamesTheDeclaredIntent).
		"lb-pool", "l2",
	} {
		_, err := RenderInspection(item, section)
		if err == nil {
			t.Errorf("RenderInspection(%q) on a substrate-only cluster = nil error", section)
			continue
		}
		for _, want := range []string{`cluster "demo" does not declare a curated cni`, "--cni cilium", "--cni flannel"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("RenderInspection(%q) error = %v, want it to mention %q", section, err, want)
			}
		}
	}
}

// TestRenderInspectionBGPSectionNamesTheDeclaredIntent: bgp announcements are a
// declared intent, not a CNI choice, so the refusal must never recommend --cni —
// the flag it used to recommend dead-ended on the very next command.
func TestRenderInspectionBGPSectionNamesTheDeclaredIntent(t *testing.T) {
	substrate := cluster.Cluster{Name: "qa-a", SubnetIndex: 4}
	cilium := cluster.Cluster{Name: "qa-a", SubnetIndex: 4, ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true}}
	flannel := cluster.Cluster{Name: "qa-a", SubnetIndex: 4, ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true}}
	for _, testCase := range []struct {
		name string
		item cluster.Cluster
		cni  string
		want []string
	}{
		{name: "substrate-only", item: substrate, want: []string{"bgp", "tbx cluster create", "--bgp"}},
		{name: "substrate-only under the cni override", item: substrate, cni: "cilium", want: []string{"bgp", "tbx cluster create", "--bgp"}},
		{name: "cilium without bgp", item: cilium, want: []string{"tbx bgp enable qa-a", "l2"}},
		{name: "cilium without load balancer", item: cluster.Cluster{Name: "qa-a", SubnetIndex: 4, ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium}}, want: []string{"tbx cluster create", "--bgp", "lb defaults to true"}},
		{name: "flannel", item: flannel, want: []string{"cilium"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := RenderInspectionWithCNI(testCase.item, "bgp", testCase.cni)
			if err == nil {
				t.Fatal("bgp section rendered for a cluster that declares no bgp")
			}
			// The manifests --cni override renders the curated path a cluster
			// is created with, which announces over l2: recommending it for
			// bgp is the dead end this test guards against. Naming create's
			// own --cni cilium --bgp pair is a different, workable remedy.
			if strings.Contains(err.Error(), "--cni cilium or --cni flannel") ||
				strings.Contains(err.Error(), "pass --cni") {
				t.Fatalf("bgp refusal recommends the manifests --cni override, which cannot render it: %v", err)
			}
			for _, want := range testCase.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("bgp refusal = %v, want it to mention %q", err, want)
				}
			}
		})
	}
}

// TestRenderInspectionBGPSectionStillRendersForADeclaredBGPCluster guards the
// refusal against over-reach.
func TestRenderInspectionBGPSectionStillRendersForADeclaredBGPCluster(t *testing.T) {
	item := cluster.Cluster{Name: "qa-a", SubnetIndex: 4, ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true, BGP: true}}
	got, err := RenderInspectionWithCNI(item, "bgp", "cilium")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "CiliumBGPClusterConfig") {
		t.Fatalf("bgp section = %q, want the BGP cluster config", got)
	}
}

// TestRenderInspectionCNIOverrideRendersTheNamedPath is the "what would tbx
// apply" question a substrate-only user asks before installing a CNI by hand.
func TestRenderInspectionCNIOverrideRendersTheNamedPath(t *testing.T) {
	item := cluster.Cluster{Name: "demo", SubnetIndex: 4}
	cilium := cluster.Cluster{Name: "demo", SubnetIndex: 4, ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true}}
	for _, section := range []string{"values", "objects", "extras", "lb-pool", "l2", "machine"} {
		got, err := RenderInspectionWithCNI(item, section, "cilium")
		if err != nil {
			t.Fatalf("RenderInspectionWithCNI(%q, cilium): %v", section, err)
		}
		want, err := RenderInspection(cilium, section)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("--cni cilium %s section differs from a declared Cilium cluster", section)
		}
	}
	values, err := RenderInspectionWithCNI(item, "values", "flannel")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(values, "l2Announcements") && !strings.Contains(values, "speaker") {
		t.Fatalf("--cni flannel values do not look like MetalLB values:\n%s", values)
	}
	if strings.Contains(values, "kubeProxyReplacement") {
		t.Fatalf("--cni flannel rendered Cilium values:\n%s", values)
	}
}

func TestRenderInspectionCNIOverrideMustMatchADeclaredCNI(t *testing.T) {
	item := cluster.Cluster{Name: "demo", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true}}
	if _, err := RenderInspectionWithCNI(item, "objects", "flannel"); err == nil || !strings.Contains(err.Error(), "declares cni cilium") {
		t.Fatalf("mismatched --cni error = %v, want it to name the declared cni", err)
	}
	matched, err := RenderInspectionWithCNI(item, "objects", "cilium")
	if err != nil {
		t.Fatal(err)
	}
	declared, err := RenderInspection(item, "objects")
	if err != nil {
		t.Fatal(err)
	}
	if matched != declared {
		t.Fatalf("--cni naming the declared cni changed the render")
	}
	if _, err := RenderInspectionWithCNI(item, "objects", "calico"); err == nil || !strings.Contains(err.Error(), "--cni must be one of cilium or flannel") {
		t.Fatalf("uncurated --cni error = %v, want the curated-set message", err)
	}
}

// TestRenderInspectionAllSubstrateOnlyRendersTheMachinePatchAndNamesTheFlag
// covers the bundle: it must carry the substrate sections rather than only the
// storage guidance, and say how to see the CNI-derived ones.
func TestRenderInspectionAllSubstrateOnlyRendersTheMachinePatchAndNamesTheFlag(t *testing.T) {
	item := cluster.Cluster{Name: "demo", SubnetIndex: 8}
	all, err := RenderInspection(item, "all")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# Machine-config prerequisite patch",
		"name: none",
		"RegistryMirrorConfig",
		"tbx manifests demo objects --cni cilium",
		"# Storage machine-config prerequisite patch",
	} {
		if !strings.Contains(all, want) {
			t.Fatalf("substrate-only all bundle missing %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "kubeProxyReplacement") || strings.Contains(all, "kind: DaemonSet") {
		t.Fatalf("substrate-only all bundle claims curated objects:\n%s", all)
	}
}

func TestRenderInspectionLonghornStreamsMatchRendererPartitionsAndOrder(t *testing.T) {
	item := cluster.Cluster{
		Name: "longhorn", Nodes: make([]cluster.Node, 3),
		ProvisioningIntent: cluster.ProvisioningIntent{CSI: cluster.CSILonghorn},
	}
	objects, err := renderLonghorn(item)
	if err != nil {
		t.Fatal(err)
	}
	namespaces, chartObjects, crds := partitionLonghornObjects(objects)
	for section, objects := range map[string][]unstructured.Unstructured{
		"storage-namespaces": namespaces,
		"storage-crds":       crds,
		"storage-objects":    chartObjects,
	} {
		want, err := encodeInspectionObjects(objects)
		if err != nil {
			t.Fatal(err)
		}
		got, err := RenderInspection(item, section)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s differs from Longhorn renderer partition", section)
		}
	}
	values, err := RenderInspection(item, "storage-values")
	if err != nil {
		t.Fatal(err)
	}
	if values != longhornValues(3, 3, true) {
		t.Fatalf("storage values differ from Longhorn renderer input")
	}
	bundle, err := RenderInspection(item, "storage")
	if err != nil {
		t.Fatal(err)
	}
	commands := []string{
		"tbx manifests longhorn storage-namespaces | kubectl apply --server-side -f -",
		"tbx manifests longhorn storage-crds | kubectl apply --server-side -f -",
		"tbx manifests longhorn storage-crds | kubectl wait --for=condition=Established --timeout=120s -f -",
		"tbx manifests longhorn storage-objects | kubectl apply --server-side -f -",
	}
	previous := -1
	for _, command := range commands {
		index := strings.Index(bundle, command)
		if index < 0 {
			t.Fatalf("storage inspection missing command %q:\n%s", command, bundle)
		}
		if index <= previous {
			t.Fatalf("storage inspection command %q is out of order:\n%s", command, bundle)
		}
		previous = index
	}
	if !strings.Contains(bundle, "Do not combine them into a one-shot apply") {
		t.Fatalf("storage inspection implies a one-shot Longhorn apply:\n%s", bundle)
	}
	all, err := RenderInspection(item, "all")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(all, bundle) {
		t.Fatalf("all inspection omits ordered Longhorn storage instructions:\n%s", all)
	}
}

func TestRenderInspectionLocalPathStreamsMatchRendererPartitions(t *testing.T) {
	item := cluster.Cluster{Name: "local-path", ProvisioningIntent: cluster.ProvisioningIntent{CSI: cluster.CSILocalPath}}
	objects, err := renderLocalPath(item)
	if err != nil {
		t.Fatal(err)
	}
	namespaces, manifestObjects := partitionLocalPathObjects(objects)
	for section, objects := range map[string][]unstructured.Unstructured{
		"storage-namespaces": namespaces,
		"storage-objects":    manifestObjects,
	} {
		want, err := encodeInspectionObjects(objects)
		if err != nil {
			t.Fatal(err)
		}
		got, err := RenderInspection(item, section)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s differs from local-path renderer partition", section)
		}
	}
	if _, err := RenderInspection(item, "storage-crds"); err == nil || !strings.Contains(err.Error(), "does not declare Longhorn storage CRDs") {
		t.Fatalf("local-path storage-crds error = %v, want targeted error", err)
	}
	bundle, err := RenderInspection(item, "storage")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bundle, "Local-path has no CRD barrier") {
		t.Fatalf("local-path storage inspection does not explain its stream order:\n%s", bundle)
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

func TestInspectionAllQuotesClusterNameInEveryCommand(t *testing.T) {
	item := cluster.Cluster{Name: "demo; echo owned", SubnetIndex: 2, ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true}}
	all, err := RenderInspection(item, "all")
	if err != nil {
		t.Fatal(err)
	}
	for _, section := range []string{"machine", "values", "objects", "extras"} {
		want := "tbx manifests 'demo; echo owned' " + section
		if !strings.Contains(all, want) {
			t.Fatalf("all inspection missing quoted command %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "tbx manifests demo; echo owned") {
		t.Fatalf("all inspection contains unquoted cluster name:\n%s", all)
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

func TestProvisioningNarrationQuotesClusterName(t *testing.T) {
	item := cluster.Cluster{Name: "demo; echo owned", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true}}
	for stage, narration := range map[string][]string{
		"Cilium":  ciliumNarration(item, true),
		"MetalLB": metalLBNarration(item),
	} {
		joined := strings.Join(narration, "\n")
		for _, section := range []string{"objects", "extras"} {
			want := "tbx manifests 'demo; echo owned' " + section + " | kubectl apply --server-side -f -"
			if !strings.Contains(joined, want) {
				t.Fatalf("%s narration missing quoted command %q:\n%s", stage, want, joined)
			}
		}
	}
}

func TestCiliumValuesRefusalNamesTheRunnableCommand(t *testing.T) {
	item := cluster.Cluster{Name: "demo", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true}}
	_, err := RenderInspection(item, "cilium-values")
	if err == nil || !strings.Contains(err.Error(), "tbx manifests demo values") {
		t.Fatalf("cilium-values refusal = %v, want the runnable redirect", err)
	}

	// A name with shell metacharacters must arrive quoted, so the printed
	// command stays copy-pasteable and inert.
	item.Name = "de mo"
	_, err = RenderInspection(item, "cilium-values")
	if err == nil || !strings.Contains(err.Error(), "tbx manifests 'de mo' values") {
		t.Fatalf("cilium-values refusal with unsafe name = %v, want the quoted redirect", err)
	}
}
