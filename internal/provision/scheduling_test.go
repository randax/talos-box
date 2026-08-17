package provision

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"go.yaml.in/yaml/v4"
)

const workerlessMachineConfig = `version: v1alpha1
machine:
  type: controlplane
  nodeLabels:
    other: keep
cluster:
  allowSchedulingOnControlPlanes: true
`

const workerMachineConfig = `version: v1alpha1
machine:
  type: controlplane
  nodeLabels:
    other: keep
    node.kubernetes.io/exclude-from-external-load-balancers: ""
cluster:
  allowSchedulingOnControlPlanes: false
`

func TestWithControlPlaneSchedulingReconcilesBothDirections(t *testing.T) {
	tests := []struct {
		name       string
		config     string
		workerless bool
		changed    bool
	}{
		{name: "gain worker-less adaptations", config: workerMachineConfig, workerless: true, changed: true},
		{name: "drop worker-less adaptations", config: workerlessMachineConfig, workerless: false, changed: true},
		{name: "already worker-less", config: workerlessMachineConfig, workerless: true},
		{name: "already has workers", config: workerMachineConfig, workerless: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			patched, changed, err := withControlPlaneScheduling([]byte(test.config), test.workerless)
			if err != nil {
				t.Fatal(err)
			}
			if changed != test.changed {
				t.Fatalf("changed = %t, want %t", changed, test.changed)
			}
			if !changed && string(patched) != test.config {
				t.Fatalf("converged config was rewritten:\n%s", patched)
			}
			var document map[string]any
			if err := yaml.Unmarshal(patched, &document); err != nil {
				t.Fatal(err)
			}
			clusterSection, _ := document["cluster"].(map[string]any)
			if got, _ := clusterSection["allowSchedulingOnControlPlanes"].(bool); got != test.workerless {
				t.Fatalf("allowSchedulingOnControlPlanes = %v, want %t", clusterSection["allowSchedulingOnControlPlanes"], test.workerless)
			}
			machineSection, _ := document["machine"].(map[string]any)
			nodeLabels, _ := machineSection["nodeLabels"].(map[string]any)
			_, excluded := nodeLabels[loadBalancerExclusionLabel]
			if excluded == test.workerless {
				t.Fatalf("exclusion label present = %t with workerless = %t", excluded, test.workerless)
			}
			if got, _ := nodeLabels["other"].(string); got != "keep" {
				t.Fatalf("unrelated node label = %v, want keep", nodeLabels["other"])
			}
		})
	}
}

func TestWithControlPlaneSchedulingKeepsTrailingDocuments(t *testing.T) {
	trailing := "---\napiVersion: v1alpha1\nkind: RegistryMirrorConfig\nname: \"*\"\n"
	patched, changed, err := withControlPlaneScheduling([]byte(workerMachineConfig+trailing), true)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("worker-less adaptations were not applied")
	}
	if !strings.HasSuffix(string(patched), trailing) {
		t.Fatalf("trailing document was not preserved:\n%s", patched)
	}
	if strings.Count(string(patched), "kind: RegistryMirrorConfig") != 1 {
		t.Fatalf("trailing document was duplicated:\n%s", patched)
	}
}

func TestWithControlPlaneSchedulingRejectsMalformedConfig(t *testing.T) {
	if _, _, err := withControlPlaneScheduling([]byte("machine: [unterminated"), true); err == nil {
		t.Fatal("withControlPlaneScheduling() accepted malformed YAML")
	}
}

func schedulingRequest(t *testing.T, workers int, client *fakeClient) (cluster.Cluster, Request) {
	t.Helper()
	item, err := cluster.New("demo", 0, 1, workers, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.TalosVersion = "v1.13.6"
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel}
	nodes := make([]Node, 0, len(item.Nodes))
	for _, node := range item.Nodes {
		nodes = append(nodes, Node{Name: node.Name, Role: node.Role, IP: node.IP, Phase: PhaseConfigured})
	}
	return item, Request{Cluster: item, Client: client, PollInterval: 0, Observe: func(context.Context) ([]Node, error) {
		return nodes, nil
	}}
}

