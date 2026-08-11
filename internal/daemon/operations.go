package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/dns"
	"github.com/randax/talos-box/internal/domain"
	"github.com/randax/talos-box/internal/helper"
	"github.com/randax/talos-box/internal/hypervisor"
	"github.com/randax/talos-box/internal/imagecache"
)

type createArgs struct {
	Name          string                `json:"name"`
	ControlPlanes *int                  `json:"controlPlanes"`
	Workers       *int                  `json:"workers"`
	Node          cluster.NodeDefaults  `json:"node"`
	NodeDefaults  cluster.NodeDefaults  `json:"nodeDefaults"`
	ControlPlane  *cluster.NodeDefaults `json:"controlPlane,omitempty"`
	Worker        *cluster.NodeDefaults `json:"worker,omitempty"`
	BGP           bool                  `json:"bgp,omitempty"`
	// Domain is the requested cluster domain; empty means the default,
	// <name>.k8s.test. AllowUnsafeDomain is the explicit opt-in for domains
	// that can shadow real DNS.
	Domain            string `json:"domain,omitempty"`
	AllowUnsafeDomain bool   `json:"allowUnsafeDomain,omitempty"`
	Force             bool   `json:"force"`
	Schematic         string `json:"schematic"`
	Version           string `json:"version"`
	TalosVersion      string `json:"talosVersion"`
}

type nameArgs struct {
	Name string `json:"name"`
}

type startArgs struct {
	Name  string `json:"name"`
	Force bool   `json:"force"`
}

type destroyArgs struct {
	Name  string `json:"name"`
	Force bool   `json:"force"`
}

type nodeArgs struct {
	Cluster string       `json:"cluster"`
	Name    string       `json:"name"`
	Role    cluster.Role `json:"role"`
	Force   bool         `json:"force"`
}

type statusArgs struct {
	Cluster string `json:"cluster"`
	Name    string `json:"name"`
}

type cachePullArgs struct {
	Schematic    string `json:"schematic"`
	Version      string `json:"version"`
	TalosVersion string `json:"talosVersion"`
}

type CacheWarmArgs struct {
	Refs []string `json:"refs"`
}

type CachePruneScope string

const (
	CachePruneScopeImages CachePruneScope = "images"
	CachePruneScopeMirror CachePruneScope = "mirror"
	CachePruneScopeAll    CachePruneScope = "all"
)

type CachePruneArgs struct {
	Scope CachePruneScope `json:"scope"`
}

// ClusterSummary is the compact cluster.list result.
type ClusterSummary struct {
	Name          string               `json:"name"`
	Index         int                  `json:"index"`
	SubnetIndex   int                  `json:"subnetIndex"`
	ControlPlanes int                  `json:"controlPlanes"`
	Workers       int                  `json:"workers"`
	NodeDefaults  cluster.NodeDefaults `json:"nodeDefaults"`
	TalosVersion  string               `json:"talosVersion"`
	Schematic     string               `json:"schematic"`
	BGP           bool                 `json:"bgp"`
	// Domain is the explicitly chosen cluster domain; empty means the
	// default, <name>.k8s.test.
	Domain            string `json:"domain,omitempty"`
	AllowUnsafeDomain bool   `json:"allowUnsafeDomain,omitempty"`
	Running           bool   `json:"running"`
	Warning           string `json:"warning,omitempty"`
}

// EffectiveDomain returns the domain the cluster is reachable under.
func (s ClusterSummary) EffectiveDomain() string {
	if s.Domain != "" {
		return s.Domain
	}
	return s.Name + "." + cluster.DefaultDomainSuffix
}

// NodeStatus is the observed host-side state of one node.
type NodeStatus struct {
	Name          string       `json:"name"`
	Role          cluster.Role `json:"role"`
	MAC           string       `json:"mac"`
	IP            string       `json:"ip,omitempty"`
	APIDReachable bool         `json:"apidReachable"`
	Phase         Phase        `json:"phase"`
	Warning       string       `json:"warning,omitempty"`
}

// ClusterStatus is the status result for one cluster.
type ClusterStatus struct {
	Name   string `json:"name"`
	Subnet string `json:"subnet"`
	// Domain is the cluster's effective domain (explicit or defaulted).
	Domain  string       `json:"domain"`
	BGP     bool         `json:"bgp"`
	Running bool         `json:"running"`
	Nodes   []NodeStatus `json:"nodes"`
	Hints   []string     `json:"hints,omitempty"`
}

