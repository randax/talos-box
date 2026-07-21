// Package mirror serves pull-through registry mirrors to vmnet guests over
// plain HTTP. All upstream traffic — including CDN blob redirects and
// anonymous bearer tokens — happens server-side as host-process traffic,
// which corporate security agents attribute and allow (SPEC §5, gate G4).
package mirror

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Server mirrors one upstream registry, caching immutable blobs on disk.
type Server struct {
	base     string // upstream base URL, e.g. https://registry-1.docker.io
	cacheDir string
	client   *http.Client

	mu     sync.Mutex
	tokens map[string]token // key: auth challenge scope
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

var (
	blobPathRe     = regexp.MustCompile(`^/v2/.+/blobs/(sha256:[a-f0-9]{64})$`)
	manifestPathRe = regexp.MustCompile(`^/v2/(.+)/manifests/(.+)$`)
)

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
		if s.serveCachedBlob(w, r, digest) {
			return
		}
	}
	isManifest := manifestPathRe.MatchString(r.URL.Path)

	resp, err := s.fetch(r)
	if err != nil {
		// offline: a manifest we cached earlier still serves the pull
		if isManifest && s.serveCachedManifest(w, r) {
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
			copyResponseHeaders(w, resp)
			if !s.serveCachedBlob(w, r, digest) {
				http.Error(w, "serve verified blob: cached file unavailable", http.StatusInternalServerError)
			}
			return
		case isManifest:
			data, err := validateManifest(resp, manifestReference(r.URL.Path))
			if err != nil {
				http.Error(w, fmt.Sprintf("upstream manifest: %v", err), http.StatusBadGateway)
				return
			}
			if err := s.storeManifest(r.URL.Path, resp.Header.Get("Content-Type"), data); err != nil {
				http.Error(w, fmt.Sprintf("cache manifest: %v", err), http.StatusInternalServerError)
				return
			}
			copyResponseHeaders(w, resp)
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(data)
			return
		}
	}

	copyResponseHeaders(w, resp)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func copyResponseHeaders(w http.ResponseWriter, resp *http.Response) {
	for _, header := range []string{"Content-Type", "Content-Length", "Docker-Content-Digest", "Etag"} {
		if value := resp.Header.Get(header); value != "" {
			w.Header().Set(header, value)
		}
	}
}

// fetch performs the upstream request, negotiating an anonymous bearer token
// on a 401 challenge and following redirects (the http.Client default).
func (s *Server) fetch(r *http.Request) (*http.Response, error) {
	url := s.base + r.URL.RequestURI()
	request, err := http.NewRequest(r.Method, url, nil)
	if err != nil {
		return nil, err
	}
	for _, header := range []string{"Accept", "Range"} {
		if v := r.Header.Get(header); v != "" {
			request.Header.Set(header, v)
		}
	}
	if bearer := s.cachedToken(scopeOf(r.URL.Path)); bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := s.client.Do(request)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	_ = resp.Body.Close()
	bearer, err := s.negotiateToken(challenge)
	if err != nil {
		return nil, err
	}
	retry := request.Clone(request.Context())
	retry.Header.Set("Authorization", "Bearer "+bearer)
	return s.client.Do(retry)
}

func (s *Server) serveCachedBlob(w http.ResponseWriter, r *http.Request, digest string) bool {
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

	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, hasher), body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("download blob: %w", err)
	}
	got := hex.EncodeToString(hasher.Sum(nil))
	want := strings.TrimPrefix(digest, "sha256:")
	if got != want {
		_ = tmp.Close()
		return fmt.Errorf("digest mismatch: requested sha256:%s, downloaded sha256:%s", want, got)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close staged blob: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("publish verified blob: %w", err)
	}
	return nil
}

// manifestPath maps a manifest request path to its on-disk cache location.
func (s *Server) manifestPath(requestPath string) string {
	safe := strings.ReplaceAll(strings.TrimPrefix(requestPath, "/v2/"), "/", "_")
	return filepath.Join(s.cacheDir, "manifests", safe)
}

