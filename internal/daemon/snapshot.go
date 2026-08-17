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
	// Force overrides the restore's storage-volume gate; the other snapshot
	// operations delete no node disk and ignore it.
	Force bool `json:"force"`
}

// SnapshotStatus is snapshot.create's and snapshot.restore's response: the
// cluster's snapshots plus any advisory finding raised while the operation ran
// — restore's storage data-loss gate, and the host-subnet finding from the
// restart both operations perform.
type SnapshotStatus struct {
	Snapshots []cluster.SnapshotInfo `json:"snapshots"`
	Warning   string                 `json:"warning,omitempty"`
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
	var startErr error
	if err := start(); err != nil {
		startErr = fmt.Errorf("restart cluster: %w", err)
	}
	return errors.Join(bodyErr, startErr)
}

func (s *Server) snapshotCreate(raw json.RawMessage) (SnapshotStatus, error) {
	args, item, err := s.loadSnapshotTarget(raw)
	if err != nil {
		return SnapshotStatus{}, err
	}
	// SPEC §7: the disks are cloned as one crash-consistent set, so the VMs are
	// stopped first — there is no live-snapshot fast path — and a cluster that
	// was running is restarted afterward, while a stopped one stays stopped.
	running := s.clusterRunning(item.Name)
	var created bool
	var restartWarning string
	err = withClusterStopped(running,
		func() error { return s.stop(item.Name) },
		func() error {
			warning, startErr := s.startAndLogWarning(item)
			restartWarning = warning
			return startErr
		},
		func() error {
			if err := cluster.CreateSnapshot(item, args.Name); err != nil {
				return err
			}
			created = true
			return nil
		},
	)
	if err != nil {
		if created {
			// the clone is on disk and intact; only the restart failed, and
			// saying so keeps the operator from re-running a snapshot that
			// already exists
			return SnapshotStatus{}, fmt.Errorf("snapshot %q was created, but %w; start it with tbx cluster start %s", args.Name, err, item.Name)
		}
		return SnapshotStatus{}, err
	}
	snapshots, err := cluster.ListSnapshots(item.Name)
	if err != nil {
		return SnapshotStatus{}, err
	}
	return SnapshotStatus{Snapshots: snapshots, Warning: restartWarning}, nil
}

func (s *Server) snapshotRestore(raw json.RawMessage) (SnapshotStatus, error) {
	args, item, err := s.loadSnapshotTarget(raw)
	if err != nil {
		return SnapshotStatus{}, err
	}
	running := s.clusterRunning(item.Name)
	var restartWarning string
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
			warning, startErr := s.startAndLogWarning(restored)
			restartWarning = warning
			return startErr
		},
		func() error { return cluster.RestoreSnapshot(item, args.Name) },
	)
	if err != nil {
		return SnapshotStatus{}, err
	}
	snapshots, err := cluster.ListSnapshots(item.Name)
	if err != nil {
		return SnapshotStatus{}, err
	}
	return SnapshotStatus{Snapshots: snapshots, Warning: restartWarning}, nil
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
