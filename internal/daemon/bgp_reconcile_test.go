package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/provision"
)

var errNoReconcile = errors.New("cilium apply refused")

// recordingBGPHelper answers the host-side speaker calls and remembers them, so
// a test can tell the host half of a mode change from the cluster half.
type recordingBGPHelper struct {
	enabled  []string
	disabled []string
	active   bool
}

func (c *recordingBGPHelper) EnableBGP(name string, _ int, _, _ uint32) error {
	c.enabled = append(c.enabled, name)
	c.active = true
	return nil
}

func (c *recordingBGPHelper) DisableBGP(name string) error {
	c.disabled = append(c.disabled, name)
	c.active = false
	return nil
}

func (c *recordingBGPHelper) HasBGP(string) (bool, error) { return c.active, nil }
func (c *recordingBGPHelper) Close() error                { return nil }

// runningCiliumClusterForBGP arranges the exact situation #344 was reported
// from: a live, fully converged Cilium cluster with a load balancer, whose
// announcement mode is about to change. Every convergence probe passes, so an
// unforced reconcile would take the fast no-op path and change nothing.
func runningCiliumClusterForBGP(t *testing.T, bgp bool) (*Server, cluster.Cluster, *recordingBGPHelper) {
	t.Helper()
	service, item := runningLonghornClusterForNodeMutation(t, 1, 1)
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true, BGP: bgp}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	convergedClusterForFastNoop(t, item.Name)
	originalCilium, originalVIP := ciliumConvergenceProbe, loadBalancerVIPProbe
	t.Cleanup(func() {
		ciliumConvergenceProbe = originalCilium
		loadBalancerVIPProbe = originalVIP
	})
	ciliumConvergenceProbe = func(context.Context, []byte, cluster.Cluster) error { return nil }
	loadBalancerVIPProbe = func(cluster.Cluster) (string, bool) { return "172.30.0.200", true }

	client := &recordingBGPHelper{active: bgp}
	originalConnect := connectBGPHelper
	connectBGPHelper = func() (bgpHelperClient, error) { return client, nil }
	t.Cleanup(func() { connectBGPHelper = originalConnect })
	if !service.provisioningComplete(item) {
		t.Fatal("test arrangement is not a converged cluster, so it cannot show the fast no-op being forced past")
	}
	return service, item, client
}

func dispatchBGPRequest(t *testing.T, service *Server, op, clusterName string) Response {
	t.Helper()
	raw, err := json.Marshal(nameArgs{Name: clusterName})
	if err != nil {
		t.Fatal(err)
	}
	return service.dispatch(Request{Op: op, Args: raw})
}

// recordCiliumReconciles captures the cluster every provisioning pass renders
// from. The intent it carries is what decides bgpControlPlane, the BGP CRDs and
// the announcement objects, so it is the whole cluster-side contract of a mode
// change.
func recordCiliumReconciles(service *Server) *[]cluster.Cluster {
	reconciled := &[]cluster.Cluster{}
	service.provisionReconcile = func(_ context.Context, request provision.Request) (provision.Result, error) {
		*reconciled = append(*reconciled, request.Cluster)
		return provision.Result{}, nil
	}
	return reconciled
}

// The host speaker alone leaves the cluster announcing over L2 with the BGP
// control plane still disabled — the silent no-op #344 reported. Enabling must
// re-render Cilium from the new intent before it reports success.
func TestBGPEnableReconcilesCiliumWithTheControlPlaneEnabled(t *testing.T) {
	service, item, client := runningCiliumClusterForBGP(t, false)
	reconciled := recordCiliumReconciles(service)

	response := dispatchBGPRequest(t, service, "bgp.enable", item.Name)
	if !response.OK {
		t.Fatalf("bgp.enable failed: %s", response.Error)
	}

	if len(*reconciled) != 1 {
		t.Fatalf("Cilium reconciles after bgp.enable = %d, want 1", len(*reconciled))
	}
	reconciledItem := (*reconciled)[0]
	if reconciledItem.CNI != cluster.CNICilium || !reconciledItem.BGP || !reconciledItem.LB {
		t.Fatalf("reconciled intent = %+v, want cilium with lb and bgp", reconciledItem.ProvisioningIntent)
	}
	if strings.Join(client.enabled, ",") != item.Name {
		t.Fatalf("host speaker enables = %q, want %q", client.enabled, item.Name)
	}
	var summary ClusterSummary
	if err := json.Unmarshal(response.Data, &summary); err != nil {
		t.Fatal(err)
	}
	if !summary.BGP || len(summary.Warnings) != 0 {
		t.Fatalf("bgp.enable summary = %+v, want bgp with no deferral warning", summary)
	}
}

