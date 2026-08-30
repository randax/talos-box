package daemon

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
)

// node.remove stops an active VM, so it holds the balloon latch too (#513).
func TestRemoveNodeQuiescesBalloonWhileStopping(t *testing.T) {
	var quiescedAtClose atomic.Bool
	worker := &fakeMachine{active: true}
	service, item := teardownTestServer(t, &fakeMachine{active: true}, worker)
	service.mutationLocks = map[string]*sync.Mutex{}
	worker.onClose = func() { quiescedAtClose.Store(service.balloonQuiesced()) }
	raw, _ := json.Marshal(nodeArgs{Cluster: item.Name, Name: item.Nodes[1].Name})
	if _, _, err := service.removeNodeLocked(raw, stageFunc(func(string) {})); err != nil {
		t.Fatal(err)
	}
	if !quiescedAtClose.Load() {
		t.Fatal("node.remove closed the VM without the balloon latch held")
	}
	if service.balloonQuiesced() {
		t.Fatal("balloon latch leaked after node.remove")
	}
}
