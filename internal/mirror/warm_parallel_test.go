package mirror

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/imagecache"
)

// parallelWarmFixture serves `repos`, each an image whose layers are served
// only after the transport observes `inFlight` concurrent blob requests or
// gives up waiting — so a test can prove how many downloads warm keeps in
// flight (#506).
type parallelWarmFixture struct {
	manager  *Manager
	refs     []string
	blobs    []string
	inFlight atomic.Int64
	peak     atomic.Int64
	gate     chan struct{}
	failBlob string
	// shareLayers gives every image the same layer bytes, so refs share a blob
	shareLayers bool
	onBlob      func(digest string)
	// holdBlob blocks that one digest on hold instead of the gate
	holdBlob string
	hold     chan struct{}
}

func newParallelWarmFixture(t *testing.T, repos []string, layersPerImage int, options ...func(*parallelWarmFixture)) *parallelWarmFixture {
	t.Helper()
	f := &parallelWarmFixture{gate: make(chan struct{}), hold: make(chan struct{})}
	for _, option := range options {
		option(f)
	}
	type image struct {
		manifest, digest string
		blobs            map[string][]byte
	}
	images := map[string]image{}
	for _, repo := range repos {
		blobs := map[string][]byte{}
		var layers []string
		for i := 0; i < layersPerImage; i++ {
			data := []byte(fmt.Sprintf("%s-layer-%d", repo, i))
			if f.shareLayers {
				data = []byte(fmt.Sprintf("shared-layer-%d", i))
			}
			digest := "sha256:" + sha256Hex(data)
			blobs[digest] = data
			layers = append(layers, fmt.Sprintf(`{"digest":"%s"}`, digest))
			f.blobs = append(f.blobs, digest)
		}
		config := []byte(repo + "-config")
		configDigest := "sha256:" + sha256Hex(config)
		blobs[configDigest] = config
		manifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"%s"},"layers":[%s]}`, configDigest, strings.Join(layers, ","))
		images[repo] = image{manifest: manifest, digest: "sha256:" + sha256Hex([]byte(manifest)), blobs: blobs}
		f.refs = append(f.refs, "registry.example/"+repo+":stable")
	}
	f.manager = newManagerWithPorts(t.TempDir(), nil, 0)
	f.manager.serverFactory = func(_, base, cacheDir string) http.Handler {
		server := NewServer(base, cacheDir)
		server.client.Transport = warmRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			response := &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), Request: request}
			parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/v2/"), "/")
			img, ok := images[parts[0]]
			if !ok || len(parts) != 3 {
				response.StatusCode, response.Status = http.StatusNotFound, "404 Not Found"
				response.Body = io.NopCloser(strings.NewReader("not found"))
				return response, nil
			}
			switch parts[1] {
			case "manifests":
				response.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
				response.Header.Set("Docker-Content-Digest", img.digest)
				response.Body = io.NopCloser(strings.NewReader(img.manifest))
			case "blobs":
				if parts[2] == f.failBlob {
					response.StatusCode, response.Status = http.StatusInternalServerError, "500 Internal Server Error"
					response.Body = io.NopCloser(strings.NewReader("boom"))
					return response, nil
				}
				if f.onBlob != nil {
					f.onBlob(parts[2])
				}
				n := f.inFlight.Add(1)
				for {
					peak := f.peak.Load()
					if n <= peak || f.peak.CompareAndSwap(peak, n) {
						break
					}
				}
				wait := f.gate
				if parts[2] == f.holdBlob {
					wait = f.hold
				}
				select {
				case <-wait:
				case <-request.Context().Done():
					f.inFlight.Add(-1)
					return nil, request.Context().Err()
				}
				f.inFlight.Add(-1)
				response.Body = io.NopCloser(bytes.NewReader(img.blobs[parts[2]]))
			}
			return response, nil
		})
		return server
	}
	t.Cleanup(f.manager.Close)
	return f
}

// openGateWhen releases every blob once `want` are in flight, or after a grace
// period so a serialized warm still finishes; it reports which happened.
func (f *parallelWarmFixture) openGateWhen(want int64, grace time.Duration) <-chan bool {
	reached := make(chan bool, 1)
	go func() {
		deadline := time.Now().Add(grace)
		for time.Now().Before(deadline) {
			if f.inFlight.Load() >= want {
				close(f.gate)
				reached <- true
				return
			}
			time.Sleep(time.Millisecond)
		}
		close(f.gate)
		reached <- false
	}()
	return reached
}

func TestWarmDownloadsBlobsOfOneImageInParallel(t *testing.T) {
	f := newParallelWarmFixture(t, []string{"app"}, 4)
	reached := f.openGateWhen(5, 5*time.Second) // 4 layers + config
	summary, err := f.manager.Warm(context.Background(), f.refs, imagecache.ArchitectureAMD64, WarmOptions{Jobs: 8})
	if err != nil {
		t.Fatal(err)
	}
	if !<-reached {
		t.Fatalf("blob downloads never ran 5-wide; peak in flight = %d", f.peak.Load())
	}
	if summary.Warmed != 1 || summary.Failed != 0 {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestWarmDownloadsBlobsAcrossRefsUnderOneBudget(t *testing.T) {
	f := newParallelWarmFixture(t, []string{"one", "two", "three"}, 2)
	reached := f.openGateWhen(4, 5*time.Second)
	summary, err := f.manager.Warm(context.Background(), f.refs, imagecache.ArchitectureAMD64, WarmOptions{Jobs: 4})
	if err != nil {
		t.Fatal(err)
	}
	if !<-reached {
		t.Fatalf("blob downloads never ran 4-wide across refs; peak = %d", f.peak.Load())
	}
	if peak := f.peak.Load(); peak > 4 {
		t.Fatalf("peak in flight = %d, want <= jobs (4)", peak)
	}
	if summary.Warmed != 3 || summary.Failed != 0 {
		t.Fatalf("summary = %+v", summary)
	}
	for i, ref := range f.refs {
		if summary.Results[i].Ref != ref {
			t.Fatalf("results[%d].Ref = %q, want list order %q", i, summary.Results[i].Ref, ref)
		}
	}
}

func TestWarmJobsOneDownloadsSerially(t *testing.T) {
	f := newParallelWarmFixture(t, []string{"one", "two"}, 3)
	reached := f.openGateWhen(2, 300*time.Millisecond)
	summary, err := f.manager.Warm(context.Background(), f.refs, imagecache.ArchitectureAMD64, WarmOptions{Jobs: 1})
	if err != nil {
		t.Fatal(err)
	}
	if <-reached {
		t.Fatal("jobs=1 ran two blob downloads at once")
	}
	if peak := f.peak.Load(); peak != 1 {
		t.Fatalf("peak in flight = %d, want 1", peak)
	}
	if summary.Warmed != 2 {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestWarmJobsAreCappedByTheDaemonWideLimit(t *testing.T) {
	f := newParallelWarmFixture(t, []string{"big"}, MaxWarmJobs+8)
	reached := f.openGateWhen(MaxWarmJobs+1, 2*time.Second)
	if _, err := f.manager.Warm(context.Background(), f.refs, imagecache.ArchitectureAMD64, WarmOptions{Jobs: 1000}); err != nil {
		t.Fatal(err)
	}
	if <-reached {
		t.Fatalf("peak in flight = %d, want <= %d", f.peak.Load(), MaxWarmJobs)
	}
	if peak := f.peak.Load(); peak < 2 || peak > MaxWarmJobs {
		t.Fatalf("peak in flight = %d, want parallel but within the cap %d", peak, MaxWarmJobs)
	}
}

func TestWarmFirstBlobErrorStopsStartingSiblingsAndOtherRefsContinue(t *testing.T) {
	var mu sync.Mutex
	fetched := map[string]int{}
	f := newParallelWarmFixture(t, []string{"bad", "good"}, 3, func(f *parallelWarmFixture) {
		f.onBlob = func(digest string) {
			mu.Lock()
			fetched[digest]++
			mu.Unlock()
		}
	})
	f.failBlob = f.blobs[0] // bad's first layer; its config is requested before it
	<-f.openGateWhen(0, 0)  // nothing waits: each blob is served as soon as it is asked for
	summary, err := f.manager.Warm(context.Background(), f.refs, imagecache.ArchitectureAMD64, WarmOptions{Jobs: 1})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Failed != 1 || summary.Warmed != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	bad := summary.Results[0]
	if bad.Outcome != WarmOutcomeFailedMissing || !strings.Contains(bad.Error, "500") || strings.Contains(bad.Error, "context canceled") {
		t.Fatalf("bad result = %+v, want the 500 and not the stop's fallout", bad)
	}
	if summary.Results[1].Outcome != WarmOutcomeWarmed {
		t.Fatalf("good result = %+v", summary.Results[1])
	}
	mu.Lock()
	defer mu.Unlock()
	// with one download at a time the failure lands before bad's other two
	// layers start, and they must then not start at all
	for _, layer := range f.blobs[1:3] {
		if fetched[layer] != 0 {
			t.Fatalf("bad's layer %s was fetched after a sibling failed", layer)
		}
	}
	for _, layer := range f.blobs[3:6] {
		if fetched[layer] != 1 {
			t.Fatalf("good's layer %s fetched %d times, want 1", layer, fetched[layer])
		}
	}
}

func TestWarmFirstBlobErrorLetsInFlightSiblingsFinish(t *testing.T) {
	f := newParallelWarmFixture(t, []string{"bad"}, 3)
	// blobs start in manifest order (config, then layers), so with the last
	// layer failing the config and two healthy layers are already in flight,
	// blocked on the gate, when the failure lands. Release them then.
	f.failBlob = f.blobs[2]
	reached := f.openGateWhen(3, 5*time.Second)
	summary, err := f.manager.Warm(context.Background(), f.refs, imagecache.ArchitectureAMD64, WarmOptions{Jobs: 8})
	if err != nil {
		t.Fatal(err)
	}
	if !<-reached {
		t.Fatalf("healthy siblings never ran alongside the failing layer; peak = %d", f.peak.Load())
	}
	if summary.Failed != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	f.manager.mu.Lock()
	server := f.manager.dynamicServers["registry.example"]
	f.manager.mu.Unlock()
	if server == nil {
		t.Fatal("no dynamic server for registry.example")
	}
	for _, layer := range f.blobs[0:2] {
		if !blobCached(server, layer) {
			t.Fatalf("in-flight layer %s was discarded after the sibling failed; it must stay for the rerun", layer)
		}
	}
}

func TestWarmSerializesOnlySameTag(t *testing.T) {
	f := newParallelWarmFixture(t, []string{"one", "two"}, 1)
	// two distinct tags: both must be in flight at once (2 layers + 2 configs)
	reached := f.openGateWhen(4, 5*time.Second)
	if _, err := f.manager.Warm(context.Background(), f.refs, imagecache.ArchitectureAMD64, WarmOptions{Jobs: 8}); err != nil {
		t.Fatal(err)
	}
	if !<-reached {
		t.Fatalf("distinct tags were serialized; peak = %d", f.peak.Load())
	}

	var k keyedMutex
	unlockA, err := k.lock(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	unlockB, err := k.lock(context.Background(), "b") // unrelated key: must not block
	if err != nil {
		t.Fatal(err)
	}
	unlockB()
	second := make(chan struct{})
	go func() {
		defer close(second)
		unlock, err := k.lock(context.Background(), "a")
		if err != nil {
			t.Error(err)
			return
		}
		unlock()
	}()
	select {
	case <-second:
		t.Fatal("same key was not serialized")
	case <-time.After(50 * time.Millisecond):
	}
	unlockA()
	select {
	case <-second:
	case <-time.After(5 * time.Second):
		t.Fatal("same key never handed over")
	}
	if len(k.locks) != 0 {
		t.Fatalf("locks map = %v, want released keys forgotten", k.locks)
	}
}

func TestKeyedMutexWaitHonoursCancellation(t *testing.T) {
	var k keyedMutex
	unlock, err := k.lock(context.Background(), "layer")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	waited := make(chan error, 1)
	go func() {
		_, err := k.lock(ctx, "layer")
		waited <- err
	}()
	cancel()
	select {
	case err := <-waited:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled wait = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled waiter never returned")
	}
	unlock()
	if len(k.locks) != 0 {
		t.Fatalf("locks map = %v, want the cancelled waiter forgotten", k.locks)
	}
}

func TestWarmCancellationFreesARefWaitingOnASharedBlob(t *testing.T) {
	// "slow" downloads the shared layer and is held there; "fast" shares it
	// and so waits for slow's download, holding no slot. Cancelling fast's
	// warm must end that wait at once rather than after slow's download.
	f := newParallelWarmFixture(t, []string{"slow", "fast"}, 1, func(f *parallelWarmFixture) { f.shareLayers = true })
	f.holdBlob = f.blobs[0]
	close(f.gate) // everything but the held layer is served at once

	slowDone := make(chan struct{})
	go func() {
		defer close(slowDone)
		_, _ = f.manager.Warm(context.Background(), f.refs[:1], imagecache.ArchitectureAMD64, WarmOptions{Jobs: 1})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && f.inFlight.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	if f.inFlight.Load() == 0 {
		t.Fatal("slow never started the shared layer")
	}

	ctx, cancel := context.WithCancel(context.Background())
	fastDone := make(chan WarmResult, 1)
	go func() {
		summary, _ := f.manager.Warm(ctx, f.refs[1:], imagecache.ArchitectureAMD64, WarmOptions{Jobs: 1})
		fastDone <- summary.Results[0]
	}()
	time.Sleep(100 * time.Millisecond) // let fast reach the shared layer's wait
	cancel()
	select {
	case result := <-fastDone:
		if result.Outcome != WarmOutcomeFailedMissing || !strings.Contains(result.Error, context.Canceled.Error()) {
			t.Fatalf("fast result = %+v, want the cancellation", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fast never returned after cancellation while slow held the shared layer")
	}
	close(f.hold)
	<-slowDone
}

func TestWarmPoolAcquireHonoursCancellation(t *testing.T) {
	m := newManagerWithPorts(t.TempDir(), nil, 0)
	pool := m.newWarmPool(1)
	release, err := pool.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	var second error
	go func() {
		defer wg.Done()
		_, second = pool.acquire(ctx)
	}()
	cancel()
	wg.Wait()
	if !errors.Is(second, context.Canceled) {
		t.Fatalf("second acquire = %v, want context.Canceled", second)
	}
	release()
	if release, err = pool.acquire(context.Background()); err != nil {
		t.Fatalf("slot not returned after cancelled wait: %v", err)
	}
	release()
	if len(pool.shared) != 0 || len(pool.request) != 0 {
		t.Fatalf("slots leaked: shared=%d request=%d", len(pool.shared), len(pool.request))
	}
}

func TestUpstreamTransportsBoundHeaderWaitAndKeepWarmWidthIdle(t *testing.T) {
	for name, transport := range map[string]*http.Transport{
		"safe":    newSafeTransport(defaultEgressDependencies()),
		"default": newUpstreamClient(nil).Transport.(*http.Transport),
	} {
		if transport.ResponseHeaderTimeout != upstreamStallTimeout {
			t.Errorf("%s ResponseHeaderTimeout = %s, want %s", name, transport.ResponseHeaderTimeout, upstreamStallTimeout)
		}
		if transport.MaxIdleConnsPerHost != MaxWarmJobs {
			t.Errorf("%s MaxIdleConnsPerHost = %d, want %d", name, transport.MaxIdleConnsPerHost, MaxWarmJobs)
		}
	}
	for name, client := range map[string]*http.Client{
		"safe":    newUpstreamClient(newSafeTransport(defaultEgressDependencies())),
		"default": newUpstreamClient(nil),
	} {
		if client.Timeout != 0 {
			t.Errorf("%s client Timeout = %s, want none: the bound is silence, not duration", name, client.Timeout)
		}
	}
}
