package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/dns"
	"github.com/randax/talos-box/internal/domain"
	"github.com/randax/talos-box/internal/extensions"
	"github.com/randax/talos-box/internal/helper"
	"github.com/randax/talos-box/internal/hypervisor"
	"github.com/randax/talos-box/internal/imagecache"
	"github.com/randax/talos-box/internal/talosversion"
)

type createArgs struct {
	Name          string                `json:"name"`
	ControlPlanes *int                  `json:"controlPlanes"`
	Workers       *int                  `json:"workers"`
	Node          cluster.NodeDefaults  `json:"node"`
	NodeDefaults  cluster.NodeDefaults  `json:"nodeDefaults"`
	ControlPlane  *cluster.NodeDefaults `json:"controlPlane,omitempty"`
	Worker        *cluster.NodeDefaults `json:"worker,omitempty"`
	cluster.ProvisioningIntentInput
	// Domain is the requested cluster domain; empty means the default,
	// <name>.k8s.test. AllowUnsafeDomain is the explicit opt-in for domains
	// that can shadow real DNS.
	Domain            string `json:"domain,omitempty"`
	AllowUnsafeDomain bool   `json:"allowUnsafeDomain,omitempty"`
	Force             bool   `json:"force"`
	Schematic         string `json:"schematic"`
	Version           string `json:"version"`
	TalosVersion      string `json:"talosVersion"`
	// Extensions are the requested curated Talos extensions, composed into
	// the schematic at create. No omitempty: an explicit empty list (the
	// config-level opt-out) must survive the wire distinct from the field
	// being absent.
	Extensions []string `json:"extensions"`
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

type CacheCheckArgs struct {
	Refs []string `json:"refs"`
	Deep bool     `json:"deep,omitempty"`
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
	cluster.ProvisioningIntent
	// Domain is the explicitly chosen cluster domain; empty means the
	// default, <name>.k8s.test.
	Domain            string   `json:"domain,omitempty"`
	AllowUnsafeDomain bool     `json:"allowUnsafeDomain,omitempty"`
	Running           bool     `json:"running"`
	Warning           string   `json:"warning,omitempty"`
	Narration         []string `json:"narration,omitempty"`
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

// StoragePhase is the observed storage readiness for a CSI-backed cluster.
type StoragePhase string

const (
	StoragePhaseProvisioning StoragePhase = "provisioning"
	StoragePhaseLive         StoragePhase = "live"
)

// ClusterStatus is the status result for one cluster.
type ClusterStatus struct {
	Name   string `json:"name"`
	Subnet string `json:"subnet"`
	// Domain is the cluster's effective domain (explicit or defaulted).
	Domain string `json:"domain"`
	// TalosVersion and Schematic identify the image the cluster was created
	// from, as persisted at create.
	TalosVersion string `json:"talosVersion,omitempty"`
	Schematic    string `json:"schematic,omitempty"`
	// BaseSchematic is the schematic the extensions were re-composed into,
	// when the cluster brought one.
	BaseSchematic string `json:"baseSchematic,omitempty"`
	// TalosExtensions are the curated extensions the schematic was composed
	// from, as requested at create.
	TalosExtensions []string `json:"talosExtensions,omitempty"`
	cluster.ProvisioningIntent
	BGP             bool         `json:"bgp"`
	Running         bool         `json:"running"`
	KubernetesReady bool         `json:"kubernetesReady"`
	StoragePhase    StoragePhase `json:"storagePhase,omitempty"`
	StorageError    string       `json:"storageError,omitempty"`
	VIP             string       `json:"vip,omitempty"`
	VIPLive         bool         `json:"vipLive"`
	Nodes           []NodeStatus `json:"nodes"`
	// Capabilities reports the host capabilities this cluster's configuration
	// depends on, so a file stays portable across host substrates and the gate
	// is visible instead of silently doing nothing.
	Capabilities []CapabilityStatus `json:"capabilities,omitempty"`
	Hints        []string           `json:"hints,omitempty"`
	subnetIndex  int
}

// CapabilityStatus is one host capability a cluster depends on, with the reason
// the active hypervisor backend cannot provide it.
type CapabilityStatus struct {
	Name      string `json:"name"`
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
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

type CacheCheckStatus string

const (
	CacheCheckStatusComplete CacheCheckStatus = "complete"
	CacheCheckStatusFailed   CacheCheckStatus = "failed"
)

type CacheCheckEntry struct {
	Ref    string           `json:"ref"`
	Status CacheCheckStatus `json:"status"`
	Reason string           `json:"reason,omitempty"`
}

type CacheCheckResult struct {
	Entries  []CacheCheckEntry `json:"entries"`
	Complete int               `json:"complete"`
	Failed   int               `json:"failed"`
}
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
	Images                []CacheImageEntry  `json:"images"`
	Mirror                []MirrorCacheEntry `json:"mirror"`
	MirrorTotal           MirrorCacheTotals  `json:"mirrorTotal"`
	MirrorBoundGatewayIPs []string           `json:"mirrorBoundGatewayIps"`
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
	if args.Version != "" {
		if err := talosversion.Validate(args.Version); err != nil {
			return ClusterSummary{}, err
		}
	}
	intent, err := args.Intent()
	if err != nil {
		return ClusterSummary{}, err
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
	if err := s.requireHelper(); err != nil {
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
	item.ProvisioningIntent = intent
	item.Domain = canonicalDomain
	item.AllowUnsafeDomain = canonicalDomain != "" && args.AllowUnsafeDomain
	item.ImageArchitecture = string(s.hypervisor.Architecture())
	longhornWarning := s.checkLonghornMemoryWarning(item)
	longhornCustomSchematicWarning := s.longhornCustomSchematicWarning(item, args.Schematic != "")
	item.Schematic, item.TalosVersion, err = s.resolveImage(args.Schematic, args.Version, args.Extensions)
	if err != nil {
		return ClusterSummary{}, err
	}
	if args.Schematic != "" && item.Schematic != args.Schematic {
		// The schematic was re-composed: keep the brought id, since the
		// composed one no longer names anything the user wrote.
		item.BaseSchematic = args.Schematic
	}
	// One line at create only; startCluster and status never repeat it.
	talosVersionWarning := talosversion.NewerThanTestedWarning(item.TalosVersion)
	item.TalosExtensions = args.Extensions
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
		result.Warning = joinWarnings(talosVersionWarning, overcommitWarning, hostPressureWarning, longhornWarning, longhornCustomSchematicWarning, subnetWarning)
		startErr := fmt.Errorf("cluster created but failed to start: %w", err)
		if talosVersionWarning != "" {
			// the failure response drops the summary, and a boot failure on
			// an untested version is exactly where this warning is the
			// diagnosis — it must ride the error
			startErr = fmt.Errorf("%w (warning: %s)", startErr, talosVersionWarning)
		}
		return result, startErr
	}
	result := summary(item, true)
	result.Warning = joinWarnings(talosVersionWarning, overcommitWarning, hostPressureWarning, longhornWarning, longhornCustomSchematicWarning, subnetWarning, startWarning)
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
	longhornWarning := s.checkLonghornMemoryWarning(item)
	longhornCustomSchematicWarning := s.longhornCustomSchematicWarning(item, s.defaultSchematic != "" && item.Schematic != "" && item.Schematic != s.defaultSchematic)
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
	result.Warning = joinWarnings(overcommitWarning, hostPressureWarning, longhornWarning, longhornCustomSchematicWarning, subnetWarning)
	return result, nil
}

func (s *Server) longhornCustomSchematicWarning(item cluster.Cluster, custom bool) string {
	if item.CSI != cluster.CSILonghorn || !custom {
		return ""
	}
	return "Longhorn on a custom Talos schematic requires siderolabs/iscsi-tools and siderolabs/util-linux-tools; tbx's default generated schematic already includes them"
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
		EFIVarsPath:          filepath.Join(dir, node.Name+".efi"),
		ConsoleSocketPath:    filepath.Join(dir, node.Name+".console.sock"),
		GuestAgentSocketPath: guestAgentSocketPath(item, dir, node),
		Restore:              restore,
	})
}

// guestAgentSocketPath asks the backend for a guest-agent channel only when the
// cluster baked the extension. Backends without the capability ignore it, which
// is what keeps the same file usable on both host substrates.
func guestAgentSocketPath(item cluster.Cluster, dir string, node cluster.Node) string {
	if !extensions.Requested(item.TalosExtensions, extensions.GuestAgent) {
		return ""
	}
	return filepath.Join(dir, node.Name+".qga.sock")
}

func helperInstallError(err error) error {
	return fmt.Errorf("network helper unavailable; run `sudo tbx system install`: %w", err)
}

// requireHelper checks the network helper is reachable; the injected
// helperCheck is the test seam.
func (s *Server) requireHelper() error {
	if s.helperCheck != nil {
		return s.helperCheck()
	}
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
	s.cancelProvisionLocked(name)
	s.invalidateStoragePhaseLocked(name)
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
	s.cancelProvisionLocked(args.Name)
	dir, err := cluster.Dir(args.Name)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("cluster %q does not exist", args.Name)
	}
	if err := disableHostBGP(args.Name); err != nil {
		log.Printf("disable host BGP for %s during force destroy: %v", args.Name, err)
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
	s.invalidateStoragePhaseLocked(args.Name)
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
	status, _, err := s.addNodeLocked(raw)
	return status, err
}

func (s *Server) addNodeLocked(raw json.RawMessage) (NodeStatus, []provisionTask, error) {
	var args nodeArgs
	if err := decodeArgs(raw, &args); err != nil {
		return NodeStatus{}, nil, err
	}
	if args.Role == "" {
		args.Role = cluster.RoleWorker
	}
	item, err := cluster.Load(args.Cluster)
	if err != nil {
		return NodeStatus{}, nil, err
	}
	addMiB := item.DefaultsFor(args.Role).MemoryMiB
	overcommitWarning, err := s.checkOvercommit(addMiB, args.Force)
	if err != nil {
		return NodeStatus{}, nil, err
	}
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		return NodeStatus{}, nil, err
	}
	hostPressureWarning, err := s.checkHostPressure(dir, args.Force)
	if err != nil {
		return NodeStatus{}, nil, err
	}
	running := s.clusterRunning(item.Name)
	var subnetWarning string
	if running {
		subnetWarning, err = cluster.CheckSubnetIndex(item.SubnetIndex, s.hostSubnetSources())
		if err != nil {
			return NodeStatus{}, nil, err
		}
	}
	cachedDisk, err := s.cachedDisk(item)
	if err != nil {
		return NodeStatus{}, nil, err
	}
	node, err := cluster.AddNode(&item, args.Role, args.Name)
	if err != nil {
		return NodeStatus{}, nil, err
	}
	if err := cluster.ProvisionDisks(item, cachedDisk); err != nil {
		_ = removeNodeFiles(item.Name, node.Name)
		return NodeStatus{}, nil, err
	}
	if err := cluster.Save(item); err != nil {
		_ = removeNodeFiles(item.Name, node.Name)
		return NodeStatus{}, nil, err
	}
	if running {
		machine, err := s.launchMachine(item, node, nil)
		if err != nil {
			return nodeStatus(node, item.SubnetIndex, false), nil, fmt.Errorf("node added but failed to create VM: %w", err)
		}
		s.vms[item.Name][node.Name] = machine
	}
	status := nodeStatus(node, item.SubnetIndex, s.nodeRunning(item.Name, node.Name))
	customSchematic := s.defaultSchematic != "" && item.Schematic != "" && item.Schematic != s.defaultSchematic
	status.Warning = joinWarnings(overcommitWarning, hostPressureWarning, subnetWarning, s.longhornCustomSchematicWarning(item, customSchematic))
	return status, s.beginNodeMutationProvisionLocked(item), nil
}

