package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/hypervisor"
	"github.com/randax/talos-box/internal/imagecache"
)

func TestCacheCheckRejectsUnpinnedRefsBeforeStartingCheck(t *testing.T) {
	t.Parallel()

	service := &Server{
		checkCache: func(context.Context, []string, imagecache.Architecture, bool) (CacheCheckResult, error) {
			t.Fatal("checkCache should not run for invalid refs")
			return CacheCheckResult{}, nil
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
			_, err := service.handle(Request{Op: "cache.check", Args: mustCheckArgs(t, CacheCheckArgs{Refs: refs})}, nil)
			if err == nil {
				t.Fatalf("cache.check accepted invalid refs %v", refs)
			}
		})
	}
}

func TestCacheCheckUsesBoundedContextAndOptions(t *testing.T) {
	t.Parallel()

	service := &Server{
		hypervisors: singleFakeRegistry(&fakeHypervisor{architecture: hypervisor.ArchitectureAMD64}),
		checkCache: func(ctx context.Context, refs []string, architecture imagecache.Architecture, deep bool) (CacheCheckResult, error) {
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("checkCache context has no deadline")
			}
			remaining := time.Until(deadline)
			if remaining <= 0 {
				t.Fatalf("checkCache deadline already expired: %s", remaining)
			}
			if remaining > cacheWarmTimeout {
				t.Fatalf("checkCache deadline too far out: %s > %s", remaining, cacheWarmTimeout)
			}
			if !deep {
				t.Fatal("checkCache deep = false, want true")
			}
			if architecture != imagecache.ArchitectureAMD64 {
				t.Fatalf("architecture = %q, want amd64", architecture)
			}
			return CacheCheckResult{}, nil
		},
	}

	if _, err := service.handle(Request{
		Op:   "cache.check",
		Args: mustCheckArgs(t, CacheCheckArgs{Refs: []string{"docker.io/library/nginx:1.27.0"}, Deep: true}),
	}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownCancelsInFlightCacheCheck(t *testing.T) {
	t.Parallel()

	lifecycle, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	service := &Server{
		hypervisors:      singleFakeRegistry(&fakeHypervisor{architecture: hypervisor.ArchitectureAMD64}),
		lifecycleContext: lifecycle,
		lifecycleCancel:  cancel,
		checkCache: func(ctx context.Context, refs []string, architecture imagecache.Architecture, deep bool) (CacheCheckResult, error) {
			if !deep {
				t.Fatal("checkCache deep = false, want true")
			}
			close(started)
			<-ctx.Done()
			return CacheCheckResult{}, ctx.Err()
		},
	}

	response := make(chan Response, 1)
	go func() {
		response <- service.dispatch(Request{
			Op:   "cache.check",
			Args: mustCheckArgs(t, CacheCheckArgs{Refs: []string{"docker.io/library/nginx:1.27.0"}, Deep: true}),
		})
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("cache.check did not start")
	}

	shutdown := make(chan error, 1)
	go func() {
		shutdown <- service.Shutdown()
	}()

	select {
	case got := <-response:
		if got.OK {
			t.Fatal("cache.check succeeded, want cancellation failure")
		}
		if !strings.Contains(got.Error, context.Canceled.Error()) {
			t.Fatalf("cache.check error = %q, want %q", got.Error, context.Canceled)
		}
	case <-time.After(time.Second):
		t.Fatal("cache.check did not return after shutdown cancellation")
	}

	select {
	case err := <-shutdown:
		if err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Shutdown() did not return after canceling cache.check")
	}
}

func mustCheckArgs(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
