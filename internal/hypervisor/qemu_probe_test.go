package hypervisor

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProbeQEMUUsesInjectedPeerVerifier(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "tbx-qemu-probe-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	launcher := filepath.Join(dir, "fake-qemu")
	script := `#!/bin/sh
socket=
while [ "$#" -gt 0 ]; do
	if [ "$1" = "-qmp" ]; then
		shift
		socket=${1#unix:}
		socket=${socket%,server=on,wait=off}
	fi
	shift
done
TBX_QEMU_PROBE_HELPER=1 exec "$TBX_QEMU_PROBE_TEST_BINARY" -test.run=^TestQEMUProbeHelper$ -- "$socket"
`
	if err := os.WriteFile(launcher, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TBX_QEMU_PROBE_TEST_BINARY", os.Args[0])
	verified := false
	probe, err := probeQEMU(context.Background(), launcher, func(connection net.Conn, process *qemuProcess) error {
		verified = connection != nil && process != nil && process.process != nil
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !verified {
		t.Fatal("probe did not call the peer verifier with the live connection and process")
	}
	if probe.Version != (qemuVersion{Major: 8, Minor: 2, Patch: 1}) {
		t.Fatalf("probe version = %s, want 8.2.1", probe.Version)
	}
	if len(probe.MachineAliases) != 1 || probe.MachineAliases[0] != "q35" {
		t.Fatalf("probe aliases = %v, want q35", probe.MachineAliases)
	}
}

func TestQEMUProbeHelper(t *testing.T) {
	if os.Getenv("TBX_QEMU_PROBE_HELPER") != "1" {
		return
	}
	listener, err := net.Listen("unix", os.Args[len(os.Args)-1])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	connection, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	encoder := json.NewEncoder(connection)
	decoder := json.NewDecoder(connection)
	if err := encoder.Encode(map[string]any{"QMP": map[string]any{"capabilities": []string{}}}); err != nil {
		t.Fatal(err)
	}
	for {
		var request qmpTestRequest
		if err := decoder.Decode(&request); err != nil {
			t.Fatal(err)
		}
		var result any = map[string]any{}
		switch request.Execute {
		case "query-version":
			result = map[string]any{"qemu": map[string]any{"major": 8, "minor": 2, "micro": 1}}
		case "query-machines":
			result = []map[string]any{{"name": "pc-q35-8.2", "alias": "q35"}}
		}
		if err := encoder.Encode(map[string]any{"return": result, "id": request.ID}); err != nil {
			t.Fatal(err)
		}
		if request.Execute == "quit" {
			return
		}
	}
}

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
	wantReason := "suspend requires QEMU >= 8.2 (found 6.2); upgrade QEMU"
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
