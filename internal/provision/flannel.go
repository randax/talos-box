// Package provision drives Talos's API for the opt-in cluster provisioning
// path. It deliberately persists only credentials and receives observed node
// state from the daemon; provisioning progress is never written to cluster
// state.
package provision

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/manifests"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	machineryconfig "github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/generate"
	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	"github.com/siderolabs/talos/pkg/machinery/config/machine"
	v1alpha1 "github.com/siderolabs/talos/pkg/machinery/config/types/v1alpha1"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"go.yaml.in/yaml/v4"
	"google.golang.org/grpc/codes"
)

const defaultPollInterval = time.Second

// Phase is the observed Talos API state relevant to config application.
type Phase string

const (
	PhaseMaintenance Phase = "maintenance"
	PhaseConfigured  Phase = "configured"
)

// StoragePhase is the durable storage readiness projection the daemon can map
// into user-visible cluster status.
type StoragePhase string

const (
	StoragePhaseNone         StoragePhase = ""
	StoragePhaseProvisioning StoragePhase = "storage-provisioning"
	StoragePhaseLive         StoragePhase = "storage-live"
)

// Node is the provisioning-relevant projection of an observed cluster node.
type Node struct {
	Name  string
	Role  cluster.Role
	IP    string
	Phase Phase
}

// TalosClient is the subset of the machinery client this reconciliation needs.
// Apply is intentionally separate from Bootstrap: the former is allowed only
// while a node is in maintenance mode, while the latter is authenticated.
type TalosClient interface {
	Apply(context.Context, string, []byte) error
	Bootstrap(context.Context, string) error
	Kubeconfig(context.Context, string) ([]byte, error)
	KubernetesReady(context.Context, []byte, []string) error
}

// LoadBalancerReconciler installs and verifies the flannel LoadBalancer
// implementation after Talos has produced a ready Kubernetes API.
type LoadBalancerReconciler interface {
	Reconcile(context.Context, cluster.Cluster, []byte) (LoadBalancerResult, error)
}

// StorageReconciler installs and verifies the requested storage implementation
// after Talos has produced a ready Kubernetes API.
type StorageReconciler interface {
	Reconcile(context.Context, cluster.Cluster, []byte) (StorageResult, error)
}

// Request supplies all transient state required for one reconciliation.
type Request struct {
	Cluster      cluster.Cluster
	Observe      func(context.Context) ([]Node, error)
	Client       TalosClient
	LoadBalancer LoadBalancerReconciler
	Storage      StorageReconciler
	PollInterval time.Duration
}

// Result contains derived credential paths and manual-equivalent narration.
type Result struct {
	TalosconfigPath string
	KubeconfigPath  string
	Narration       []string
	VIP             string
	StoragePhase    StoragePhase
	StorageLive     bool
}

type generated struct {
	talosconfig *clientconfig.Config
	configs     map[cluster.Role][]byte
	paths       credentialPaths
}

type credentialPaths struct {
	dir         string
	secrets     string
	talosconfig string
	kubeconfig  string
}

