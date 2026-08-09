package imagecache

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSchematicRequestBody(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		extra []string
		want  string
	}{
		{
			name: "required arguments",
			want: `{"customization":{"extraKernelArgs":["console=tty0","console=hvc0"]}}`,
		},
		{
			name:  "user arguments follow required arguments",
			extra: []string{"talos.platform=metal", "panic=10"},
			want:  `{"customization":{"extraKernelArgs":["console=tty0","console=hvc0","talos.platform=metal","panic=10"]}}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			body, err := schematicRequestBody(test.extra)
			if err != nil {
				t.Fatalf("schematicRequestBody() error = %v", err)
			}

			if string(body) != test.want {
				t.Fatalf("request body = %s, want %s", body, test.want)
			}
		})
	}
}

func TestHTTPClientTimeoutConfiguration(t *testing.T) {
	t.Parallel()

	cache := New(t.TempDir())
	tests := []struct {
		name               string
		client             *http.Client
		wantTimeout        time.Duration
		wantTransport      bool
		wantTLSHandshake   time.Duration
		wantResponseHeader time.Duration
	}{
		{
			name:        "schematic POST has an overall timeout",
			client:      cache.schematicClient,
			wantTimeout: 30 * time.Second,
		},
		{
			name:               "image download has phase timeouts only",
			client:             cache.downloadClient,
			wantTransport:      true,
			wantTLSHandshake:   10 * time.Second,
			wantResponseHeader: 30 * time.Second,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.client.Timeout != test.wantTimeout {
				t.Errorf("client timeout = %s, want %s", test.client.Timeout, test.wantTimeout)
			}
			if !test.wantTransport {
				return
			}
			transport, ok := test.client.Transport.(*http.Transport)
			if !ok {
				t.Fatalf("transport = %T, want *http.Transport", test.client.Transport)
			}
			if transport.DialContext == nil {
				t.Error("transport has no dial timeout configuration")
			}
			if transport.TLSHandshakeTimeout != test.wantTLSHandshake {
				t.Errorf("TLS handshake timeout = %s, want %s", transport.TLSHandshakeTimeout, test.wantTLSHandshake)
			}
			if transport.ResponseHeaderTimeout != test.wantResponseHeader {
				t.Errorf("response header timeout = %s, want %s", transport.ResponseHeaderTimeout, test.wantResponseHeader)
			}
		})
	}
}

func TestDownloadValidatesXZMagicBeforeCaching(t *testing.T) {
	t.Parallel()

	xzMagic := []byte{0xfd, 0x37, 0x7a, 0x58, 0x5a, 0x00}
	tests := []struct {
		name        string
		contentType string
		body        []byte
		wantError   string
	}{
		{
			name:        "HTML content type is rejected",
			contentType: "text/html; charset=utf-8",
			body:        []byte("<html>request blocked</html>"),
			wantError:   "possible proxy block page",
		},
		{
			name:        "block page body without XZ magic is rejected",
			contentType: "application/octet-stream",
			body:        []byte("<html>request blocked</html>"),
			wantError:   "possible proxy block page",
		},
		{
			name:        "truncated body reports the read error, not a block page",
			contentType: "application/x-xz",
			body:        xzMagic[:3],
			wantError:   "read response prefix",
		},
		{
			name:        "valid XZ is accepted with magic intact",
			contentType: "application/x-xz",
			body:        append(append([]byte(nil), xzMagic...), []byte("compressed-payload")...),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				_, _ = w.Write(test.body)
			}))
			defer upstream.Close()

			cache := New(t.TempDir())
			cache.downloadClient = upstream.Client()
			destination := filepath.Join(cache.root, "image.raw.xz")
			err := cache.download(upstream.URL+"/image.raw.xz", destination)

			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("download() error = %v, want containing %q", err, test.wantError)
				}
				if _, statErr := os.Stat(destination); !os.IsNotExist(statErr) {
					t.Fatalf("rejected download was cached (stat error: %v)", statErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("download() error = %v", err)
			}
			got, err := os.ReadFile(destination)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, test.body) {
				t.Fatalf("downloaded bytes = %x, want %x", got, test.body)
			}
			if !bytes.HasPrefix(got, xzMagic) {
				t.Fatalf("download lost XZ magic: %x", got)
			}
		})
	}
}

func TestEnsureCachesArchitecturesSideBySide(t *testing.T) {
	requireXZ(t)
	t.Parallel()

	archives := map[string][]byte{
		"/image/test-schematic/v1.2.3/metal-amd64.raw.xz": compressXZ(t, "amd64 disk"),
		"/image/test-schematic/v1.2.3/metal-arm64.raw.xz": compressXZ(t, "arm64 disk"),
	}
	requests := make(chan string, len(archives))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.URL.EscapedPath()
		archive, ok := archives[r.URL.EscapedPath()]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-xz")
		_, _ = w.Write(archive)
	}))
	defer upstream.Close()

	cache := New(t.TempDir())
	cache.factoryURL = upstream.URL
	cache.downloadClient = upstream.Client()

	paths := make(map[Architecture]string)
	for _, architecture := range []Architecture{ArchitectureAMD64, ArchitectureARM64} {
		path, err := cache.Ensure("test-schematic", "v1.2.3", architecture)
		if err != nil {
			t.Fatalf("Ensure(%q) error = %v", architecture, err)
		}
		paths[architecture] = path
		wantPath := filepath.Join(cache.root, "test-schematic", "v1.2.3", string(architecture), "disk.raw")
		if path != wantPath {
			t.Errorf("Ensure(%q) path = %q, want %q", architecture, path, wantPath)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf("%s disk", architecture)
		if string(got) != want {
			t.Errorf("Ensure(%q) disk = %q, want %q", architecture, got, want)
		}
	}
	if paths[ArchitectureAMD64] == paths[ArchitectureARM64] {
		t.Fatalf("amd64 and arm64 resolved to the same path %q", paths[ArchitectureAMD64])
	}

	close(requests)
	var gotRequests []string
	for request := range requests {
		gotRequests = append(gotRequests, request)
	}
	wantRequests := []string{
		"/image/test-schematic/v1.2.3/metal-amd64.raw.xz",
		"/image/test-schematic/v1.2.3/metal-arm64.raw.xz",
	}
	if !reflect.DeepEqual(gotRequests, wantRequests) {
		t.Fatalf("download requests = %v, want %v", gotRequests, wantRequests)
	}

	entries, err := cache.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("List() returned %d entries, want 2: %+v", len(entries), entries)
	}
	for index, architecture := range []Architecture{ArchitectureAMD64, ArchitectureARM64} {
		entry := entries[index]
		if entry.Schematic != "test-schematic" || entry.Version != "v1.2.3" || entry.Architecture != architecture || entry.Path != paths[architecture] {
			t.Errorf("List()[%d] = %+v, want architecture %q path %q", index, entry, architecture, paths[architecture])
		}
	}
}

func TestEnsureMigratesLegacyArm64DiskWithoutUsingItForAMD64(t *testing.T) {
	requireXZ(t)
	t.Parallel()

	root := t.TempDir()
	legacyPath := filepath.Join(root, "test-schematic", "v1.2.3", "disk.raw")
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte("legacy arm64 disk"), 0o600); err != nil {
		t.Fatal(err)
	}

	amd64Archive := compressXZ(t, "amd64 disk")
	var requests []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.EscapedPath())
		w.Header().Set("Content-Type", "application/x-xz")
		_, _ = w.Write(amd64Archive)
	}))
	defer upstream.Close()

	cache := New(root)
	cache.factoryURL = upstream.URL
	cache.downloadClient = upstream.Client()

	entries, err := cache.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Architecture != ArchitectureARM64 || entries[0].Path != legacyPath {
		t.Fatalf("List() legacy entries = %+v, want one arm64 entry at %q", entries, legacyPath)
	}

	arm64Path, err := cache.Ensure("test-schematic", "v1.2.3", ArchitectureARM64)
	if err != nil {
		t.Fatalf("Ensure(arm64) error = %v", err)
	}
	wantArm64Path := filepath.Join(root, "test-schematic", "v1.2.3", "arm64", "disk.raw")
	if arm64Path != wantArm64Path {
		t.Fatalf("Ensure(arm64) path = %q, want %q", arm64Path, wantArm64Path)
	}
	got, err := os.ReadFile(arm64Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "legacy arm64 disk" {
		t.Fatalf("migrated disk = %q, want legacy arm64 disk", got)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy disk still exists after migration (stat error: %v)", err)
	}
	if len(requests) != 0 {
		t.Fatalf("arm64 migration made download requests: %v", requests)
	}

	amd64Path, err := cache.Ensure("test-schematic", "v1.2.3", ArchitectureAMD64)
	if err != nil {
		t.Fatalf("Ensure(amd64) error = %v", err)
	}
	got, err = os.ReadFile(amd64Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "amd64 disk" {
		t.Fatalf("amd64 disk = %q, want freshly downloaded amd64 disk", got)
	}
	wantRequest := "/image/test-schematic/v1.2.3/metal-amd64.raw.xz"
	if !reflect.DeepEqual(requests, []string{wantRequest}) {
		t.Fatalf("download requests = %v, want [%s]", requests, wantRequest)
	}
}

func TestEnsureRejectsUnsupportedArchitecture(t *testing.T) {
	t.Parallel()

	cache := New(t.TempDir())
	_, err := cache.Ensure("test-schematic", "v1.2.3", Architecture("ppc64"))
	if err == nil || !strings.Contains(err.Error(), `unsupported architecture "ppc64"`) {
		t.Fatalf("Ensure() error = %v, want unsupported architecture", err)
	}
}

func compressXZ(t *testing.T, contents string) []byte {
	t.Helper()
	command := exec.Command("xz", "-c")
	command.Stdin = strings.NewReader(contents)
	archive, err := command.Output()
	if err != nil {
		t.Fatalf("compress XZ test fixture: %v", err)
	}
	return archive
}

func requireXZ(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("xz"); err != nil {
		t.Skip("xz not installed; skipping imagecache Ensure test")
	}
}
