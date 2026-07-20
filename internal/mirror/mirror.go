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
	"hash"
	"io"
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

	for _, header := range []string{"Content-Type", "Content-Length", "Docker-Content-Digest", "Etag"} {
		if v := resp.Header.Get(header); v != "" {
			w.Header().Set(header, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	if r.Method != http.MethodGet || resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(w, resp.Body)
		return
	}
	switch {
	case digest != "":
		_, _ = io.Copy(w, s.teeBlob(resp.Body, digest))
	case isManifest:
		s.copyAndStoreManifest(w, r, resp)
	default:
		_, _ = io.Copy(w, resp.Body)
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

// teeBlob streams the blob to the client while hashing and writing it to a
// temp file, renamed into place only if the content hashes to the requested
// digest (partial downloads and corrupt bodies never poison the store).
func (s *Server) teeBlob(body io.Reader, digest string) io.Reader {
	path := s.blobPath(digest)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return body
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".partial-*")
	if err != nil {
		return body
	}
	want := strings.TrimPrefix(digest, "sha256:")
	return &cachingReader{source: body, tmp: tmp, final: path, hasher: sha256.New(), want: want}
}

type cachingReader struct {
	source io.Reader
	tmp    *os.File
	final  string
	hasher hash.Hash
	want   string
	failed bool
}

func (c *cachingReader) Read(p []byte) (int, error) {
	n, err := c.source.Read(p)
	if n > 0 && !c.failed {
		_, _ = c.hasher.Write(p[:n])
		if _, werr := c.tmp.Write(p[:n]); werr != nil {
			c.failed = true
		}
	}
	if err == io.EOF && !c.failed {
		_ = c.tmp.Close()
		if hex.EncodeToString(c.hasher.Sum(nil)) == c.want {
			_ = os.Rename(c.tmp.Name(), c.final)
		} else {
			_ = os.Remove(c.tmp.Name()) // content did not match its digest
		}
	} else if err != nil {
		_ = c.tmp.Close()
		_ = os.Remove(c.tmp.Name())
	}
	return n, err
}

// manifestPath maps a manifest request path to its on-disk cache location.
func (s *Server) manifestPath(requestPath string) string {
	safe := strings.ReplaceAll(strings.TrimPrefix(requestPath, "/v2/"), "/", "_")
	return filepath.Join(s.cacheDir, "manifests", safe)
}

// copyAndStoreManifest streams the manifest to the client and saves a copy
// (with its content type) so it can be served offline later.
func (s *Server) copyAndStoreManifest(w http.ResponseWriter, r *http.Request, resp *http.Response) {
	var buf bytes.Buffer
	_, _ = io.Copy(w, io.TeeReader(resp.Body, &buf))
	path := s.manifestPath(r.URL.Path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(path+".ct", []byte(resp.Header.Get("Content-Type")), 0o644)
	_ = os.WriteFile(path, buf.Bytes(), 0o644)
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
