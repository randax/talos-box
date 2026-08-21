package imagecache

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const factoryURL = "https://factory.talos.dev"

const (
	schematicRequestTimeout    = 30 * time.Second
	imageDialTimeout           = 10 * time.Second
	imageTLSHandshakeTimeout   = 10 * time.Second
	imageResponseHeaderTimeout = 30 * time.Second
)

var xzMagic = []byte{0xfd, 0x37, 0x7a, 0x58, 0x5a, 0x00}

// Architecture identifies a Talos Image Factory machine architecture.
type Architecture string

const (
	ArchitectureAMD64 Architecture = "amd64"
	ArchitectureARM64 Architecture = "arm64"
)

// Cache stores Talos disk images by schematic, version, and architecture.
type Cache struct {
	root            string
	factoryURL      string
	schematicClient *http.Client
	downloadClient  *http.Client
}

// Entry is a ready-to-use disk image in the cache.
type Entry struct {
	Schematic    string
	Version      string
	Architecture Architecture
	Path         string
	Size         int64
	// AllocatedSize is what the image costs on disk. A disk.raw is sparse,
	// so Size is its apparent extent, not its footprint.
	AllocatedSize int64
	// Incomplete marks a combination that holds prunable artifacts but no
	// usable disk.raw. Listing it is what makes `cache list` a faithful
	// prune preview: prune deletes such a combination like any other.
	Incomplete bool
}

type MirrorUpstreamStats struct {
	Upstream      string
	BlobCount     int
	BlobBytes     int64
	ManifestCount int
	ManifestBytes int64
}

type MirrorTotals struct {
	BlobCount     int
	BlobBytes     int64
	ManifestCount int
	ManifestBytes int64
}

// Combination identifies one disk-image directory in the cache: the unit both
// pinning and reference-aware pruning reason about.
type Combination struct {
	Schematic    string
	Version      string
	Architecture Architecture
}

// PrunedCombination is one combination a prune removed, with the bytes its
// artifacts occupied.
type PrunedCombination struct {
	Combination
	Bytes int64
}

type CachePruneResult struct {
	ImageCount int
	ImageBytes int64
	// KeptImages counts combinations the keep classifier retained, so a
	// zero-prune can be explained instead of looking like an empty cache.
	KeptImages int
	Images     []PrunedCombination
	Mirror     MirrorTotals
}

// New returns a cache rooted at root.
func New(root string) *Cache {
	return &Cache{
		root:            root,
		factoryURL:      factoryURL,
		schematicClient: &http.Client{Timeout: schematicRequestTimeout},
		downloadClient:  newDownloadClient(),
	}
}

func newDownloadClient() *http.Client {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if ok {
		transport = transport.Clone()
	} else {
		// DefaultTransport was replaced with a custom RoundTripper
		transport = &http.Transport{
			Proxy:             http.ProxyFromEnvironment,
			ForceAttemptHTTP2: true,
		}
	}
	transport.DialContext = (&net.Dialer{
		Timeout:   imageDialTimeout,
		KeepAlive: 30 * time.Second,
	}).DialContext
	transport.TLSHandshakeTimeout = imageTLSHandshakeTimeout
	transport.ResponseHeaderTimeout = imageResponseHeaderTimeout

	return &http.Client{Transport: transport}
}

// DefaultRoot is the cache directory under the current user's home.
func DefaultRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return filepath.Join(home, ".talosbox", "cache"), nil
}

// NewDefault returns a cache under the current user's home directory.
func NewDefault() (*Cache, error) {
	root, err := DefaultRoot()
	if err != nil {
		return nil, err
	}
	return New(root), nil
}