// Reconcile brings exactly the Talos-managed flannel/no-LB path to a usable
// Kubernetes API. It follows observations rather than recording progress:
// only nodes currently in maintenance are given a machine config, then the
// configured control plane is bootstrapped idempotently and yields kubeconfig.
func Reconcile(ctx context.Context, request Request) (Result, error) {
	if request.Cluster.CNI != cluster.CNIFlannel {
		return Result{}, nil
	}
	if request.Client == nil || request.Observe == nil {
		return Result{}, errors.New("flannel provisioning requires a Talos client and node observer")
	}
	if request.PollInterval <= 0 {
		request.PollInterval = defaultPollInterval
	}

	generated, err := generateFlannel(request.Cluster)
	if err != nil {
		return Result{}, err
	}
	result := Result{
		TalosconfigPath: generated.paths.talosconfig,
		KubeconfigPath:  generated.paths.kubeconfig,
	}
	if err := writeTalosconfig(generated.paths.talosconfig, generated.talosconfig); err != nil {
		return Result{}, err
	}

	for {
		nodes, err := request.Observe(ctx)
		if err != nil {
			return Result{}, fmt.Errorf("observe Talos node state: %w", err)
		}
		if len(nodes) == 0 {
			return Result{}, errors.New("observe Talos node state: cluster has no nodes")
		}

		applied := false
		for _, node := range nodes {
			if node.Phase != PhaseMaintenance {
				continue
			}
			config, ok := generated.configs[node.Role]
			if !ok {
				return Result{}, fmt.Errorf("generate machine config for node %q: unknown role %q", node.Name, node.Role)
			}
			if err := request.Client.Apply(ctx, node.IP, config); err != nil {
				return Result{}, fmt.Errorf("apply machine config to %s: %w", node.Name, err)
			}
			result.Narration = append(result.Narration,
				fmt.Sprintf("≈ talosctl apply-config --insecure --nodes %s --file %s.yaml", node.IP, roleConfigName(node.Role)),
			)
			applied = true
		}
		if applied {
			if err := wait(ctx, request.PollInterval); err != nil {
				return Result{}, err
			}
			continue
		}

		controlPlane, allConfigured := configuredControlPlane(nodes)
		if !allConfigured {
			if err := wait(ctx, request.PollInterval); err != nil {
				return Result{}, err
			}
			continue
		}
		if err := request.Client.Bootstrap(ctx, controlPlane.IP); err != nil && !alreadyBootstrapped(err) {
			return Result{}, fmt.Errorf("bootstrap Kubernetes: %w", err)
		}
		result.Narration = append(result.Narration,
			fmt.Sprintf("≈ talosctl --nodes %s bootstrap", controlPlane.IP),
		)
		kubeconfig, err := request.Client.Kubeconfig(ctx, controlPlane.IP)
		if err != nil {
			return Result{}, fmt.Errorf("retrieve kubeconfig: %w", err)
		}
		if err := writeSecure(generated.paths.kubeconfig, kubeconfig); err != nil {
			return Result{}, fmt.Errorf("write kubeconfig: %w", err)
		}
		for {
			if err := request.Client.KubernetesReady(ctx, kubeconfig, nodeNames(nodes)); err == nil {
				break
			}
			if err := wait(ctx, request.PollInterval); err != nil {
				return Result{}, err
			}
		}
		result.Narration = append(result.Narration,
			fmt.Sprintf("≈ talosctl --nodes %s kubeconfig %s", controlPlane.IP, generated.paths.kubeconfig),
			fmt.Sprintf("export TALOSCONFIG=%s", generated.paths.talosconfig),
			fmt.Sprintf("export KUBECONFIG=%s", generated.paths.kubeconfig),
		)
		if request.Cluster.LB {
			if request.LoadBalancer == nil {
				return Result{}, errors.New("flannel LoadBalancer provisioning requires a Kubernetes reconciler")
			}
			loadBalancer, err := request.LoadBalancer.Reconcile(ctx, request.Cluster, kubeconfig)
			if err != nil {
				return Result{}, fmt.Errorf("reconcile flannel LoadBalancer: %w", err)
			}
			result.VIP = loadBalancer.VIP
			result.Narration = append(result.Narration, loadBalancer.Narration...)
		}
		if request.Cluster.CSI != "" {
			if request.Storage == nil {
				return Result{}, errors.New("flannel storage provisioning requires a Kubernetes reconciler")
			}
			storage, err := request.Storage.Reconcile(ctx, request.Cluster, kubeconfig)
			if err != nil {
				return Result{}, fmt.Errorf("reconcile flannel storage: %w", err)
			}
			result.StoragePhase = storage.Phase
			result.StorageLive = storage.Live
			result.Narration = append(result.Narration, storage.Narration...)
		}
		return result, nil
	}
}