func (s *Server) removeNodeLocked(raw json.RawMessage) (NodeStatus, []provisionTask, error) {
	var args nodeArgs
	if err := decodeArgs(raw, &args); err != nil {
		return NodeStatus{}, nil, err
	}
	item, err := cluster.Load(args.Cluster)
	if err != nil {
		return NodeStatus{}, nil, err
	}
	node, err := cluster.RemoveNode(&item, args.Name)
	if err != nil {
		return NodeStatus{}, nil, err
	}
	if machine := s.vms[item.Name][node.Name]; machine != nil {
		if err := closeMachine(machine); err != nil {
			return NodeStatus{}, nil, fmt.Errorf("stop node %s: %w", node.Name, err)
		}
		delete(s.vms[item.Name], node.Name)
	}
	if err := cluster.Save(item); err != nil {
		return NodeStatus{}, nil, err
	}
	if err := removeNodeFiles(item.Name, node.Name); err != nil {
		return NodeStatus{}, nil, err
	}
	return nodeStatus(node, item.SubnetIndex, false), s.beginNodeMutationProvisionLocked(item), nil
}

func (s *Server) handleNodeMutationLocked(request Request) (any, []provisionTask, error) {
	switch request.Op {
	case "node.add":
		result, tasks, err := s.addNodeLocked(request.Args)
		if err != nil {
			return nil, nil, err
		}
		return result, tasks, nil
	case "node.remove":
		result, tasks, err := s.removeNodeLocked(request.Args)
		if err != nil {
			return nil, nil, err
		}
		return result, tasks, nil
	default:
		return nil, nil, fmt.Errorf("operation %q is not a node mutation", request.Op)
	}
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
		clusterStatus := ClusterStatus{Name: item.Name, Subnet: cluster.SubnetCIDR(item.SubnetIndex), Domain: item.EffectiveDomain(), TalosVersion: item.TalosVersion, Schematic: item.Schematic, BaseSchematic: item.BaseSchematic, TalosExtensions: item.TalosExtensions, ProvisioningIntent: item.ProvisioningIntent, BGP: item.BGP, Running: s.clusterRunning(item.Name), Capabilities: s.clusterCapabilities(item), subnetIndex: item.SubnetIndex}
		for _, node := range item.Nodes {
			running := s.nodeRunning(item.Name, node.Name)
			clusterStatus.Nodes = append(clusterStatus.Nodes, NodeStatus{Name: node.Name, Role: node.Role, MAC: node.MAC, Phase: ClassifyPhase(running, ProbeResult{})})
		}
		clusterStatus.Hints = Hints(clusterStatus)
		result = append(result, clusterStatus)
	}
	return result, nil
}