// CachePullResult describes the image made ready by cache.pull.
type CachePullResult struct {
	Schematic    string                  `json:"schematic"`
	Version      string                  `json:"version"`
	Architecture hypervisor.Architecture `json:"architecture"`
	Path         string                  `json:"path"`
}

type CacheWarmStatus string

const (
	CacheWarmStatusWarmed          CacheWarmStatus = "warmed"
	CacheWarmStatusAlreadyComplete CacheWarmStatus = "already-complete"
	CacheWarmStatusFailed          CacheWarmStatus = "failed"
)

type CacheWarmEntry struct {
	Ref    string          `json:"ref"`
	Status CacheWarmStatus `json:"status"`
	Reason string          `json:"reason,omitempty"`
}

type CacheWarmResult struct {
	Entries         []CacheWarmEntry `json:"entries"`
	Warmed          int              `json:"warmed"`
	AlreadyComplete int              `json:"alreadyComplete"`
	Failed          int              `json:"failed"`
}

const cacheWarmTimeout = 2 * time.Hour

type CacheImageEntry struct {
	Schematic    string `json:"schematic"`
	Version      string `json:"version"`
	Architecture string `json:"architecture"`
	Size         int64  `json:"size"`
}

type MirrorCacheEntry struct {
	Upstream      string `json:"upstream"`
	BlobCount     int    `json:"blobCount"`
	BlobBytes     int64  `json:"blobBytes"`
	ManifestCount int    `json:"manifestCount"`
	ManifestBytes int64  `json:"manifestBytes"`
}

type MirrorCacheTotals struct {
	BlobCount     int   `json:"blobCount"`
	BlobBytes     int64 `json:"blobBytes"`
	ManifestCount int   `json:"manifestCount"`
	ManifestBytes int64 `json:"manifestBytes"`
}

type CacheListResult struct {
	Images      []CacheImageEntry  `json:"images"`
	Mirror      []MirrorCacheEntry `json:"mirror"`
	MirrorTotal MirrorCacheTotals  `json:"mirrorTotal"`
}

type CachePruneResult struct {
	Scope      CachePruneScope   `json:"scope"`
	ImageCount int               `json:"imageCount"`
	ImageBytes int64             `json:"imageBytes"`
	Mirror     MirrorCacheTotals `json:"mirror"`
}

type MirrorOfflineStatus struct {
	Enabled bool `json:"enabled"`
}

