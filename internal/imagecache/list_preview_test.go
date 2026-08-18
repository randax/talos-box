package imagecache

import (
	"os"
	"path/filepath"
	"testing"
)

func writeCacheFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func listedCombinations(t *testing.T, cache *Cache) map[Combination]Entry {
	t.Helper()
	entries, err := cache.List()
	if err != nil {
		t.Fatal(err)
	}
	listed := make(map[Combination]Entry, len(entries))
	for _, entry := range entries {
		listed[Combination{Schematic: entry.Schematic, Version: entry.Version, Architecture: entry.Architecture}] = entry
	}
	return listed
}

func TestListReportsIncompleteCombinationsAsEntries(t *testing.T) {
	root := t.TempDir()
	cache := New(root)
	writeCacheFile(t, filepath.Join(root, "ready", "v1.13.6", "amd64", "disk.raw"), "disk-bytes")
	writeCacheFile(t, filepath.Join(root, "archive-only", "v1.13.6", "amd64", "metal-amd64.raw.xz"), "archive-bytes")
	writeCacheFile(t, filepath.Join(root, "empty-disk", "v1.13.6", "arm64", "disk.raw"), "")
	writeCacheFile(t, filepath.Join(root, "pin-only", "v1.13.6", "arm64", "pinned"), "")
	writeCacheFile(t, filepath.Join(root, "temp-only", "v1.13.6", "amd64", ".disk.raw-partial"), "partial")

	listed := listedCombinations(t, cache)
	for _, want := range []Combination{
		{Schematic: "ready", Version: "v1.13.6", Architecture: ArchitectureAMD64},
		{Schematic: "archive-only", Version: "v1.13.6", Architecture: ArchitectureAMD64},
		{Schematic: "empty-disk", Version: "v1.13.6", Architecture: ArchitectureARM64},
		{Schematic: "pin-only", Version: "v1.13.6", Architecture: ArchitectureARM64},
	} {
		if _, ok := listed[want]; !ok {
			t.Fatalf("combination %+v missing from cache list: %+v", want, listed)
		}
	}
	if entry := listed[Combination{Schematic: "ready", Version: "v1.13.6", Architecture: ArchitectureAMD64}]; entry.Incomplete {
		t.Fatalf("ready image reported as incomplete: %+v", entry)
	}
	incomplete := listed[Combination{Schematic: "archive-only", Version: "v1.13.6", Architecture: ArchitectureAMD64}]
	if !incomplete.Incomplete {
		t.Fatalf("archive-only combination = %+v, want Incomplete", incomplete)
	}
	if want := int64(len("archive-bytes")); incomplete.Size != want {
		t.Fatalf("archive-only size = %d, want the artifact bytes %d", incomplete.Size, want)
	}
	if _, ok := listed[Combination{Schematic: "temp-only", Version: "v1.13.6", Architecture: ArchitectureAMD64}]; ok {
		t.Fatal("a combination holding only abandoned temporaries was listed; prune sweeps it silently")
	}
}

func TestListReportsIncompleteLegacyLayoutOnce(t *testing.T) {
	root := t.TempDir()
	cache := New(root)
	writeCacheFile(t, filepath.Join(root, "schematic", "v1.13.6", "metal-arm64.raw.xz"), "legacy-archive")
	writeCacheFile(t, filepath.Join(root, "schematic", "v1.13.6", "arm64", "metal-arm64.raw.xz"), "arch-archive")

	entries, err := cache.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want the legacy and arch layouts merged into one arm64 row", entries)
	}
	if want := int64(len("legacy-archive") + len("arch-archive")); entries[0].Size != want {
		t.Fatalf("merged size = %d, want %d", entries[0].Size, want)
	}
}

// The runbook treats `cache list` as the prune preview, so nothing may be
// deleted that the preceding listing did not show.
func TestUnscopedPruneOnlyRemovesListedCombinations(t *testing.T) {
	root := t.TempDir()
	cache := New(root)
	writeCacheFile(t, filepath.Join(root, "keeper", "v1.13.6", "amd64", "disk.raw"), "kept-disk")
	writeCacheFile(t, filepath.Join(root, "orphan", "v1.13.6", "amd64", "disk.raw"), "orphan-disk")
	writeCacheFile(t, filepath.Join(root, "orphan-archive", "v1.13.6", "arm64", "metal-arm64.raw.xz"), "orphan-archive")
	writeCacheFile(t, filepath.Join(root, "orphan-empty", "v1.13.6", "amd64", "disk.raw"), "")
	writeCacheFile(t, filepath.Join(root, "orphan-legacy", "v1.13.6", "metal-arm64.raw.xz"), "legacy-archive")

	before := listedCombinations(t, cache)
	result, err := cache.PruneDiskExcept(func(combination Combination) (bool, error) {
		return combination.Schematic == "keeper", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Images) != 4 {
		t.Fatalf("pruned combinations = %+v, want the four unreferenced ones", result.Images)
	}
	for _, pruned := range result.Images {
		if _, ok := before[pruned.Combination]; !ok {
			t.Fatalf("prune removed %+v which cache list never showed: %+v", pruned.Combination, before)
		}
	}
	after := listedCombinations(t, cache)
	keeper := Combination{Schematic: "keeper", Version: "v1.13.6", Architecture: ArchitectureAMD64}
	if len(after) != 1 {
		t.Fatalf("listing after prune = %+v, want only the kept combination", after)
	}
	if _, ok := after[keeper]; !ok {
		t.Fatalf("kept combination missing from listing after prune: %+v", after)
	}
}

func TestPruneCountsEveryReportedCombinationAsAnImage(t *testing.T) {
	root := t.TempDir()
	cache := New(root)
	writeCacheFile(t, filepath.Join(root, "orphan", "v1.13.6", "amd64", "metal-amd64.raw.xz"), "orphan-archive")

	result, err := cache.PruneDiskExcept(func(Combination) (bool, error) { return false, nil })
	if err != nil {
		t.Fatal(err)
	}
	if result.ImageCount != len(result.Images) {
		t.Fatalf("ImageCount = %d, want it to agree with the %d reported combination(s)", result.ImageCount, len(result.Images))
	}
	if result.ImageCount != 1 {
		t.Fatalf("ImageCount = %d, want the orphan combination counted", result.ImageCount)
	}
	if want := int64(len("orphan-archive")); result.ImageBytes != want {
		t.Fatalf("ImageBytes = %d, want %d", result.ImageBytes, want)
	}
}

func TestPruneKeptCountMatchesTheSurvivingListing(t *testing.T) {
	root := t.TempDir()
	cache := New(root)
	writeCacheFile(t, filepath.Join(root, "keeper", "v1.13.6", "amd64", "disk.raw"), "kept-disk")
	writeCacheFile(t, filepath.Join(root, "keeper", "v1.13.6", "arm64", "metal-arm64.raw.xz"), "kept-archive")

	result, err := cache.PruneDiskExcept(func(Combination) (bool, error) { return true, nil })
	if err != nil {
		t.Fatal(err)
	}
	after := listedCombinations(t, cache)
	if result.KeptImages != len(after) {
		t.Fatalf("KeptImages = %d, want the %d combination(s) cache list still shows", result.KeptImages, len(after))
	}
}
