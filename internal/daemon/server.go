package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
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

	opMu                  sync.Mutex
	vms                   map[string]map[string]hypervisor.Machine
	provisions            map[string]activeProvision
	storagePhases         map[string]StoragePhase
	storageStatusProbes   map[string]activeStorageProbe
	storageProbeFailures  map[string]storageProbeFailure
	storageProbeSequence  uint64
	provisionSequence     uint64
	provisionReconcile    provisionReconcileFunc
	storageProbe          func(context.Context, []byte) error
	destroyVolumeCount    func(context.Context, cluster.Cluster) (int, error)
	nodeVolumeCount       nodeVolumeCountFunc
	storageEngineDelete   func(context.Context, cluster.Cluster) error
	storageEngineValidate func(context.Context, cluster.Cluster) error
	nodeIPLookup          func(string, int) string
	nodeProbe             func(string) ProbeResult
	hostFreeMemory        func() (int, error)
	helperCheck           func() error
	maintenanceLoad       func(string) (cluster.Cluster, error)
	lifecycleContext      context.Context
	lifecycleCancel       context.CancelFunc
	mirrors               *mirror.Manager
	mirrorOffline         atomic.Bool
	defaultSchematic      string
	subnetSources         cluster.SubnetSources
	hostPressure          func(string) (hostpressure.Snapshot, error)

	listenerMu   sync.Mutex
	listener     net.Listener
	closing      bool
	connections  map[net.Conn]struct{}
	connectionWG sync.WaitGroup
}

type activeProvision struct {
	generation uint64
	cancel     context.CancelFunc
}

type activeStorageProbe struct {
	generation uint64
	cancel     context.CancelFunc
}

type storageProbeFailure struct {
	message string
	at      time.Time
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
		destroyVolumeCount:    countDestroyStorageVolumes,
		nodeVolumeCount:       countNodeRemovalStorageVolumes,
		storageEngineDelete:   deleteConfiguredStorageEngine,
		storageEngineValidate: validateConfiguredStorageEngine,
	}
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
		if err := encoder.Encode(s.dispatch(request)); err != nil {
			return
		}
	}
}

func (s *Server) dispatch(request Request) Response {
	if request.Op == "status" {
		return s.dispatchStatus(request)
	}
	if request.Op == "cluster.create" || request.Op == "cluster.start" || request.Op == "up" {
		return s.dispatchProvisioning(request)
	}
	if request.Op == "node.add" || request.Op == "node.remove" {
		return s.dispatchNodeMutation(request)
	}
	if request.Op == "snapshot.restore" {
		return s.dispatchSnapshotRestore(request)
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()

	data, err := s.handle(request)
	if err != nil {
		return failure(err)
	}
	return success(data)
}

func (s *Server) lifecycleTimeoutContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	parent := s.lifecycleContext
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, timeout)
}

func (s *Server) dispatchProvisioning(request Request) Response {
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
	data, tasks, err := s.handleProvisioningLocked(request, maintenance, storage)
	s.opMu.Unlock()
	if err != nil {
		return failure(err)
	}
	if err := s.runProvisionTasks(data, tasks); err != nil {
		return failure(err)
	}
	return success(data)
}

func (s *Server) dispatchNodeMutation(request Request) Response {
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
		warning, err := s.gateNodeRemoval(args)
		if err != nil {
			lock.Unlock()
			return failure(err)
		}
		removalWarning = warning
	}
	s.opMu.Lock()
	data, tasks, err := s.handleNodeMutationLocked(request)
	s.opMu.Unlock()
	lock.Unlock()
	if err != nil {
		if removalWarning != "" {
			// the removal may have deleted state before failing; the data-loss
			// note must reach the user alongside the failure
			return failure(fmt.Errorf("%w (warning: %s)", err, removalWarning))
		}
		return failure(err)
	}
	if removalWarning != "" {
		if status, ok := data.(NodeStatus); ok {
			status.Warning = joinWarnings(status.Warning, removalWarning)
			data = status
		}
	}
	if err := s.runProvisionTasks(data, tasks); err != nil {
		if removalWarning != "" {
			// the node's disk is already gone; the data-loss note must survive
			// a failed follow-up reconcile
			return failure(fmt.Errorf("%w (warning: %s)", err, removalWarning))
		}
		return failure(err)
	}
	return success(data)
}

// dispatchSnapshotRestore gates the restore's node deletions outside the
// operation lock, like dispatchNodeMutation: a restore drops every live node
// the snapshot did not capture, disks and all.
func (s *Server) dispatchSnapshotRestore(request Request) Response {
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
	snapshots, err := s.snapshotRestore(request.Args)
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
	return success(SnapshotRestoreStatus{Snapshots: snapshots, Warning: warning})
}

func (s *Server) clusterMutationLock(clusterName string) *sync.Mutex {
	// normalize the key: on a case-insensitive filesystem "Demo" and "demo"
	// load the same cluster state and must share one mutation lock
	clusterName = strings.ToLower(clusterName)
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
	data, err := s.handle(request)
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

func (s *Server) handle(request Request) (any, error) {
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
		return s.createCluster(request.Args)
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
	case "node.add", "node.remove":
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
		return s.snapshotCreate(request.Args)
	case "snapshot.restore":
		// restore flows through dispatchSnapshotRestore, which volume-gates the
		// nodes it deletes before taking opMu; a locked ungated path must not exist
		return nil, fmt.Errorf("operation %q must be dispatched as a snapshot restore", request.Op)
	case "snapshot.list":
		return s.snapshotList(request.Args)
	case "snapshot.delete":
		return s.snapshotDelete(request.Args)
	case "bgp.enable":
		return s.setBGP(request.Args, true)
	case "bgp.disable":
		return s.setBGP(request.Args, false)
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
	for _, suffix := range []string{".img", ".efi", ".console.sock", ".qga.sock"} {
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