func (s *Server) createCluster(raw json.RawMessage) (ClusterSummary, error) {
	var args createArgs
	if err := decodeArgs(raw, &args); err != nil {
		return ClusterSummary{}, err
	}
	controlPlanes, workers := 1, 2
	if args.ControlPlanes != nil {
		controlPlanes = *args.ControlPlanes
	}
	if args.Workers != nil {
		workers = *args.Workers
	}
	if args.Node == (cluster.NodeDefaults{}) {
		args.Node = args.NodeDefaults
	}
	if args.Version == "" {
		args.Version = args.TalosVersion
	}

	dir, err := cluster.Dir(args.Name)
	if err != nil {
		return ClusterSummary{}, err
	}
	if _, err := os.Stat(dir); err == nil {
		return ClusterSummary{}, fmt.Errorf("cluster %q already exists", args.Name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return ClusterSummary{}, fmt.Errorf("inspect cluster directory: %w", err)
	}
	hostPressureWarning, err := s.checkHostPressure(dir, args.Force)
	if err != nil {
		return ClusterSummary{}, err
	}
	if err := requireHelper(); err != nil {
		return ClusterSummary{}, err
	}
	addMiB := (controlPlanes + workers) * memoryOr(args.Node.MemoryMiB, cluster.DefaultMemoryMiB)
	overcommitWarning, err := s.checkOvercommit(addMiB, args.Force)
	if err != nil {
		return ClusterSummary{}, err
	}
	clusters, err := cluster.List()
	if err != nil {
		return ClusterSummary{}, err
	}
	canonicalDomain := ""
	if args.Domain != "" {
		canonicalDomain, err = domain.Validate(args.Domain, args.AllowUnsafeDomain)
		if err != nil {
			return ClusterSummary{}, err
		}
		// An explicit domain equal to the cluster's own default is the
		// default; storing it as such keeps every comparison canonical.
		if canonicalDomain == args.Name+"."+cluster.DefaultDomainSuffix {
			canonicalDomain = ""
		}
	}
	effectiveDomain := canonicalDomain
	if effectiveDomain == "" {
		effectiveDomain = args.Name + "." + cluster.DefaultDomainSuffix
		// The default domain derives from the cluster name, which is only
		// path-checked; a name that does not already form a canonical DNS
		// name (case included) must fail here, not at helper registration.
		canonical, err := domain.Validate(effectiveDomain, true)
		if err != nil || canonical != effectiveDomain {
			return ClusterSummary{}, fmt.Errorf("cluster name %q does not form a valid domain (lowercase DNS labels required)", args.Name)
		}
	}
	if cluster.DomainInUse(effectiveDomain, clusters) {
		return ClusterSummary{}, fmt.Errorf("domain %q is already used by another cluster", effectiveDomain)
	}
	subnetIndex, subnetWarning, err := cluster.LowestUsableSubnetIndex(clusters, s.hostSubnetSources())
	if err != nil {
		return ClusterSummary{}, err
	}
	item, err := cluster.New(args.Name, subnetIndex, controlPlanes, workers, args.Node)
	if err != nil {
		return ClusterSummary{}, err
	}
	item.ControlPlaneDefaults = args.ControlPlane
	item.WorkerDefaults = args.Worker
	item.BGP = args.BGP
	item.Domain = canonicalDomain
	item.AllowUnsafeDomain = canonicalDomain != "" && args.AllowUnsafeDomain
	item.ImageArchitecture = string(s.hypervisor.Architecture())
	item.Schematic, item.TalosVersion, err = s.resolveImage(args.Schematic, args.Version)
	if err != nil {
		return ClusterSummary{}, err
	}
	cachedDisk, err := s.cache.Ensure(item.Schematic, item.TalosVersion, s.imageArchitecture())
	if err != nil {
		return ClusterSummary{}, err
	}
	if err := cluster.ProvisionDisks(item, cachedDisk); err != nil {
		_ = cluster.Destroy(item.Name)
		return ClusterSummary{}, err
	}
	if err := cluster.Save(item); err != nil {
		_ = cluster.Destroy(item.Name)
		return ClusterSummary{}, err
	}
	if item.Domain != "" {
		if err := SyncResolverFiles(); err != nil {
			log.Printf("resolver files for %s: %v", item.Name, err)
		}
	}
	startWarning, err := s.start(item)
	if err != nil {
		result := summary(item, false)
		result.Warning = joinWarnings(overcommitWarning, hostPressureWarning, subnetWarning)
		return result, fmt.Errorf("cluster created but failed to start: %w", err)
	}
	result := summary(item, true)
	result.Warning = joinWarnings(overcommitWarning, hostPressureWarning, subnetWarning, startWarning)
	return result, nil
}

func (s *Server) startCluster(raw json.RawMessage) (ClusterSummary, error) {
	var args startArgs
	if err := decodeArgs(raw, &args); err != nil {
		return ClusterSummary{}, err
	}
	item, err := cluster.Load(args.Name)
	if err != nil {
		return ClusterSummary{}, err
	}
	var overcommitWarning string
	if !s.clusterRunning(item.Name) {
		w, err := s.checkOvercommit(clusterMemoryMiB(item), args.Force)
		if err != nil {
			return ClusterSummary{}, err
		}
		overcommitWarning = w
	}
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		return ClusterSummary{}, err
	}
	hostPressureWarning, err := s.checkHostPressure(dir, args.Force)
	if err != nil {
		return ClusterSummary{}, err
	}
	subnetWarning, err := s.start(item)
	if err != nil {
		return ClusterSummary{}, err
	}
	result := summary(item, true)
	result.Warning = joinWarnings(overcommitWarning, hostPressureWarning, subnetWarning)
	return result, nil
}

func (s *Server) start(item cluster.Cluster) (string, error) {
	subnetWarning, err := cluster.CheckSubnetIndex(item.SubnetIndex, s.hostSubnetSources())
	if err != nil {
		return "", err
	}
	nodes := s.vms[item.Name]
	if nodes == nil {
		nodes = make(map[string]hypervisor.Machine)
		s.vms[item.Name] = nodes
	}
	var started []string
	for _, node := range item.Nodes {
		if existing := nodes[node.Name]; existing != nil {
			if existing.Active() {
				continue
			}
			if err := existing.Close(); err != nil {
				return "", fmt.Errorf("release inactive VM %s: %w", node.Name, err)
			}
			delete(nodes, node.Name)
		}
		machine, err := s.launchMachine(item, node, nil)
		if err != nil {
			rollbackErr := s.rollbackStarted(item.Name, nodes, started)
			return "", errors.Join(fmt.Errorf("create VM %s: %w", node.Name, err), rollbackErr)
		}
		nodes[node.Name] = machine
		started = append(started, node.Name)
	}
	go s.bindMirrors(item.SubnetIndex) // async: don't hold opMu across the retry
	return subnetWarning, nil
}

