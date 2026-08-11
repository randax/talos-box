package mirror

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/randax/talos-box/internal/imagecache"
)

const maxCachedManifestSidecarBytes = 1 << 20

type CheckSummary struct {
	Results  []CheckResult
	Complete int
	Failed   int
}

type CheckResult struct {
	Ref   string
	Error string
}

func (m *Manager) Check(ctx context.Context, references []string, architecture imagecache.Architecture, deep bool) (CheckSummary, error) {
	var summary CheckSummary
	for _, reference := range references {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		err := m.checkOne(ctx, reference, string(architecture), deep)
		item := CheckResult{Ref: reference}
		if err != nil {
			item.Error = err.Error()
			summary.Failed++
		} else {
			summary.Complete++
		}
		summary.Results = append(summary.Results, item)
	}
	return summary, nil
}

func (m *Manager) checkOne(ctx context.Context, reference, hostArch string, deep bool) error {
	parsed, err := parseWarmReference(reference)
	if err != nil {
		return err
	}
	authority, err := parseUpstreamAuthority(parsed.upstream)
	if err != nil {
		return err
	}
	server := &Server{cacheDir: filepath.Join(m.cacheRoot, authority.cacheKey)}

	listedPath := manifestRequestPath(parsed.repository, parsed.listedRef)
	body, resolvedDigest, err := checkCachedManifest(ctx, server, listedPath, parsed.pinnedDigest)
	if err != nil {
		return err
	}

	seenManifests := map[string]bool{}
	seenBlobs := map[string]bool{}
	if resolvedDigest != "" {
		seenManifests[resolvedDigest] = true
	}
	if parsed.listedRef != resolvedDigest {
		digestPath := manifestRequestPath(parsed.repository, resolvedDigest)
		if _, _, err := checkCachedManifest(ctx, server, digestPath, resolvedDigest); err != nil {
			return fmt.Errorf("resolved manifest %s: %w", resolvedDigest, err)
		}
	}
	return checkManifestGraph(ctx, server, parsed.repository, body, hostArch, deep, seenManifests, map[string]bool{}, seenBlobs)
}

func checkManifestGraph(ctx context.Context, server *Server, repository string, body []byte, hostArch string, deep bool, seenManifests, checkedHostManifests, seenBlobs map[string]bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
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
			if err := checkCachedBlob(ctx, server, blob, deep); err != nil {
				return err
			}
		}
		return nil
	}

	matchedHost := false
	for _, child := range children {
		if _, _, err := checkedSupportedDigest(child.digest); err != nil {
			return fmt.Errorf("child manifest %s invalid: %w", child.digest, err)
		}
		hostMatch := child.architecture == hostArch && (child.os == "" || child.os == "linux")
		if hostMatch {
			matchedHost = true
		}
		childPath := manifestRequestPath(repository, child.digest)
		var childBody []byte
		if !seenManifests[child.digest] || (hostMatch && !checkedHostManifests[child.digest]) {
			var err error
			childBody, _, err = checkCachedManifest(ctx, server, childPath, child.digest)
			if err != nil {
				return fmt.Errorf("child manifest %s: %w", child.digest, err)
			}
			seenManifests[child.digest] = true
		}
		if hostMatch && !checkedHostManifests[child.digest] {
			checkedHostManifests[child.digest] = true
			if err := checkManifestGraph(ctx, server, repository, childBody, hostArch, deep, seenManifests, checkedHostManifests, seenBlobs); err != nil {
				return err
			}
		}
	}
	if !matchedHost {
		return fmt.Errorf("no linux/%s manifest found in index", hostArch)
	}
	return nil
}

func checkCachedManifest(ctx context.Context, server *Server, requestPath, expectedDigest string) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	data, err := checkedCachedManifestBytes(server, requestPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, "", fmt.Errorf("%s not cached", requestPath)
		}
		return nil, "", fmt.Errorf("%s invalid: %w", requestPath, err)
	}
	resolvedDigest := checkedCachedManifestMetadata(server, requestPath, data).DockerContentDigest
	if expectedDigest != "" {
		canonical, err := verifySupportedDigest(data, expectedDigest)
		if err != nil {
			return nil, "", fmt.Errorf("%s corrupted: %w", requestPath, err)
		}
		resolvedDigest = canonical
	} else if reference := manifestReference(requestPath); isDigestReference(reference) {
		canonical, err := verifySupportedDigest(data, reference)
		if err != nil {
			return nil, "", fmt.Errorf("%s corrupted: %w", requestPath, err)
		}
		resolvedDigest = canonical
	}
	if _, _, _, err := analyzeWarmManifest(data); err != nil {
		return nil, "", fmt.Errorf("%s invalid: %w", requestPath, err)
	}
	return data, resolvedDigest, nil
}

