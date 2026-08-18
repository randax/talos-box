package daemon

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/provision"
)

const (
	// CNIProvisionTimeout and StorageProvisionTimeout are exported so the CLI
	// states the same deadline in its liveness output that the daemon holds
	// the request to — a mirrored constant would silently drift.
	CNIProvisionTimeout = 10 * time.Minute
	// A declared storage engine adds its image pulls (Longhorn alone is
	// gigabytes across every node) and the write/readback probe to the same
	// provisioning pass, so it gets a larger budget.
	StorageProvisionTimeout = 25 * time.Minute
	cniProvisionTimeout     = CNIProvisionTimeout
	storageProvisionTimeout = StorageProvisionTimeout
	kubernetesReadyTimeout  = 5 * time.Second
	// The scheduling posture is one small Nodes read, on the same budget as the
	// readiness probe it accompanies.
	controlPlaneSchedulingTimeout = 5 * time.Second
	ciliumConvergenceTimeout      = 15 * time.Second
	storageConvergenceTimeout     = 30 * time.Second
)

var (
	kubernetesReadyProbe    = provision.KubernetesReady
	ciliumConvergenceProbe  = provision.CiliumConverged
	loadBalancerVIPProbe    = loadBalancerVIP
	storageConvergenceProbe = storageConverged
	// The zero-worker boundary has no other end-state probe: an interrupted
	// mutation pass leaves the control plane's machine config drifted while
	// every health probe still passes.
	controlPlaneSchedulingProbe = provision.ControlPlaneSchedulingConverged
)

const storageProbeRetryBackoff = time.Minute

type provisionReconcileFunc func(context.Context, provision.Request) (provision.Result, error)

type provisionTask struct {
	item       cluster.Cluster
	ctx        context.Context
	generation uint64
	action     int
	// force skips the fast no-op path: a topology mutation changes the machine
	// config already-configured nodes need, which no health probe observes.
	force bool
	// done is closed once this task has run its course, so an operation that
	// destroys the cluster's files can wait the reconcile out instead of racing
	// it (#334).
	done chan struct{}
}

// finish releases everyone waiting on this task. It is the task's own end
// marker: every task returned by beginProvisionTasksLocked is closed exactly
// once, whether it ran, failed, or was cancelled before it started.
func (t provisionTask) finish() {
	if t.done != nil {
		close(t.done)
	}
}

func (s *Server) handleProvisioningLocked(request Request, maintenance map[string]maintenanceObservation, storage map[string]storageObservation, progress stageFunc) (any, []provisionTask, error) {
	switch request.Op {
	case "cluster.create":
		result, err := s.createCluster(request.Args, progress)
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
		actions, err := s.upWithObservations(request.Args, maintenance, storage)
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
		if item.CSI != "" {
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
		done := make(chan struct{})
		s.provisions[item.Name] = activeProvision{generation: s.provisionSequence, cancel: cancel, done: done}
		tasks = append(tasks, provisionTask{item: item, ctx: ctx, generation: s.provisionSequence, action: -1, done: done})
	}
	return tasks
}

// beginNodeMutationProvisionLocked schedules the forced reconcile a topology
// change needs. item is the post-mutation membership, and a reconcile can only
// converge over a fully-running one: its request lists every member, and
// configuredControlPlane/KubernetesReady poll DHCP leases and readiness for
// each of them. A partly-running cluster would spin to the provision timeout —
// synchronously on `node add`'s request path — and park storage at
// `provisioning`, so it schedules nothing (#332).
// It reports whether the reconcile was deferred, so a caller whose operation
// silently left the cluster unconverged can say so (#333).
func (s *Server) beginNodeMutationProvisionLocked(item cluster.Cluster) ([]provisionTask, bool) {
	if !s.allNodesRunning(item) {
		log.Printf("provision %s: cluster is only partly running, skipping reconcile", item.Name)
		// Membership or run state just changed, so a recorded `live` phase is
		// stale whether or not a reconcile follows: refreshStoragePhases
		// short-circuits on it and would keep reporting storage live over a
		// topology that no longer matches. Invalidating only drops the memo —
		// the phase is re-probed, not parked at `provisioning`.
		s.invalidateStoragePhaseLocked(item.Name)
		// A cluster with no members left has nothing to start and nothing to
		// reconcile, so "start the members" would be advice about a cluster that
		// no longer has any.
		return nil, clusterIsProvisioned(item) && len(item.Nodes) > 0
	}
	tasks := s.beginProvisionTasksLocked([]cluster.Cluster{item})
	for i := range tasks {
		tasks[i].force = true
	}
	return tasks, false
}

// clusterIsProvisioned reports whether the cluster has a provisioning stage at
// all. A substrate-only cluster never reconciles, so a deferred reconcile is
// not something its operator can act on and must not be warned about.
func clusterIsProvisioned(item cluster.Cluster) bool {
	return item.CNI == cluster.CNIFlannel || item.CNI == cluster.CNICilium
}

// nodeAddDeferredReconcileWarning explains the silence: the VM is up but no
// reconcile ran, so the new node sits in maintenance mode, unconfigured, until
// the operator brings the rest of the cluster back.
func nodeAddDeferredReconcileWarning(nodeName string) string {
	return fmt.Sprintf(
		"cluster members are stopped; node %s stays unconfigured until every member is running — start them to trigger provisioning",
		nodeName,
	)
}

// nodeRemoveDeferredReconcileWarning is the other half: the member is gone from
// state and disk, but the cluster has not been reconciled. It deliberately does
// not promise that starting the members finishes the job — the reconcile a start
// schedules is unforced and never deletes the removed member's Kubernetes Node
// object; that cleanup is named by stoppedClusterKubernetesNodeWarning.
func nodeRemoveDeferredReconcileWarning(nodeName string) string {
	return fmt.Sprintf(
		"cluster members are stopped; %s is gone from the cluster's state and disk, but the cluster is not reconciled until every member is running again",
		nodeName,
	)
}

func (s *Server) finishProvision(task provisionTask) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if active, ok := s.provisions[task.item.Name]; ok && active.generation == task.generation {
		active.cancel()
		delete(s.provisions, task.item.Name)
	}
}

