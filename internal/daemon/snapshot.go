package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
	// Warnings is the same advisory set as Warning, one entry per finding.
	// Warning stays populated for older clients that only read it.
	Warnings []string `json:"warnings,omitempty"`
}

// setWarnings fills both the list and the legacy joined string, so the restore
// gate's data-loss note and the restart's host-subnet finding each get their
// own line instead of fusing into one (#291).
func (s *SnapshotStatus) setWarnings(warnings ...string) {
	s.Warnings = warningList(warnings...)
	s.Warning = strings.Join(s.Warnings, "; ")
}

// prependWarning puts a finding ahead of the ones already recorded: the
// restore gate's note is decided before the status is built, so it reads first.
func (s *SnapshotStatus) prependWarning(warning string) {
	s.setWarnings(append([]string{warning}, s.Warnings...)...)
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

func (s *Server) snapshotCreate(raw json.RawMessage, progress stageFunc) (SnapshotStatus, error) {
	args, item, err := s.loadSnapshotTarget(raw)
	if err != nil {
		return SnapshotStatus{}, err
	}
	// SPEC §7: the disks are cloned as one crash-consistent set, so the VMs are
	// stopped first — there is no live-snapshot fast path — and a cluster that
	// was running is restarted afterward, while a stopped one stays stopped.
	running := s.clusterRunning(item.Name)
	var created bool
	var restartWarnings []string
	err = withClusterStopped(running,
		func() error {
			progress.stage("stopping cluster %s", item.Name)
			return s.stop(item.Name)
		},
		func() error {
			progress.stage("restarting cluster %s", item.Name)
			warnings, startErr := s.startAndLogWarning(item)
			restartWarnings = warnings
			return startErr
		},
		func() error {
			progress.stage("cloning %d node disk(s) as one crash-consistent set", len(item.Nodes))
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
	if running {
		progress.stage("%s", convergenceHint(item.Name))
	}
	snapshots, err := cluster.ListSnapshots(item.Name)
	if err != nil {
		return SnapshotStatus{}, err
	}
	status := SnapshotStatus{Snapshots: snapshots}
	status.setWarnings(restartWarnings...)
	return status, nil
}

func (s *Server) snapshotRestore(raw json.RawMessage, progress stageFunc) (SnapshotStatus, error) {
	args, item, err := s.loadSnapshotTarget(raw)
	if err != nil {
		return SnapshotStatus{}, err
	}
	running := s.clusterRunning(item.Name)
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		return SnapshotStatus{}, err
	}
	var restartWarnings []string
	var discardWarnings []string
	// restore always ends powered on (SPEC §7: cold boot), even if the cluster
	// was stopped when restore was invoked
	err = withClusterStopped(true,
		func() error {
			if running {
				progress.stage("stopping cluster %s", item.Name)
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
			progress.stage("starting cluster %s", restored.Name)
			warnings, startErr := s.startAndLogWarning(restored)
			restartWarnings = warnings
			return startErr
		},
		func() error {
			progress.stage("%s", restoreStage(item, args.Name))
			if err := cluster.RestoreSnapshot(item, args.Name); err != nil {
				return err
			}
			// The restore swapped the node disks out from under any suspended
			// memory, so every save in the directory now describes a machine
			// state that no longer matches its disk — resuming onto one would
			// corrupt the restored cluster. They are dropped before anything
			// starts, and before status can report the cluster Suspended.
			dropped, failures := discardClusterSavedStates(dir)
			if dropped {
				discardWarnings = append(discardWarnings, discardedSaveStateWarning("the cluster"))
			}
			discardWarnings = append(discardWarnings, failures...)
			return nil
		},
	)
	if err != nil {
		return SnapshotStatus{}, err
	}
	// A restore always ends powered on, so it always leaves nodes converging.
	progress.stage("%s", convergenceHint(item.Name))
	snapshots, err := cluster.ListSnapshots(item.Name)
	if err != nil {
		return SnapshotStatus{}, err
	}
	status := SnapshotStatus{Snapshots: snapshots}
	status.setWarnings(append(discardWarnings, restartWarnings...)...)
	return status, nil
}

// restoreStage narrates what the restore actually does: it restores the disks
// the snapshot captured — not the live node count, which may have grown since —
// and names the live nodes it deletes because the snapshot never captured them
// (#273). A snapshot whose captured state cannot be read is narrated by name
// alone; cluster.RestoreSnapshot owns reporting why it is unusable.
func restoreStage(item cluster.Cluster, name string) string {
	captured, err := cluster.SnapshotNodes(item.Name, name)
	if err != nil {
		return fmt.Sprintf("restoring node disks from snapshot %s", name)
	}
	stage := fmt.Sprintf("restoring %d node disk(s) from snapshot %s", len(captured), name)
	vanishing := vanishingRestoreNodes(item.Nodes, captured)
	if len(vanishing) > 0 {
		stage += fmt.Sprintf(", deleting %d node(s) it did not capture (%s)", len(vanishing), strings.Join(nodeNames(vanishing), ", "))
	}
	return stage
}

func (s *Server) snapshotList(raw json.RawMessage) ([]cluster.SnapshotInfo, error) {
	var args snapshotArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	// ListSnapshots never returns nil on success, so an empty result
	// marshals as [] like cluster.list and status do.
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
