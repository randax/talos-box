package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/randax/talos-box/internal/balloon"
	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hostpressure"
	"github.com/randax/talos-box/internal/hypervisor"
	"github.com/randax/talos-box/internal/imagecache"
	"github.com/randax/talos-box/internal/mirror"
	"golang.org/x/sys/unix"
)

// Server owns all VMs started by one daemon process.
type Server struct {
	cache               *imagecache.Cache
	hypervisor          hypervisor.Hypervisor
	warmCache           func(context.Context, []string, imagecache.Architecture) (CacheWarmResult, error)
	checkCache          func(context.Context, []string, imagecache.Architecture, bool) (CacheCheckResult, error)
	boundMirrorGateways func() []string

	// mutationMu guards mutationLocks, whose per-cluster mutexes serialize
	// every operation that adds or deletes node disks — node.add, node.remove,
	// snapshot.restore — with the unlocked volume observations that gate them,
	// so no gate can vouch for a node another operation is about to delete.
	mutationMu    sync.Mutex
	mutationLocks map[string]*sync.Mutex

	// preemptions lets an operation queued on a cluster's mutation lock tell
	// the current holder that the work it is still doing for that cluster —
	// a create's boot wait, a reconcile — is not worth finishing.
	preemptions preemptions

	opMu sync.Mutex
	vms  map[string]map[string]hypervisor.Machine
	// vmStarts records when each node's VM was launched, so a node that never
	// answers can be aged against the boot window it was promised (#288).
	vmStarts map[string]map[string]time.Time
	// reachability records when a node that had been answering went quiet, so
	// a stall is aged from that transition rather than from VM uptime (#288).
	reachability reachabilityLog
	stalls       stallLog
	// stallWatch* drive the daemon-side stall observation: without it a stall
	// that nobody polls status for never reaches tbxd.log (#288).
	stallScanInterval time.Duration
	stallWatchMu      sync.Mutex
	stallWatchStop    chan struct{}
	stallWatchDone    chan struct{}

	provisions            map[string]activeProvision
	storagePhases         map[string]StoragePhase
	storageStatusProbes   map[string]activeStorageProbe
	storageProbeFailures  map[string]storageProbeFailure
	storageProbeSequence  uint64
	provisionSequence     uint64
	provisionReconcile    provisionReconcileFunc
	storageProbe          func(context.Context, []byte) error
	destroyVolumeCount    func(context.Context, cluster.Cluster) (int, error)
	storageVolumeClaims   func(context.Context, cluster.Cluster) ([]string, error)
	nodeVolumeCount       nodeVolumeCountFunc
	storageEngineDelete   func(context.Context, cluster.Cluster) error
	storageEngineValidate func(context.Context, cluster.Cluster) error
	deleteKubernetesNode  func(context.Context, cluster.Cluster, string) error
	nodeIPLookup          func(string, int) string
	nodeProbe             func(string) ProbeResult
	hostFreeMemory        func() (int, error)
	hostTotalMemory       func() (int, error)
	helperCheck           func() error
	maintenanceLoad       func(string) (cluster.Cluster, error)
	lifecycleContext      context.Context
	lifecycleCancel       context.CancelFunc
	mirrors               *mirror.Manager
	mirrorOffline         atomic.Bool
	// settingsPath is where daemon-wide modes are persisted so they survive a
	// restart (#318). An empty path disables persistence, which is what a
	// hand-built test server wants.
	settingsPath     string
	defaultSchematic string
	subnetSources    cluster.SubnetSources
	hostPressure     func(string) (hostpressure.Snapshot, error)

	// backgroundProvisions tracks reconciles that were moved off the request
	// path, so shutdown and tests can wait for them.
	backgroundProvisions sync.WaitGroup

	listenerMu   sync.Mutex
	listener     net.Listener
	closing      bool
	connections  map[net.Conn]struct{}
	connectionWG sync.WaitGroup
}

