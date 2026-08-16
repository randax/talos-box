package imagecache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/randax/talos-box/internal/extensions"
)

// compositionDirName holds the composed-schematic records, beside the disk
// images. Its name is not a 64-hex schematic id, so it can never collide with
// an image directory, and prune only ever removes known image artifacts.
const compositionDirName = "schematics"

// ExtensionCatalogEntry is one entry of the Image Factory's official
// extension catalog for a Talos version.
type ExtensionCatalogEntry struct {
	Name        string `json:"name"`
	Ref         string `json:"ref"`
	Author      string `json:"author"`
	Description string `json:"description"`
}

type compositionRecord struct {
	Base       string   `json:"base,omitempty"`
	Version    string   `json:"talosVersion"`
	Extensions []string `json:"extensions"`
	ID         string   `json:"id"`
}

// ComposeSchematic returns the schematic id for talosbox's base customization
// plus the curated extensions requested for talosVersion.
//
// A composition already recorded on disk is reused verbatim: that record is
// what makes an offline create from a cached composed image possible, so
// neither validation nor any Factory request may happen on that path.
// Otherwise the names are validated locally, checked against the Factory's
// catalog for the version, and composed with one POST.
func (c *Cache) ComposeSchematic(base, talosVersion string, requested []string) (string, error) {
	if len(requested) == 0 {
		return "", errors.New("compose schematic: no extensions requested")
	}
	if id, found, err := c.CompositionID(base, talosVersion, requested); err != nil {
		return "", err
	} else if found {
		return id, nil
	}
	refs, err := extensions.Resolve(requested)
	if err != nil {
		return "", err
	}
	if base != "" {
		// Merging extensions into a brought schematic means re-composing it
		// through the Factory's schematic API; until that lands the brought
		// schematic stays sovereign and only the names are validated.
		return base, nil
	}
	if err := c.checkExtensionAvailability(talosVersion, refs); err != nil {
		return "", err
	}
	body, err := schematicRequestBody(refs, nil)
	if err != nil {
		return "", err
	}
	id, err := c.postSchematic(body)
	if err != nil {
		return "", err
	}
	if err := c.RecordComposition(base, talosVersion, requested, id); err != nil {
		return "", err
	}
	return id, nil
}

// CompositionID returns the recorded schematic id for a composition, if any.
func (c *Cache) CompositionID(base, talosVersion string, requested []string) (string, bool, error) {
	data, err := os.ReadFile(c.compositionPath(base, talosVersion, requested))
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read composed schematic record: %w", err)
	}
	var record compositionRecord
	if err := json.Unmarshal(data, &record); err != nil || record.ID == "" {
		return "", false, nil
	}
	return record.ID, true, nil
}

// RecordComposition remembers the schematic id a composition resolved to.
func (c *Cache) RecordComposition(base, talosVersion string, requested []string, id string) error {
	path := c.compositionPath(base, talosVersion, requested)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create composed schematic directory: %w", err)
	}
	data, err := json.Marshal(compositionRecord{
		Base: base, Version: talosVersion, Extensions: canonicalExtensions(requested), ID: id,
	})
	if err != nil {
		return fmt.Errorf("encode composed schematic record: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".composition-")
	if err != nil {
		return fmt.Errorf("create composed schematic record: %w", err)
	}
	defer func() { _ = os.Remove(temporary.Name()) }()
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write composed schematic record: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("write composed schematic record: %w", err)
	}
	if err := os.Rename(temporary.Name(), path); err != nil {
		return fmt.Errorf("publish composed schematic record: %w", err)
	}
	return nil
}

// OfficialExtensions returns the official extension catalog the Image Factory
// publishes for talosVersion.
func (c *Cache) OfficialExtensions(talosVersion string) ([]ExtensionCatalogEntry, error) {
	catalogURL := fmt.Sprintf("%s/version/%s/extensions/official",
		strings.TrimRight(c.factoryURL, "/"), url.PathEscape(talosVersion))
	response, err := c.schematicClient.Get(catalogURL)
	if err != nil {
		return nil, fmt.Errorf("fetch extension catalog: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		return nil, fmt.Errorf("fetch extension catalog for Talos %s: %s: %s",
			talosVersion, response.Status, strings.TrimSpace(string(message)))
	}
	var catalog []ExtensionCatalogEntry
	if err := json.NewDecoder(response.Body).Decode(&catalog); err != nil {
		return nil, fmt.Errorf("decode extension catalog: %w", err)
	}
	return catalog, nil
}

func (c *Cache) checkExtensionAvailability(talosVersion string, refs []string) error {
	catalog, err := c.OfficialExtensions(talosVersion)
	if err != nil {
		return err
	}
	available := make(map[string]ExtensionCatalogEntry, len(catalog))
	for _, entry := range catalog {
		available[entry.Name] = entry
	}
	var missing []string
	for _, ref := range refs {
		if _, ok := available[ref]; !ok {
			missing = append(missing, fmt.Sprintf("%q", ref))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("extension %s not available for Talos %s; %s",
		strings.Join(missing, ", "), talosVersion, curatedCatalogSummary(available))
}

// curatedCatalogSummary describes the curated extensions the Factory does
// publish for the version, so the error carries the catalog's own words.
func curatedCatalogSummary(available map[string]ExtensionCatalogEntry) string {
	var lines []string
	for _, name := range extensions.Names() {
		ref, _ := extensions.Ref(name)
		entry, ok := available[ref]
		if !ok {
			continue
		}
		line := fmt.Sprintf("%s (%s)", name, ref)
		if description := firstLine(entry.Description); description != "" {
			line += ": " + description
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return "no curated extensions are available for this version"
	}
	return "curated extensions available for this version: " + strings.Join(lines, "; ")
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return strings.TrimSpace(line)
}

func (c *Cache) compositionPath(base, talosVersion string, requested []string) string {
	key := strings.Join(append([]string{base, talosVersion}, canonicalExtensions(requested)...), "\n")
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(c.root, compositionDirName, hex.EncodeToString(sum[:])+".json")
}

// canonicalExtensions is the spelling-independent form of a request: the
// composed id is content-addressed, so order and repeats must not key a
// separate record.
func canonicalExtensions(requested []string) []string {
	seen := make(map[string]struct{}, len(requested))
	canonical := make([]string, 0, len(requested))
	for _, name := range requested {
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		canonical = append(canonical, name)
	}
	sort.Strings(canonical)
	return canonical
}
