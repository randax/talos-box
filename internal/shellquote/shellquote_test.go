package shellquote

import "testing"

func TestQuote(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "safe", value: "demo-1", want: "demo-1"},
		{name: "empty", want: "''"},
		{name: "metacharacters", value: "demo; echo owned", want: "'demo; echo owned'"},
		{name: "single quote", value: "demo'owned", want: "'demo'\"'\"'owned'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Quote(tt.value); got != tt.want {
				t.Fatalf("Quote(%q) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}