// provisionDrainTimeout bounds how long an operation waits for a cancelled
// reconcile to actually stop. A reconcile that ignores its cancellation must
// not hang `cluster destroy` forever; the wait is logged if it expires. It is a
// var only so tests can shorten it.
var provisionDrainTimeout = 30 * time.Second

// drainProvision cancels a cluster's reconcile and waits for it to finish. It
// must be called WITHOUT opMu held: the reconcile's epilogue takes opMu to
// record its phase and retire itself, so waiting under the lock deadlocks.
//
// Cancelling alone is not enough for an operation that deletes the cluster's
// files: the goroutine can still be mid-write into the directory being removed,
// and its epilogue would re-register state for a cluster that no longer exists.
func (s *Server) drainProvision(name string) {
	s.opMu.Lock()
	active, ok := s.provisions[name]
	if ok {
		active.cancel()
		delete(s.provisions, name)
	}
	s.opMu.Unlock()
	if !ok || active.done == nil {
		return
	}
	select {
	case <-active.done:
	case <-time.After(provisionDrainTimeout):
		log.Printf("provision %s: reconcile did not stop within %s; proceeding", name, provisionDrainTimeout)
	}
}

// cancelProvisionForHandover cancels a cluster's reconcile without retiring it,
// so the operation that asked for the handover can still drain it: drainProvision
// waits on the task's done channel, and a retired entry leaves nothing to wait
// on — cancelling only asks the goroutine to stop, it does not stop it.
func (s *Server) cancelProvisionForHandover(name string) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if active, ok := s.provisions[name]; ok {
		active.cancel()
	}
}

func (s *Server) cancelProvisionLocked(name string) {
	if active, ok := s.provisions[name]; ok {
		active.cancel()
		delete(s.provisions, name)
	}
}

func (s *Server) cancelLifecycle() {
	if s.lifecycleCancel != nil {
		s.lifecycleCancel()
	}
}

func (s *Server) cancelAllProvisionsLocked() {
	for name, active := range s.provisions {
		active.cancel()
		delete(s.provisions, name)
	}
}

