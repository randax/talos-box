package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

// A probe that failed must say why, so a slow or degraded API server is
// distinguishable from an unreachable one (#356).
func TestInspectDestroyClusterFallbackNamesProbeFailure(t *testing.T) {
	item := cluster.Cluster{Name: "demo", ProvisioningIntent: cluster.ProvisioningIntent{CSI: cluster.CSILonghorn}}
	server := &Server{
		destroyVolumeCount: func(context.Context, cluster.Cluster) (int, error) {
			return 0, errors.New("dial tcp 10.0.0.2:6443: connect: connection refused")
		},
	}

	result := server.inspectDestroyCluster(item)

	if !strings.Contains(result.Warning, "connection refused") {
		t.Fatalf("warning = %q, want the probe failure reason", result.Warning)
	}
}

// A cluster with no volumes has no data to lose: warning about zero of them is
// pure noise (#356).
func TestInspectDestroyClusterSuppressesWarningForZeroVolumes(t *testing.T) {
	item := cluster.Cluster{Name: "demo", ProvisioningIntent: cluster.ProvisioningIntent{CSI: cluster.CSILocalPath}}
	server := &Server{destroyVolumeCount: func(context.Context, cluster.Cluster) (int, error) { return 0, nil }}

	result := server.inspectDestroyCluster(item)

	if result.Warning != "" {
		t.Fatalf("warning = %q, want none for a cluster with no volumes", result.Warning)
	}
}

func TestDestroyInspectionCountWarningAgreesWithCount(t *testing.T) {
	for _, tc := range []struct {
		count int
		want  string
	}{
		{count: 1, want: "delete 1 longhorn volume and its data"},
		{count: 2, want: "delete 2 longhorn volumes and their data"},
	} {
		got := destroyInspectionCountWarning("demo", cluster.CSILonghorn, tc.count)
		if !strings.Contains(got, tc.want) {
			t.Fatalf("warning(%d) = %q, want %q", tc.count, got, tc.want)
		}
	}
}

func TestDestroyCountScheduleRetriesTransientFailures(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "persistentvolumes"}, "", errors.New("RBAC still settling"))
	for _, tc := range []struct {
		name         string
		err          error
		wantAttempts int
		wantCount    int
		wantErr      bool
	}{
		{name: "authz heals", err: forbidden, wantAttempts: 2, wantCount: 3},
		{name: "server unavailable heals", err: apierrors.NewServiceUnavailable("starting"), wantAttempts: 2, wantCount: 3},
		{name: "unreachable falls back at once", err: errors.New("connection refused"), wantAttempts: 1, wantErr: true},
		{name: "not found falls back at once", err: apierrors.NewNotFound(schema.GroupResource{Resource: "persistentvolumes"}, "pv"), wantAttempts: 1, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schedule := destroyCountSchedule{attempt: time.Second, budget: time.Second, interval: time.Millisecond}
			attempts := 0
			count, err := schedule.count(context.Background(), func(context.Context) (int, error) {
				attempts++
				if attempts == 1 {
					return 0, tc.err
				}
				return 3, nil
			})

			if attempts != tc.wantAttempts {
				t.Fatalf("attempts = %d, want %d", attempts, tc.wantAttempts)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("count() error = %v, wantErr %v", err, tc.wantErr)
			}
			if count != tc.wantCount {
				t.Fatalf("count = %d, want %d", count, tc.wantCount)
			}
		})
	}
}

// A transient failure that never heals must still fall back, keeping the last
// probe error as the reported reason.
func TestDestroyCountScheduleGivesUpAfterBudget(t *testing.T) {
	schedule := destroyCountSchedule{attempt: time.Second, budget: 10 * time.Millisecond, interval: time.Millisecond}
	attempts := 0
	unauthorized := apierrors.NewUnauthorized("no token")

	_, err := schedule.count(context.Background(), func(context.Context) (int, error) {
		attempts++
		return 0, unauthorized
	})

	if attempts < 2 {
		t.Fatalf("attempts = %d, want at least one retry", attempts)
	}
	if !errors.Is(err, unauthorized) {
		t.Fatalf("count() error = %v, want the last probe error", err)
	}
}

