package talosversion

import (
	"strings"
	"testing"

	machineryconfig "github.com/siderolabs/talos/pkg/machinery/config"
)

func TestValidateAcceptsSupportedVersions(t *testing.T) {
	for _, version := range []string{Min, Default, "v1.12.3", "v1.13.0", "v1.14.0", "v2.0.0", "v1.14.0-beta.1"} {
		if err := Validate(version); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", version, err)
		}
	}
}

func TestValidateRefusesBelowFloorNamingBothVersions(t *testing.T) {
	for _, version := range []string{"v0.14.0", "v1.11.9", "v1.0.0", "v1.12.0-alpha.1"} {
		err := Validate(version)
		if err == nil {
			t.Fatalf("Validate(%q) = nil, want below-floor refusal", version)
		}
		if !strings.Contains(err.Error(), version) || !strings.Contains(err.Error(), Min) {
			t.Errorf("Validate(%q) error %q must name the requested and minimum versions", version, err)
		}
	}
}

func TestValidateRefusesMalformedVersions(t *testing.T) {
	for _, version := range []string{
		"garbage", "1.13", "v1.13", "v1..6", "v1.13.six", "latest", "v1.13.6 ", "",
		// Semver forbids empty dot-separated pre-release identifiers.
		"v1.14.0-", "v1.14.0-..", "v1.14.0-.a", "v1.14.0-a..b", "v1.14.0-a.",
	} {
		if err := Validate(version); err == nil {
			t.Errorf("Validate(%q) = nil, want malformed-version refusal", version)
		}
	}
}

func TestWarningAboveDefaultOnlyAndNamesBothVersions(t *testing.T) {
	if warning := NewerThanTestedWarning("v1.14.0"); warning == "" ||
		!strings.Contains(warning, "v1.14.0") || !strings.Contains(warning, Default) {
		t.Errorf("NewerThanTestedWarning(v1.14.0) = %q, want a warning naming both versions", warning)
	}
	if warning := NewerThanTestedWarning("v1.13.7"); warning == "" {
		t.Error("NewerThanTestedWarning(v1.13.7) = \"\", want a warning for a newer patch")
	}
	if warning := NewerThanTestedWarning("v1.13.7-alpha.1"); warning == "" {
		t.Error("NewerThanTestedWarning(v1.13.7-alpha.1) = \"\", want a warning for a pre-release above the default")
	}
	for _, version := range []string{Default, Min, "v1.12.5", "v1.13.6-beta.0"} {
		if warning := NewerThanTestedWarning(version); warning != "" {
			t.Errorf("NewerThanTestedWarning(%q) = %q, want silence within [floor, default]", version, warning)
		}
	}
}

// CI guard: machinery must still define a version contract for the pinned
// floor. A machinery bump that drops the contract fails compilation here.
func TestMachineryDefinesContractForFloor(t *testing.T) {
	contract, err := machineryconfig.ParseContractFromVersion(Min)
	if err != nil {
		t.Fatalf("ParseContractFromVersion(%q) = %v", Min, err)
	}
	if *contract != *machineryconfig.TalosVersion1_12 {
		t.Fatalf("floor %q parses to contract %s, want machinery's TalosVersion1_12; bump the named contract with the floor", Min, contract)
	}
}

func TestFloorIsDefaultsPreviousMinor(t *testing.T) {
	floor, err := machineryconfig.ParseContractFromVersion(Min)
	if err != nil {
		t.Fatal(err)
	}
	tested, err := machineryconfig.ParseContractFromVersion(Default)
	if err != nil {
		t.Fatal(err)
	}
	if floor.Major != tested.Major || floor.Minor != tested.Minor-1 {
		t.Fatalf("floor %s is not the previous minor of default %s; bump both in the same diff", floor, tested)
	}
}