// Ensure returns a decompressed disk image for architecture, downloading it
// when necessary.
func (c *Cache) Ensure(schematic, version string, architecture Architecture) (string, error) {
	if err := validateComponent("schematic", schematic); err != nil {
		return "", err
	}
	if err := validateComponent("version", version); err != nil {
		return "", err
	}
	if err := validateArchitecture(architecture); err != nil {
		return "", err
	}

	dir := filepath.Join(c.root, schematic, version, string(architecture))
	diskPath := filepath.Join(dir, "disk.raw")
	if fileReady(diskPath) {
		return diskPath, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create cache directory: %w", err)
	}
	if architecture == ArchitectureARM64 {
		legacyPath := filepath.Join(c.root, schematic, version, "disk.raw")
		migrated, err := migrateLegacyDisk(legacyPath, diskPath)
		if err != nil {
			return "", err
		}
		if migrated {
			return diskPath, nil
		}
	}

	asset := fmt.Sprintf("metal-%s.raw.xz", architecture)
	archivePath := filepath.Join(dir, asset)
	if !fileReady(archivePath) {
		assetURL := fmt.Sprintf("%s/image/%s/%s/%s",
			strings.TrimRight(c.factoryURL, "/"), url.PathEscape(schematic), url.PathEscape(version), asset)
		if err := c.download(assetURL, archivePath); err != nil {
			return "", err
		}
	}
	if err := decompress(archivePath, diskPath); err != nil {
		return "", err
	}

	return diskPath, nil
}

// List returns the disk-image combinations currently in the cache: the
// complete images plus the incomplete combinations a prune would remove, so a
// listing is the prune preview it is documented to be.
func (c *Cache) List() ([]Entry, error) {
	schematics, err := os.ReadDir(c.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list cache: %w", err)
	}

	var entries []Entry
	for _, schematic := range schematics {
		if !schematic.IsDir() {
			continue
		}
		versions, err := os.ReadDir(filepath.Join(c.root, schematic.Name()))
		if err != nil {
			return nil, fmt.Errorf("list schematic %q: %w", schematic.Name(), err)
		}
		for _, version := range versions {
			if !version.IsDir() {
				continue
			}
			versionDir := filepath.Join(c.root, schematic.Name(), version.Name())
			for _, architecture := range []Architecture{ArchitectureAMD64, ArchitectureARM64} {
				entry, ok, err := combinationEntry(schematic.Name(), version.Name(), architecture, combinationDirs(versionDir, architecture))
				if err != nil {
					return nil, err
				}
				if ok {
					entries = append(entries, entry)
				}
			}
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Schematic != entries[j].Schematic {
			return entries[i].Schematic < entries[j].Schematic
		}
		if entries[i].Version != entries[j].Version {
			return entries[i].Version < entries[j].Version
		}
		return entries[i].Architecture < entries[j].Architecture
	})

	return entries, nil
}

func (c *Cache) MirrorStats() ([]MirrorUpstreamStats, MirrorTotals, error) {
	mirrorRoot := filepath.Join(c.root, "mirror")
	entries, err := os.ReadDir(mirrorRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil, MirrorTotals{}, nil
	}
	if err != nil {
		return nil, MirrorTotals{}, fmt.Errorf("list mirror cache: %w", err)
	}

	var stats []MirrorUpstreamStats
	var totals MirrorTotals
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		stat := MirrorUpstreamStats{Upstream: entry.Name()}
		if err := walkMirrorObjects(filepath.Join(mirrorRoot, entry.Name(), "blobs"), func(size int64) {
			stat.BlobCount++
			stat.BlobBytes += size
		}); err != nil {
			return nil, MirrorTotals{}, err
		}
		if err := walkMirrorObjects(filepath.Join(mirrorRoot, entry.Name(), "manifests"), func(size int64) {
			stat.ManifestCount++
			stat.ManifestBytes += size
		}, ".meta", ".ct"); err != nil {
			return nil, MirrorTotals{}, err
		}
		totals.BlobCount += stat.BlobCount
		totals.BlobBytes += stat.BlobBytes
		totals.ManifestCount += stat.ManifestCount
		totals.ManifestBytes += stat.ManifestBytes
		stats = append(stats, stat)
	}

	sort.Slice(stats, func(i, j int) bool { return stats[i].Upstream < stats[j].Upstream })
	return stats, totals, nil
}

