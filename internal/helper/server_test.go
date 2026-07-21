package helper

import "testing"

func TestIsAuthorizedUID(t *testing.T) {
	t.Parallel()

	allowedUID := uint32(501)
	tests := []struct {
		name       string
		uid        uint32
		allowedUID *uint32
		want       bool
	}{
		{name: "allowed uid", uid: 501, allowedUID: &allowedUID, want: true},
		{name: "root", uid: 0, allowedUID: &allowedUID, want: true},
		{name: "other uid", uid: 502, allowedUID: &allowedUID, want: false},
		{name: "unset allows root", uid: 0, want: true},
		{name: "unset rejects user", uid: 501, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := isAuthorizedUID(test.uid, test.allowedUID); got != test.want {
				t.Fatalf("isAuthorizedUID(%d) = %t, want %t", test.uid, got, test.want)
			}
		})
	}
}
