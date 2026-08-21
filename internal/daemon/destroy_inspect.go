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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// DestroyInspection is the best-effort storage data-loss warning surfaced
// before a forced cluster destroy. Volumes and CSI carry the same finding in
// countable form, so the destroy's own summary can account for the volumes it
// warned about (#422); both are zero when the count could not be taken.
type DestroyInspection struct {
	Warning string      `json:"warning,omitempty"`
	Volumes int         `json:"volumes,omitempty"`
	CSI     cluster.CSI `json:"csi,omitempty"`
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
	// The inspection is what a destroy prints its confirmation from, so it is
	// bounded by what an interactive verb can absorb rather than by the daemon
	// lifetime: an unreachable control plane must fall back to the generic
	// warning, not hold the operator at a silent socket (#356).
	ctx, cancel := context.WithTimeout(s.lifecycle(), destroyInspectionTimeout)
	defer cancel()
	count, err := s.destroyVolumeCount(ctx, item)
	if err != nil {
		return DestroyInspection{Warning: destroyInspectionProbeFailureWarning(item.Name, item.CSI, err)}
	}
	// A cluster with no volumes has no data to lose, so a warning about zero
	// of them is noise (#356).
	if count == 0 {
		return DestroyInspection{}
	}
	return DestroyInspection{
		Warning: destroyInspectionCountWarning(item.Name, item.CSI, count),
		Volumes: count,
		CSI:     item.CSI,
	}
}

func countDestroyStorageVolumes(ctx context.Context, item cluster.Cluster) (int, error) {
	kubeconfig, err := clusterKubeconfig(item.Name)
	if err != nil {
		return 0, fmt.Errorf("read kubeconfig for destroy inspection: %w", err)
	}
	return defaultDestroyCountSchedule.count(ctx, func(probeCtx context.Context) (int, error) {
		return provision.CountProvisionedStorageVolumes(probeCtx, kubeconfig, item.CSI)
	})
}

// listStorageVolumeClaims names the claims a curated engine still serves. The
// csi gate refuses on them, so the refusal can point at the volumes to delete
// rather than only count them (#393); it shares the destroy probe's retry
// schedule because it reads the same objects through the same cold API server.
func listStorageVolumeClaims(ctx context.Context, item cluster.Cluster) ([]string, error) {
	kubeconfig, err := clusterKubeconfig(item.Name)
	if err != nil {
		return nil, fmt.Errorf("read kubeconfig for storage volume inspection: %w", err)
	}
	var claims []string
	if _, err := defaultDestroyCountSchedule.count(ctx, func(probeCtx context.Context) (int, error) {
		found, err := provision.ProvisionedStorageVolumeClaims(probeCtx, kubeconfig, item.CSI)
		if err != nil {
			return 0, err
		}
		claims = found
		return len(found), nil
	}); err != nil {
		return nil, err
	}
	return claims, nil
}

// destroyCountSchedule bounds the volume-count probe: every attempt gets its
// own timeout, and attempts that can still heal are retried until the budget
// runs out. Without the retry a cold-booted API server — which answers authz
// errors for about a minute while RBAC settles — is indistinguishable from an
// unreachable one, and the destroy warns about volumes it could have counted
// (#356).
//
// The budget is what an operator waiting on `tbx cluster destroy` can absorb
// before the silence is worse than the fallback warning, not how long RBAC may
// take to settle: the destroy that follows does not need the count, and a
// longer retry only delays the confirmation prompt.
type destroyCountSchedule struct {
	attempt  time.Duration
	budget   time.Duration
	interval time.Duration
}

var defaultDestroyCountSchedule = destroyCountSchedule{
	attempt:  5 * time.Second,
	budget:   10 * time.Second,
	interval: 2 * time.Second,
}

// destroyInspectionTimeout is the whole inspection's bound, schedule included:
// the retry budget is per-probe arithmetic, and only a deadline over the lot of
// it keeps the verb's silence to something an operator recognises as a pause.
const destroyInspectionTimeout = 15 * time.Second

func (s destroyCountSchedule) count(ctx context.Context, probe func(context.Context) (int, error)) (int, error) {
	deadline := time.Now().Add(s.budget)
	for {
		count, err := s.attemptCount(ctx, probe)
		if err == nil || !destroyCountRetryable(err) || !time.Now().Before(deadline) {
			return count, err
		}
		select {
		case <-ctx.Done():
			return 0, err
		case <-time.After(s.interval):
		}
	}
}

func (s destroyCountSchedule) attemptCount(ctx context.Context, probe func(context.Context) (int, error)) (int, error) {
	probeCtx, cancel := context.WithTimeout(ctx, s.attempt)
	defer cancel()
	return probe(probeCtx)
}

// destroyCountRetryable reports whether a failed count can still heal by
// waiting. Authz and server-side refusals do while a freshly booted control
// plane settles; an unreachable endpoint, a bad kubeconfig or a missing
// resource never will, so those fall back immediately.
func destroyCountRetryable(err error) bool {
	return apierrors.IsUnauthorized(err) ||
		apierrors.IsForbidden(err) ||
		apierrors.IsServerTimeout(err) ||
		apierrors.IsTooManyRequests(err) ||
		apierrors.IsInternalError(err) ||
		apierrors.IsServiceUnavailable(err)
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

// destroyInspectionProbeFailureWarning keeps the reason the count is unknown
// in the warning, so a degraded probe is diagnosable instead of silent (#356).
func destroyInspectionProbeFailureWarning(name string, engine cluster.CSI, err error) string {
	return fmt.Sprintf("%s (volume count unavailable: %v)", DestroyInspectionDataLossWarning(name, engine), err)
}

func destroyInspectionCountWarning(name string, engine cluster.CSI, count int) string {
	return fmt.Sprintf(
		"destroying cluster %s will permanently delete %d %s %s and %s data",
		name,
		count,
		strings.TrimSpace(string(engine)),
		Unit(count, "volume", "volumes"),
		volumePossessive(count),
	)
}

func volumePossessive(count int) string {
	if count == 1 {
		return "its"
	}
	return "their"
}