func (c *Cache) PruneDisk() (CachePruneResult, error) {
	return c.PruneDiskExcept(nil)
}

// PruneDiskExcept removes cached disk images except the combinations keep
// reports as worth keeping. A nil keep removes every combination, which is
// what the explicit whole-cache scope asks for. Nothing is removed when keep
// fails: an undecidable classification must never widen the deletion.
func (c *Cache) PruneDiskExcept(keep func(Combination) (bool, error)) (CachePruneResult, error) {
	if err := validateCacheRoot(c.root); err != nil {
		return CachePruneResult{}, err
	}
	result, err := c.pruneKnownDiskArtifacts(keep)
	if err != nil {
		return CachePruneResult{}, fmt.Errorf("prune disk cache: %w", err)
	}
	return result, nil
}

func (c *Cache) PruneMirror() (CachePruneResult, error) {
	if err := validateCacheRoot(c.root); err != nil {
		return CachePruneResult{}, err
	}
	plan, err := c.planMirrorPrune()
	if err != nil {
		return CachePruneResult{}, fmt.Errorf("prune mirror cache: %w", err)
	}
	if err := c.executeMirrorPrune(plan); err != nil {
		return CachePruneResult{}, fmt.Errorf("prune mirror cache: %w", err)
	}
	return CachePruneResult{Mirror: plan.totals}, nil
}

func (c *Cache) PruneAll() (CachePruneResult, error) {
	if err := validateCacheRoot(c.root); err != nil {
		return CachePruneResult{}, err
	}
	mirrorPlan, err := c.planMirrorPrune()
	if err != nil {
		return CachePruneResult{}, fmt.Errorf("prune mirror cache: %w", err)
	}
	disk, err := c.PruneDisk()
	if err != nil {
		return CachePruneResult{}, err
	}
	if err := c.executeMirrorPrune(mirrorPlan); err != nil {
		return CachePruneResult{}, err
	}
	disk.Mirror = mirrorPlan.totals
	return disk, nil
}

func (c *Cache) download(sourceURL, destination string) error {
	response, err := c.downloadClient.Get(sourceURL)
	if err != nil {
		return fmt.Errorf("download image: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("download image: %s", response.Status)
	}
	if strings.EqualFold(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]), "text/html") {
		return fmt.Errorf("download image %s: response is text/html; possible proxy block page", sourceURL)
	}

	prefix := make([]byte, len(xzMagic))
	if _, err := io.ReadFull(response.Body, prefix); err != nil {
		return fmt.Errorf("download image %s: read response prefix: %w", sourceURL, err)
	}
	if !bytes.Equal(prefix, xzMagic) {
		return fmt.Errorf("download image %s: response does not start with XZ magic; possible proxy block page", sourceURL)
	}

	temporary, err := os.CreateTemp(filepath.Dir(destination), "."+filepath.Base(destination)+"-*")
	if err != nil {
		return fmt.Errorf("create image download: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()

	if _, err := io.Copy(temporary, io.MultiReader(bytes.NewReader(prefix), response.Body)); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write image download: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close image download: %w", err)
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return fmt.Errorf("publish image download: %w", err)
	}

	return nil
}

func decompress(source, destination string) error {
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".disk.raw-*")
	if err != nil {
		return fmt.Errorf("create decompressed image: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()

	command := exec.Command("xz", "-dc", source)
	command.Stdout = temporary
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("decompress image: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close decompressed image: %w", err)
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return fmt.Errorf("publish decompressed image: %w", err)
	}

	return nil
}

func fileReady(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

func validateCacheRoot(root string) error {
	if root == "" || filepath.Clean(root) == string(filepath.Separator) {
		return errors.New("refusing to prune an empty or root cache path")
	}
	if info, exists, err := lstatPath(root); err != nil {
		return err
	} else if exists {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to prune symlink %q", root)
		}
		if !info.IsDir() {
			return fmt.Errorf("refusing to prune non-directory cache path %q", root)
		}
	}
	return nil
}

