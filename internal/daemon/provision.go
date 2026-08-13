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

const (
	cniProvisionTimeout       = 10 * time.Minute
	kubernetesReadyTimeout    = 5 * time.Second
	ciliumConvergenceTimeout  = 15 * time.Second
	storageConvergenceTimeout = 30 * time.Second
)

var (
	kubernetesReadyProbe    = provision.KubernetesReady
	ciliumConvergenceProbe  = provision.CiliumConverged
	loadBalancerVIPProbe    = loadBalancerVIP
	storageConvergenceProbe = storageConverged
)

const storageProbeRetryBackoff = time.Minute

type provisionReconcileFunc func(context.Context, provision.Request) (provision.Result, error)

type provisionTask struct {
	item       cluster.Cluster
	ctx        context.Context
	generation uint64
	action     int
}

func (s *Server) handleProvisioningLocked(request Request, maintenance map[string]maintenanceObservation) (any, []provisionTask, error) {
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
		actions, err := s.upWithMaintenance(request.Args, maintenance)
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
		if item.CNI != cluster.CNIFlannel && item.CNI != cluster.CNICilium {
			continue
		}
		if s.provisions == nil {
			s.provisions = make(map[string]activeProvision)
		}
		if item.CNI == cluster.CNIFlannel && item.CSI != "" {
			s.invalidateStoragePhaseLocked(item.Name)
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

func (s *Server) beginNodeMutationProvisionLocked(item cluster.Cluster) []provisionTask {
	if !s.clusterRunning(item.Name) {
		return nil
	}
	return s.beginProvisionTasksLocked([]cluster.Cluster{item})
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
		narration, phase, err := s.provisionCNI(task.ctx, task.item, false)
		if task.item.CNI == cluster.CNIFlannel && task.item.CSI != "" {
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
				result[task.action].Kind = actionAfterProvision(result[task.action].Kind, narration)
			}
		}
	}
	return nil
}

func (s *Server) provisionCNI(parent context.Context, item cluster.Cluster, force bool) ([]string, StoragePhase, error) {
	if item.CNI != cluster.CNIFlannel && item.CNI != cluster.CNICilium {
		return nil, "", nil
	}
	// Once every desired outcome is observed healthy, a rerun is a genuine fast
	// no-op. Cilium additionally probes its optional Hubble deployments: a live
	// VIP and Ready Nodes alone cannot establish that that desired set converged.
	if !force && s.tryFastNoopReconcile(item) {
		if item.CNI == cluster.CNIFlannel && item.CSI != "" {
			return nil, StoragePhaseLive, nil
		}
		return nil, "", nil
	}
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		return nil, "", err
	}
	ctx, cancel := context.WithTimeout(parent, cniProvisionTimeout)
	defer cancel()
	var loadBalancer provision.LoadBalancerReconciler
	switch item.CNI {
	case cluster.CNIFlannel:
		loadBalancer = provision.MetalLBReconciler{PollInterval: time.Second}
	case cluster.CNICilium:
		loadBalancer = provision.CiliumReconciler{PollInterval: time.Second}
	}
	reconcile := s.provisionReconcile
	if reconcile == nil {
		reconcile = provision.Reconcile
	}
	request := provision.Request{
		Cluster:      item,
		Client:       provision.MachineryClient{TalosconfigPath: filepath.Join(dir, "talosconfig")},
		LoadBalancer: loadBalancer,
		BGP:          hostBGPReconciler{},
		Observe: func(context.Context) ([]provision.Node, error) {
			return s.observeProvisionNodes(item), nil
		},
		PollInterval: time.Second,
	}
	if item.CNI == cluster.CNIFlannel {
		switch item.CSI {
		case cluster.CSILocalPath:
			request.Storage = provision.LocalPathReconciler{PollInterval: time.Second}
		case cluster.CSILonghorn:
			request.Storage = provision.LonghornReconciler{PollInterval: time.Second}
		}
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
	return s.observeProvisionNodesWithRunning(item, running)
}

