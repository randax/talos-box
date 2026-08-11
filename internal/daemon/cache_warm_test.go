package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

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
	} {
		t.Run(strings.Join(refs, ","), func(t *testing.T) {
			_, err := service.handle(Request{Op: "cache.warm", Args: mustWarmArgs(t, CacheWarmArgs{Refs: refs})})
			if err == nil {
				t.Fatalf("cache.warm accepted invalid refs %v", refs)
			}
		})
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
