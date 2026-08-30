//go:build darwin

package main

import (
	"bufio"
	"fmt"
	"strings"
)

func securityInventoryFindings(command commandOutput) []doctorFinding {
	output, err := command("/usr/bin/systemextensionsctl", "list")
	if err != nil {
		return []doctorFinding{{
			level: "INFO", check: "security-inventory",
			detail: fmt.Sprintf("system extension inventory unavailable: %v", err),
		}}
	}
	bundleIDs := parseActivatedSystemExtensions(output)
	if len(bundleIDs) == 0 {
		return []doctorFinding{{
			level: "INFO", check: "security-inventory",
			detail: "no activated system extensions found",
		}}
	}
	findings := make([]doctorFinding, 0, len(bundleIDs))
	for _, bundleID := range bundleIDs {
		detail := bundleID
		if warning := securityExtensionWarning(bundleID); warning != "" {
			detail += ": " + warning
		}
		findings = append(findings, doctorFinding{
			level: "INFO", check: "security-inventory", detail: detail,
		})
	}
	findings = append(findings, panicRiskFindings(command, bundleIDs)...)
	return findings
}

// panicRiskFindings reports the #513 exposure: a content-filter system
// extension on silicon that enforces kernel memory tagging. On such a host an
// XNU content-filter bug can panic the whole Mac when any process — tbxd
// included — closes a filtered TCP socket. Both ingredients are required, so
// nothing prints without them, and an unreadable capability probe stays
// silent rather than guessing.
func panicRiskFindings(command commandOutput, bundleIDs []string) []doctorFinding {
	var filters []string
	for _, bundleID := range bundleIDs {
		if isContentFilterExtension(bundleID) {
			filters = append(filters, bundleID)
		}
	}
	if len(filters) == 0 || !hostEnforcesMemoryTagging(command) {
		return nil
	}
	findings := make([]doctorFinding, 0, len(filters))
	for _, bundleID := range filters {
		findings = append(findings, doctorFinding{
			level: "WARN", check: "security-inventory",
			detail: bundleID + ": content filter active on a memory-tagging (MTE) host; " +
				"a macOS kernel bug can panic this Mac when filtered TCP sockets close, " +
				"which cluster lifecycle transitions do in bulk (#513); " +
				"exempt tbx traffic from the filter or deactivate the extension to remove the exposure",
		})
	}
	return findings
}

// isContentFilterExtension reports whether the bundle is a known socket
// content-filter provider — the class that leaves cfil state on TCP sockets.
// Plain VPNs and EDR agents without a filter data provider are not the #513
// shape, so they stay out.
func isContentFilterExtension(bundleID string) bool {
	return containsAny(strings.ToLower(bundleID),
		"paloaltonetworks", "globalprotect",
		"zscaler", "netskope", "cisco.anyconnect", "cisco.secureclient",
	)
}

// hostEnforcesMemoryTagging asks the kernel whether the silicon has MTE —
// hw.optional.arm.FEAT_MTE is 1 on M5-generation Macs, 0 before, and only
// MTE hardware can turn the #513 use-after-free into a panic.
func hostEnforcesMemoryTagging(command commandOutput) bool {
	output, err := command("/usr/sbin/sysctl", "-n", "hw.optional.arm.FEAT_MTE")
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(output)) == "1"
}

func parseActivatedSystemExtensions(output []byte) []string {
	var bundleIDs []string
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[0] != "*" || fields[1] != "*" ||
			!strings.Contains(strings.ToLower(line), "[activated") {
			continue
		}
		bundleIDs = append(bundleIDs, fields[3])
	}
	return bundleIDs
}

func securityExtensionWarning(bundleID string) string {
	lower := strings.ToLower(bundleID)
	switch {
	case containsAny(lower, "paloaltonetworks", "globalprotect"):
		return "guest TLS will be reset; registry mirrors are required"
	case containsAny(lower, "zscaler", "netskope", "cisco.anyconnect", "cisco.secureclient"):
		return "may filter local/guest traffic or DNS"
	case containsAny(lower, "crowdstrike", "wdav", "sentinelone"):
		return "EDR present; ad-hoc-signed binaries may be blocked"
	case containsAny(lower, "tailscale", "protonvpn", "wireguard"):
		return "VPN present; check route capture"
	default:
		return ""
	}
}

func containsAny(value string, substrings ...string) bool {
	for _, substring := range substrings {
		if strings.Contains(value, substring) {
			return true
		}
	}
	return false
}
