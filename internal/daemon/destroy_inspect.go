package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/provision"
)

// DestroyInspection is the best-effort storage data-loss warning surfaced
// before a forced cluster destroy.
type DestroyInspection struct {
	Warning string `json:"warning,omitempty"`
}

func (s *Server) destroyInspect(raw json.RawMessage) (DestroyInspection, error) {
	var args destroyArgs
	if err := decodeArgs(raw, &args); err != nil {
		return DestroyInspection{}, err
	}
	if !args.Force {
		return DestroyInspection{}, errors.New("cluster.destroy.inspect requires force=true")
	}
	// A cluster that never existed has no data to lose: say so here, before
	// the client can print a data-loss warning about nothing (#268).
	// A name no cluster can carry names no cluster either: refuse it as
	// missing so the client suppresses the warning here too, keeping the
	// reason the name was rejected in the message (#268).
	if err := cluster.ValidateName(args.Name); err != nil {
		return DestroyInspection{}, fmt.Errorf("%w: %w", ClusterMissingError(args.Name), err)
	}
	dir, err := cluster.Dir(args.Name)
	if err != nil {
		return DestroyInspection{}, err
	}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return DestroyInspection{}, ClusterMissingError(args.Name)
	}
	item, err := cluster.Load(args.Name)
	if err != nil {
		return DestroyInspection{}, err
	}
	return s.inspectDestroyCluster(item), nil
}

func (s *Server) inspectDestroyCluster(item cluster.Cluster) DestroyInspection {
	if item.CSI == "" {
		return DestroyInspection{}
	}
	if s.destroyVolumeCount == nil {
		return DestroyInspection{Warning: DestroyInspectionDataLossWarning(item.Name, item.CSI)}
	}
	ctx := s.lifecycleContext
	if ctx == nil {
		ctx = context.Background()
	}
	count, err := s.destroyVolumeCount(ctx, item)
	if err != nil {
		return DestroyInspection{Warning: DestroyInspectionDataLossWarning(item.Name, item.CSI)}
	}
	return DestroyInspection{Warning: destroyInspectionCountWarning(item.Name, item.CSI, count)}
}

func countDestroyStorageVolumes(ctx context.Context, item cluster.Cluster) (int, error) {
	kubeconfig, err := clusterKubeconfig(item.Name)
	if err != nil {
		return 0, fmt.Errorf("read kubeconfig for destroy inspection: %w", err)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return provision.CountProvisionedStorageVolumes(probeCtx, kubeconfig, item.CSI)
}

func clusterKubeconfig(name string) ([]byte, error) {
	dir, err := cluster.Dir(name)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(dir, "kubeconfig"))
}

// ClusterMissingError is the daemon's refusal for a cluster that does not
// exist. It crosses the wire as a plain string, so IsClusterMissing is how a
// client tells it apart from an inspection that merely failed.
func ClusterMissingError(name string) error {
	return fmt.Errorf("cluster %q does not exist", name)
}

// IsClusterMissing reports whether err is the missing-cluster refusal for name.
func IsClusterMissing(err error, name string) bool {
	return err != nil && strings.Contains(err.Error(), ClusterMissingError(name).Error())
}

func DestroyInspectionDataLossWarning(name string, engine cluster.CSI) string {
	subject := "CSI-backed data"
	if engine != "" {
		subject = fmt.Sprintf("%s volumes and their data", engine)
	}
	return fmt.Sprintf(
		"destroying cluster %s may permanently delete %s; inspect persistent volumes manually if you need to keep them",
		name,
		subject,
	)
}

func destroyInspectionCountWarning(name string, engine cluster.CSI, count int) string {
	return fmt.Sprintf(
		"destroying cluster %s will permanently delete %d %s %s and their data",
		name,
		count,
		strings.TrimSpace(string(engine)),
		volumeUnit(count),
	)
}
