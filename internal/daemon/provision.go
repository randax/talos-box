package daemon

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/provision"
)

const (
	cniProvisionTimeout      = 10 * time.Minute
	kubernetesReadyTimeout   = 5 * time.Second
	ciliumConvergenceTimeout = 15 * time.Second
)

var (
	kubernetesReadyProbe   = provision.KubernetesReady
	ciliumConvergenceProbe = provision.CiliumConverged
)

func (s *Server) provisionCNI(item cluster.Cluster, force bool) ([]string, error) {
	if item.CNI != cluster.CNIFlannel && item.CNI != cluster.CNICilium {
		return nil, nil
	}
	// Once every desired outcome is observed healthy, a rerun is a genuine fast
	// no-op. Cilium additionally probes its optional Hubble deployments: a live
	// VIP and Ready Nodes alone cannot establish that that desired set converged.
	if !force && s.fastNoopProvisioned(item) {
		return nil, nil
	}
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), cniProvisionTimeout)
	defer cancel()
	var loadBalancer provision.LoadBalancerReconciler
	switch item.CNI {
	case cluster.CNIFlannel:
		loadBalancer = provision.MetalLBReconciler{PollInterval: time.Second}
	case cluster.CNICilium:
		loadBalancer = provision.CiliumReconciler{PollInterval: time.Second}
	}
	result, err := provision.Reconcile(ctx, provision.Request{
		Cluster:      item,
		Client:       provision.MachineryClient{TalosconfigPath: filepath.Join(dir, "talosconfig")},
		LoadBalancer: loadBalancer,
		BGP:          hostBGPReconciler{},
		Observe: func(context.Context) ([]provision.Node, error) {
			states := make([]provision.Node, 0, len(item.Nodes))
			for _, node := range item.Nodes {
				status := nodeStatus(node, item.SubnetIndex, s.nodeRunning(item.Name, node.Name))
				phase := provision.Phase(status.Phase)
				states = append(states, provision.Node{Name: node.Name, Role: node.Role, IP: status.IP, Phase: phase})
			}
			return states, nil
		},
		PollInterval: time.Second,
	})
	if err != nil {
		return nil, err
	}
	return result.Narration, nil
}

func (s *Server) fastNoopProvisioned(item cluster.Cluster) bool {
	if !s.provisioningComplete(item) {
		return false
	}
	// A BGP -> L2 interruption can leave a process-local speaker behind after
	// Kubernetes has already converged. Disable is idempotent, so it closes that
	// gap without a progress marker or a full chart SSA pass.
	if item.CNI == cluster.CNICilium && item.LB && !item.BGP {
		return disableHostBGP(item.Name) == nil
	}
	return true
}

func (s *Server) provisioningComplete(item cluster.Cluster) bool {
	if !provisioningCredentialsPresent(item.Name) {
		return false
	}
	ready := kubernetesReady(item.Name, nodeNames(item))
	if !ready {
		return false
	}
	vipLive := false
	if item.LB {
		_, vipLive = loadBalancerVIP(item)
	}
	if item.CNI == cluster.CNICilium {
		dir, err := cluster.Dir(item.Name)
		if err != nil {
			return false
		}
		kubeconfig, err := os.ReadFile(filepath.Join(dir, "kubeconfig"))
		if err != nil {
			return false
		}
		ctx, cancel := context.WithTimeout(context.Background(), ciliumConvergenceTimeout)
		defer cancel()
		if ciliumConvergenceProbe(ctx, kubeconfig, item) != nil {
			return false
		}
		if item.LB {
			active, err := hostBGPActive(item.Name)
			if err != nil {
				return false
			}
			return active == item.BGP
		}
		return true
	}
	return provisioningCompleteEligible(item.ProvisioningIntent, true, ready, vipLive, true)
}

// provisioningCompleteEligible is deliberately stricter than the generic status view:
// every requested end state, including Hubble's symmetric toggle, must have
// a successful observed probe before a cluster is reported as up to date.
func provisioningCompleteEligible(intent cluster.ProvisioningIntent, credentials, ready, vipLive, hubbleConverged bool) bool {
	if (intent.CNI != cluster.CNIFlannel && intent.CNI != cluster.CNICilium) || !credentials || !ready {
		return false
	}
	if intent.LB && !vipLive {
		return false
	}
	return intent.CNI != cluster.CNICilium || hubbleConverged
}

func provisioningCredentialsPresent(name string) bool {
	dir, err := cluster.Dir(name)
	if err != nil {
		return false
	}
	for _, file := range []string{"secrets.yaml", "talosconfig", "kubeconfig"} {
		info, err := os.Stat(filepath.Join(dir, file))
		if err != nil || !info.Mode().IsRegular() {
			return false
		}
	}
	return true
}

func nodeNames(item cluster.Cluster) []string {
	names := make([]string, 0, len(item.Nodes))
	for _, node := range item.Nodes {
		names = append(names, node.Name)
	}
	return names
}

type hostBGPReconciler struct{}

func (hostBGPReconciler) ReconcileBGP(_ context.Context, item cluster.Cluster) error {
	return enableHostBGP(item)
}

func (hostBGPReconciler) DisableBGP(_ context.Context, item cluster.Cluster) error {
	return disableHostBGP(item.Name)
}

func kubernetesReady(name string, expectedNodes []string) bool {
	dir, err := cluster.Dir(name)
	if err != nil {
		return false
	}
	kubeconfig, err := os.ReadFile(filepath.Join(dir, "kubeconfig"))
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubernetesReadyTimeout)
	defer cancel()
	return kubernetesReadyProbe(ctx, kubeconfig, expectedNodes) == nil
}

func loadBalancerVIP(item cluster.Cluster) (string, bool) {
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		return "", false
	}
	kubeconfig, err := os.ReadFile(filepath.Join(dir, "kubeconfig"))
	if err != nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return provision.LiveVIP(ctx, item, kubeconfig)
}
