package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/randax/talos-box/internal/imagecache"
)

type Platform struct {
	OS           string
	Architecture imagecache.Architecture
}

type CacheTarget struct {
	Repository string
	Tag        string
	Digest     string
	Platform   Platform
}

type CacheGapKind string

const (
	CacheGapTagMapping       CacheGapKind = "tag-mapping"
	CacheGapRootManifest     CacheGapKind = "root-manifest"
	CacheGapPlatformManifest CacheGapKind = "platform-manifest"
	CacheGapConfig           CacheGapKind = "config"
	CacheGapLayer            CacheGapKind = "layer"
	CacheGapCorrupt          CacheGapKind = "corrupt"
)

type CacheGap struct {
	Kind   CacheGapKind
	Path   string
	Digest string
	Detail string
	object string
}

type InspectOptions struct{ Deep bool }

type CacheStatus struct {
	Target       CacheTarget
	RootDigest   string
	ManifestData []byte
	Gaps         []CacheGap
	layerTotal   int
}

func (s CacheStatus) Complete() bool { return len(s.Gaps) == 0 }

func (s CacheStatus) Reason() string {
	if s.Complete() {
		return "complete"
	}
	gap := s.Gaps[0]
	platform := platformName(s.Target.Platform)
	switch gap.Kind {
	case CacheGapTagMapping:
		if gap.Detail != "" {
			return fmt.Sprintf("tag mapping %s invalid: %s", gap.Path, gap.Detail)
		}
		return fmt.Sprintf("tag mapping %s not cached", gap.Path)
	case CacheGapRootManifest:
		if gap.Detail != "" {
			return fmt.Sprintf("root manifest %s corrupted: %s", gap.Digest, gap.Detail)
		}
		return fmt.Sprintf("root manifest %s not cached", gap.Digest)
	case CacheGapPlatformManifest:
		if gap.Detail != "" {
			return fmt.Sprintf("index present; %s manifest %s invalid: %s", platform, gap.Digest, gap.Detail)
		}
		return fmt.Sprintf("index present; %s manifest %s not cached", platform, gap.Digest)
	case CacheGapConfig:
		return fmt.Sprintf("%s manifest present; config %s not cached", platform, gap.Digest)
	case CacheGapLayer:
		missing := make([]string, 0, len(s.Gaps))
		for _, candidate := range s.Gaps {
			if candidate.Kind == CacheGapLayer {
				missing = append(missing, candidate.Digest)
			}
		}
		return fmt.Sprintf("%d of %d %s layers not cached: %s", len(missing), s.layerTotal, platform, strings.Join(missing, ", "))
	case CacheGapCorrupt:
		object := gap.object
		if object == "" {
			object = "blob"
		}
		return fmt.Sprintf("%s %s corrupted: %s", object, gap.Digest, gap.Detail)
	default:
		return gap.Detail
	}
}

func platformName(platform Platform) string {
	osName := platform.OS
	if osName == "" {
		osName = "linux"
	}
	return osName + "/" + string(platform.Architecture)
}