func generateFlannel(item cluster.Cluster) (generated, error) {
	paths, err := credentials(item.Name)
	if err != nil {
		return generated{}, err
	}
	if err := os.MkdirAll(paths.dir, 0o700); err != nil {
		return generated{}, fmt.Errorf("create credential directory: %w", err)
	}
	if err := os.Chmod(paths.dir, 0o700); err != nil {
		return generated{}, fmt.Errorf("secure credential directory: %w", err)
	}
	version := item.TalosVersion
	if version == "" {
		version = "v1.13.6"
	}
	contract, err := machineryconfig.ParseContractFromVersion(version)
	if err != nil {
		return generated{}, fmt.Errorf("parse Talos version %q: %w", version, err)
	}
	bundle, err := loadOrCreateSecrets(paths.secrets, contract)
	if err != nil {
		return generated{}, err
	}
	controlPlane, controlPlaneEndpoints := clusterControlPlane(item)
	if controlPlane.IP == "" {
		return generated{}, errors.New("cluster has no control plane")
	}
	input, err := generate.NewInput(
		item.Name,
		"https://"+controlPlane.IP+":6443",
		constants.DefaultKubernetesVersion,
		generate.WithVersionContract(contract),
		generate.WithSecretsBundle(bundle),
		generate.WithEndpointList(controlPlaneEndpoints),
		generate.WithInstallDisk("/dev/vda"),
		generate.WithClusterCNIConfig(&v1alpha1.CNIConfig{CNIName: string(cluster.CNIFlannel)}),
	)
	if err != nil {
		return generated{}, fmt.Errorf("generate Talos config input: %w", err)
	}
	configs := make(map[cluster.Role][]byte, 2)
	for _, role := range []cluster.Role{cluster.RoleControlPlane, cluster.RoleWorker} {
		config, err := input.Config(machineType(role))
		if err != nil {
			return generated{}, fmt.Errorf("generate %s config: %w", role, err)
		}
		bytes, err := config.Bytes()
		if err != nil {
			return generated{}, fmt.Errorf("encode %s config: %w", role, err)
		}
		bytes, err = applyStoragePrerequisiteMounts(bytes)
		if err != nil {
			return generated{}, fmt.Errorf("patch %s config: %w", role, err)
		}
		configs[role] = bytes
	}
	talosconfig, err := input.Talosconfig()
	if err != nil {
		return generated{}, fmt.Errorf("generate talosconfig: %w", err)
	}
	return generated{talosconfig: talosconfig, configs: configs, paths: paths}, nil
}

func applyStoragePrerequisiteMounts(config []byte) ([]byte, error) {
	var document map[string]any
	if err := yaml.Unmarshal(config, &document); err != nil {
		return nil, fmt.Errorf("decode generated machine config: %w", err)
	}
	machineSection, ok := document["machine"].(map[string]any)
	if !ok {
		return nil, errors.New("generated machine config is missing machine settings")
	}
	kubeletSection, ok := machineSection["kubelet"].(map[string]any)
	if !ok {
		kubeletSection = map[string]any{}
		machineSection["kubelet"] = kubeletSection
	}

	mounts := make([]map[string]any, 0, len(manifests.StoragePrerequisiteKubeletExtraMounts()))
	for _, mount := range manifests.StoragePrerequisiteKubeletExtraMounts() {
		mounts = append(mounts, map[string]any{
			"destination": mount.Destination,
			"type":        mount.Type,
			"source":      mount.Source,
			"options":     append([]string(nil), mount.Options...),
		})
	}
	kubeletSection["extraMounts"] = mounts

	patched, err := yaml.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode generated machine config: %w", err)
	}
	return patched, nil
}

func loadOrCreateSecrets(path string, contract *machineryconfig.VersionContract) (*secrets.Bundle, error) {
	bundle, err := secrets.LoadBundle(path)
	if err == nil {
		return bundle, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("load secrets: %w", err)
	}
	bundle, err = secrets.NewBundle(secrets.NewFixedClock(time.Now()), contract)
	if err != nil {
		return nil, fmt.Errorf("generate secrets: %w", err)
	}
	data, err := yaml.Marshal(bundle)
	if err != nil {
		return nil, fmt.Errorf("encode secrets: %w", err)
	}
	if err := writeSecure(path, data); err != nil {
		return nil, fmt.Errorf("write secrets: %w", err)
	}
	return bundle, nil
}

func writeTalosconfig(path string, config *clientconfig.Config) error {
	data, err := config.Bytes()
	if err != nil {
		return fmt.Errorf("encode talosconfig: %w", err)
	}
	return writeSecure(path, data)
}

