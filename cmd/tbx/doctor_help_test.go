package main

import (
	"bytes"
	"strings"
	"testing"
)

// `tbx doctor --help` used to print the bare usage line, leaving the exit-code
// contract — the command's most automation-relevant fact — documented only in
// docs/macos.md (#419).
func TestDoctorHelpDescribesChecksAndExitCodes(t *testing.T) {
	for _, flag := range []string{"-h", "--help"} {
		t.Run(flag, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			command := cli{out: &stdout, err: &stderr}
			if err := command.runDoctorWithDependencies([]string{flag}, doctorDependencies{}); err != nil {
				t.Fatalf("doctor %s = %v, want nil", flag, err)
			}
			help := stdout.String()
			want := []string{
				"usage: tbx doctor",
				"exits non-zero",
				"WARN",
				"INFO",
				// every portable check name is listed
				"helper", "resolver", "DNS", "forwarding", "host-pressure",
				"system-dns", "routes", "guest-agent", "mirror-health",
				"image-cache", "egress", "security-inventory",
			}
			for _, substring := range want {
				if !strings.Contains(help, substring) {
					t.Errorf("doctor %s output missing %q:\n%s", flag, substring, help)
				}
			}
			for _, name := range platformDoctorCheckNames() {
				if !strings.Contains(help, name) {
					t.Errorf("doctor %s output missing platform check %q:\n%s", flag, name, help)
				}
			}
		})
	}
}

// A help flag must never be mistaken for an argument, and a real argument must
// still be refused.
func TestDoctorRejectsArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}
	err := command.runDoctorWithDependencies([]string{"cluster"}, doctorDependencies{})
	if err == nil || !strings.Contains(err.Error(), "usage: tbx doctor") {
		t.Fatalf("doctor cluster = %v, want a usage error", err)
	}
}
