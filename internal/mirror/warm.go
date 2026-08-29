package mirror

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/randax/talos-box/internal/imagecache"
)

type WarmSummary struct {
	Results          []WarmResult
	Warmed           int
	AlreadyComplete  int
	Failed           int
	FailedMissing    int
	FailedRevalidate int
	// ReResolvedTags counts the tag-pinned refs whose tag was resolved
	// upstream again. That resolution is what a re-warm of an already
	// complete list spends its time on, so the summary can explain the
	// cost instead of looking like a download (#405).
	ReResolvedTags int
}

type WarmResult struct {
	Ref             string
	AlreadyComplete bool
	Outcome         WarmOutcome
	RefreshWarning  string
	// ReResolvedTag records that this ref named a tag and the tag was
	// re-resolved against the upstream registry.
	ReResolvedTag bool
	Error         string
}

type WarmOptions struct {
	Refresh bool
	// Jobs bounds the blob downloads this warm keeps in flight; zero means
	// DefaultWarmJobs and MaxWarmJobs is the ceiling (#506).
	Jobs int
}

type WarmOutcome string

const (
	WarmOutcomeWarmed           WarmOutcome = "warmed"
	WarmOutcomeAlreadyComplete  WarmOutcome = "already-complete"
	WarmOutcomeFailedMissing    WarmOutcome = "failed-missing"
	WarmOutcomeFailedRevalidate WarmOutcome = "failed-revalidate"
)

type warmReference struct {
	upstream     string
	repository   string
	listedRef    string
	pinnedDigest string
}

func (m *Manager) Warm(ctx context.Context, references []string, architecture imagecache.Architecture, options WarmOptions) (WarmSummary, error) {
	pool := m.newWarmPool(options.Jobs)
	results := make([]WarmResult, len(references))
	errs := make([]error, len(references))
	refSlots := make(chan struct{}, pool.jobs())
	var wg sync.WaitGroup
	for i, reference := range references {
		select {
		case refSlots <- struct{}{}:
		case <-ctx.Done():
			errs[i] = ctx.Err()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-refSlots }()
			results[i], errs[i] = m.warmOne(ctx, reference, architecture, options, pool)
		}()
	}
	wg.Wait()

	var summary WarmSummary
	for i, reference := range references {
		result, err := results[i], errs[i]
		if err != nil {
			result.Ref = reference
			result.Error = err.Error()
			if result.Outcome == "" {
				result.Outcome = WarmOutcomeFailedMissing
			}
			summary.Results = append(summary.Results, result)
			summary.Failed++
			if result.Outcome == WarmOutcomeFailedRevalidate {
				summary.FailedRevalidate++
			} else {
				summary.FailedMissing++
			}
			continue
		}
		summary.Results = append(summary.Results, result)
		if result.ReResolvedTag {
			summary.ReResolvedTags++
		}
		switch result.Outcome {
		case WarmOutcomeAlreadyComplete:
			summary.AlreadyComplete++
		default:
			summary.Warmed++
		}
	}
	return summary, nil
}

