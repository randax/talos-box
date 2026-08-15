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
			_, err := service.handle(Request{Op: "cache.warm", Args: mustWarmArgs(t, CacheWarmArgs{Refs: refs})})
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
	}); err != nil {
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