type pruneAction struct {
	path string
	size int64
	// temporary marks an abandoned partial download. A combination made up
	// of nothing else is swept silently: the space is reclaimed but the
	// combination is neither listed nor counted, because `cache list` never
	// showed it either.
	temporary bool
}

// combinationPlan groups the artifacts of one combination so a prune can
// report what it removed per combination, not just in total.
type combinationPlan struct {
	combination Combination
	actions     []pruneAction
}

// reportable answers whether a plan removes a combination a user would
// recognize from `cache list`, which is both what gets itemized and what the
// image count counts.
func (p combinationPlan) reportable() bool {
	for _, action := range p.actions {
		if !action.temporary {
			return true
		}
	}
	return false
}

type mirrorPrunePlan struct {
	root   string
	totals MirrorTotals
}

func (c *Cache) pruneKnownDiskArtifacts(keep func(Combination) (bool, error)) (CachePruneResult, error) {
	rootEntries, err := os.ReadDir(c.root)
	if errors.Is(err, os.ErrNotExist) {
		return CachePruneResult{}, nil
	}
	if err != nil {
		return CachePruneResult{}, err
	}

	var result CachePruneResult
	var plan []combinationPlan
	var cleanupDirs []string
	for _, schematic := range rootEntries {
		if schematic.Name() == "mirror" || !schematic.IsDir() {
			continue
		}
		schematicPath := filepath.Join(c.root, schematic.Name())
		if err := requireDirectoryPath(schematicPath); err != nil {
			return CachePruneResult{}, err
		}
		versionEntries, err := os.ReadDir(schematicPath)
		if err != nil {
			return CachePruneResult{}, err
		}
		touchedSchematic := false
		for _, version := range versionEntries {
			if !version.IsDir() {
				continue
			}
			versionPath := filepath.Join(schematicPath, version.Name())
			if err := requireDirectoryPath(versionPath); err != nil {
				return CachePruneResult{}, err
			}
			versionPlan, versionCleanup, keptVersion, touchedVersion, err := planKnownVersionPrune(versionPath, schematic.Name(), version.Name(), keep)
			if err != nil {
				return CachePruneResult{}, err
			}
			result.KeptImages += keptVersion
			if !touchedVersion {
				continue
			}
			touchedSchematic = true
			plan = append(plan, versionPlan...)
			cleanupDirs = append(cleanupDirs, versionCleanup...)
			cleanupDirs = append(cleanupDirs, versionPath)
		}
		if touchedSchematic {
			cleanupDirs = append(cleanupDirs, schematicPath)
		}
	}
	for _, combination := range plan {
		var bytes int64
		for _, action := range combination.actions {
			if err := os.Remove(action.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return CachePruneResult{}, err
			}
			bytes += action.size
		}
		if combination.reportable() {
			// Count and bytes both cover exactly the reported combinations, so
			// the summary line, the byte total and the itemized list cannot
			// disagree. Abandoned temporaries are swept silently: their bytes
			// belong to no image the user was ever shown.
			result.ImageCount++
			result.ImageBytes += bytes
			result.Images = append(result.Images, PrunedCombination{Combination: combination.combination, Bytes: bytes})
		}
	}
	for _, path := range cleanupDirs {
		if err := removeIfEmpty(path); err != nil {
			return CachePruneResult{}, err
		}
	}
	return result, nil
}

