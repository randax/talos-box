package daemon

import (
	"slices"

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

// statuses reports every keep-reason that applies to a combination, so a
// listing can show what a prune would actually weigh: an image can be pinned
// *and* in use, and reporting only the strongest reason makes it look prunable
// once the weaker one lapses (#407). Orphan is the reason-less answer, so it is
// never reported beside another.
func (c *cacheImageClassifier) statuses(combination imagecache.Combination) ([]CacheImageStatus, []string, error) {
	pinned, err := c.cache.Pinned(combination.Schematic, combination.Version, combination.Architecture)
	if err != nil {
		return nil, nil, err
	}
	clusters := c.references[combination]
	var reasons []CacheImageStatus
	if pinned {
		reasons = append(reasons, CacheImageStatusPinned)
	}
	if c.hasDefault && combination == c.defaultCombination {
		reasons = append(reasons, CacheImageStatusDefault)
	}
	if len(clusters) > 0 {
		reasons = append(reasons, CacheImageStatusInUse)
	}
	if len(reasons) == 0 {
		return []CacheImageStatus{CacheImageStatusOrphan}, nil, nil
	}
	return reasons, clusters, nil
}

// status is the single strongest reason, which prune and the stray report
// already reason about.
func (c *cacheImageClassifier) status(combination imagecache.Combination) (CacheImageStatus, []string, error) {
	reasons, clusters, err := c.statuses(combination)
	if err != nil {
		return "", nil, err
	}
	return primaryCacheImageStatus(reasons), clusters, nil
}

func primaryCacheImageStatus(reasons []CacheImageStatus) CacheImageStatus {
	for _, want := range []CacheImageStatus{CacheImageStatusInUse, CacheImageStatusPinned, CacheImageStatusDefault} {
		if slices.Contains(reasons, want) {
			return want
		}
	}
	return CacheImageStatusOrphan
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
