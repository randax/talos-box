// Package mirror serves pull-through registry mirrors to vmnet guests over
// plain HTTP. All upstream traffic — including CDN blob redirects and
// anonymous bearer tokens — happens server-side as host-process traffic,
// which corporate security agents attribute and allow (SPEC §5, gate G4).
package mirror

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/randax/talos-box/internal/imagecache"
)

// Server mirrors one upstream registry, caching immutable blobs on disk.
type Server struct {
	base             string // upstream base URL, e.g. https://registry-1.docker.io
	namespace        string // canonical ns containerd asked for, e.g. docker.io
	cacheDir         string
	client           *http.Client
	offline          *atomic.Bool
	validateUpstream func(context.Context) error
	now              func() time.Time                           // tests only: control token expiry
	retrySleep       func(context.Context, time.Duration) error // tests only: avoid real retry delays

	mu     sync.Mutex
	tokens map[string]token // key: pull scope of the request
}

type token struct {
	value   string
	expires time.Time
}

// NewServer mirrors the upstream at base (scheme included), caching blobs
// under cacheDir.
func NewServer(base, cacheDir string) *Server {
	return &Server{
		base:     strings.TrimSuffix(base, "/"),
		cacheDir: cacheDir,
		client:   newUpstreamClient(nil),
		tokens:   make(map[string]token),
	}
}

// upstreamStallTimeout is how long an upstream may go silent — before its
// headers, or between two reads of its body — before the request is given up.
// It replaces a whole-request timeout: with several layers sharing the link at
// once a large layer legitimately takes many times longer than it did alone,
// and only a stalled one is a failure (#506).
const upstreamStallTimeout = 5 * time.Minute

// newUpstreamClient bounds an upstream request by silence, not by total
// duration: the transport times out the headers and fetch wraps each body so a
// stall cancels the request. A nil transport takes the default one with the
// same header bound.
func newUpstreamClient(transport http.RoundTripper) *http.Client {
	if transport == nil {
		base := http.DefaultTransport.(*http.Transport).Clone()
		base.ResponseHeaderTimeout = upstreamStallTimeout
		transport = base
	}
	return &http.Client{Transport: transport}
}

func newServerWithEgress(base, cacheDir string, egress egressDependencies) *Server {
	return &Server{
		base:     strings.TrimSuffix(base, "/"),
		cacheDir: cacheDir,
		client:   newUpstreamClient(newSafeTransport(egress)),
		tokens:   make(map[string]token),
	}
}

type egressDependencies struct {
	resolve     func(context.Context, string) ([]net.IP, error)
	hostIPs     func() ([]net.IP, error)
	dialContext func(context.Context, string, string) (net.Conn, error)
	blocked     func(net.IP, []net.IP) bool
}

func defaultEgressDependencies() egressDependencies {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return egressDependencies{
		resolve: func(ctx context.Context, host string) ([]net.IP, error) {
			if ip := net.ParseIP(host); ip != nil {
				return []net.IP{ip}, nil
			}
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		},
		hostIPs: hostOwnedIPs,
		dialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, address)
		},
		blocked: namespaceIPBlocked,
	}
}

func newSafeTransport(egress egressDependencies) *http.Transport {
	var transport *http.Transport
	if base, ok := http.DefaultTransport.(*http.Transport); ok && base != nil {
		transport = base.Clone()
	} else {
		transport = &http.Transport{
			Proxy:                 nil,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   MaxWarmJobs, // a warm keeps this many fetches on one host
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: upstreamStallTimeout,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
	}
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return dialValidatedIP(ctx, network, address, egress)
	}
	return transport
}

func dialValidatedIP(ctx context.Context, network, address string, egress egressDependencies) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, err := egress.resolve(ctx, canonicalLookupHost(host))
	if err != nil {
		return nil, err
	}
	hostIPs, err := egress.hostIPs()
	if err != nil {
		return nil, err
	}
	var allowed []net.IP
	blocked := egress.blocked
	if blocked == nil {
		blocked = namespaceIPBlocked
	}
	for _, ip := range ips {
		if blocked(ip, hostIPs) {
			continue
		}
		allowed = append(allowed, ip)
	}
	if len(allowed) == 0 {
		return nil, fmt.Errorf("refuse upstream host %q: no public address", host)
	}
	var dialErr error
	for _, ip := range allowed {
		conn, err := egress.dialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		dialErr = err
	}
	return nil, dialErr
}

