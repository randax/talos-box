package provision

import (
	"errors"
	"fmt"
	"strings"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/manifests"
	"go.yaml.in/yaml/v4"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// InspectionSections are the path-aware documents tbx manifests can expose.
// The legacy aliases remain accepted so existing shell snippets keep working.
func InspectionSections() []string {
	return []string{"all", "machine", "values", "objects", "extras", "talos", "cilium-values", "metallb-values", "metallb-extras", "k8s", "mirrors", "balloon", "lb-pool", "bgp", "l2"}
}

// RenderInspection renders the exact inputs consumed by the provisioning
// reconcilers. It is intentionally built from their render functions rather
// than parallel templates so a user can fork the output onto a substrate-only
// cluster without guessing what tbx would have applied.
func RenderInspection(item cluster.Cluster, section string) (string, error) {
	if item.CNI != cluster.CNICilium && item.CNI != cluster.CNIFlannel {
		return "", fmt.Errorf("cluster %q does not declare a curated cni", item.Name)
	}
	if section == "" {
		section = "all"
	}
	switch section {
	case "talos":
		section = "machine"
	case "k8s":
		section = "extras"
	case "cilium-values":
		if item.CNI != cluster.CNICilium {
			return "", fmt.Errorf("cluster %q uses flannel; use values", item.Name)
		}
		section = "values"
	case "metallb-values":
		if item.CNI != cluster.CNIFlannel || !item.LB {
			return "", fmt.Errorf("cluster %q has no MetalLB provisioning path", item.Name)
		}
		section = "values"
	case "metallb-extras":
		if item.CNI != cluster.CNIFlannel || !item.LB {
			return "", fmt.Errorf("cluster %q has no MetalLB extras", item.Name)
		}
		section = "extras"
	}

	switch section {
	case "machine":
		return machinePrerequisitePatch(item), nil
	case "values":
		return inspectionValues(item)
	case "mirrors":
		return catchAllMirrorDocument(item.SubnetIndex), nil
	case "balloon":
		return "", errors.New("section balloon is deprecated: curated provisioning does not apply a balloon patch; use machine")
	case "lb-pool", "bgp", "l2":
		return renderLegacyExtra(item, section)
	}

	inspection, err := inspectProvisioning(item)
	if err != nil {
		return "", err
	}
	switch section {
	case "all":
		return inspection.all(item.Name), nil
	case "objects":
		return inspection.objects, nil
	case "extras":
		return inspection.extras, nil
	default:
		return "", fmt.Errorf("unknown section %q (use %s)", section, strings.Join(InspectionSections(), ", "))
	}
}

func inspectionValues(item cluster.Cluster) (string, error) {
	switch item.CNI {
	case cluster.CNICilium:
		return manifests.CiliumValues(manifestFacts(item)), nil
	case cluster.CNIFlannel:
		if !item.LB {
			return "", fmt.Errorf("cluster %q has no Helm values because LoadBalancer support is disabled", item.Name)
		}
		return manifests.MetalLBValues(manifestFacts(item)), nil
	default:
		return "", fmt.Errorf("cluster %q does not declare a curated cni", item.Name)
	}
}

func renderLegacyExtra(item cluster.Cluster, section string) (string, error) {
	inspection, err := inspectProvisioning(item)
	if err != nil {
		return "", err
	}
	objects, err := decodeObjects([]byte(inspection.extras))
	if err != nil {
		return "", fmt.Errorf("decode rendered extras: %w", err)
	}
	wanted := map[string]bool{}
	switch section {
	case "lb-pool":
		wanted["CiliumLoadBalancerIPPool"] = true
		wanted["IPAddressPool"] = true
	case "bgp":
		wanted["CiliumBGPClusterConfig"] = true
		wanted["CiliumBGPPeerConfig"] = true
		wanted["CiliumBGPAdvertisement"] = true
	case "l2":
		wanted["CiliumL2AnnouncementPolicy"] = true
		wanted["L2Advertisement"] = true
	}
	filtered := make([]unstructured.Unstructured, 0, len(objects))
	for _, object := range objects {
		if wanted[object.GetKind()] {
			filtered = append(filtered, object)
		}
	}
	if len(filtered) == 0 {
		return "", fmt.Errorf("cluster %q has no %s extras in its declared provisioning path", item.Name, section)
	}
	return encodeInspectionObjects(filtered)
}

type inspection struct {
	machine string
	values  string
	objects string
	extras  string
	chart   inspectionChartMetadata
}

type inspectionChartMetadata struct {
	name      string
	version   string
	release   string
	namespace string
}

func inspectProvisioning(item cluster.Cluster) (inspection, error) {
	result := inspection{machine: machinePrerequisitePatch(item)}
	switch item.CNI {
	case cluster.CNICilium:
		objects, err := renderCilium(item)
		if err != nil {
			return inspection{}, err
		}
		result.chart = inspectionChartMetadata{
			name:      "cilium/cilium",
			version:   ciliumChartVersion,
			release:   "cilium",
			namespace: ciliumNamespace,
		}
		namespaces, chart, extras, probe := partitionCiliumObjects(objects)
		result.values = manifests.CiliumValues(manifestFacts(item))
		result.objects, err = encodeInspectionObjects(append(namespaces, chart...))
		if err != nil {
			return inspection{}, err
		}
		result.extras, err = encodeInspectionObjects(append(extras, probe...))
		if err != nil {
			return inspection{}, err
		}
	case cluster.CNIFlannel:
		if !item.LB {
			return result, nil
		}
		objects, err := renderMetalLB(item)
		if err != nil {
			return inspection{}, err
		}
		result.chart = inspectionChartMetadata{
			name:      "metallb/metallb",
			version:   metalLBChartVersion,
			release:   "metallb",
			namespace: metalLBNamespace,
		}
		namespaces, chart, crds, extras, probe := partitionMetalLBObjects(objects)
		result.values = manifests.MetalLBValues(manifestFacts(item))
		result.objects, err = encodeInspectionObjects(append(append(namespaces, crds...), chart...))
		if err != nil {
			return inspection{}, err
		}
		result.extras, err = encodeInspectionObjects(append(extras, probe...))
		if err != nil {
			return inspection{}, err
		}
	}
	return result, nil
}

func machinePrerequisitePatch(item cluster.Cluster) string {
	cniName := machineCNIName(item)
	patch := fmt.Sprintf("cluster:\n  network:\n    cni:\n      name: %s\n", cniName)
	if ciliumDisablesKubeProxy(item) {
		patch += "  proxy:\n    disabled: true\n"
	}
	return patch + catchAllMirrorDocument(item.SubnetIndex)
}

func encodeInspectionObjects(objects []unstructured.Unstructured) (string, error) {
	if len(objects) == 0 {
		return "", nil
	}
	var documents []string
	for _, object := range objects {
		data, err := yaml.Marshal(object.Object)
		if err != nil {
			return "", fmt.Errorf("encode %s %q: %w", object.GetKind(), object.GetName(), err)
		}
		documents = append(documents, string(data))
	}
	return strings.Join(documents, "---\n"), nil
}

func (i inspection) all(clusterName string) string {
	var sections []string
	sections = append(sections, fmt.Sprintf("# Machine-config prerequisite patch (apply before bootstrap):\n#   tbx manifests %s machine\n%s", clusterName, i.machine))
	if i.values != "" {
		sections = append(sections, fmt.Sprintf("# Pinned Helm values used by tbx (release %s in namespace %s):\n#   tbx manifests %s values > values.yaml\n%s", i.chart.release, i.chart.namespace, clusterName, i.values))
	}
	if i.objects != "" {
		sections = append(sections, fmt.Sprintf("# Exact rendered chart objects tbx applies server-side (%s %s, release %s, namespace %s):\n#   tbx manifests %s objects | kubectl apply --server-side -f -\n%s", i.chart.name, i.chart.version, i.chart.release, i.chart.namespace, clusterName, i.objects))
	}
	if i.extras != "" {
		sections = append(sections, fmt.Sprintf("# Exact LoadBalancer/BGP extras and probe tbx applies:\n#   tbx manifests %s extras | kubectl apply --server-side -f -\n%s", clusterName, i.extras))
	}
	return strings.Join(sections, "---\n")
}