func (m *Manager) warmOne(ctx context.Context, reference string, architecture imagecache.Architecture, options WarmOptions, pool *warmPool) (WarmResult, error) {
	parsed, err := parseWarmReference(reference)
	if err != nil {
		return WarmResult{Outcome: WarmOutcomeFailedMissing}, err
	}
	if !isDigestReference(parsed.listedRef) {
		// the same tag resolved and published by two warms at once must not
		// interleave; other tags are unrelated and run alongside
		defer m.warmTagLocks.lock(parsed.upstream + "/" + parsed.repository + ":" + parsed.listedRef)()
	}
	authority, err := parseUpstreamAuthority(parsed.upstream)
	if err != nil {
		return WarmResult{Outcome: WarmOutcomeFailedMissing}, err
	}
	server := &Server{cacheDir: filepath.Join(m.cacheRoot, authority.cacheKey)}
	target := CacheTarget{Repository: parsed.repository, Digest: parsed.pinnedDigest, Platform: Platform{OS: "linux", Architecture: architecture}}
	if !isDigestReference(parsed.listedRef) {
		target.Tag = parsed.listedRef
	}
	before := server.InspectCached(ctx, target, InspectOptions{})
	refresh := options.Refresh && target.Tag != "" && parsed.pinnedDigest == ""
	if before.Complete() && !refresh {
		return WarmResult{Ref: reference, AlreadyComplete: true, Outcome: WarmOutcomeAlreadyComplete}, nil
	}

	handler := m.handlerForUpstream(authority)
	m.mu.Lock()
	server = m.dynamicServers[authority.cacheKey]
	m.mu.Unlock()
	if server == nil {
		return failedWarmResult(before), fmt.Errorf("warm server for %q unavailable", parsed.upstream)
	}

	result := WarmResult{Ref: reference, Outcome: WarmOutcomeWarmed}
	seenManifests := map[string]bool{}
	seenBlobs := map[string]bool{}
	staged := &stagedManifest{}

	requestReference := parsed.listedRef
	validateReference := requestReference
	listedContext := ctx
	if parsed.pinnedDigest != "" {
		requestReference = parsed.pinnedDigest
		validateReference = parsed.pinnedDigest
		// only the listed ref is pinned by the warm list; everything reached
		// from it is pinned by its parent manifest
		listedContext = withFilePin(ctx)
	}
	body, digest, _, err := warmManifestRequest(listedContext, handler, server, parsed.repository, requestReference, validateReference, staged)
	if err != nil {
		if before.Complete() && refresh && isTransientWarmError(err) {
			return WarmResult{Ref: reference, AlreadyComplete: true, Outcome: WarmOutcomeAlreadyComplete, RefreshWarning: refreshWarning(err)}, nil
		}
		return failedWarmResult(before), err
	}
	if refresh && staged.requestPath == "" {
		return WarmResult{Ref: reference, AlreadyComplete: true, Outcome: WarmOutcomeAlreadyComplete, RefreshWarning: "upstream unavailable, not revalidated"}, nil
	}
	if parsed.pinnedDigest != "" && digest != parsed.pinnedDigest {
		return failedWarmResult(before), fmt.Errorf("listed ref %q resolved to %s, want %s", reference, digest, parsed.pinnedDigest)
	}
	staged.metadata.DockerContentDigest = digest
	seenManifests[digest] = true

	if err := warmManifestGraph(ctx, handler, server, parsed.repository, body, string(architecture), seenManifests, map[string]bool{}, seenBlobs, &result, pool); err != nil {
		return failedWarmResult(before), err
	}
	if len(body) == 0 {
		return failedWarmResult(before), fmt.Errorf("listed ref %q produced no manifest", reference)
	}
	metadata := staged.metadata
	metadata.DockerContentDigest = digest
	if err := server.storeManifest(manifestRequestPath(parsed.repository, digest), metadata, body); err != nil {
		return failedWarmResult(before), fmt.Errorf("publish digest manifest: %w", err)
	}
	digestStatus := server.InspectCached(ctx, CacheTarget{Repository: parsed.repository, Digest: digest, Platform: target.Platform}, InspectOptions{})
	if !digestStatus.Complete() {
		return failedWarmResult(before), errors.New(digestStatus.Reason())
	}
	if target.Tag != "" {
		if err := server.storeManifest(manifestRequestPath(parsed.repository, target.Tag), metadata, body); err != nil {
			return failedWarmResult(before), fmt.Errorf("publish tag manifest: %w", err)
		}
		result.ReResolvedTag = true
	}
	finalTarget := target
	finalTarget.Digest = digest
	if status := server.InspectCached(ctx, finalTarget, InspectOptions{}); !status.Complete() {
		return failedWarmResult(before), errors.New(status.Reason())
	}
	if before.Complete() && before.RootDigest == digest {
		result.AlreadyComplete = true
		result.Outcome = WarmOutcomeAlreadyComplete
	}
	return result, nil
}

