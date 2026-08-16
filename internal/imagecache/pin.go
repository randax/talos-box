package imagecache

import (
	"fmt"
	"os"
	"path/filepath"
)

// pinMarkerName marks a disk-image combination as explicitly wanted. It sits
// in the image's own directory so the pin outlives the daemon that set it, and
// it is empty: its existence is the whole record.
const pinMarkerName = "pinned"

// Pin marks a combination as explicitly pulled. Pinning is independent of the
// image being present, so a pull pins what it fetched even when the image was
// already cached.
func (c *Cache) Pin(schematic, version string, architecture Architecture) error {
	path, err := c.pinMarkerPath(schematic, version, architecture)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create cache directory: %w", err)
	}
	marker, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("pin cached image: %w", err)
	}
	if err := marker.Close(); err != nil {
		return fmt.Errorf("pin cached image: %w", err)
	}
	return nil
}

// Pinned reports whether a combination carries a pin marker.
func (c *Cache) Pinned(schematic, version string, architecture Architecture) (bool, error) {
	path, err := c.pinMarkerPath(schematic, version, architecture)
	if err != nil {
		return false, err
	}
	info, exists, err := lstatPath(path)
	if err != nil || !exists {
		return false, err
	}
	return info.Mode().IsRegular(), nil
}

func (c *Cache) pinMarkerPath(schematic, version string, architecture Architecture) (string, error) {
	if err := validateComponent("schematic", schematic); err != nil {
		return "", err
	}
	if err := validateComponent("version", version); err != nil {
		return "", err
	}
	if err := validateArchitecture(architecture); err != nil {
		return "", err
	}
	return filepath.Join(c.root, schematic, version, string(architecture), pinMarkerName), nil
}