func writeSecure(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func credentials(name string) (credentialPaths, error) {
	dir, err := cluster.Dir(name)
	if err != nil {
		return credentialPaths{}, err
	}
	return credentialPaths{
		dir: dir, secrets: filepath.Join(dir, "secrets.yaml"),
		talosconfig: filepath.Join(dir, "talosconfig"), kubeconfig: filepath.Join(dir, "kubeconfig"),
	}, nil
}

func clusterControlPlane(item cluster.Cluster) (Node, []string) {
	var controlPlane Node
	var endpoints []string
	for _, node := range item.Nodes {
		if node.Role != cluster.RoleControlPlane {
			continue
		}
		endpoints = append(endpoints, node.IP)
		if controlPlane.IP == "" {
			controlPlane = Node{Name: node.Name, Role: node.Role, IP: node.IP}
		}
	}
	return controlPlane, endpoints
}

func configuredControlPlane(nodes []Node) (Node, bool) {
	var controlPlane Node
	for _, node := range nodes {
		if node.Phase != PhaseConfigured {
			return Node{}, false
		}
		if node.Role == cluster.RoleControlPlane && controlPlane.IP == "" {
			controlPlane = node
		}
	}
	return controlPlane, controlPlane.IP != ""
}

func nodeNames(nodes []Node) []string {
	names := make([]string, 0, len(nodes))
	for _, node := range nodes {
		names = append(names, node.Name)
	}
	return names
}

func machineType(role cluster.Role) machine.Type {
	if role == cluster.RoleControlPlane {
		return machine.TypeControlPlane
	}
	return machine.TypeWorker
}

func roleConfigName(role cluster.Role) string {
	if role == cluster.RoleControlPlane {
		return "controlplane"
	}
	return "worker"
}

func alreadyBootstrapped(err error) bool {
	return talosclient.StatusCode(err) == codes.AlreadyExists || strings.Contains(strings.ToLower(err.Error()), "already bootstrapped")
}

func wait(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		return nil
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// MachineryClient links directly to Talos machinery instead of executing
// talosctl. It creates an ephemeral maintenance client only for the insecure
// ApplyConfiguration call; bootstrap and kubeconfig use the credentialed
// talosconfig generated from the persisted secrets bundle.
type MachineryClient struct {
	TalosconfigPath string
}

func (client MachineryClient) Apply(ctx context.Context, node string, config []byte) error {
	connection, err := talosclient.New(ctx,
		talosclient.WithEndpoints(node),
		talosclient.WithTLSConfig(&tls.Config{InsecureSkipVerify: true}), //nolint:gosec // Talos maintenance API is intentionally unauthenticated
	)
	if err != nil {
		return err
	}
	defer func() { _ = connection.Close() }()
	_, err = connection.ApplyConfiguration(ctx, &machineapi.ApplyConfigurationRequest{
		Data: config,
		Mode: machineapi.ApplyConfigurationRequest_AUTO,
	})
	return err
}

func (client MachineryClient) Bootstrap(ctx context.Context, node string) error {
	connection, err := client.secure(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = connection.Close() }()
	return connection.Bootstrap(talosclient.WithNode(ctx, node), &machineapi.BootstrapRequest{})
}

func (client MachineryClient) Kubeconfig(ctx context.Context, node string) ([]byte, error) {
	connection, err := client.secure(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = connection.Close() }()
	return connection.Kubeconfig(talosclient.WithNode(ctx, node))
}

// KubernetesReady authenticates with the just-derived kubeconfig and requires
// each Talos node to report the Kubernetes Ready condition. This avoids
// claiming success merely because Talos accepted configuration and bootstrap.
func (MachineryClient) KubernetesReady(ctx context.Context, kubeconfig []byte, expectedNodes []string) error {
	return KubernetesReady(ctx, kubeconfig, expectedNodes)
}

// KubernetesReady verifies the API server reports every expected Node as
// Ready using the credentials generated for this cluster.
func KubernetesReady(ctx context.Context, kubeconfig []byte, expectedNodes []string) error {
	transport, server, err := kubeTransport(kubeconfig)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server+"/api/v1/nodes", nil)
	if err != nil {
		return err
	}
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("kubernetes nodes API: %s", response.Status)
	}
	var nodes kubernetesNodeList
	if err := json.NewDecoder(response.Body).Decode(&nodes); err != nil {
		return fmt.Errorf("decode Kubernetes nodes: %w", err)
	}
	expected := make(map[string]struct{}, len(expectedNodes))
	for _, name := range expectedNodes {
		expected[name] = struct{}{}
	}
	ready := make(map[string]bool, len(expected))
	for _, node := range nodes.Items {
		if _, wanted := expected[node.Metadata.Name]; !wanted {
			continue
		}
		if !node.ready() {
			return fmt.Errorf("kubernetes node %q is not Ready", node.Metadata.Name)
		}
		ready[node.Metadata.Name] = true
	}
	for name := range expected {
		if !ready[name] {
			return fmt.Errorf("kubernetes expected node %q was not found", name)
		}
	}
	return nil
}