func failedWarmResult(before CacheStatus) WarmResult {
	if before.Complete() {
		return WarmResult{Outcome: WarmOutcomeFailedRevalidate}
	}
	return WarmResult{Outcome: WarmOutcomeFailedMissing}
}

func warmManifestGraph(ctx context.Context, handler http.Handler, server *Server, repository string, body []byte, hostArch string, seenManifests, warmedHostManifests, seenBlobs map[string]bool, result *WarmResult, pool *warmPool) error {
	kind, children, blobs, err := analyzeWarmManifest(body)
	if err != nil {
		return err
	}
	if kind == "manifest" {
		group := newWarmGroup(ctx)
		downloading := false
		for _, blob := range blobs {
			if seenBlobs[blob] {
				continue
			}
			seenBlobs[blob] = true
			if blobCached(server, blob) {
				continue
			}
			downloading = true
			group.run(func(ctx context.Context) error {
				release, err := pool.acquire(ctx)
				if err != nil {
					return err
				}
				defer release()
				return warmBlobRequest(ctx, handler, repository, blob)
			})
		}
		if err := group.wait(); err != nil {
			return err
		}
		if downloading {
			result.AlreadyComplete = false
		}
		return nil
	}

	selectedChild, ok := selectPlatformDescriptor(children, Platform{OS: "linux", Architecture: imagecache.Architecture(hostArch)})
	if !ok {
		return fmt.Errorf("no linux/%s manifest found in index", hostArch)
	}
	var (
		childBody     []byte
		cachedBefore  bool
		needChildBody = !seenManifests[selectedChild.Digest] || !warmedHostManifests[selectedChild.Digest]
	)
	if needChildBody {
		var err error
		childBody, _, cachedBefore, err = warmManifestRequest(ctx, handler, server, repository, selectedChild.Digest, selectedChild.Digest, nil)
		if err != nil {
			return err
		}
		seenManifests[selectedChild.Digest] = true
	}
	if needChildBody && !cachedBefore {
		result.AlreadyComplete = false
	}
	if !warmedHostManifests[selectedChild.Digest] {
		warmedHostManifests[selectedChild.Digest] = true
		if err := warmManifestGraph(ctx, handler, server, repository, childBody, hostArch, seenManifests, warmedHostManifests, seenBlobs, result, pool); err != nil {
			return err
		}
	}
	return nil
}

func analyzeWarmManifest(body []byte) (string, []platformDescriptor, []string, error) {
	manifest, err := decodeManifestGraph(body)
	if err != nil {
		return "", nil, nil, err
	}
	if isManifestIndex(manifest) {
		children := make([]platformDescriptor, 0, len(manifest.Manifests))
		for _, child := range manifest.Manifests {
			children = append(children, platformDescriptor{
				Digest:       child.Digest,
				Architecture: child.Platform.Architecture,
				OS:           child.Platform.OS,
			})
		}
		return "index", children, nil, nil
	}

	var blobs []string
	blobs = append(blobs, manifest.Config.Digest)
	for _, layer := range manifest.Layers {
		blobs = append(blobs, layer.Digest)
	}
	return "manifest", nil, blobs, nil
}

func warmManifestRequest(ctx context.Context, handler http.Handler, server *Server, repository, reference, validateReference string, staged *stagedManifest) ([]byte, string, bool, error) {
	path := "/v2/" + repository + "/manifests/" + reference
	cachedBefore := manifestCached(server, path)
	requestContext := withManifestRefresh(ctx)
	if staged != nil {
		requestContext = withStagedManifest(requestContext, staged)
	}
	requestContext = withManifestValidationReference(requestContext, validateReference)
	request := httptest.NewRequest(http.MethodGet, path, nil).WithContext(requestContext)
	request.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	}, ","))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code == http.StatusOK {
		return recorder.Body.Bytes(), recorder.Header().Get("Docker-Content-Digest"), cachedBefore, nil
	}
	message := strings.TrimSpace(recorder.Body.String())
	if recorder.Code == http.StatusConflict {
		// the mirror already named the image and both digests; the request
		// path and status would only bury that under transport detail
		return nil, "", cachedBefore, errors.New(message)
	}
	return nil, "", cachedBefore, &warmRequestError{
		path:    path,
		status:  recorder.Code,
		message: message,
		reason:  recorder.Header().Get(reasonHeader),
	}
}

