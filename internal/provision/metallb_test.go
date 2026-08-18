package provision

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestRenderMetalLBUsesPinnedL2OnlyAssets(t *testing.T) {
	item, err := cluster.New("demo", 3, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true}
	objects, err := renderMetalLB(item)
	if err != nil {
		t.Fatal(err)
	}
	namespaces, chart, crds, extras, probe := partitionMetalLBObjects(objects)
	if len(namespaces) != 1 || namespaces[0].GetName() != metalLBNamespace {
		t.Fatalf("namespaces = %v, want managed %s namespace", objectNames(namespaces), metalLBNamespace)
	}
	// The speaker and frr-k8s DaemonSets need hostNetwork and NET_ADMIN/NET_RAW,
	// which Talos's default baseline PodSecurity rejects without these labels.
	for _, label := range []string{"pod-security.kubernetes.io/enforce", "pod-security.kubernetes.io/audit", "pod-security.kubernetes.io/warn"} {
		if got := namespaces[0].GetLabels()[label]; got != "privileged" {
			t.Fatalf("namespace label %s = %q, want privileged", label, got)
		}
	}
	if len(crds) != 9 {
		t.Fatalf("MetalLB CRDs = %d, want 9", len(crds))
	}
	if len(chart) == 0 {
		t.Fatal("MetalLB chart rendered no controller resources")
	}
	if got := objectKinds(extras); got != "IPAddressPool,L2Advertisement" {
		t.Fatalf("extras = %s", got)
	}
	if got := objectKinds(probe); got != "Namespace,Deployment,Service" {
		t.Fatalf("probe = %s", got)
	}
	for _, object := range objects {
		image, found, err := nestedStringField(object.Object, "spec", "template", "spec", "containers", "0", "image")
		if err != nil || !found {
			continue
		}
		if !strings.HasPrefix(image, "quay.io/metallb/") && image != "registry.k8s.io/e2e-test-images/agnhost:2.53" {
			t.Errorf("unmirrored unexpected workload image %q in %s/%s", image, object.GetKind(), object.GetName())
		}
	}
	probeService := probe[2]
	if got, _, err := nestedStringField(probeService.Object, "spec", "loadBalancerIP"); err != nil || got != "172.30.3.200" {
		t.Errorf("probe VIP = %q, %v", got, err)
	}
}

func TestApplyAllUsesServerSideApplyWithTalosboxOwnership(t *testing.T) {
	scheme := runtime.NewScheme()
	client := fake.NewSimpleDynamicClient(scheme)
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Group: "metallb.io", Version: "v1beta1"}})
	mapper.Add(schema.GroupVersionKind{Group: "metallb.io", Version: "v1beta1", Kind: "IPAddressPool"}, meta.RESTScopeNamespace)
	objects, err := decodeObjects([]byte(`apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata:
  name: demo-pool
  namespace: metallb-system
spec:
  addresses: [172.30.0.200-172.30.0.239]
`))
	if err != nil {
		t.Fatal(err)
	}
	client.PrependReactor("patch", "ipaddresspools", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &objects[0], nil
	})
	if err := applyAll(context.Background(), client, mapper, objects); err != nil {
		t.Fatal(err)
	}
	actions := client.Actions()
	if len(actions) != 1 {
		t.Fatalf("actions = %v", actions)
	}
	patch, ok := actions[0].(interface {
		GetPatchType() types.PatchType
		GetPatchOptions() metav1.PatchOptions
	})
	if !ok || patch.GetPatchType() != types.ApplyPatchType || patch.GetPatchOptions().FieldManager != fieldManager || actions[0].GetVerb() != "patch" || actions[0].GetResource().Resource != "ipaddresspools" {
		t.Fatalf("apply action = %#v", actions[0])
	}
	if err := applyAll(context.Background(), client, mapper, objects); err != nil {
		t.Fatalf("idempotent reapply: %v", err)
	}
}

