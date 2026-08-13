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
