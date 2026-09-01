//go:build darwin

package main

import "testing"

func TestDarwinPlatformDoctorCheckNamesExcludeWSL(t *testing.T) {
	t.Parallel()

	names := platformDoctorCheckNames()
	if len(names) != 1 || names[0] != "port-179" {
		t.Fatalf("platformDoctorCheckNames() = %v, want [port-179]", names)
	}
	deps := doctorDependencies{}
	for _, finding := range darwinPlatformDoctorFindings(deps) {
		if finding.check == "wsl" {
			t.Fatalf("Darwin findings = %+v, must exclude wsl", darwinPlatformDoctorFindings(deps))
		}
	}
}
