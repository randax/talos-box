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
			versionDir := filepath.Join(c.root, schematic.Name(), version.Name())
			for _, architecture := range []Architecture{ArchitectureAMD64, ArchitectureARM64} {
				path := filepath.Join(versionDir, string(architecture), "disk.raw")
				entry, ok, err := cacheEntry(schematic.Name(), version.Name(), architecture, path)
				if err != nil {
					return nil, err
				}
				if ok {
					entries = append(entries, entry)
				}
			}
			if fileReady(filepath.Join(versionDir, string(ArchitectureARM64), "disk.raw")) {
				continue
			}
			legacyPath := filepath.Join(versionDir, "disk.raw")
			entry, ok, err := cacheEntry(schematic.Name(), version.Name(), ArchitectureARM64, legacyPath)
			if err != nil {
				return nil, err
			}
			if ok {
				entries = append(entries, entry)
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
		Schematic:    schematic,
		Version:      version,
		Architecture: architecture,
		Path:         path,
		Size:         info.Size(),
	}, true, nil
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
