package provision

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/manifests"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
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

func TestWaitForIngressTLSVerifiesTheHostnameWhileConnectingToTheVIP(t *testing.T) {
	now := time.Date(2026, time.August, 31, 10, 0, 0, 0, time.UTC)
	item := cluster.Cluster{Name: "demo", Domain: "workshop.internal", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true}}
	pki, err := ensureIngressPKI(item, testIngressPKIPaths(t), ingressPKIOptions{Now: fixedTime(now), Rand: newDeterministicReader(20)})
	if err != nil {
		t.Fatal(err)
	}
	transportFactory := func(config *tls.Config, _ func(context.Context, string, string) (net.Conn, error)) http.RoundTripper {
		if config.ServerName != "probe.workshop.internal" {
			t.Fatalf("TLS ServerName = %q", config.ServerName)
		}
		if _, err := pki.LeafCertificate.Verify(x509.VerifyOptions{DNSName: config.ServerName, Roots: config.RootCAs, CurrentTime: now.Add(time.Hour)}); err != nil {
			t.Fatalf("generated certificate does not verify with readiness TLS config: %v", err)
		}
		return roundTripper(func(request *http.Request) (*http.Response, error) {
			if request.URL.String() != "https://172.30.9.200/" {
				t.Errorf("TLS probe URL = %s", request.URL)
			}
			if request.Host != "probe.workshop.internal" {
				t.Errorf("TLS probe Host = %q", request.Host)
			}
			return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: http.NoBody, Header: make(http.Header)}, nil
		})
	}
	if err := waitForIngressTLS(context.Background(), item, "172.30.9.200", pki, transportFactory); err != nil {
		t.Fatal(err)
	}
}

func TestRecreateLegacyCiliumProbeServiceTransitionsOnlyTheManagedLoadBalancer(t *testing.T) {
	for _, test := range []struct {
		name       string
		service    *corev1.Service
		wantDelete bool
		wantError  bool
	}{
		{
			name:       "managed legacy LoadBalancer",
			service:    &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "lb-probe", Namespace: probeNamespace, Labels: map[string]string{"talosbox.dev/managed": "true"}}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer}},
			wantDelete: true,
		},
		{
			name:    "already ClusterIP",
			service: &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "lb-probe", Namespace: probeNamespace, Labels: map[string]string{"talosbox.dev/managed": "true"}}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP}},
		},
		{
			name:      "unmanaged LoadBalancer",
			service:   &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "lb-probe", Namespace: probeNamespace}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer}},
			wantError: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := kubernetesfake.NewClientset(test.service)
			err := recreateLegacyCiliumProbeService(context.Background(), client, time.Millisecond)
			if (err != nil) != test.wantError {
				t.Fatalf("recreateLegacyCiliumProbeService() error = %v", err)
			}
			deleted := false
			for _, action := range client.Actions() {
				deleted = deleted || action.GetVerb() == "delete" && action.GetResource().Resource == "services"
			}
			if deleted != test.wantDelete {
				t.Fatalf("Service delete action = %t, want %t", deleted, test.wantDelete)
			}
		})
	}
}

func (r recordingReconciler) Reconcile(context.Context, cluster.Cluster, []byte) (LoadBalancerResult, error) {
	*r.order = append(*r.order, "cilium")
	return LoadBalancerResult{}, nil
}

type recordingBGPReconciler struct {
	order *[]string
}

func (r recordingBGPReconciler) ReconcileBGP(context.Context, cluster.Cluster) error {
	*r.order = append(*r.order, "bgp")
	return nil
}

