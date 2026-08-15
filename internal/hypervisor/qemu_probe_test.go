package hypervisor

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateQEMUProbe(t *testing.T) {
	tests := []struct {
		name    string
		probe   qemuProbe
		machine string
		wantErr string
	}{
		{name: "minimum", probe: qemuProbe{Version: qemuVersion{Major: 6, Minor: 2}, Machines: []string{"q35"}}, machine: "q35"},
		{name: "machine alias", probe: qemuProbe{Version: qemuVersion{Major: 8, Minor: 2}, Machines: []string{"pc-q35-8.2"}, MachineAliases: []string{"q35"}}, machine: "q35"},
		{name: "too old", probe: qemuProbe{Version: qemuVersion{Major: 6, Minor: 1}, Machines: []string{"q35"}}, machine: "q35", wantErr: "QEMU >= 6.2 is required"},
		{name: "machine absent", probe: qemuProbe{Version: qemuVersion{Major: 8, Minor: 2}, Machines: []string{"pc"}}, machine: "q35", wantErr: `required machine type "q35"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateQEMUProbe(test.probe, test.machine)
			if test.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if test.wantErr != "" && (!errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("validateQEMUProbe() = %v, want ErrUnsupported containing %q", err, test.wantErr)
			}
		})
	}
}

func TestQEMUCapabilitiesGateSuspend(t *testing.T) {
	old := qemuCapabilities(qemuVersion{Major: 6, Minor: 2, Patch: 0})
	if old.Suspend.Supported {
		t.Fatal("QEMU 6.2 unexpectedly supports suspend")
	}
	wantReason := "suspend requires QEMU >= 8.2 (found 6.2) — upgrade to Ubuntu 24.04+"
	if old.Suspend.Reason != wantReason {
		t.Fatalf("reason = %q, want %q", old.Suspend.Reason, wantReason)
	}
	if !old.BalloonReadback.Supported {
		t.Fatal("QEMU did not report balloon readback")
	}

	current := qemuCapabilities(qemuVersion{Major: 8, Minor: 2})
	if !current.Suspend.Supported || current.Suspend.Reason != "" {
		t.Fatalf("QEMU 8.2 suspend = %+v, want supported", current.Suspend)
	}
}
