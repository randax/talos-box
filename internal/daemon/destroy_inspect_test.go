package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
)

func TestInspectDestroyClusterWarnsWithKnownVolumeCount(t *testing.T) {
	item := cluster.Cluster{Name: "demo", ProvisioningIntent: cluster.ProvisioningIntent{CSI: cluster.CSILonghorn}}
	server := &Server{
		destroyVolumeCount: func(context.Context, cluster.Cluster) (int, error) {
			return 2, nil
		},
	}

	result := server.inspectDestroyCluster(item)

	if !strings.Contains(result.Warning, "2 longhorn volumes") {
		t.Fatalf("warning = %q, want count-specific longhorn warning", result.Warning)
	}
}

func TestInspectDestroyClusterFallsBackToGenericDataLossWarning(t *testing.T) {
	item := cluster.Cluster{Name: "demo", ProvisioningIntent: cluster.ProvisioningIntent{CSI: cluster.CSILocalPath}}
	server := &Server{
		destroyVolumeCount: func(context.Context, cluster.Cluster) (int, error) {
			return 0, errors.New("unreachable")
		},
	}

	result := server.inspectDestroyCluster(item)

	if !strings.Contains(result.Warning, "may permanently delete local-path volumes and their data") {
		t.Fatalf("warning = %q, want generic local-path data-loss warning", result.Warning)
	}
}

func TestInspectDestroyClusterShowsReachableZeroVolumeCount(t *testing.T) {
	item := cluster.Cluster{Name: "demo", ProvisioningIntent: cluster.ProvisioningIntent{CSI: cluster.CSILocalPath}}
	server := &Server{destroyVolumeCount: func(context.Context, cluster.Cluster) (int, error) { return 0, nil }}

	result := server.inspectDestroyCluster(item)

	if !strings.Contains(result.Warning, "0 local-path volumes") {
		t.Fatalf("warning = %q, want known zero-volume count", result.Warning)
	}
}

func TestInspectDestroyClusterSkipsClustersWithoutCSIIntent(t *testing.T) {
	server := &Server{
		destroyVolumeCount: func(context.Context, cluster.Cluster) (int, error) {
			t.Fatal("destroyVolumeCount called without CSI intent")
			return 0, nil
		},
	}

	result := server.inspectDestroyCluster(cluster.Cluster{Name: "demo"})

	if result.Warning != "" {
		t.Fatalf("warning = %q, want empty", result.Warning)
	}
}

// Inspecting a cluster that does not exist must refuse rather than produce a
// warning the client would print before the destroy fails (#268).
func TestDestroyInspectRefusesMissingCluster(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	server := &Server{}

	result, err := server.destroyInspect(mustRawJSON(t, destroyArgs{Name: "ghost", Force: true}))

	if err == nil || !IsClusterMissing(err, "ghost") {
		t.Fatalf("destroyInspect() error = %v, want missing-cluster refusal", err)
	}
	if result.Warning != "" {
		t.Fatalf("warning = %q, want none for a cluster that does not exist", result.Warning)
	}
}

// A name no cluster could ever carry is the same case: it must refuse as
// missing rather than let the client warn about data that cannot exist (#268).
func TestDestroyInspectRefusesUnnameableClusters(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	server := &Server{}

	for _, name := range []string{"", "a/b", ".."} {
		result, err := server.destroyInspect(mustRawJSON(t, destroyArgs{Name: name, Force: true}))
		if err == nil || !IsClusterMissing(err, name) {
			t.Fatalf("destroyInspect(%q) error = %v, want missing-cluster refusal", name, err)
		}
		if result.Warning != "" {
			t.Fatalf("destroyInspect(%q) warning = %q, want none", name, result.Warning)
		}
	}
}
