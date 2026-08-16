package extensions

import (
	"reflect"
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		requested []string
		want      []string
		wantError []string
	}{
		{
			name:      "curated names map to official refs",
			requested: []string{"qemu-guest-agent", "gvisor", "nfs-utils"},
			want:      []string{"siderolabs/gvisor", "siderolabs/nfs-utils", "siderolabs/qemu-guest-agent"},
		},
		{
			name:      "repeats collapse",
			requested: []string{"gvisor", "gvisor"},
			want:      []string{"siderolabs/gvisor"},
		},
		{
			name:      "typo suggests the near miss",
			requested: []string{"gvisr"},
			wantError: []string{`unknown extension "gvisr"`, `did you mean "gvisor"`},
		},
		{
			name:      "prefix suggests the near miss",
			requested: []string{"nfs"},
			wantError: []string{`did you mean "nfs-utils"`},
		},
		{
			name:      "unrelated name lists the curated set",
			requested: []string{"tailscale"},
			wantError: []string{`unknown extension "tailscale"`, "curated extensions: gvisor, nfs-utils, qemu-guest-agent"},
		},
		{
			name:      "official ref is not a curated name",
			requested: []string{"siderolabs/gvisor"},
			wantError: []string{`unknown extension "siderolabs/gvisor"`},
		},
		{
			name:      "empty name",
			requested: []string{""},
			wantError: []string{"extension name must not be empty"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := Resolve(test.requested)
			if len(test.wantError) > 0 {
				if err == nil {
					t.Fatalf("Resolve(%v) error = nil, want an error", test.requested)
				}
				for _, fragment := range test.wantError {
					if !strings.Contains(err.Error(), fragment) {
						t.Fatalf("Resolve(%v) error = %q, want it to contain %q", test.requested, err, fragment)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve(%v) error = %v", test.requested, err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("Resolve(%v) = %v, want %v", test.requested, got, test.want)
			}
		})
	}
}

func TestNamesAreSortedAndComplete(t *testing.T) {
	t.Parallel()

	want := []string{"gvisor", "nfs-utils", "qemu-guest-agent"}
	if got := Names(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
}

func TestRef(t *testing.T) {
	t.Parallel()

	if got, ok := Ref("qemu-guest-agent"); !ok || got != "siderolabs/qemu-guest-agent" {
		t.Fatalf("Ref(\"qemu-guest-agent\") = (%q, %t), want (\"siderolabs/qemu-guest-agent\", true)", got, ok)
	}
	if _, ok := Ref("zfs"); ok {
		t.Fatal("Ref(\"zfs\") reported a curated extension")
	}
}