func (s *Server) startAndLogWarning(item cluster.Cluster) error {
	warning, err := s.start(item)
	if warning != "" {
		log.Printf("start %s: %s", item.Name, warning)
	}
	return err
}

// hostSubnetSources merges injected sources with system defaults per field, so
// a partially-configured Server never yields a nil source.
func (s *Server) hostSubnetSources() cluster.SubnetSources {
	sources := cluster.SystemSubnetSources()
	if s.subnetSources.Interfaces != nil {
		sources.Interfaces = s.subnetSources.Interfaces
	}
	if s.subnetSources.Route != nil {
		sources.Route = s.subnetSources.Route
	}
	return sources
}

func (s *Server) rollbackStarted(clusterName string, nodes map[string]hypervisor.Machine, names []string) error {
	return s.closeNodes(clusterName, nodes, names)
}

func (s *Server) launchMachine(item cluster.Cluster, node cluster.Node, restore *hypervisor.Restore) (hypervisor.Machine, error) {
	if _, err := s.clusterImageArchitecture(item); err != nil {
		return nil, err
	}
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		return nil, err
	}
	sizing := item.DefaultsFor(node.Role)
	return s.hypervisor.Launch(context.Background(), hypervisor.Spec{
		CPUs:      sizing.CPUs,
		MemoryMiB: sizing.MemoryMiB,
		DiskPath:  filepath.Join(dir, node.Name+".img"),
		MAC:       node.MAC,
		Network: func() (*helper.Attachment, error) {
			attachment, err := helper.Attach(item.Name, item.SubnetIndex, node.Name)
			if errors.Is(err, helper.ErrUnavailable) {
				return nil, helperInstallError(err)
			}
			return attachment, err
		},
		EFIVarsPath:       filepath.Join(dir, node.Name+".efi"),
		ConsoleSocketPath: filepath.Join(dir, node.Name+".console.sock"),
		Restore:           restore,
	})
}

func helperInstallError(err error) error {
	return fmt.Errorf("network helper unavailable; run `sudo tbx system install`: %w", err)
}

func requireHelper() error {
	client, err := helper.Connect()
	if err != nil {
		return helperInstallError(err)
	}
	defer func() { _ = client.Close() }()
	if err := client.Ping(); err != nil {
		return helperInstallError(err)
	}
	return nil
}

func (s *Server) stopCluster(raw json.RawMessage) (ClusterSummary, error) {
	var args nameArgs
	if err := decodeArgs(raw, &args); err != nil {
		return ClusterSummary{}, err
	}
	item, err := cluster.Load(args.Name)
	if err != nil {
		return ClusterSummary{}, err
	}
	if err := s.stop(item.Name); err != nil {
		return ClusterSummary{}, err
	}
	return summary(item, false), nil
}

func (s *Server) stop(name string) error {
	if item, err := cluster.Load(name); err == nil {
		s.unbindMirrors(item.SubnetIndex)
	} else {
		log.Printf("unbind mirrors for %s: cluster state unreadable: %v", name, err)
	}
	nodes := s.vms[name]
	if len(nodes) == 0 {
		delete(s.vms, name)
		return nil
	}
	return s.closeNodes(name, nodes, sortedNodeNames(nodes))
}

// bindMirrors serves the registry mirrors on a cluster's gateway once its
// vmnet interface is up. Runs in its own goroutine (Bind has its own lock and
// must not hold opMu across the retry sleep); best-effort with a short retry as
// the gateway address appears, a failure is logged, not fatal.
func (s *Server) bindMirrors(subnetIndex int) {
	if s.mirrors == nil {
		return
	}
	gateway := cluster.Gateway(subnetIndex)
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		if err = s.mirrors.Bind(gateway); err == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	log.Printf("registry mirrors not bound on %s: %v", gateway, err)
}

func (s *Server) unbindMirrors(subnetIndex int) {
	if s.mirrors != nil {
		s.mirrors.Unbind(cluster.Gateway(subnetIndex))
	}
}