func planKnownVersionPrune(versionPath, schematic, version string, keep func(Combination) (bool, error)) ([]combinationPlan, []string, int, bool, error) {
	var plan []combinationPlan
	var cleanupDirs []string
	keptCount := 0
	touched := false
	arm64Kept := false
	arm64PlanIndex := -1
	for _, architecture := range []Architecture{ArchitectureAMD64, ArchitectureARM64} {
		archDir := filepath.Join(versionPath, string(architecture))
		exists, err := pathExists(archDir)
		if err != nil {
			return nil, nil, 0, false, err
		}
		if exists {
			touched = true
			combination := Combination{Schematic: schematic, Version: version, Architecture: architecture}
			kept, err := keepCombination(keep, combination)
			if err != nil {
				return nil, nil, 0, false, err
			}
			if kept {
				// Count (and dedupe against the legacy layout) only when the
				// combination holds artifacts: cache list shows exactly those,
				// and the kept report must agree with it.
				present, err := artifactsPresent(archDir, knownDiskArtifactNames(architecture, true))
				if err != nil {
					return nil, nil, 0, false, err
				}
				if present {
					keptCount++
					if architecture == ArchitectureARM64 {
						arm64Kept = true
					}
				}
				// A kept combination still sheds abandoned partial
				// downloads: they are safe to delete regardless of
				// retention and would otherwise accumulate forever.
				if err := requireDirectoryPath(archDir); err != nil {
					return nil, nil, 0, false, err
				}
				tempPlan, err := planKnownFiles(archDir, nil, []knownPrunePrefix{
					{prefix: ".disk.raw-"},
					{prefix: fmt.Sprintf(".metal-%s.raw.xz-", architecture)},
				})
				if err != nil {
					return nil, nil, 0, false, err
				}
				if len(tempPlan) != 0 {
					plan = append(plan, combinationPlan{combination: combination, actions: tempPlan})
				}
				continue
			}
			if err := requireDirectoryPath(archDir); err != nil {
				return nil, nil, 0, false, err
			}
			// The pin marker is in the removal set: a survivor would keep the
			// directory alive and re-pin an image the next pull downloads.
			archPlan, err := planKnownFiles(archDir, knownDiskArtifactNames(architecture, true), []knownPrunePrefix{
				{prefix: ".disk.raw-"},
				{prefix: fmt.Sprintf(".metal-%s.raw.xz-", architecture)},
			})
			if err != nil {
				return nil, nil, 0, false, err
			}
			if architecture == ArchitectureARM64 {
				arm64PlanIndex = len(plan)
			}
			plan = append(plan, combinationPlan{combination: combination, actions: archPlan})
			cleanupDirs = append(cleanupDirs, archDir)
		}
	}
	// The legacy flat layout only ever held arm64 images. It is the same
	// combination as the arm64 architecture directory, so keep decisions,
	// kept counting, and the pruned-combination report must all treat the
	// two layouts as one.
	legacy := Combination{Schematic: schematic, Version: version, Architecture: ArchitectureARM64}
	kept, err := keepCombination(keep, legacy)
	if err != nil {
		return nil, nil, 0, false, err
	}
	legacyPrefixes := []knownPrunePrefix{
		{prefix: ".disk.raw-"},
		{prefix: fmt.Sprintf(".metal-%s.raw.xz-", ArchitectureARM64)},
	}
	if kept {
		// A kept legacy combination still sheds abandoned partial downloads,
		// exactly like the kept architecture-directory branch above.
		tempPlan, err := planKnownFiles(versionPath, nil, legacyPrefixes)
		if err != nil {
			return nil, nil, 0, false, err
		}
		if len(tempPlan) != 0 {
			plan = append(plan, combinationPlan{combination: legacy, actions: tempPlan})
		}
		present, err := artifactsPresent(versionPath, knownDiskArtifactNames(ArchitectureARM64, false))
		if err != nil {
			return nil, nil, 0, false, err
		}
		if present && !arm64Kept {
			keptCount++
		}
	} else {
		legacyPlan, err := planKnownFiles(versionPath, knownDiskArtifactNames(ArchitectureARM64, false), legacyPrefixes)
		if err != nil {
			return nil, nil, 0, false, err
		}
		if len(legacyPlan) != 0 {
			if arm64PlanIndex >= 0 {
				plan[arm64PlanIndex].actions = append(plan[arm64PlanIndex].actions, legacyPlan...)
			} else {
				plan = append(plan, combinationPlan{combination: legacy, actions: legacyPlan})
			}
		}
	}
	return plan, cleanupDirs, keptCount, touched || len(plan) > 0, nil
}

