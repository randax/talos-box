package vm

import "testing"

func TestParseLeaseIP(t *testing.T) {
	t.Parallel()

	const leases = `{
	name=other
	ip_address=172.30.3.2
	hw_address=1,52:54:0:aa:bb:4
}
{
	name=talos
	ip_address=172.30.3.7
	hw_address=1,52:54:0:a:b:5
}
`

	tests := []struct {
		name   string
		mac    string
		subnet int
		want   string
	}{
		{name: "stripped leading zeros", mac: "52:54:00:0a:0b:05", subnet: 3, want: "172.30.3.7"},
		{name: "another lease", mac: "52:54:00:aa:bb:04", subnet: 3, want: "172.30.3.2"},
		{name: "wrong subnet", mac: "52:54:00:aa:bb:04", subnet: 4, want: ""},
		{name: "not found", mac: "52:54:00:aa:bb:06", subnet: 3, want: ""},
		{name: "invalid MAC", mac: "not-a-mac", subnet: 3, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := parseLeaseIP(leases, tt.mac, tt.subnet); got != tt.want {
				t.Fatalf("parseLeaseIP() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseLeaseIPSkipsStaleSubnet(t *testing.T) {
	t.Parallel()
	const leases = `{
	ip_address=172.30.2.9
	hw_address=1,52:54:0:a:b:5
}
{
	ip_address=172.30.3.10
	hw_address=1,52:54:0:a:b:5
}
`
	if got := parseLeaseIP(leases, "52:54:00:0a:0b:05", 3); got != "172.30.3.10" {
		t.Fatalf("parseLeaseIP() = %q, want 172.30.3.10", got)
	}
}

func TestValidLeaseIPRange(t *testing.T) {
	t.Parallel()
	tests := []struct {
		ip   string
		want bool
	}{
		{ip: "172.30.3.1"},
		{ip: "172.30.3.2", want: true},
		{ip: "172.30.3.179", want: true},
		{ip: "172.30.3.180"},
		{ip: "172.30.4.2"},
		{ip: "not-an-ip"},
	}
	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			t.Parallel()
			if got := validLeaseIP(tt.ip, 3); got != tt.want {
				t.Fatalf("validLeaseIP(%q, 3) = %v, want %v", tt.ip, got, tt.want)
			}
		})
	}
}