type activeProvision struct {
	generation uint64
	cancel     context.CancelFunc
	// done is closed when the task that owns this generation has finished.
	// Cancelling a reconcile only asks it to stop; an operation that is about
	// to delete the cluster's files has to wait until it actually has (#334).
	done chan struct{}
	// skipStorage records the scope this pass was registered with, so an
	// operation that supersedes it can tell whether cancelling it drops storage
	// work nobody else would resume (see beginBGPProvisionLocked).
	skipStorage bool
}

type activeStorageProbe struct {
	generation uint64
	cancel     context.CancelFunc
}

// storageProbeFailure is the last probe pass the daemon has to back off
// from. It covers both reasons to back off: a pass that failed, and a pass that
// never ran because the previous one's teardown was still finishing. Only the
// second is benign, so the record says which — the backoff is the same, the
// text the operator sees is not.
type storageProbeFailure struct {
	message string
	at      time.Time
	// pending marks the benign case: nothing is wrong, the daemon is still
	// clearing the previous pass's objects. Reporting it as StorageError would
	// tell the operator their storage stack failed a readiness probe.
	pending bool
}

type lockedListener struct {
	net.Listener
	lock *os.File
}

const machineStopTimeout = 30 * time.Second

func (l *lockedListener) Close() error {
	// Keep the process lock until exit so a replacement daemon cannot bind while
	// this daemon is still stopping VMs and cleaning up its socket.
	err := l.Listener.Close()
	runtime.KeepAlive(l.lock)
	return err
}

// NewServer creates a daemon using the default image cache and probes the host
// hypervisor once before accepting requests.
func NewServer(ctx context.Context) (*Server, error) {
	backend, err := hypervisor.New(ctx)
	if err != nil {
		return nil, err
	}
	cache, err := imagecache.NewDefault()
	if err != nil {
		return nil, err
	}
	root, err := imagecache.DefaultRoot()
	if err != nil {
		return nil, err
	}
	lifecycleContext, lifecycleCancel := context.WithCancel(ctx)
	server := &Server{
		cache:                 cache,
		hypervisor:            backend,
		vms:                   make(map[string]map[string]hypervisor.Machine),
		provisions:            make(map[string]activeProvision),
		storagePhases:         make(map[string]StoragePhase),
		storageStatusProbes:   make(map[string]activeStorageProbe),
		storageProbeFailures:  make(map[string]storageProbeFailure),
		lifecycleContext:      lifecycleContext,
		lifecycleCancel:       lifecycleCancel,
		mirrors:               mirror.NewManager(mirror.DefaultDir(root)),
		subnetSources:         cluster.SystemSubnetSources(),
		hostPressure:          hostpressure.SystemSnapshot,
		hostFreeMemory:        balloon.HostFreeMiB,
		hostTotalMemory:       balloon.HostTotalMiB,
		destroyVolumeCount:    countDestroyStorageVolumes,
		storageVolumeClaims:   listStorageVolumeClaims,
		nodeVolumeCount:       countNodeRemovalStorageVolumes,
		storageEngineDelete:   deleteConfiguredStorageEngine,
		storageEngineValidate: validateConfiguredStorageEngine,
	}
	// A persisted mode is re-applied before the socket exists, so the first
	// request after a restart already sees it (#318).
	server.applyPersistedSettings()
	server.boundMirrorGateways = func() []string {
		return server.mirrors.BoundGatewayIPs()
	}
	server.warmCache = func(ctx context.Context, refs []string, architecture imagecache.Architecture) (CacheWarmResult, error) {
		summary, err := server.mirrors.Warm(ctx, refs, architecture)
		if err != nil {
			return CacheWarmResult{}, err
		}
		result := CacheWarmResult{
			Warmed:          summary.Warmed,
			AlreadyComplete: summary.AlreadyComplete,
			Failed:          summary.Failed,
			Entries:         make([]CacheWarmEntry, 0, len(summary.Results)),
		}
		for _, entry := range summary.Results {
			status := CacheWarmStatusWarmed
			if entry.Error != "" {
				status = CacheWarmStatusFailed
			} else if entry.AlreadyComplete {
				status = CacheWarmStatusAlreadyComplete
			}
			result.Entries = append(result.Entries, CacheWarmEntry{
				Ref:    entry.Ref,
				Status: status,
				Reason: entry.Error,
			})
		}
		return result, nil
	}
	server.checkCache = func(ctx context.Context, refs []string, architecture imagecache.Architecture, deep bool) (CacheCheckResult, error) {
		summary, err := server.mirrors.Check(ctx, refs, architecture, deep)
		if err != nil {
			return CacheCheckResult{}, err
		}
		result := CacheCheckResult{
			Complete: summary.Complete,
			Failed:   summary.Failed,
			Entries:  make([]CacheCheckEntry, 0, len(summary.Results)),
		}
		for _, entry := range summary.Results {
			status := CacheCheckStatusComplete
			if entry.Error != "" {
				status = CacheCheckStatusFailed
			}
			result.Entries = append(result.Entries, CacheCheckEntry{
				Ref:    entry.Ref,
				Status: status,
				Reason: entry.Error,
			})
		}
		return result, nil
	}
	return server, nil
}

