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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeClient struct {
	applied   []string
	bootstrap int
	kube      int
	kubeData  []byte
	bootErr   error
	readyErrs []error
	expected  [][]string
}

func (f *fakeClient) Apply(_ context.Context, node string, config []byte) error {
	if !strings.Contains(string(config), "name: flannel") {
		return errors.New("machine config did not select Talos-managed flannel")
	}
	f.applied = append(f.applied, node)
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
	return f.kubeData, nil
}

func (f *fakeClient) KubernetesReady(_ context.Context, _ []byte, expectedNodes []string) error {
	f.expected = append(f.expected, append([]string(nil), expectedNodes...))
	if len(f.readyErrs) == 0 {
		return nil
	}
	err := f.readyErrs[0]
	f.readyErrs = f.readyErrs[1:]
	return err
}

func TestFlannelReconcileAppliesOnlyMaintenanceThenBootstraps(t *testing.T) {
	home := t.TempDir()
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
	for _, wanted := range []string{"apply-config --insecure", "bootstrap", "kubeconfig", "export TALOSCONFIG=", "export KUBECONFIG="} {
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
