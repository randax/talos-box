package helper

import (
	"net"
	"reflect"
	"testing"
)

func TestBuildLinuxNFTPlanOwnsOneTableAndNormalizesSubnets(t *testing.T) {
	t.Parallel()

	plan := buildLinuxNFTPlan([]int{7, 3, 7, -1, 256})
	if plan.tableFamily != "inet" || plan.tableName != linuxNFTTableName {
		t.Fatalf("table = %s/%s, want inet/%s", plan.tableFamily, plan.tableName, linuxNFTTableName)
	}
	if got, want := plan.subnetIndexes, []int{3, 7}; !reflect.DeepEqual(got, want) {
		t.Fatalf("subnet indexes = %v, want %v", got, want)
	}
	if len(plan.rules) != 6 {
		t.Fatalf("rules = %d, want 6 (three per unique subnet)", len(plan.rules))
	}
}

func TestBuildLinuxNFTPlanMasqueradesAndForwardsEachBridge(t *testing.T) {
	t.Parallel()

	plan := buildLinuxNFTPlan([]int{9})
	want := []linuxNFTRule{
		{
			kind:       linuxNFTRuleMasquerade,
			chain:      "postrouting",
			bridge:     bridgeNameForSubnet(9),
			sourceCIDR: "172.30.9.0/24",
		},
		{kind: linuxNFTRuleForwardIn, chain: "forward", bridge: bridgeNameForSubnet(9)},
		{kind: linuxNFTRuleForwardOut, chain: "forward", bridge: bridgeNameForSubnet(9)},
	}
	if !reflect.DeepEqual(plan.rules, want) {
		t.Fatalf("rules = %#v, want %#v", plan.rules, want)
	}
	if ip, network, err := net.ParseCIDR(plan.rules[0].sourceCIDR); err != nil || !ip.Equal(net.IPv4(172, 30, 9, 0)) || network.String() != "172.30.9.0/24" {
		t.Fatalf("masquerade source = %q, want 172.30.9.0/24", plan.rules[0].sourceCIDR)
	}
}
