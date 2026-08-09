package hypervisor

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestQEMUProcessHelper(t *testing.T) {
	if os.Getenv("TBX_QEMU_PROCESS_HELPER") != "1" {
		return
	}
	os.Exit(0)
}

func TestQEMUProcessDeathChangesActive(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=^TestQEMUProcessHelper$")
	command.Env = append(os.Environ(), "TBX_QEMU_PROCESS_HELPER=1")
	process, err := startQEMUProcess(command)
	if err != nil {
		t.Fatal(err)
	}
	if !process.active() {
		t.Fatal("process was inactive immediately after Start")
	}
	select {
	case <-process.done:
	case <-time.After(2 * time.Second):
		t.Fatal("process exit was not supervised")
	}
	if process.active() {
		t.Fatal("process remained active after child exit")
	}
	if err := process.waitError(); err != nil {
		t.Fatalf("waitError() = %v, want nil", err)
	}
}
