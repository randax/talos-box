package mirror

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestOfflineMissIsLogged pins #403: containerd surfaces only a bare 503 to the
// kubelet event, so without a daemon-log line an offline miss leaves no
// operator-visible trace anywhere in tbx.
func TestOfflineMissIsLogged(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "manifest tag",
			path: "/v2/library/nginx/manifests/1.27.3-alpine",
			want: "mirror offline miss: library/nginx:1.27.3-alpine (upstream namespace ",
		},
		{
			name: "manifest digest",
			path: "/v2/library/nginx/manifests/" + blobDigest,
			want: "mirror offline miss: library/nginx@" + blobDigest,
		},
		{
			name: "blob",
			path: "/v2/library/nginx/blobs/" + blobDigest,
			want: "mirror offline miss: library/nginx/blobs/" + blobDigest,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newFakeRegistry(t, false)
			server := newLoopbackMirrorServer(t, f.registry.URL, t.TempDir())
			mirror := httptest.NewServer(server)
			defer mirror.Close()
			server.setOfflineMode(true)

			logged := captureDaemonLog(t)
			resp, err := http.Get(mirror.URL + test.path)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", resp.StatusCode)
			}
			if got := logged.String(); !strings.Contains(got, test.want) {
				t.Fatalf("daemon log = %q, want substring %q", got, test.want)
			}
		})
	}
}

// A cached hit while offline is not a miss and must stay silent, so the log
// stays greppable for the misses that matter.
func TestOfflineHitIsNotLogged(t *testing.T) {
	f := newFakeRegistry(t, false)
	server := newLoopbackMirrorServer(t, f.registry.URL, t.TempDir())
	mirror := httptest.NewServer(server)
	defer mirror.Close()

	resp, err := http.Get(mirror.URL + "/v2/app/manifests/latest")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("warm status = %d, want 200", resp.StatusCode)
	}
	server.setOfflineMode(true)

	logged := captureDaemonLog(t)
	resp, err = http.Get(mirror.URL + "/v2/app/manifests/latest")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("offline cached status = %d, want 200", resp.StatusCode)
	}
	if got := logged.String(); strings.Contains(got, "offline miss") {
		t.Fatalf("daemon log = %q, want no miss line for a cache hit", got)
	}
}

// captureDaemonLog redirects the standard logger — which in tbxd is tbxd.log —
// into a buffer for the duration of a test.
func captureDaemonLog(t *testing.T) *lockedBuffer {
	t.Helper()
	buffer := &lockedBuffer{}
	flags, writer := log.Flags(), log.Writer()
	log.SetOutput(buffer)
	t.Cleanup(func() {
		log.SetOutput(writer)
		log.SetFlags(flags)
	})
	return buffer
}

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

// The miss line is meant to be recomposed into "<namespace>/<ref>" and handed
// to tbx cache warm/list, so it must name the namespace containerd sent, not
// the CDN alias the base URL points at: docker.io is served from
// registry-1.docker.io, which keys a different cache directory (#403).
func TestOfflineMissLogsRequestedNamespace(t *testing.T) {
	manager := newManagerWithPorts(t.TempDir(), nil, freePort(t))
	manager.offline.Store(true)
	defer manager.Close()
	mirror := httptest.NewServer(http.HandlerFunc(manager.serveCatchAll))
	defer mirror.Close()

	logged := captureDaemonLog(t)
	resp, err := http.Get(mirror.URL + "/v2/library/nginx/manifests/1.27.3?ns=docker.io")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	want := "mirror offline miss: library/nginx:1.27.3 (upstream namespace docker.io)"
	if got := logged.String(); !strings.Contains(got, want) {
		t.Fatalf("daemon log = %q, want substring %q", got, want)
	}
}
