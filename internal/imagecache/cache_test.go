package imagecache

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
