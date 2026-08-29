package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/hypervisor"
	"github.com/randax/talos-box/internal/imagecache"
	"github.com/randax/talos-box/internal/mirror"
)

func TestCacheWarmForwardsRefreshOptionAndTypedCounts(t *testing.T) {
	t.Parallel()

	ref := "docker.io/library/nginx:1.27.0"
	service := &Server{
		hypervisor: &fakeHypervisor{architecture: hypervisor.ArchitectureAMD64},
		warmCacheWithOptions: func(_ context.Context, refs []string, architecture imagecache.Architecture, options mirror.WarmOptions) (mirror.WarmSummary, error) {
			if len(refs) != 1 || refs[0] != ref {
				t.Fatalf("refs = %v, want [%q]", refs, ref)
			}
			if architecture != imagecache.ArchitectureAMD64 {
				t.Fatalf("architecture = %q, want %q", architecture, imagecache.ArchitectureAMD64)
			}
			if !options.Refresh {
				t.Fatal("Refresh = false, want true")
			}
			if options.Jobs != 3 {
				t.Fatalf("Jobs = %d, want 3", options.Jobs)
			}
			return mirror.WarmSummary{
				Results: []mirror.WarmResult{
					{Ref: ref, Outcome: mirror.WarmOutcomeFailedMissing, Error: "layer missing", ReResolvedTag: true},
					{Ref: "registry.example/app:v1", Outcome: mirror.WarmOutcomeFailedRevalidate, Error: "upstream 404"},
				},
				Failed:           2,
				FailedMissing:    1,
				FailedRevalidate: 1,
				ReResolvedTags:   1,
			}, nil
		},
	}

	value, err := service.handle(Request{
		Op:   "cache.warm",
		Args: mustWarmArgs(t, CacheWarmArgs{Refs: []string{ref}, Refresh: true, Jobs: 3}),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, ok := value.(CacheWarmResult)
	if !ok {
		t.Fatalf("result type = %T, want CacheWarmResult", value)
	}
	if result.FailedMissing != 1 || result.FailedRevalidate != 1 {
		t.Fatalf("typed failures = missing %d, revalidate %d; want 1 and 1", result.FailedMissing, result.FailedRevalidate)
	}
	if result.ReResolvedTags != 1 || !result.Entries[0].ReResolvedTag {
		t.Fatalf("re-resolved fields = %+v, want summary=1 and first entry=true", result)
	}
	if result.Entries[0].Status != CacheWarmStatusFailedMissing || result.Entries[1].Status != CacheWarmStatusFailedRevalidate {
		t.Fatalf("statuses = %q, %q; want typed failures", result.Entries[0].Status, result.Entries[1].Status)
	}
}

func TestCacheWarmNarratesEachFinishedEntryInOrder(t *testing.T) {
	t.Parallel()
	refs := []string{"registry.example/one:v1", "registry.example/two:v1"}
	service := &Server{
		hypervisor: &fakeHypervisor{architecture: hypervisor.ArchitectureAMD64},
		warmCacheWithOptions: func(_ context.Context, refs []string, _ imagecache.Architecture, options mirror.WarmOptions) (mirror.WarmSummary, error) {
			if options.OnResult == nil {
				t.Fatal("OnResult = nil, want narration wired for a narrated request")
			}
			var summary mirror.WarmSummary
			for i, ref := range refs {
				result := mirror.WarmResult{Ref: ref, Outcome: mirror.WarmOutcomeWarmed}
				if i == 1 {
					result = mirror.WarmResult{Ref: ref, Outcome: mirror.WarmOutcomeFailedRevalidate, Error: "upstream 404"}
				}
				summary.Results = append(summary.Results, result)
				options.OnResult(result)
			}
			return summary, nil
		},
	}
	var stages []string
	value, err := service.handle(Request{
		Op:   "cache.warm",
		Args: mustWarmArgs(t, CacheWarmArgs{Refs: refs, Jobs: 4}),
	}, func(stage string) { stages = append(stages, stage) })
	if err != nil {
		t.Fatal(err)
	}
	result := value.(CacheWarmResult)
	if len(stages) != 2 {
		t.Fatalf("stages = %q, want one per ref", stages)
	}
	for i, stage := range stages {
		entry, ok := ParseCacheWarmEntryStage(stage)
		if !ok {
			t.Fatalf("stage %q did not parse as a warm entry", stage)
		}
		if entry != result.Entries[i] {
			t.Fatalf("narrated entry %d = %+v, want the final entry %+v", i, entry, result.Entries[i])
		}
	}
	if _, ok := ParseCacheWarmEntryStage("waiting for the daemon's current operation to finish"); ok {
		t.Fatal("plain narration parsed as a warm entry")
	}
}

func TestCacheWarmSkipsNarrationWithoutAListener(t *testing.T) {
	t.Parallel()
	service := &Server{
		hypervisor: &fakeHypervisor{architecture: hypervisor.ArchitectureAMD64},
		warmCacheWithOptions: func(_ context.Context, _ []string, _ imagecache.Architecture, options mirror.WarmOptions) (mirror.WarmSummary, error) {
			if options.OnResult != nil {
				t.Fatal("OnResult set for an unnarrated request")
			}
			return mirror.WarmSummary{}, nil
		},
	}
	if _, err := service.handle(Request{Op: "cache.warm", Args: mustWarmArgs(t, CacheWarmArgs{Refs: []string{"registry.example/one:v1"}})}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCacheWarmRejectsJobsOutsideItsRange(t *testing.T) {
	t.Parallel()
	service := &Server{
		hypervisor: &fakeHypervisor{architecture: hypervisor.ArchitectureAMD64},
		warmCacheWithOptions: func(context.Context, []string, imagecache.Architecture, mirror.WarmOptions) (mirror.WarmSummary, error) {
			t.Fatal("warm must not start with negative jobs")
			return mirror.WarmSummary{}, nil
		},
	}
	for _, jobs := range []int{-1, MaxCacheWarmJobs + 1} {
		_, err := service.handle(Request{
			Op:   "cache.warm",
			Args: mustWarmArgs(t, CacheWarmArgs{Refs: []string{"docker.io/library/nginx:1.27.0"}, Jobs: jobs}),
		}, nil)
		if err == nil || !strings.Contains(err.Error(), "jobs must be between 0 and 16") {
			t.Fatalf("jobs %d: err = %v, want range rejection", jobs, err)
		}
	}
}

func TestCacheWarmResultJSONKeepsOldAndNewFields(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(CacheWarmResult{
		Entries: []CacheWarmEntry{{
			Ref:            "registry.example/demo:stable",
			Status:         CacheWarmStatusAlreadyComplete,
			Reason:         "complete",
			RefreshWarning: "upstream unavailable, not revalidated",
			ReResolvedTag:  true,
		}},
		Warmed:           0,
		AlreadyComplete:  1,
		Failed:           0,
		FailedMissing:    0,
		FailedRevalidate: 0,
		ReResolvedTags:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"reResolvedTag":true`, `"reResolvedTags":1`, `"failedMissing":0`, `"failedRevalidate":0`} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("CacheWarmResult JSON = %s, want %s", encoded, want)
		}
	}
}

func TestCacheWarmRejectsUnpinnedRefsBeforeStartingWarm(t *testing.T) {
	t.Parallel()

	service := &Server{
		warmCache: func(context.Context, []string, imagecache.Architecture) (CacheWarmResult, error) {
			t.Fatal("warmCache should not run for invalid refs")
			return CacheWarmResult{}, nil
		},
	}
	for _, refs := range [][]string{
		{"docker.io/library/nginx:latest"},
		{"docker.io/library/nginx"},
		{"library/nginx:1.2.3"},
		{"docker.io/library/nginx@sha256:not-hex"},
		{"docker.io/library/nginx@sha512:abcd"},
		{"docker.io/library/repo:@sha256:1111111111111111111111111111111111111111111111111111111111111111"},
		{"docker.io/:tag"},
		{"docker.io/library/nginx:1.2.3?query"},
		{"docker.io/library/nginx:1.2.3#fragment"},
		{"docker.io/library/ bad:1.2.3"},
		{"registry.example/repo:one:two"},
		{"registry.example/Uppercase/repo:1.2.3"},
		{"registry.example/repo:-not-a-tag"},
		{"registry.example:0/repo:tag"},
		{"registry.example:99999/repo:tag"},
		{strings.Repeat("a", 64) + ".example/repo:tag"},
		{strings.Repeat("a.", 126) + "aaaa/repo:tag"},
	} {
		t.Run(strings.Join(refs, ","), func(t *testing.T) {
			_, err := service.handle(Request{Op: "cache.warm", Args: mustWarmArgs(t, CacheWarmArgs{Refs: refs})}, nil)
			if err == nil {
				t.Fatalf("cache.warm accepted invalid refs %v", refs)
			}
		})
	}
}

func TestCacheWarmUsesBoundedContext(t *testing.T) {
	t.Parallel()

	service := &Server{
		hypervisor: &fakeHypervisor{architecture: hypervisor.ArchitectureAMD64},
		warmCache: func(ctx context.Context, refs []string, architecture imagecache.Architecture) (CacheWarmResult, error) {
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("warmCache context has no deadline")
			}
			remaining := time.Until(deadline)
			if remaining <= 0 {
				t.Fatalf("warmCache deadline already expired: %s", remaining)
			}
			if remaining > cacheWarmTimeout {
				t.Fatalf("warmCache deadline too far out: %s > %s", remaining, cacheWarmTimeout)
			}
			return CacheWarmResult{}, nil
		},
	}

	if _, err := service.handle(Request{
		Op:   "cache.warm",
		Args: mustWarmArgs(t, CacheWarmArgs{Refs: []string{"docker.io/library/nginx:1.27.0"}}),
	}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestValidateWarmRefAcceptsDistributionRepositoryAndIPv6AuthorityGrammar(t *testing.T) {
	t.Parallel()

	for _, ref := range []string{
		"registry.example/team/foo--bar__baz:1",
		"REGISTRY.example/team/foo:1",
		"[2001:db8::1]:5000/repo:tag",
	} {
		t.Run(ref, func(t *testing.T) {
			if err := ValidateWarmRef(ref); err != nil {
				t.Fatalf("ValidateWarmRef(%q) = %v, want nil", ref, err)
			}
		})
	}
}

func TestShutdownCancelsInFlightCacheWarm(t *testing.T) {
	t.Parallel()

	lifecycle, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	service := &Server{
		hypervisor:       &fakeHypervisor{architecture: hypervisor.ArchitectureAMD64},
		lifecycleContext: lifecycle,
		lifecycleCancel:  cancel,
		warmCache: func(ctx context.Context, refs []string, architecture imagecache.Architecture) (CacheWarmResult, error) {
			if got, want := architecture, imagecache.ArchitectureAMD64; got != want {
				t.Fatalf("architecture = %q, want %q", got, want)
			}
			close(started)
			<-ctx.Done()
			return CacheWarmResult{}, ctx.Err()
		},
	}

	response := make(chan Response, 1)
	go func() {
		response <- service.dispatch(Request{
			Op:   "cache.warm",
			Args: mustWarmArgs(t, CacheWarmArgs{Refs: []string{"docker.io/library/nginx:1.27.0"}}),
		})
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("cache.warm did not start")
	}

	shutdown := make(chan error, 1)
	go func() {
		shutdown <- service.Shutdown()
	}()

	select {
	case got := <-response:
		if got.OK {
			t.Fatal("cache.warm succeeded, want cancellation failure")
		}
		if !strings.Contains(got.Error, context.Canceled.Error()) {
			t.Fatalf("cache.warm error = %q, want %q", got.Error, context.Canceled)
		}
	case <-time.After(time.Second):
		t.Fatal("cache.warm did not return after shutdown cancellation")
	}

	select {
	case err := <-shutdown:
		if err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Shutdown() did not return after canceling cache.warm")
	}
}

func mustWarmArgs(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
