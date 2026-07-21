package daemon

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/randax/talos-box/internal/cluster"
)

type snapshotArgs struct {
	Cluster string `json:"cluster"`
	Name    string `json:"name"`
}

// withClusterStopped runs body with the cluster's VMs stopped, restarting them
// afterward if they were running — even if body fails, so a failed snapshot or
// restore never leaves a workshop cluster powered off. Returns the joined error.
func withClusterStopped(running bool, stop, start, body func() error) error {
	if !running {
		return body()
	}
	if err := stop(); err != nil {
		return fmt.Errorf("stop cluster: %w", err)
	}
	bodyErr := body()
	startErr := start()
	return errors.Join(bodyErr, startErr)
}

func (s *Server) snapshotCreate(raw json.RawMessage) ([]cluster.SnapshotInfo, error) {
	args, item, err := s.loadSnapshotTarget(raw)
	if err != nil {
		return nil, err
	}
	running := s.clusterRunning(item.Name)
	err = withClusterStopped(running,
		func() error { return s.stop(item.Name) },
		func() error { return s.startAndLogWarning(item) },
		func() error { return cluster.CreateSnapshot(item, args.Name) },
	)
	if err != nil {
		return nil, err
	}
	return cluster.ListSnapshots(item.Name)
}

func (s *Server) snapshotRestore(raw json.RawMessage) ([]cluster.SnapshotInfo, error) {
	args, item, err := s.loadSnapshotTarget(raw)
	if err != nil {
		return nil, err
	}
	running := s.clusterRunning(item.Name)
	// restore always ends powered on (SPEC §7: cold boot), even if the cluster
	// was stopped when restore was invoked
	err = withClusterStopped(true,
		func() error {
			if running {
				return s.stop(item.Name)
			}
			return nil
		},
		func() error {
			// the snapshot may have restored a different node set; reload
			restored, loadErr := cluster.Load(item.Name)
			if loadErr != nil {
				return loadErr
			}
			return s.startAndLogWarning(restored)
		},
		func() error { return cluster.RestoreSnapshot(item, args.Name) },
	)
	if err != nil {
		return nil, err
	}
	return cluster.ListSnapshots(item.Name)
}

func (s *Server) snapshotList(raw json.RawMessage) ([]cluster.SnapshotInfo, error) {
	var args snapshotArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	return cluster.ListSnapshots(args.Cluster)
}

func (s *Server) snapshotDelete(raw json.RawMessage) ([]cluster.SnapshotInfo, error) {
	var args snapshotArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := cluster.DeleteSnapshot(args.Cluster, args.Name); err != nil {
		return nil, err
	}
	return cluster.ListSnapshots(args.Cluster)
}

func (s *Server) loadSnapshotTarget(raw json.RawMessage) (snapshotArgs, cluster.Cluster, error) {
	var args snapshotArgs
	if err := decodeArgs(raw, &args); err != nil {
		return args, cluster.Cluster{}, err
	}
	if args.Name == "" {
		return args, cluster.Cluster{}, errors.New("snapshot name is required")
	}
	item, err := cluster.Load(args.Cluster)
	return args, item, err
}
