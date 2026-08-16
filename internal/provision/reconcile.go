// Package provision drives Talos's API for opt-in cluster provisioning and
// reconciles curated Kubernetes networking stacks. It persists only
// credentials and receives observed node state from the daemon; provisioning
// progress is never written to cluster state.
package provision

import (
	"bytes"
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

	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/extensions"
	"github.com/randax/talos-box/internal/manifests"
	"github.com/randax/talos-box/internal/shellquote"
	"github.com/randax/talos-box/internal/talosversion"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	machineryconfig "github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/generate"
	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	"github.com/siderolabs/talos/pkg/machinery/config/machine"
	v1alpha1 "github.com/siderolabs/talos/pkg/machinery/config/types/v1alpha1"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	configresource "github.com/siderolabs/talos/pkg/machinery/resources/config"
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
// ReconcileControlPlaneScheduling is authenticated as well: it reconciles an
// already-configured control plane with the cluster's current worker count and
// reports whether the machine config had drifted.
type TalosClient interface {
	Apply(context.Context, string, []byte) error
	ReconcileControlPlaneScheduling(ctx context.Context, node string, workerless bool) (bool, error)
	Bootstrap(context.Context, string) error
	Kubeconfig(context.Context, string) ([]byte, error)
	KubernetesReady(context.Context, []byte, []string) error
}

// LoadBalancerReconciler installs and verifies a curated CNI's host-side
// LoadBalancer implementation through the Kubernetes API.
type LoadBalancerReconciler interface {
	Reconcile(context.Context, cluster.Cluster, []byte) (LoadBalancerResult, error)
}

// StorageReconciler installs and verifies the requested storage implementation
// after Talos has produced a ready Kubernetes API.
type StorageReconciler interface {
	Reconcile(context.Context, cluster.Cluster, []byte) (StorageResult, error)
}

// BGPReconciler reasserts the host-side BGP speaker after Cilium has applied
// the in-cluster peer resources. The host process keeps no durable speaker
// state, so this belongs in every observed-state provisioning pass.
type BGPReconciler interface {
	ReconcileBGP(context.Context, cluster.Cluster) error
}

// BGPDisabler is optional because only the daemon owns a host speaker. It is
// intentionally separate from BGPReconciler so isolated render/reconcile
// callers need not manufacture host-network state.
type BGPDisabler interface {
	DisableBGP(context.Context, cluster.Cluster) error
}

// Request supplies all transient state required for one reconciliation.
type Request struct {
	Cluster      cluster.Cluster
	Observe      func(context.Context) ([]Node, error)
	Client       TalosClient
	LoadBalancer LoadBalancerReconciler
	Storage      StorageReconciler
	BGP          BGPReconciler
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

// Reconcile brings a curated CNI path to a usable Kubernetes API. It follows
// observations rather than recording progress: nodes in maintenance are given
// their generated machine config, already-configured control planes have their
// worker-less adaptations reconciled against the cluster's current worker count
// over the authenticated API (the only machine-config write tbx makes to a
// running node), then the control plane is bootstrapped idempotently and yields
// kubeconfig.
func Reconcile(ctx context.Context, request Request) (Result, error) {
	if request.Cluster.CNI != cluster.CNIFlannel && request.Cluster.CNI != cluster.CNICilium {
		return Result{}, nil
	}
	if request.Client == nil || request.Observe == nil {
		return Result{}, errors.New("CNI provisioning requires a Talos client and node observer")
	}
	if request.PollInterval <= 0 {
		request.PollInterval = defaultPollInterval
	}

	generated, err := generateMachineConfigs(request.Cluster)
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
			config, err := withNodeHostname(config, node.Name)
			if err != nil {
				return Result{}, fmt.Errorf("set hostname for node %q: %w", node.Name, err)
			}
			if err := request.Client.Apply(ctx, node.IP, config); err != nil {
				return Result{}, fmt.Errorf("apply machine config to %s: %w", node.Name, err)
			}
			result.Narration = append(result.Narration,
				fmt.Sprintf("machine config: ≈ talosctl apply-config --insecure --nodes %s --file %s.yaml", node.IP, roleConfigName(node.Role)),
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
		// Machine configs are applied only in maintenance mode, so crossing the
		// zero-worker boundary on a running cluster (tbx node add/remove) has to
		// reconcile the worker-less adaptations over the authenticated API.
		workerless := !clusterHasWorkers(request.Cluster)
		for _, node := range nodes {
			if node.Role != cluster.RoleControlPlane {
				continue
			}
			changed, err := schedulingWithRetry(ctx, request.Client, node.IP, workerless, request.PollInterval)
			if err != nil {
				return Result{}, fmt.Errorf("reconcile control plane scheduling on %s: %w", node.Name, err)
			}
			if !changed {
				continue
			}
			// The manual equivalent is a strategic merge patch, which edits the
			// node's own applied config: it moves both fields the reconcile
			// moves and nothing else. A whole-config apply is not equivalent —
			// it would drop the per-node hostname tbx injects at apply time —
			// and JSON6902 is unusable on the multi-document configs tbx
			// generates.
			result.Narration = append(result.Narration,
				fmt.Sprintf("machine config: ≈ talosctl patch mc --mode auto --talosconfig %s --nodes %[2]s --endpoints %[2]s --patch %[3]s",
					shellquote.Quote(generated.paths.talosconfig), node.IP, shellquote.Quote(controlPlaneSchedulingPatch(workerless))),
			)
		}
		if err := request.Client.Bootstrap(ctx, controlPlane.IP); err != nil && !alreadyBootstrapped(err) {
			return Result{}, fmt.Errorf("bootstrap Kubernetes: %w", err)
		}
		result.Narration = append(result.Narration,
			fmt.Sprintf("bootstrap: ≈ talosctl bootstrap --talosconfig %s --nodes %[2]s --endpoints %[2]s", shellquote.Quote(generated.paths.talosconfig), controlPlane.IP),
		)
		kubeconfig, err := kubeconfigWithRetry(ctx, request.Client, controlPlane.IP, request.PollInterval)
		if err != nil {
			return Result{}, fmt.Errorf("retrieve kubeconfig: %w", err)
		}
		if err := writeSecure(generated.paths.kubeconfig, kubeconfig); err != nil {
			return Result{}, fmt.Errorf("write kubeconfig: %w", err)
		}
		if request.Cluster.CNI == cluster.CNICilium {
			if request.LoadBalancer == nil {
				return Result{}, errors.New("cilium provisioning requires a Kubernetes reconciler")
			}
			if request.Cluster.BGP {
				if request.BGP == nil {
					return Result{}, errors.New("BGP provisioning requires a host BGP reconciler")
				}
				// The Cilium reconcile verifies the VIP from the host. Start the
				// host peer first so BGP advertisements can install that route
				// before the reachability probe runs.
				if err := request.BGP.ReconcileBGP(ctx, request.Cluster); err != nil {
					return Result{}, fmt.Errorf("reconcile host BGP: %w", err)
				}
				result.Narration = append(result.Narration,
					fmt.Sprintf("host BGP: ≈ tbx bgp enable %s", request.Cluster.Name),
				)
			}
			// Cilium is the CNI: cni.name none means Nodes cannot report Ready
			// until its chart is applied. Reconcile it before the Ready wait.
			loadBalancer, err := request.LoadBalancer.Reconcile(ctx, request.Cluster, kubeconfig)
			if err != nil {
				return Result{}, fmt.Errorf("reconcile Cilium: %w", err)
			}
			if request.Cluster.LB && !request.Cluster.BGP {
				if disabler, ok := request.BGP.(BGPDisabler); ok {
					// Cilium's reconciler has already applied L2 and removed its
					// owned BGP objects. Only now may the host withdraw the peer.
					if err := disabler.DisableBGP(ctx, request.Cluster); err != nil {
						return Result{}, fmt.Errorf("disable host BGP: %w", err)
					}
					result.Narration = append(result.Narration,
						fmt.Sprintf("host BGP: ≈ tbx bgp disable %s", request.Cluster.Name),
					)
				}
			}
			result.VIP = loadBalancer.VIP
			result.Narration = append(result.Narration, loadBalancer.Narration...)
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
			fmt.Sprintf("credentials: ≈ talosctl kubeconfig %s --talosconfig %s --nodes %[3]s --endpoints %[3]s", shellquote.Quote(generated.paths.kubeconfig), shellquote.Quote(generated.paths.talosconfig), controlPlane.IP),
			fmt.Sprintf("export TALOSCONFIG=%s", shellquote.Quote(generated.paths.talosconfig)),
			fmt.Sprintf("export KUBECONFIG=%s", shellquote.Quote(generated.paths.kubeconfig)),
		)
		if request.Cluster.CNI == cluster.CNIFlannel && request.Cluster.LB {
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

func generateMachineConfigs(item cluster.Cluster) (generated, error) {
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
		version = talosversion.Default
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
	cniName := machineCNIName(item)
	options := []generate.Option{
		generate.WithVersionContract(contract),
		generate.WithSecretsBundle(bundle),
		generate.WithEndpointList(controlPlaneEndpoints),
		generate.WithInstallDisk("/dev/vda"),
		generate.WithClusterCNIConfig(&v1alpha1.CNIConfig{CNIName: cniName}),
	}
	// A worker-less cluster has nowhere else to run workloads, so the CSI
	// components and the storage probe need the control plane schedulable.
	if !clusterHasWorkers(item) {
		options = append(options, generate.WithAllowSchedulingOnControlPlanes(true))
	}
	input, err := generate.NewInput(
		item.Name,
		"https://"+controlPlane.IP+":6443",
		constants.DefaultKubernetesVersion,
		options...,
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
		if ciliumDisablesKubeProxy(item) {
			bytes, err = disableKubeProxy(bytes)
			if err != nil {
				return generated{}, fmt.Errorf("disable kube-proxy in %s config: %w", role, err)
			}
		}
		bytes, err = applyProvisioningPrerequisites(bytes, manifestFacts(item))
		if err != nil {
			return generated{}, fmt.Errorf("patch %s config: %w", role, err)
		}
		if extensions.Requested(item.TalosExtensions, extensions.GVisor) {
			bytes, err = withGVisorUserNamespaces(bytes)
			if err != nil {
				return generated{}, fmt.Errorf("patch %s config: %w", role, err)
			}
		}
		// A worker-less control plane is also the only node that can announce
		// LoadBalancer VIPs, so Talos's generated exclusion label goes with
		// the taint.
		if role == cluster.RoleControlPlane && !clusterHasWorkers(item) {
			bytes, err = withoutLoadBalancerExclusion(bytes)
			if err != nil {
				return generated{}, fmt.Errorf("patch %s config: %w", role, err)
			}
		}
		bytes = addCatchAllMirror(bytes, item.SubnetIndex)
		configs[role] = bytes
	}
	talosconfig, err := input.Talosconfig()
	if err != nil {
		return generated{}, fmt.Errorf("generate talosconfig: %w", err)
	}
	return generated{talosconfig: talosconfig, configs: configs, paths: paths}, nil
}

// withGVisorUserNamespaces re-opens unprivileged user namespaces on nodes
// that will run gVisor pods: runsc forks its gofer into new user namespaces,
// and Talos's KSPP hardening pins user.max_user_namespaces to 0, which
// surfaces as a misleading ENOSPC at sandbox create. Requesting the curated
// gvisor extension is the consent to relax that hardening; the value mirrors
// the extension's documented prerequisite.
func withGVisorUserNamespaces(config []byte) ([]byte, error) {
	var document map[string]any
	if err := yaml.Unmarshal(config, &document); err != nil {
		return nil, fmt.Errorf("decode generated machine config: %w", err)
	}
	machineSection, ok := document["machine"].(map[string]any)
	if !ok {
		return nil, errors.New("generated machine config is missing machine settings")
	}
	sysctls, ok := machineSection["sysctls"].(map[string]any)
	if !ok {
		sysctls = map[string]any{}
		machineSection["sysctls"] = sysctls
	}
	sysctls["user.max_user_namespaces"] = "11255"
	return yaml.Marshal(document)
}

func applyProvisioningPrerequisites(config []byte, facts manifests.Facts) ([]byte, error) {
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

	var mirrorPatch map[string]any
	if err := yaml.Unmarshal([]byte(manifests.RegistryMirrors(facts)), &mirrorPatch); err != nil {
		return nil, fmt.Errorf("decode registry mirror patch: %w", err)
	}
	patchMachine, ok := mirrorPatch["machine"].(map[string]any)
	if !ok {
		return nil, errors.New("registry mirror patch is missing machine settings")
	}
	registries, ok := patchMachine["registries"].(map[string]any)
	if !ok {
		return nil, errors.New("registry mirror patch is missing registries settings")
	}
	machineSection["registries"] = registries

	patched, err := yaml.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode generated machine config: %w", err)
	}
	return patched, nil
}

func disableKubeProxy(config []byte) ([]byte, error) {
	var document map[string]any
	if err := yaml.Unmarshal(config, &document); err != nil {
		return nil, err
	}
	clusterConfig, ok := document["cluster"].(map[string]any)
	if !ok {
		return nil, errors.New("generated config lacks cluster section")
	}
	clusterConfig["proxy"] = map[string]any{"disabled": true}
	return yaml.Marshal(document)
}

func addCatchAllMirror(config []byte, subnetIndex int) []byte {
	return append(config, []byte(catchAllMirrorDocument(subnetIndex))...)
}

func machineCNIName(item cluster.Cluster) string {
	if item.CNI == cluster.CNICilium {
		return "none"
	}
	return string(item.CNI)
}

func ciliumDisablesKubeProxy(item cluster.Cluster) bool {
	return item.CNI == cluster.CNICilium
}

func catchAllMirrorDocument(subnetIndex int) string {
	return fmt.Sprintf(`---
apiVersion: v1alpha1
kind: RegistryMirrorConfig
name: "*"
endpoints:
  - url: http://172.30.%d.1:%d
skipFallback: true
`, subnetIndex, manifests.CatchAllPort)
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

// withNodeHostname sets machine.network.hostname on a role-level config so the
// guest — and therefore its Kubernetes node — carries the tbx node name.
// KubernetesReady, DNS naming, and replica placement all key on these names,
// and DHCP on the substrate does not hand out hostnames.
func withNodeHostname(config []byte, name string) ([]byte, error) {
	// The generated config is multi-document YAML (the v1alpha1 config plus
	// appended documents like the catch-all mirror); patch only the first
	// document and carry the rest through untouched.
	first, rest, multiDocument := bytes.Cut(config, []byte("\n---\n"))
	var document map[string]any
	if err := yaml.Unmarshal(first, &document); err != nil {
		return nil, fmt.Errorf("decode machine config: %w", err)
	}
	machineSection, ok := document["machine"].(map[string]any)
	if !ok {
		return nil, errors.New("machine config is missing machine settings")
	}
	networkSection, ok := machineSection["network"].(map[string]any)
	if !ok {
		networkSection = map[string]any{}
		machineSection["network"] = networkSection
	}
	networkSection["hostname"] = name
	patched, err := yaml.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode machine config: %w", err)
	}
	if multiDocument {
		patched = append(patched, []byte("---\n")...)
		patched = append(patched, rest...)
	}
	return patched, nil
}

// loadBalancerExclusionLabel keeps a node out of LoadBalancer announcements;
// Talos bakes it into control-plane configs and MetalLB and Cilium honor it.
const loadBalancerExclusionLabel = "node.kubernetes.io/exclude-from-external-load-balancers"

// withControlPlaneScheduling reconciles an already-applied control-plane
// machine config with the cluster's current worker count. Both adaptations
// move together: a worker-less control plane must be schedulable and must
// announce VIPs, and a cluster that regained a worker must return to the
// stock control-plane posture. It reports whether the config actually drifted
// so callers can skip a no-op apply.
func withControlPlaneScheduling(config []byte, workerless bool) ([]byte, bool, error) {
	// The applied config is multi-document YAML; patch only the first
	// document and carry the rest through untouched.
	first, rest, multiDocument := bytes.Cut(config, []byte("\n---\n"))
	var document map[string]any
	if err := yaml.Unmarshal(first, &document); err != nil {
		return nil, false, fmt.Errorf("decode machine config: %w", err)
	}
	machineSection, ok := document["machine"].(map[string]any)
	if !ok {
		return nil, false, errors.New("machine config is missing machine settings")
	}
	clusterSection, ok := document["cluster"].(map[string]any)
	if !ok {
		return nil, false, errors.New("machine config is missing cluster settings")
	}
	nodeLabels, _ := machineSection["nodeLabels"].(map[string]any)
	schedulable, _ := clusterSection["allowSchedulingOnControlPlanes"].(bool)
	_, excluded := nodeLabels[loadBalancerExclusionLabel]
	if schedulable == workerless && excluded == !workerless {
		return config, false, nil
	}
	clusterSection["allowSchedulingOnControlPlanes"] = workerless
	if workerless {
		delete(nodeLabels, loadBalancerExclusionLabel)
	} else {
		if nodeLabels == nil {
			nodeLabels = map[string]any{}
			machineSection["nodeLabels"] = nodeLabels
		}
		nodeLabels[loadBalancerExclusionLabel] = ""
	}
	patched, err := yaml.Marshal(document)
	if err != nil {
		return nil, false, fmt.Errorf("encode machine config: %w", err)
	}
	if multiDocument {
		patched = append(patched, []byte("---\n")...)
		patched = append(patched, rest...)
	}
	return patched, true, nil
}

// controlPlaneSchedulingPatch narrates withControlPlaneScheduling as the
// strategic merge patch a user would apply by hand. Talos merges such a patch
// into the node's own applied config, so — unlike a whole-config apply — it
// leaves the per-node hostname and the appended documents alone; JSON6902 is
// not an option because tbx configs are multi-document.
func controlPlaneSchedulingPatch(workerless bool) string {
	exclusion := `""`
	if workerless {
		exclusion = `{"$patch":"delete"}`
	}
	return fmt.Sprintf(`{"cluster":{"allowSchedulingOnControlPlanes":%t},"machine":{"nodeLabels":{%q:%s}}}`,
		workerless, loadBalancerExclusionLabel, exclusion)
}

// withoutLoadBalancerExclusion drops the node.kubernetes.io/exclude-from-
// external-load-balancers label Talos bakes into control-plane configs;
// MetalLB and Cilium honor it and would otherwise never announce a VIP on a
// worker-less cluster.
func withoutLoadBalancerExclusion(config []byte) ([]byte, error) {
	var document map[string]any
	if err := yaml.Unmarshal(config, &document); err != nil {
		return nil, fmt.Errorf("decode machine config: %w", err)
	}
	machineSection, ok := document["machine"].(map[string]any)
	if !ok {
		return nil, errors.New("machine config is missing machine settings")
	}
	nodeLabels, ok := machineSection["nodeLabels"].(map[string]any)
	if !ok {
		return config, nil
	}
	delete(nodeLabels, loadBalancerExclusionLabel)
	patched, err := yaml.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode machine config: %w", err)
	}
	return patched, nil
}

func clusterHasWorkers(item cluster.Cluster) bool {
	for _, node := range item.Nodes {
		if node.Role == cluster.RoleWorker {
			return true
		}
	}
	return false
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

// kubeconfigWithRetry covers the short interval after bootstrap where Talos
// has accepted the request but apid is still restarting or its Kubernetes
// credentials are not yet available. Permanent failures remain immediate;
// an interrupted run is still always safely recoverable by rerunning tbx up.
func kubeconfigWithRetry(ctx context.Context, client TalosClient, node string, interval time.Duration) ([]byte, error) {
	for {
		kubeconfig, err := client.Kubeconfig(ctx, node)
		if err == nil {
			return kubeconfig, nil
		}
		switch talosclient.StatusCode(err) {
		case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
			if err := wait(ctx, interval); err != nil {
				return nil, err
			}
		default:
			return nil, err
		}
	}
}

// schedulingWithRetry tolerates the transient window in which apid is still
// restarting after a machine config apply, exactly like kubeconfigWithRetry.
// Permanent failures remain immediate so a pass fails loudly and rerunning
// tbx up recovers.
func schedulingWithRetry(ctx context.Context, client TalosClient, node string, workerless bool, interval time.Duration) (bool, error) {
	for {
		changed, err := client.ReconcileControlPlaneScheduling(ctx, node, workerless)
		if err == nil {
			return changed, nil
		}
		switch talosclient.StatusCode(err) {
		case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
			if err := wait(ctx, interval); err != nil {
				return false, err
			}
		default:
			return false, err
		}
	}
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

// ReconcileControlPlaneScheduling reads the node's active machine config over
// the authenticated API, reconciles the worker-less adaptations, and applies
// the result only when it drifted. Mode AUTO suffices: neither the scheduling
// flag nor the node label needs a reboot.
func (client MachineryClient) ReconcileControlPlaneScheduling(ctx context.Context, node string, workerless bool) (bool, error) {
	connection, err := client.secure(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = connection.Close() }()
	nodeCtx := talosclient.WithNode(ctx, node)
	active, err := safe.StateGetByID[*configresource.MachineConfig](nodeCtx, connection.COSI, configresource.ActiveID)
	if err != nil {
		return false, fmt.Errorf("read active machine config: %w", err)
	}
	current, err := active.Provider().Bytes()
	if err != nil {
		return false, fmt.Errorf("encode active machine config: %w", err)
	}
	patched, changed, err := withControlPlaneScheduling(current, workerless)
	if err != nil || !changed {
		return false, err
	}
	if _, err := connection.ApplyConfiguration(nodeCtx, &machineapi.ApplyConfigurationRequest{
		Data: patched,
		Mode: machineapi.ApplyConfigurationRequest_AUTO,
	}); err != nil {
		return false, fmt.Errorf("apply machine config: %w", err)
	}
	return true, nil
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
	nodes, err := kubernetesNodes(ctx, kubeconfig)
	if err != nil {
		return err
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

func kubernetesNodes(ctx context.Context, kubeconfig []byte) (kubernetesNodeList, error) {
	var nodes kubernetesNodeList
	transport, server, err := kubeTransport(kubeconfig)
	if err != nil {
		return nodes, err
	}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server+"/api/v1/nodes", nil)
	if err != nil {
		return nodes, err
	}
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		return nodes, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nodes, fmt.Errorf("kubernetes nodes API: %s", response.Status)
	}
	if err := json.NewDecoder(response.Body).Decode(&nodes); err != nil {
		return nodes, fmt.Errorf("decode Kubernetes nodes: %w", err)
	}
	return nodes, nil
}

// ControlPlaneSchedulingConverged observes the live posture the worker-less
// machine-config adaptations produce: worker-less control planes carry no
// NoSchedule taint and no load-balancer exclusion label, and control planes of
// a cluster that has workers carry both. It is the end-state probe for the
// zero-worker boundary; without it an interrupted mutation pass would leave
// drift that no other probe can see, so a rerun would report the cluster up to
// date forever.
func ControlPlaneSchedulingConverged(ctx context.Context, kubeconfig []byte, item cluster.Cluster) error {
	expected := make(map[string]struct{}, len(item.Nodes))
	for _, node := range item.Nodes {
		if node.Role == cluster.RoleControlPlane {
			expected[node.Name] = struct{}{}
		}
	}
	if len(expected) == 0 {
		return nil
	}
	reserved := clusterHasWorkers(item)
	nodes, err := kubernetesNodes(ctx, kubeconfig)
	if err != nil {
		return err
	}
	observed := make(map[string]bool, len(expected))
	for _, node := range nodes.Items {
		if _, wanted := expected[node.Metadata.Name]; !wanted {
			continue
		}
		if node.controlPlaneNoSchedule() != reserved {
			return fmt.Errorf("kubernetes node %q NoSchedule taint = %t, want %t", node.Metadata.Name, !reserved, reserved)
		}
		if _, excluded := node.Metadata.Labels[loadBalancerExclusionLabel]; excluded != reserved {
			return fmt.Errorf("kubernetes node %q load-balancer exclusion label = %t, want %t", node.Metadata.Name, excluded, reserved)
		}
		observed[node.Metadata.Name] = true
	}
	for name := range expected {
		if !observed[name] {
			return fmt.Errorf("kubernetes control plane %q was not found", name)
		}
	}
	return nil
}

// HubbleConverged verifies the optional Cilium Hubble deployments match the
// desired toggle. It is intentionally a small observed-state check for the
// fast path: a live VIP and Ready Nodes alone cannot prove that relay and UI
// converged after their setting changed.
func HubbleConverged(ctx context.Context, kubeconfig []byte, enabled bool) error {
	transport, server, err := kubeTransport(kubeconfig)
	if err != nil {
		return err
	}
	defer transport.CloseIdleConnections()
	for _, name := range []string{"hubble-relay", "hubble-ui"} {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, server+"/apis/apps/v1/namespaces/kube-system/deployments/"+name, nil)
		if err != nil {
			return err
		}
		response, err := (&http.Client{Transport: transport}).Do(request)
		if err != nil {
			return err
		}
		if !enabled {
			_ = response.Body.Close()
			if response.StatusCode != http.StatusNotFound {
				return fmt.Errorf("hubble is disabled but deployment %q still exists (%s)", name, response.Status)
			}
			continue
		}
		if response.StatusCode != http.StatusOK {
			_ = response.Body.Close()
			return fmt.Errorf("hubble deployment %q: %s", name, response.Status)
		}
		var deployment struct {
			Metadata struct {
				Generation int64 `json:"generation"`
			} `json:"metadata"`
			Status struct {
				ObservedGeneration int64 `json:"observedGeneration"`
				ReadyReplicas      int32 `json:"readyReplicas"`
				AvailableReplicas  int32 `json:"availableReplicas"`
			} `json:"status"`
		}
		decodeErr := json.NewDecoder(response.Body).Decode(&deployment)
		_ = response.Body.Close()
		if decodeErr != nil {
			return fmt.Errorf("decode Hubble deployment %q: %w", name, decodeErr)
		}
		if deployment.Status.ObservedGeneration < deployment.Metadata.Generation || deployment.Status.ReadyReplicas < 1 || deployment.Status.AvailableReplicas < 1 {
			return fmt.Errorf("hubble deployment %q is not Ready", name)
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
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Taints []struct {
			Key    string `json:"key"`
			Effect string `json:"effect"`
		} `json:"taints"`
	} `json:"spec"`
	Status struct {
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
	} `json:"status"`
}

// controlPlaneTaint is the taint Talos applies to control planes that are not
// meant to run workloads; a worker-less cluster drops it.
const controlPlaneTaint = "node-role.kubernetes.io/control-plane"

func (node kubernetesNode) controlPlaneNoSchedule() bool {
	for _, taint := range node.Spec.Taints {
		if taint.Key == controlPlaneTaint && taint.Effect == "NoSchedule" {
			return true
		}
	}
	return false
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