func keepCombination(keep func(Combination) (bool, error), combination Combination) (bool, error) {
	if keep == nil {
		return false, nil
	}
	return keep(combination)
}

type knownPrunePrefix struct {
	prefix string
}

func planKnownFiles(dir string, names []string, prefixes []knownPrunePrefix) ([]pruneAction, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]struct{}, len(names))
	for _, name := range names {
		byName[name] = struct{}{}
	}
	var plan []pruneAction
	for _, entry := range entries {
		if _, ok := byName[entry.Name()]; ok {
			action, err := planKnownArtifact(filepath.Join(dir, entry.Name()), false)
			if err != nil {
				return nil, err
			}
			if action.path != "" {
				plan = append(plan, action)
			}
			continue
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(entry.Name(), prefix.prefix) {
				action, err := planKnownArtifact(filepath.Join(dir, entry.Name()), true)
				if err != nil {
					return nil, err
				}
				if action.path != "" {
					plan = append(plan, action)
				}
				break
			}
		}
	}
	return plan, nil
}

func planKnownArtifact(path string, temporary bool) (pruneAction, error) {
	info, exists, err := lstatPath(path)
	if err != nil {
		return pruneAction{}, err
	}
	if !exists {
		return pruneAction{}, nil
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return pruneAction{}, fmt.Errorf("refusing to prune symlink %q", path)
	}
	if !info.Mode().IsRegular() {
		return pruneAction{}, fmt.Errorf("refusing to prune non-regular path %q", path)
	}
	return pruneAction{path: path, size: info.Size(), temporary: temporary}, nil
}

func (c *Cache) planMirrorPrune() (mirrorPrunePlan, error) {
	plan := mirrorPrunePlan{root: filepath.Join(c.root, "mirror")}
	if _, exists, err := lstatPath(plan.root); err != nil {
		return mirrorPrunePlan{}, err
	} else if exists {
		if err := requireDirectoryPath(plan.root); err != nil {
			return mirrorPrunePlan{}, err
		}
	}
	_, totals, err := c.MirrorStats()
	if err != nil {
		return mirrorPrunePlan{}, err
	}
	plan.totals = totals
	return plan, nil
}

func (c *Cache) executeMirrorPrune(plan mirrorPrunePlan) error {
	if err := os.RemoveAll(plan.root); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func removeIfEmpty(path string) error {
	info, exists, err := lstatPath(path)
	if err != nil || !exists {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to prune symlink %q", path)
	}
	if !info.IsDir() {
		return fmt.Errorf("refusing to prune non-directory path %q", path)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return nil
	}
	return os.Remove(path)
}

func requireDirectoryPath(path string) error {
	info, exists, err := lstatPath(path)
	if err != nil || !exists {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to prune symlink %q", path)
	}
	if !info.IsDir() {
		return fmt.Errorf("refusing to prune non-directory path %q", path)
	}
	return nil
}

func pathExists(path string) (bool, error) {
	_, exists, err := lstatPath(path)
	return exists, err
}

func lstatPath(path string) (os.FileInfo, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return info, true, nil
}

func walkMirrorObjects(root string, visit func(int64), ignoredSuffixes ...string) error {
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if strings.HasPrefix(info.Name(), ".partial") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(info.Name(), ".partial") {
			return nil
		}
		for _, suffix := range ignoredSuffixes {
			if strings.HasSuffix(info.Name(), suffix) {
				return nil
			}
		}
		visit(info.Size())
		return nil
	})
}

