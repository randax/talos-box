package provision

import (
	"regexp"
	"slices"
	"strings"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/talosversion"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// factoryInstallerRepository holds the per-schematic installer image the
// Factory builds. A cluster created from a schematic upgrades from its own
// installer, not the vanilla one.
const factoryInstallerRepository = "factory.talos.dev/metal-installer"

// vanillaInstallerRepository is the installer for a cluster with no schematic
// recorded, which is only stored state predating schematic persistence.
const vanillaInstallerRepository = "ghcr.io/siderolabs/installer"

// KubernetesSandboxImage is the CRI pod-sandbox ("pause") image the kubelet
// asks containerd for before it can start any pod, static control-plane pods
// included. It is pinned by hand because it is coupled to the Kubernetes
// version tbx generates machine configs with — constants.DefaultKubernetesVersion
// — yet machinery exports no constant for it, no rendered object references
// it, and an online node never fetches it through the mirror (Talos seeds it
// into the rootfs image store), so it would otherwise be absent from the cache
// exactly when a venue goes offline. Bump it together with
// constants.DefaultKubernetesVersion, reading the value the matching kubelet
// defaults to (kubeadm's DefaultPauseImage / the kubelet's
// --pod-infra-container-image); Kubernetes 1.36 uses pause:3.10.1.
const KubernetesSandboxImage = "registry.k8s.io/pause:3.10.1"

// sandboxImageKubernetesMinor is the Kubernetes minor the pin above was read
// from. A machinery bump past it must re-read the kubelet's sandbox default,
// which TestSandboxImagePinTracksTheBundledKubernetes enforces.
const sandboxImageKubernetesMinor = "1.36"

// BootstrapRequiredImages is what every node must find in the cache before a
// single static pod can start, and which no rendered object carries — so no
// derived warm list can contain it. `cache warm --check --deep` verifies this
// set on top of whatever list it was handed, so a venue finds out the sandbox
// image is missing while it can still be pulled rather than from a bootstrap
// that cannot recover.
func BootstrapRequiredImages() []string {
	return []string{KubernetesSandboxImage}
}

// ClusterImages is every container image the cluster pulls: the images carried
// by the objects tbx actually renders for its declared provisioning intent,
// plus the Talos system images for its pinned version. The set is derived from
// the rendered objects rather than a parallel list so it cannot drift from
// what the reconcilers apply. Refs are deduped and sorted so the output is
// stable enough to diff, and each one is a fully qualified `cache warm` entry.
func ClusterImages(item cluster.Cluster) ([]string, error) {
	objects, err := provisioningObjects(item)
	if err != nil {
		return nil, err
	}
	refs := map[string]struct{}{}
	for _, object := range objects {
		collectObjectImages(object.Object, refs)
	}
	for _, ref := range talosSystemImages(item) {
		refs[ref] = struct{}{}
	}
	// The storage probe pod and local-path's helper pod are created from a
	// constant and an embedded pod template at runtime, so neither leaves a
	// rendered object to walk.
	if item.CSI != "" {
		refs[localPathHelperImage] = struct{}{}
	}
	result := make([]string, 0, len(refs))
	for ref := range refs {
		result = append(result, ref)
	}
	slices.Sort(result)
	return result, nil
}

// imageInspection is the `tbx manifests <cluster> images` document: a
// `cache warm` list, so it pipes straight into `tbx cache warm -`.
func imageInspection(item cluster.Cluster) (string, error) {
	images, err := ClusterImages(item)
	if err != nil {
		return "", err
	}
	var document strings.Builder
	// Comment lines are skipped by the warm-list parser, so naming the
	// cluster costs the round-trip nothing.
	document.WriteString("# Images cluster " + item.Name + " pulls, for tbx cache warm\n")
	for _, image := range images {
		document.WriteString(image + "\n")
	}
	return document.String(), nil
}

// provisioningObjects renders everything the declared intent installs. A
// substrate-only cluster renders nothing, which is the honest answer: tbx
// applies no workloads to it.
func provisioningObjects(item cluster.Cluster) ([]unstructured.Unstructured, error) {
	var objects []unstructured.Unstructured
	switch item.CNI {
	case cluster.CNICilium:
		rendered, err := renderCilium(item)
		if err != nil {
			return nil, err
		}
		objects = append(objects, rendered...)
	case cluster.CNIFlannel:
		if item.LB {
			rendered, err := renderMetalLB(item)
			if err != nil {
				return nil, err
			}
			objects = append(objects, rendered...)
		}
	}
	switch item.CSI {
	case cluster.CSILonghorn, cluster.CSILocalPath:
		streams, err := storageInspectionStreams(item)
		if err != nil {
			return nil, err
		}
		objects = append(objects, streams.namespaces...)
		objects = append(objects, streams.crds...)
		objects = append(objects, streams.objects...)
	}
	return objects, nil
}

// talosSystemImages enumerates what Talos itself pulls on a node. The
// Kubernetes half is whatever the machinery this binary is built against
// pins, because that is the version tbx generates machine configs with
// (see generateMachineConfigs); the installer half is keyed by the cluster's
// own Talos version and schematic.
func talosSystemImages(item cluster.Cluster) []string {
	version := item.TalosVersion
	if version == "" {
		version = talosversion.Default
	}
	kubernetes := ":v" + constants.DefaultKubernetesVersion
	images := []string{
		installerImage(item.Schematic, version),
		// The kubelet's sandbox image is pulled for every pod on every
		// node, so it belongs to the system half of every cluster's set
		// regardless of the declared intent.
		KubernetesSandboxImage,
		constants.KubeletImage + kubernetes,
		constants.KubernetesAPIServerImage + kubernetes,
		constants.KubernetesControllerManagerImage + kubernetes,
		constants.KubernetesSchedulerImage + kubernetes,
		constants.EtcdImage + ":" + constants.DefaultEtcdVersion,
		constants.CoreDNSImage + ":" + constants.DefaultCoreDNSVersion,
	}
	// Cilium replaces kube-proxy, so a Cilium cluster never pulls it.
	if !ciliumDisablesKubeProxy(item) {
		images = append(images, constants.KubeProxyImage+kubernetes)
	}
	// Flannel is deployed by Talos itself, not by a tbx-rendered object.
	if item.CNI == cluster.CNIFlannel {
		images = append(images, "ghcr.io/siderolabs/flannel:"+constants.FlannelVersion)
	}
	return images
}

func installerImage(schematic, version string) string {
	if schematic == "" {
		return vanillaInstallerRepository + ":" + version
	}
	return factoryInstallerRepository + "/" + schematic + ":" + version
}

// collectObjectImages walks a rendered object for image references. Container
// image fields are the obvious carrier; Longhorn additionally passes the
// images its manager spawns as command flags and environment values, so those
// are collected too — hence the shape check instead of a path whitelist.
func collectObjectImages(value any, refs map[string]struct{}) {
	switch typed := value.(type) {
	case map[string]any:
		for key, entry := range typed {
			switch key {
			case "image", "value":
				if ref, ok := entry.(string); ok && isImageRef(ref) {
					refs[ref] = struct{}{}
				}
			case "command", "args":
				if list, ok := entry.([]any); ok {
					for _, element := range list {
						if ref, ok := element.(string); ok && isImageRef(ref) {
							refs[ref] = struct{}{}
						}
					}
				}
			}
			collectObjectImages(entry, refs)
		}
	case []any:
		for _, entry := range typed {
			collectObjectImages(entry, refs)
		}
	}
}

var (
	imageRepositoryShape = regexp.MustCompile(`^[a-z0-9-]+(\.[a-z0-9-]+)+(:[0-9]+)?(/[a-z0-9]+([._-][a-z0-9]+)*)+$`)
	imageTagShape        = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)
	imageDigestShape     = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// isImageRef reports whether a string is a fully qualified, immutable image
// reference. The rules are `cache warm`'s: a registry host, and a tag that is
// not latest or a digest, so an emitted set is directly warmable.
func isImageRef(value string) bool {
	name, digest, hasDigest := strings.Cut(value, "@")
	if hasDigest && !imageDigestShape.MatchString(digest) {
		return false
	}
	repository, tag := name, ""
	hasTag := false
	if colon := strings.LastIndex(name, ":"); colon > strings.LastIndex(name, "/") {
		repository, tag, hasTag = name[:colon], name[colon+1:], true
	}
	if !hasTag && !hasDigest {
		return false
	}
	if hasTag && (tag == "latest" || !imageTagShape.MatchString(tag)) {
		return false
	}
	return imageRepositoryShape.MatchString(repository)
}
