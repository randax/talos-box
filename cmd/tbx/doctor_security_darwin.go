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
	if len(filters) == 0 || !hostHasMemoryTagging(command) {
		return nil
	}
	// One WARN covers the exposure however many filter extensions share it:
	// the remediation is the same, and a vendor shipping several bundles must
	// not turn one condition into a wall of near-identical lines.
	return []doctorFinding{{
		level: "WARN", check: "security-inventory",
		detail: strings.Join(filters, ", ") + ": content filter active on a memory-tagging (MTE) host; " +
			"a macOS kernel bug can panic this Mac when filtered TCP sockets close, " +
			"which cluster lifecycle transitions do in bulk (#513); " +
			"exempt tbx traffic from the filter or deactivate the extension to remove the exposure " +
			"(docs/macos-panics.md)",
	}}
}

// isContentFilterExtension reports whether the bundle is a known socket
// filter-provider — the class that leaves cfil state on TCP sockets. That
// includes the EDR network extensions, whose protection modules are socket
// filters too; only plain VPNs stay out. The vendor table is shared with
// securityExtensionWarning so a vendor is never listed in one and forgotten
// in the other.
func isContentFilterExtension(bundleID string) bool {
	lower := strings.ToLower(bundleID)
	for _, class := range securityExtensionClasses {
		if class.contentFilter && containsAny(lower, class.substrings...) {
			return true
		}
	}
	return false
}

// hostHasMemoryTagging asks the kernel whether the silicon supports MTE —
// hw.optional.arm.FEAT_MTE is 1 where it does — because only MTE hardware
// can turn the #513 use-after-free into a panic. The bit reports the ISA
// capability, which is the best userspace proxy for kernel tag enforcement.
func hostHasMemoryTagging(command commandOutput) bool {
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

// securityExtensionClasses is the one table of known security-software
// vendors: the INFO annotation each class gets, and whether its extension is
// a socket filter-provider (the #513 panic-exposure class).
var securityExtensionClasses = []struct {
	substrings    []string
	warning       string
	contentFilter bool
}{
	{
		substrings:    []string{"paloaltonetworks", "globalprotect"},
		warning:       "guest TLS will be reset; registry mirrors are required",
		contentFilter: true,
	},
	{
		substrings:    []string{"zscaler", "netskope", "cisco.anyconnect", "cisco.secureclient"},
		warning:       "may filter local/guest traffic or DNS",
		contentFilter: true,
	},
	{
		substrings:    []string{"crowdstrike", "wdav", "sentinelone"},
		warning:       "EDR present; ad-hoc-signed binaries may be blocked",
		contentFilter: true,
	},
	{
		substrings:    []string{"tailscale", "protonvpn", "wireguard"},
		warning:       "VPN present; check route capture",
		contentFilter: false,
	},
}

func securityExtensionWarning(bundleID string) string {
	lower := strings.ToLower(bundleID)
	for _, class := range securityExtensionClasses {
		if containsAny(lower, class.substrings...) {
			return class.warning
		}
	}
	return ""
}

func containsAny(value string, substrings ...string) bool {
	for _, substring := range substrings {
		if strings.Contains(value, substring) {
			return true
		}
	}
	return false
}