func TestWaitForProbeRequiresExpectedVIPAndAResponse(t *testing.T) {
	item := cluster.Cluster{SubnetIndex: 2, ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true}}
	readyDeployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "lb-probe", Namespace: probeNamespace, Generation: 1}, Status: appsv1.DeploymentStatus{ObservedGeneration: 1, ReadyReplicas: 1, AvailableReplicas: 1}}
	t.Run("responding expected VIP", func(t *testing.T) {
		service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "lb-probe", Namespace: probeNamespace}, Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{{IP: "172.30.2.200"}}}}}
		client := kubernetesfake.NewClientset(readyDeployment, service)
		httpClient := &http.Client{Transport: roundTripper(func(request *http.Request) (*http.Response, error) {
			if request.URL.String() != "http://172.30.2.200/" {
				t.Errorf("probe URL = %s", request.URL)
			}
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
		})}
		vip, err := waitForProbe(context.Background(), client, item, time.Millisecond, httpClient)
		if err != nil || vip != "172.30.2.200" {
			t.Fatalf("waitForProbe() = %q, %v", vip, err)
		}
	})
	t.Run("wrong VIP never passes", func(t *testing.T) {
		service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "lb-probe", Namespace: probeNamespace}, Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{{IP: "172.30.2.201"}}}}}
		client := kubernetesfake.NewClientset(readyDeployment, service)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		defer cancel()
		if _, err := waitForProbe(ctx, client, item, time.Millisecond, nil); err == nil {
			t.Fatal("wrong VIP passed")
		} else if !strings.Contains(err.Error(), "want 172.30.2.200") {
			t.Fatalf("waitForProbe() error = %v", err)
		}
	})
	t.Run("nonresponding VIP never passes", func(t *testing.T) {
		service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "lb-probe", Namespace: probeNamespace}, Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{{IP: "172.30.2.200"}}}}}
		client := kubernetesfake.NewClientset(readyDeployment, service)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		defer cancel()
		failing := &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusBadGateway, Body: http.NoBody, Header: make(http.Header)}, nil
		})}
		if _, err := waitForProbe(ctx, client, item, time.Millisecond, failing); err == nil {
			t.Fatal("nonresponding VIP passed")
		} else if !strings.Contains(err.Error(), "LoadBalancer probe response =") {
			t.Fatalf("waitForProbe() error = %v", err)
		}
	})
	t.Run("response body close failure never passes", func(t *testing.T) {
		service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "lb-probe", Namespace: probeNamespace}, Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{{IP: "172.30.2.200"}}}}}
		client := kubernetesfake.NewClientset(readyDeployment, service)
		attempts := 0
		failing := &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
			attempts++
			if attempts == 1 {
				return &http.Response{StatusCode: http.StatusOK, Body: closeErrorBody{err: errors.New("close failed")}, Header: make(http.Header)}, nil
			}
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
		})}
		if _, err := waitForProbe(context.Background(), client, item, time.Millisecond, failing); err != nil {
			t.Fatalf("waitForProbe() error = %v", err)
		}
		if attempts != 2 {
			t.Fatalf("probe attempts = %d, want 2 after a response body close failure", attempts)
		}
	})
}

func TestWaitForMetalLBRequiresControllerSpeakerAndWebhook(t *testing.T) {
	controller := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "metallb-controller", Namespace: metalLBNamespace, Generation: 1}, Status: appsv1.DeploymentStatus{ObservedGeneration: 1, ReadyReplicas: 1, AvailableReplicas: 1}}
	speaker := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "metallb-speaker", Namespace: metalLBNamespace, Generation: 1}, Status: appsv1.DaemonSetStatus{ObservedGeneration: 1, DesiredNumberScheduled: 1, NumberReady: 1}}
	ready := true
	webhook := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: "metallb-webhook-service-abcde", Namespace: metalLBNamespace, Labels: map[string]string{discoveryv1.LabelServiceName: "metallb-webhook-service"}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"172.30.2.2"}, Conditions: discoveryv1.EndpointConditions{Ready: &ready}}},
		Ports:       []discoveryv1.EndpointPort{{Port: int32ptr(443)}},
	}
	if err := waitForMetalLB(context.Background(), kubernetesfake.NewClientset(controller, speaker, webhook), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	notReady := false
	webhook.Endpoints[0].Conditions.Ready = &notReady
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if err := waitForMetalLB(ctx, kubernetesfake.NewClientset(controller, speaker, webhook), time.Millisecond); err == nil {
		t.Fatal("MetalLB passed with an unready webhook endpoint")
	}
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if err := waitForMetalLB(ctx, kubernetesfake.NewClientset(controller, speaker), time.Millisecond); err == nil {
		t.Fatal("MetalLB passed without a webhook endpoint")
	}
}

