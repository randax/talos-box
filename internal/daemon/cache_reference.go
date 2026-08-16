package daemon

import (
	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/imagecache"
)

// CacheImageStatus is the reason a cached disk-image combination is worth
// keeping. `cache list` renders it and `cache prune` acts on it, from one
// classification, so what a user reads is the decision prune makes.
type CacheImageStatus string

const (
	CacheImageStatusInUse   CacheImageStatus = "in-use"
	CacheImageStatusPinned  CacheImageStatus = "pinned"
	CacheImageStatusDefault CacheImageStatus = "default"
	CacheImageStatusOrphan  CacheImageStatus = "orphan"
)

// cacheImageClassifier answers why a combination is worth keeping. It is built
// once per operation so a listing and the prune it describes see the same
// clusters, pins, and default combination.
type cacheImageClassifier struct {
	cache              *imagecache.Cache
	references         map[imagecache.Combination][]string
	defaultCombination imagecache.Combination
	hasDefault         bool
}

func (s *Server) cacheImageClassifier() (*cacheImageClassifier, error) {
	defaultSchematic, hasDefault, err := s.recordedDefaultSchematic()
	if err != nil {
		return nil, err
	}
	classifier := &cacheImageClassifier{cache: s.cache, hasDefault: hasDefault}
	if hasDefault {
		classifier.defaultCombination = imagecache.Combination{
			Schematic:    defaultSchematic,
			Version:      DefaultTalosVersion,
			Architecture: s.imageArchitecture(),
		}
	}
	if classifier.references, err = s.clusterImageReferences(defaultSchematic); err != nil {
		return nil, err
	}
	return classifier, nil
}

func (c *cacheImageClassifier) status(combination imagecache.Combination) (CacheImageStatus, []string, error) {
	if clusters := c.references[combination]; len(clusters) > 0 {
		return CacheImageStatusInUse, clusters, nil
	}
	pinned, err := c.cache.Pinned(combination.Schematic, combination.Version, combination.Architecture)
	if err != nil {
		return "", nil, err
	}
	switch {
	case pinned:
		return CacheImageStatusPinned, nil, nil
	case c.hasDefault && combination == c.defaultCombination:
		return CacheImageStatusDefault, nil, nil
	default:
		return CacheImageStatusOrphan, nil, nil
	}
}

// keep is prune's predicate: everything that carries a reason to exist stays.
func (c *cacheImageClassifier) keep(combination imagecache.Combination) (bool, error) {
	status, _, err := c.status(combination)
	if err != nil {
		return false, err
	}
	return status != CacheImageStatusOrphan, nil
}

// recordedDefaultSchematic resolves the built-in default combination's
// schematic from disk only. Retention must never depend on the Factory being
// reachable, so an unresolved default simply spares nothing extra.
func (s *Server) recordedDefaultSchematic() (string, bool, error) {
	if s.defaultSchematic != "" {
		return s.defaultSchematic, true, nil
	}
	return s.cache.RecordedDefaultSchematic()
}

// clusterImageReferences maps every combination a persisted cluster would boot
// from to the clusters referencing it. Stored state already carries the
// composed schematic id, so this resolution stays offline.
func (s *Server) clusterImageReferences(defaultSchematic string) (map[imagecache.Combination][]string, error) {
	items, err := cluster.List()
	if err != nil {
		return nil, err
	}
	references := make(map[imagecache.Combination][]string, len(items))
	for _, item := range items {
		schematic := item.Schematic
		if schematic == "" {
			schematic = defaultSchematic
		}
		if schematic == "" {
			continue
		}
		version := item.TalosVersion
		if version == "" {
			version = DefaultTalosVersion
		}
		architecture := item.ImageArchitecture
		if architecture == "" {
			architecture = cluster.LegacyImageArchitecture
		}
		combination := imagecache.Combination{
			Schematic:    schematic,
			Version:      version,
			Architecture: imagecache.Architecture(architecture),
		}
		references[combination] = append(references[combination], item.Name)
	}
	return references, nil
}
