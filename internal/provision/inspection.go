package provision

import (
	"errors"
	"fmt"
	"strings"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/manifests"
	"github.com/randax/talos-box/internal/shellquote"
	"go.yaml.in/yaml/v4"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// InspectionSections are the path-aware documents tbx manifests can expose.
// The legacy aliases remain accepted so existing shell snippets keep working.
func InspectionSections() []string {
	return []string{"all", "machine", "values", "objects", "extras", "storage", "storage-machine", "storage-values", "storage-namespaces", "storage-crds", "storage-objects", "talos", "cilium-values", "metallb-values", "metallb-extras", "k8s", "mirrors", "balloon", "lb-pool", "bgp", "l2"}
}

// RenderInspection renders the exact inputs consumed by the provisioning
// reconcilers. It is intentionally built from their render functions rather
// than parallel templates so a user can fork the output onto a substrate-only
// cluster without guessing what tbx would have applied.
func RenderInspection(item cluster.Cluster, section string) (string, error) {
	if section == "" {
		section = "all"
	}
	if section != "all" && !isStorageInspectionSection(section) && item.CNI != cluster.CNICilium && item.CNI != cluster.CNIFlannel {
		return "", fmt.Errorf("cluster %q does not declare a curated cni", item.Name)
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
	case "storage":
		return storageInspection(item)
	case "storage-machine":
		return manifests.StoragePrerequisiteKubeletMounts(), nil
	case "storage-values":
		return storageInspectionValues(item)
	case "storage-namespaces":
		return storageInspectionNamespaces(item)
	case "storage-crds":
		return storageInspectionCRDs(item)
	case "storage-objects":
		return storageInspectionObjects(item)
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
		storage, err := storageInspection(item)
		if err != nil {
			return "", err
		}
		return inspection.all(item.Name, storage), nil
	case "objects":
		return inspection.objects, nil
	case "extras":
		return inspection.extras, nil
	default:
		return "", fmt.Errorf("unknown section %q (use %s)", section, strings.Join(InspectionSections(), ", "))
	}
}

func isStorageInspectionSection(section string) bool {
	switch section {
	case "storage", "storage-machine", "storage-values", "storage-namespaces", "storage-crds", "storage-objects":
		return true
	default:
		return false
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
	result := inspection{}
	switch item.CNI {
	case cluster.CNICilium:
		result.machine = machinePrerequisitePatch(item)
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
		result.machine = machinePrerequisitePatch(item)
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

func storageInspection(item cluster.Cluster) (string, error) {
	quoted := shellquote.Quote(item.Name)
	sections := []string{fmt.Sprintf("# Storage machine-config prerequisite patch (apply to every node before installing any CSI):\n#   tbx manifests %s storage-machine > storage-machine.yaml\n#   talosctl patch mc -p @storage-machine.yaml --nodes <node-ip>\n%s", quoted, manifests.StoragePrerequisiteKubeletMounts())}
	sections = append(sections, "# After the machine-config patch and before a BYO CSI install, create its namespace if needed and label it for Pod Security Admission:\n#   kubectl create namespace <your-csi-namespace> --dry-run=client -o yaml | kubectl apply -f -\n#   kubectl label namespace <your-csi-namespace> pod-security.kubernetes.io/enforce=privileged pod-security.kubernetes.io/audit=privileged pod-security.kubernetes.io/warn=privileged --overwrite\n# Curated CSI namespace streams carry their own PSA labels. BYO CSI remains unsupported above the substrate.")
	if item.CSI == cluster.CSILonghorn {
		values, err := storageInspectionValues(item)
		if err != nil {
			return "", err
		}
		sections = append(sections, fmt.Sprintf("# Pinned Longhorn values used by tbx (release longhorn in namespace %s):\n#   tbx manifests %s storage-values > longhorn-values.yaml\n%s", longhornNamespace, quoted, values))
	}
	if item.CSI != "" {
		streams, err := storageInspectionStreams(item)
		if err != nil {
			return "", err
		}
		if item.CSI == cluster.CSILonghorn {
			instructions, err := longhornStorageInspectionInstructions(quoted, streams)
			if err != nil {
				return "", err
			}
			sections = append(sections, instructions)
		} else {
			instructions, err := localPathStorageInspectionInstructions(quoted, streams)
			if err != nil {
				return "", err
			}
			sections = append(sections, instructions)
		}
	}
	return strings.Join(sections, "---\n"), nil
}

func storageInspectionValues(item cluster.Cluster) (string, error) {
	if item.CSI != cluster.CSILonghorn {
		return "", fmt.Errorf("cluster %q does not declare Longhorn storage values", item.Name)
	}
	return longhornValues(longhornReplicaCount(len(item.Nodes))), nil
}

func storageInspectionObjects(item cluster.Cluster) (string, error) {
	streams, err := storageInspectionStreams(item)
	if err != nil {
		return "", err
	}
	return encodeInspectionObjects(streams.objects)
}

func storageInspectionNamespaces(item cluster.Cluster) (string, error) {
	streams, err := storageInspectionStreams(item)
	if err != nil {
		return "", err
	}
	return encodeInspectionObjects(streams.namespaces)
}

func storageInspectionCRDs(item cluster.Cluster) (string, error) {
	if item.CSI != cluster.CSILonghorn {
		return "", fmt.Errorf("cluster %q does not declare Longhorn storage CRDs", item.Name)
	}
	streams, err := storageInspectionStreams(item)
	if err != nil {
		return "", err
	}
	return encodeInspectionObjects(streams.crds)
}

type storageInspectionStream struct {
	namespaces []unstructured.Unstructured
	crds       []unstructured.Unstructured
	objects    []unstructured.Unstructured
}

func storageInspectionStreams(item cluster.Cluster) (storageInspectionStream, error) {
	var (
		objects []unstructured.Unstructured
		err     error
	)
	switch item.CSI {
	case cluster.CSILonghorn:
		objects, err = renderLonghorn(item)
	case cluster.CSILocalPath:
		objects, err = renderLocalPath(item)
	default:
		return storageInspectionStream{}, fmt.Errorf("cluster %q does not declare a curated csi", item.Name)
	}
	if err != nil {
		return storageInspectionStream{}, err
	}
	streams := storageInspectionStream{}
	switch item.CSI {
	case cluster.CSILonghorn:
		streams.namespaces, streams.objects, streams.crds = partitionLonghornObjects(objects)
	case cluster.CSILocalPath:
		streams.namespaces, streams.objects = partitionLocalPathObjects(objects)
	}
	return streams, nil
}

func longhornStorageInspectionInstructions(quoted string, streams storageInspectionStream) (string, error) {
	namespaces, err := encodeInspectionObjects(streams.namespaces)
	if err != nil {
		return "", err
	}
	crds, err := encodeInspectionObjects(streams.crds)
	if err != nil {
		return "", err
	}
	objects, err := encodeInspectionObjects(streams.objects)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("# Longhorn must be applied in these separate streams. Do not combine them into a one-shot apply: CRDs must be Established before chart objects are sent to the API.\n#   tbx manifests %s storage-namespaces | kubectl apply --server-side -f -\n%s---\n#   tbx manifests %s storage-crds | kubectl apply --server-side -f -\n%s---\n#   tbx manifests %s storage-crds | kubectl wait --for=condition=Established --timeout=120s -f -\n#   tbx manifests %s storage-objects | kubectl apply --server-side -f -\n%s", quoted, namespaces, quoted, crds, quoted, quoted, objects), nil
}

func localPathStorageInspectionInstructions(quoted string, streams storageInspectionStream) (string, error) {
	namespaces, err := encodeInspectionObjects(streams.namespaces)
	if err != nil {
		return "", err
	}
	objects, err := encodeInspectionObjects(streams.objects)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("# Local-path has no CRD barrier; apply its namespace stream before its chart-object stream:\n#   tbx manifests %s storage-namespaces | kubectl apply --server-side -f -\n%s---\n#   tbx manifests %s storage-objects | kubectl apply --server-side -f -\n%s", quoted, namespaces, quoted, objects), nil
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

func (i inspection) all(clusterName, storage string) string {
	quoted := shellquote.Quote(clusterName)
	sections := []string{
		"# Inspection bundle only: this stream mixes a Talos patch, Helm values, and Kubernetes objects.\n# Do not pipe it wholesale to talosctl, helm, or kubectl; run the command above each section instead.\n",
	}
	if i.machine != "" {
		sections = append(sections, fmt.Sprintf("# Machine-config prerequisite patch (apply before bootstrap):\n#   tbx manifests %s machine\n%s", quoted, i.machine))
	}
	if i.values != "" {
		sections = append(sections, fmt.Sprintf("# Pinned Helm values used by tbx (release %s in namespace %s):\n#   tbx manifests %s values > values.yaml\n%s", i.chart.release, i.chart.namespace, quoted, i.values))
	}
	if i.objects != "" {
		sections = append(sections, fmt.Sprintf("# Exact rendered chart objects tbx applies server-side (%s %s, release %s, namespace %s):\n#   tbx manifests %s objects | kubectl apply --server-side -f -\n%s", i.chart.name, i.chart.version, i.chart.release, i.chart.namespace, quoted, i.objects))
	}
	if i.extras != "" {
		sections = append(sections, fmt.Sprintf("# Exact LoadBalancer/BGP extras and probe tbx applies:\n#   tbx manifests %s extras | kubectl apply --server-side -f -\n%s", quoted, i.extras))
	}
	sections = append(sections, storage)
	return strings.Join(sections, "---\n")
}