// Listen creates the daemon socket, replacing it only when it is stale.
func Listen(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create daemon directory: %w", err)
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open daemon lock: %w", err)
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("another daemon owns %s: %w", path, err)
	}
	closeLock := func() {
		_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
		_ = lock.Close()
	}

	listener, err := net.Listen("unix", path)
	if err != nil {
		if connection, dialErr := net.DialTimeout("unix", path, 100*time.Millisecond); dialErr == nil {
			_ = connection.Close()
			closeLock()
			return nil, fmt.Errorf("daemon socket is already in use: %s", path)
		}
		if removeErr := os.Remove(path); removeErr != nil {
			closeLock()
			return nil, fmt.Errorf("remove stale daemon socket: %w", removeErr)
		}
		listener, err = net.Listen("unix", path)
	}
	if err != nil {
		closeLock()
		return nil, fmt.Errorf("listen on daemon socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		closeLock()
		return nil, fmt.Errorf("set daemon socket permissions: %w", err)
	}
	return &lockedListener{Listener: listener, lock: lock}, nil
}

// Serve accepts daemon protocol connections until Shutdown closes listener.
func (s *Server) Serve(listener net.Listener) error {
	s.listenerMu.Lock()
	s.listener = listener
	if s.connections == nil {
		s.connections = make(map[net.Conn]struct{})
	}
	closing := s.closing
	s.listenerMu.Unlock()
	if closing {
		_ = listener.Close()
		return nil
	}
	// stalls must reach tbxd.log whether or not anybody polls status (#288)
	s.startStallWatch()

	for {
		connection, err := listener.Accept()
		if err != nil {
			s.listenerMu.Lock()
			closing := s.closing
			s.listenerMu.Unlock()
			if closing || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept daemon connection: %w", err)
		}
		s.listenerMu.Lock()
		if s.closing {
			s.listenerMu.Unlock()
			_ = connection.Close()
			continue
		}
		s.connections[connection] = struct{}{}
		s.connectionWG.Add(1)
		s.listenerMu.Unlock()
		go func() {
			defer func() {
				s.listenerMu.Lock()
				delete(s.connections, connection)
				s.listenerMu.Unlock()
				s.connectionWG.Done()
			}()
			s.serveConnection(connection)
		}()
	}
}

// Shutdown stops accepting requests and gracefully closes every running VM.
func (s *Server) Shutdown() error {
	s.listenerMu.Lock()
	s.closing = true
	listener := s.listener
	connections := make([]net.Conn, 0, len(s.connections))
	for connection := range s.connections {
		connections = append(connections, connection)
	}
	s.listenerMu.Unlock()
	// stop the stall watch before anything is torn down: it takes opMu and
	// reads the VM map this shutdown is about to empty
	s.stopStallWatch()
	if listener != nil {
		_ = listener.Close()
	}
	for _, connection := range connections {
		_ = connection.Close()
	}
	s.cancelLifecycle()
	s.opMu.Lock()
	s.cancelAllProvisionsLocked()
	s.opMu.Unlock()
	s.connectionWG.Wait()
	// the reconciles that outlive their request answer to the cancelled
	// lifecycle context, so this only waits for them to unwind
	s.backgroundProvisions.Wait()

	s.opMu.Lock()
	defer s.opMu.Unlock()
	var all []hypervisor.Machine
	for _, nodes := range s.vms {
		for _, machine := range nodes {
			all = append(all, machine)
		}
	}
	err := closeVMs(all)
	s.vms = make(map[string]map[string]hypervisor.Machine)
	s.forgetAllNodeTracking()
	if s.mirrors != nil {
		s.mirrors.Close()
	}
	return err
}

