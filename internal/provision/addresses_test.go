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
// takes a fresh lease. Every authenticated call has to follow the observed
// lease, or provisioning dials an address nothing answers on.
func TestReconcileDialsObservedLeaseAddresses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item := leaseCluster(t)
	planned := item.Nodes[0].IP
	leased := "172.30.0.25"
	if planned == leased {
		t.Fatalf("planned address %s must differ from the leased address", planned)
	}
	client := &fakeClient{kubeData: []byte("kubeconfig")}
	result, err := Reconcile(context.Background(), Request{Cluster: item, Client: client, PollInterval: 0,
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
	result, err := Reconcile(context.Background(), Request{Cluster: item, Client: client, PollInterval: 0,
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
	result, err := Reconcile(context.Background(), Request{Cluster: item, Client: client, PollInterval: 0,
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
func TestSchedulingWithRetryNamesUnreachableEndpoint(t *testing.T) {
	unavailable := status.Error(codes.Unavailable, "i/o timeout")
	client := &fakeClient{schedulingErrs: []error{unavailable, unavailable, unavailable}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Millisecond)
	defer cancel()
	_, err := schedulingWithRetry(ctx, client, "172.30.0.2", true, time.Millisecond)
	if err == nil {
		t.Fatal("schedulingWithRetry() succeeded, want a deadline")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("schedulingWithRetry() error = %v, want a deadline", err)
	}
	if !strings.Contains(err.Error(), "172.30.0.2") || !strings.Contains(err.Error(), "i/o timeout") {
		t.Fatalf("schedulingWithRetry() error = %v, want the unreachable endpoint and last error", err)
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