type kubeconfigDocument struct {
	Clusters []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server                   string `yaml:"server"`
			CertificateAuthorityData string `yaml:"certificate-authority-data"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Contexts []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster string `yaml:"cluster"`
			User    string `yaml:"user"`
		} `yaml:"context"`
	} `yaml:"contexts"`
	CurrentContext string `yaml:"current-context"`
	Users          []struct {
		Name string `yaml:"name"`
		User struct {
			ClientCertificateData string `yaml:"client-certificate-data"`
			ClientKeyData         string `yaml:"client-key-data"`
		} `yaml:"user"`
	} `yaml:"users"`
}

type kubernetesNodeList struct {
	Items []kubernetesNode `json:"items"`
}

type kubernetesNode struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Status struct {
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
	} `json:"status"`
}

func (node kubernetesNode) ready() bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == "Ready" {
			return condition.Status == "True"
		}
	}
	return false
}

func kubeTransport(data []byte) (*http.Transport, string, error) {
	var document kubeconfigDocument
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, "", fmt.Errorf("decode kubeconfig: %w", err)
	}
	var clusterName, userName string
	for _, context := range document.Contexts {
		if context.Name == document.CurrentContext {
			clusterName, userName = context.Context.Cluster, context.Context.User
			break
		}
	}
	if clusterName == "" || userName == "" {
		return nil, "", errors.New("kubeconfig has no current context")
	}
	var server, encodedCA, encodedCert, encodedKey string
	for _, cluster := range document.Clusters {
		if cluster.Name == clusterName {
			server, encodedCA = cluster.Cluster.Server, cluster.Cluster.CertificateAuthorityData
			break
		}
	}
	for _, user := range document.Users {
		if user.Name == userName {
			encodedCert, encodedKey = user.User.ClientCertificateData, user.User.ClientKeyData
			break
		}
	}
	if server == "" || encodedCA == "" || encodedCert == "" || encodedKey == "" {
		return nil, "", errors.New("kubeconfig lacks server or client credentials")
	}
	ca, err := base64.StdEncoding.DecodeString(encodedCA)
	if err != nil {
		return nil, "", fmt.Errorf("decode kubeconfig CA: %w", err)
	}
	certificate, err := base64.StdEncoding.DecodeString(encodedCert)
	if err != nil {
		return nil, "", fmt.Errorf("decode kubeconfig client certificate: %w", err)
	}
	key, err := base64.StdEncoding.DecodeString(encodedKey)
	if err != nil {
		return nil, "", fmt.Errorf("decode kubeconfig client key: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, "", errors.New("decode kubeconfig CA certificate")
	}
	pair, err := tls.X509KeyPair(certificate, key)
	if err != nil {
		return nil, "", fmt.Errorf("load kubeconfig client certificate: %w", err)
	}
	return &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}}, strings.TrimRight(server, "/"), nil
}

func (client MachineryClient) secure(ctx context.Context) (*talosclient.Client, error) {
	return talosclient.New(ctx,
		talosclient.WithConfigFromFile(client.TalosconfigPath),
		talosclient.WithDefaultGRPCDialOptions(),
	)
}