func (s *Server) serveConnection(connection net.Conn) {
	defer func() { _ = connection.Close() }()
	decoder := json.NewDecoder(connection)
	encoder := json.NewEncoder(connection)
	for {
		var request Request
		if err := decoder.Decode(&request); err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			if !errors.Is(err, net.ErrClosed) && !errors.Is(err, os.ErrClosed) {
				_ = encoder.Encode(failure(fmt.Errorf("decode request: %w", err)))
			}
			return
		}
		// Narration goes onto this same connection ahead of the result, and
		// only when the client asked for it; the sink is closed first so the
		// result is always the last message of the exchange.
		var progress stageFunc
		var sink *progressSink
		if request.Progress {
			sink = &progressSink{encoder: encoder}
			progress = sink.emit
		}
		response := s.dispatchWithProgress(request, progress)
		if sink != nil {
			sink.close()
		}
		if err := encoder.Encode(response); err != nil {
			return
		}
	}
}

// dispatch serves one request without narrating it.
func (s *Server) dispatch(request Request) Response {
	return s.dispatchWithProgress(request, nil)
}

func (s *Server) dispatchWithProgress(request Request, progress stageFunc) Response {
	if request.Op == "status" {
		return s.dispatchStatus(request)
	}
	if request.Op == "cluster.create" || request.Op == "cluster.start" || request.Op == "up" {
		return s.dispatchProvisioning(request, progress)
	}
	if isNodeMutation(request.Op) {
		return s.dispatchNodeMutation(request, progress)
	}
	if request.Op == "snapshot.restore" {
		return s.dispatchSnapshotRestore(request, progress)
	}
	if isBGPModeChange(request.Op) {
		return s.dispatchBGP(request, progress)
	}
	if request.Op == "cluster.destroy" {
		return s.dispatchDestroy(request)
	}
	if request.Op == "cluster.destroy.inspect" {
		return s.dispatchDestroyInspect(request)
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()

	data, err := s.handle(request, progress)
	if err != nil {
		return failure(err)
	}
	return success(data)
}

// dispatchDestroy drains the cluster's background reconcile before the
// destruction takes the operation lock. The reconcile writes into the very
// directory the destroy removes, and its epilogue re-registers daemon state for
// the cluster — state a later cluster of the same name would inherit. The wait
// cannot happen under opMu, which is why it is here and not in destroyCluster.
func (s *Server) dispatchDestroy(request Request) Response {
	var args destroyArgs
	if err := decodeArgs(request.Args, &args); err != nil {
		return failure(err)
	}
	// The cluster's disk-mutation lock closes the gap between the drain and
	// the destroy: a concurrent node mutation could otherwise register a fresh
	// reconcile after the drain and have it write into the directory being
	// removed.
	//
	// Announce the destroy before queueing on that lock. A create holding it is
	// in a boot wait, or a reconcile, whose outcome nobody wants any more —
	// waiting out a budget measured in minutes for a cluster about to be
	// deleted is time the operator spends for nothing. The announcement only
	// interrupts; the lock is still what serializes the destruction.
	releasePreemption := s.preemptions.request(args.Name)
	lock := s.clusterMutationLock(args.Name)
	lock.Lock()
	releasePreemption()
	defer lock.Unlock()
	s.drainProvision(args.Name)
	s.opMu.Lock()
	defer s.opMu.Unlock()
	data, err := s.destroyCluster(request.Args)
	if err != nil {
		return failure(err)
	}
	return success(data)
}

// dispatchDestroyInspect answers the destroy's data-loss question without the
// operation lock. The inspection reads the cluster's files and probes its
// Kubernetes API — it mutates nothing the lock protects — and its probe retries
// a control plane that can still heal, so running it under opMu would freeze
// every other operation for the whole retry budget: exactly the half-broken
// cluster an operator reaches for `tbx cluster destroy` on is the one that
// would make `tbx status` hang behind the warning it is about to print (#356).
func (s *Server) dispatchDestroyInspect(request Request) Response {
	inspection, err := s.destroyInspect(request.Args)
	if err != nil {
		return failure(err)
	}
	return success(inspection)
}

// lifecycle is the daemon's own lifetime, safe to call on a Server a test
// assembled without one. Work that must stop when the daemon does — but that
// deliberately outlives the operation that started it, such as a storage
// probe's teardown — hangs off this rather than off the operation's context.
func (s *Server) lifecycle() context.Context {
	if s.lifecycleContext == nil {
		return context.Background()
	}
	return s.lifecycleContext
}

func (s *Server) lifecycleTimeoutContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(s.lifecycle(), timeout)
}