func checkCachedBlob(ctx context.Context, server *Server, digest string, deep bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, _, err := checkedSupportedDigest(digest); err != nil {
		return fmt.Errorf("blob %s invalid: %w", digest, err)
	}
	path := server.blobPath(digest)
	file, _, err := openCheckedRegularFile(path)
	if err != nil {
		return fmt.Errorf("blob %s not cached", digest)
	}
	defer func() { _ = file.Close() }()
	if !deep {
		return nil
	}
	if err := verifyBlobFile(file, digest); err != nil {
		return fmt.Errorf("blob %s corrupted: %w", digest, err)
	}
	return nil
}

func verifyBlobFile(file *os.File, digest string) error {
	algorithm, encoded, err := checkedSupportedDigest(digest)
	if err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}

	switch algorithm {
	case "sha256":
		hasher := sha256.New()
		if _, err := io.Copy(hasher, file); err != nil {
			return err
		}
		actual := hex.EncodeToString(hasher.Sum(nil))
		if actual != strings.ToLower(encoded) {
			return fmt.Errorf("content is sha256:%s", actual)
		}
	case "sha512":
		hasher := sha512.New()
		if _, err := io.Copy(hasher, file); err != nil {
			return err
		}
		actual := hex.EncodeToString(hasher.Sum(nil))
		if actual != strings.ToLower(encoded) {
			return fmt.Errorf("content is sha512:%s", actual)
		}
	default:
		return fmt.Errorf("unsupported digest algorithm %q", algorithm)
	}
	return nil
}

func checkedSupportedDigest(reference string) (string, string, error) {
	algorithm, encoded, ok := splitDigestReference(reference)
	if !ok {
		return "", "", fmt.Errorf("invalid digest reference")
	}
	switch algorithm {
	case "sha256":
		if len(encoded) != 64 {
			return "", "", fmt.Errorf("invalid digest reference")
		}
	case "sha512":
		if len(encoded) != 128 {
			return "", "", fmt.Errorf("invalid digest reference")
		}
	default:
		return "", "", fmt.Errorf("unsupported digest algorithm %q", algorithm)
	}
	return algorithm, encoded, nil
}

func checkedCachedManifestBytes(server *Server, requestPath string) ([]byte, error) {
	return readCheckedRegularFile(server.manifestPath(requestPath), maxManifestBytes)
}

func checkedCachedManifestMetadata(server *Server, requestPath string, data []byte) manifestMetadata {
	metadata := manifestMetadata{
		ContentType:         "application/vnd.oci.image.manifest.v1+json",
		ContentLength:       int64(len(data)),
		DockerContentDigest: cachedManifestDigest(data, manifestReference(requestPath), ""),
	}
	if rawMetadata, err := readCheckedRegularFile(server.manifestMetadataPath(requestPath), maxCachedManifestSidecarBytes); err == nil {
		var stored manifestMetadata
		if json.Unmarshal(rawMetadata, &stored) == nil {
			if stored.ContentType != "" {
				metadata.ContentType = stored.ContentType
			}
			metadata.DockerContentDigest = cachedManifestDigest(data, manifestReference(requestPath), stored.DockerContentDigest)
			return metadata
		}
	}
	if ct, err := readCheckedRegularFile(server.manifestPath(requestPath)+".ct", maxCachedManifestSidecarBytes); err == nil && len(ct) > 0 {
		metadata.ContentType = string(ct)
	}
	return metadata
}

func openCheckedRegularFile(path string) (*os.File, os.FileInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	info, err := openRegularFileInfo(file)
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	if err := validateOpenedRegularFile(path, info); err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	return file, info, nil
}

func openRegularFileInfo(file *os.File) (os.FileInfo, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("cached file is not a regular file")
	}
	return info, nil
}

func validateOpenedRegularFile(path string, info os.FileInfo) error {
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() {
		return fmt.Errorf("cached file is not a regular file")
	}
	if !os.SameFile(info, pathInfo) {
		return fmt.Errorf("cached file changed during open")
	}
	return nil
}

func readCheckedRegularFile(path string, maxBytes int64) ([]byte, error) {
	file, info, err := openCheckedRegularFile(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("cached manifest exceeds %d bytes", maxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("cached manifest exceeds %d bytes", maxBytes)
	}
	return data, nil
}

func manifestRequestPath(repository, reference string) string {
	return "/v2/" + repository + "/manifests/" + reference
}
