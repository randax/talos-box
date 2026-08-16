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
	server      *httptest.Server
	schematicID string
	catalog     string
	// definitions are the schematics the fake knows by id; anything else is
	// unknown to it, like an id the Factory never issued.
	definitions   map[string]string
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
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/schematics/"):
			definition, known := fake.definitions[strings.TrimPrefix(r.URL.Path, "/schematics/")]
			if !known {
				http.Error(w, "schematic not found", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/yaml")
			_, _ = io.WriteString(w, definition)
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

// TestComposeSchematicRecomposesBroughtSchematic pins the merge semantics: a
// brought schematic stays sovereign, so only the requested extensions are added
// to whatever it already declares — no kernel args and no always-on storage
// extensions are injected, and its own omissions survive.
func TestComposeSchematicRecomposesBroughtSchematic(t *testing.T) {
	tests := []struct {
		name       string
		definition string
		requested  []string
		want       string
	}{
		{
			name: "merges into the brought extension list",
			definition: "customization:\n" +
				"    extraKernelArgs:\n" +
				"        - console=ttyS0\n" +
				"    systemExtensions:\n" +
				"        officialExtensions:\n" +
				"            - siderolabs/intel-ucode\n",
			requested: []string{"gvisor"},
			want:      `{"customization":{"extraKernelArgs":["console=ttyS0"],"systemExtensions":{"officialExtensions":["siderolabs/intel-ucode","siderolabs/gvisor"]}}}`,
		},
		{
			name: "adds nothing else to a schematic without extensions",
			definition: "customization:\n" +
				"    extraKernelArgs:\n" +
				"        - console=ttyS0\n" +
				"overlay:\n" +
				"    name: rpi_generic\n" +
				"    image: siderolabs/sbc-raspberrypi\n",
			requested: []string{"gvisor", "nfs-utils"},
			want:      `{"customization":{"extraKernelArgs":["console=ttyS0"],"systemExtensions":{"officialExtensions":["siderolabs/gvisor","siderolabs/nfs-utils"]}},"overlay":{"image":"siderolabs/sbc-raspberrypi","name":"rpi_generic"}}`,
		},
		{
			name: "keeps an extension the brought schematic already declares",
			definition: "customization:\n" +
				"    systemExtensions:\n" +
				"        officialExtensions:\n" +
				"            - siderolabs/gvisor\n",
			requested: []string{"gvisor"},
			want:      `{"customization":{"systemExtensions":{"officialExtensions":["siderolabs/gvisor"]}}}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newFactoryFake(t, "composed-id", catalogJSON)
			fake.definitions = map[string]string{"brought": test.definition}
			cache := New(t.TempDir())
			fake.attach(cache)

			id, err := cache.ComposeSchematic("brought", "v1.13.6", test.requested)
			if err != nil {
				t.Fatalf("ComposeSchematic() error = %v", err)
			}
			if id != "composed-id" {
				t.Fatalf("ComposeSchematic() = %q, want %q", id, "composed-id")
			}
			if fake.requestedPost != test.want {
				t.Fatalf("schematic request = %s, want %s", fake.requestedPost, test.want)
			}
			if want := "GET /schematics/brought"; len(fake.requests) == 0 || fake.requests[0] != want {
				t.Fatalf("factory requests = %v, want %s first", fake.requests, want)
			}
		})
	}
}

// TestComposeSchematicRecompositionIsDeterministic guards the content-addressed
// contract: the same inputs must produce the same request, and therefore the
// same composed id, from any cache.
func TestComposeSchematicRecompositionIsDeterministic(t *testing.T) {
	const definition = "customization:\n" +
		"    systemExtensions:\n" +
		"        officialExtensions:\n" +
		"            - siderolabs/intel-ucode\n"
	fake := newFactoryFake(t, "composed-id", catalogJSON)
	fake.definitions = map[string]string{"brought": definition}

	compose := func(requested []string) string {
		t.Helper()

		cache := New(t.TempDir())
		fake.attach(cache)
		if _, err := cache.ComposeSchematic("brought", "v1.13.6", requested); err != nil {
			t.Fatalf("ComposeSchematic() error = %v", err)
		}
		return fake.requestedPost
	}

	first := compose([]string{"gvisor", "nfs-utils"})
	second := compose([]string{"nfs-utils", "gvisor", "gvisor"})
	if first != second {
		t.Fatalf("schematic request = %s, want %s", second, first)
	}
}

func TestComposeSchematicFailsWhenBroughtSchematicCannotBeFetched(t *testing.T) {
	t.Run("factory unreachable", func(t *testing.T) {
		cache := unreachableCache(t, t.TempDir())

		_, err := cache.ComposeSchematic("brought", "v1.13.6", []string{"gvisor"})
		if err == nil {
			t.Fatal("ComposeSchematic() composed without Factory access")
		}
		for _, fragment := range []string{"brought", "Image Factory access", "tbx cache pull"} {
			if !strings.Contains(err.Error(), fragment) {
				t.Fatalf("ComposeSchematic() error = %q, want it to contain %q", err, fragment)
			}
		}
	})

	t.Run("unknown schematic id", func(t *testing.T) {
		fake := newFactoryFake(t, "composed-id", catalogJSON)
		cache := New(t.TempDir())
		fake.attach(cache)

		_, err := cache.ComposeSchematic("brought", "v1.13.6", []string{"gvisor"})
		if err == nil {
			t.Fatal("ComposeSchematic() composed from an unknown schematic id")
		}
		for _, fragment := range []string{"brought", "Image Factory access", "tbx cache pull"} {
			if !strings.Contains(err.Error(), fragment) {
				t.Fatalf("ComposeSchematic() error = %q, want it to contain %q", err, fragment)
			}
		}
		for _, request := range fake.requests {
			if strings.HasPrefix(request, http.MethodPost) {
				t.Fatalf("factory requests = %v, want no schematic POST", fake.requests)
			}
		}
	})

	t.Run("unknown extension name", func(t *testing.T) {
		cache := offlineCache(t, t.TempDir())

		if _, err := cache.ComposeSchematic("brought", "v1.13.6", []string{"gvisr"}); err == nil {
			t.Fatal("ComposeSchematic() accepted an unknown extension on a brought schematic")
		}
	})
}

// TestComposeSchematicReusesRecordedRecomposition keeps the offline path intact
// for brought schematics too: the recorded id is the whole answer.
func TestComposeSchematicReusesRecordedRecomposition(t *testing.T) {
	root := t.TempDir()
	if err := New(root).RecordComposition("brought", "v1.13.6", []string{"gvisor"}, "composed-id"); err != nil {
		t.Fatalf("RecordComposition() error = %v", err)
	}

	offline := offlineCache(t, root)
	id, err := offline.ComposeSchematic("brought", "v1.13.6", []string{"gvisor"})
	if err != nil {
		t.Fatalf("ComposeSchematic() error = %v", err)
	}
	if id != "composed-id" {
		t.Fatalf("ComposeSchematic() = %q, want %q", id, "composed-id")
	}

	// A different base schematic is a different composition and must not
	// reuse the record.
	unreachable := unreachableCache(t, root)
	if _, err := unreachable.ComposeSchematic("other", "v1.13.6", []string{"gvisor"}); err == nil {
		t.Fatal("ComposeSchematic() reused the recorded id for a different base schematic")
	}
}