// InspectCached is the mirror's single completeness predicate. It only walks
// the selected platform because foreign manifests cannot help the guest pull.
func (s *Server) InspectCached(ctx context.Context, target CacheTarget, opts InspectOptions) CacheStatus {
	status := CacheStatus{Target: target}
	if err := ctx.Err(); err != nil {
		status.Gaps = append(status.Gaps, CacheGap{Kind: CacheGapRootManifest, Digest: target.Digest, Detail: err.Error()})
		return status
	}

	rootDigest := target.Digest
	if target.Tag != "" {
		tagPath := manifestRequestPath(target.Repository, target.Tag)
		data, path, err := checkedCachedManifest(s, tagPath)
		if err != nil {
			status.Gaps = append(status.Gaps, CacheGap{Kind: CacheGapTagMapping, Path: tagPath, Detail: cacheReadDetail(err)})
			return status
		}
		metadata := checkedCachedManifestMetadataAtPath(tagPath, path, data)
		mapped, err := verifySupportedDigest(data, metadata.DockerContentDigest)
		if err != nil || (rootDigest != "" && mapped != rootDigest) {
			detail := "digest does not match cached manifest"
			if err == nil {
				detail = fmt.Sprintf("maps to %s, want %s", mapped, rootDigest)
			}
			status.Gaps = append(status.Gaps, CacheGap{Kind: CacheGapTagMapping, Path: tagPath, Digest: mapped, Detail: detail})
			return status
		}
		rootDigest = mapped
	}
	status.RootDigest = rootDigest
	if _, _, err := checkedSupportedDigest(rootDigest); err != nil {
		status.Gaps = append(status.Gaps, CacheGap{Kind: CacheGapRootManifest, Digest: rootDigest, Detail: err.Error()})
		return status
	}

	rootPath := manifestRequestPath(target.Repository, rootDigest)
	root, _, err := checkedCachedManifest(s, rootPath)
	if err != nil {
		status.Gaps = append(status.Gaps, CacheGap{Kind: CacheGapRootManifest, Path: rootPath, Digest: rootDigest, Detail: cacheReadDetail(err)})
		return status
	}
	if _, err := verifySupportedDigest(root, rootDigest); err != nil {
		status.Gaps = append(status.Gaps, CacheGap{Kind: CacheGapRootManifest, Path: rootPath, Digest: rootDigest, Detail: err.Error()})
		return status
	}
	status.ManifestData = root
	manifest, err := decodeCachedGraph(root)
	if err != nil {
		status.Gaps = append(status.Gaps, CacheGap{Kind: CacheGapRootManifest, Path: rootPath, Digest: rootDigest, Detail: err.Error()})
		return status
	}
	if len(manifest.Manifests) > 0 || strings.Contains(manifest.MediaType, "index") || strings.Contains(manifest.MediaType, "manifest.list") {
		selected := ""
		for _, child := range manifest.Manifests {
			if child.Platform.Architecture == string(target.Platform.Architecture) && (child.Platform.OS == "" || child.Platform.OS == targetOS(target.Platform)) {
				selected = child.Digest
				break
			}
		}
		if selected == "" {
			status.Gaps = append(status.Gaps, CacheGap{Kind: CacheGapPlatformManifest, Detail: "no selected platform in index"})
			return status
		}
		if _, _, err := checkedSupportedDigest(selected); err != nil {
			status.Gaps = append(status.Gaps, CacheGap{Kind: CacheGapPlatformManifest, Digest: selected, Detail: err.Error()})
			return status
		}
		childPath := manifestRequestPath(target.Repository, selected)
		child, _, err := checkedCachedManifest(s, childPath)
		if err != nil {
			status.Gaps = append(status.Gaps, CacheGap{Kind: CacheGapPlatformManifest, Path: childPath, Digest: selected, Detail: cacheReadDetail(err)})
			return status
		}
		if _, err := verifySupportedDigest(child, selected); err != nil {
			status.Gaps = append(status.Gaps, CacheGap{Kind: CacheGapPlatformManifest, Path: childPath, Digest: selected, Detail: err.Error()})
			return status
		}
		manifest, err = decodeCachedGraph(child)
		if err != nil {
			status.Gaps = append(status.Gaps, CacheGap{Kind: CacheGapPlatformManifest, Path: childPath, Digest: selected, Detail: err.Error()})
			return status
		}
	}
	status.inspectBlobs(ctx, s, manifest, opts)
	return status
}

func targetOS(platform Platform) string {
	if platform.OS == "" {
		return "linux"
	}
	return platform.OS
}
func cacheReadDetail(err error) string {
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	return err.Error()
}

type cachedGraph struct {
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

func decodeCachedGraph(data []byte) (cachedGraph, error) {
	var graph cachedGraph
	if err := json.Unmarshal(data, &graph); err != nil {
		return graph, fmt.Errorf("decode manifest graph: %w", err)
	}
	return graph, nil
}

func (status *CacheStatus) inspectBlobs(ctx context.Context, server *Server, graph cachedGraph, opts InspectOptions) {
	if graph.Config.Digest != "" {
		status.inspectBlob(ctx, server, CacheGapConfig, "config", graph.Config.Digest, opts)
	}
	status.layerTotal = len(graph.Layers)
	for _, layer := range graph.Layers {
		status.inspectBlob(ctx, server, CacheGapLayer, "layer", layer.Digest, opts)
	}
}

func (status *CacheStatus) inspectBlob(ctx context.Context, server *Server, kind CacheGapKind, label, digest string, opts InspectOptions) {
	if ctx.Err() != nil {
		status.Gaps = append(status.Gaps, CacheGap{Kind: kind, Digest: digest, Detail: ctx.Err().Error()})
		return
	}
	if _, _, err := checkedSupportedDigest(digest); err != nil {
		status.Gaps = append(status.Gaps, CacheGap{Kind: kind, Digest: digest, Detail: err.Error()})
		return
	}
	file, _, err := openCheckedRegularFile(server.blobPath(digest))
	if err != nil {
		status.Gaps = append(status.Gaps, CacheGap{Kind: kind, Digest: digest, Detail: cacheReadDetail(err)})
		return
	}
	defer func() { _ = file.Close() }()
	if opts.Deep {
		if err := verifyBlobFile(file, digest); err != nil {
			status.Gaps = append(status.Gaps, CacheGap{Kind: CacheGapCorrupt, Path: server.blobPath(digest), Digest: digest, Detail: err.Error(), object: label})
		}
	}
}
