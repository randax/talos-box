//go:build linux

package main

import (
	"strings"
	"testing"
)

func TestLinuxAllowedUIDResolution(t *testing.T) {
	t.Parallel()

	explicit := uint32(700)
	tests := []struct {
		name     string
		explicit *uint32
		euid     uint32
		sudoUID  string
		want     *uint32
		wantErr  string
	}{
		{name: "explicit wins", explicit: &explicit, euid: 0, sudoUID: "501", want: uint32Pointer(700)},
		{name: "capability helper authorizes its uid", euid: 501, want: uint32Pointer(501)},
		{name: "sudo helper authorizes invoking uid", euid: 0, sudoUID: "501", want: uint32Pointer(501)},
		{name: "root without sudo remains root only", euid: 0},
		{name: "invalid sudo uid fails closed", euid: 0, sudoUID: "invalid", wantErr: "SUDO_UID"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := linuxAllowedUID(test.explicit, test.euid, test.sudoUID)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("linuxAllowedUID() error = %v, want containing %q", err, test.wantErr)
				}
				return
			}
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