type warmRequestError struct {
	path    string
	status  int
	message string
	reason  string
}

func (e *warmRequestError) Error() string {
	return fmt.Sprintf("%s returned %d: %s", e.path, e.status, e.message)
}

func isTransientWarmError(err error) bool {
	var requestErr *warmRequestError
	return errors.As(err, &requestErr) && requestErr.reason != reasonUpstreamManifestInvalid && (requestErr.status == http.StatusTooManyRequests || requestErr.status >= 500)
}

func refreshWarning(err error) string {
	var requestErr *warmRequestError
	if errors.As(err, &requestErr) {
		return fmt.Sprintf("upstream %d, not revalidated", requestErr.status)
	}
	return "upstream unavailable, not revalidated"
}

func warmBlobRequest(ctx context.Context, handler http.Handler, repository, digest string) error {
	path := "/v2/" + repository + "/blobs/" + digest
	request := httptest.NewRequest(http.MethodGet, path, nil).WithContext(withWarmBlob(ctx))
	recorder := newDiscardWarmResponse()
	handler.ServeHTTP(recorder, request)
	if recorder.status != http.StatusOK {
		return fmt.Errorf("%s returned %d: %s", path, recorder.status, strings.TrimSpace(recorder.errorBody.String()))
	}
	return nil
}

func manifestCached(server *Server, path string) bool {
	_, err := server.cachedManifestBytes(path)
	return err == nil
}

func blobCached(server *Server, digest string) bool {
	_, err := os.Stat(server.blobPath(digest))
	return err == nil
}

func parseWarmReference(reference string) (warmReference, error) {
	name, digest, hasDigest := strings.Cut(reference, "@")
	if hasDigest && digest == "" {
		return warmReference{}, fmt.Errorf("invalid pinned digest %q", digest)
	}
	lastSlash := strings.LastIndex(name, "/")
	lastColon := strings.LastIndex(name, ":")
	tag := ""
	if lastColon > lastSlash {
		tag = name[lastColon+1:]
	}
	nameWithoutTag := name
	if tag != "" {
		nameWithoutTag = name[:lastColon]
	}
	host, remainder, ok := strings.Cut(nameWithoutTag, "/")
	if !ok || host == "" || remainder == "" {
		return warmReference{}, fmt.Errorf("invalid image reference %q", reference)
	}
	if digest != "" {
		canonicalDigest, ok := canonicalSupportedDigest(digest)
		if !ok {
			return warmReference{}, fmt.Errorf("invalid or unsupported pinned digest %q", digest)
		}
		digest = canonicalDigest
	}

	listed := digest
	if tag != "" {
		listed = tag
	}
	if !hasDigest {
		digest = ""
		listed = tag
	}
	if listed == "" {
		listed = digest
	}
	return warmReference{
		upstream:     host,
		repository:   remainder,
		listedRef:    listed,
		pinnedDigest: digest,
	}, nil
}

type discardWarmResponse struct {
	headers   http.Header
	status    int
	errorBody strings.Builder
}

const maxWarmErrorBodyBytes = 64 << 10

func newDiscardWarmResponse() *discardWarmResponse {
	return &discardWarmResponse{headers: make(http.Header), status: http.StatusOK}
}

func (d *discardWarmResponse) Header() http.Header { return d.headers }

func (d *discardWarmResponse) WriteHeader(status int) {
	d.status = status
}

func (d *discardWarmResponse) Write(data []byte) (int, error) {
	originalLength := len(data)
	if d.status >= http.StatusBadRequest {
		remaining := maxWarmErrorBodyBytes - d.errorBody.Len()
		if remaining > 0 {
			if len(data) > remaining {
				data = data[:remaining]
			}
			_, _ = d.errorBody.Write(data)
		}
	}
	return originalLength, nil
}