func (s *Server) closeNodes(clusterName string, nodes map[string]hypervisor.Machine, names []string) error {
	type result struct {
		name string
		err  error
	}
	results := make(chan result, len(names))
	for _, name := range names {
		machine := nodes[name]
		go func() { results <- result{name: name, err: closeMachine(machine)} }()
	}
	errorsByName := make(map[string]error, len(names))
	for range names {
		item := <-results
		errorsByName[item.name] = item.err
	}

	var resultErr error
	for _, name := range names {
		if err := errorsByName[name]; err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("stop VM %s: %w", name, err))
			continue
		}
		delete(nodes, name)
	}
	if len(nodes) == 0 {
		delete(s.vms, clusterName)
	}
	return resultErr
}

func (s *Server) destroyCluster(raw json.RawMessage) (map[string]string, error) {
	var args destroyArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if !args.Force {
		return nil, errors.New("cluster.destroy requires force=true")
	}
	dir, err := cluster.Dir(args.Name)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("cluster %q does not exist", args.Name)
	}
	// stop what we can, but a partially-destroyed cluster (state dir present,
	// cluster.json gone) must still be removable
	if _, loadErr := cluster.Load(args.Name); loadErr == nil {
		if err := s.stop(args.Name); err != nil {
			return nil, err
		}
	}
	if err := cluster.Destroy(args.Name); err != nil {
		return nil, err
	}
	if err := SyncResolverFiles(); err != nil {
		log.Printf("resolver files after destroying %s: %v", args.Name, err)
	}
	return map[string]string{"name": args.Name}, nil
}

// resolverSyncMu makes SyncResolverFiles the single owner of resolver-file
// mutation: every caller re-reads state under the lock, so a concurrent
// create/destroy and the periodic drift repair can never apply a stale
// domain set (which would delete a just-created file or resurrect a
// just-removed one).
var resolverSyncMu sync.Mutex

// SyncResolverFiles converges the host's per-domain resolver files to the
// clusters now in state, so a custom domain resolves the moment create
// returns and its file disappears at destroy rather than on the next drift
// tick. Best-effort for the create/destroy callers (the tbxd reconciler
// re-asserts on its own cadence); the error return lets that reconciler
// report repair failures honestly.
func SyncResolverFiles() error {
	resolverSyncMu.Lock()
	defer resolverSyncMu.Unlock()
	clusters, err := cluster.List()
	if err != nil {
		return fmt.Errorf("skip resolver-file sync: %w", err)
	}
	var domains []string
	for _, item := range clusters {
		if item.Domain != "" {
			domains = append(domains, item.Domain)
		}
	}
	client, err := helper.Connect()
	if err != nil {
		return fmt.Errorf("skip resolver-file sync: %w", err)
	}
	defer func() { _ = client.Close() }()
	if err := client.SyncDomainResolvers(domains, dns.Port); err != nil {
		return fmt.Errorf("sync custom-domain resolvers: %w", err)
	}
	return nil
}

func (s *Server) listClusters() ([]ClusterSummary, error) {
	items, err := cluster.List()
	if err != nil {
		return nil, err
	}
	result := make([]ClusterSummary, 0, len(items))
	for _, item := range items {
		result = append(result, summary(item, s.clusterRunning(item.Name)))
	}
	return result, nil
}

func (s *Server) addNode(raw json.RawMessage) (NodeStatus, error) {
	var args nodeArgs
	if err := decodeArgs(raw, &args); err != nil {
		return NodeStatus{}, err
	}
	if args.Role == "" {
		args.Role = cluster.RoleWorker
	}
	item, err := cluster.Load(args.Cluster)
	if err != nil {
		return NodeStatus{}, err
	}
	addMiB := item.DefaultsFor(args.Role).MemoryMiB
	overcommitWarning, err := s.checkOvercommit(addMiB, args.Force)
	if err != nil {
		return NodeStatus{}, err
	}
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		return NodeStatus{}, err
	}
	hostPressureWarning, err := s.checkHostPressure(dir, args.Force)
	if err != nil {
		return NodeStatus{}, err
	}
	running := s.clusterRunning(item.Name)
	var subnetWarning string
	if running {
		subnetWarning, err = cluster.CheckSubnetIndex(item.SubnetIndex, s.hostSubnetSources())
		if err != nil {
			return NodeStatus{}, err
		}
	}
	cachedDisk, err := s.cachedDisk(item)
	if err != nil {
		return NodeStatus{}, err
	}
	node, err := cluster.AddNode(&item, args.Role, args.Name)
	if err != nil {
		return NodeStatus{}, err
	}
	if err := cluster.ProvisionDisks(item, cachedDisk); err != nil {
		_ = removeNodeFiles(item.Name, node.Name)
		return NodeStatus{}, err
	}
	if err := cluster.Save(item); err != nil {
		_ = removeNodeFiles(item.Name, node.Name)
		return NodeStatus{}, err
	}
	if running {
		machine, err := s.launchMachine(item, node, nil)
		if err != nil {
			return nodeStatus(node, item.SubnetIndex, false), fmt.Errorf("node added but failed to create VM: %w", err)
		}
		s.vms[item.Name][node.Name] = machine
	}
	status := nodeStatus(node, item.SubnetIndex, s.nodeRunning(item.Name, node.Name))
	status.Warning = joinWarnings(overcommitWarning, hostPressureWarning, subnetWarning)
	return status, nil
}

