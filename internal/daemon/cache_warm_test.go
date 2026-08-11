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

func mustWarmArgs(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