// dispatchCreate serializes a create against the cluster's other mutations for
// the whole span it owns the cluster: the launch, the boot wait it answers
// from, and the reconcile that follows. The wait used to run outside every
// lock, so a destroy could delete the cluster while the create was still
// waiting for its nodes — and the create would then report a past-tense success
// for a cluster that no longer existed (#263).
//
// Status stays answerable throughout: neither the wait nor the reconcile holds
// opMu, and the lock order is the one dispatchDestroy and dispatchNodeMutation
// use — the cluster's mutation lock first, opMu only inside it, never the
// reverse.
func (s *Server) dispatchCreate(request Request, progress stageFunc) Response {
	var args createArgs
	if err := decodeArgs(request.Args, &args); err != nil {
		return failure(err)
	}
	lock := s.clusterMutationLock(args.Name)
	lock.Lock()
	defer lock.Unlock()

	s.opMu.Lock()
	data, tasks, err := s.handleProvisioningLocked(request, nil, nil, progress)
	s.opMu.Unlock()
	if err != nil {
		return failure(err)
	}
	// Everything left is work a destroy queued behind this lock has no use for.
	// Offer it up: the reconcile answers to its own context, and the boot wait
	// registers its own interrupt. Without this the queued destroy would sit
	// behind a provisioning budget measured in minutes.
	release := s.preemptions.register(args.Name, func() { s.cancelProvisionForHandover(args.Name) })
	defer release()
	// A create answers only once the substrate it promised exists: the VMs are
	// launched under opMu, but a launched VM is not a booted node, and a
	// past-tense success printed ahead of the boot is a lie the operator's next
	// command races (#263). The wait runs outside opMu — it takes minutes, and
	// status must stay answerable while it runs.
	if summary, ok := data.(*ClusterSummary); ok {
		summary.addWarnings(s.waitForNodesBooted(summary.Name, progress))
	}
	if err := s.runProvisionTasks(data, tasks, progress); err != nil {
		return failure(err)
	}
	return success(data)
}

