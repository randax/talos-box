package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hypervisor"
)

// A status queued behind the operation lock must say so: the client's silence
// bound is re-armed by that line, so a busy daemon is not mistaken for a
// stalled one.
func TestDispatchStatusNarratesOperationLockWait(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := cluster.Save(cluster.Cluster{Name: "demo", SubnetIndex: 1}); err != nil {
		t.Fatal(err)
	}
	service := &Server{
		hypervisors:  singleFakeRegistry(&fakeHypervisor{}),
		vms:          map[string]map[string]hypervisor.Machine{},
		nodeIPLookup: func(string, int) string { return "" },
		nodeProbe:    func(string) ProbeResult { return ProbeResult{} },
	}
	service.opMu.Lock()
	stages := make(chan string, 8)
	done := make(chan Response, 1)
	go func() {
		done <- service.dispatchStatus(Request{Op: "status", Args: json.RawMessage(`{"cluster":"demo"}`)}, func(stage string) {
			stages <- stage
		})
	}()
	select {
	case stage := <-stages:
		if !strings.Contains(stage, "waiting for the daemon's current operation") {
			t.Fatalf("first stage = %q, want the lock wait", stage)
		}
	case <-time.After(5 * time.Second):
		service.opMu.Unlock()
		t.Fatal("status queued behind the operation lock narrated nothing")
	}
	service.opMu.Unlock()
	select {
	case response := <-done:
		if !response.OK {
			t.Fatalf("status failed after the lock was released: %s", response.Error)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("status did not finish after the lock was released")
	}
}
