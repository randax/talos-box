package main

import (
	"errors"
	"io"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/daemon"
)

func TestParseClusterStartArgsAcceptsForce(t *testing.T) {
	name, force, err := parseClusterStartArgs([]string{"demo", "--force"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if name != "demo" || !force {
		t.Fatalf("parseClusterStartArgs() = %q, %v; want demo, true", name, force)
	}
}

func TestStartProvisionDeadlineMatchesTheStoredClusterCSI(t *testing.T) {
	previous := storedClustersQuery
	t.Cleanup(func() { storedClustersQuery = previous })
	storedClustersQuery = func() ([]daemon.ClusterSummary, error) {
		return []daemon.ClusterSummary{
			{Name: "other", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, CSI: cluster.CSILonghorn}},
			{Name: "demo", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium}},
		}, nil
	}
	if got := storedProvisionDeadline("demo"); got != cniProvisionDeadline {
		t.Fatalf("storedProvisionDeadline(cni-only) = %v, want %v", got, cniProvisionDeadline)
	}
	if got := storedProvisionDeadline("other"); got != storageProvisionDeadline {
		t.Fatalf("storedProvisionDeadline(csi) = %v, want %v", got, storageProvisionDeadline)
	}
}

func TestStartProvisionDeadlineFallsBackToTheConservativeBound(t *testing.T) {
	previous := storedClustersQuery
	t.Cleanup(func() { storedClustersQuery = previous })
	storedClustersQuery = func() ([]daemon.ClusterSummary, error) {
		return nil, errors.New("no daemon")
	}
	if got := storedProvisionDeadline("demo"); got != storageProvisionDeadline {
		t.Fatalf("storedProvisionDeadline(query failed) = %v, want the conservative %v", got, storageProvisionDeadline)
	}
	storedClustersQuery = func() ([]daemon.ClusterSummary, error) {
		return []daemon.ClusterSummary{{Name: "elsewhere"}}, nil
	}
	if got := storedProvisionDeadline("demo"); got != storageProvisionDeadline {
		t.Fatalf("storedProvisionDeadline(unknown cluster) = %v, want the conservative %v", got, storageProvisionDeadline)
	}
}

// stubStoredClusters answers a verb's deadline lookup (cluster start, node
// add) without a
// daemon, so a harness that scripts an exact response sequence is not thrown
// off by the extra query.
func stubStoredClusters(t *testing.T, clusters ...daemon.ClusterSummary) {
	t.Helper()
	previous := storedClustersQuery
	t.Cleanup(func() { storedClustersQuery = previous })
	storedClustersQuery = func() ([]daemon.ClusterSummary, error) { return clusters, nil }
}
