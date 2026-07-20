package imagecache

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const factoryURL = "https://factory.talos.dev"

// Cache stores Talos disk images by schematic and version.
type Cache struct {
	root       string
	factoryURL string
	httpClient *http.Client
}

// Entry is a ready-to-use disk image in the cache.
type Entry struct {
	Schematic string
	Version   string
	Path      string
	Size      int64
}

// New returns a cache rooted at root.
func New(root string) *Cache {
	return &Cache{
		root:       root,
		factoryURL: factoryURL,
		httpClient: http.DefaultClient,
	}
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

// Ensure returns a decompressed disk image, downloading it when necessary.
func (c *Cache) Ensure(schematic, version string) (string, error) {
	if err := validateComponent("schematic", schematic); err != nil {
		return "", err
	}
	if err := validateComponent("version", version); err != nil {
		return "", err
	}

	dir := filepath.Join(c.root, schematic, version)
	diskPath := filepath.Join(dir, "disk.raw")
	if fileReady(diskPath) {
		return diskPath, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create cache directory: %w", err)
	}

	archivePath := filepath.Join(dir, "metal-arm64.raw.xz")
	if !fileReady(archivePath) {
		assetURL := fmt.Sprintf("%s/image/%s/%s/metal-arm64.raw.xz",
			strings.TrimRight(c.factoryURL, "/"), url.PathEscape(schematic), url.PathEscape(version))
		if err := c.download(assetURL, archivePath); err != nil {
			return "", err
		}
	}
	if err := decompress(archivePath, diskPath); err != nil {
		return "", err
	}

	return diskPath, nil
}

// List returns the complete disk images currently in the cache.
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
			path := filepath.Join(c.root, schematic.Name(), version.Name(), "disk.raw")
			info, err := os.Stat(path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("stat cached image %q: %w", path, err)
			}
			if !info.Mode().IsRegular() || info.Size() == 0 {
				continue
			}
			entries = append(entries, Entry{
				Schematic: schematic.Name(),
				Version:   version.Name(),
				Path:      path,
				Size:      info.Size(),
			})
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Schematic == entries[j].Schematic {
			return entries[i].Version < entries[j].Version
		}
		return entries[i].Schematic < entries[j].Schematic
	})

	return entries, nil
}

// Prune removes every cache entry.
func (c *Cache) Prune() error {
	if c.root == "" || filepath.Clean(c.root) == string(filepath.Separator) {
		return errors.New("refusing to prune an empty or root cache path")
	}
	if err := os.RemoveAll(c.root); err != nil {
		return fmt.Errorf("prune cache: %w", err)
	}
	return nil
}

func (c *Cache) download(sourceURL, destination string) error {
	response, err := c.httpClient.Get(sourceURL)
	if err != nil {
		return fmt.Errorf("download image: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("download image: %s", response.Status)
	}

	temporary, err := os.CreateTemp(filepath.Dir(destination), ".metal-arm64.raw.xz-*")
	if err != nil {
		return fmt.Errorf("create image download: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()

	if _, err := io.Copy(temporary, response.Body); err != nil {
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

func validateComponent(name, value string) error {
	if value == "" || value == "." || value == ".." || filepath.Base(value) != value {
		return fmt.Errorf("invalid %s %q", name, value)
	}
	return nil
}