func canonicalLookupHost(host string) string {
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

var (
	blobPathRe     = regexp.MustCompile(`^/v2/.+/blobs/((?:sha256:[A-Fa-f0-9]{64})|(?:sha512:[A-Fa-f0-9]{128}))$`)
	manifestPathRe = regexp.MustCompile(`^/v2/(.+)/manifests/(.+)$`)
	digestRefRe    = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9]*(?:[+._-][A-Za-z][A-Za-z0-9]*)*):([A-Fa-f0-9]+)$`)
)

type manifestRefreshKey struct{}
type manifestValidationReferenceKey struct{}
type stagedManifestKey struct{}
type warmBlobKey struct{}
type filePinKey struct{}

func withManifestRefresh(ctx context.Context) context.Context {
	return context.WithValue(ctx, manifestRefreshKey{}, true)
}

func shouldRefreshManifest(ctx context.Context) bool {
	value, _ := ctx.Value(manifestRefreshKey{}).(bool)
	return value
}

func withManifestValidationReference(ctx context.Context, reference string) context.Context {
	return context.WithValue(ctx, manifestValidationReferenceKey{}, reference)
}

func manifestValidationReference(ctx context.Context, fallback string) string {
	if value, ok := ctx.Value(manifestValidationReferenceKey{}).(string); ok && value != "" {
		return value
	}
	return fallback
}

// withFilePin marks a request whose validation digest comes from a warm list
// file, so a mismatch names the file as the thing to edit rather than the
// request (child manifests are pinned by their parent index, not by a file).
func withFilePin(ctx context.Context) context.Context {
	return context.WithValue(ctx, filePinKey{}, true)
}

func pinSource(ctx context.Context) string {
	if pinned, _ := ctx.Value(filePinKey{}).(bool); pinned {
		return "file"
	}
	return "request"
}

type stagedManifest struct {
	requestPath string
	data        []byte
	metadata    manifestMetadata
}

func withStagedManifest(ctx context.Context, staged *stagedManifest) context.Context {
	return context.WithValue(ctx, stagedManifestKey{}, staged)
}

func stagedManifestFromContext(ctx context.Context) *stagedManifest {
	staged, _ := ctx.Value(stagedManifestKey{}).(*stagedManifest)
	return staged
}

func withWarmBlob(ctx context.Context) context.Context {
	return context.WithValue(ctx, warmBlobKey{}, true)
}

func shouldSkipWarmBlobReplay(ctx context.Context) bool {
	value, _ := ctx.Value(warmBlobKey{}).(bool)
	return value
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "mirror is pull-only", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == "/v2/" {
		// the version ping carries no data; answering locally keeps it
		// independent of upstreams that deny scopeless anonymous tokens
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		w.WriteHeader(http.StatusOK)
		return
	}

	var digest string
	if m := blobPathRe.FindStringSubmatch(r.URL.Path); m != nil {
		digest = m[1]
	}
	isManifest := manifestPathRe.MatchString(r.URL.Path)
	if served, err := s.serveCacheIfAvailable(w, r, digest, isManifest); served {
		return
	} else if err != nil {
		http.Error(w, err.Error(), err.status)
		return
	}
	if s.offlineEnabled() {
		writeOfflineMiss(w, offlineNotCachedMessage)
		return
	}
	stale := s.staleCandidate(r)
	if s.validateUpstream != nil {
		if err := s.validateUpstream(r.Context()); err != nil {
			if stale.Complete() && shouldServeStaleOnValidationError(err) && s.serveStaleManifest(w, r, stale, err.Error()) {
				return
			}
			var validationErr *upstreamValidationError
			if errors.As(err, &validationErr) {
				http.Error(w, validationErr.Error(), validationErr.status)
				return
			}
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
	}
	if served, err := s.serveCacheIfAvailable(w, r, digest, isManifest); served {
		return
	} else if err != nil {
		http.Error(w, err.Error(), err.status)
		return
	}
	if s.offlineEnabled() {
		writeOfflineMiss(w, offlineNotCachedMessage)
		return
	}
	policy := s.fillRequestPolicy()
	if stale.Complete() {
		policy = immediateRequestPolicy()
	}
	resp, err := s.fetch(r, policy)
	if err != nil {
		if stale.Complete() && s.serveStaleManifest(w, r, stale, err.Error()) {
			return
		}
		if served, cacheErr := s.serveManifestCacheOnFetchFailure(w, r); served {
			return
		} else if cacheErr != nil {
			http.Error(w, cacheErr.Error(), cacheErr.status)
			return
		}
		http.Error(w, fmt.Sprintf("upstream: %v", err), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if stale.Complete() && isTransientUpstreamStatus(resp.StatusCode) {
		closeRetryResponse(resp)
		if s.serveStaleManifest(w, r, stale, fmt.Sprintf("status %d", resp.StatusCode)) {
			return
		}
		http.Error(w, fmt.Sprintf("upstream status %d and stale cache unavailable", resp.StatusCode), http.StatusBadGateway)
		return
	}

	if r.Method == http.MethodGet && resp.StatusCode == http.StatusOK {
		switch {
		case digest != "":
			if err := s.cacheBlob(resp.Body, digest); err != nil {
				http.Error(w, fmt.Sprintf("upstream blob %s: %v", responseURL(resp, s.base+r.URL.RequestURI()), err), http.StatusBadGateway)
				return
			}
			if shouldSkipWarmBlobReplay(r.Context()) {
				w.Header().Set("Docker-Content-Digest", digest)
				w.WriteHeader(http.StatusOK)
				return
			}
			copyResponseHeaders(w, resp)
			if !s.serveCachedBlob(w, r, digest) {
				http.Error(w, "serve verified blob: cached file unavailable", http.StatusInternalServerError)
			}
			return
		case isManifest:
			pathReference := manifestReference(r.URL.Path)
			data, metadata, err := validateManifest(resp, manifestValidationReference(r.Context(), pathReference), pathReference)
			if err != nil {
				var mismatch *pinnedDigestMismatchError
				if errors.As(err, &mismatch) {
					// the upstream answered correctly; it is the caller's pin
					// that is stale, so this is a client-side conflict (#365)
					http.Error(w, mismatch.message(s.imageReference(r.URL.Path), pinSource(r.Context())), http.StatusConflict)
					return
				}
				setReasonHeaders(w, reasonUpstreamManifestInvalid, err.Error())
				http.Error(w, fmt.Sprintf("upstream manifest: %v", err), http.StatusBadGateway)
				return
			}
			if staged := stagedManifestFromContext(r.Context()); staged != nil {
				staged.requestPath = r.URL.Path
				staged.data = append([]byte(nil), data...)
				staged.metadata = metadata
			} else {
				// the validated manifest is already in memory; a cache write
				// failure only costs offline replay, not this pull
				if err := s.storeManifest(r.URL.Path, metadata, data); err != nil {
					log.Printf("mirror: cache manifest %s: %v", r.URL.Path, err)
				}
			}
			copyResponseHeaders(w, resp)
			applyManifestMetadataHeaders(w, metadata)
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(data)
			return
		}
	}

	copyResponseHeaders(w, resp)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (s *Server) staleCandidate(r *http.Request) CacheStatus {
	if s.offlineEnabled() || shouldRefreshManifest(r.Context()) {
		return CacheStatus{Gaps: []CacheGap{{Detail: "not a live online request"}}}
	}
	return s.cachedTagStatus(r)
}

func (s *Server) cachedTagStatus(r *http.Request) CacheStatus {
	match := manifestPathRe.FindStringSubmatch(r.URL.Path)
	if match == nil || isDigestReference(match[2]) {
		return CacheStatus{Gaps: []CacheGap{{Detail: "not a mutable tag request"}}}
	}
	return s.InspectCached(r.Context(), CacheTarget{
		Repository: match[1],
		Tag:        match[2],
		Platform:   Platform{OS: "linux", Architecture: imagecache.Architecture(runtime.GOARCH)},
	}, InspectOptions{})
}

func isTransientUpstreamStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError && status <= 599
}

func (s *Server) serveStaleManifest(w http.ResponseWriter, r *http.Request, status CacheStatus, upstream string) bool {
	data, path, err := s.cachedManifest(r.URL.Path)
	if err != nil || !bytes.Equal(data, status.ManifestData) {
		return false
	}
	metadata := s.cachedManifestMetadataAtPath(r.URL.Path, path, data)
	setReasonHeaders(w, reasonServedStale, staleManifestMessage)
	log.Printf("mirror served stale: %s/%s (upstream %s; cache complete for %s)",
		s.upstreamNamespace(), offlineMissReference(r.URL.Path), upstream, platformName(status.Target.Platform))
	return serveManifestBytes(w, r, status.ManifestData, metadata)
}

// serveCacheIfAvailable replays cached content, stamping an unserved offline
// request with the reason for its fallback-friendly 404. Every offline miss
// ends here or in the Manager's cache-only probe, so stamping on the way out
// covers both without either writer knowing about the other (#363).
func (s *Server) serveCacheIfAvailable(w http.ResponseWriter, r *http.Request, digest string, isManifest bool) (bool, *cacheReplayError) {
	served, err := s.replayCache(w, r, digest, isManifest)
	if !served && s.offlineEnabled() {
		logReason := offlineNotCachedMessage
		if isManifest && !isDigestReference(manifestReference(r.URL.Path)) {
			status := s.cachedTagStatus(r)
			logReason = status.Reason()
			if err == nil && cacheStatusCorrupted(status) {
				setReasonHeaders(w, reasonOfflineCacheCorrupted, offlineCacheCorruptedMessage)
				err = &cacheReplayError{
					status: http.StatusServiceUnavailable,
					err:    errors.New(offlineCacheCorruptedMessage),
				}
			}
		} else if err != nil {
			logReason = err.Error()
		}
		if err == nil {
			setReasonHeaders(w, reasonOfflineNotCached, offlineNotCachedMessage)
		}
		// containerd only surfaces the bare status to the kubelet event, so
		// without this line an offline miss left no trace anywhere in tbx and
		// the operator saw an unexplained ImagePullBackOff (#403).
		log.Printf("mirror offline miss: %s (upstream namespace %s): %s",
			offlineMissReference(r.URL.Path), s.upstreamNamespace(), logReason)
	}
	return served, err
}

// upstreamNamespace names the registry the way containerd asked for it, which
// is not always the host the base URL points at: docker.io is served from
// registry-1.docker.io, and that alias keys a different cache directory. The
// miss line is meant to be recomposed into "<namespace>/<ref>" and handed back
// to tbx cache warm/list (#403), so it has to carry the namespace.
func (s *Server) upstreamNamespace() string {
	if s.namespace != "" {
		return s.namespace
	}
	return upstreamHost(s.base)
}

// offlineMissReference names what was asked for the way an operator writes it,
// without the upstream host the miss line reports separately.
func offlineMissReference(requestPath string) string {
	if match := manifestPathRe.FindStringSubmatch(requestPath); match != nil {
		separator := ":"
		if isDigestReference(match[2]) {
			separator = "@"
		}
		return match[1] + separator + match[2]
	}
	return strings.TrimPrefix(requestPath, "/v2/")
}

func (s *Server) replayCache(w http.ResponseWriter, r *http.Request, digest string, isManifest bool) (bool, *cacheReplayError) {
	if digest != "" && s.serveCachedBlob(w, r, digest) {
		return true, nil
	}
	if isManifest {
		// A refresh request deliberately bypasses the cache to re-resolve the
		// reference upstream — but offline there is no upstream to re-resolve
		// against, so bypassing would report a miss on content the cache holds and the
		// checker calls complete.
		if shouldRefreshManifest(r.Context()) && !s.offlineEnabled() {
			return false, nil
		}
		reference := manifestReference(r.URL.Path)
		if isDigestReference(reference) {
			return s.serveCachedDigestManifest(w, r, reference)
		}
		if s.offlineEnabled() && s.cachedTagStatus(r).Complete() && s.serveCachedManifest(w, r) {
			return true, nil
		}
	}
	return false, nil
}

func (s *Server) serveManifestCacheOnFetchFailure(w http.ResponseWriter, r *http.Request) (bool, *cacheReplayError) {
	if !manifestPathRe.MatchString(r.URL.Path) {
		return false, nil
	}
	reference := manifestReference(r.URL.Path)
	if isDigestReference(reference) {
		return s.serveCachedDigestManifest(w, r, reference)
	}
	return false, nil
}

func (s *Server) CloseIdleConnections() {
	type idleCloser interface {
		CloseIdleConnections()
	}
	if transport, ok := s.client.Transport.(idleCloser); ok {
		transport.CloseIdleConnections()
	}
}

func (s *Server) setOfflineMode(enabled bool) {
	if s.offline == nil {
		s.offline = &atomic.Bool{}
	}
	s.offline.Store(enabled)
}

func (s *Server) offlineEnabled() bool {
	return s.offline != nil && s.offline.Load()
}

func copyResponseHeaders(w http.ResponseWriter, resp *http.Response) {
	for _, header := range []string{"Content-Type", "Content-Length", "Docker-Content-Digest", "Etag"} {
		if value := resp.Header.Get(header); value != "" {
			w.Header().Set(header, value)
		}
	}
}

func applyManifestMetadataHeaders(w http.ResponseWriter, metadata manifestMetadata) {
	if metadata.ContentType != "" {
		w.Header().Set("Content-Type", metadata.ContentType)
	}
	if metadata.ContentLength > 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", metadata.ContentLength))
	}
	if metadata.DockerContentDigest != "" {
		w.Header().Set("Docker-Content-Digest", metadata.DockerContentDigest)
	}
}

func (s *Server) serveCachedBlob(w http.ResponseWriter, r *http.Request, digest string) bool {
	canonical, ok := canonicalSupportedDigest(digest)
	if ok {
		digest = canonical
	}
	path := s.blobPath(digest)
	file, info, err := openCheckedRegularFile(path)
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size()))
	w.Header().Set("Docker-Content-Digest", digest)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = io.Copy(w, file)
	}
	return true
}

// cacheBlob stages the complete blob and publishes it only after its content
// hashes to the requested digest. Callers serve the published file afterward.
func (s *Server) cacheBlob(body io.Reader, digest string) error {
	canonical, ok := canonicalSupportedDigest(digest)
	if ok {
		digest = canonical
	}
	path := s.blobPath(digest)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create blob cache directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".partial-*")
	if err != nil {
		return fmt.Errorf("create staged blob: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	algorithm, encoded, ok := splitDigestReference(digest)
	if !ok {
		_ = tmp.Close()
		return fmt.Errorf("invalid digest reference")
	}
	var hasher hashWriter
	switch algorithm {
	case "sha256":
		hasher = sha256.New()
	case "sha512":
		hasher = sha512.New()
	default:
		_ = tmp.Close()
		return fmt.Errorf("unsupported digest algorithm %q", algorithm)
	}
	if _, err := io.Copy(io.MultiWriter(tmp, hasher), body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("download blob: %w", err)
	}
	got := hex.EncodeToString(hasher.Sum(nil))
	if got != encoded {
		_ = tmp.Close()
		return fmt.Errorf("digest mismatch: requested %s:%s, downloaded %s:%s", algorithm, encoded, algorithm, got)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close staged blob: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("publish verified blob: %w", err)
	}
	return nil
}

type hashWriter interface {
	Write([]byte) (int, error)
	Sum([]byte) []byte
}

// canonicalManifestRequestPath normalizes supported digest references before
// they become cache keys, so differently cased requests share one entry.
func canonicalManifestRequestPath(requestPath string) string {
	if match := manifestPathRe.FindStringSubmatch(requestPath); match != nil {
		if canonical, ok := canonicalSupportedDigest(match[2]); ok {
			return "/v2/" + match[1] + "/manifests/" + canonical
		}
	}
	return requestPath
}

// manifestPath maps a canonical manifest request path to a versioned,
// collision-resistant on-disk cache key. The previous slash-to-underscore
// format could map distinct repositories (for example a/b and a_b) to the
// same file.
func (s *Server) manifestPath(requestPath string) string {
	key := sha256.Sum256([]byte(canonicalManifestRequestPath(requestPath)))
	return filepath.Join(s.cacheDir, "manifests", "v2-"+hex.EncodeToString(key[:]))
}

func (s *Server) legacyManifestPath(requestPath string) string {
	safe := strings.ReplaceAll(strings.TrimPrefix(canonicalManifestRequestPath(requestPath), "/v2/"), "/", "_")
	return filepath.Join(s.cacheDir, "manifests", safe)
}

func (s *Server) legacyManifestFallbackPath(requestPath string) (string, bool) {
	match := manifestPathRe.FindStringSubmatch(canonicalManifestRequestPath(requestPath))
	if match == nil || !isDigestReference(match[2]) {
		return "", false
	}
	// A legacy tag key flattened both the repository and tag with underscores,
	// so no tag key can prove which repository created it. Digest entries are
	// safe to replay only after their bytes verify against the request.
	return s.legacyManifestPath(requestPath), true
}

type manifestMetadata struct {
	ContentType         string `json:"contentType"`
	ContentLength       int64  `json:"contentLength"`
	DockerContentDigest string `json:"dockerContentDigest"`
	// Repository and Reference record what the opaque `v2-<hash>` cache key
	// stands for, so the on-disk mirror cache can be answered with grep
	// instead of a hash reconstruction (#406). They are descriptive only:
	// nothing is served from them.
	Repository string `json:"repository,omitempty"`
	Reference  string `json:"reference,omitempty"`
}

func (s *Server) manifestMetadataPath(requestPath string) string {
	return s.manifestPath(requestPath) + ".meta"
}

func (s *Server) storeManifest(requestPath string, metadata manifestMetadata, data []byte) error {
	path := s.manifestPath(requestPath)
	if match := manifestPathRe.FindStringSubmatch(canonicalManifestRequestPath(requestPath)); match != nil {
		metadata.Repository, metadata.Reference = match[1], match[2]
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create manifest cache directory: %w", err)
	}
	rawMetadata, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("encode manifest metadata: %w", err)
	}
	if err := writeFileAtomic(s.manifestMetadataPath(requestPath), rawMetadata); err != nil {
		return fmt.Errorf("write manifest metadata: %w", err)
	}
	if err := writeFileAtomic(path, data); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}

// writeFileAtomic stages data in a temp file and renames it into place, so
// readers never observe a truncated file.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".partial-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

var manifestMediaTypes = map[string]struct{}{
	"application/json": {},
	"application/vnd.docker.distribution.manifest.list.v2+json": {},
	"application/vnd.docker.distribution.manifest.v2+json":      {},
	"application/vnd.oci.image.index.v1+json":                   {},
	"application/vnd.oci.image.manifest.v1+json":                {},
}

// maxManifestBytes bounds manifest reads; real OCI/Docker manifests and
// indexes are well under 1 MiB, so 10 MiB leaves generous headroom.
const maxManifestBytes = 10 << 20

// validateManifest checks an upstream manifest response against
// requestedReference — the digest or tag the bytes must answer to, which for a
// warm is the pin its parent index or warm list carries rather than the
// reference in the path. pathReference is the reference the request itself
// asked for, and it is what separates the two mismatch stories: a tag request
// validated against a pin can have a stale pin, while a digest-addressed
// request can only be looking at corruption, tampering, or a rewriting proxy.
func validateManifest(resp *http.Response, requestedReference, pathReference string) ([]byte, manifestMetadata, error) {
	manifestURL := responseURL(resp, "upstream URL")
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		return nil, manifestMetadata{}, fmt.Errorf("read manifest from %s: %w", manifestURL, err)
	}
	if len(data) > maxManifestBytes {
		return nil, manifestMetadata{}, fmt.Errorf("manifest response from %s exceeds %d bytes", manifestURL, maxManifestBytes)
	}

	contentType := resp.Header.Get("Content-Type")
	mediaType, _, mediaTypeErr := mime.ParseMediaType(contentType)
	if strings.EqualFold(mediaType, "text/html") || bytes.HasPrefix(bytes.TrimSpace(data), []byte("<")) {
		return nil, manifestMetadata{}, fmt.Errorf("manifest response from %s looks like a web-filter/proxy block page", manifestURL)
	}
	if mediaTypeErr != nil {
		return nil, manifestMetadata{}, fmt.Errorf("manifest response from %s has invalid Content-Type %q: %w", manifestURL, contentType, mediaTypeErr)
	}
	if _, ok := manifestMediaTypes[strings.ToLower(mediaType)]; !ok {
		return nil, manifestMetadata{}, fmt.Errorf("manifest response from %s has unsupported Content-Type %q", manifestURL, contentType)
	}
	if !json.Valid(data) {
		return nil, manifestMetadata{}, fmt.Errorf("manifest response from %s is not valid JSON", manifestURL)
	}
	if err := validateManifestBytes(data); err != nil {
		return nil, manifestMetadata{}, err
	}

	persistedDigest := ""
	if headerDigest := strings.TrimSpace(resp.Header.Get("Docker-Content-Digest")); headerDigest != "" {
		canonicalDigest, err := verifySupportedDigest(data, headerDigest)
		if err != nil {
			return nil, manifestMetadata{}, fmt.Errorf("manifest response from %s has Docker-Content-Digest %s: %w", manifestURL, headerDigest, err)
		}
		persistedDigest = canonicalDigest
	}
	if isDigestReference(requestedReference) {
		canonicalDigest, err := verifySupportedDigest(data, requestedReference)
		if err != nil {
			// Only a request that did *not* name a digest can have a stale
			// pin. When the caller asked for sha256:A and got bytes hashing to
			// B, there is no pin to update — the content is wrong — so that
			// stays an upstream-integrity failure (#367).
			if served := digestOfAs(requestedReference, data); served != "" && !isDigestReference(pathReference) {
				return nil, manifestMetadata{}, &pinnedDigestMismatchError{pinned: requestedReference, served: served}
			}
			return nil, manifestMetadata{}, fmt.Errorf("manifest response from %s does not match requested digest %s: %w", manifestURL, requestedReference, err)
		}
		if persistedDigest != "" && persistedDigest != canonicalDigest {
			return nil, manifestMetadata{}, fmt.Errorf("manifest response from %s has Docker-Content-Digest %s, requested digest is %s", manifestURL, persistedDigest, canonicalDigest)
		}
		persistedDigest = canonicalDigest
	}
	if persistedDigest == "" {
		persistedDigest = "sha256:" + manifestSHA256(data)
	}

	return data, manifestMetadata{
		ContentType:         contentType,
		ContentLength:       int64(len(data)),
		DockerContentDigest: persistedDigest,
	}, nil
}

func manifestReference(requestPath string) string {
	if match := manifestPathRe.FindStringSubmatch(requestPath); match != nil {
		return match[2]
	}
	return ""
}

func isDigestReference(reference string) bool {
	_, _, ok := splitDigestReference(reference)
	return ok
}

func responseURL(resp *http.Response, fallback string) string {
	if resp.Request != nil && resp.Request.URL != nil {
		return resp.Request.URL.String()
	}
	return fallback
}

func manifestSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func manifestSHA512(data []byte) string {
	sum := sha512.Sum512(data)
	return hex.EncodeToString(sum[:])
}

func (s *Server) serveCachedManifest(w http.ResponseWriter, r *http.Request) bool {
	data, path, err := s.cachedManifest(r.URL.Path)
	if err != nil {
		return false
	}
	metadata := s.cachedManifestMetadataAtPath(r.URL.Path, path, data)
	return serveManifestBytes(w, r, data, metadata)
}

func (s *Server) serveCachedDigestManifest(w http.ResponseWriter, r *http.Request, requestedDigest string) (bool, *cacheReplayError) {
	data, path, err := s.cachedManifest(r.URL.Path)
	if err != nil {
		if s.offlineEnabled() && errors.Is(err, errCachedManifestDigestMismatch) {
			setReasonHeaders(w, reasonOfflineCacheCorrupted, offlineCacheCorruptedMessage)
			return false, &cacheReplayError{
				status: http.StatusServiceUnavailable,
				err:    errors.New(offlineCacheCorruptedMessage),
			}
		}
		return false, nil
	}
	canonical, err := verifySupportedDigest(data, requestedDigest)
	if err != nil {
		if s.offlineEnabled() {
			setReasonHeaders(w, reasonOfflineCacheCorrupted, offlineCacheCorruptedMessage)
			return false, &cacheReplayError{
				status: http.StatusServiceUnavailable,
				err:    errors.New(offlineCacheCorruptedMessage),
			}
		}
		return false, nil
	}
	metadata := s.cachedManifestMetadataAtPath(r.URL.Path, path, data)
	metadata.DockerContentDigest = canonical
	return serveManifestBytes(w, r, data, metadata), nil
}

func serveManifestBytes(w http.ResponseWriter, r *http.Request, data []byte, metadata manifestMetadata) bool {
	w.Header().Set("Content-Type", metadata.ContentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", metadata.ContentLength))
	w.Header().Set("Docker-Content-Digest", metadata.DockerContentDigest)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
	return true
}

func (s *Server) cachedManifestBytes(requestPath string) ([]byte, error) {
	data, _, err := s.cachedManifest(requestPath)
	return data, err
}

var errCachedManifestDigestMismatch = errors.New("cached manifest does not match requested digest")

func (s *Server) cachedManifest(requestPath string) ([]byte, string, error) {
	return checkedCachedManifest(s, requestPath)
}

func (s *Server) cachedManifestMetadata(requestPath string, data []byte) manifestMetadata {
	return s.cachedManifestMetadataAtPath(requestPath, s.manifestPath(requestPath), data)
}

func (s *Server) cachedManifestMetadataAtPath(requestPath, path string, data []byte) manifestMetadata {
	metadata := manifestMetadata{
		ContentType:         s.cachedManifestContentTypeAtPath(path, data),
		ContentLength:       int64(len(data)),
		DockerContentDigest: cachedManifestDigest(data, manifestReference(requestPath), ""),
	}
	if rawMetadata, err := readCheckedRegularFile(path+".meta", maxCachedManifestSidecarBytes); err == nil {
		var stored manifestMetadata
		if json.Unmarshal(rawMetadata, &stored) == nil {
			_, digestErr := verifySupportedDigest(data, stored.DockerContentDigest)
			if digestErr == nil && stored.ContentType != "" {
				metadata.ContentType = stored.ContentType
			} else if digestErr != nil {
				if legacyContentType := s.cachedLegacyManifestContentTypeAtPath(path); legacyContentType != "" {
					metadata.ContentType = legacyContentType
				}
			}
			metadata.DockerContentDigest = cachedManifestDigest(data, manifestReference(requestPath), stored.DockerContentDigest)
		}
	}
	return metadata
}

func (s *Server) cachedManifestContentTypeAtPath(path string, data []byte) string {
	if contentType := manifestContentType(data); contentType != "" {
		return contentType
	}
	if contentType := s.cachedLegacyManifestContentTypeAtPath(path); contentType != "" {
		return contentType
	}
	return "application/vnd.oci.image.manifest.v1+json"
}

func (s *Server) cachedLegacyManifestContentTypeAtPath(path string) string {
	if ct, err := readCheckedRegularFile(path+".ct", maxCachedManifestSidecarBytes); err == nil && len(ct) > 0 {
		return string(ct)
	}
	return ""
}

func manifestContentType(data []byte) string {
	var manifest struct {
		MediaType string `json:"mediaType"`
	}
	if json.Unmarshal(data, &manifest) != nil {
		return ""
	}
	mediaType := strings.TrimSpace(manifest.MediaType)
	if _, ok := manifestMediaTypes[strings.ToLower(mediaType)]; !ok {
		return ""
	}
	return mediaType
}

func (s *Server) blobPath(digest string) string {
	if canonical, ok := canonicalSupportedDigest(digest); ok {
		digest = canonical
	}
	return filepath.Join(s.cacheDir, "blobs", strings.ReplaceAll(digest, ":", "-"))
}

func decodeJSON(r io.Reader, destination any) error {
	data, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, destination)
}

// Reason headers ride along with the error surfaces a client may never see a
// body for: containerd probes with HEAD first, and a HEAD response carries no
// body, so a bare status reads as a broken mirror. Headers survive HEAD, so the
// honest reason travels there — X-Talosbox-Reason for machines, Warning for
// the surfaces (containerd, kubectl) that already render it (#363).
const reasonHeader = "X-Talosbox-Reason"

const (
	reasonOfflineNotCached        = "offline-not-cached"
	reasonOfflineCacheCorrupted   = "offline-cache-corrupted"
	reasonServedStale             = "served-stale"
	reasonUpstreamManifestInvalid = "upstream-manifest-invalid"
)

const (
	offlineMissStatus            = http.StatusNotFound
	offlineNotCachedMessage      = "mirror offline: content not cached"
	offlineCacheCorruptedMessage = "mirror offline: cached digest corrupted"
	staleManifestMessage         = "mirror served stale"
)

func writeOfflineMiss(w http.ResponseWriter, detail string) {
	setReasonHeaders(w, reasonOfflineNotCached, offlineNotCachedMessage)
	http.Error(w, detail, offlineMissStatus)
}

// setReasonHeaders stamps the reason on a response whose body may be dropped.
// The Warning value follows RFC 7234's "199 <agent> <quoted-text>" shape.
func setReasonHeaders(w http.ResponseWriter, reason, text string) {
	w.Header().Set(reasonHeader, reason)
	w.Header().Set("Warning", fmt.Sprintf("199 talos-box %q", text))
}

func shouldServeStaleOnValidationError(err error) bool {
	var resolutionErr *upstreamResolutionError
	return errors.As(err, &resolutionErr)
}

func cacheStatusCorrupted(status CacheStatus) bool {
	for _, gap := range status.Gaps {
		switch gap.Kind {
		case CacheGapCorrupt:
			return true
		case CacheGapTagMapping, CacheGapRootManifest, CacheGapPlatformManifest:
			if gap.Detail != "" {
				return true
			}
		}
	}
	return false
}

// pinnedDigestMismatchError reports a manifest the upstream served correctly
// but whose bytes the caller's pinned digest no longer names. The pin is what
// is stale, so callers surface this as a client-side conflict rather than an
// upstream failure (#365).
type pinnedDigestMismatchError struct {
	pinned string
	served string
}

func (e *pinnedDigestMismatchError) Error() string {
	return fmt.Sprintf("pinned digest mismatch: pinned %s, upstream serves %s", e.pinned, e.served)
}

// message names the image and where the stale pin lives, so the reader knows
// what to edit.
func (e *pinnedDigestMismatchError) message(image, source string) string {
	return fmt.Sprintf("pinned digest mismatch for %s: %s pins %s, upstream serves %s", image, source, e.pinned, e.served)
}

// imageReference renders a request path the way an operator writes it — the
// form a warm list would carry — so the pin is easy to find and fix.
func (s *Server) imageReference(requestPath string) string {
	match := manifestPathRe.FindStringSubmatch(requestPath)
	if match == nil {
		return upstreamHost(s.base) + requestPath
	}
	separator := ":"
	if isDigestReference(match[2]) {
		separator = "@"
	}
	return upstreamHost(s.base) + "/" + match[1] + separator + match[2]
}

func upstreamHost(base string) string {
	if parsed, err := url.Parse(base); err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return base
}

type upstreamValidationError struct {
	status int
	err    error
}

func (e *upstreamValidationError) Error() string { return e.err.Error() }
func (e *upstreamValidationError) Unwrap() error { return e.err }

type upstreamResolutionError struct{ err error }

func (e *upstreamResolutionError) Error() string { return e.err.Error() }
func (e *upstreamResolutionError) Unwrap() error { return e.err }

type cacheReplayError struct {
	status int
	err    error
}

func (e *cacheReplayError) Error() string { return e.err.Error() }
func (e *cacheReplayError) Unwrap() error { return e.err }

func splitDigestReference(reference string) (string, string, bool) {
	match := digestRefRe.FindStringSubmatch(reference)
	if match == nil {
		return "", "", false
	}
	return strings.ToLower(match[1]), strings.ToLower(match[2]), true
}

func canonicalSupportedDigest(reference string) (string, bool) {
	algorithm, encoded, ok := splitDigestReference(reference)
	if !ok {
		return "", false
	}
	switch algorithm {
	case "sha256":
		if len(encoded) != 64 {
			return "", false
		}
	case "sha512":
		if len(encoded) != 128 {
			return "", false
		}
	default:
		return "", false
	}
	return algorithm + ":" + encoded, true
}

// digestOfAs returns data's digest under reference's algorithm, or "" when
// that algorithm is not one the mirror verifies.
func digestOfAs(reference string, data []byte) string {
	algorithm, _, ok := splitDigestReference(reference)
	if !ok {
		return ""
	}
	switch algorithm {
	case "sha256":
		return "sha256:" + manifestSHA256(data)
	case "sha512":
		return "sha512:" + manifestSHA512(data)
	}
	return ""
}

func verifySupportedDigest(data []byte, reference string) (string, error) {
	algorithm, encoded, ok := splitDigestReference(reference)
	if !ok {
		return "", fmt.Errorf("invalid digest reference")
	}
	switch algorithm {
	case "sha256":
		actual := manifestSHA256(data)
		if encoded != actual {
			return "", fmt.Errorf("content is sha256:%s", actual)
		}
	case "sha512":
		actual := manifestSHA512(data)
		if encoded != actual {
			return "", fmt.Errorf("content is sha512:%s", actual)
		}
	default:
		return "", fmt.Errorf("unsupported digest algorithm %q", algorithm)
	}
	return algorithm + ":" + encoded, nil
}

func cachedManifestDigest(data []byte, requestedReference, storedDigest string) string {
	if isDigestReference(requestedReference) {
		if canonical, err := verifySupportedDigest(data, requestedReference); err == nil {
			return canonical
		}
	}
	if storedDigest != "" {
		if canonical, err := verifySupportedDigest(data, storedDigest); err == nil {
			return canonical
		}
	}
	return "sha256:" + manifestSHA256(data)
}
