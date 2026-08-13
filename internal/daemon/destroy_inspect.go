package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/randax/talos-box/internal/cluster"
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
	if count <= 0 {
		return DestroyInspection{}
	}
	return DestroyInspection{Warning: destroyInspectionCountWarning(item.Name, item.CSI, count)}
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
	unit := "volumes"
	if count == 1 {
		unit = "volume"
	}
	return fmt.Sprintf(
		"destroying cluster %s will permanently delete %d %s %s and their data",
		name,
		count,
		strings.TrimSpace(string(engine)),
		unit,
	)
}