// Disabling is the same contract in reverse: Cilium is re-rendered without the
// control plane, which is what removes the BGP objects and restores L2.
func TestBGPDisableReconcilesCiliumWithTheControlPlaneDisabled(t *testing.T) {
	service, item, client := runningCiliumClusterForBGP(t, true)
	reconciled := recordCiliumReconciles(service)

	response := dispatchBGPRequest(t, service, "bgp.disable", item.Name)
	if !response.OK {
		t.Fatalf("bgp.disable failed: %s", response.Error)
	}

	if len(*reconciled) != 1 {
		t.Fatalf("Cilium reconciles after bgp.disable = %d, want 1", len(*reconciled))
	}
	if reconciledItem := (*reconciled)[0]; reconciledItem.BGP {
		t.Fatalf("reconciled intent = %+v, want the BGP control plane off", reconciledItem.ProvisioningIntent)
	}
	if strings.Join(client.disabled, ",") != item.Name {
		t.Fatalf("host speaker disables = %q, want %q", client.disabled, item.Name)
	}
}

// The reconcile runs on the request path: the verb's whole purpose is the
// cluster-side change, so a failure must fail the verb rather than reach the
// operator as a success line and a log entry (#344).
func TestBGPEnableFailsTheRequestWhenTheReconcileFails(t *testing.T) {
	service, item, _ := runningCiliumClusterForBGP(t, false)
	service.provisionReconcile = func(context.Context, provision.Request) (provision.Result, error) {
		return provision.Result{}, errNoReconcile
	}

	response := dispatchBGPRequest(t, service, "bgp.enable", item.Name)
	if response.OK {
		t.Fatal("bgp.enable reported success over a failed Cilium reconcile")
	}
	if !strings.Contains(response.Error, "cilium apply refused") {
		t.Fatalf("bgp.enable error = %q, want the reconcile failure", response.Error)
	}
	// The mode is still recorded: the host speaker and the persisted intent
	// changed before the reconcile, and a rerun must resume from there.
	reloaded, err := cluster.Load(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.BGP {
		t.Fatal("failed reconcile also discarded the recorded BGP mode")
	}
}

// A reconcile can only converge over a fully-running cluster, so a stopped one
// records the mode and says plainly that Cilium still runs the old one.
func TestBGPEnableOnAStoppedClusterWarnsInsteadOfClaimingItIsInEffect(t *testing.T) {
	service, item, _ := runningCiliumClusterForBGP(t, false)
	reconciled := recordCiliumReconciles(service)
	for _, node := range item.Nodes {
		machine, ok := service.vms[item.Name][node.Name].(*fakeMachine)
		if !ok {
			t.Fatalf("node %s is not a fake machine", node.Name)
		}
		machine.active = false
	}

	response := dispatchBGPRequest(t, service, "bgp.enable", item.Name)
	if !response.OK {
		t.Fatalf("bgp.enable failed: %s", response.Error)
	}
	if len(*reconciled) != 0 {
		t.Fatalf("Cilium reconciles over a stopped cluster = %d, want 0", len(*reconciled))
	}
	var summary ClusterSummary
	if err := json.Unmarshal(response.Data, &summary); err != nil {
		t.Fatal(err)
	}
	warnings := strings.Join(summary.Warnings, "\n")
	for _, want := range []string{"stopped", "bgp"} {
		if !strings.Contains(strings.ToLower(warnings), want) {
			t.Fatalf("bgp.enable warnings = %q, want a deferral note mentioning %q", warnings, want)
		}
	}
}

// The reconcile is the longest stretch of the verb, and the host speaker step
// precedes it. Both are narrated so the operator sees which half is running
// (#273 #344).
func TestBGPEnableNarratesTheSpeakerAndTheReconcile(t *testing.T) {
	service, item, _ := runningCiliumClusterForBGP(t, false)
	recordCiliumReconciles(service)
	progress, stages := recordStages()

	raw, err := json.Marshal(nameArgs{Name: item.Name})
	if err != nil {
		t.Fatal(err)
	}
	response := service.dispatchWithProgress(Request{Op: "bgp.enable", Args: raw}, progress)
	if !response.OK {
		t.Fatalf("bgp.enable failed: %s", response.Error)
	}

	speaker := indexOfStage(*stages, "host BGP speaker")
	reconcile := indexOfStage(*stages, "reconciling cilium on cluster demo")
	if speaker < 0 || reconcile < 0 || speaker > reconcile {
		t.Fatalf("bgp.enable narration = %q, want the speaker stage before the reconcile", *stages)
	}
}

// A mode change re-renders Cilium and nothing else. Dragging the storage chart
// and its write/readback probe into the forced pass made `bgp enable` fail on
// unrelated storage faults and hold the request for the storage budget, while
// the memo storage had already established stayed perfectly good (#344).
func TestBGPEnableLeavesStorageAlone(t *testing.T) {
	service, item, _ := runningCiliumClusterForBGP(t, false)
	service.storagePhases[item.Name] = StoragePhaseLive
	var requests []provision.Request
	service.provisionReconcile = func(_ context.Context, request provision.Request) (provision.Result, error) {
		requests = append(requests, request)
		return provision.Result{}, nil
	}
	storageProbes := 0
	originalStorage := storageConvergenceProbe
	t.Cleanup(func() { storageConvergenceProbe = originalStorage })
	storageConvergenceProbe = func(context.Context, cluster.Cluster, []byte) error {
		storageProbes++
		return nil
	}

	response := dispatchBGPRequest(t, service, "bgp.enable", item.Name)
	if !response.OK {
		t.Fatalf("bgp.enable failed: %s", response.Error)
	}

	if len(requests) != 1 {
		t.Fatalf("reconciles after bgp.enable = %d, want 1", len(requests))
	}
	if !requests[0].SkipStorage || requests[0].Storage != nil {
		t.Fatalf("bgp.enable reconcile = %+v, want the storage stage skipped", requests[0])
	}
	if storageProbes != 0 {
		t.Fatalf("bgp.enable ran the storage probe %d time(s), want none while storage is already live", storageProbes)
	}
	if service.storagePhases[item.Name] != StoragePhaseLive {
		t.Fatalf("storage phase after bgp.enable = %q, want it untouched at %q", service.storagePhases[item.Name], StoragePhaseLive)
	}
}

// Registering the BGP pass cancels whatever provision was already running, so
// a scoped pass would strand an in-flight storage install: nothing re-drives it
// and the memo it never reached stays as it was. When something is genuinely in
// flight the BGP pass therefore takes the full scope and carries the storage
// stage itself.
func TestBGPEnableDuringAnActiveProvisionKeepsStorageInScope(t *testing.T) {
	service, item, _ := runningCiliumClusterForBGP(t, false)
	var requests []provision.Request
	service.provisionReconcile = func(_ context.Context, request provision.Request) (provision.Result, error) {
		requests = append(requests, request)
		return provision.Result{}, nil
	}
	// A `tbx up` pass mid storage-install: registered, cancellable, nothing
	// waiting on it yet.
	cancelled := false
	service.opMu.Lock()
	if service.provisions == nil {
		service.provisions = make(map[string]activeProvision)
	}
	service.provisions[item.Name] = activeProvision{
		generation: 1,
		cancel:     func() { cancelled = true },
		done:       nil,
	}
	service.opMu.Unlock()

	response := dispatchBGPRequest(t, service, "bgp.enable", item.Name)
	if !response.OK {
		t.Fatalf("bgp.enable failed: %s", response.Error)
	}

	if !cancelled {
		t.Fatal("bgp.enable left the in-flight provision running, so this test proves nothing")
	}
	if len(requests) != 1 {
		t.Fatalf("reconciles after bgp.enable = %d, want 1", len(requests))
	}
	if requests[0].SkipStorage {
		t.Fatalf("bgp.enable reconcile = %+v, want the storage stage back in scope after cancelling an active provision", requests[0])
	}
}

// Disabling on a cluster whose Kubernetes side never announced over BGP is a
// host-side withdrawal and nothing else: there are no BGP objects to remove, so
// forcing a full reconcile only exposes the verb to unrelated failures.
func TestBGPDisableWithoutAClusterSideToUndoSkipsTheReconcile(t *testing.T) {
	tests := []struct {
		name string
		cni  cluster.CNI
		bgp  bool
	}{
		{name: "flannel cluster carrying legacy bgp state", cni: cluster.CNIFlannel, bgp: true},
		{name: "cilium cluster that never enabled bgp", cni: cluster.CNICilium, bgp: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, item := runningLonghornClusterForNodeMutation(t, 1, 1)
			item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: test.cni, LB: true, BGP: test.bgp}
			if err := cluster.Save(item); err != nil {
				t.Fatal(err)
			}
			client := &recordingBGPHelper{active: test.bgp}
			originalConnect := connectBGPHelper
			connectBGPHelper = func() (bgpHelperClient, error) { return client, nil }
			t.Cleanup(func() { connectBGPHelper = originalConnect })
			reconciled := recordCiliumReconciles(service)

			response := dispatchBGPRequest(t, service, "bgp.disable", item.Name)
			if !response.OK {
				t.Fatalf("bgp.disable failed: %s", response.Error)
			}

			if len(*reconciled) != 0 {
				t.Fatalf("reconciles after bgp.disable = %d, want none", len(*reconciled))
			}
			if strings.Join(client.disabled, ",") != item.Name {
				t.Fatalf("host speaker disables = %q, want %q", client.disabled, item.Name)
			}
			saved, err := cluster.Load(item.Name)
			if err != nil {
				t.Fatal(err)
			}
			if saved.BGP {
				t.Fatal("bgp.disable did not record the mode change")
			}
		})
	}
}
