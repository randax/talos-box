package main

import "testing"

func TestParseAllowedUID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want *uint32
	}{
		{name: "unset"},
		{name: "configured", args: []string{"--allowed-uid", "501"}, want: uint32Pointer(501)},
		{name: "root", args: []string{"--allowed-uid=0"}, want: uint32Pointer(0)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseAllowedUID(test.args)
			if err != nil {
				t.Fatal(err)
			}
			if got == nil || test.want == nil {
				if got != nil || test.want != nil {
					t.Fatalf("allowed uid = %v, want %v", got, test.want)
				}
				return
			}
			if *got != *test.want {
				t.Fatalf("allowed uid = %d, want %d", *got, *test.want)
			}
		})
	}
}

func TestParseAllowedUIDRejectsInvalidValue(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"", "-1", "not-a-uid", "4294967296"} {
		if _, err := parseAllowedUID([]string{"--allowed-uid", value}); err == nil {
			t.Fatalf("parseAllowedUID accepted %q", value)
		}
	}
}

func uint32Pointer(value uint32) *uint32 {
	return &value
}