func (r recordingBGPReconciler) DisableBGP(context.Context, cluster.Cluster) error {
	*r.order = append(*r.order, "disable-bgp")
	return nil
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

func TestCiliumReconcileReassertsHostBGPAfterApplyingCilium(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("demo", 0, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.TalosVersion = "v1.13.6"
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true, BGP: true}
	order := []string{}
	client := &fakeClient{kubeData: []byte("kubeconfig")}
	client.readyHook = func() { order = append(order, "ready") }
	result, err := Reconcile(context.Background(), Request{
		Cluster:      item,
		Client:       client,
		PollInterval: 0,
		LoadBalancer: recordingReconciler{order: &order},
		BGP:          recordingBGPReconciler{order: &order},
		Observe: func(context.Context) ([]Node, error) {
			return []Node{{Name: item.Nodes[0].Name, Role: cluster.RoleControlPlane, IP: item.Nodes[0].IP, Phase: PhaseConfigured}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(order, ","), "bgp,cilium,ready"; got != want {
		t.Fatalf("Cilium/BGP reconcile order = %s, want %s", got, want)
	}
	if narration := strings.Join(result.Narration, "\n"); !strings.Contains(narration, "host BGP: ≈ tbx bgp enable demo") {
		t.Fatalf("Cilium/BGP narration missing host stage: %s", narration)
	}
}

func TestCiliumL2ReconcileDisablesHostBGPEveryRunAfterApplyingL2(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("demo", 0, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.TalosVersion = "v1.13.6"
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true}
	order := []string{}
	client := &fakeClient{kubeData: []byte("kubeconfig")}
	client.readyHook = func() { order = append(order, "ready") }
	result, err := Reconcile(context.Background(), Request{
		Cluster:      item,
		Client:       client,
		PollInterval: 0,
		LoadBalancer: recordingReconciler{order: &order},
		BGP:          recordingBGPReconciler{order: &order},
		Observe: func(context.Context) ([]Node, error) {
			return []Node{{Name: item.Nodes[0].Name, Role: cluster.RoleControlPlane, IP: item.Nodes[0].IP, Phase: PhaseConfigured}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(order, ","), "cilium,disable-bgp,ready"; got != want {
		t.Fatalf("Cilium/L2 reconcile order = %s, want %s", got, want)
	}
	if narration := strings.Join(result.Narration, "\n"); !strings.Contains(narration, "host BGP: ≈ tbx bgp disable demo") {
		t.Fatalf("Cilium/L2 narration missing host stage: %s", narration)
	}
}

func TestCiliumWithoutLoadBalancerDoesNotTouchHostBGP(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("demo", 0, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.TalosVersion = "v1.13.6"
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNICilium}
	order := []string{}
	client := &fakeClient{kubeData: []byte("kubeconfig")}
	client.readyHook = func() { order = append(order, "ready") }
	_, err = Reconcile(context.Background(), Request{
		Cluster:      item,
		Client:       client,
		PollInterval: 0,
		LoadBalancer: recordingReconciler{order: &order},
		BGP:          recordingBGPReconciler{order: &order},
		Observe: func(context.Context) ([]Node, error) {
			return []Node{{Name: item.Nodes[0].Name, Role: cluster.RoleControlPlane, IP: item.Nodes[0].IP, Phase: PhaseConfigured}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(order, ","), "cilium,ready"; got != want {
		t.Fatalf("Cilium non-LB reconcile order = %s, want %s", got, want)
	}
}

func TestCiliumConvergedRejectsStaleBGPResourcesOnL2Path(t *testing.T) {
	item := cluster.Cluster{Name: "demo", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true}}
	server, kubeconfig := ciliumConvergedServer(t, item, true)
	defer server.Close()
	if err := CiliumConverged(context.Background(), kubeconfig, item); err == nil || !strings.Contains(strings.ToLower(err.Error()), "stale cilium object") {
		t.Fatalf("CiliumConverged() error = %v, want stale BGP object", err)
	}
}

func TestCiliumConvergedAcceptsExactL2DesiredSet(t *testing.T) {
	item := cluster.Cluster{Name: "demo", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true}}
	server, kubeconfig := ciliumConvergedServer(t, item, false)
	defer server.Close()
	if err := CiliumConverged(context.Background(), kubeconfig, item); err != nil {
		t.Fatal(err)
	}
}

func TestCiliumConvergedAcceptsExactBGPDesiredSet(t *testing.T) {
	item := cluster.Cluster{Name: "demo", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true, BGP: true}}
	server, kubeconfig := ciliumConvergedServer(t, item, false)
	defer server.Close()
	if err := CiliumConverged(context.Background(), kubeconfig, item); err != nil {
		t.Fatal(err)
	}
}

func TestCiliumConvergedAcceptsNoLoadBalancerDesiredSet(t *testing.T) {
	item := cluster.Cluster{Name: "demo", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium}}
	server, kubeconfig := ciliumConvergedServer(t, item, false)
	defer server.Close()
	if err := CiliumConverged(context.Background(), kubeconfig, item); err != nil {
		t.Fatal(err)
	}
}

func TestCiliumConvergedAcceptsFullHubbleDesiredSet(t *testing.T) {
	item := cluster.Cluster{Name: "demo", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, Hubble: true}}
	server, kubeconfig := ciliumConvergedServer(t, item, false)
	defer server.Close()
	if err := CiliumConverged(context.Background(), kubeconfig, item); err != nil {
		t.Fatal(err)
	}
}

func TestCiliumConvergedRejectsPartialHubbleDeletion(t *testing.T) {
	item := cluster.Cluster{Name: "demo", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium}}
	server, kubeconfig := ciliumConvergedServerWithOptions(t, item, ciliumConvergedOptions{staleHubble: true})
	defer server.Close()
	if err := CiliumConverged(context.Background(), kubeconfig, item); err == nil || !strings.Contains(strings.ToLower(err.Error()), "stale cilium object") {
		t.Fatalf("CiliumConverged() error = %v, want stale Hubble object", err)
	}
}

func TestCiliumConvergedRejectsUnmanagedProbe(t *testing.T) {
	item := cluster.Cluster{Name: "demo", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true}}
	server, kubeconfig := ciliumConvergedServerWithOptions(t, item, ciliumConvergedOptions{unmanagedProbe: true})
	defer server.Close()
	if err := CiliumConverged(context.Background(), kubeconfig, item); err == nil || !strings.Contains(strings.ToLower(err.Error()), "not owned") {
		t.Fatalf("CiliumConverged() error = %v, want unmanaged probe error", err)
	}
}

func TestCiliumConvergedRejectsUnmanagedAnnouncement(t *testing.T) {
	item := cluster.Cluster{Name: "demo", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true}}
	server, kubeconfig := ciliumConvergedServerWithOptions(t, item, ciliumConvergedOptions{unmanagedAnnouncement: true})
	defer server.Close()
	if err := CiliumConverged(context.Background(), kubeconfig, item); err == nil || !strings.Contains(strings.ToLower(err.Error()), "not owned") {
		t.Fatalf("CiliumConverged() error = %v, want unmanaged announcement error", err)
	}
}

func ciliumConvergedServer(t *testing.T, item cluster.Cluster, staleBGP bool) (*httptest.Server, []byte) {
	return ciliumConvergedServerWithOptions(t, item, ciliumConvergedOptions{staleBGP: staleBGP})
}

type ciliumConvergedOptions struct {
	staleBGP              bool
	staleHubble           bool
	unmanagedProbe        bool
	unmanagedAnnouncement bool
}

func ciliumConvergedServerWithOptions(t *testing.T, item cluster.Cluster, options ciliumConvergedOptions) (*httptest.Server, []byte) {
	t.Helper()
	if item.LB {
		t.Setenv("HOME", t.TempDir())
		if _, err := ensureIngressPKI(item, ingressPKIPathsForDir(filepath.Join(os.Getenv("HOME"), ".talosbox", "clusters", item.Name)), ingressPKIOptions{
			Now:  func() time.Time { return time.Date(2026, time.August, 31, 10, 0, 0, 0, time.UTC) },
			Rand: newDeterministicReader(11),
		}); err != nil {
			t.Fatal(err)
		}
	}
	originalHTTPProbe := ciliumDirectHTTPProbe
	originalTLSProbe := ciliumDirectTLSProbe
	ciliumDirectHTTPProbe = func(context.Context, cluster.Cluster, string, *http.Client) error { return nil }
	ciliumDirectTLSProbe = func(context.Context, cluster.Cluster, string, ingressPKI) error { return nil }
	t.Cleanup(func() {
		ciliumDirectHTTPProbe = originalHTTPProbe
		ciliumDirectTLSProbe = originalTLSProbe
	})
	hubbleCandidates, err := ciliumHubbleObjects(item)
	if err != nil {
		t.Fatal(err)
	}
	hubblePaths := make(map[string]string, len(hubbleCandidates))
	for _, candidate := range hubbleCandidates {
		path, err := ciliumObjectPath(candidate)
		if err != nil {
			t.Fatal(err)
		}
		hubblePaths[path] = candidate.GetKind()
	}
	certificate, certificatePEM, keyPEM := testCertificate(t)
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certificatePEM) {
		t.Fatal("parse test CA")
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		path := request.URL.Path
		if kind, hubble := hubblePaths[path]; hubble {
			if !item.Hubble && !options.staleHubble {
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			if kind == "Deployment" {
				_, _ = writer.Write([]byte(ciliumOwnedDeploymentJSON))
				return
			}
			_, _ = writer.Write([]byte(ciliumHubbleOwnedObjectJSON))
			return
		}
		switch path {
		case "/apis/networking.k8s.io/v1/ingressclasses/cilium":
			if !item.LB {
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = writer.Write([]byte(`{"metadata":{"annotations":{"ingressclass.kubernetes.io/is-default-class":"true"}}}`))
		case "/api/v1/namespaces/kube-system/services/cilium-ingress":
			if !item.LB {
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = fmt.Fprintf(writer, `{"metadata":{"annotations":{"lbipam.cilium.io/ips":"172.30.%d.200"}},"status":{"loadBalancer":{"ingress":[{"ip":"172.30.%d.200"}]}}}`, item.SubnetIndex, item.SubnetIndex)
		case "/apis/apps/v1/namespaces/kube-system/deployments/cilium-operator":
			_, _ = writer.Write([]byte(`{"metadata":{"generation":1},"status":{"observedGeneration":1,"readyReplicas":1,"availableReplicas":1}}`))
		case "/apis/apps/v1/namespaces/kube-system/daemonsets/cilium", "/apis/apps/v1/namespaces/kube-system/daemonsets/cilium-envoy":
			_, _ = writer.Write([]byte(`{"metadata":{"generation":1},"status":{"observedGeneration":1,"desiredNumberScheduled":1,"numberReady":1}}`))
		case ciliumProbeDeploymentPath():
			if !item.LB {
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			if options.unmanagedProbe {
				_, _ = writer.Write([]byte(ciliumUnmanagedDeploymentJSON))
				return
			}
			_, _ = writer.Write([]byte(ciliumProbeDeploymentJSON))
		case ciliumProbeServicePath():
			if !item.LB {
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			if options.unmanagedProbe {
				_, _ = writer.Write([]byte(`{}`))
				return
			}
			_, _ = writer.Write([]byte(ciliumProbeObjectJSON))
		case ciliumProbeIngressPath():
			if !item.LB {
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			if options.unmanagedProbe {
				_, _ = writer.Write([]byte(`{}`))
				return
			}
			_, _ = writer.Write([]byte(ciliumProbeIngressJSON))
		case ciliumProbeTLSSecretPath():
			if !item.LB {
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			if options.unmanagedProbe {
				_, _ = writer.Write([]byte(`{}`))
				return
			}
			_, _ = writer.Write([]byte(ciliumProbeSecretJSON))
		case "/apis/cilium.io/v2/ciliumloadbalancerippools/demo-pool":
			if !item.LB {
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = writer.Write([]byte(ciliumOwnedObjectJSON))
		case "/apis/cilium.io/v2alpha1/ciliuml2announcementpolicies/demo-l2":
			if !item.LB || item.BGP {
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			if options.unmanagedAnnouncement {
				_, _ = writer.Write([]byte(`{}`))
				return
			}
			_, _ = writer.Write([]byte(ciliumOwnedObjectJSON))
		default:
			if (item.LB && item.BGP || options.staleBGP) && strings.Contains(path, "/ciliumbgp") {
				_, _ = writer.Write([]byte(ciliumOwnedObjectJSON))
				return
			}
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	kubeconfig := fmt.Sprintf(`apiVersion: v1
clusters:
- name: test
  cluster:
    server: %s
    certificate-authority-data: %s
contexts:
- name: test
  context:
    cluster: test
    user: admin
current-context: test
users:
- name: admin
  user:
    client-certificate-data: %s
    client-key-data: %s
`, server.URL, base64.StdEncoding.EncodeToString(certificatePEM), base64.StdEncoding.EncodeToString(certificatePEM), base64.StdEncoding.EncodeToString(keyPEM))
	return server, []byte(kubeconfig)
}

const ciliumOwnedObjectJSON = `{"metadata":{"annotations":{"talosbox.dev/announcement-owned":"talosbox"}}}`
const ciliumHubbleOwnedObjectJSON = `{"metadata":{"annotations":{"talosbox.dev/hubble-owned":"talosbox"}}}`
const ciliumOwnedDeploymentJSON = `{"metadata":{"generation":1,"annotations":{"talosbox.dev/hubble-owned":"talosbox"}},"status":{"observedGeneration":1,"readyReplicas":1,"availableReplicas":1}}`
const ciliumProbeDeploymentJSON = `{"metadata":{"generation":1,"labels":{"talosbox.dev/managed":"true"}},"status":{"observedGeneration":1,"readyReplicas":1,"availableReplicas":1}}`
const ciliumProbeObjectJSON = `{"metadata":{"labels":{"talosbox.dev/managed":"true"}},"spec":{"type":"ClusterIP"}}`
const ciliumProbeIngressJSON = `{"metadata":{"labels":{"talosbox.dev/managed":"true"}}}`
const ciliumProbeSecretJSON = `{"metadata":{"labels":{"talosbox.dev/managed":"true"}}}`
const ciliumUnmanagedDeploymentJSON = `{"metadata":{"generation":1},"status":{"observedGeneration":1,"readyReplicas":1,"availableReplicas":1}}`

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
	if got := objectKinds(probe); got != "Namespace,Deployment,Service,Ingress" {
		t.Fatalf("Cilium probe = %s", got)
	}
	ingressClass := findRenderedObject(t, objects, "IngressClass", "cilium")
	if got := ingressClass.GetAnnotations()["ingressclass.kubernetes.io/is-default-class"]; got != "true" {
		t.Fatalf("Cilium IngressClass default annotation = %q", got)
	}
	ingressService := findRenderedObject(t, objects, "Service", "cilium-ingress")
	if ingressService.GetNamespace() != ciliumNamespace {
		t.Fatalf("Cilium ingress Service namespace = %q, want %q", ingressService.GetNamespace(), ciliumNamespace)
	}
	if serviceType, _, _ := unstructured.NestedString(ingressService.Object, "spec", "type"); serviceType != "LoadBalancer" {
		t.Fatalf("Cilium ingress Service type = %q", serviceType)
	}
	if got := ingressService.GetAnnotations()["lbipam.cilium.io/ips"]; got != "172.30.3.200" {
		t.Fatalf("Cilium ingress Service requested IP = %q", got)
	}
	probeService := findRenderedObject(t, probe, "Service", "lb-probe")
	if serviceType, _, _ := unstructured.NestedString(probeService.Object, "spec", "type"); serviceType != "ClusterIP" {
		t.Fatalf("Cilium probe Service type = %q", serviceType)
	}
	probeIngress := findRenderedObject(t, probe, "Ingress", "lb-probe")
	if probeIngress.GetLabels()["talosbox.dev/managed"] != "true" {
		t.Fatal("Cilium probe Ingress is not talosbox-owned")
	}
	if ingressClassName, _, _ := unstructured.NestedString(probeIngress.Object, "spec", "ingressClassName"); ingressClassName != "cilium" {
		t.Fatalf("Cilium probe Ingress class = %q", ingressClassName)
	}
	if host, _, _ := nestedStringField(probeIngress.Object, "spec", "rules", "0", "host"); host != "*.demo.k8s.test" {
		t.Fatalf("Cilium probe Ingress host = %q", host)
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(probeIngress.Object, "spec", "defaultBackend"); found {
		t.Fatal("Cilium probe Ingress uses spec.defaultBackend")
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

func TestCiliumValuesEnableIngressWithoutHubble(t *testing.T) {
	facts := manifests.Facts{Cluster: "demo", SubnetIndex: 3, LB: true}
	values := manifests.CiliumValues(facts)
	for _, wanted := range []string{
		"mode: kubernetes",
		"kubeProxyReplacement: true",
		"k8sServiceHost: localhost",
		"k8sServicePort: 7445",
		"hostLegacyRouting: true",
		"ingressController:\n  enabled: true\n  default: true\n  loadbalancerMode: shared\n  enforceHttps: false",
		"defaultSecretNamespace: talosbox-system",
		"defaultSecretName: ingress-wildcard-tls",
		"lbipam.cilium.io/ips: \"172.30.3.200\"",
		"hubble:\n  enabled: false\n  tls:\n    auto:\n      method: cronJob\n  relay:\n    enabled: false\n  ui:\n    enabled: false",
		"qps: 10",
		"burst: 20",
	} {
		if !strings.Contains(values, wanted) {
			t.Errorf("Cilium values missing %q:\n%s", wanted, values)
		}
	}
	for _, forbidden := range []string{"enforceHttps: true", "spec.defaultBackend"} {
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

func TestRenderCiliumMakesHubbleRelayAndUIFollowIntent(t *testing.T) {
	for _, tt := range []struct {
		name   string
		hubble bool
		want   []string
	}{
		{name: "disabled"},
		{name: "enabled", hubble: true, want: []string{"hubble-peer", "hubble-relay", "hubble-ui"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			item := cluster.Cluster{ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, Hubble: tt.hubble}}
			objects, err := renderCilium(item)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"hubble-peer", "hubble-relay", "hubble-ui"} {
				found := false
				for _, object := range objects {
					if object.GetName() == name {
						found = true
						break
					}
				}
				want := false
				for _, expected := range tt.want {
					want = want || expected == name
				}
				if found != want {
					t.Errorf("rendered %q = %t, want %t", name, found, want)
				}
			}
			if tt.hubble {
				candidates, err := ciliumHubbleObjects(item)
				if err != nil {
					t.Fatal(err)
				}
				for _, candidate := range candidates {
					for _, object := range objects {
						if objectID(object) == objectID(candidate) && object.GetAnnotations()[hubbleOwnershipAnnotation] != fieldManager {
							t.Errorf("Hubble object %s/%s lacks tbx ownership annotation", object.GetKind(), object.GetName())
						}
					}
				}
			}
		})
	}
}

func TestRenderCiliumBGPUsesNoL2AnnouncementObject(t *testing.T) {
	item := cluster.Cluster{ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true, BGP: true}}
	objects, err := renderCilium(item)
	if err != nil {
		t.Fatal(err)
	}
	_, _, extras, _ := partitionCiliumObjects(objects)
	if got, want := objectKinds(extras), "CiliumLoadBalancerIPPool,CiliumBGPClusterConfig,CiliumBGPPeerConfig,CiliumBGPAdvertisement"; got != want {
		t.Fatalf("BGP extras = %s, want %s", got, want)
	}
}

func TestWaitForHubbleRequiresRelayAndUI(t *testing.T) {
	relay := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "hubble-relay", Namespace: ciliumNamespace, Generation: 1}, Status: appsv1.DeploymentStatus{ObservedGeneration: 1, ReadyReplicas: 1, AvailableReplicas: 1}}
	ui := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "hubble-ui", Namespace: ciliumNamespace, Generation: 1}, Status: appsv1.DeploymentStatus{ObservedGeneration: 1, ReadyReplicas: 1, AvailableReplicas: 1}}
	if err := waitForHubble(context.Background(), kubernetesfake.NewClientset(relay, ui), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if err := waitForHubble(ctx, kubernetesfake.NewClientset(relay), time.Millisecond); err == nil {
		t.Fatal("Hubble passed without its UI")
	}
}

func TestDeleteHubbleObjectsRemovesOnlyTalosboxOwnedCandidates(t *testing.T) {
	item := cluster.Cluster{ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium}}
	candidates, err := ciliumHubbleObjects(item)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) < 2 {
		t.Fatalf("Hubble deletion candidates = %d, want at least two", len(candidates))
	}
	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	mapper := ciliumTestMapper(candidates)
	owned := candidates[0].DeepCopy()
	owned.SetAnnotations(map[string]string{hubbleOwnershipAnnotation: fieldManager})
	unmanaged := candidates[1].DeepCopy()
	for _, object := range []*unstructured.Unstructured{owned, unmanaged} {
		resource := ciliumTestResource(t, client, mapper, object)
		if _, err := resource.Create(context.Background(), object, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	if err := deleteHubbleObjects(context.Background(), client, mapper, candidates); err != nil {
		t.Fatal(err)
	}
	if _, err := ciliumTestResource(t, client, mapper, owned).Get(context.Background(), owned.GetName(), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("owned Hubble object get error = %v, want NotFound", err)
	}
	if _, err := ciliumTestResource(t, client, mapper, unmanaged).Get(context.Background(), unmanaged.GetName(), metav1.GetOptions{}); err != nil {
		t.Fatalf("unmanaged Hubble object was deleted: %v", err)
	}
}

func TestValidateHubbleOwnershipRejectsUnmanagedNameCollision(t *testing.T) {
	item := cluster.Cluster{ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium}}
	candidates, err := ciliumHubbleObjects(item)
	if err != nil {
		t.Fatal(err)
	}
	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	mapper := ciliumTestMapper(candidates)
	unmanaged := candidates[0].DeepCopy()
	if _, err := ciliumTestResource(t, client, mapper, unmanaged).Create(context.Background(), unmanaged, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := validateHubbleOwnership(context.Background(), client, mapper, candidates); err == nil || !strings.Contains(err.Error(), "unmanaged Hubble") {
		t.Fatalf("Hubble ownership validation error = %v", err)
	}
}

func TestDeleteStaleCiliumAnnouncementsRemovesOnlyOwnedAlternative(t *testing.T) {
	for _, tt := range []struct {
		name string
		item cluster.Cluster
		want string
	}{
		{name: "BGP becomes L2", item: cluster.Cluster{ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true}}, want: "CiliumBGPClusterConfig,CiliumBGPPeerConfig,CiliumBGPAdvertisement"},
		{name: "L2 becomes BGP", item: cluster.Cluster{ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true, BGP: true}}, want: "CiliumL2AnnouncementPolicy"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			candidates, err := staleCiliumAnnouncementObjects(tt.item)
			if err != nil {
				t.Fatal(err)
			}
			if got := objectKinds(candidates); got != tt.want {
				t.Fatalf("stale announcement objects = %s, want %s", got, tt.want)
			}
			client := fake.NewSimpleDynamicClient(runtime.NewScheme())
			mapper := ciliumTestMapper(candidates)
			for i := range candidates {
				object := candidates[i].DeepCopy()
				object.SetAnnotations(map[string]string{announcementOwnershipAnnotation: fieldManager})
				if _, err := ciliumTestResource(t, client, mapper, object).Create(context.Background(), object, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}

			if err := deleteStaleCiliumAnnouncements(context.Background(), client, mapper, tt.item); err != nil {
				t.Fatal(err)
			}
			for i := range candidates {
				if _, err := ciliumTestResource(t, client, mapper, &candidates[i]).Get(context.Background(), candidates[i].GetName(), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
					t.Fatalf("stale %s %q get error = %v, want NotFound", candidates[i].GetKind(), candidates[i].GetName(), err)
				}
			}
		})
	}
}

func TestDeleteStaleCiliumAnnouncementsRejectsUnmanagedWithoutDeletingOwned(t *testing.T) {
	item := cluster.Cluster{ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true}}
	candidates, err := staleCiliumAnnouncementObjects(item)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) < 2 {
		t.Fatal("stale announcement candidates < 2, want at least two")
	}
	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	mapper := ciliumTestMapper(candidates)
	owned := candidates[0].DeepCopy()
	owned.SetAnnotations(map[string]string{announcementOwnershipAnnotation: fieldManager})
	if _, err := ciliumTestResource(t, client, mapper, owned).Create(context.Background(), owned, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	unmanaged := candidates[len(candidates)-1].DeepCopy()
	unmanaged.SetAnnotations(nil)
	unmanaged.SetManagedFields(nil)
	if _, err := ciliumTestResource(t, client, mapper, unmanaged).Create(context.Background(), unmanaged, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	err = deleteStaleCiliumAnnouncements(context.Background(), client, mapper, item)
	if err == nil || !strings.Contains(err.Error(), "unmanaged Cilium") {
		t.Fatalf("delete stale announcements error = %v, want unmanaged Cilium", err)
	}
	if _, err := ciliumTestResource(t, client, mapper, owned).Get(context.Background(), owned.GetName(), metav1.GetOptions{}); err != nil {
		t.Fatalf("owned stale object was deleted before validation completed: %v", err)
	}
}

func ciliumTestMapper(objects []unstructured.Unstructured) meta.RESTMapper {
	versions := make(map[schema.GroupVersion]struct{})
	for _, object := range objects {
		versions[object.GroupVersionKind().GroupVersion()] = struct{}{}
	}
	groups := make([]schema.GroupVersion, 0, len(versions))
	for version := range versions {
		groups = append(groups, version)
	}
	mapper := meta.NewDefaultRESTMapper(groups)
	for _, object := range objects {
		scope := meta.RESTScopeRoot
		if object.GetNamespace() != "" {
			scope = meta.RESTScopeNamespace
		}
		mapper.Add(object.GroupVersionKind(), scope)
	}
	return mapper
}

func ciliumTestResource(t *testing.T, client *fake.FakeDynamicClient, mapper meta.RESTMapper, object *unstructured.Unstructured) dynamic.ResourceInterface {
	t.Helper()
	mapping, err := mapper.RESTMapping(object.GroupVersionKind().GroupKind(), object.GroupVersionKind().Version)
	if err != nil {
		t.Fatal(err)
	}
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		return client.Resource(mapping.Resource).Namespace(object.GetNamespace())
	}
	return client.Resource(mapping.Resource)
}

func establishedCRD(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": name},
		"status": map[string]any{
			"conditions": []any{map[string]any{"type": "Established", "status": "True"}},
		},
	}}
}

func TestWaitForCiliumCRDsMatchesWhatEachIntentInstalls(t *testing.T) {
	// The installed CRD sets are hardcoded per intent — deliberately not
	// derived from the production predicate — so a wrong wait set fails here
	// instead of mirroring the bug.
	for _, tt := range []struct {
		name      string
		lb, bgp   bool
		installed []string
	}{
		{
			name: "default L2 load balancer", lb: true,
			installed: []string{"ciliumloadbalancerippools.cilium.io", "ciliuml2announcementpolicies.cilium.io"},
		},
		{
			name: "bgp load balancer installs no l2 CRD", lb: true, bgp: true,
			installed: []string{
				"ciliumloadbalancerippools.cilium.io",
				"ciliumbgpclusterconfigs.cilium.io",
				"ciliumbgppeerconfigs.cilium.io",
				"ciliumbgpadvertisements.cilium.io",
			},
		},
		{
			name: "no load balancer installs no l2 CRD", lb: false,
			installed: []string{"ciliumloadbalancerippools.cilium.io"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			objects := []runtime.Object{}
			for _, name := range tt.installed {
				objects = append(objects, establishedCRD(name))
			}
			client := fake.NewSimpleDynamicClient(runtime.NewScheme(), objects...)
			item := cluster.Cluster{ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: tt.lb, BGP: tt.bgp}}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := waitForCiliumCRDs(ctx, client, time.Millisecond, item); err != nil {
				t.Fatalf("CRD wait demanded a CRD this intent's chart never installs: %v", err)
			}
		})
	}
}

func TestWaitForCiliumCRDsStillRequiresTheEnabledSet(t *testing.T) {
	// Only LB-IPAM present: a BGP intent must keep waiting for its trio.
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), establishedCRD("ciliumloadbalancerippools.cilium.io"))
	item := cluster.Cluster{ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true, BGP: true}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := waitForCiliumCRDs(ctx, client, time.Millisecond, item); err == nil {
		t.Fatal("BGP-enabled CRD wait passed without the BGP CRDs")
	}
}

func TestWaitForAPIServerRetriesRefusedDialsUntilServerListens(t *testing.T) {
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserved.Addr().String()
	_ = reserved.Close() // the port now refuses connections — client-go does not retry ECONNREFUSED itself
	const listenAfter = 300 * time.Millisecond
	served := make(chan struct{})
	go func() {
		time.Sleep(listenAfter)
		var listener net.Listener
		var err error
		// Another process can steal the released port; retry the bind briefly
		// instead of silently leaving the wait to hit its 10 s deadline.
		for attempt := 0; attempt < 40; attempt++ {
			listener, err = net.Listen("tcp", address)
			if err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if err != nil {
			return
		}
		server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write([]byte(`{"major":"1","minor":"34"}`))
		})}
		defer func() { _ = server.Close() }()
		close(served)
		_ = server.Serve(listener)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	if err := waitForAPIServer(ctx, &rest.Config{Host: "http://" + address}, 50*time.Millisecond); err != nil {
		t.Fatalf("API server wait did not survive refused dials: %v", err)
	}
	if elapsed := time.Since(start); elapsed < listenAfter {
		t.Fatalf("API server wait returned in %v, before the server could have been listening", elapsed)
	}
	<-served
}

func TestWaitForAPIServerFailsFastOnForbidden(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusForbidden)
		_, _ = writer.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403}`))
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	err := waitForAPIServer(ctx, &rest.Config{Host: server.URL}, 50*time.Millisecond)
	if err == nil {
		t.Fatal("API server wait passed against a forbidden endpoint")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("forbidden endpoint burned %v of the deadline, want fail-fast", elapsed)
	}
	if !strings.Contains(err.Error(), server.URL) {
		t.Fatalf("fail-fast error does not name the endpoint: %v", err)
	}
}

func TestWaitForAPIServerNamesTheEndpointOnDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := waitForAPIServer(ctx, &rest.Config{Host: "http://127.0.0.1:1"}, 10*time.Millisecond)
	if err == nil {
		t.Fatal("API server wait passed against a closed port")
	}
	if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Fatalf("API server wait error does not name the endpoint: %v", err)
	}
}

// TestAnnotateAPIServerTimeoutExplainsOfflineSandboxPulls covers the only
// diagnosis available at deadline time: with the mirror offline, a control
// plane that never answers is nearly always a static pod looping on an
// uncached sandbox image, and the bare deadline says nothing about it.
func TestAnnotateAPIServerTimeoutExplainsOfflineSandboxPulls(t *testing.T) {
	t.Parallel()
	deadline := fmt.Errorf("wait for kube-apiserver at https://172.30.0.5:6443: %w", context.DeadlineExceeded)
	tests := []struct {
		name          string
		err           error
		mirrorOffline bool
		wantHint      bool
	}{
		{name: "offline deadline", err: deadline, mirrorOffline: true, wantHint: true},
		{name: "online deadline", err: deadline, mirrorOffline: false},
		{name: "offline but not a deadline", err: errors.New("unauthorized"), mirrorOffline: true},
		{name: "no error", err: nil, mirrorOffline: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := annotateAPIServerTimeout(test.err, test.mirrorOffline)
			if test.err == nil {
				if got != nil {
					t.Fatalf("annotateAPIServerTimeout(nil) = %v, want nil", got)
				}
				return
			}
			if !errors.Is(got, test.err) {
				t.Fatalf("annotated error dropped the cause: %v", got)
			}
			hinted := strings.Contains(got.Error(), KubernetesSandboxImage)
			if hinted != test.wantHint {
				t.Fatalf("hint present = %v, want %v (error: %v)", hinted, test.wantHint, got)
			}
			if test.wantHint && !strings.Contains(got.Error(), "wait for kube-apiserver") {
				t.Fatalf("annotated error no longer names the wait: %v", got)
			}
		})
	}
}

// TestCiliumReconcileSurfacesSandboxHintOnAPIServerDeadline is the wiring: the
// hint has to reach the create path's error, not just the helper's tests.
func TestCiliumReconcileSurfacesSandboxHintOnAPIServerDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	reconciler := CiliumReconciler{PollInterval: 10 * time.Millisecond, MirrorOffline: true}
	_, err := reconciler.reconcile(ctx, cluster.Cluster{
		Name:               "demo",
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium},
	}, &rest.Config{Host: "http://127.0.0.1:1"}, nil, ingressPKI{})
	if err == nil {
		t.Fatal("Cilium reconcile passed against a closed API server port")
	}
	if !strings.Contains(err.Error(), KubernetesSandboxImage) {
		t.Fatalf("reconcile error does not mention the sandbox image: %v", err)
	}
}

func TestDeleteStaleCiliumAnnouncementsToleratesAbsentBGPCRDs(t *testing.T) {
	// On an L2-only cluster the BGP CRDs are never installed, so the stale-BGP
	// candidates cannot exist: an unmapped kind is "nothing to delete", not an
	// error.
	item := cluster.Cluster{ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true}}
	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	mapper := meta.NewDefaultRESTMapper(nil)
	if err := deleteStaleCiliumAnnouncements(context.Background(), client, mapper, item); err != nil {
		t.Fatalf("stale BGP cleanup with absent CRDs = %v, want nil", err)
	}
}

func TestWaitForAPIServerFailsFastOnUntrustedCertificate(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"major":"1","minor":"34"}`))
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	err := waitForAPIServer(ctx, &rest.Config{Host: server.URL}, 50*time.Millisecond)
	if err == nil {
		t.Fatal("API server wait trusted a certificate from an unknown authority")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("untrusted CA burned %v of the deadline, want fail-fast", elapsed)
	}
}

