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

func TestServerAllowedUIDUsesSocketAdmissionOnlyWhenImplicitAndActivated(t *testing.T) {
	t.Parallel()

	serviceUID := uint32(995)
	userUID := uint32(501)
	if got := serverAllowedUID(&serviceUID, false, true); got != nil {
		t.Fatalf("implicit activated UID = %v, want socket admission", got)
	}
	if got := serverAllowedUID(&userUID, true, true); got == nil || *got != userUID {
		t.Fatalf("explicit activated UID = %v, want %d", got, userUID)
	}
	if got := serverAllowedUID(&userUID, false, false); got == nil || *got != userUID {
		t.Fatalf("manual helper UID = %v, want %d", got, userUID)
	}
}

func uint32Pointer(value uint32) *uint32 {
	return &value
}
