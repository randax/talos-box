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

	"gopkg.in/yaml.v3"

	"github.com/randax/talos-box/internal/extensions"
)

// schematicDefinitionLimit caps what a schematic fetch is allowed to read; a
// customization is a few hundred bytes, so anything larger is not one.
const schematicDefinitionLimit = 256 << 10

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

// ComposeSchematic returns the schematic id carrying the curated extensions
// requested for talosVersion. With no base it composes talosbox's own
// customization; with one it re-composes the brought schematic's definition.
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
	var body []byte
	if base != "" {
		if body, err = c.recomposedRequestBody(base, talosVersion, requested, refs); err != nil {
			return "", err
		}
	} else {
		if err := c.checkExtensionAvailability(talosVersion, refs); err != nil {
			return "", err
		}
		if body, err = schematicRequestBody(refs, nil); err != nil {
			return "", err
		}
	}
	id, err := c.postSchematic(body)
	if err != nil {
		if base != "" {
			return "", factoryAccessError(err, base, talosVersion, requested)
		}
		return "", err
	}
	if err := c.RecordComposition(base, talosVersion, requested, id); err != nil {
		return "", err
	}
	return id, nil
}

// recomposedRequestBody merges the requested extensions into the definition of
// a brought schematic. The brought schematic stays sovereign: its own kernel
// arguments, extensions, and omissions are carried over untouched and nothing
// talosbox composes by default is injected.
func (c *Cache) recomposedRequestBody(base, talosVersion string, requested, refs []string) ([]byte, error) {
	definition, err := c.schematicDefinition(base)
	if err != nil {
		return nil, factoryAccessError(err, base, talosVersion, requested)
	}
	if err := c.checkExtensionAvailability(talosVersion, refs); err != nil {
		return nil, err
	}
	body, err := mergeSchematicExtensions(definition, refs)
	if err != nil {
		return nil, fmt.Errorf("recompose schematic %s: %w", base, err)
	}
	return body, nil
}

// schematicDefinition fetches a schematic's customization from the Factory.
// Nothing local can reconstruct it, which is why this is the one part of
// composition that cannot be answered offline.
func (c *Cache) schematicDefinition(id string) ([]byte, error) {
	if err := validateComponent("schematic", id); err != nil {
		return nil, err
	}
	response, err := c.schematicClient.Get(strings.TrimRight(c.factoryURL, "/") + "/schematics/" + url.PathEscape(id))
	if err != nil {
		return nil, fmt.Errorf("fetch schematic %s: %w", id, err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		return nil, fmt.Errorf("fetch schematic %s: %s: %s", id, response.Status, strings.TrimSpace(string(message)))
	}
	definition, err := io.ReadAll(io.LimitReader(response.Body, schematicDefinitionLimit))
	if err != nil {
		return nil, fmt.Errorf("read schematic %s: %w", id, err)
	}
	return definition, nil
}

// mergeSchematicExtensions adds refs to the schematic's officialExtensions and
// re-encodes the whole document, so fields talosbox knows nothing about survive
// the round trip. The merged list keeps the schematic's own order and appends
// only what is missing, which makes the composed id a function of the inputs.
func mergeSchematicExtensions(definition []byte, refs []string) ([]byte, error) {
	document := map[string]any{}
	if err := yaml.Unmarshal(definition, &document); err != nil {
		return nil, fmt.Errorf("decode schematic definition: %w", err)
	}
	customization, err := childMap(document, "customization")
	if err != nil {
		return nil, err
	}
	systemExtensions, err := childMap(customization, "systemExtensions")
	if err != nil {
		return nil, err
	}
	existing, err := stringList(systemExtensions["officialExtensions"])
	if err != nil {
		return nil, fmt.Errorf("customization.systemExtensions.officialExtensions: %w", err)
	}
	seen := make(map[string]struct{}, len(existing)+len(refs))
	merged := make([]string, 0, len(existing)+len(refs))
	for _, ref := range append(existing, refs...) {
		if _, duplicate := seen[ref]; duplicate {
			continue
		}
		seen[ref] = struct{}{}
		merged = append(merged, ref)
	}
	systemExtensions["officialExtensions"] = merged
	customization["systemExtensions"] = systemExtensions
	document["customization"] = customization

	body, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode schematic request: %w", err)
	}
	return body, nil
}

func childMap(parent map[string]any, key string) (map[string]any, error) {
	switch value := parent[key].(type) {
	case nil:
		return map[string]any{}, nil
	case map[string]any:
		return value, nil
	default:
		return nil, fmt.Errorf("schematic field %q is not a mapping", key)
	}
}

func stringList(value any) ([]string, error) {
	switch typed := value.(type) {
	case nil:
		return nil, nil
	case []any:
		list := make([]string, 0, len(typed))
		for _, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil, errors.New("expected a list of strings")
			}
			list = append(list, text)
		}
		return list, nil
	default:
		return nil, errors.New("expected a list of strings")
	}
}

// factoryAccessError explains the one combination that cannot be resolved from
// the cache alone, and points at the command that makes it cacheable.
func factoryAccessError(err error, base, talosVersion string, requested []string) error {
	return fmt.Errorf("%w; extensions %s on schematic %s need Image Factory access to compose for Talos %s — run `tbx cache pull` while online first, after which this combination resolves from cache",
		err, strings.Join(canonicalExtensions(requested), ", "), base, talosVersion)
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