func (s *Server) storeManifest(requestPath, contentType string, data []byte) error {
	path := s.manifestPath(requestPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create manifest cache directory: %w", err)
	}
	if err := os.WriteFile(path+".ct", []byte(contentType), 0o644); err != nil {
		return fmt.Errorf("write manifest content type: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}

var manifestMediaTypes = map[string]struct{}{
	"application/json": {},
	"application/vnd.docker.distribution.manifest.list.v2+json": {},
	"application/vnd.docker.distribution.manifest.v2+json":      {},
	"application/vnd.oci.image.index.v1+json":                   {},
	"application/vnd.oci.image.manifest.v1+json":                {},
}

func validateManifest(resp *http.Response, requestedReference string) ([]byte, error) {
	manifestURL := responseURL(resp, "upstream URL")
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read manifest from %s: %w", manifestURL, err)
	}

	contentType := resp.Header.Get("Content-Type")
	mediaType, _, mediaTypeErr := mime.ParseMediaType(contentType)
	if strings.EqualFold(mediaType, "text/html") || bytes.HasPrefix(bytes.TrimSpace(data), []byte("<")) {
		return nil, fmt.Errorf("manifest response from %s looks like a web-filter/proxy block page", manifestURL)
	}
	if mediaTypeErr != nil {
		return nil, fmt.Errorf("manifest response from %s has invalid Content-Type %q: %w", manifestURL, contentType, mediaTypeErr)
	}
	if _, ok := manifestMediaTypes[strings.ToLower(mediaType)]; !ok {
		return nil, fmt.Errorf("manifest response from %s has unsupported Content-Type %q", manifestURL, contentType)
	}
	if !json.Valid(data) {
		return nil, fmt.Errorf("manifest response from %s is not valid JSON", manifestURL)
	}

	actualDigest := "sha256:" + manifestSHA256(data)
	if headerDigest := strings.TrimSpace(resp.Header.Get("Docker-Content-Digest")); headerDigest != "" && headerDigest != actualDigest {
		return nil, fmt.Errorf("manifest response from %s has Docker-Content-Digest %s, content is %s", manifestURL, headerDigest, actualDigest)
	}
	if strings.HasPrefix(requestedReference, "sha256:") && requestedReference != actualDigest {
		return nil, fmt.Errorf("manifest response from %s does not match requested digest %s (content is %s)", manifestURL, requestedReference, actualDigest)
	}

	return data, nil
}

func manifestReference(requestPath string) string {
	if match := manifestPathRe.FindStringSubmatch(requestPath); match != nil {
		return match[2]
	}
	return ""
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

func (s *Server) serveCachedManifest(w http.ResponseWriter, r *http.Request) bool {
	path := s.manifestPath(r.URL.Path)
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	if ct, err := os.ReadFile(path + ".ct"); err == nil && len(ct) > 0 {
		w.Header().Set("Content-Type", string(ct))
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
	return true
}

func (s *Server) blobPath(digest string) string {
	return filepath.Join(s.cacheDir, "blobs", strings.ReplaceAll(digest, ":", "-"))
}

var challengeRe = regexp.MustCompile(`(\w+)="([^"]*)"`)

func (s *Server) negotiateToken(challenge string) (string, error) {
	if !strings.HasPrefix(challenge, "Bearer ") {
		return "", fmt.Errorf("unsupported auth challenge %q", challenge)
	}
	params := map[string]string{}
	for _, m := range challengeRe.FindAllStringSubmatch(challenge, -1) {
		params[m[1]] = m[2]
	}
	realm := params["realm"]
	if realm == "" {
		return "", fmt.Errorf("auth challenge without realm: %q", challenge)
	}
	url := fmt.Sprintf("%s?service=%s&scope=%s", realm, params["service"], params["scope"])
	resp, err := s.client.Get(url)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
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
	lifetime := time.Duration(payload.ExpiresIn) * time.Second
	if lifetime < time.Minute {
		lifetime = 4 * time.Minute
	}
	s.mu.Lock()
	s.tokens[params["scope"]] = token{value: bearer, expires: time.Now().Add(lifetime - 30*time.Second)}
	s.mu.Unlock()
	return bearer, nil
}

func (s *Server) cachedToken(scope string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.tokens[scope]
	if !ok || time.Now().After(entry.expires) {
		return ""
	}
	return entry.value
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