func (s *Server) runProvisionTasks(data any, tasks []provisionTask, progress stageFunc) error {
	// Every task must release its waiters, including the ones a failure never
	// gets to: a drain that outlived its task would hang the operation waiting
	// for it.
	finished := 0
	defer func() {
		for _, pending := range tasks[finished:] {
			pending.finish()
		}
	}()
	for i, task := range tasks {
		// The reconcile is the longest stretch of any verb that keeps it on the
		// request path, and it used to run behind a silent socket for its whole
		// budget (#273). It reports no intermediate progress of its own, so the
		// stage names the work and the bound the daemon holds the request to.
		if reconcilesCNI(task.item) {
			progress.stage("reconciling %s on cluster %s (up to %s)",
				task.item.CNI, task.item.Name, formatBootWindow(provisionTimeout(task.item)))
		}
		narration, phase, err := s.provisionCNI(task.ctx, task.item, task.force)
		if task.item.CSI != "" {
			s.opMu.Lock()
			s.recordStoragePhaseIfCurrentLocked(task.item.Name, task.generation, phase)
			s.opMu.Unlock()
		}
		s.finishProvision(task)
		finished = i + 1
		task.finish()
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

// runProvisionTasksAsync runs a post-mutation reconcile off the request path.
// The forced reconcile a node mutation schedules takes minutes against a
// cluster whose topology just changed, and holding the response for it left
// `tbx node remove` hanging with nothing to show for it (#314). The tasks were
// already registered under opMu, so a later mutation still supersedes them.
func (s *Server) runProvisionTasksAsync(op string, tasks []provisionTask) {
	if len(tasks) == 0 {
		return
	}
	s.backgroundProvisions.Add(1)
	go func() {
		defer s.backgroundProvisions.Done()
		// No progress sink: the request it followed has already been answered,
		// so there is no connection left to narrate onto.
		if err := s.runProvisionTasks(nil, tasks, nil); err != nil {
			log.Printf("%s: follow-up reconcile failed: %v", op, err)
		}
	}()
}

// reconcilesCNI reports whether a provisioning pass over this cluster has any
// work to do at all: only the curated CNIs are reconciled, and a substrate-only
// cluster must not be narrated as if it were.
func reconcilesCNI(item cluster.Cluster) bool {
	return item.CNI == cluster.CNIFlannel || item.CNI == cluster.CNICilium
}

// tasksReconcile reports whether any of these tasks has a reconcile to run, and
// so whether the verb that scheduled them still has work ahead of it.
func tasksReconcile(tasks []provisionTask) bool {
	for _, task := range tasks {
		if reconcilesCNI(task.item) {
			return true
		}
	}
	return false
}

// provisionTimeout budgets one provisioning pass by what it must converge.
func provisionTimeout(item cluster.Cluster) time.Duration {
	if item.CSI != "" {
		return storageProvisionTimeout
	}
	return cniProvisionTimeout
}

func (s *Server) provisionCNI(parent context.Context, item cluster.Cluster, force bool) ([]string, StoragePhase, error) {
	if !reconcilesCNI(item) {
		return nil, "", nil
	}
	// Once every desired outcome is observed healthy, a rerun is a genuine fast
	// no-op. Cilium additionally probes its optional Hubble deployments: a live
	// VIP and Ready Nodes alone cannot establish that that desired set converged.
	if !force && s.tryFastNoopReconcile(item) {
		if item.CSI != "" {
			return nil, StoragePhaseLive, nil
		}
		return nil, "", nil
	}
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		return nil, "", err
	}
	ctx, cancel := context.WithTimeout(parent, provisionTimeout(item))
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
	switch item.CSI {
	case cluster.CSILocalPath:
		request.Storage = provision.LocalPathReconciler{PollInterval: time.Second}
	case cluster.CSILonghorn:
		request.Storage = provision.LonghornReconciler{PollInterval: time.Second}
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
	ready := kubernetesReady(item.Name, nodeNames(item.Nodes))
	if !ready {
		return false
	}
	vipLive := false
	if item.LB {
		_, vipLive = loadBalancerVIPProbe(item)
	}
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		return false
	}
	kubeconfig, err := os.ReadFile(filepath.Join(dir, "kubeconfig"))
	if err != nil {
		return false
	}
	if !controlPlaneSchedulingConverged(item, kubeconfig) {
		return false
	}
	if item.CSI != "" {
		ctx, cancel := context.WithTimeout(context.Background(), storageConvergenceTimeout)
		defer cancel()
		if storageConvergenceProbe(ctx, item, kubeconfig) != nil {
			return false
		}
	}
	if item.CNI == cluster.CNICilium {
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

// controlPlaneSchedulingConverged decides the fast no-op on observed state
// rather than on the in-memory force flag: a mutation pass that failed or was
// superseded before it patched the control planes must still be recoverable by
// rerunning tbx up.
func controlPlaneSchedulingConverged(item cluster.Cluster, kubeconfig []byte) bool {
	ctx, cancel := context.WithTimeout(context.Background(), controlPlaneSchedulingTimeout)
	defer cancel()
	return controlPlaneSchedulingProbe(ctx, kubeconfig, item) == nil
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
		if err := provision.ProbeLonghornStorage(ctx, kubeconfig, time.Second); err != nil {
			return err
		}
		// The write/readback probe passes regardless of where replicas may
		// land, so storage convergence has to include the control-plane
		// scheduling posture or a mutation interrupted before the storage
		// stage would fast no-op forever. The scheduling read gets its own
		// small budget: sharing the probe's context would fail it on
		// whatever deadline the write/readback left over and force a full
		// reconcile of a healthy cluster.
		schedulingCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), controlPlaneSchedulingTimeout)
		defer cancel()
		return provision.LonghornSchedulingConverged(schedulingCtx, kubeconfig, item)
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
		case status.CSI == "", !status.Running:
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

// recordStoragePhaseIfCurrentLocked writes a finished pass's storage phase only
// while that pass is still the cluster's active one. A superseded task — one a
// newer reconcile replaced, or one a stop/suspend/destroy cancelled outright —
// finishes with a phase that describes a cluster state nobody is in any more,
// and letting it write would park a stale `live` over a fresh `provisioning`, or
// resurrect an entry for a cluster whose files are already gone (#334).
func (s *Server) recordStoragePhaseIfCurrentLocked(name string, generation uint64, phase StoragePhase) {
	active, ok := s.provisions[name]
	if !ok || active.generation != generation {
		log.Printf("provision %s: superseded pass, storage phase %q not recorded", name, phase)
		return
	}
	s.recordStoragePhaseLocked(name, phase)
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