// Each attempt gets its own timeout so one hung call cannot eat the budget: a
// probe that never returns on its own has to be cut at the attempt bound and
// retried, which is only observable with an attempt far shorter than the budget
// and a count of how many attempts the budget bought.
func TestDestroyCountScheduleBoundsEachAttempt(t *testing.T) {
	schedule := destroyCountSchedule{attempt: 5 * time.Millisecond, budget: 100 * time.Millisecond, interval: time.Millisecond}
	unavailable := apierrors.NewServiceUnavailable("still starting")
	attempts := 0

	_, err := schedule.count(context.Background(), func(ctx context.Context) (int, error) {
		// a call that only ever ends when its own bound cuts it
		<-ctx.Done()
		attempts++
		return 0, unavailable
	})

	if attempts < 3 {
		t.Fatalf("attempts = %d, want the budget to buy several attempt-bounded calls", attempts)
	}
	if !errors.Is(err, unavailable) {
		t.Fatalf("count() error = %v, want the last probe error", err)
	}
}

// The inspection is bounded as a whole, not only per attempt: it runs on the
// request path of an interactive verb, and the daemon lifetime is no bound at
// all for an operator watching a silent socket (#356).
func TestInspectDestroyClusterBoundsTheWholeProbe(t *testing.T) {
	var deadline time.Time
	var bounded bool
	server := &Server{destroyVolumeCount: func(ctx context.Context, _ cluster.Cluster) (int, error) {
		deadline, bounded = ctx.Deadline()
		return 0, nil
	}}

	server.inspectDestroyCluster(cluster.Cluster{Name: "demo", ProvisioningIntent: cluster.ProvisioningIntent{CSI: cluster.CSILonghorn}})

	if !bounded {
		t.Fatal("destroy inspection probe ran without a deadline")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > destroyInspectionTimeout {
		t.Fatalf("probe deadline in %v, want a bound within %v", remaining, destroyInspectionTimeout)
	}
	if defaultDestroyCountSchedule.budget >= destroyInspectionTimeout {
		t.Fatalf("retry budget %v must fit inside the inspection bound %v", defaultDestroyCountSchedule.budget, destroyInspectionTimeout)
	}
}

// The inspection retries a control plane that can still heal, and a retry under
// the daemon's operation lock would freeze every other verb for the whole
// budget — on exactly the half-broken cluster an operator is destroying (#356).
func TestDestroyInspectDispatchDoesNotHoldTheOperationLock(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("demo", 0, 1, 0, cluster.NodeDefaults{MemoryMiB: 1, CPUs: 1, DiskGiB: 1})
	if err != nil {
		t.Fatal(err)
	}
	item.ProvisioningIntent = cluster.ProvisioningIntent{CSI: cluster.CSILonghorn}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	service := &Server{destroyVolumeCount: func(context.Context, cluster.Cluster) (int, error) { return 2, nil }}

	// Another operation owns the lock for the whole inspection.
	service.opMu.Lock()
	defer service.opMu.Unlock()

	answered := make(chan Response, 1)
	go func() {
		answered <- service.dispatch(Request{Op: "cluster.destroy.inspect", Args: mustRawJSON(t, destroyArgs{Name: item.Name, Force: true})})
	}()

	select {
	case response := <-answered:
		if !response.OK {
			t.Fatalf("cluster.destroy.inspect failed: %s", response.Error)
		}
		var inspection DestroyInspection
		if err := json.Unmarshal(response.Data, &inspection); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(inspection.Warning, "2 longhorn volumes") {
			t.Fatalf("warning = %q, want the counted data-loss warning", inspection.Warning)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cluster.destroy.inspect blocked on the operation lock")
	}
}

func TestDestroyCountScheduleStopsWhenContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	schedule := destroyCountSchedule{attempt: time.Second, budget: time.Minute, interval: time.Hour}

	_, err := schedule.count(ctx, func(context.Context) (int, error) {
		cancel()
		return 0, apierrors.NewUnauthorized("no token")
	})

	if err == nil {
		t.Fatal("count() error = nil, want the probe error after cancellation")
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
