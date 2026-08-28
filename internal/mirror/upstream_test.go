package mirror

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRegistryRequestRetries429UsingRetryAfterSeconds(t *testing.T) {
	var hits atomic.Int64
	server := NewServer("https://registry.example", t.TempDir())
	server.client.Transport = roundTripFunc(func(_ *http.Request) *http.Response {
		if hits.Add(1) == 1 {
			return retryResponse(http.StatusTooManyRequests, "7")
		}
		return retryResponse(http.StatusOK, "")
	})

	var waits []time.Duration
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://registry.example/v2/demo/manifests/latest", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := server.doRegistryRequest(request, requestPolicy{Retry: retryPolicy{
		MaxAttempts: 3,
		MaxDelay:    30 * time.Second,
		Sleep: func(_ context.Context, delay time.Duration) error {
			waits = append(waits, delay)
			return nil
		},
		Now: time.Now,
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || hits.Load() != 2 {
		t.Fatalf("response = %d after %d hits, want 200 after 2", resp.StatusCode, hits.Load())
	}
	if len(waits) != 1 || waits[0] != 7*time.Second {
		t.Fatalf("waits = %v, want [7s]", waits)
	}
}

func TestRegistryRequestRetries429UsingRetryAfterHTTPDate(t *testing.T) {
	now := time.Date(2026, time.August, 28, 12, 0, 0, 0, time.UTC)
	var hits atomic.Int64
	server := NewServer("https://registry.example", t.TempDir())
	server.client.Transport = roundTripFunc(func(_ *http.Request) *http.Response {
		if hits.Add(1) == 1 {
			return retryResponse(http.StatusTooManyRequests, now.Add(12*time.Second).Format(http.TimeFormat))
		}
		return retryResponse(http.StatusOK, "")
	})

	var wait time.Duration
	request, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://registry.example/v2/demo/manifests/latest", nil)
	resp, err := server.doRegistryRequest(request, requestPolicy{Retry: retryPolicy{MaxAttempts: 3, MaxDelay: 30 * time.Second, Now: func() time.Time { return now }, Sleep: func(_ context.Context, delay time.Duration) error {
		wait = delay
		return nil
	}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if wait != 12*time.Second {
		t.Fatalf("wait = %s, want 12s", wait)
	}
}

func TestRegistryRequestFallsBackToBoundedBackoffWithoutRetryAfter(t *testing.T) {
	var hits atomic.Int64
	server := NewServer("https://registry.example", t.TempDir())
	server.client.Transport = roundTripFunc(func(_ *http.Request) *http.Response {
		hits.Add(1)
		return retryResponse(http.StatusTooManyRequests, "")
	})

	var waits []time.Duration
	request, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://registry.example/v2/demo/manifests/latest", nil)
	resp, err := server.doRegistryRequest(request, requestPolicy{Retry: retryPolicy{MaxAttempts: 3, MaxDelay: 30 * time.Second, Now: time.Now, Sleep: func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return nil
	}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || hits.Load() != 3 {
		t.Fatalf("response = %d after %d hits, want 429 after 3", resp.StatusCode, hits.Load())
	}
	if fmt.Sprint(waits) != "[500ms 1s]" {
		t.Fatalf("waits = %v, want [500ms 1s]", waits)
	}
}

type roundTripFunc func(*http.Request) *http.Response

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request), nil
}

func retryResponse(status int, retryAfter string) *http.Response {
	header := make(http.Header)
	if retryAfter != "" {
		header.Set("Retry-After", retryAfter)
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader("response"))}
}

func TestRegistryRequestCapsExcessiveRetryAfterAtThirtySeconds(t *testing.T) {
	if delay, ok := parseRetryAfter("3600", time.Now()); !ok || delay != time.Hour {
		t.Fatalf("parseRetryAfter = %s, %v; want 1h, true", delay, ok)
	}
	policy := normalizedRetryPolicy(retryPolicy{MaxAttempts: 3, MaxDelay: 30 * time.Second, Now: time.Now, Sleep: func(context.Context, time.Duration) error { return nil }})
	if delay := policy.retryDelay("3600", 0); delay != 30*time.Second {
		t.Fatalf("retry delay = %s, want 30s", delay)
	}
}

func TestRegistryRetryWaitStopsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepContext(ctx, time.Second); err != context.Canceled {
		t.Fatalf("sleepContext error = %v, want context canceled", err)
	}
}

func TestPublicECRBearerChallengeUsesAdvertisedTokenEndpoint(t *testing.T) {
	server := NewServer("https://public.ecr.aws", t.TempDir())
	var tokenQuery string
	server.client.Transport = roundTripFunc(func(request *http.Request) *http.Response {
		switch request.URL.Host {
		case "public.ecr.aws":
			if request.Header.Get("Authorization") == "Bearer ecr-token" {
				return retryResponse(http.StatusOK, "")
			}
			resp := retryResponse(http.StatusUnauthorized, "")
			resp.Header.Set("WWW-Authenticate", `Bearer realm="https://auth.example/token",service="public.ecr.aws",scope="repository:docker/library/redis:pull"`)
			return resp
		case "auth.example":
			tokenQuery = request.URL.RawQuery
			resp := retryResponse(http.StatusOK, "")
			resp.Body = io.NopCloser(strings.NewReader(`{"access_token":"ecr-token"}`))
			return resp
		default:
			return retryResponse(http.StatusNotFound, "")
		}
	})
	request, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://public.ecr.aws/v2/docker/library/redis/manifests/latest", nil)
	resp, err := server.doRegistryRequest(request, immediateRequestPolicy())
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{"service=public.ecr.aws", "scope=repository%3Adocker%2Flibrary%2Fredis%3Apull"} {
		if !strings.Contains(tokenQuery, want) {
			t.Fatalf("token query = %q, want %q", tokenQuery, want)
		}
	}
}

func TestTokenEndpointRetries429UsingRetryAfter(t *testing.T) {
	server := NewServer("https://registry.example", t.TempDir())
	var tokenHits atomic.Int64
	var waits []time.Duration
	server.client.Transport = roundTripFunc(func(request *http.Request) *http.Response {
		if request.URL.Host == "token.example" {
			if tokenHits.Add(1) == 1 {
				return retryResponse(http.StatusTooManyRequests, "2")
			}
			resp := retryResponse(http.StatusOK, "")
			resp.Body = io.NopCloser(strings.NewReader(`{"access_token":"token"}`))
			return resp
		}
		if request.Header.Get("Authorization") == "Bearer token" {
			return retryResponse(http.StatusOK, "")
		}
		resp := retryResponse(http.StatusUnauthorized, "")
		resp.Header.Set("WWW-Authenticate", `Bearer realm="https://token.example/token",service="registry.example"`)
		return resp
	})
	policy := defaultRequestPolicy()
	policy.Retry.Sleep = func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return nil
	}
	request, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://registry.example/v2/demo/manifests/latest", nil)
	resp, err := server.doRegistryRequest(request, policy)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || tokenHits.Load() != 2 {
		t.Fatalf("response = %d, token hits = %d; want 200, 2", resp.StatusCode, tokenHits.Load())
	}
	if fmt.Sprint(waits) != "[2s]" {
		t.Fatalf("waits = %v, want [2s]", waits)
	}
}

