//go:build darwin

package main

import (
	"errors"
	"strings"
	"testing"
)

// fakeSecurityHost answers the two probes securityInventoryFindings makes:
// the system-extension listing and the MTE capability sysctl.
func fakeSecurityHost(t *testing.T, extensionList string, mte string, mteErr error) commandOutput {
	t.Helper()
	return func(name string, args ...string) ([]byte, error) {
		switch {
		case strings.HasSuffix(name, "systemextensionsctl"):
			return []byte(extensionList), nil
		case strings.HasSuffix(name, "sysctl"):
			if mteErr != nil {
				return nil, mteErr
			}
			return []byte(mte + "\n"), nil
		default:
			t.Fatalf("unexpected command %s %v", name, args)
			return nil, nil
		}
	}
}

const networkExtensionHeader = "--- com.apple.system_extension.network_extension (Go to 'System Settings' to modify these system extension(s))\n"

const endpointSecurityHeader = "--- com.apple.system_extension.endpoint_security\n"

const activatedGlobalProtect = networkExtensionHeader +
	"* * PXPZ95SK77 com.paloaltonetworks.GlobalProtect.client.extension (6.2.5/6.2.5) GlobalProtect [activated enabled]\n"

// A content-filter system extension on a host whose silicon enforces memory
// tagging is the #513 panic shape: closing a filtered TCP socket can take the
// whole Mac down. Doctor must say so, as a WARN, naming the extension.
func TestSecurityInventoryWarnsContentFilterOnMTEHost(t *testing.T) {
	t.Parallel()

	findings := securityInventoryFindings(fakeSecurityHost(t, activatedGlobalProtect, "1", nil))
	var warn *doctorFinding
	for i := range findings {
		if findings[i].level == "WARN" {
			warn = &findings[i]
		}
	}
	if warn == nil {
		t.Fatalf("findings = %+v, want a WARN panic-risk finding", findings)
	}
	if warn.check != "security-inventory" {
		t.Fatalf("check = %q, want security-inventory", warn.check)
	}
	for _, want := range []string{"com.paloaltonetworks.GlobalProtect.client.extension", "#513", "panic"} {
		if !strings.Contains(warn.detail, want) {
			t.Fatalf("detail %q does not mention %q", warn.detail, want)
		}
	}
}

// Several filter extensions share one exposure and one remediation, so they
// share one WARN line naming them all — never a wall of near-duplicates.
func TestSecurityInventoryOneWarnForManyFilters(t *testing.T) {
	t.Parallel()

	list := activatedGlobalProtect +
		"* * PXPZ95SK77 com.paloaltonetworks.GlobalProtect.filter.extension (6.2.5/6.2.5) GlobalProtect Filter [activated enabled]\n" +
		"* * ZSCALER123 com.zscaler.tunnel (4.3/4.3) Zscaler [activated enabled]\n"
	findings := securityInventoryFindings(fakeSecurityHost(t, list, "1", nil))
	var warns []doctorFinding
	for _, finding := range findings {
		if finding.level == "WARN" {
			warns = append(warns, finding)
		}
	}
	if len(warns) != 1 {
		t.Fatalf("got %d WARN findings %+v, want exactly one", len(warns), warns)
	}
	for _, want := range []string{
		"com.paloaltonetworks.GlobalProtect.client.extension",
		"com.paloaltonetworks.GlobalProtect.filter.extension",
		"com.zscaler.tunnel",
	} {
		if !strings.Contains(warns[0].detail, want) {
			t.Fatalf("detail %q does not name %q", warns[0].detail, want)
		}
	}
}

// A filter vendor's non-filter bundles are not the #513 shape: Defender's
// endpoint-security extension shares the wdav name with its network filter
// but holds no cfil state, so on its own it must not WARN even on MTE.
func TestSecurityInventoryNoPanicRiskForEndpointSecurityBundle(t *testing.T) {
	t.Parallel()

	list := endpointSecurityHeader +
		"* * UBF8T346G9 com.microsoft.wdav.epsext (101.25082.0003/101.25082.0003) Microsoft Defender Security Extension [activated enabled]\n"
	findings := securityInventoryFindings(fakeSecurityHost(t, list, "1", nil))
	for _, finding := range findings {
		if finding.level != "INFO" {
			t.Fatalf("finding %+v, want INFO only for an endpoint-security bundle", finding)
		}
	}
}

// The same vendor's network extension IS a socket filter: only that bundle
// carries the exposure, and the WARN must name it alone.
func TestSecurityInventoryWarnsOnlyOnFilterBundleOfMixedVendor(t *testing.T) {
	t.Parallel()

	list := endpointSecurityHeader +
		"* * UBF8T346G9 com.microsoft.wdav.epsext (101.25082.0003/101.25082.0003) Microsoft Defender Security Extension [activated enabled]\n" +
		networkExtensionHeader +
		"* * UBF8T346G9 com.microsoft.wdav.netext (101.25082.0003/101.25082.0003) Microsoft Defender Network Extension [activated enabled]\n"
	findings := securityInventoryFindings(fakeSecurityHost(t, list, "1", nil))
	var warns []doctorFinding
	for _, finding := range findings {
		if finding.level == "WARN" {
			warns = append(warns, finding)
		}
	}
	if len(warns) != 1 {
		t.Fatalf("got %d WARN findings %+v, want exactly one", len(warns), warns)
	}
	if !strings.Contains(warns[0].detail, "com.microsoft.wdav.netext") ||
		strings.Contains(warns[0].detail, "com.microsoft.wdav.epsext") {
		t.Fatalf("detail %q must name the network filter and not the endpoint-security bundle", warns[0].detail)
	}
}

// The same filter on silicon without memory tagging cannot panic the host, so
// the risk line stays absent and the inventory remains informational.
func TestSecurityInventoryNoPanicRiskWithoutMTE(t *testing.T) {
	t.Parallel()

	findings := securityInventoryFindings(fakeSecurityHost(t, activatedGlobalProtect, "0", nil))
	for _, finding := range findings {
		if finding.level != "INFO" {
			t.Fatalf("finding %+v, want INFO only without MTE", finding)
		}
	}
}

// An MTE host with no content-filter-class extension has no #513 exposure.
func TestSecurityInventoryNoPanicRiskWithoutFilter(t *testing.T) {
	t.Parallel()

	tailscaleOnly := networkExtensionHeader +
		"* * XYZ1234567 com.tailscale.ipn.macsys.network-extension (1.66/1.66) Tailscale [activated enabled]\n"
	findings := securityInventoryFindings(fakeSecurityHost(t, tailscaleOnly, "1", nil))
	for _, finding := range findings {
		if finding.level != "INFO" {
			t.Fatalf("finding %+v, want INFO only without a content filter", finding)
		}
	}
}

// A failed capability probe is silence, not a guess: no WARN on unknown MTE.
func TestSecurityInventoryNoPanicRiskWhenMTEUnknown(t *testing.T) {
	t.Parallel()

	findings := securityInventoryFindings(fakeSecurityHost(t, activatedGlobalProtect, "", errors.New("sysctl unavailable")))
	for _, finding := range findings {
		if finding.level != "INFO" {
			t.Fatalf("finding %+v, want INFO only when MTE capability is unknown", finding)
		}
	}
}