func (s *Server) dispatchProvisioning(request Request, progress stageFunc) Response {
	if request.Op == "cluster.create" {
		return s.dispatchCreate(request, progress)
	}
	var maintenance map[string]maintenanceObservation
	if request.Op == "up" {
		discovered, err := s.observeUpMaintenance(request.Args)
		if err != nil {
			return failure(err)
		}
		// Confirm the externally-owned Talos phase once more before serialized
		// preflight. The locked decision also revalidates daemon-owned VM state;
		// the first observation is discovery, not authority.
		confirmed, err := s.observeUpMaintenance(request.Args)
		if err != nil {
			return failure(err)
		}
		maintenance = make(map[string]maintenanceObservation, len(confirmed))
		for name, observation := range confirmed {
			if first, ok := discovered[name]; ok && first.sameSnapshot(observation) {
				maintenance[name] = observation
			}
		}
	}
	var storage map[string]storageObservation
	if request.Op == "up" {
		var err error
		storage, err = s.observeUpStorage(request.Args)
		if err != nil {
			return failure(err)
		}
		s.opMu.Lock()
		err = s.validateUp(request.Args, maintenance, storage)
		s.opMu.Unlock()
		if err != nil {
			return failure(err)
		}
		if err := s.deleteUpStorageTransitions(request.Args, storage); err != nil {
			return failure(err)
		}
	}
	s.opMu.Lock()
	data, tasks, err := s.handleProvisioningLocked(request, maintenance, storage, progress)
	s.opMu.Unlock()
	if err != nil {
		return failure(err)
	}
	// A start's reconcile is the same one create keeps on the request path, and
	// it needs the same nodes: waiting here, outside opMu, is what lets it judge
	// a cluster that is up instead of one still booting (#364). `up` reaches the
	// identical path for every cluster it planned a start for, so the wait is
	// keyed on what the pass did, not on which verb asked for it.
	s.waitForStartedNodesBooted(data, tasks, progress)
	if err := s.runProvisionTasks(data, tasks, progress); err != nil {
		return failure(err)
	}
	return success(data)
}

// isNodeMutation names the operations that change one node's membership or run
// state, and so share the per-cluster mutation lock.
func isNodeMutation(op string) bool {
	switch op {
	case "node.add", "node.remove", "node.start", "node.stop":
		return true
	default:
		return false
	}
}

func (s *Server) dispatchNodeMutation(request Request, progress stageFunc) Response {
	var args nodeArgs
	if err := decodeArgs(request.Args, &args); err != nil {
		return failure(err)
	}
	// Every node mutation takes the cluster's disk-mutation lock, so gate and
	// operation serialize: two concurrent removals could otherwise each observe
	// the other's node as the surviving replica holder and delete both copies
	// of a volume, and a node added between another operation's observation and
	// its disk deletions would lose its disk unobserved.
	lock := s.clusterMutationLock(args.Cluster)
	lock.Lock()
	var removalWarning string
	if request.Op == "node.remove" {
		log.Printf("node.remove %s/%s: begin", args.Cluster, args.Name)
		warning, err := s.gateNodeRemoval(args)
		if err != nil {
			lock.Unlock()
			log.Printf("node.remove %s/%s: refused: %v", args.Cluster, args.Name, err)
			return failure(err)
		}
		removalWarning = warning
	}
	s.opMu.Lock()
	data, tasks, err := s.handleNodeMutationLocked(request, progress)
	s.opMu.Unlock()
	if err != nil {
		lock.Unlock()
		if removalWarning != "" {
			// the removal may have deleted state before failing; the data-loss
			// note must reach the user alongside the failure
			return failure(fmt.Errorf("%w (warning: %s)", err, removalWarning))
		}
		return failure(err)
	}
	warnings := []string{removalWarning}
	if request.Op == "node.remove" {
		// Still under the cluster mutation lock: a concurrent node.add could
		// otherwise reuse the removed name before this bounded cleanup runs and
		// have its freshly registered Kubernetes Node object deleted.
		warnings = append(warnings, s.deleteRemovedKubernetesNode(args.Cluster, args.Name))
	}
	lock.Unlock()
	if status, ok := data.(NodeStatus); ok {
		status.addWarnings(warnings...)
		data = status
	}
	// node.add keeps its reconcile on the request path: the added node is not a
	// usable cluster member until it is configured, and the CLI's confirmation
	// would otherwise outrun it. Every other node mutation answers as soon as
	// the substrate settled and reconciles behind the response (#314).
	if request.Op == "node.add" {
		if err := s.runProvisionTasks(data, tasks, progress); err != nil {
			return failure(err)
		}
		return success(data)
	}
	s.runProvisionTasksAsync(request.Op, tasks)
	if request.Op == "node.remove" {
		log.Printf("node.remove %s/%s: complete", args.Cluster, args.Name)
	}
	return success(data)
}

