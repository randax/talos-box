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
	"github.com/randax/talos-box/internal/shellquote"
	"github.com/siderolabs/talos/pkg/machinery/config/configloader"
	"github.com/siderolabs/talos/pkg/machinery/config/types/cri"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gopkg.in/yaml.v3"
)

type fakeClient struct {
	applied   []string
	configs   [][]byte
	bootstrap int
	kube      int
	kubeData  []byte
	kubeErrs  []error
	bootErr   error
	readyErrs []error
	expected  [][]string
	readyHook func()
}

type fakeLoadBalancer struct {
	calls int
	errs  []error
}

func (f *fakeLoadBalancer) Reconcile(context.Context, cluster.Cluster, []byte) (LoadBalancerResult, error) {
	f.calls++
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		if err != nil {
			return LoadBalancerResult{}, err
		}
	}
	return LoadBalancerResult{VIP: "172.30.0.200"}, nil
}

func TestCiliumMachineConfigRoutesRuntimeImagesThroughCatchAllMirror(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("demo", 3, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.TalosVersion = "v1.13.6"
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNICilium}
	generated, err := generateMachineConfigs(item)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := configloader.NewFromBytes(generated.configs[cluster.RoleControlPlane])
	if err != nil {
		t.Fatal(err)
	}
	var mirror *cri.RegistryMirrorConfigV1Alpha1
	for _, document := range provider.Documents() {
		candidate, ok := document.(*cri.RegistryMirrorConfigV1Alpha1)
		if ok && candidate.MetaName == "*" {
			mirror = candidate
		}
	}
	if mirror == nil || !mirror.SkipFallback() || len(mirror.RegistryEndpoints) != 1 || mirror.RegistryEndpoints[0].Endpoint() != "http://172.30.3.1:5059" {
		t.Fatalf("catch-all mirror = %#v", mirror)
	}
}

func (f *fakeClient) Apply(_ context.Context, node string, config []byte) error {
	if !strings.Contains(string(config), "name: flannel") && !strings.Contains(string(config), "name: none") {
		return fmt.Errorf("machine config did not select a curated CNI:\n%s", config)
	}
	f.applied = append(f.applied, node)
	f.configs = append(f.configs, append([]byte(nil), config...))
	return nil
}

func (f *fakeClient) Bootstrap(context.Context, string) error {
	f.bootstrap++
	return f.bootErr
}