func (s *Server) removeNode(raw json.RawMessage) (NodeStatus, error) {
	var args nodeArgs
	if err := decodeArgs(raw, &args); err != nil {
		return NodeStatus{}, err
	}
	item, err := cluster.Load(args.Cluster)
	if err != nil {
		return NodeStatus{}, err
	}
	node, err := cluster.RemoveNode(&item, args.Name)
	if err != nil {
		return NodeStatus{}, err
	}
	if machine := s.vms[item.Name][node.Name]; machine != nil {
		if err := closeMachine(machine); err != nil {
			return NodeStatus{}, fmt.Errorf("stop node %s: %w", node.Name, err)
		}
		delete(s.vms[item.Name], node.Name)
	}
	if err := cluster.Save(item); err != nil {
		return NodeStatus{}, err
	}
	if err := removeNodeFiles(item.Name, node.Name); err != nil {
		return NodeStatus{}, err
	}
	return nodeStatus(node, item.SubnetIndex, false), nil
}

func (s *Server) status(raw json.RawMessage) ([]ClusterStatus, error) {
	var args statusArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.Cluster == "" {
		args.Cluster = args.Name
	}
	var items []cluster.Cluster
	if args.Cluster != "" {
		item, err := cluster.Load(args.Cluster)
		if err != nil {
			return nil, err
		}
		items = []cluster.Cluster{item}
	} else {
		var err error
		items, err = cluster.List()
		if err != nil {
			return nil, err
		}
	}

	result := make([]ClusterStatus, 0, len(items))
	for _, item := range items {
		clusterStatus := ClusterStatus{Name: item.Name, Subnet: cluster.SubnetCIDR(item.SubnetIndex), Domain: item.EffectiveDomain(), BGP: item.BGP, Running: s.clusterRunning(item.Name)}
		for _, node := range item.Nodes {
			clusterStatus.Nodes = append(clusterStatus.Nodes, nodeStatus(node, item.SubnetIndex, s.nodeRunning(item.Name, node.Name)))
		}
		clusterStatus.Hints = Hints(clusterStatus)
		result = append(result, clusterStatus)
	}
	return result, nil
}

func (s *Server) nodeRunning(clusterName, nodeName string) bool {
	machine := s.vms[clusterName][nodeName]
	return machine != nil && machine.Active()
}

func (s *Server) clusterRunning(name string) bool {
	for _, machine := range s.vms[name] {
		if machine.Active() {
			return true
		}
	}
	return false
}

func nodeStatus(node cluster.Node, subnetIndex int, vmRunning bool) NodeStatus {
	return nodeStatusWith(node, subnetIndex, vmRunning, cluster.LookupIP, probeAPID)
}

func nodeStatusWith(
	node cluster.Node,
	subnetIndex int,
	vmRunning bool,
	lookupIP func(string, int) string,
	probe func(string) ProbeResult,
) NodeStatus {
	ip := lookupIP(node.MAC, subnetIndex)
	probeResult := ProbeResult{}
	if vmRunning && ip != "" {
		probeResult = probe(ip)
	}
	return NodeStatus{
		Name:          node.Name,
		Role:          node.Role,
		MAC:           node.MAC,
		IP:            ip,
		APIDReachable: probeResult.Dialed,
		Phase:         ClassifyPhase(vmRunning, probeResult),
	}
}

