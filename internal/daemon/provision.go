package daemon

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/provision"
)

const cniProvisionTimeout = 10 * time.Minute

func (s *Server) provisionCNI(item cluster.Cluster) ([]string, error) {
	if item.CNI != cluster.CNIFlannel && item.CNI != cluster.CNICilium {
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

func kubernetesReady(name string, expectedNodes []string) bool {
	dir, err := cluster.Dir(name)
	if err != nil {
		return false
	}
	kubeconfig, err := os.ReadFile(filepath.Join(dir, "kubeconfig"))
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return provision.KubernetesReady(ctx, kubeconfig, expectedNodes) == nil
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