// clusterCapabilities reports only the capabilities this cluster actually asked
// for, so a status listing stays silent about gates nobody depends on.
func (s *Server) clusterCapabilities(item cluster.Cluster) []CapabilityStatus {
	if s.hypervisor == nil || !extensions.Requested(item.TalosExtensions, extensions.GuestAgent) {
		return nil
	}
	guestAgent := s.hypervisor.Capabilities().GuestAgent
	return []CapabilityStatus{{
		Name:      extensions.GuestAgent,
		Supported: guestAgent.Supported,
		Reason:    guestAgent.Reason,
	}}
}

func (s *Server) refreshNodeStatuses(statuses []ClusterStatus) {
	lookupIP := s.nodeIPLookup
	if lookupIP == nil {
		lookupIP = cluster.LookupIP
	}
	probe := s.nodeProbe
	if probe == nil {
		probe = probeAPID
	}
	for i := range statuses {
		for j, snapshot := range statuses[i].Nodes {
			node := cluster.Node{Name: snapshot.Name, Role: snapshot.Role, MAC: snapshot.MAC}
			statuses[i].Nodes[j] = nodeStatusWith(node, statuses[i].subnetIndex, snapshot.Phase != PhaseStopped, lookupIP, probe)
		}
		statuses[i].Hints = Hints(statuses[i])
	}
}