func expiredTestCertificate(t *testing.T) (tls.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "talos-box expired test"},
		NotBefore:             time.Now().Add(-2 * time.Hour),
		NotAfter:              time.Now().Add(-time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(certificatePEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	return certificate, certificatePEM
}

func TestWaitForAPIServerKeepsRetryingAnExpiredCertificate(t *testing.T) {
	// Guest clock skew right after boot presents as expired/not-yet-valid:
	// the condition heals by waiting, so it must retry to the deadline
	// instead of failing terminally like an unknown authority.
	certificate, certificatePEM := expiredTestCertificate(t)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"major":"1","minor":"34"}`))
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()
	const deadline = 500 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	start := time.Now()
	config := &rest.Config{Host: server.URL, TLSClientConfig: rest.TLSClientConfig{CAData: certificatePEM}}
	err := waitForAPIServer(ctx, config, 50*time.Millisecond)
	if err == nil {
		t.Fatal("API server wait accepted an expired certificate")
	}
	if elapsed := time.Since(start); elapsed < deadline {
		t.Fatalf("expired certificate failed terminally after %v, want retries to the %v deadline", elapsed, deadline)
	}
}

func TestWaitForAPIServerFailsFastOnIncompatibleCertificateUsage(t *testing.T) {
	// A serving cert whose EKU forbids server auth is fixed at issuance and
	// cannot heal by waiting — it must be terminal like an unknown authority.
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := x509.Certificate{
		SerialNumber:          big.NewInt(3),
		Subject:               pkix.Name{CommonName: "talos-box test CA"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, &caTemplate, &caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := x509.Certificate{
		SerialNumber: big.NewInt(4),
		Subject:      pkix.Name{CommonName: "talos-box client-only leaf"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, &leafTemplate, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"major":"1","minor":"34"}`))
	}))
	server.TLS = &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER, caDER}, PrivateKey: leafKey}},
		MinVersion:   tls.VersionTLS12,
	}
	server.StartTLS()
	defer server.Close()
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	config := &rest.Config{Host: server.URL, TLSClientConfig: rest.TLSClientConfig{CAData: caPEM}}
	err = waitForAPIServer(ctx, config, 50*time.Millisecond)
	if err == nil {
		t.Fatal("API server wait accepted a client-only serving certificate")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("incompatible-usage certificate burned %v of the deadline, want fail-fast", elapsed)
	}
}
