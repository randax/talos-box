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
	return findings
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