func refreshKubernetesReadiness(statuses []ClusterStatus) {
	for index := range statuses {
		status := &statuses[index]
		if status.CNI != cluster.CNIFlannel && status.CNI != cluster.CNICilium {
			continue
		}
		if !status.Running {
			status.KubernetesReady = false
			status.VIP = ""
			status.VIPLive = false
			status.Hints = Hints(*status)
			continue
		}
		nodeNames := make([]string, 0, len(status.Nodes))
		for _, node := range status.Nodes {
			nodeNames = append(nodeNames, node.Name)
		}
		status.KubernetesReady = kubernetesReady(status.Name, nodeNames)
		if status.KubernetesReady && status.LB {
			item, err := cluster.Load(status.Name)
			if err == nil {
				status.VIP, status.VIPLive = loadBalancerVIP(item)
			}
		}
		status.Hints = Hints(*status)
	}
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
	schematic, talosVersion, err := s.resolveImage(args.Schematic, args.Version, nil)
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
	ctx, cancel := s.lifecycleTimeoutContext(cacheWarmTimeout)
	defer cancel()
	return s.warmCache(ctx, args.Refs, s.imageArchitecture())
}

func (s *Server) checkMirrorCache(raw json.RawMessage) (CacheCheckResult, error) {
	var args CacheCheckArgs
	if err := decodeArgs(raw, &args); err != nil {
		return CacheCheckResult{}, err
	}
	if len(args.Refs) == 0 {
		return CacheCheckResult{}, errors.New("at least one image reference is required")
	}
	for _, ref := range args.Refs {
		if err := ValidateWarmRef(ref); err != nil {
			return CacheCheckResult{}, err
		}
	}
	if s.checkCache == nil {
		return CacheCheckResult{}, errors.New("cache check is not configured")
	}
	ctx, cancel := s.lifecycleTimeoutContext(cacheWarmTimeout)
	defer cancel()
	return s.checkCache(ctx, args.Refs, s.imageArchitecture(), args.Deep)
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
	if s.boundMirrorGateways != nil {
		result.MirrorBoundGatewayIPs = s.boundMirrorGateways()
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

	if strings.Count(ref, "@") > 1 {
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
	if !isValidWarmRegistry(host) || strings.HasSuffix(name, "/") || strings.HasPrefix(remainder, ":") {
		return fmt.Errorf("image reference %q is malformed", ref)
	}

	repository, tag, hasTag := remainder, "", false
	if colon := strings.LastIndex(remainder, ":"); colon >= 0 {
		repository, tag, hasTag = remainder[:colon], remainder[colon+1:], true
	}
	if !isValidWarmRepository(repository) || hasTag && !isValidWarmTag(tag) {
		return fmt.Errorf("image reference %q is malformed", ref)
	}
	if !hasDigest && !hasTag {
		return fmt.Errorf("image reference %q must include a non-latest tag or digest", ref)
	}
	if tag == "latest" {
		return fmt.Errorf("image reference %q must not use :latest", ref)
	}
	return nil
}

func isValidWarmRegistry(value string) bool {
	if strings.HasPrefix(value, "[") {
		closingBracket := strings.IndexByte(value, ']')
		if closingBracket < 0 {
			return false
		}
		address := value[1:closingBracket]
		if !strings.Contains(address, ":") || net.ParseIP(address) == nil {
			return false
		}
		port := value[closingBracket+1:]
		return port == "" || strings.HasPrefix(port, ":") && isValidWarmPort(port[1:])
	}
	host := value
	if colon := strings.LastIndex(value, ":"); colon >= 0 {
		host = value[:colon]
		if host == "" || !isValidWarmPort(value[colon+1:]) {
			return false
		}
	}
	if host == "localhost" {
		return true
	}
	if len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if !isValidWarmHostLabel(label) {
			return false
		}
	}
	return true
}

func isValidWarmHostLabel(value string) bool {
	if value == "" || len(value) > 63 || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for _, character := range value {
		if !isAlphaNumeric(character) && character != '-' {
			return false
		}
	}
	return true
}

func isValidWarmRepository(value string) bool {
	if value == "" || len(value) > 255 {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if !isValidWarmRepositoryComponent(component) {
			return false
		}
	}
	return true
}

func isValidWarmRepositoryComponent(value string) bool {
	if value == "" || !isLowerAlphaNumeric(value[0]) {
		return false
	}
	for index := 1; index < len(value); {
		character := value[index]
		if isLowerAlphaNumeric(character) {
			index++
			continue
		}
		switch character {
		case '-':
			for index < len(value) && value[index] == '-' {
				index++
			}
		case '.', '_':
			index++
			if character == '_' && index < len(value) && value[index] == '_' {
				index++
			}
		default:
			return false
		}
		if index == len(value) || !isLowerAlphaNumeric(value[index]) {
			return false
		}
	}
	return true
}

func isValidWarmTag(value string) bool {
	if len(value) == 0 || len(value) > 128 || !isTagStart(value[0]) {
		return false
	}
	for index := 1; index < len(value); index++ {
		character := value[index]
		if !isTagStart(character) && character != '.' && character != '-' {
			return false
		}
	}
	return true
}

func isLowerAlphaNumeric(character byte) bool {
	return (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9')
}

func isAlphaNumeric(character rune) bool {
	return (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9')
}

func isTagStart(character byte) bool {
	return isLowerAlphaNumeric(character) || (character >= 'A' && character <= 'Z') || character == '_'
}

func isDecimal(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func isValidWarmPort(value string) bool {
	if !isDecimal(value) {
		return false
	}
	port := 0
	for _, character := range value {
		digit := int(character - '0')
		if port > (65535-digit)/10 {
			return false
		}
		port = port*10 + digit
	}
	return port > 0
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

// resolveImage guards the request boundary: a requested version must be
// well-formed and inside the support window before it reaches image
// resolution. Stored cluster state goes through imageDefaults instead —
// a floor bump must not retroactively refuse clusters that already exist.
func (s *Server) resolveImage(schematic, talosVersion string, requestedExtensions []string) (string, string, error) {
	if talosVersion != "" {
		if err := talosversion.Validate(talosVersion); err != nil {
			return "", "", err
		}
	}
	return s.imageDefaults(schematic, talosVersion, requestedExtensions)
}

func (s *Server) imageDefaults(schematic, talosVersion string, requestedExtensions []string) (string, string, error) {
	if talosVersion == "" {
		talosVersion = DefaultTalosVersion
	}
	if len(requestedExtensions) > 0 {
		// Composition owns validation: it is skipped entirely when the
		// composed id is already recorded, which is what lets a cached
		// composed image be created from offline.
		composed, err := s.cache.ComposeSchematic(schematic, talosVersion, requestedExtensions)
		if err != nil {
			return "", "", err
		}
		return composed, talosVersion, nil
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
	// item.Schematic already is the composed id when the cluster requested
	// extensions, so stored state never re-composes.
	schematic, talosVersion, err := s.imageDefaults(item.Schematic, item.TalosVersion, nil)
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
		Name:               item.Name,
		Index:              item.Index,
		SubnetIndex:        item.SubnetIndex,
		ControlPlanes:      item.ControlPlanes,
		Workers:            item.Workers,
		NodeDefaults:       item.NodeDefaults,
		TalosVersion:       item.TalosVersion,
		Schematic:          item.Schematic,
		ProvisioningIntent: item.ProvisioningIntent,
		Domain:             item.Domain,
		AllowUnsafeDomain:  item.AllowUnsafeDomain,
		Running:            running,
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