func TestReconcileMakesConfiguredControlPlaneSchedulableWhenWorkerless(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	client := &fakeClient{kubeData: []byte("kubeconfig"), schedulingChanged: []bool{true}}
	item, request := schedulingRequest(t, 0, client)
	result, err := Reconcile(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(client.scheduling) != 1 || client.scheduling[0] != (schedulingCall{node: item.Nodes[0].IP, workerless: true}) {
		t.Fatalf("scheduling calls = %+v, want one worker-less call for %s", client.scheduling, item.Nodes[0].IP)
	}
	narration := strings.Join(result.Narration, "\n")
	// The narrated equivalent has to reproduce the whole change (scheduling flag
	// and load-balancer exclusion label) and nothing else: a whole-config apply
	// would also clear the per-node hostname tbx injects, renaming the
	// Kubernetes node the reconcile just converged.
	if !strings.Contains(narration, "talosctl patch mc --mode auto") || !strings.Contains(narration, item.Nodes[0].IP) {
		t.Fatalf("narration missing machine config patch:\n%s", narration)
	}
	if strings.Contains(narration, "apply-config --mode auto") || strings.Contains(narration, "--file controlplane.yaml") {
		t.Fatalf("narration applies a whole config to a running node:\n%s", narration)
	}
	if !strings.Contains(narration, `"allowSchedulingOnControlPlanes":true`) || !strings.Contains(narration, `{"$patch":"delete"}`) {
		t.Fatalf("narrated patch does not move both worker-less adaptations:\n%s", narration)
	}
}

func TestControlPlaneSchedulingPatchMovesBothAdaptations(t *testing.T) {
	workerless := controlPlaneSchedulingPatch(true)
	if !strings.Contains(workerless, `"allowSchedulingOnControlPlanes":true`) ||
		!strings.Contains(workerless, `"`+loadBalancerExclusionLabel+`":{"$patch":"delete"}`) {
		t.Fatalf("worker-less patch = %s", workerless)
	}
	reserved := controlPlaneSchedulingPatch(false)
	if !strings.Contains(reserved, `"allowSchedulingOnControlPlanes":false`) ||
		!strings.Contains(reserved, `"`+loadBalancerExclusionLabel+`":""`) {
		t.Fatalf("reserved patch = %s", reserved)
	}
	for _, patch := range []string{workerless, reserved} {
		var document map[string]any
		if err := yaml.Unmarshal([]byte(patch), &document); err != nil {
			t.Fatalf("narrated patch %s is not valid YAML: %v", patch, err)
		}
	}
}

func TestReconcileRestoresControlPlaneTaintWhenClusterHasWorkers(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	client := &fakeClient{kubeData: []byte("kubeconfig"), schedulingChanged: []bool{true}}
	item, request := schedulingRequest(t, 1, client)
	if _, err := Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(client.scheduling) != 1 || client.scheduling[0] != (schedulingCall{node: item.Nodes[0].IP, workerless: false}) {
		t.Fatalf("scheduling calls = %+v, want one control-plane call with workerless=false", client.scheduling)
	}
}

func TestReconcileStaysSilentWhenControlPlaneSchedulingIsConverged(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	client := &fakeClient{kubeData: []byte("kubeconfig")}
	_, request := schedulingRequest(t, 0, client)
	result, err := Reconcile(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(client.scheduling) != 1 {
		t.Fatalf("scheduling calls = %+v, want one", client.scheduling)
	}
	if narration := strings.Join(result.Narration, "\n"); strings.Contains(narration, "patch mc --mode auto") {
		t.Fatalf("converged scheduling was narrated:\n%s", narration)
	}
}

func TestReconcileFailsWithNodeContextWhenSchedulingReconcileFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	refused := errors.New("apid refused the patch")
	client := &fakeClient{kubeData: []byte("kubeconfig"), configErrs: []error{refused}}
	item, request := schedulingRequest(t, 0, client)
	_, err := Reconcile(context.Background(), request)
	if !errors.Is(err, refused) {
		t.Fatalf("Reconcile() error = %v, want %v", err, refused)
	}
	if !strings.Contains(err.Error(), item.Nodes[0].Name) {
		t.Fatalf("Reconcile() error = %v, want node context", err)
	}
	if client.bootstrap != 0 {
		t.Fatalf("bootstrap calls = %d, want none after a failed scheduling reconcile", client.bootstrap)
	}
}

func controlPlaneNodeBody(taint, exclusion bool) string {
	taints := ""
	if taint {
		taints = `"taints":[{"key":"node-role.kubernetes.io/control-plane","effect":"NoSchedule"}]`
	}
	labels := ""
	if exclusion {
		labels = `,"node.kubernetes.io/exclude-from-external-load-balancers":""`
	}
	return `{"items":[{"metadata":{"name":"demo-cp-1","labels":{"kubernetes.io/hostname":"demo-cp-1"` + labels + `}},"spec":{` + taints + `}}]}`
}

// The zero-worker boundary is the one desired end state with no other probe,
// so the fast no-op depends on observing it in both directions.
func TestControlPlaneSchedulingConvergedObservesBothDirections(t *testing.T) {
	tests := []struct {
		name      string
		workers   int
		taint     bool
		exclusion bool
		wantErr   string
	}{
		{name: "worker-less and schedulable", workers: 0},
		{name: "worker-less but still tainted", workers: 0, taint: true, exclusion: true, wantErr: "NoSchedule taint"},
		{name: "worker-less but still excluded", workers: 0, exclusion: true, wantErr: "exclusion label"},
		{name: "with workers and reserved", workers: 1, taint: true, exclusion: true},
		{name: "with workers but still schedulable", workers: 1, wantErr: "NoSchedule taint"},
		{name: "with workers but still announcing", workers: 1, taint: true, wantErr: "exclusion label"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			item, err := cluster.New("demo", 0, 1, test.workers, cluster.NodeDefaults{})
			if err != nil {
				t.Fatal(err)
			}
			server, kubeconfig := readyServer(t, 200, controlPlaneNodeBody(test.taint, test.exclusion))
			defer server.Close()
			err = ControlPlaneSchedulingConverged(context.Background(), kubeconfig, item)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("ControlPlaneSchedulingConverged() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("ControlPlaneSchedulingConverged() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestControlPlaneSchedulingConvergedRequiresEveryControlPlane(t *testing.T) {
	item, err := cluster.New("demo", 0, 2, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	server, kubeconfig := readyServer(t, 200, controlPlaneNodeBody(false, false))
	defer server.Close()
	err = ControlPlaneSchedulingConverged(context.Background(), kubeconfig, item)
	if err == nil || !strings.Contains(err.Error(), "was not found") {
		t.Fatalf("ControlPlaneSchedulingConverged() error = %v, want a missing control plane", err)
	}
}
