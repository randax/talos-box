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
		if item.CSI == cluster.CSILocalPath {
			if s.storagePhases == nil {
				s.storagePhases = make(map[string]StoragePhase)
			}
			s.storagePhases[item.Name] = StoragePhaseProvisioning
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
		narration, phase, err := s.provisionFlannel(task.ctx, task.item)
		if task.item.CSI != "" {
			s.opMu.Lock()
			s.recordStoragePhaseLocked(task.item.Name, phase)
			s.opMu.Unlock()
		}
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

func (s *Server) provisionFlannel(parent context.Context, item cluster.Cluster) ([]string, StoragePhase, error) {
	if item.CNI != cluster.CNIFlannel {
		return nil, "", nil
	}
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		return nil, "", err
	}
	ctx, cancel := context.WithTimeout(parent, flannelProvisionTimeout)
	defer cancel()
	reconcile := s.provisionReconcile
	if reconcile == nil {
		reconcile = provision.Reconcile
	}
	request := provision.Request{
		Cluster: item,
		Client:  provision.MachineryClient{TalosconfigPath: filepath.Join(dir, "talosconfig")},
		LoadBalancer: provision.MetalLBReconciler{
			PollInterval: time.Second,
		},
		Observe: func(context.Context) ([]provision.Node, error) {
			return s.observeProvisionNodes(item), nil
		},
		PollInterval: time.Second,
	}
	if item.CSI == cluster.CSILocalPath {
		request.Storage = provision.LocalPathReconciler{PollInterval: time.Second}
	}
	result, err := reconcile(ctx, request)
	if err != nil {
		return nil, "", err
	}
	return result.Narration, storagePhaseFromProvisionResult(result), nil
}

func (s *Server) observeProvisionNodes(item cluster.Cluster) []provision.Node {
	running := make(map[string]bool, len(item.Nodes))
	s.opMu.Lock()
	for _, node := range item.Nodes {
		running[node.Name] = s.nodeRunning(item.Name, node.Name)
	}
	s.opMu.Unlock()

	lookupIP := s.nodeIPLookup
	if lookupIP == nil {
		lookupIP = cluster.LookupIP
	}
	probe := s.nodeProbe
	if probe == nil {
		probe = probeAPID
	}
	states := make([]provision.Node, 0, len(item.Nodes))
	for _, node := range item.Nodes {
		status := nodeStatusWith(node, item.SubnetIndex, running[node.Name], lookupIP, probe)
		states = append(states, provision.Node{Name: node.Name, Role: node.Role, IP: status.IP, Phase: provision.Phase(status.Phase)})
	}
	return states
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

func (s *Server) refreshStoragePhases(statuses []ClusterStatus) {
	s.opMu.Lock()
	known := make(map[string]StoragePhase, len(s.storagePhases))
	for name, phase := range s.storagePhases {
		known[name] = phase
	}
	active := make(map[string]bool, len(s.provisions))
	for name := range s.provisions {
		active[name] = true
	}
	s.opMu.Unlock()

	for index := range statuses {
		status := &statuses[index]
		status.StoragePhase = ""
		switch {
		case status.CNI != cluster.CNIFlannel, status.CSI != cluster.CSILocalPath, !status.Running:
		case known[status.Name] == StoragePhaseLive:
			status.StoragePhase = StoragePhaseLive
		case active[status.Name]:
			status.StoragePhase = StoragePhaseProvisioning
		case s.probeStorageStatus(status.Name) == nil:
			status.StoragePhase = StoragePhaseLive
			s.opMu.Lock()
			s.recordStoragePhaseLocked(status.Name, StoragePhaseLive)
			s.opMu.Unlock()
		default:
			status.StoragePhase = StoragePhaseProvisioning
		}
		status.Hints = Hints(*status)
	}
}

func (s *Server) probeStorageStatus(name string) error {
	dir, err := cluster.Dir(name)
	if err != nil {
		return err
	}
	kubeconfig, err := os.ReadFile(filepath.Join(dir, "kubeconfig"))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	probe := s.storageProbe
	if probe == nil {
		probe = func(ctx context.Context, kubeconfig []byte) error {
			return provision.ProbeLocalPathStorage(ctx, kubeconfig, time.Second)
		}
	}
	return probe(ctx, kubeconfig)
}

func (s *Server) recordStoragePhaseLocked(name string, phase StoragePhase) {
	if s.storagePhases == nil {
		s.storagePhases = make(map[string]StoragePhase)
	}
	if phase == "" {
		phase = StoragePhaseProvisioning
	}
	s.storagePhases[name] = phase
}

func storagePhaseFromProvisionResult(result provision.Result) StoragePhase {
	if result.StorageLive || result.StoragePhase == provision.StoragePhaseLive {
		return StoragePhaseLive
	}
	if result.StoragePhase == provision.StoragePhaseProvisioning {
		return StoragePhaseProvisioning
	}
	return ""
}
