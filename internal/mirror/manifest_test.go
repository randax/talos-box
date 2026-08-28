package mirror

import (
	"strings"
	"testing"
)

func TestDecodeManifestGraphRejectsStructurallyInvalidManifests(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "missing config descriptor",
			body: `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[]}`,
			want: "missing config descriptor",
		},
		{
			name: "leaf cannot masquerade as index",
			body: `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","manifests":[]}`,
			want: "missing config descriptor",
		},
		{
			name: "missing layers array",
			body: `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:` + strings.Repeat("a", 64) + `"}}`,
			want: "missing layers array",
		},
		{
			name: "invalid config digest",
			body: `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:not-a-digest"},"layers":[]}`,
			want: "config descriptor has invalid digest: invalid digest reference",
		},
		{
			name: "missing manifests array",
			body: `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json"}`,
			want: "missing manifests array",
		},
		{
			name: "invalid child digest",
			body: `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":"sha256:not-a-digest","platform":{"os":"linux","architecture":"amd64"}}]}`,
			want: "manifest descriptor has invalid digest at manifests[0]: invalid digest reference",
		},
		{
			name: "wrong schema version",
			body: `{"schemaVersion":1,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:` + strings.Repeat("a", 64) + `"},"layers":[]}`,
			want: "schemaVersion 1, want 2",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeManifestGraph([]byte(test.body)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("decodeManifestGraph() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestDecodeManifestGraphAcceptsLeafAndIndex(t *testing.T) {
	leaf := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:` + strings.Repeat("a", 64) + `"},"layers":[]}`
	index := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":"sha256:` + strings.Repeat("b", 64) + `","platform":{"os":"linux","architecture":"amd64"}}]}`

	for _, body := range []string{leaf, index} {
		if _, err := decodeManifestGraph([]byte(body)); err != nil {
			t.Fatalf("decodeManifestGraph(%s) = %v", body, err)
		}
	}
}
