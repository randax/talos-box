package imagecache

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const catalogJSON = `[
	{"name":"siderolabs/gvisor","ref":"ghcr.io/siderolabs/gvisor:1","description":"Sandboxed container runtime.\n"},
	{"name":"siderolabs/iscsi-tools","ref":"ghcr.io/siderolabs/iscsi-tools:1","description":"iSCSI initiator.\n"},
	{"name":"siderolabs/nfs-utils","ref":"ghcr.io/siderolabs/nfs-utils:1","description":"NFSv3 client with locking.\n"},
	{"name":"siderolabs/util-linux-tools","ref":"ghcr.io/siderolabs/util-linux-tools:1","description":"Core Linux utilities.\n"}
]`

// factoryFake serves the Image Factory endpoints composition needs; every
// other request fails the test, so an unexpected call is never silent.
type factoryFake struct {
	server        *httptest.Server
	schematicID   string
	catalog       string
	requestedPost string
	requests      []string
}

func newFactoryFake(t *testing.T, schematicID, catalog string) *factoryFake {
	t.Helper()

	fake := &factoryFake{schematicID: schematicID, catalog: catalog}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.requests = append(fake.requests, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/schematics":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read request body: %v", err)
			}
			fake.requestedPost = string(body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"id":%q}`, fake.schematicID)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/extensions/official"):
			if fake.catalog == "" {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, fake.catalog)
		default:
			t.Errorf("unexpected request = %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusBadRequest)
		}
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *factoryFake) attach(cache *Cache) {
	cache.factoryURL = f.server.URL
	cache.schematicClient = f.server.Client()
	cache.downloadClient = f.server.Client()
}

// offlineCache returns a cache whose Factory calls all fail the test: it
// proves a code path stayed offline.
func offlineCache(t *testing.T, root string) *Cache {
	t.Helper()

	cache := New(root)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected Factory request = %s %s", r.Method, r.URL.Path)
		http.Error(w, "offline", http.StatusBadRequest)
	}))
	t.Cleanup(server.Close)
	cache.factoryURL = server.URL
	cache.schematicClient = server.Client()
	cache.downloadClient = server.Client()
	return cache
}

// unreachableCache returns a cache pointed at a closed local listener, so any
// Factory call fails instead of reaching the network.
func unreachableCache(t *testing.T, root string) *Cache {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	address := server.URL
	server.Close()
	cache := New(root)
	cache.factoryURL = address
	return cache
}

func TestComposeSchematicAddsRequestedExtensionsToAlwaysOnSet(t *testing.T) {
	fake := newFactoryFake(t, "composed-id", catalogJSON)
	cache := New(t.TempDir())
	fake.attach(cache)

	// The repeat must collapse and the order must not matter: the composed
	// id is content-addressed, so the request body has to be canonical.
	id, err := cache.ComposeSchematic("", "v1.13.6", []string{"nfs-utils", "gvisor", "nfs-utils"})
	if err != nil {
		t.Fatalf("ComposeSchematic() error = %v", err)
	}
	if id != "composed-id" {
		t.Fatalf("ComposeSchematic() = %q, want %q", id, "composed-id")
	}
	want := `{"customization":{"extraKernelArgs":["console=tty0","console=hvc0"],"systemExtensions":{"officialExtensions":["siderolabs/gvisor","siderolabs/iscsi-tools","siderolabs/nfs-utils","siderolabs/util-linux-tools"]}}}`
	if fake.requestedPost != want {
		t.Fatalf("schematic request = %s, want %s", fake.requestedPost, want)
	}
	wantCatalog := "GET /version/v1.13.6/extensions/official"
	if len(fake.requests) != 2 || fake.requests[0] != wantCatalog {
		t.Fatalf("factory requests = %v, want %v then POST /schematics", fake.requests, wantCatalog)
	}
}

func TestComposeSchematicRejectsUnknownNameOffline(t *testing.T) {
	cache := offlineCache(t, t.TempDir())

	_, err := cache.ComposeSchematic("", "v1.13.6", []string{"gvisr"})
	if err == nil {
		t.Fatal("ComposeSchematic() accepted an unknown extension")
	}
	for _, fragment := range []string{`unknown extension "gvisr"`, `did you mean "gvisor"`} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("ComposeSchematic() error = %q, want it to contain %q", err, fragment)
		}
	}
}

func TestComposeSchematicRejectsExtensionMissingFromVersionCatalog(t *testing.T) {
	const withoutGvisor = `[
		{"name":"siderolabs/nfs-utils","ref":"ghcr.io/siderolabs/nfs-utils:1","description":"NFSv3 client with locking.\nSecond line."}
	]`
	fake := newFactoryFake(t, "composed-id", withoutGvisor)
	cache := New(t.TempDir())
	fake.attach(cache)

	_, err := cache.ComposeSchematic("", "v1.12.0", []string{"gvisor"})
	if err == nil {
		t.Fatal("ComposeSchematic() accepted an extension missing from the catalog")
	}
	for _, fragment := range []string{
		`"siderolabs/gvisor"`,
		"v1.12.0",
		"nfs-utils (siderolabs/nfs-utils): NFSv3 client with locking.",
	} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("ComposeSchematic() error = %q, want it to contain %q", err, fragment)
		}
	}
	for _, request := range fake.requests {
		if strings.HasPrefix(request, http.MethodPost) {
			t.Fatalf("factory requests = %v, want no schematic POST", fake.requests)
		}
	}
}

func TestComposeSchematicReusesRecordedComposition(t *testing.T) {
	root := t.TempDir()
	fake := newFactoryFake(t, "composed-id", catalogJSON)
	cache := New(root)
	fake.attach(cache)

	if _, err := cache.ComposeSchematic("", "v1.13.6", []string{"gvisor"}); err != nil {
		t.Fatalf("ComposeSchematic() error = %v", err)
	}

	// A recorded composition is what makes an offline create possible: the
	// second resolution must neither validate nor call the Factory.
	offline := offlineCache(t, root)
	id, err := offline.ComposeSchematic("", "v1.13.6", []string{"gvisor"})
	if err != nil {
		t.Fatalf("ComposeSchematic() error = %v", err)
	}
	if id != "composed-id" {
		t.Fatalf("ComposeSchematic() = %q, want %q", id, "composed-id")
	}

	// Different inputs are a different composition and must not reuse it:
	// with no Factory reachable they have to fail rather than resolve.
	unreachable := unreachableCache(t, root)
	if _, err := unreachable.ComposeSchematic("", "v1.13.6", []string{"nfs-utils"}); err == nil {
		t.Fatal("ComposeSchematic() reused the recorded id for different extensions")
	}
	if _, err := unreachable.ComposeSchematic("", "v1.14.0", []string{"gvisor"}); err == nil {
		t.Fatal("ComposeSchematic() reused the recorded id for a different Talos version")
	}
}

func TestCompositionRecordRoundTrip(t *testing.T) {
	cache := New(t.TempDir())

	if _, found, err := cache.CompositionID("", "v1.13.6", []string{"gvisor"}); err != nil || found {
		t.Fatalf("CompositionID() = (found %t, %v), want no record", found, err)
	}
	if err := cache.RecordComposition("", "v1.13.6", []string{"gvisor"}, "composed-id"); err != nil {
		t.Fatalf("RecordComposition() error = %v", err)
	}
	// Spelling of the request must not matter: the record is keyed by the
	// canonical composition, matching the content-addressed id.
	id, found, err := cache.CompositionID("", "v1.13.6", []string{"gvisor", "gvisor"})
	if err != nil || !found || id != "composed-id" {
		t.Fatalf("CompositionID() = (%q, %t, %v), want (\"composed-id\", true, nil)", id, found, err)
	}
}

// TestComposeSchematicKeepsCustomSchematicSovereign records the interim
// contract for a brought schematic: it is used as-is (re-composition is not
// implemented yet), but the requested names are still validated locally.
func TestComposeSchematicKeepsCustomSchematicSovereign(t *testing.T) {
	cache := offlineCache(t, t.TempDir())

	id, err := cache.ComposeSchematic("aaa111", "v1.13.6", []string{"gvisor"})
	if err != nil {
		t.Fatalf("ComposeSchematic() error = %v", err)
	}
	if id != "aaa111" {
		t.Fatalf("ComposeSchematic() = %q, want %q", id, "aaa111")
	}
	if _, err := cache.ComposeSchematic("aaa111", "v1.13.6", []string{"gvisr"}); err == nil {
		t.Fatal("ComposeSchematic() accepted an unknown extension on a custom schematic")
	}
}
