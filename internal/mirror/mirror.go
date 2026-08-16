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
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Server mirrors one upstream registry, caching immutable blobs on disk.
type Server struct {
	base             string // upstream base URL, e.g. https://registry-1.docker.io
	cacheDir         string
	client           *http.Client
	offline          *atomic.Bool
	validateUpstream func(context.Context) error
	now              func() time.Time // tests only: control token expiry

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
		client:   &http.Client{Timeout: 5 * time.Minute},
		tokens:   make(map[string]token),
	}
}

func newServerWithEgress(base, cacheDir string, egress egressDependencies) *Server {
	return &Server{
		base:     strings.TrimSuffix(base, "/"),
		cacheDir: cacheDir,
		client:   &http.Client{Timeout: 5 * time.Minute, Transport: newSafeTransport(egress)},
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
			MaxIdleConnsPerHost:   http.DefaultMaxIdleConnsPerHost,
			IdleConnTimeout:       90 * time.Second,
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
		http.Error(w, "mirror offline: content not cached", http.StatusServiceUnavailable)
		return
	}
	if s.validateUpstream != nil {
		if err := s.validateUpstream(r.Context()); err != nil {
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
		http.Error(w, "mirror offline: content not cached", http.StatusServiceUnavailable)
		return
	}

	resp, err := s.fetch(r)
	if err != nil {
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
			data, metadata, err := validateManifest(resp, manifestValidationReference(r.Context(), manifestReference(r.URL.Path)))
			if err != nil {
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

func (s *Server) serveCacheIfAvailable(w http.ResponseWriter, r *http.Request, digest string, isManifest bool) (bool, *cacheReplayError) {
	if digest != "" && s.serveCachedBlob(w, r, digest) {
		return true, nil
	}
	if isManifest {
		if shouldRefreshManifest(r.Context()) {
			return false, nil
		}
		reference := manifestReference(r.URL.Path)
		if isDigestReference(reference) {
			return s.serveCachedDigestManifest(w, r, reference)
		}
		if s.offlineEnabled() && s.serveCachedManifest(w, r) {
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
	if s.serveCachedManifest(w, r) {
		return true, nil
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

// fetch performs the upstream request, negotiating an anonymous bearer token
// on a 401 challenge and following redirects (the http.Client default).
func (s *Server) fetch(r *http.Request) (*http.Response, error) {
	url := s.base + r.URL.RequestURI()
	request, err := http.NewRequestWithContext(r.Context(), r.Method, url, nil)
	if err != nil {
		return nil, err
	}
	for _, header := range []string{"Accept", "Range"} {
		if v := r.Header.Get(header); v != "" {
			request.Header.Set(header, v)
		}
	}
	scope := scopeOf(r.URL.Path)
	if bearer := s.cachedToken(scope); bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := s.client.Do(request)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	challenge, ok := parseBearerChallenge(resp.Header.Get("WWW-Authenticate"))
	if !ok {
		// no token challenge to answer: the upstream's own response is the
		// most faithful thing to hand back
		return resp, nil
	}
	_ = resp.Body.Close()
	// a cached token that just drew a 401 is stale even if it has not expired
	s.forgetToken(scope)
	bearer, err := s.negotiateToken(r.Context(), challenge, scope)
	if err != nil {
		return nil, err
	}
	retry := request.Clone(request.Context())
	retry.Header.Set("Authorization", "Bearer "+bearer)
	return s.client.Do(retry)
}

func (s *Server) serveCachedBlob(w http.ResponseWriter, r *http.Request, digest string) bool {
	canonical, ok := canonicalSupportedDigest(digest)
	if ok {
		digest = canonical
	}
	path := s.blobPath(digest)
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return false
	}
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
}

func (s *Server) manifestMetadataPath(requestPath string) string {
	return s.manifestPath(requestPath) + ".meta"
}

func (s *Server) storeManifest(requestPath string, metadata manifestMetadata, data []byte) error {
	path := s.manifestPath(requestPath)
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

func validateManifest(resp *http.Response, requestedReference string) ([]byte, manifestMetadata, error) {
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
			return false, &cacheReplayError{
				status: http.StatusServiceUnavailable,
				err:    fmt.Errorf("mirror offline: cached digest corrupted"),
			}
		}
		return false, nil
	}
	canonical, err := verifySupportedDigest(data, requestedDigest)
	if err != nil {
		if s.offlineEnabled() {
			return false, &cacheReplayError{
				status: http.StatusServiceUnavailable,
				err:    fmt.Errorf("mirror offline: cached digest corrupted"),
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
	path := s.manifestPath(requestPath)
	data, err := os.ReadFile(path)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return data, path, err
	}

	legacyPath, ok := s.legacyManifestFallbackPath(requestPath)
	if !ok {
		return nil, path, err
	}
	data, err = os.ReadFile(legacyPath)
	if err != nil {
		return nil, legacyPath, err
	}
	if reference := manifestReference(requestPath); isDigestReference(reference) {
		if _, err := verifySupportedDigest(data, reference); err != nil {
			return nil, legacyPath, fmt.Errorf("%w: %v", errCachedManifestDigestMismatch, err)
		}
	}
	return data, legacyPath, nil
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

var challengeRe = regexp.MustCompile(`([A-Za-z0-9_-]+)="([^"]*)"`)

// bearerChallenge is a parsed RFC 6750 style WWW-Authenticate challenge as
// registries issue it: a token realm plus the service and scope to ask for.
type bearerChallenge struct {
	realm   string
	service string
	scope   string
}

// parseBearerChallenge reports whether header carries a Bearer challenge and,
// if so, its parameters. Nothing here is registry-specific: every value comes
// from the challenge itself.
func parseBearerChallenge(header string) (bearerChallenge, bool) {
	scheme, rest, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return bearerChallenge{}, false
	}
	params := map[string]string{}
	for _, match := range challengeRe.FindAllStringSubmatch(rest, -1) {
		params[strings.ToLower(match[1])] = match[2]
	}
	return bearerChallenge{
		realm:   params["realm"],
		service: params["service"],
		scope:   params["scope"],
	}, true
}

// defaultTokenLifetime applies when a token endpoint omits expires_in; a 401
// on a stale token re-negotiates anyway, so a short default is safe.
const defaultTokenLifetime = 60 * time.Second

// negotiateToken exchanges an anonymous token at the challenge's realm and
// caches it under requestScope. Docker Hub answers manifest requests with a
// challenge that carries no scope, so the scope derived from the request path
// is the fallback — without it the token comes back with no repository access
// and the retry 401s again.
func (s *Server) negotiateToken(ctx context.Context, challenge bearerChallenge, requestScope string) (string, error) {
	if challenge.realm == "" {
		return "", fmt.Errorf("auth challenge without realm")
	}
	scope := challenge.scope
	if scope == "" {
		scope = requestScope
	}
	realm, err := url.Parse(challenge.realm)
	if err != nil {
		return "", fmt.Errorf("auth challenge realm %q: %w", challenge.realm, err)
	}
	query := realm.Query()
	if challenge.service != "" {
		query.Set("service", challenge.service)
	}
	if scope != "" {
		query.Set("scope", scope)
	}
	realm.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := s.client.Do(request)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint %s returned %d", realm.Redacted(), resp.StatusCode)
	}
	var payload struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := decodeJSON(resp.Body, &payload); err != nil {
		return "", fmt.Errorf("token response: %w", err)
	}
	bearer := payload.Token
	if bearer == "" {
		bearer = payload.AccessToken
	}
	if bearer == "" {
		return "", fmt.Errorf("token endpoint returned no token")
	}

	cacheKey := requestScope
	if cacheKey == "" {
		cacheKey = scope
	}
	s.mu.Lock()
	if s.tokens == nil {
		s.tokens = make(map[string]token)
	}
	s.tokens[cacheKey] = token{value: bearer, expires: s.currentTime().Add(tokenLifetime(payload.ExpiresIn))}
	s.mu.Unlock()
	return bearer, nil
}

// tokenLifetime honors the endpoint's expires_in, minus a small skew so a
// token is never presented in its final moments.
func tokenLifetime(expiresIn int) time.Duration {
	lifetime := defaultTokenLifetime
	if expiresIn > 0 {
		lifetime = time.Duration(expiresIn) * time.Second
	}
	skew := lifetime / 10
	if skew > 30*time.Second {
		skew = 30 * time.Second
	}
	return lifetime - skew
}

func (s *Server) currentTime() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *Server) cachedToken(scope string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.tokens[scope]
	if !ok || !s.currentTime().Before(entry.expires) {
		return ""
	}
	return entry.value
}

func (s *Server) forgetToken(scope string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tokens, scope)
}

// scopeOf derives the pull scope a request needs, matching the "scope" a
// registry's token challenge would carry for that repository.
func scopeOf(requestPath string) string {
	if m := manifestPathRe.FindStringSubmatch(requestPath); m != nil {
		return "repository:" + m[1] + ":pull"
	}
	if m := regexp.MustCompile(`^/v2/(.+)/blobs/`).FindStringSubmatch(requestPath); m != nil {
		return "repository:" + m[1] + ":pull"
	}
	return ""
}

func decodeJSON(r io.Reader, destination any) error {
	data, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, destination)
}

type upstreamValidationError struct {
	status int
	err    error
}

func (e *upstreamValidationError) Error() string { return e.err.Error() }
func (e *upstreamValidationError) Unwrap() error { return e.err }

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