func migrateLegacyDisk(legacyPath, destination string) (bool, error) {
	if !fileReady(legacyPath) {
		return false, nil
	}
	if err := os.Rename(legacyPath, destination); err != nil {
		if fileReady(destination) {
			return true, nil
		}
		return false, fmt.Errorf("migrate legacy arm64 image: %w", err)
	}
	return true, nil
}

func cacheEntry(schematic, version string, architecture Architecture, path string) (Entry, bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fmt.Errorf("stat cached image %q: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return Entry{}, false, nil
	}
	return Entry{
		Schematic:     schematic,
		Version:       version,
		Architecture:  architecture,
		Path:          path,
		Size:          info.Size(),
		AllocatedSize: AllocatedSize(info),
	}, true, nil
}

// artifactDir is one on-disk home of a combination: its architecture
// directory, plus the pre-architecture flat layout for arm64.
type artifactDir struct {
	path  string
	names []string
}

func combinationDirs(versionDir string, architecture Architecture) []artifactDir {
	dirs := []artifactDir{{
		path:  filepath.Join(versionDir, string(architecture)),
		names: knownDiskArtifactNames(architecture, true),
	}}
	if architecture == ArchitectureARM64 {
		dirs = append(dirs, artifactDir{path: versionDir, names: knownDiskArtifactNames(architecture, false)})
	}
	return dirs
}

// knownDiskArtifactNames are the non-temporary files a prune removes for one
// combination. List and prune share them, so a listing cannot omit something a
// prune would delete. The flat legacy layout never carried a pin marker.
func knownDiskArtifactNames(architecture Architecture, includePin bool) []string {
	names := []string{"disk.raw", fmt.Sprintf("metal-%s.raw.xz", architecture)}
	if includePin {
		names = append(names, pinMarkerName)
	}
	return names
}

// combinationEntry describes one combination: the ready image when there is
// one, otherwise the incomplete leftovers a prune would reclaim.
func combinationEntry(schematic, version string, architecture Architecture, dirs []artifactDir) (Entry, bool, error) {
	for _, dir := range dirs {
		entry, ok, err := cacheEntry(schematic, version, architecture, filepath.Join(dir.path, "disk.raw"))
		if err != nil || ok {
			return entry, ok, err
		}
	}
	var entry Entry
	for _, dir := range dirs {
		size, allocated, present, err := artifactStats(dir.path, dir.names)
		if err != nil {
			return Entry{}, false, err
		}
		if !present {
			continue
		}
		if entry.Path == "" {
			entry.Path = dir.path
		}
		entry.Size += size
		entry.AllocatedSize += allocated
	}
	if entry.Path == "" {
		return Entry{}, false, nil
	}
	entry.Schematic, entry.Version, entry.Architecture, entry.Incomplete = schematic, version, architecture, true
	return entry, true, nil
}

// artifactStats sums the named artifacts present in dir. Non-regular files are
// ignored: a prune refuses them rather than reclaiming them.
func artifactStats(dir string, names []string) (int64, int64, bool, error) {
	var size, allocated int64
	present := false
	for _, name := range names {
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, 0, false, fmt.Errorf("stat cached artifact %q: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		present = true
		size += info.Size()
		allocated += AllocatedSize(info)
	}
	return size, allocated, present, nil
}

// artifactsPresent reports whether a combination holds anything a listing
// shows, so the kept report counts exactly the rows that survive a prune.
func artifactsPresent(dir string, names []string) (bool, error) {
	_, _, present, err := artifactStats(dir, names)
	return present, err
}

func validateArchitecture(architecture Architecture) error {
	switch architecture {
	case ArchitectureAMD64, ArchitectureARM64:
		return nil
	default:
		return fmt.Errorf("unsupported architecture %q", architecture)
	}
}

func validateComponent(name, value string) error {
	if value == "" || value == "." || value == ".." || filepath.Base(value) != value {
		return fmt.Errorf("invalid %s %q", name, value)
	}
	return nil
}