func (s *Server) observeProvisionNodesWithRunning(item cluster.Cluster, running map[string]bool) []provision.Node {
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

func (s *Server) tryFastNoopReconcile(item cluster.Cluster) bool {
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
		_, vipLive = loadBalancerVIPProbe(item)
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
			if !vipLive {
				return false
			}
			active, err := hostBGPActive(item.Name)
			if err != nil {
				return false
			}
			return active == item.BGP
		}
		return true
	}
	if item.CSI != "" {
		dir, err := cluster.Dir(item.Name)
		if err != nil {
			return false
		}
		kubeconfig, err := os.ReadFile(filepath.Join(dir, "kubeconfig"))
		if err != nil {
			return false
		}
		ctx, cancel := context.WithTimeout(context.Background(), storageConvergenceTimeout)
		defer cancel()
		if storageConvergenceProbe(ctx, item, kubeconfig) != nil {
			return false
		}
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

func storageConverged(ctx context.Context, item cluster.Cluster, kubeconfig []byte) error {
	switch item.CSI {
	case cluster.CSILocalPath:
		return provision.ProbeLocalPathStorage(ctx, kubeconfig, time.Second)
	case cluster.CSILonghorn:
		return provision.ProbeLonghornStorage(ctx, kubeconfig, time.Second)
	default:
		return nil
	}
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
	failures := make(map[string]storageProbeFailure, len(s.storageProbeFailures))
	for name, failure := range s.storageProbeFailures {
		failures[name] = failure
	}
	s.opMu.Unlock()

	for index := range statuses {
		status := &statuses[index]
		status.StoragePhase = ""
		status.StorageError = ""
		switch {
		case status.CNI != cluster.CNIFlannel, status.CSI == "", !status.Running:
		case known[status.Name] == StoragePhaseLive:
			status.StoragePhase = StoragePhaseLive
		case active[status.Name], !status.KubernetesReady:
			status.StoragePhase = StoragePhaseProvisioning
		default:
			status.StoragePhase = StoragePhaseProvisioning
			if failure, ok := failures[status.Name]; ok && time.Since(failure.at) < storageProbeRetryBackoff {
				status.StorageError = failure.message
			} else {
				s.beginStorageStatusProbe(status.Name)
			}
		}
		status.Hints = Hints(*status)
	}
}

func (s *Server) beginStorageStatusProbe(name string) {
	s.opMu.Lock()
	failure, failed := s.storageProbeFailures[name]
	backingOff := failed && time.Since(failure.at) < storageProbeRetryBackoff
	if !s.clusterRunning(name) || backingOff || s.storagePhases[name] == StoragePhaseLive || s.storageStatusProbes[name].cancel != nil {
		s.opMu.Unlock()
		return
	}
	if s.storageStatusProbes == nil {
		s.storageStatusProbes = make(map[string]activeStorageProbe)
	}
	parent := s.lifecycleContext
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	s.storageProbeSequence++
	generation := s.storageProbeSequence
	s.storageStatusProbes[name] = activeStorageProbe{generation: generation, cancel: cancel}
	s.opMu.Unlock()
	go s.runStorageStatusProbe(ctx, name, generation)
}

func (s *Server) runStorageStatusProbe(ctx context.Context, name string, generation uint64) {
	err := s.probeStorageStatus(ctx, name)
	s.opMu.Lock()
	defer s.opMu.Unlock()
	active, ok := s.storageStatusProbes[name]
	if !ok || active.generation != generation {
		return
	}
	active.cancel()
	delete(s.storageStatusProbes, name)
	if err == nil && s.clusterRunning(name) {
		delete(s.storageProbeFailures, name)
		s.recordStoragePhaseLocked(name, StoragePhaseLive)
	} else if err != nil && s.clusterRunning(name) {
		if s.storageProbeFailures == nil {
			s.storageProbeFailures = make(map[string]storageProbeFailure)
		}
		s.storageProbeFailures[name] = storageProbeFailure{message: err.Error(), at: time.Now()}
	}
}

func (s *Server) probeStorageStatus(parent context.Context, name string) error {
	dir, err := cluster.Dir(name)
	if err != nil {
		return err
	}
	kubeconfig, err := os.ReadFile(filepath.Join(dir, "kubeconfig"))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	probe := s.storageProbe
	if probe == nil {
		item, err := cluster.Load(name)
		if err != nil {
			return err
		}
		probe = func(ctx context.Context, kubeconfig []byte) error {
			switch item.CSI {
			case cluster.CSILocalPath:
				return provision.ProbeLocalPathStorage(ctx, kubeconfig, time.Second)
			case cluster.CSILonghorn:
				return provision.ProbeLonghornStorage(ctx, kubeconfig, time.Second)
			default:
				return nil
			}
		}
	}
	return probe(ctx, kubeconfig)
}

func (s *Server) invalidateStoragePhaseLocked(name string) {
	delete(s.storagePhases, name)
	delete(s.storageProbeFailures, name)
	if active, ok := s.storageStatusProbes[name]; ok {
		active.cancel()
		delete(s.storageStatusProbes, name)
	}
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
