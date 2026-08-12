package provision

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/manifests"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
)

type recordingReconciler struct {
	order *[]string
}

func TestWaitForCiliumRequiresOperatorAgentAndEnvoy(t *testing.T) {
	operator := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "cilium-operator", Namespace: ciliumNamespace, Generation: 1}, Status: appsv1.DeploymentStatus{ObservedGeneration: 1, ReadyReplicas: 1, AvailableReplicas: 1}}
	agent := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "cilium", Namespace: ciliumNamespace, Generation: 1}, Status: appsv1.DaemonSetStatus{ObservedGeneration: 1, DesiredNumberScheduled: 1, NumberReady: 1}}
	envoy := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "cilium-envoy", Namespace: ciliumNamespace, Generation: 1}, Status: appsv1.DaemonSetStatus{ObservedGeneration: 1, DesiredNumberScheduled: 1, NumberReady: 1}}
	if err := waitForCilium(context.Background(), kubernetesfake.NewClientset(operator, agent, envoy), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if err := waitForCilium(ctx, kubernetesfake.NewClientset(operator, agent), time.Millisecond); err == nil {
		t.Fatal("Cilium passed without envoy")
	}
}

func (r recordingReconciler) Reconcile(context.Context, cluster.Cluster, []byte) (LoadBalancerResult, error) {
	*r.order = append(*r.order, "cilium")
	return LoadBalancerResult{}, nil
}

func TestCiliumReconcileInstallsCNIBeforeWaitingForNodesReady(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("demo", 0, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.TalosVersion = "v1.13.6"
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNICilium}
	order := []string{}
	client := &fakeClient{kubeData: []byte("kubeconfig")}
	client.readyErrs = []error{fmt.Errorf("Cilium not installed yet")}
	client.readyHook = func() { order = append(order, "ready") }
	observed := 0
	result, err := Reconcile(context.Background(), Request{
		Cluster:      item,
		Client:       client,
		PollInterval: 0,
		LoadBalancer: recordingReconciler{order: &order},
		Observe: func(context.Context) ([]Node, error) {
			phase := PhaseConfigured
			if observed == 0 {
				phase = PhaseMaintenance
			}
			observed++
			return []Node{{Name: item.Nodes[0].Name, Role: cluster.RoleControlPlane, IP: item.Nodes[0].IP, Phase: phase}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(client.configs) != 1 {
		t.Fatalf("applied config count = %d, want 1", len(client.configs))
	}
	config := string(client.configs[0])
	for _, wanted := range []string{"name: none", "proxy:", "disabled: true", "kind: RegistryMirrorConfig", "http://172.30.0.1:5059", "skipFallback: true"} {
		if !strings.Contains(config, wanted) {
			t.Errorf("Cilium machine config missing %q:\n%s", wanted, config)
		}
	}
	if got, want := strings.Join(order, ","), "cilium,ready,ready"; got != want {
		t.Fatalf("Cilium reconcile order = %s, want %s", got, want)
	}
	if len(client.expected) != 2 {
		t.Fatalf("Kubernetes Ready calls = %d, want Cilium-before-Ready retry", len(client.expected))
	}
	if result.KubeconfigPath == "" {
		t.Fatal("Cilium reconciliation did not write kubeconfig")
	}
}

func TestRenderCiliumUsesPinnedTalosValuesAndNativeLoadBalancer(t *testing.T) {
	item, err := cluster.New("demo", 3, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true}

	objects, err := renderCilium(item)
	if err != nil {
		t.Fatal(err)
	}
	namespaces, chart, extras, probe := partitionCiliumObjects(objects)
	if len(namespaces) != 1 || namespaces[0].GetName() != "cilium-secrets" {
		t.Fatalf("Cilium namespaces = %v, want chart-owned cilium-secrets namespace", objectNames(namespaces))
	}
	if len(chart) == 0 {
		t.Fatal("Cilium chart rendered no workload objects")
	}
	for _, required := range []string{"cilium-operator", "cilium", "cilium-envoy"} {
		found := false
		for _, object := range chart {
			if object.GetName() == required {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("Cilium chart omitted required workload %q: %v", required, objectNames(chart))
		}
	}
	if got := objectKinds(extras); got != "CiliumLoadBalancerIPPool,CiliumL2AnnouncementPolicy" {
		t.Fatalf("Cilium extras = %s", got)
	}
	if got := objectKinds(probe); got != "Namespace,Deployment,Service" {
		t.Fatalf("Cilium probe = %s", got)
	}
	service := probe[len(probe)-1]
	annotation, found, err := nestedStringField(service.Object, "metadata", "annotations", "lbipam.cilium.io/ips")
	if err != nil || !found || annotation != "172.30.3.200" {
		t.Fatalf("Cilium probe requested IP = %q, found=%t, err=%v", annotation, found, err)
	}
	for _, object := range objects {
		image, found, err := nestedStringField(object.Object, "spec", "template", "spec", "containers", "0", "image")
		if err != nil || !found {
			continue
		}
		if !strings.HasPrefix(image, "quay.io/cilium/") && image != "registry.k8s.io/e2e-test-images/agnhost:2.53" {
			t.Errorf("unmirrored unexpected Cilium workload image %q in %s/%s", image, object.GetKind(), object.GetName())
		}
	}
}

func TestCiliumValuesHaveNoIngressOrHubble(t *testing.T) {
	facts := manifests.Facts{Cluster: "demo", SubnetIndex: 3, LB: true}
	values := manifests.CiliumValues(facts)
	for _, wanted := range []string{
		"mode: kubernetes",
		"kubeProxyReplacement: true",
		"k8sServiceHost: localhost",
		"k8sServicePort: 7445",
		"hostLegacyRouting: true",
		"enabled: false",
		"hubble:\n  enabled: false\n  relay:\n    enabled: false\n  ui:\n    enabled: false",
		"qps: 10",
		"burst: 20",
	} {
		if !strings.Contains(values, wanted) {
			t.Errorf("Cilium values missing %q:\n%s", wanted, values)
		}
	}
	for _, forbidden := range []string{"loadbalancerMode", "lbipam.cilium.io/ips", "default: true"} {
		if strings.Contains(values, forbidden) {
			t.Errorf("Cilium values unexpectedly contain %q:\n%s", forbidden, values)
		}
	}
}

func TestRenderCiliumWithoutLoadBalancerOmitsExtrasAndProbe(t *testing.T) {
	item := cluster.Cluster{ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium}}
	objects, err := renderCilium(item)
	if err != nil {
		t.Fatal(err)
	}
	_, chart, extras, probe := partitionCiliumObjects(objects)
	if len(chart) == 0 {
		t.Fatal("Cilium chart rendered no workload objects")
	}
	if len(extras) != 0 || len(probe) != 0 {
		t.Fatalf("lb:false extras = %s, probe = %s", objectKinds(extras), objectKinds(probe))
	}
}