func TestRegistryRequestStopsAfterThree429Responses(t *testing.T) {
	var hits atomic.Int64
	server := NewServer("https://registry.example", t.TempDir())
	server.client.Transport = roundTripFunc(func(*http.Request) *http.Response {
		hits.Add(1)
		return retryResponse(http.StatusTooManyRequests, "0")
	})
	request, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://registry.example/v2/demo/manifests/latest", nil)
	resp, err := server.doRegistryRequest(request, defaultRequestPolicy())
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || hits.Load() != 3 {
		t.Fatalf("response = %d after %d hits, want 429 after exactly 3", resp.StatusCode, hits.Load())
	}
}

func TestRegistryBearerHandlerRemainsGenericAcrossDockerHubECRAndGHCR(t *testing.T) {
	for _, test := range []struct {
		name, service, scope, field string
	}{
		{name: "docker hub scopeless", service: "registry.docker.io", field: "token"},
		{name: "public ecr scoped", service: "public.ecr.aws", scope: "repository:docker/library/redis:pull", field: "access_token"},
		{name: "ghcr scoped", service: "ghcr.io", scope: "repository:owner/app:pull", field: "token"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := NewServer("https://registry.example", t.TempDir())
			var query string
			server.client.Transport = roundTripFunc(func(request *http.Request) *http.Response {
				if request.URL.Host == "auth.example" {
					query = request.URL.RawQuery
					resp := retryResponse(http.StatusOK, "")
					resp.Body = io.NopCloser(strings.NewReader(fmt.Sprintf(`{"%s":"generic-token"}`, test.field)))
					return resp
				}
				if request.Header.Get("Authorization") == "Bearer generic-token" {
					return retryResponse(http.StatusOK, "")
				}
				challenge := fmt.Sprintf(`Bearer realm="https://auth.example/token",service=%q`, test.service)
				if test.scope != "" {
					challenge += fmt.Sprintf(`,scope=%q`, test.scope)
				}
				resp := retryResponse(http.StatusUnauthorized, "")
				resp.Header.Set("WWW-Authenticate", challenge)
				return resp
			})
			request, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://registry.example/v2/owner/app/manifests/latest", nil)
			resp, err := server.doRegistryRequest(request, immediateRequestPolicy())
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK || !strings.Contains(query, "service=") || !strings.Contains(query, "scope=") {
				t.Fatalf("response = %d, token query = %q", resp.StatusCode, query)
			}
		})
	}
}