func TestVIPHTTPClientDisablesProxyWithoutMutatingCaller(t *testing.T) {
	proxyFn := func(*http.Request) (*url.URL, error) {
		return url.Parse("http://proxy.invalid")
	}
	base := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: proxyFn}}
	client := vipHTTPClient(base)

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("vipHTTPClient transport = %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("vipHTTPClient left proxy enabled")
	}
	if got := client.Timeout; got != time.Second {
		t.Fatalf("vipHTTPClient timeout = %s, want %s", got, time.Second)
	}
	original, ok := base.Transport.(*http.Transport)
	if !ok || original.Proxy == nil {
		t.Fatal("vipHTTPClient mutated the caller's transport")
	}
}

func TestPollReturnsContextAndLastCheckError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	err := poll(ctx, time.Millisecond, func(context.Context) error {
		return errors.New("still waiting on VIP")
	})
	if err == nil {
		t.Fatal("poll() returned nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("poll() error = %v, want deadline exceeded", err)
	}
	if !strings.Contains(err.Error(), "still waiting on VIP") {
		t.Fatalf("poll() error = %v, want last check failure", err)
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (fn roundTripper) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }

type closeErrorBody struct{ err error }

func (body closeErrorBody) Read([]byte) (int, error) { return 0, io.EOF }

func (body closeErrorBody) Close() error { return body.err }

func int32ptr(value int32) *int32 { return &value }

func TestRenderMetalLBRejectsNonFlannelIntent(t *testing.T) {
	_, err := renderMetalLB(cluster.Cluster{ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true}})
	if err == nil || !strings.Contains(err.Error(), "flannel with lb: true") {
		t.Fatalf("renderMetalLB() error = %v", err)
	}
}

// The curated flannel path is L2-only, so the frr-k8s subchart the packaged
// MetalLB chart depends on must be pruned by its `frrk8s.enabled` condition
// rather than rendered: its workloads watch FRRConfiguration CRDs this path
// deliberately never installs and would CrashLoopBackOff (#336).
func TestRenderMetalLBOmitsDisabledFRRK8sSubchart(t *testing.T) {
	item, err := cluster.New("demo", 3, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true}
	objects, err := renderMetalLB(item)
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range objects {
		if chart := object.GetLabels()["helm.sh/chart"]; strings.HasPrefix(chart, "frr-k8s") {
			t.Errorf("rendered %s %q from disabled subchart %s", object.GetKind(), object.GetName(), chart)
		}
		if strings.Contains(object.GetName(), "frr") || strings.Contains(object.GetAPIVersion(), "frrk8s.metallb.io") {
			t.Errorf("rendered frr-k8s object %s %q (%s)", object.GetKind(), object.GetName(), object.GetAPIVersion())
		}
		group, _, _ := unstructured.NestedString(object.Object, "spec", "group")
		if group == "frrk8s.metallb.io" {
			t.Errorf("rendered frr-k8s CRD %q", object.GetName())
		}
	}
	// The curated values must keep describing what actually lands (SPEC
	// manifests parity), so the speaker still runs without the FRR backends.
	for _, object := range objects {
		if object.GetKind() != "DaemonSet" {
			continue
		}
		if object.GetName() != "metallb-speaker" {
			t.Errorf("unexpected DaemonSet %q in L2-only render", object.GetName())
		}
	}
}

func objectNames(objects []unstructured.Unstructured) []string {
	names := make([]string, 0, len(objects))
	for _, object := range objects {
		names = append(names, object.GetName())
	}
	return names
}

func objectKinds(objects []unstructured.Unstructured) string {
	kinds := make([]string, 0, len(objects))
	for _, object := range objects {
		kinds = append(kinds, object.GetKind())
	}
	return strings.Join(kinds, ",")
}

func nestedStringField(value map[string]any, path ...string) (string, bool, error) {
	var current any = value
	for _, key := range path {
		switch typed := current.(type) {
		case map[string]any:
			current = typed[key]
		case []any:
			if key != "0" || len(typed) == 0 {
				return "", false, nil
			}
			current = typed[0]
		default:
			return "", false, fmt.Errorf("read %q from %T", key, current)
		}
	}
	result, ok := current.(string)
	return result, ok, nil
}