// isBGPModeChange names the operations that change a cluster's announcement
// mode, and so re-render its CNI.
func isBGPModeChange(op string) bool {
	return op == "bgp.enable" || op == "bgp.disable"
}

// dispatchBGP changes a cluster's announcement mode and keeps the reconcile that
// realizes it on the request path. That reconcile is the verb: the host speaker
// alone leaves Cilium announcing the old way, so answering ahead of it is the
// silent success #344 reported. It also means a failed apply fails the verb
// instead of surfacing only in the daemon log — the recorded mode stays, so a
// rerun resumes from it.
//
// The cluster's mutation lock is held for the state change only, the way
// dispatchNodeMutation holds it: the task is already registered under opMu, so a
// later mutation supersedes it without the reconcile blocking the lock.
func (s *Server) dispatchBGP(request Request, progress stageFunc) Response {
	var args nameArgs
	if err := decodeArgs(request.Args, &args); err != nil {
		return failure(err)
	}
	lock := s.clusterMutationLock(args.Name)
	lock.Lock()
	s.opMu.Lock()
	summary, tasks, err := s.setBGPLocked(request.Args, request.Op == "bgp.enable", progress)
	s.opMu.Unlock()
	lock.Unlock()
	if err != nil {
		return failure(err)
	}
	if err := s.runProvisionTasks(summary, tasks, progress); err != nil {
		return failure(err)
	}
	return success(summary)
}

// dispatchSnapshotRestore gates the restore's node deletions outside the
// operation lock, like dispatchNodeMutation: a restore drops every live node
// the snapshot did not capture, disks and all.
func (s *Server) dispatchSnapshotRestore(request Request, progress stageFunc) Response {
	var args snapshotArgs
	if err := decodeArgs(request.Args, &args); err != nil {
		return failure(err)
	}
	// restore shares the node mutations' per-cluster lock: they all delete or
	// add node disks, and no gate may observe a node another operation is
	// about to delete as a copy holder.
	lock := s.clusterMutationLock(args.Cluster)
	lock.Lock()
	warning, err := s.gateSnapshotRestore(args)
	if err != nil {
		lock.Unlock()
		return failure(err)
	}
	s.opMu.Lock()
	status, err := s.snapshotRestore(request.Args, progress)
	s.opMu.Unlock()
	lock.Unlock()
	if err != nil {
		if warning != "" {
			// the restore may have deleted disks before failing; the data-loss
			// note must reach the user alongside the failure
			return failure(fmt.Errorf("%w (warning: %s)", err, warning))
		}
		return failure(err)
	}
	// the gate's data-loss note and the restart's host-subnet finding are both
	// advisory and both belong to this one response
	status.prependWarning(warning)
	return success(status)
}

func (s *Server) clusterMutationLock(clusterName string) *sync.Mutex {
	clusterName = clusterKey(clusterName)
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	if s.mutationLocks == nil {
		s.mutationLocks = make(map[string]*sync.Mutex)
	}
	lock, ok := s.mutationLocks[clusterName]
	if !ok {
		lock = &sync.Mutex{}
		s.mutationLocks[clusterName] = lock
	}
	return lock
}

// dispatchStatus keeps the existing VM-state snapshot serialized, then probes
// Kubernetes after releasing opMu so a slow API server cannot block lifecycle
// operations.
func (s *Server) dispatchStatus(request Request) Response {
	s.opMu.Lock()
	data, err := s.handle(request, nil)
	s.opMu.Unlock()
	if err != nil {
		return failure(err)
	}
	statuses, ok := data.([]ClusterStatus)
	if !ok {
		return failure(errors.New("status returned an unexpected response"))
	}
	s.refreshNodeStatuses(statuses)
	refreshKubernetesReadiness(statuses)
	s.refreshStoragePhases(statuses)
	return success(statuses)
}

