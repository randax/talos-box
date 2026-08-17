package provision

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// leaseCluster is a single control-plane cluster whose planned address is the
// start of the subnet, which is what cluster.json persists.
func leaseCluster(t *testing.T) cluster.Cluster {
	t.Helper()
	item, err := cluster.New("demo", 0, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.TalosVersion = "v1.13.6"
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel}
	return item
}

func talosconfigEndpoints(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	config, err := clientconfig.FromBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	context, ok := config.Contexts[config.Context]
	if !ok {
		t.Fatalf("talosconfig has no context %q", config.Context)
	}
	return context.Endpoints
}

// A node is free to hold a lease other than the address tbx planned for it:
// some hosts start the vmnet range mid-subnet, and applying a machine config
// takes a fresh lease. This pins what Reconcile hands the client — the observed
// lease for every call, and a talosconfig whose endpoints match it.
// TestSecureClientDialsObservedEndpoint pins that the client then dials it.
func TestReconcileRoutesObservedLeaseAddressesToEveryCall(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item := leaseCluster(t)
	planned := item.Nodes[0].IP
	leased := "172.30.0.25"
	if planned == leased {
		t.Fatalf("planned address %s must differ from the leased address", planned)
	}
	client := &fakeClient{kubeData: []byte("kubeconfig")}
	result, err := Reconcile(context.Background(), Request{Cluster: item, Client: client, PollInterval: time.Millisecond,
		Observe: func(context.Context) ([]Node, error) {
			return []Node{{Name: item.Nodes[0].Name, Role: cluster.RoleControlPlane, IP: leased, Phase: PhaseConfigured}}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	if len(client.scheduling) != 1 || client.scheduling[0].node != leased {
		t.Fatalf("scheduling calls = %+v, want one against %s", client.scheduling, leased)
	}
	if len(client.bootstrapNodes) != 1 || client.bootstrapNodes[0] != leased {
		t.Fatalf("bootstrap nodes = %v, want [%s]", client.bootstrapNodes, leased)
	}
	if len(client.kubeNodes) != 1 || client.kubeNodes[0] != leased {
		t.Fatalf("kubeconfig nodes = %v, want [%s]", client.kubeNodes, leased)
	}
	narration := strings.Join(result.Narration, "\n")
	if strings.Contains(narration, "--nodes "+planned+" ") {
		t.Fatalf("narration names the planned address %s:\n%s", planned, narration)
	}
	if !strings.Contains(narration, "--nodes "+leased+" ") {
		t.Fatalf("narration does not name the leased address %s:\n%s", leased, narration)
	}
	if endpoints := talosconfigEndpoints(t, result.TalosconfigPath); len(endpoints) != 1 || endpoints[0] != leased {
		t.Fatalf("talosconfig endpoints = %v, want [%s]", endpoints, leased)
	}
}

// The talosconfig is the only endpoint source the secure client and the user's
// own talosctl share, so it has to carry the observed leases too.
func TestReconcileWritesTalosconfigEndpointsFromObservedLeases(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item := leaseCluster(t)
	leased := "172.30.0.25"
	client := &fakeClient{kubeData: []byte("kubeconfig")}
	phases := []Phase{PhaseMaintenance, PhaseConfigured}
	call := 0
	result, err := Reconcile(context.Background(), Request{Cluster: item, Client: client, PollInterval: time.Millisecond,
		Observe: func(context.Context) ([]Node, error) {
			phase := phases[min(call, len(phases)-1)]
			call++
			return []Node{{Name: item.Nodes[0].Name, Role: cluster.RoleControlPlane, IP: leased, Phase: phase}}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	endpoints := talosconfigEndpoints(t, result.TalosconfigPath)
	if len(endpoints) != 1 || endpoints[0] != leased {
		t.Fatalf("talosconfig endpoints = %v, want [%s]", endpoints, leased)
	}
	if len(client.configs) == 0 {
		t.Fatal("no machine config was applied")
	}
	if !strings.Contains(string(client.configs[0]), "https://"+leased+":6443") {
		t.Fatalf("applied machine config does not point Kubernetes at the leased address %s", leased)
	}
}

// A node that already holds its machine config keeps whatever Kubernetes
// control-plane endpoint was baked in at apply time. When the control plane
// re-leases, that endpoint is dead: kubelets stop reaching the API and the
// cluster never converges, so the endpoint has to be reconciled over the
// authenticated API on every configured node — before bootstrap asks Talos for
// a kubeconfig derived from it.
func TestReconcileRepairsControlPlaneEndpointAfterLeaseChange(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("demo", 0, 1, 1, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.TalosVersion = "v1.13.6"
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel}
	controlPlane, worker := item.Nodes[0].Name, item.Nodes[1].Name
	observations := [][]Node{
		{
			{Name: controlPlane, Role: cluster.RoleControlPlane, IP: "172.30.0.25", Phase: PhaseMaintenance},
			{Name: worker, Role: cluster.RoleWorker, IP: "172.30.0.35", Phase: PhaseMaintenance},
		},
		{
			{Name: controlPlane, Role: cluster.RoleControlPlane, IP: "172.30.0.26", Phase: PhaseConfigured},
			{Name: worker, Role: cluster.RoleWorker, IP: "172.30.0.35", Phase: PhaseConfigured},
		},
	}
	call := 0
	client := &fakeClient{kubeData: []byte("kubeconfig"), endpointChanged: []bool{true, true}}
	result, err := Reconcile(context.Background(), Request{Cluster: item, Client: client, PollInterval: time.Millisecond,
		Observe: func(context.Context) ([]Node, error) {
			index := min(call, len(observations)-1)
			call++
			return observations[index], nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	want := "https://172.30.0.26:6443"
	if len(client.endpoints) != 2 {
		t.Fatalf("endpoint reconcile calls = %+v, want one per configured node", client.endpoints)
	}
	for _, call := range client.endpoints {
		if call.endpoint != want {
			t.Fatalf("endpoint reconcile %+v, want %s on every node", call, want)
		}
	}
	if got := []string{client.endpoints[0].node, client.endpoints[1].node}; got[0] != "172.30.0.26" || got[1] != "172.30.0.35" {
		t.Fatalf("endpoint reconcile nodes = %v, want the observed leases", got)
	}
	// A kubeconfig fetched before the endpoint is repaired names the dead
	// address, which only moves the hang from apid to the Kubernetes API.
	order := strings.Join(client.calls, ",")
	if !strings.Contains(order, "config,config") ||
		strings.Index(order, "config") > strings.Index(order, "bootstrap") ||
		strings.Index(order, "config") > strings.Index(order, "kubeconfig") {
		t.Fatalf("call order = %s, want the endpoint reconciled before bootstrap and kubeconfig", order)
	}
	narration := strings.Join(result.Narration, "\n")
	if !strings.Contains(narration, want) || !strings.Contains(narration, "patch mc --mode auto") {
		t.Fatalf("narration does not report the endpoint repair:\n%s", narration)
	}
}

// Generation keys on the control-plane addresses, so applying anything before
// every control plane holds a lease bakes a planned address that may never
// exist into its peers' configs — permanently, since a configured node is not
// re-applied.
func TestReconcileWaitsForEveryControlPlaneLeaseBeforeApplying(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("demo", 0, 3, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.TalosVersion = "v1.13.6"
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel}
	planned := item.Nodes[0].IP
	leases := []string{"172.30.0.25", "172.30.0.26", "172.30.0.27"}
	observation := func(firstIP string, phase Phase) []Node {
		nodes := []Node{{Name: item.Nodes[0].Name, Role: cluster.RoleControlPlane, IP: firstIP, Phase: phase}}
		for i := 1; i < 3; i++ {
			nodes = append(nodes, Node{Name: item.Nodes[i].Name, Role: cluster.RoleControlPlane, IP: leases[i], Phase: phase})
		}
		return nodes
	}
	observations := [][]Node{
		// The first control plane has no lease yet; its peers are ready to apply.
		observation("", PhaseMaintenance),
		observation(leases[0], PhaseMaintenance),
		observation(leases[0], PhaseConfigured),
	}
	call := 0
	client := &fakeClient{kubeData: []byte("kubeconfig")}
	if _, err := Reconcile(context.Background(), Request{Cluster: item, Client: client, PollInterval: time.Millisecond,
		Observe: func(context.Context) ([]Node, error) {
			index := min(call, len(observations)-1)
			call++
			return observations[index], nil
		}}); err != nil {
		t.Fatal(err)
	}
	if len(client.configs) != 3 {
		t.Fatalf("applied configs = %d, want one per node", len(client.configs))
	}
	for i, config := range client.configs {
		if strings.Contains(string(config), "https://"+planned+":6443") {
			t.Fatalf("config %d was applied with the planned control-plane endpoint %s", i, planned)
		}
		if !strings.Contains(string(config), "https://"+leases[0]+":6443") {
			t.Fatalf("config %d does not point Kubernetes at the leased control plane %s", i, leases[0])
		}
	}
}

// The apply itself makes the node take a fresh lease, so a later observation
// has to replace the address the earlier pass generated against.
func TestReconcileFollowsLeaseChangeAfterConfigApply(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item := leaseCluster(t)
	client := &fakeClient{kubeData: []byte("kubeconfig")}
	observations := [][]Node{
		{{Name: item.Nodes[0].Name, Role: cluster.RoleControlPlane, IP: "172.30.0.25", Phase: PhaseMaintenance}},
		{{Name: item.Nodes[0].Name, Role: cluster.RoleControlPlane, IP: "172.30.0.26", Phase: PhaseConfigured}},
	}
	call := 0
	result, err := Reconcile(context.Background(), Request{Cluster: item, Client: client, PollInterval: time.Millisecond,
		Observe: func(context.Context) ([]Node, error) {
			index := min(call, len(observations)-1)
			call++
			return observations[index], nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	if len(client.applied) != 1 || client.applied[0] != "172.30.0.25" {
		t.Fatalf("applied nodes = %v, want [172.30.0.25]", client.applied)
	}
	// The config was applied at the pre-apply lease, so the endpoint it carries
	// is stale the moment the node re-leases: it must be repaired, not left.
	if len(client.endpoints) != 1 || client.endpoints[0].endpoint != "https://172.30.0.26:6443" {
		t.Fatalf("endpoint reconcile calls = %+v, want the post-apply lease", client.endpoints)
	}
	if len(client.bootstrapNodes) != 1 || client.bootstrapNodes[0] != "172.30.0.26" {
		t.Fatalf("bootstrap nodes = %v, want the post-apply lease [172.30.0.26]", client.bootstrapNodes)
	}
	endpoints := talosconfigEndpoints(t, result.TalosconfigPath)
	if len(endpoints) != 1 || endpoints[0] != "172.30.0.26" {
		t.Fatalf("talosconfig endpoints = %v, want the post-apply lease [172.30.0.26]", endpoints)
	}
}

// secure() must dial the observed node exactly like the insecure apply path;
// trusting the talosconfig alone is what made bootstrap hang on a dead address.
func TestSecureClientDialsObservedEndpoint(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	generated, err := generateMachineConfigs(leaseCluster(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeTalosconfig(generated.paths.talosconfig, generated.talosconfig); err != nil {
		t.Fatal(err)
	}
	connection, err := MachineryClient{TalosconfigPath: generated.paths.talosconfig}.secure(context.Background(), "172.30.0.25")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	if endpoints := connection.GetEndpoints(); len(endpoints) != 1 || endpoints[0] != "172.30.0.25" {
		t.Fatalf("client endpoints = %v, want [172.30.0.25]", endpoints)
	}
}

// An expired provisioning budget used to surface as a bare deadline, hiding
// which address never answered.
func TestMachineConfigWithRetryNamesUnreachableEndpoint(t *testing.T) {
	unavailable := status.Error(codes.Unavailable, "i/o timeout")
	client := &fakeClient{schedulingErrs: []error{unavailable, unavailable, unavailable}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Millisecond)
	defer cancel()
	_, err := machineConfigWithRetry(ctx, client, "172.30.0.2", ConfigTarget{Endpoint: "https://172.30.0.2:6443", ControlPlaneScheduling: true}, time.Millisecond)
	if err == nil {
		t.Fatal("machineConfigWithRetry() succeeded, want a deadline")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("machineConfigWithRetry() error = %v, want a deadline", err)
	}
	if !strings.Contains(err.Error(), "172.30.0.2") || !strings.Contains(err.Error(), "i/o timeout") {
		t.Fatalf("machineConfigWithRetry() error = %v, want the unreachable endpoint and last error", err)
	}
}

func TestKubeconfigWithRetryNamesUnreachableEndpoint(t *testing.T) {
	unavailable := status.Error(codes.Unavailable, "i/o timeout")
	client := &fakeClient{kubeErrs: []error{unavailable, unavailable, unavailable}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Millisecond)
	defer cancel()
	_, err := kubeconfigWithRetry(ctx, client, "172.30.0.2", time.Millisecond)
	if err == nil {
		t.Fatal("kubeconfigWithRetry() succeeded, want a deadline")
	}
	if !strings.Contains(err.Error(), "172.30.0.2") || !strings.Contains(err.Error(), "i/o timeout") {
		t.Fatalf("kubeconfigWithRetry() error = %v, want the unreachable endpoint and last error", err)
	}
}

// Both repairs a running node can need have to ride on one read-patch-apply:
// a second read would see the config Talos had before the first apply landed
// and revert it, putting the dead endpoint back.
func TestReconcileRepairsEndpointAndSchedulingInOneApply(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item := leaseCluster(t)
	client := &fakeClient{kubeData: []byte("kubeconfig"), endpointChanged: []bool{true}, schedulingChanged: []bool{true}}
	result, err := Reconcile(context.Background(), Request{Cluster: item, Client: client, PollInterval: time.Millisecond,
		Observe: func(context.Context) ([]Node, error) {
			return []Node{{Name: item.Nodes[0].Name, Role: cluster.RoleControlPlane, IP: "172.30.0.25", Phase: PhaseConfigured}}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	got := 0
	for _, call := range client.calls {
		if call == "config" {
			got++
		}
	}
	if got != 1 {
		t.Fatalf("machine config reconciles = %d, want a single composed apply per node", got)
	}
	if len(client.endpoints) != 1 || len(client.scheduling) != 1 {
		t.Fatalf("endpoint calls = %+v, scheduling calls = %+v, want one composed call", client.endpoints, client.scheduling)
	}
	narration := strings.Join(result.Narration, "\n")
	if !strings.Contains(narration, `{"cluster":{"controlPlane":{"endpoint":"https://172.30.0.25:6443"}}}`) ||
		!strings.Contains(narration, `"allowSchedulingOnControlPlanes":true`) {
		t.Fatalf("narration does not report both repairs:\n%s", narration)
	}
}

// withConfigTarget is what makes the single apply carry both repairs.
func TestWithConfigTargetComposesBothRepairs(t *testing.T) {
	const config = `version: v1alpha1
machine:
  type: controlplane
  nodeLabels:
    node.kubernetes.io/exclude-from-external-load-balancers: ""
cluster:
  controlPlane:
    endpoint: https://172.30.0.25:6443
  allowSchedulingOnControlPlanes: false
`
	target := ConfigTarget{Endpoint: "https://172.30.0.26:6443", ControlPlaneScheduling: true, Workerless: true}
	patched, changes, err := withConfigTarget([]byte(config), target)
	if err != nil {
		t.Fatal(err)
	}
	if changes != (ConfigChanges{Endpoint: true, Scheduling: true}) {
		t.Fatalf("changes = %+v, want both", changes)
	}
	if !strings.Contains(string(patched), "https://172.30.0.26:6443") ||
		!strings.Contains(string(patched), "allowSchedulingOnControlPlanes: true") ||
		strings.Contains(string(patched), loadBalancerExclusionLabel) {
		t.Fatalf("composed patch lost a repair:\n%s", patched)
	}
	// A worker carries the endpoint but has no scheduling adaptations.
	_, changes, err = withConfigTarget([]byte(config), ConfigTarget{Endpoint: "https://172.30.0.26:6443"})
	if err != nil {
		t.Fatal(err)
	}
	if changes != (ConfigChanges{Endpoint: true}) {
		t.Fatalf("changes = %+v, want the endpoint only", changes)
	}
}

// A budget that expires while control planes have no lease must name them:
// "context deadline exceeded" alone tells nobody what tbx was waiting for.
func TestReconcileNamesControlPlanesWithoutALease(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item := leaseCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	_, err := Reconcile(ctx, Request{Cluster: item, Client: &fakeClient{}, PollInterval: time.Millisecond,
		Observe: func(context.Context) ([]Node, error) {
			return []Node{{Name: item.Nodes[0].Name, Role: cluster.RoleControlPlane}}, nil
		}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Reconcile() error = %v, want a deadline", err)
	}
	if !strings.Contains(err.Error(), item.Nodes[0].Name) || !strings.Contains(err.Error(), "DHCP lease") {
		t.Fatalf("Reconcile() error = %v, want the unleased control plane named", err)
	}
}

// Bootstrap runs right after config applies that can take apid away for a
// moment, so it gets the same tolerance as every other authenticated call.
func TestBootstrapWithRetryHandlesTransientTalosRestart(t *testing.T) {
	client := &fakeClient{bootErrs: []error{status.Error(codes.Unavailable, "apid restarting")}}
	if err := bootstrapWithRetry(context.Background(), client, "172.30.0.25", 0); err != nil {
		t.Fatal(err)
	}
	if client.bootstrap != 2 {
		t.Fatalf("bootstrap calls = %d, want a successful second call", client.bootstrap)
	}
}

func TestBootstrapWithRetryDoesNotHidePermanentErrors(t *testing.T) {
	client := &fakeClient{bootErrs: []error{status.Error(codes.PermissionDenied, "bad credential")}}
	err := bootstrapWithRetry(context.Background(), client, "172.30.0.25", 0)
	if err == nil || !strings.Contains(err.Error(), "bad credential") {
		t.Fatalf("bootstrapWithRetry() error = %v, want permanent error", err)
	}
	if client.bootstrap != 1 {
		t.Fatalf("bootstrap calls = %d, want 1", client.bootstrap)
	}
}

func TestBootstrapWithRetryTreatsAlreadyBootstrappedAsSuccess(t *testing.T) {
	client := &fakeClient{bootErr: status.Error(codes.AlreadyExists, "already bootstrapped")}
	if err := bootstrapWithRetry(context.Background(), client, "172.30.0.25", 0); err != nil {
		t.Fatal(err)
	}
	if client.bootstrap != 1 {
		t.Fatalf("bootstrap calls = %d, want 1", client.bootstrap)
	}
}

func TestWithControlPlaneEndpointRepointsOnlyWhenItDrifted(t *testing.T) {
	const config = `version: v1alpha1
machine:
  type: controlplane
cluster:
  controlPlane:
    endpoint: https://172.30.0.25:6443
`
	patched, changed, err := withControlPlaneEndpoint([]byte(config), "https://172.30.0.25:6443")
	if err != nil {
		t.Fatal(err)
	}
	if changed || string(patched) != config {
		t.Fatalf("converged config was rewritten (changed = %t):\n%s", changed, patched)
	}
	trailing := "---\napiVersion: v1alpha1\nkind: RegistryMirrorConfig\nname: \"*\"\n"
	patched, changed, err = withControlPlaneEndpoint([]byte(config+trailing), "https://172.30.0.26:6443")
	if err != nil {
		t.Fatal(err)
	}
	if !changed || !strings.Contains(string(patched), "https://172.30.0.26:6443") {
		t.Fatalf("endpoint was not repointed (changed = %t):\n%s", changed, patched)
	}
	if !strings.HasSuffix(string(patched), trailing) || strings.Count(string(patched), "kind: RegistryMirrorConfig") != 1 {
		t.Fatalf("trailing document was not preserved:\n%s", patched)
	}
	if _, _, err := withControlPlaneEndpoint([]byte("cluster: [unterminated"), "https://172.30.0.26:6443"); err == nil {
		t.Fatal("withControlPlaneEndpoint() accepted malformed YAML")
	}
}

// The endpoint reconcile has to be spelled the same way generation is, or every
// pass would see drift and re-apply a config that is already correct.
func TestGeneratedConfigMatchesTheReconciledEndpoint(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item := leaseCluster(t)
	generated, err := generateMachineConfigs(item)
	if err != nil {
		t.Fatal(err)
	}
	_, changed, err := withControlPlaneEndpoint(generated.configs[cluster.RoleControlPlane], kubernetesEndpoint(item.Nodes[0].IP))
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("a freshly generated config already reads as drifted from its own endpoint")
	}
}

func TestClusterWithObservedAddressesKeepsPlannedAddressWithoutLease(t *testing.T) {
	item := leaseCluster(t)
	planned := item.Nodes[0].IP
	observed := clusterWithObservedAddresses(item, []Node{{Name: item.Nodes[0].Name, Role: cluster.RoleControlPlane}})
	if observed.Nodes[0].IP != planned {
		t.Fatalf("address = %s, want the planned %s when no lease is observed", observed.Nodes[0].IP, planned)
	}
	if item.Nodes[0].IP != planned {
		t.Fatalf("the persisted cluster was mutated: %s", item.Nodes[0].IP)
	}
}
