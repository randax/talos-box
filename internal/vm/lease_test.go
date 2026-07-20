package vm

import "testing"

func TestParseLeaseIP(t *testing.T) {
	t.Parallel()

	const leases = `{
	name=other
	ip_address=192.168.64.2
	hw_address=1,52:54:0:aa:bb:4
}
{
	name=talos
	ip_address=192.168.64.7
	hw_address=1,52:54:0:a:b:5
}
`

	tests := []struct {
		name string
		mac  string
		want string
	}{
		{name: "stripped leading zeros", mac: "52:54:00:0a:0b:05", want: "192.168.64.7"},
		{name: "another lease", mac: "52:54:00:aa:bb:04", want: "192.168.64.2"},
		{name: "not found", mac: "52:54:00:aa:bb:06", want: ""},
		{name: "invalid MAC", mac: "not-a-mac", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := parseLeaseIP(leases, tt.mac); got != tt.want {
				t.Fatalf("parseLeaseIP() = %q, want %q", got, tt.want)
			}
		})
	}
}