func (s *Server) handle(request Request, progress stageFunc) (any, error) {
	switch request.Op {
	case "daemon.ping":
		return map[string]bool{"pong": true}, nil
	case "daemon.info":
		return Info{ProtocolVersion: ProtocolVersion}, nil
	case "up":
		return s.up(request.Args)
	case "down":
		return s.down(request.Args)
	case "cluster.create":
		return s.createCluster(request.Args, progress)
	case "cluster.start":
		return s.startCluster(request.Args)
	case "cluster.stop":
		return s.stopCluster(request.Args)
	case "cluster.destroy":
		return s.destroyCluster(request.Args)
	case "cluster.destroy.inspect":
		return s.destroyInspect(request.Args)
	case "cluster.list":
		return s.listClusters()
	case "node.add", "node.remove", "node.start", "node.stop":
		// node mutations flow through dispatchNodeMutation, which volume-gates
		// node.remove before taking opMu; a locked ungated path must not exist
		return nil, fmt.Errorf("operation %q must be dispatched as a node mutation", request.Op)
	case "status":
		return s.status(request.Args)
	case "cluster.suspend":
		return s.suspendCluster(request.Args)
	case "cluster.resume":
		return s.resumeCluster(request.Args)
	case "snapshot.create":
		return s.snapshotCreate(request.Args, progress)
	case "snapshot.restore":
		// restore flows through dispatchSnapshotRestore, which volume-gates the
		// nodes it deletes before taking opMu; a locked ungated path must not exist
		return nil, fmt.Errorf("operation %q must be dispatched as a snapshot restore", request.Op)
	case "snapshot.list":
		return s.snapshotList(request.Args)
	case "snapshot.delete":
		return s.snapshotDelete(request.Args)
	case "bgp.enable", "bgp.disable":
		// mode changes flow through dispatchBGP, which schedules the Cilium
		// reconcile that puts the requested mechanism in effect; a locked path
		// that only moves the host speaker must not exist (#344)
		return nil, fmt.Errorf("operation %q must be dispatched as a BGP mode change", request.Op)
	case "cache.pull":
		return s.pullCache(request.Args)
	case "cache.warm":
		return s.warmMirrorCache(request.Args)
	case "cache.check":
		return s.checkMirrorCache(request.Args)
	case "cache.list":
		return s.listCache()
	case "cache.prune":
		return s.pruneCache(request.Args)
	case "mirror.offline.get":
		return s.getMirrorOffline()
	case "mirror.offline.set":
		return s.setMirrorOffline(request.Args)
	default:
		return nil, fmt.Errorf("unknown operation %q", request.Op)
	}
}

func decodeArgs(raw json.RawMessage, destination any) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(raw, destination); err != nil {
		return fmt.Errorf("decode arguments: %w", err)
	}
	return nil
}

func closeVMs(machines []hypervisor.Machine) error {
	errorsByVM := make(chan error, len(machines))
	var wait sync.WaitGroup
	for _, machine := range machines {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errorsByVM <- closeMachine(machine)
		}()
	}
	wait.Wait()
	close(errorsByVM)
	var result error
	for err := range errorsByVM {
		result = errors.Join(result, err)
	}
	return result
}

func closeMachine(machine hypervisor.Machine) error {
	return errors.Join(stopMachine(machine), machine.Close())
}

func stopMachine(machine hypervisor.Machine) error {
	ctx, cancel := context.WithTimeout(context.Background(), machineStopTimeout)
	defer cancel()
	return machine.Stop(ctx)
}

func removeNodeFiles(clusterName, nodeName string) error {
	dir, err := cluster.Dir(clusterName)
	if err != nil {
		return err
	}
	// saveStateSuffix belongs here too: a removed node's suspended memory has
	// nothing left to restore into, and clusterHasSavedState only globs the
	// directory — an orphaned save would keep reporting the whole cluster
	// Suspended and keep the hint recommending a resume forever.
	for _, suffix := range []string{".img", ".efi", ".console.sock", ".qga.sock", saveStateSuffix} {
		if err := os.Remove(filepath.Join(dir, nodeName+suffix)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove node file: %w", err)
		}
	}
	return nil
}

func sortedNodeNames(nodes map[string]hypervisor.Machine) []string {
	names := make([]string, 0, len(nodes))
	for name := range nodes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
