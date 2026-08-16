package imagecache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// contentAddressedFactory mimics the Factory closely enough for a pull: the
// schematic id is a hash of the posted body, so composing the same inputs
// twice yields the same id, and any schematic id can be downloaded.
func contentAddressedFactory(t *testing.T, definitions map[string]string, archive []byte) (*httptest.Server, *int) {
	t.Helper()

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/schematics":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read request body: %v", err)
			}
			sum := sha256.Sum256(body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"id":%q}`, hex.EncodeToString(sum[:]))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/schematics/"):
			definition, known := definitions[strings.TrimPrefix(r.URL.Path, "/schematics/")]
			if !known {
				http.Error(w, "schematic not found", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/yaml")
			_, _ = io.WriteString(w, definition)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/extensions/official"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, catalogJSON)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/image/"):
			w.Header().Set("Content-Type", "application/x-xz")
			_, _ = w.Write(archive)
		default:
			t.Errorf("unexpected request = %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusBadRequest)
		}
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

// TestPulledCombinationsResolveOfflineAfterRestart is the contract a
// file-aware pull sells: everything the Factory answered once — the default
// schematic, the composed ids, the images themselves — is on disk afterwards,
// so a later create resolves the same combinations without a single request.
func TestPulledCombinationsResolveOfflineAfterRestart(t *testing.T) {
	t.Parallel()

	requireXZ(t)

	const (
		version = "v1.13.6"
		brought = "brought-schematic-id"
	)
	definitions := map[string]string{
		brought: "customization:\n    extraKernelArgs:\n        - console=ttyS0\n    systemExtensions:\n        officialExtensions:\n            - siderolabs/tailscale\n",
	}
	upstream, requests := contentAddressedFactory(t, definitions, compressXZ(t, "disk"))

	root := t.TempDir()
	online := New(root)
	online.factoryURL = upstream.URL
	online.schematicClient = upstream.Client()
	online.downloadClient = upstream.Client()

	defaultSchematic, err := online.DefaultSchematic()
	if err != nil {
		t.Fatalf("DefaultSchematic() error = %v", err)
	}
	curated, err := online.ComposeSchematic("", version, []string{"gvisor"})
	if err != nil {
		t.Fatalf("ComposeSchematic(curated) error = %v", err)
	}
	recomposed, err := online.ComposeSchematic(brought, version, []string{"nfs-utils"})
	if err != nil {
		t.Fatalf("ComposeSchematic(recomposed) error = %v", err)
	}
	combinations := []string{defaultSchematic, curated, recomposed}
	for _, schematic := range combinations {
		if _, err := online.Ensure(schematic, version, ArchitectureARM64); err != nil {
			t.Fatalf("Ensure(%s) error = %v", schematic, err)
		}
		if err := online.Pin(schematic, version, ArchitectureARM64); err != nil {
			t.Fatalf("Pin(%s) error = %v", schematic, err)
		}
	}
	if *requests == 0 {
		t.Fatal("the online pull made no Factory requests at all")
	}

	// A fresh cache over the same root is a restarted daemon; its Factory is
	// wired to fail the test, so any request below is a bug.
	offline := offlineCache(t, root)
	if got, err := offline.DefaultSchematic(); err != nil || got != defaultSchematic {
		t.Fatalf("offline DefaultSchematic() = (%q, %v), want %q", got, err, defaultSchematic)
	}
	if got, err := offline.ComposeSchematic("", version, []string{"gvisor"}); err != nil || got != curated {
		t.Fatalf("offline ComposeSchematic(curated) = (%q, %v), want %q", got, err, curated)
	}
	if got, err := offline.ComposeSchematic(brought, version, []string{"nfs-utils"}); err != nil || got != recomposed {
		t.Fatalf("offline ComposeSchematic(recomposed) = (%q, %v), want %q", got, err, recomposed)
	}
	for _, schematic := range combinations {
		if _, err := offline.Ensure(schematic, version, ArchitectureARM64); err != nil {
			t.Fatalf("offline Ensure(%s) error = %v", schematic, err)
		}
		pinned, err := offline.Pinned(schematic, version, ArchitectureARM64)
		if err != nil {
			t.Fatalf("offline Pinned(%s) error = %v", schematic, err)
		}
		if !pinned {
			t.Fatalf("offline Pinned(%s) = false, want a surviving pin", schematic)
		}
	}
}

// TestDefaultSchematicIsComposedOnlyOnce keeps the default combination on the
// same offline footing as a composed one: the id is recorded on first use.
func TestDefaultSchematicIsComposedOnlyOnce(t *testing.T) {
	t.Parallel()

	upstream, requests := contentAddressedFactory(t, nil, nil)

	root := t.TempDir()
	online := New(root)
	online.factoryURL = upstream.URL
	online.schematicClient = upstream.Client()

	first, err := online.DefaultSchematic()
	if err != nil {
		t.Fatalf("DefaultSchematic() error = %v", err)
	}
	second, err := New(root).DefaultSchematic()
	if err != nil {
		t.Fatalf("DefaultSchematic() on a fresh cache error = %v", err)
	}
	if second != first {
		t.Fatalf("DefaultSchematic() = %q, want the recorded %q", second, first)
	}
	if *requests != 1 {
		t.Fatalf("Factory requests = %d, want 1", *requests)
	}
}
