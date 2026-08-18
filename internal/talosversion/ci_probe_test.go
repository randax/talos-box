package talosversion

import (
	"errors"
	"os/exec"
	"testing"
)

// The maintenance-API readiness probe in the KVM e2e harness has to accept both
// ends of the supported version window: Talos v1.13 answers
// MachineService/Version in maintenance mode, v1.12 rejects it as Unimplemented.
// Classifying the reply is the whole of that decision, so it lives in one shell
// function and is exercised here directly — no node, no network.
func TestMaintenanceAPIReplyClassification(t *testing.T) {
	t.Parallel()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash unavailable: %v", err)
	}
	tests := []struct {
		name  string
		reply string
		ready bool
	}{
		{
			name:  "v1.12 maintenance rejection",
			reply: `rpc error: code = Unimplemented desc = API is not implemented in maintenance mode`,
			ready: true,
		},
		{
			name:  "unimplemented method",
			reply: `error getting version: 1 error occurred:` + "\n" + `* rpc error: code = Unimplemented desc = unknown method Version for service machine.MachineService`,
			ready: true,
		},
		{
			name:  "connection refused",
			reply: `error getting version: rpc error: code = Unavailable desc = connection error: desc = "transport: Error while dialing: dial tcp 172.30.0.2:50000: connect: connection refused"`,
			ready: false,
		},
		{
			name:  "dial timeout",
			reply: `rpc error: code = DeadlineExceeded desc = context deadline exceeded`,
			ready: false,
		},
		{
			name:  "no route to host",
			reply: `transport: Error while dialing: dial tcp 172.30.0.2:50000: connect: no route to host`,
			ready: false,
		},
		{
			name:  "empty output",
			reply: "",
			ready: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			script := `set -Eeuo pipefail
root=$(pwd)
required_bytes=0
source ../../scripts/ci/kvm-e2e-lib.sh
maintenance_api_reply_ready "$1"`
			cmd := exec.Command(bash, "-c", script, "bash", test.reply)
			runErr := cmd.Run()
			ready := runErr == nil
			var exit *exec.ExitError
			if runErr != nil && !errors.As(runErr, &exit) {
				t.Fatalf("run classifier: %v", runErr)
			}
			if ready != test.ready {
				t.Errorf("maintenance_api_reply_ready(%q) = %v, want %v", test.reply, ready, test.ready)
			}
		})
	}
}