func (s *Server) pullCache(raw json.RawMessage) (CachePullResult, error) {
	var args cachePullArgs
	if err := decodeArgs(raw, &args); err != nil {
		return CachePullResult{}, err
	}
	if args.Version == "" {
		args.Version = args.TalosVersion
	}
	schematic, talosVersion, err := s.resolveImage(args.Schematic, args.Version)
	if err != nil {
		return CachePullResult{}, err
	}
	architecture := s.hypervisor.Architecture()
	path, err := s.cache.Ensure(schematic, talosVersion, imagecache.Architecture(architecture))
	if err != nil {
		return CachePullResult{}, err
	}
	return CachePullResult{Schematic: schematic, Version: talosVersion, Architecture: architecture, Path: path}, nil
}

func (s *Server) warmMirrorCache(raw json.RawMessage) (CacheWarmResult, error) {
	var args CacheWarmArgs
	if err := decodeArgs(raw, &args); err != nil {
		return CacheWarmResult{}, err
	}
	if len(args.Refs) == 0 {
		return CacheWarmResult{}, errors.New("at least one image reference is required")
	}
	for _, ref := range args.Refs {
		if err := ValidateWarmRef(ref); err != nil {
			return CacheWarmResult{}, err
		}
	}
	if s.warmCache == nil {
		return CacheWarmResult{}, errors.New("cache warm is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), cacheWarmTimeout)
	defer cancel()
	return s.warmCache(ctx, args.Refs, s.imageArchitecture())
}

func (s *Server) listCache() (CacheListResult, error) {
	entries, err := s.cache.List()
	if err != nil {
		return CacheListResult{}, err
	}
	mirrorStats, mirrorTotals, err := s.cache.MirrorStats()
	if err != nil {
		return CacheListResult{}, err
	}
	result := CacheListResult{
		Images:      make([]CacheImageEntry, 0, len(entries)),
		Mirror:      make([]MirrorCacheEntry, 0, len(mirrorStats)),
		MirrorTotal: MirrorCacheTotals(mirrorTotals),
	}
	for _, entry := range entries {
		result.Images = append(result.Images, CacheImageEntry{
			Schematic:    entry.Schematic,
			Version:      entry.Version,
			Architecture: string(entry.Architecture),
			Size:         entry.Size,
		})
	}
	for _, stat := range mirrorStats {
		result.Mirror = append(result.Mirror, MirrorCacheEntry{
			Upstream:      stat.Upstream,
			BlobCount:     stat.BlobCount,
			BlobBytes:     stat.BlobBytes,
			ManifestCount: stat.ManifestCount,
			ManifestBytes: stat.ManifestBytes,
		})
	}
	return result, nil
}

func (s *Server) pruneCache(raw json.RawMessage) (CachePruneResult, error) {
	var args CachePruneArgs
	if err := decodeArgs(raw, &args); err != nil {
		return CachePruneResult{}, err
	}
	if args.Scope == "" {
		args.Scope = CachePruneScopeImages
	}
	switch args.Scope {
	case CachePruneScopeImages:
		result, err := s.cache.PruneDisk()
		if err != nil {
			return CachePruneResult{}, err
		}
		return CachePruneResult{Scope: args.Scope, ImageCount: result.ImageCount, ImageBytes: result.ImageBytes, Mirror: MirrorCacheTotals(result.Mirror)}, nil
	case CachePruneScopeMirror:
		result, err := s.cache.PruneMirror()
		if err != nil {
			return CachePruneResult{}, err
		}
		return CachePruneResult{Scope: args.Scope, ImageCount: result.ImageCount, ImageBytes: result.ImageBytes, Mirror: MirrorCacheTotals(result.Mirror)}, nil
	case CachePruneScopeAll:
		result, err := s.cache.PruneAll()
		if err != nil {
			return CachePruneResult{}, err
		}
		return CachePruneResult{Scope: args.Scope, ImageCount: result.ImageCount, ImageBytes: result.ImageBytes, Mirror: MirrorCacheTotals(result.Mirror)}, nil
	default:
		return CachePruneResult{}, fmt.Errorf("unknown cache prune scope %q", args.Scope)
	}
}

func ValidateWarmRef(ref string) error {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return errors.New("image reference is required")
	}
	if strings.ContainsAny(ref, "?#") || strings.ContainsFunc(ref, func(character rune) bool {
		return character <= 0x20
	}) {
		return fmt.Errorf("image reference %q is malformed", ref)
	}

	name, digest, hasDigest := strings.Cut(ref, "@")
	if hasDigest {
		if digest == "" || !isSupportedWarmDigest(digest) {
			return fmt.Errorf("image reference %q must use a sha256 or sha512 digest", ref)
		}
	}

	host, remainder, ok := strings.Cut(name, "/")
	if !ok || remainder == "" || (!strings.Contains(host, ".") && !strings.Contains(host, ":") && host != "localhost") {
		return fmt.Errorf("image reference %q must include a registry host", ref)
	}
	if strings.HasSuffix(name, "/") || strings.HasPrefix(remainder, ":") {
		return fmt.Errorf("image reference %q is malformed", ref)
	}

	lastSlash := strings.LastIndex(name, "/")
	lastColon := strings.LastIndex(name, ":")
	tag := ""
	if lastColon > lastSlash {
		tag = name[lastColon+1:]
	}
	if hasDigest && lastColon <= lastSlash {
		return nil
	}
	if hasDigest && lastColon > lastSlash && tag == "" {
		return fmt.Errorf("image reference %q is malformed", ref)
	}
	if lastColon <= lastSlash {
		return fmt.Errorf("image reference %q must include a non-latest tag or digest", ref)
	}
	if tag == "latest" {
		return fmt.Errorf("image reference %q must not use :latest", ref)
	}
	return nil
}

func isSupportedWarmDigest(value string) bool {
	algorithm, encoded, ok := strings.Cut(value, ":")
	if !ok || encoded == "" {
		return false
	}
	for _, character := range encoded {
		if !isHexDigit(character) {
			return false
		}
	}
	switch algorithm {
	case "sha256":
		return len(encoded) == 64
	case "sha512":
		return len(encoded) == 128
	default:
		return false
	}
}

func isHexDigit(character rune) bool {
	return (character >= 'a' && character <= 'f') || (character >= 'A' && character <= 'F') || (character >= '0' && character <= '9')
}

func (s *Server) resolveImage(schematic, talosVersion string) (string, string, error) {
	if talosVersion == "" {
		talosVersion = DefaultTalosVersion
	}
	if schematic == "" {
		if s.defaultSchematic == "" {
			var err error
			s.defaultSchematic, err = s.cache.Schematic()
			if err != nil {
				return "", "", err
			}
		}
		schematic = s.defaultSchematic
	}
	return schematic, talosVersion, nil
}

func (s *Server) cachedDisk(item cluster.Cluster) (string, error) {
	schematic, talosVersion, err := s.resolveImage(item.Schematic, item.TalosVersion)
	if err != nil {
		return "", err
	}
	architecture, err := s.clusterImageArchitecture(item)
	if err != nil {
		return "", err
	}
	return s.cache.Ensure(schematic, talosVersion, architecture)
}

func (s *Server) imageArchitecture() imagecache.Architecture {
	return imagecache.Architecture(s.hypervisor.Architecture())
}

func (s *Server) clusterImageArchitecture(item cluster.Cluster) (imagecache.Architecture, error) {
	architecture := item.ImageArchitecture
	if architecture == "" {
		architecture = cluster.LegacyImageArchitecture
	}
	active := s.hypervisor.Architecture()
	if hypervisor.Architecture(architecture) != active {
		return "", fmt.Errorf("cluster %q uses %s images, but the active hypervisor targets %s", item.Name, architecture, active)
	}
	return imagecache.Architecture(architecture), nil
}

func memoryOr(mib, fallback int) int {
	if mib > 0 {
		return mib
	}
	return fallback
}

func summary(item cluster.Cluster, running bool) ClusterSummary {
	return ClusterSummary{
		Name:              item.Name,
		Index:             item.Index,
		SubnetIndex:       item.SubnetIndex,
		ControlPlanes:     item.ControlPlanes,
		Workers:           item.Workers,
		NodeDefaults:      item.NodeDefaults,
		TalosVersion:      item.TalosVersion,
		Schematic:         item.Schematic,
		BGP:               item.BGP,
		Domain:            item.Domain,
		AllowUnsafeDomain: item.AllowUnsafeDomain,
		Running:           running,
	}
}

func joinWarnings(warnings ...string) string {
	var result []string
	seen := make(map[string]bool, len(warnings))
	for _, warning := range warnings {
		if warning == "" || seen[warning] {
			continue
		}
		seen[warning] = true
		result = append(result, warning)
	}
	return strings.Join(result, "; ")
}
