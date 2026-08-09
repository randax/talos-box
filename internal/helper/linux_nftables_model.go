package helper

import (
	"fmt"
	"slices"

	"github.com/randax/talos-box/internal/cluster"
)

const linuxNFTTableName = "tbx"

type linuxNFTRuleKind uint8

const (
	linuxNFTRuleMasquerade linuxNFTRuleKind = iota
	linuxNFTRuleForwardIn
	linuxNFTRuleForwardOut
)

type linuxNFTRule struct {
	kind       linuxNFTRuleKind
	chain      string
	bridge     string
	sourceCIDR string
}

type linuxNFTPlan struct {
	tableFamily   string
	tableName     string
	subnetIndexes []int
	rules         []linuxNFTRule
}

func buildLinuxNFTPlan(subnetIndexes []int) linuxNFTPlan {
	normalized := make([]int, 0, len(subnetIndexes))
	for _, index := range subnetIndexes {
		if index >= 0 && index <= cluster.MaxSubnetIndex {
			normalized = append(normalized, index)
		}
	}
	slices.Sort(normalized)
	normalized = slices.Compact(normalized)

	plan := linuxNFTPlan{
		tableFamily:   "inet",
		tableName:     linuxNFTTableName,
		subnetIndexes: normalized,
		rules:         make([]linuxNFTRule, 0, len(normalized)*3),
	}
	for _, index := range normalized {
		bridge := bridgeNameForSubnet(index)
		plan.rules = append(plan.rules,
			linuxNFTRule{
				kind:       linuxNFTRuleMasquerade,
				chain:      "postrouting",
				bridge:     bridge,
				sourceCIDR: fmt.Sprintf("172.30.%d.0/24", index),
			},
			linuxNFTRule{kind: linuxNFTRuleForwardIn, chain: "forward", bridge: bridge},
			linuxNFTRule{kind: linuxNFTRuleForwardOut, chain: "forward", bridge: bridge},
		)
	}
	return plan
}
