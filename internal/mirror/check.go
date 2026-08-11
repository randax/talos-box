package mirror

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/randax/talos-box/internal/imagecache"
)

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
	return checkManifestGraph(ctx, server, parsed.repository, body, hostArch, deep, seenManifests, seenBlobs)
}

func checkManifestGraph(ctx context.Context, server *Server, repository string, body []byte, hostArch string, deep bool, seenManifests, seenBlobs map[string]bool) error {
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
		if child.architecture == hostArch && (child.os == "" || child.os == "linux") {
			matchedHost = true
		}
		if seenManifests[child.digest] {
			continue
		}
		childPath := manifestRequestPath(repository, child.digest)
		childBody, _, err := checkCachedManifest(ctx, server, childPath, child.digest)
		if err != nil {
			return fmt.Errorf("child manifest %s: %w", child.digest, err)
		}
		seenManifests[child.digest] = true
		if child.architecture == hostArch && (child.os == "" || child.os == "linux") {
			if err := checkManifestGraph(ctx, server, repository, childBody, hostArch, deep, seenManifests, seenBlobs); err != nil {
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
	data, err := server.cachedManifestBytes(requestPath)
	if err != nil {
		return nil, "", fmt.Errorf("%s not cached", requestPath)
	}
	resolvedDigest := server.cachedManifestMetadata(requestPath, data).DockerContentDigest
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
	path := server.blobPath(digest)
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("blob %s not cached", digest)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("blob %s incomplete", digest)
	}
	if !deep {
		return nil
	}
	if err := verifyBlobFile(path, digest); err != nil {
		return fmt.Errorf("blob %s corrupted: %w", digest, err)
	}
	return nil
}

func verifyBlobFile(path, digest string) error {
	algorithm, encoded, ok := splitDigestReference(digest)
	if !ok {
		return fmt.Errorf("invalid digest reference")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

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

func manifestRequestPath(repository, reference string) string {
	return "/v2/" + repository + "/manifests/" + reference
}
