package provision

import (
	"slices"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/siderolabs/talos/pkg/machinery/constants"
)

func imagesTestCluster(intent cluster.ProvisioningIntent) cluster.Cluster {
	return cluster.Cluster{
		Name:               "demo",
		SubnetIndex:        3,
		ControlPlanes:      1,
		Workers:            2,
		ProvisioningIntent: intent,
		Schematic:          "cafe1234",
		TalosVersion:       "v1.13.6",
		Nodes: []cluster.Node{
			{Name: "cp-1", Role: cluster.RoleControlPlane},
			{Name: "w-1", Role: cluster.RoleWorker},
			{Name: "w-2", Role: cluster.RoleWorker},
		},
	}
}

// TestClusterImagesReflectDeclaredIntent walks the images out of the objects
// each intent actually renders, so an image set can never claim more or less
// than the cluster will pull.
func TestClusterImagesReflectDeclaredIntent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		intent  cluster.ProvisioningIntent
		want    []string
		unwant  []string
		wantAny []string
	}{
		{
			name:   "substrate only",
			intent: cluster.ProvisioningIntent{},
			want:   []string{"registry.k8s.io/kube-proxy:v" + constants.DefaultKubernetesVersion},
			unwant: []string{"quay.io/cilium/cilium", "docker.io/longhornio/longhorn-manager"},
		},
		{
			name:    "cilium",
			intent:  cluster.ProvisioningIntent{CNI: cluster.CNICilium},
			wantAny: []string{"quay.io/cilium/cilium:"},
			unwant:  []string{"registry.k8s.io/kube-proxy:v" + constants.DefaultKubernetesVersion, "quay.io/metallb/controller"},
		},
		{
			name:    "flannel with metallb",
			intent:  cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true},
			want:    []string{"ghcr.io/siderolabs/flannel:" + constants.FlannelVersion},
			wantAny: []string{"quay.io/metallb/"},
			unwant:  []string{"quay.io/cilium/cilium:"},
		},
		{
			name:   "longhorn",
			intent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, CSI: cluster.CSILonghorn},
			want: []string{
				"docker.io/longhornio/longhorn-manager:v1.12.0",
				// Longhorn passes the images its manager spawns as
				// command flags, not container image fields.
				"docker.io/longhornio/longhorn-instance-manager:v1.12.0",
				"docker.io/longhornio/csi-attacher:v4.12.0",
			},
			unwant: []string{"docker.io/rancher/local-path-provisioner:v0.0.37"},
		},
		{
			name:   "local-path",
			intent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, CSI: cluster.CSILocalPath},
			want: []string{
				"docker.io/rancher/local-path-provisioner:v0.0.37",
				"docker.io/library/busybox:1.37.0",
			},
			unwant: []string{"docker.io/longhornio/longhorn-manager:v1.12.0"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			images, err := ClusterImages(imagesTestCluster(test.intent))
			if err != nil {
				t.Fatalf("ClusterImages() error = %v", err)
			}
			for _, want := range test.want {
				if !slices.Contains(images, want) {
					t.Errorf("images missing %q, got %v", want, images)
				}
			}
			for _, prefix := range test.wantAny {
				if !slices.ContainsFunc(images, func(ref string) bool { return strings.HasPrefix(ref, prefix) }) {
					t.Errorf("images missing an entry starting with %q, got %v", prefix, images)
				}
			}
			for _, unwanted := range test.unwant {
				if slices.ContainsFunc(images, func(ref string) bool { return strings.HasPrefix(ref, unwanted) }) {
					t.Errorf("images contain %q, which this intent never pulls: %v", unwanted, images)
				}
			}
			if !slices.IsSorted(images) {
				t.Errorf("images = %v, want sorted", images)
			}
		})
	}
}

// TestClusterImagesEnumerateTalosSystemImagesFromThePinnedVersion keeps the
// system half keyed by the cluster's own pin: the installer a cluster upgrades
// from is the one built for its schematic and version.
func TestClusterImagesEnumerateTalosSystemImagesFromThePinnedVersion(t *testing.T) {
	t.Parallel()
	item := imagesTestCluster(cluster.ProvisioningIntent{})
	item.TalosVersion = "v1.14.0"
	images, err := ClusterImages(item)
	if err != nil {
		t.Fatalf("ClusterImages() error = %v", err)
	}
	kubernetes := ":v" + constants.DefaultKubernetesVersion
	for _, want := range []string{
		"factory.talos.dev/metal-installer/cafe1234:v1.14.0",
		constants.KubeletImage + kubernetes,
		constants.KubernetesAPIServerImage + kubernetes,
		constants.KubernetesControllerManagerImage + kubernetes,
		constants.KubernetesSchedulerImage + kubernetes,
		constants.EtcdImage + ":" + constants.DefaultEtcdVersion,
		constants.CoreDNSImage + ":" + constants.DefaultCoreDNSVersion,
	} {
		if !slices.Contains(images, want) {
			t.Errorf("images missing %q, got %v", want, images)
		}
	}
	if slices.Contains(images, "factory.talos.dev/metal-installer/cafe1234:v1.13.6") {
		t.Errorf("images pin a version the cluster does not declare: %v", images)
	}
}

// TestClusterImagesChangeWithIntent is the property the export exists for: a
// different declared intent is a different warm list.
func TestClusterImagesChangeWithIntent(t *testing.T) {
	t.Parallel()
	base, err := ClusterImages(imagesTestCluster(cluster.ProvisioningIntent{CNI: cluster.CNICilium}))
	if err != nil {
		t.Fatal(err)
	}
	storage, err := ClusterImages(imagesTestCluster(cluster.ProvisioningIntent{CNI: cluster.CNICilium, CSI: cluster.CSILonghorn}))
	if err != nil {
		t.Fatal(err)
	}
	if slices.Equal(base, storage) {
		t.Fatal("adding a CSI left the image set unchanged")
	}
	for _, ref := range base {
		if !slices.Contains(storage, ref) {
			t.Errorf("adding a CSI dropped %q", ref)
		}
	}
}

func TestIsImageRefRejectsUnwarmableStrings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		value string
		want  bool
	}{
		{"docker.io/longhornio/longhorn-manager:v1.12.0", true},
		{"registry.k8s.io/coredns/coredns:v1.14.2", true},
		{"factory.talos.dev/metal-installer/cafe1234:v1.13.6", true},
		{"docker.io/library/busybox@sha256:" + strings.Repeat("a", 64), true},
		{"docker.io/library/busybox:latest", false},
		{"docker.io/library/busybox", false},
		{"busybox:1.37.0", false},
		{"http://172.30.3.1:5000", false},
		{"--manager-image", false},
		{"", false},
		{"30s", false},
	}
	for _, test := range tests {
		if got := isImageRef(test.value); got != test.want {
			t.Errorf("isImageRef(%q) = %v, want %v", test.value, got, test.want)
		}
	}
}
