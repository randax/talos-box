package provision

import (
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func ciliumClusterForBGPRender(t *testing.T, bgp bool) cluster.Cluster {
	t.Helper()
	item, err := cluster.New("demo", 3, 1, 1, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true, BGP: bgp}
	return item
}

func findRenderedObject(t *testing.T, objects []unstructured.Unstructured, kind, name string) unstructured.Unstructured {
	t.Helper()
	for _, object := range objects {
		if object.GetKind() == kind && object.GetName() == name {
			return object
		}
	}
	t.Fatalf("rendered objects have no %s %q", kind, name)
	return unstructured.Unstructured{}
}

// Enabling BGP on a live cluster re-renders this chart, and the render is what
// has to carry the whole cluster-side change: the agents' feature flag, the CRDs
// the announcement objects are instances of, and a pod-template change that
// makes the DaemonSet actually roll. Without the last one the flag would sit in
// the ConfigMap while the running agents kept BGP disabled (#344).
func TestRenderCiliumBGPEnablesTheControlPlaneAndRollsTheAgents(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	l2Objects, err := renderCilium(ciliumClusterForBGPRender(t, false))
	if err != nil {
		t.Fatal(err)
	}
	bgpObjects, err := renderCilium(ciliumClusterForBGPRender(t, true))
	if err != nil {
		t.Fatal(err)
	}

	l2Config := findRenderedObject(t, l2Objects, "ConfigMap", "cilium-config")
	bgpConfig := findRenderedObject(t, bgpObjects, "ConfigMap", "cilium-config")
	if enabled, _, _ := unstructured.NestedString(bgpConfig.Object, "data", "enable-bgp-control-plane"); enabled != "true" {
		t.Fatalf("BGP render's enable-bgp-control-plane = %q, want true", enabled)
	}
	if enabled, _, _ := unstructured.NestedString(l2Config.Object, "data", "enable-bgp-control-plane"); enabled == "true" {
		t.Fatal("L2 render already enables the BGP control plane, so the mode change would be unobservable")
	}

	// The agent DaemonSet carries a checksum of that ConfigMap in its pod
	// template, which is what turns the server-side apply into a rollout.
	const checksum = "cilium.io/cilium-configmap-checksum"
	l2Agent := findRenderedObject(t, l2Objects, "DaemonSet", "cilium")
	bgpAgent := findRenderedObject(t, bgpObjects, "DaemonSet", "cilium")
	l2Sum, _, _ := unstructured.NestedString(l2Agent.Object, "spec", "template", "metadata", "annotations", checksum)
	bgpSum, _, _ := unstructured.NestedString(bgpAgent.Object, "spec", "template", "metadata", "annotations", checksum)
	if l2Sum == "" || bgpSum == "" {
		t.Fatalf("agent pod template checksums = %q/%q, want both set", l2Sum, bgpSum)
	}
	if l2Sum == bgpSum {
		t.Fatal("agent pod template is identical across the mode change, so applying it would not roll the agents")
	}

	// The announcement objects ride along with the render, so the same apply that
	// flips the flag also asks for the mode. Their CRDs are created by the
	// operator once the control plane is on — which is what waitForCiliumCRDs
	// holds the pass on — so the chart itself ships none of them (#295).
	for _, kind := range []string{"CiliumBGPClusterConfig", "CiliumBGPPeerConfig", "CiliumBGPAdvertisement"} {
		found := false
		for _, object := range bgpObjects {
			if object.GetKind() == kind && strings.HasPrefix(object.GetName(), "demo-bgp") {
				found = true
			}
		}
		if !found {
			t.Errorf("BGP render carries no %s for the cluster", kind)
		}
	}
}
