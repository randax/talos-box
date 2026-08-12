package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/provision"
)

const flannelProvisionTimeout = 10 * time.Minute

type provisionReconcileFunc func(context.Context, provision.Request) (provision.Result, error)

type provisionTask struct {
	item       cluster.Cluster
	ctx        context.Context
	generation uint64
	action     int
}

func (s *Server) handleProvisioningLocked(request Request) (any, []provisionTask, error) {
	switch request.Op {
	case "cluster.create":
		result, err := s.createCluster(request.Args)
		if err != nil {
			return nil, nil, err
		}
		item, err := cluster.Load(result.Name)
		if err != nil {
			return nil, nil, err
		}
		return &result, s.beginProvisionTasksLocked([]cluster.Cluster{item}), nil
	case "cluster.start":
		result, err := s.startCluster(request.Args)
		if err != nil {
			return nil, nil, err
		}
		item, err := cluster.Load(result.Name)
		if err != nil {
			return nil, nil, err
		}
		return &result, s.beginProvisionTasksLocked([]cluster.Cluster{item}), nil
	case "up":
		actions, err := s.up(request.Args)
		if err != nil {
			return nil, nil, err
		}
		items := make([]cluster.Cluster, len(actions))
		for i := range actions {
			item, err := cluster.Load(actions[i].Cluster)
			if err != nil {
				if actions[i].Kind == ActionMissing {
					continue
				}
				return nil, nil, fmt.Errorf("load %s for provisioning: %w", actions[i].Cluster, err)
			}
			items[i] = item
		}
		tasks := s.beginProvisionTasksLocked(items)
		for i := range tasks {
			tasks[i].action = indexAction(actions, tasks[i].item.Name)
		}
		return actions, tasks, nil
	default:
		return nil, nil, fmt.Errorf("operation %q is not provisionable", request.Op)
	}
}

func indexAction(actions []Action, name string) int {
	for i := range actions {
		if actions[i].Cluster == name {
			return i
		}
	}
	return -1
}

func (s *Server) beginProvisionTasksLocked(items []cluster.Cluster) []provisionTask {
	tasks := make([]provisionTask, 0, len(items))
	for _, item := range items {
		if item.CNI != cluster.CNIFlannel {
			continue
		}
		if s.provisions == nil {
			s.provisions = make(map[string]activeProvision)
		}
		if active, ok := s.provisions[item.Name]; ok {
			active.cancel()
		}
		if s.lifecycleContext == nil {
			s.lifecycleContext, s.lifecycleCancel = context.WithCancel(context.Background())
		}
		s.provisionSequence++
		ctx, cancel := context.WithCancel(s.lifecycleContext)
		s.provisions[item.Name] = activeProvision{generation: s.provisionSequence, cancel: cancel}
		tasks = append(tasks, provisionTask{item: item, ctx: ctx, generation: s.provisionSequence, action: -1})
	}
	return tasks
}

func (s *Server) finishProvision(task provisionTask) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if active, ok := s.provisions[task.item.Name]; ok && active.generation == task.generation {
		active.cancel()
		delete(s.provisions, task.item.Name)
	}
}

func (s *Server) cancelProvisionLocked(name string) {
	if active, ok := s.provisions[name]; ok {
		active.cancel()
		delete(s.provisions, name)
	}
}

func (s *Server) cancelAllProvisionsLocked() {
	if s.lifecycleCancel != nil {
		s.lifecycleCancel()
	}
	for name, active := range s.provisions {
		active.cancel()
		delete(s.provisions, name)
	}
}

func (s *Server) runProvisionTasks(data any, tasks []provisionTask) error {
	for i, task := range tasks {
		narration, err := s.provisionFlannel(task.ctx, task.item)
		s.finishProvision(task)
		if err != nil {
			s.opMu.Lock()
			for _, pending := range tasks[i+1:] {
				s.cancelProvisionLocked(pending.item.Name)
			}
			s.opMu.Unlock()
			return fmt.Errorf("provision %s: %w", task.item.Name, err)
		}
		switch result := data.(type) {
		case *ClusterSummary:
			result.Narration = narration
		case []Action:
			if task.action >= 0 {
				result[task.action].Narration = narration
			}
		}
	}
	return nil
}

func (s *Server) provisionFlannel(parent context.Context, item cluster.Cluster) ([]string, error) {
	if item.CNI != cluster.CNIFlannel {
		return nil, nil
	}
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(parent, flannelProvisionTimeout)
	defer cancel()
	reconcile := s.provisionReconcile
	if reconcile == nil {
		reconcile = provision.Reconcile
	}
	result, err := reconcile(ctx, provision.Request{
		Cluster: item,
		Client:  provision.MachineryClient{TalosconfigPath: filepath.Join(dir, "talosconfig")},
		LoadBalancer: provision.MetalLBReconciler{
			PollInterval: time.Second,
		},
		Observe: func(context.Context) ([]provision.Node, error) {
			s.opMu.Lock()
			defer s.opMu.Unlock()
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