func TestFlannelReconcileTreatsAlreadyBootstrappedAsSuccess(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("demo", 0, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.TalosVersion = "v1.13.6"
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel}
	client := &fakeClient{kubeData: []byte("kubeconfig"), bootErr: status.Error(codes.AlreadyExists, "already bootstrapped")}
	_, err = Reconcile(context.Background(), Request{Cluster: item, Client: client, Observe: func(context.Context) ([]Node, error) {
		return []Node{{Name: item.Nodes[0].Name, Role: cluster.RoleControlPlane, IP: item.Nodes[0].IP, Phase: PhaseConfigured}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if client.bootstrap != 1 {
		t.Fatalf("bootstrap calls = %d, want 1", client.bootstrap)
	}
	if client.kube != 1 {
		t.Fatalf("kubeconfig calls = %d, want 1", client.kube)
	}
}

func (f *fakeClient) Kubeconfig(context.Context, string) ([]byte, error) {
	f.kube++
	if len(f.kubeErrs) > 0 {
		err := f.kubeErrs[0]
		f.kubeErrs = f.kubeErrs[1:]
		return nil, err
	}
	return f.kubeData, nil
}

func TestKubeconfigWithRetryHandlesTransientTalosRestart(t *testing.T) {
	client := &fakeClient{
		kubeData: []byte("kubeconfig"),
		kubeErrs: []error{status.Error(codes.Unavailable, "apid restarting")},
	}
	got, err := kubeconfigWithRetry(context.Background(), client, "172.30.0.2", 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "kubeconfig" || client.kube != 2 {
		t.Fatalf("kubeconfig retry = %q after %d calls, want successful second call", got, client.kube)
	}
}

func TestKubeconfigWithRetryDoesNotHidePermanentErrors(t *testing.T) {
	client := &fakeClient{kubeErrs: []error{status.Error(codes.PermissionDenied, "bad credential")}}
	_, err := kubeconfigWithRetry(context.Background(), client, "172.30.0.2", 0)
	if err == nil || !strings.Contains(err.Error(), "bad credential") {
		t.Fatalf("kubeconfigWithRetry() error = %v, want permanent error", err)
	}
	if client.kube != 1 {
		t.Fatalf("kubeconfig calls = %d, want 1", client.kube)
	}
}

func (f *fakeClient) KubernetesReady(_ context.Context, _ []byte, expectedNodes []string) error {
	if f.readyHook != nil {
		f.readyHook()
	}
	f.expected = append(f.expected, append([]string(nil), expectedNodes...))
	if len(f.readyErrs) == 0 {
		return nil
	}
	err := f.readyErrs[0]
	f.readyErrs = f.readyErrs[1:]
	return err
}

func TestFlannelReconcileAppliesOnlyMaintenanceThenBootstraps(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home with space")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	item, err := cluster.New("demo", 0, 1, 1, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel}
	item.TalosVersion = "v1.13.6"
	client := &fakeClient{kubeData: []byte("kubeconfig")}
	observations := [][]Node{
		{
			{Name: "demo-cp-1", Role: cluster.RoleControlPlane, IP: item.Nodes[0].IP, Phase: PhaseMaintenance},
			{Name: "demo-worker-1", Role: cluster.RoleWorker, IP: item.Nodes[1].IP, Phase: PhaseConfigured},
		},
		{
			{Name: "demo-cp-1", Role: cluster.RoleControlPlane, IP: item.Nodes[0].IP, Phase: PhaseConfigured},
			{Name: "demo-worker-1", Role: cluster.RoleWorker, IP: item.Nodes[1].IP, Phase: PhaseConfigured},
		},
	}
	call := 0
	result, err := Reconcile(context.Background(), Request{
		Cluster: item,
		Observe: func(context.Context) ([]Node, error) {
			index := call
			if index >= len(observations) {
				index = len(observations) - 1
			}
			call++
			return observations[index], nil
		},
		Client: client, PollInterval: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := client.applied, []string{item.Nodes[0].IP}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("applied nodes = %v, want %v", got, want)
	}
	if client.bootstrap != 1 || client.kube != 1 {
		t.Fatalf("bootstrap=%d kubeconfig=%d, want one each", client.bootstrap, client.kube)
	}
	if result.TalosconfigPath == "" || result.KubeconfigPath == "" {
		t.Fatalf("result paths = %+v", result)
	}
	narration := strings.Join(result.Narration, "\n")
	for _, wanted := range []string{
		"apply-config --insecure",
		fmt.Sprintf("bootstrap: ≈ talosctl bootstrap --talosconfig %s --nodes %[2]s --endpoints %[2]s", shellquote.Quote(result.TalosconfigPath), item.Nodes[0].IP),
		fmt.Sprintf("credentials: ≈ talosctl kubeconfig %s --talosconfig %s --nodes %[3]s --endpoints %[3]s", shellquote.Quote(result.KubeconfigPath), shellquote.Quote(result.TalosconfigPath), item.Nodes[0].IP),
		"export TALOSCONFIG=" + shellquote.Quote(result.TalosconfigPath),
		"export KUBECONFIG=" + shellquote.Quote(result.KubeconfigPath),
	} {
		if !strings.Contains(narration, wanted) {
			t.Fatalf("narration missing %q:\n%s", wanted, narration)
		}
	}
	for _, path := range []string{filepath.Join(filepath.Dir(result.TalosconfigPath), "secrets.yaml"), result.TalosconfigPath, result.KubeconfigPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s permissions = %o, want 600", filepath.Base(path), got)
		}
	}
	info, err := os.Stat(filepath.Dir(result.TalosconfigPath))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("state directory permissions = %o, want 700", got)
	}
	for _, path := range []string{filepath.Join(home, ".talos", "config"), filepath.Join(home, ".kube", "config")} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("global credential %s was unexpectedly created: %v", path, err)
		}
	}
}

func TestFlannelReconcileRemintsDerivedCredentialsWithoutReapply(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("demo", 0, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel}
	item.TalosVersion = "v1.13.6"
	client := &fakeClient{kubeData: []byte("first")}
	request := Request{Cluster: item, Client: client, PollInterval: 0, Observe: func(context.Context) ([]Node, error) {
		return []Node{{Name: "demo-cp-1", Role: cluster.RoleControlPlane, IP: item.Nodes[0].IP, Phase: PhaseConfigured}}, nil
	}}
	result, err := Reconcile(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(result.TalosconfigPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(result.KubeconfigPath); err != nil {
		t.Fatal(err)
	}
	client.kubeData = []byte("reminted")
	result, err = Reconcile(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(client.applied) != 0 {
		t.Fatalf("configured node was re-applied: %v", client.applied)
	}
	if contents, err := os.ReadFile(result.KubeconfigPath); err != nil || string(contents) != "reminted" {
		t.Fatalf("reminted kubeconfig = %q, %v", contents, err)
	}
}

func TestFlannelReconcileRecoversFromEveryDocumentedInterruptionStage(t *testing.T) {
	interrupted := errors.New("simulated process interruption")
	tests := []struct {
		name         string
		loadBalancer bool
		firstPass    func(cluster.Cluster, *fakeClient, *fakeLoadBalancer) Request
		verify       func(*testing.T, *fakeClient, *fakeLoadBalancer)
	}{
		{
			name: "post-apply",
			firstPass: func(item cluster.Cluster, client *fakeClient, _ *fakeLoadBalancer) Request {
				observations := 0
				return Request{Cluster: item, Client: client, PollInterval: time.Nanosecond, Observe: func(context.Context) ([]Node, error) {
					observations++
					if observations == 1 {
						return []Node{{Name: item.Nodes[0].Name, Role: cluster.RoleControlPlane, IP: item.Nodes[0].IP, Phase: PhaseMaintenance}}, nil
					}
					return nil, interrupted
				}}
			},
			verify: func(t *testing.T, client *fakeClient, _ *fakeLoadBalancer) {
				if len(client.applied) != 1 {
					t.Fatalf("machine config apply calls = %d, want one before interruption and no reapply", len(client.applied))
				}
			},
		},
		{
			name: "pre-bootstrap",
			firstPass: func(item cluster.Cluster, client *fakeClient, _ *fakeLoadBalancer) Request {
				client.bootErr = interrupted
				return configuredRequest(item, client, nil)
			},
			verify: func(t *testing.T, client *fakeClient, _ *fakeLoadBalancer) {
				if client.bootstrap != 2 {
					t.Fatalf("bootstrap calls = %d, want interrupted attempt plus successful retry", client.bootstrap)
				}
			},
		},
		{
			name: "post-bootstrap",
			firstPass: func(item cluster.Cluster, client *fakeClient, _ *fakeLoadBalancer) Request {
				client.kubeErrs = []error{interrupted}
				return configuredRequest(item, client, nil)
			},
			verify: func(t *testing.T, client *fakeClient, _ *fakeLoadBalancer) {
				if client.kube != 2 {
					t.Fatalf("kubeconfig calls = %d, want interrupted attempt plus successful retry", client.kube)
				}
			},
		},
		{
			name:         "pre-SSA",
			loadBalancer: true,
			firstPass: func(item cluster.Cluster, client *fakeClient, loadBalancer *fakeLoadBalancer) Request {
				loadBalancer.errs = []error{interrupted}
				return configuredRequest(item, client, loadBalancer)
			},
			verify: func(t *testing.T, _ *fakeClient, loadBalancer *fakeLoadBalancer) {
				if loadBalancer.calls != 2 {
					t.Fatalf("SSA reconcile calls = %d, want interrupted attempt plus idempotent retry", loadBalancer.calls)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			item, err := cluster.New("demo", 0, 1, 0, cluster.NodeDefaults{})
			if err != nil {
				t.Fatal(err)
			}
			item.TalosVersion = "v1.13.6"
			item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: test.loadBalancer}
			client := &fakeClient{kubeData: []byte("kubeconfig")}
			loadBalancer := &fakeLoadBalancer{}

			if _, err := Reconcile(context.Background(), test.firstPass(item, client, loadBalancer)); !errors.Is(err, interrupted) {
				t.Fatalf("first Reconcile() error = %v, want simulated interruption", err)
			}
			client.bootErr = nil
			result, err := Reconcile(context.Background(), configuredRequest(item, client, loadBalancer))
			if err != nil {
				t.Fatalf("retry Reconcile() failed: %v", err)
			}
			if result.KubeconfigPath == "" {
				t.Fatal("retry did not converge to derived credentials")
			}
			test.verify(t, client, loadBalancer)
		})
	}
}

func configuredRequest(item cluster.Cluster, client *fakeClient, loadBalancer LoadBalancerReconciler) Request {
	return Request{
		Cluster: item, Client: client, LoadBalancer: loadBalancer, PollInterval: time.Nanosecond,
		Observe: func(context.Context) ([]Node, error) {
			return []Node{{Name: item.Nodes[0].Name, Role: cluster.RoleControlPlane, IP: item.Nodes[0].IP, Phase: PhaseConfigured}}, nil
		},
	}
}

func TestFlannelReconcileWaitsForEveryKubernetesNode(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("demo", 0, 1, 1, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.TalosVersion = "v1.13.6"
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel}
	client := &fakeClient{kubeData: []byte("kubeconfig"), readyErrs: []error{errors.New("worker is not Ready")}}
	result, err := Reconcile(context.Background(), Request{Cluster: item, Client: client, PollInterval: 0, Observe: func(context.Context) ([]Node, error) {
		return []Node{
			{Name: item.Nodes[0].Name, Role: cluster.RoleControlPlane, IP: item.Nodes[0].IP, Phase: PhaseConfigured},
			{Name: item.Nodes[1].Name, Role: cluster.RoleWorker, IP: item.Nodes[1].IP, Phase: PhaseConfigured},
		}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(client.readyErrs) != 0 {
		t.Fatal("provisioning returned before every Kubernetes node was Ready")
	}
	if got, want := fmt.Sprint(client.expected), "[[demo-cp-1 demo-worker-1] [demo-cp-1 demo-worker-1]]"; got != want {
		t.Fatalf("Kubernetes Ready expected node counts = %s, want %s", got, want)
	}
	if !strings.Contains(strings.Join(result.Narration, "\n"), "export KUBECONFIG=") {
		t.Fatal("provisioning omitted credentials after Kubernetes Ready")
	}
}

func TestGenerateFlannelAddsStorageAndMirrorPrerequisitesForEveryRole(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	for _, csi := range []cluster.CSI{"", cluster.CSILonghorn} {
		t.Run(fmt.Sprintf("csi=%s", csi), func(t *testing.T) {
			item, err := cluster.New("demo", 0, 1, 1, cluster.NodeDefaults{})
			if err != nil {
				t.Fatal(err)
			}
			item.TalosVersion = "v1.13.6"
			item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: csi}

			generated, err := generateMachineConfigs(item)
			if err != nil {
				t.Fatal(err)
			}

			want := []talosExtraMount{
				{Destination: "/var/local-path-provisioner", Type: "bind", Source: "/var/local-path-provisioner", Options: []string{"bind", "rshared", "rw"}},
				{Destination: "/var/lib/longhorn", Type: "bind", Source: "/var/lib/longhorn", Options: []string{"bind", "rshared", "rw"}},
			}
			for _, role := range []cluster.Role{cluster.RoleControlPlane, cluster.RoleWorker} {
				config, ok := generated.configs[role]
				if !ok {
					t.Fatalf("missing generated config for %s", role)
				}
				if got := decodeGeneratedKubeletExtraMounts(t, config); !equalTalosExtraMounts(got, want) {
					t.Fatalf("%s kubelet extraMounts = %#v, want %#v", role, got, want)
				}
				endpoint, skipFallback := decodeGeneratedCatchAllMirror(t, config)
				if endpoint != "http://172.30.0.1:5059" || !skipFallback {
					t.Fatalf("%s catch-all mirror = %q skipFallback=%v", role, endpoint, skipFallback)
				}
			}
		})
	}
}

func TestGenerateAllowsSchedulingOnControlPlanesOnlyWithoutWorkers(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	for _, test := range []struct {
		workers int
		want    bool
	}{{workers: 0, want: true}, {workers: 1, want: false}} {
		t.Run(fmt.Sprintf("workers=%d", test.workers), func(t *testing.T) {
			item, err := cluster.New("demo", 0, 1, test.workers, cluster.NodeDefaults{})
			if err != nil {
				t.Fatal(err)
			}
			item.TalosVersion = "v1.13.6"
			item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: cluster.CSILocalPath}

			generated, err := generateMachineConfigs(item)
			if err != nil {
				t.Fatal(err)
			}

			var document struct {
				Cluster struct {
					AllowSchedulingOnControlPlanes bool `yaml:"allowSchedulingOnControlPlanes"`
				} `yaml:"cluster"`
			}
			if err := yaml.Unmarshal(generated.configs[cluster.RoleControlPlane], &document); err != nil {
				t.Fatal(err)
			}
			if got := document.Cluster.AllowSchedulingOnControlPlanes; got != test.want {
				t.Fatalf("allowSchedulingOnControlPlanes = %v, want %v", got, test.want)
			}
		})
	}
}

func TestFlannelReconcileStopsAtContextDeadline(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("demo", 0, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.TalosVersion = "v1.13.6"
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err = Reconcile(ctx, Request{Cluster: item, Client: &fakeClient{}, PollInterval: time.Millisecond, Observe: func(context.Context) ([]Node, error) {
		return []Node{{Name: item.Nodes[0].Name, Role: cluster.RoleControlPlane, IP: item.Nodes[0].IP, Phase: PhaseMaintenance}}, nil
	}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Reconcile() error = %v, want context deadline", err)
	}
}

func TestFlannelLoadBalancerRequiresKubernetesReconciler(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("demo", 0, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.TalosVersion = "v1.13.6"
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true}
	_, err = Reconcile(context.Background(), Request{Cluster: item, Client: &fakeClient{kubeData: []byte("kubeconfig")}, Observe: func(context.Context) ([]Node, error) {
		return []Node{{Name: item.Nodes[0].Name, Role: cluster.RoleControlPlane, IP: item.Nodes[0].IP, Phase: PhaseConfigured}}, nil
	}})
	if err == nil || !strings.Contains(err.Error(), "LoadBalancer provisioning requires a Kubernetes reconciler") {
		t.Fatalf("Reconcile() error = %v, want missing LoadBalancer reconciler", err)
	}
}

func TestKubernetesReady(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		expected []string
		wantErr  string
	}{
		{name: "all expected nodes ready", status: http.StatusOK, expected: []string{"cp", "worker"}, body: `{"items":[{"metadata":{"name":"cp"},"status":{"conditions":[{"type":"Ready","status":"True"}]}},{"metadata":{"name":"worker"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}]}`},
		{name: "not ready", status: http.StatusOK, expected: []string{"cp"}, body: `{"items":[{"metadata":{"name":"cp"},"status":{"conditions":[{"type":"Ready","status":"False"}]}}]}`, wantErr: "not Ready"},
		{name: "unrelated node does not block readiness", status: http.StatusOK, expected: []string{"cp"}, body: `{"items":[{"metadata":{"name":"cp"},"status":{"conditions":[{"type":"Ready","status":"True"}]}},{"metadata":{"name":"stale"},"status":{"conditions":[{"type":"Ready","status":"False"}]}}]}`},
		{name: "API unavailable", status: http.StatusServiceUnavailable, expected: []string{"cp"}, wantErr: "503"},
		{name: "missing node", status: http.StatusOK, expected: []string{"cp", "worker"}, body: `{"items":[{"metadata":{"name":"cp"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}]}`, wantErr: "was not found"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, kubeconfig := readyServer(t, test.status, test.body)
			defer server.Close()
			err := KubernetesReady(context.Background(), kubeconfig, test.expected)
			if test.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("KubernetesReady() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestKubernetesReadyRejectsMalformedKubeconfig(t *testing.T) {
	if err := KubernetesReady(context.Background(), []byte("not: [valid"), []string{"cp"}); err == nil {
		t.Fatal("KubernetesReady() accepted malformed kubeconfig")
	}
	invalidCredentials := []byte(`current-context: test
clusters:
- name: test
  cluster:
    server: https://127.0.0.1:6443
    certificate-authority-data: invalid
contexts:
- name: test
  context:
    cluster: test
    user: admin
users:
- name: admin
  user:
    client-certificate-data: invalid
    client-key-data: invalid
`)
	if err := KubernetesReady(context.Background(), invalidCredentials, []string{"cp"}); err == nil || !strings.Contains(err.Error(), "decode kubeconfig CA") {
		t.Fatalf("KubernetesReady() error = %v, want invalid credential error", err)
	}
}

func TestHubbleConvergedRequiresTheDesiredDeployments(t *testing.T) {
	tests := []struct {
		name    string
		desired bool
		present bool
		ready   bool
		wantErr string
	}{
		{name: "enabled and ready", desired: true, present: true, ready: true},
		{name: "enabled but not ready", desired: true, present: true, wantErr: "not Ready"},
		{name: "enabled but missing", desired: true, wantErr: "404"},
		{name: "disabled and absent", desired: false},
		{name: "disabled but still present", desired: false, present: true, ready: true, wantErr: "disabled"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, kubeconfig := hubbleServer(t, test.present, test.ready)
			defer server.Close()
			err := HubbleConverged(context.Background(), kubeconfig, test.desired)
			if test.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("HubbleConverged() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func hubbleServer(t *testing.T, present, ready bool) (*httptest.Server, []byte) {
	t.Helper()
	certificate, certificatePEM, keyPEM := testCertificate(t)
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certificatePEM) {
		t.Fatal("parse test CA")
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/apis/apps/v1/namespaces/kube-system/deployments/hubble-relay" && request.URL.Path != "/apis/apps/v1/namespaces/kube-system/deployments/hubble-ui" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		if !present {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		if ready {
			_, _ = writer.Write([]byte(`{"metadata":{"generation":2},"status":{"observedGeneration":2,"readyReplicas":1,"availableReplicas":1}}`))
			return
		}
		_, _ = writer.Write([]byte(`{"metadata":{"generation":2},"status":{"observedGeneration":1}}`))
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

func readyServer(t *testing.T, status int, body string) (*httptest.Server, []byte) {
	t.Helper()
	certificate, certificatePEM, keyPEM := testCertificate(t)
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certificatePEM) {
		t.Fatal("parse test CA")
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/nodes" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.WriteHeader(status)
		_, _ = writer.Write([]byte(body))
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

func testCertificate(t *testing.T) (tls.Certificate, []byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "talos-box test"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
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
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	certificate, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return certificate, certificatePEM, keyPEM
}

type talosExtraMount struct {
	Destination string   `yaml:"destination"`
	Type        string   `yaml:"type"`
	Source      string   `yaml:"source"`
	Options     []string `yaml:"options"`
}

func decodeGeneratedKubeletExtraMounts(t *testing.T, config []byte) []talosExtraMount {
	t.Helper()

	var decoded struct {
		Machine struct {
			Kubelet struct {
				ExtraMounts []talosExtraMount `yaml:"extraMounts"`
			} `yaml:"kubelet"`
		} `yaml:"machine"`
	}
	if err := yaml.Unmarshal(config, &decoded); err != nil {
		t.Fatalf("decode generated config: %v", err)
	}
	return decoded.Machine.Kubelet.ExtraMounts
}

func decodeGeneratedCatchAllMirror(t *testing.T, config []byte) (string, bool) {
	t.Helper()
	var decoded struct {
		Machine struct {
			Registries struct {
				Mirrors map[string]struct {
					Endpoints    []string `yaml:"endpoints"`
					SkipFallback bool     `yaml:"skipFallback"`
				} `yaml:"mirrors"`
			} `yaml:"registries"`
		} `yaml:"machine"`
	}
	if err := yaml.Unmarshal(config, &decoded); err != nil {
		t.Fatalf("decode generated config mirrors: %v", err)
	}
	catchAll := decoded.Machine.Registries.Mirrors["*"]
	if len(catchAll.Endpoints) != 1 {
		t.Fatalf("catch-all endpoints = %v, want one", catchAll.Endpoints)
	}
	return catchAll.Endpoints[0], catchAll.SkipFallback
}

func equalTalosExtraMounts(got, want []talosExtraMount) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index].Destination != want[index].Destination || got[index].Type != want[index].Type || got[index].Source != want[index].Source || strings.Join(got[index].Options, ",") != strings.Join(want[index].Options, ",") {
			return false
		}
	}
	return true
}
