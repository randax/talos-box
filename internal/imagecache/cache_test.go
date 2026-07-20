package imagecache

import (
	"testing"
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
