package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/provision"
)

// destroyBootedRequest destroys the cluster bootWaitCreateRequest makes, past
// the confirmation gate: this is a race test, not a gate test.
func destroyBootedRequest() Request {
	return Request{Op: "cluster.destroy", Args: json.RawMessage(`{"name":"booted","force":true}`)}
}

// The boot wait used to run outside every lock, so a destroy could delete the
// cluster while the create was still waiting for its nodes — and the create
// would then print a past-tense success for a cluster that no longer existed.
// The wait now runs under the cluster's mutation lock, and the destroy queued
// behind it interrupts the wait instead of sitting out its budget.
func TestCreateHoldsTheClusterAgainstADestroyDuringItsBootWait(t *testing.T) {
	service := newBootWaitCreateServer(t)
	// a real budget: only the destroy may end this wait
	restoreInterval(t, time.Millisecond, time.Minute)
	request := bootWaitCreateRequest(t)

	probing := make(chan struct{})
	var first sync.Once
	var mu sync.Mutex
	var vanished bool
	service.nodeProbe = func(string) ProbeResult {
		first.Do(func() {
			close(probing)
			// Hold this one probe open. An unserialized destroy deletes the
			// cluster inside exactly this window — cancelling the wait does not
			// stop the probe already in flight — and the check below sees it.
			time.Sleep(time.Second)
		})
		// The invariant under test: the cluster the create is waiting on must
		// still be there for the whole wait.
		if _, err := cluster.Load("booted"); err != nil {
			mu.Lock()
			vanished = true
			mu.Unlock()
		}
		return ProbeResult{}
	}

	created := make(chan Response, 1)
	go func() { created <- service.dispatchProvisioning(request, nil) }()
	select {
	case <-probing:
	case <-time.After(10 * time.Second):
		t.Fatal("the create never reached its boot wait")
	}

	destroyed := make(chan Response, 1)
	go func() { destroyed <- service.dispatch(destroyBootedRequest()) }()

	var createResponse Response
	select {
	case createResponse = <-created:
	case <-time.After(10 * time.Second):
		t.Fatal("the create held its boot wait after a destroy queued behind it")
	}
	mu.Lock()
	deletedMidWait := vanished
	mu.Unlock()
	if deletedMidWait {
		t.Fatal("the cluster was destroyed while the create was still waiting for it to boot")
	}
	if !createResponse.OK {
		t.Fatalf("cluster.create failed: %s", createResponse.Error)
	}
	var summary ClusterSummary
	if err := json.Unmarshal(createResponse.Data, &summary); err != nil {
		t.Fatal(err)
	}
	// The create did make the cluster, so it succeeds — but it must not report
	// an unqualified success for one another operation is about to take.
	if !strings.Contains(summary.Warning, "another operation on the cluster interrupted the wait") {
		t.Fatalf("summary warning = %q, want the interrupted wait reported", summary.Warning)
	}

	select {
	case response := <-destroyed:
		if !response.OK {
			t.Fatalf("cluster.destroy failed: %s", response.Error)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the destroy never got the cluster the create was holding")
	}
	if _, err := cluster.Load("booted"); err == nil {
		t.Fatal("the destroy reported success but the cluster is still there")
	}
}

// The other half of the same handover: a provisioned create keeps its reconcile
// on the request path, and a destroy queued behind it must not wait out a
// provisioning budget measured in minutes for a cluster nobody wants.
func TestDestroyDoesNotWaitOutTheReconcileOfTheCreateItRaced(t *testing.T) {
	service := newBootWaitCreateServer(t)
	restoreInterval(t, time.Millisecond, time.Minute)
	request := bootWaitCreateRequest(t)
	var args createArgs
	if err := json.Unmarshal(request.Args, &args); err != nil {
		t.Fatal(err)
	}
	args.CNI = string(cluster.CNIFlannel)
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	request.Args = raw

	// A reconcile that runs until it is told to stop: without the handover the
	// destroy would queue behind the whole provisioning budget.
	reconciling := make(chan struct{})
	var once sync.Once
	service.provisionReconcile = func(ctx context.Context, _ provision.Request) (provision.Result, error) {
		once.Do(func() { close(reconciling) })
		<-ctx.Done()
		return provision.Result{}, ctx.Err()
	}
	// The node answers at once, so the boot wait is not what this test measures.
	service.nodeProbe = func(string) ProbeResult {
		return ProbeResult{Dialed: true, TLS: true, MaintenanceCert: true}
	}

	created := make(chan Response, 1)
	go func() { created <- service.dispatchProvisioning(request, nil) }()
	select {
	case <-reconciling:
	case <-time.After(10 * time.Second):
		t.Fatal("the create never reached its reconcile")
	}

	destroyed := make(chan Response, 1)
	go func() { destroyed <- service.dispatch(destroyBootedRequest()) }()
	select {
	case response := <-destroyed:
		if !response.OK {
			t.Fatalf("cluster.destroy failed: %s", response.Error)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the destroy waited out the reconcile of the create it raced")
	}
	select {
	case <-created:
	case <-time.After(10 * time.Second):
		t.Fatal("the create never answered after its reconcile was handed over")
	}
}
