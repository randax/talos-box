package mirror

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"

	"github.com/randax/talos-box/internal/imagecache"
)

type WarmSummary struct {
	Results         []WarmResult
	Warmed          int
	AlreadyComplete int
	Failed          int
}

type WarmResult struct {
	Ref             string
	AlreadyComplete bool
	Error           string
}

type warmReference struct {
	upstream     string
	repository   string
	listedRef    string
	pinnedDigest string
}

func (m *Manager) Warm(ctx context.Context, references []string, architecture imagecache.Architecture) (WarmSummary, error) {
	var summary WarmSummary
	for _, reference := range references {
		result, err := m.warmOne(ctx, reference, string(architecture))
		if err != nil {
			summary.Results = append(summary.Results, WarmResult{Ref: reference, Error: err.Error()})
			summary.Failed++
			continue
		}
		summary.Results = append(summary.Results, result)
		if result.AlreadyComplete {
			summary.AlreadyComplete++
		} else {
			summary.Warmed++
		}
	}
	return summary, nil
}

func (m *Manager) warmOne(ctx context.Context, reference, hostArch string) (WarmResult, error) {
	parsed, err := parseWarmReference(reference)
	if err != nil {
		return WarmResult{}, err
	}
	if !isDigestReference(parsed.listedRef) {
		m.warmTagMu.Lock()
		defer m.warmTagMu.Unlock()
	}
	authority, err := parseUpstreamAuthority(parsed.upstream)
	if err != nil {
		return WarmResult{}, err
	}
	handler := m.handlerForUpstream(authority)
	m.mu.Lock()
	server := m.dynamicServers[authority.cacheKey]
	m.mu.Unlock()
	if server == nil {
		return WarmResult{}, fmt.Errorf("warm server for %q unavailable", parsed.upstream)
	}

	result := WarmResult{Ref: reference, AlreadyComplete: true}
	seenManifests := map[string]bool{}
	seenBlobs := map[string]bool{}
	var staged *stagedManifest
	stageListed := !isDigestReference(parsed.listedRef)
	if stageListed {
		staged = &stagedManifest{}
	}

	validateReference := parsed.listedRef
	if parsed.pinnedDigest != "" {
		validateReference = parsed.pinnedDigest
	}
	body, digest, cachedBefore, err := warmManifestRequest(ctx, handler, server, parsed.repository, parsed.listedRef, validateReference, staged)
	if err != nil {
		return WarmResult{}, err
	}
	if !cachedBefore {
		result.AlreadyComplete = false
	}
	if parsed.pinnedDigest != "" && digest != parsed.pinnedDigest {
		return WarmResult{}, fmt.Errorf("listed ref %q resolved to %s, want %s", reference, digest, parsed.pinnedDigest)
	}
	if stageListed && staged != nil {
		staged.metadata.DockerContentDigest = digest
	}
	seenManifests[digest] = true
	if parsed.listedRef != digest {
		_, _, digestCachedBefore, err := warmManifestRequest(ctx, handler, server, parsed.repository, digest, digest, nil)
		if err != nil {
			return WarmResult{}, err
		}
		if !digestCachedBefore {
			result.AlreadyComplete = false
		}
	}

	if err := warmManifestGraph(ctx, handler, server, parsed.repository, body, hostArch, seenManifests, seenBlobs, &result); err != nil {
		return WarmResult{}, err
	}
	if stageListed && staged != nil {
		if staged.requestPath == "" || len(staged.data) == 0 {
			return WarmResult{}, fmt.Errorf("listed ref %q did not produce a staged manifest", reference)
		}
		if err := server.storeManifest(staged.requestPath, staged.metadata, staged.data); err != nil {
			return WarmResult{}, fmt.Errorf("publish listed manifest: %w", err)
		}
	}
	return result, nil
}

func warmManifestGraph(ctx context.Context, handler http.Handler, server *Server, repository string, body []byte, hostArch string, seenManifests, seenBlobs map[string]bool, result *WarmResult) error {
	kind, children, blobs, err := analyzeWarmManifest(body)
	if err != nil {
		return err
	}
		if kind == "manifest" {
			for _, blob := range blobs {
				if seenBlobs[blob] {
					continue
				}
				seenBlobs[blob] = true
				if blobCached(server, blob) {
					continue
				}
				if err := warmBlobRequest(ctx, handler, repository, blob); err != nil {
					return err
				}
				result.AlreadyComplete = false
			}
			return nil
		}

	matchedHost := false
	for _, child := range children {
		if seenManifests[child.digest] {
			if child.architecture == hostArch && (child.os == "" || child.os == "linux") {
				matchedHost = true
			}
			continue
		}
		childBody, _, cachedBefore, err := warmManifestRequest(ctx, handler, server, repository, child.digest, child.digest, nil)
		if err != nil {
			return err
		}
		if !cachedBefore {
			result.AlreadyComplete = false
		}
		seenManifests[child.digest] = true
		if child.architecture == hostArch && (child.os == "" || child.os == "linux") {
			matchedHost = true
			if err := warmManifestGraph(ctx, handler, server, repository, childBody, hostArch, seenManifests, seenBlobs, result); err != nil {
				return err
			}
		}
	}
	if !matchedHost {
		return fmt.Errorf("no linux/%s manifest found in index", hostArch)
	}
	return nil
}

type warmChildManifest struct {
	digest       string
	architecture string
	os           string
}

func analyzeWarmManifest(body []byte) (string, []warmChildManifest, []string, error) {
	var manifest struct {
		MediaType string `json:"mediaType"`
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform struct {
				Architecture string `json:"architecture"`
				OS           string `json:"os"`
			} `json:"platform"`
		} `json:"manifests"`
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		return "", nil, nil, fmt.Errorf("decode manifest graph: %w", err)
	}
	if len(manifest.Manifests) > 0 || strings.Contains(manifest.MediaType, "index") || strings.Contains(manifest.MediaType, "manifest.list") {
		children := make([]warmChildManifest, 0, len(manifest.Manifests))
		for _, child := range manifest.Manifests {
			children = append(children, warmChildManifest{
				digest:       child.Digest,
				architecture: child.Platform.Architecture,
				os:           child.Platform.OS,
			})
		}
		return "index", children, nil, nil
	}

	var blobs []string
	if manifest.Config.Digest != "" {
		blobs = append(blobs, manifest.Config.Digest)
	}
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
	if recorder.Code != http.StatusOK {
		return nil, "", cachedBefore, fmt.Errorf("%s returned %d: %s", path, recorder.Code, strings.TrimSpace(recorder.Body.String()))
	}
	return recorder.Body.Bytes(), recorder.Header().Get("Docker-Content-Digest"), cachedBefore, nil
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
