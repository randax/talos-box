package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
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
	"github.com/randax/talos-box/internal/mirror"
	"github.com/randax/talos-box/internal/provision"
	"github.com/randax/talos-box/internal/talosversion"
)

type createArgs struct {
	Name          string                `json:"name"`
	Hypervisor    hypervisor.Name       `json:"hypervisor,omitempty"`
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
	// ConfigManaged marks a create that came from a talosbox.yaml spec, so
	// the cluster's later recovery hints can name `tbx up` honestly (#267).
	// It is set by the daemon's own up path, which re-encodes these args;
	// `tbx cluster create` never sends it.
	ConfigManaged bool `json:"configManaged,omitempty"`
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

// CachePullArgs asks for image combinations to be made available offline.
// Combinations carries the file-aware form; the scalar fields are the older
// single-combination wire shape and stay honoured for an older tbx.
type CachePullArgs struct {
	Schematic    string                 `json:"schematic,omitempty"`
	Version      string                 `json:"version,omitempty"`
	TalosVersion string                 `json:"talosVersion,omitempty"`
	Combinations []CachePullCombination `json:"combinations,omitempty"`
	// FromFile marks the flagless form, where the combinations describe every
	// cluster the file declares. Only then does warming their images and
	// calling every other pinned combination a stray mean anything.
	FromFile bool `json:"fromFile,omitempty"`
	// SkipImages is --no-images: fetch the disk images and stop there.
	SkipImages bool `json:"skipImages,omitempty"`
}

// CachePullCombination is one cluster's resolved image pin, with inheritance
// already applied by the client. Intent is the cluster's declared provisioning
// path, which decides the images the pull warms alongside the disk image.
type CachePullCombination struct {
	Schematic  string                     `json:"schematic,omitempty"`
	Version    string                     `json:"version,omitempty"`
	Extensions []string                   `json:"extensions,omitempty"`
	Intent     cluster.ProvisioningIntent `json:"intent"`
}

// DefaultCacheWarmJobs is the blob-download width `tbx cache warm` asks for
// when --jobs is not given, and MaxCacheWarmJobs the most it may ask for: the
// daemon-wide ceiling shared by every warm in flight (#506).
const (
	DefaultCacheWarmJobs = mirror.DefaultWarmJobs
	MaxCacheWarmJobs     = mirror.MaxWarmJobs
)

// CacheWarmEntryStagePrefix marks a cache.warm stage that carries one finished
// entry as JSON, narrated in list order as the warm progresses, so the client
// can report each ref while the rest still downloads (#506).
const CacheWarmEntryStagePrefix = "cache.warm entry "

// ParseCacheWarmEntryStage decodes a stage produced for a finished warm entry.
// Any other narration reports false.
func ParseCacheWarmEntryStage(stage string) (CacheWarmEntry, bool) {
	raw, ok := strings.CutPrefix(stage, CacheWarmEntryStagePrefix)
	if !ok {
		return CacheWarmEntry{}, false
	}
	var entry CacheWarmEntry
	if err := json.Unmarshal([]byte(raw), &entry); err != nil {
		return CacheWarmEntry{}, false
	}
	return entry, true
}

type CacheWarmArgs struct {
	Refs    []string `json:"refs"`
	Refresh bool     `json:"refresh,omitempty"`
	// Jobs bounds the blob downloads kept in flight; zero takes the mirror's
	// default and the daemon caps it (#506).
	Jobs int `json:"jobs,omitempty"`
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
	Domain            string `json:"domain,omitempty"`
	AllowUnsafeDomain bool   `json:"allowUnsafeDomain,omitempty"`
	Running           bool   `json:"running"`
	// Suspended reports saved VM memory on disk. Unlike Running it survives a
	// daemon restart, which is what lets a client tell a suspended cluster from
	// a merely stopped one.
	Suspended bool `json:"suspended,omitempty"`
	// SuspendSurvivesDaemonRestart is set only when the live daemon can resolve
	// a suspended cluster's backend and that backend restores from disk alone.
	SuspendSurvivesDaemonRestart bool   `json:"suspendSurvivesDaemonRestart,omitempty"`
	Warning                      string `json:"warning,omitempty"`
	// Warnings is the same advisory set as Warning, one entry per finding.
	// Warning stays populated for older clients that only read it.
	Warnings  []string `json:"warnings,omitempty"`
	Narration []string `json:"narration,omitempty"`
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
	// Suspended reports that this node's own memory is saved on disk, which
	// only suspend writes and only for nodes that were running. A stopped
	// member of a suspended cluster that has no save is plain stopped — a
	// resume brings it up cold, not back where it left off.
	Suspended bool `json:"suspended,omitempty"`
	// Kubelet is the node's kubelet as its Talos machine API reports it, when
	// the daemon could ask. The phase above is derived from apid alone, and a
	// node whose kubelet is in a permanent crash loop answers apid perfectly
	// well — without this it reads `configured` while being dead to
	// Kubernetes (#357). Nil means no reading, never "healthy".
	Kubelet *NodeService `json:"kubelet,omitempty"`
	// Services is the complete machine-API service list. Kubelet above remains
	// a compatibility projection for protocol-v11 clients.
	Services []NodeService `json:"services,omitempty"`
	// StalledServices contains only startup states with a trustworthy age over
	// the threshold. It is a list because one frozen pull must not hide another.
	StalledServices []StalledService `json:"stalledServices,omitempty"`
	// ServiceProbe distinguishes an inspected node with no services from one
	// the daemon could not inspect. Nil means the node was not eligible.
	ServiceProbe *ServiceProbe `json:"serviceProbe,omitempty"`
	Warning      string        `json:"warning,omitempty"`
	// Warnings is the same advisory set as Warning, one entry per finding.
	// Warning stays populated for older clients that only read it.
	Warnings []string `json:"warnings,omitempty"`
	// StartedAt is when the daemon launched this node's VM, when it knows:
	// the clock a stalled boot is measured against (#288). Nil means the VM
	// was not started by this daemon process.
	StartedAt *time.Time `json:"startedAt,omitempty"`
	// UnreachableSince is when a node that had been answering stopped: the
	// clock a stall is aged against once the node has proved it can answer
	// (#288). Nil means it has not answered since its VM launched, so StartedAt
	// is the only honest clock — and nil for a node that is answering now.
	UnreachableSince *time.Time `json:"unreachableSince,omitempty"`
	// RebootedAt is when this daemon observed Talos boot_time change while the
	// VM process stayed running. The status notice is transient for 15 minutes.
	RebootedAt *time.Time `json:"rebootedAt,omitempty"`
}

type ServiceProbeStatus string

const (
	ServiceProbeSucceeded          ServiceProbeStatus = "succeeded"
	ServiceProbeMissingCredentials ServiceProbeStatus = "missingCredentials"
	ServiceProbeFailed             ServiceProbeStatus = "failed"
)

// ServiceProbe records whether the service list was actually inspected.
type ServiceProbe struct {
	Status ServiceProbeStatus `json:"status"`
	Source string             `json:"source,omitempty"`
	Error  string             `json:"error,omitempty"`
}

// StalledService is a Talos service whose current startup run has exceeded
// the operator-action threshold.
type StalledService struct {
	Service string    `json:"service"`
	State   string    `json:"state"`
	Since   time.Time `json:"since"`
}

// UnreachableFor reports how long the node has been silent: since it stopped
// answering when it ever answered, otherwise since its VM launched. It is zero
// when the daemon knows neither — it cannot prove the node is stuck.
func (n NodeStatus) UnreachableFor(now time.Time) time.Duration {
	switch {
	case n.UnreachableSince != nil:
		return now.Sub(*n.UnreachableSince)
	case n.StartedAt != nil:
		return now.Sub(*n.StartedAt)
	default:
		return 0
	}
}

// suspendedPhase promotes a stopped node holding its own saved memory to
// PhaseSuspended, so the JSON surface says what the table has said since #360
// instead of the coarser "stopped" a consumer keying on phase alone misread
// (#415). Suspended stays set beside it: it is the same fact, and older
// clients read only the boolean.
func (n NodeStatus) suspendedPhase() NodeStatus {
	if n.Suspended && n.Phase == PhaseStopped {
		n.Phase = PhaseSuspended
	}
	return n
}

// answeredSinceStart reports whether the node answered at least once since its
// VM launched, which decides how its silence is described.
func (n NodeStatus) answeredSinceStart() bool { return n.UnreachableSince != nil }

// StoragePhase is the observed storage readiness for a CSI-backed cluster.
type StoragePhase string

const (
	StoragePhaseProvisioning StoragePhase = "provisioning"
	StoragePhaseLive         StoragePhase = "live"
	// StoragePhaseFailed is the terminal state an aborted provision settles
	// into. Nothing is converging any more, so reporting `provisioning` would
	// describe work no process is doing (#395).
	StoragePhaseFailed StoragePhase = "failed"
)

// ClusterStatus is the status result for one cluster.
type ClusterStatus struct {
	Name       string          `json:"name"`
	Subnet     string          `json:"subnet"`
	Hypervisor hypervisor.Name `json:"hypervisor,omitempty"`
	// Domain is the cluster's effective domain (explicit or defaulted).
	Domain string `json:"domain"`
	// AllowUnsafeDomain records the opt-in the domain was accepted under, so
	// a recovery hint naming --domain can name the flag that makes it
	// replayable (#267).
	AllowUnsafeDomain bool `json:"allowUnsafeDomain,omitempty"`
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
	BGP     bool `json:"bgp"`
	Running bool `json:"running"`
	// Suspended reports saved VM memory on disk, the difference between a
	// cluster that was stopped and one whose memory is waiting to be resumed —
	// a distinction start would silently erase (#272).
	Suspended bool `json:"suspended,omitempty"`
	// SavedStateStale reports suspended memory whose owning daemon process is
	// gone on a backend that needs it. Where restore depends on the writing
	// process (vz), `tbx system restart --force` has already lost the memory
	// and a resume will cold-boot — which the suspend hint used to promise the
	// opposite of (#413). Where restore reads the save file alone (QEMU), the
	// memory outlives the daemon and this stays false.
	SavedStateStale bool `json:"savedStateStale,omitempty"`
	KubernetesReady bool `json:"kubernetesReady"`
	// KubernetesNotReadySince is when the daemon first observed this cluster
	// failing its Kubernetes readiness probe without a success since — and
	// without a gap longer than unreadyRunAbandonWindow in which nobody looked,
	// which starts a fresh run rather than crediting unwatched time. It is
	// what keeps a momentary apiserver blip from being escalated into "destroy
	// and recreate" (#418). Nil means ready now, or never observed.
	KubernetesNotReadySince *time.Time `json:"kubernetesNotReadySince,omitempty"`
	// Converging names what is still coming back on a cluster whose nodes are
	// up and whose Kubernetes reports Ready — CSI drivers re-registering, a
	// VIP announced but not answering. Empty means nothing outstanding, which
	// is the only reading a single-sample check may treat as converged (#396).
	Converging   []string     `json:"converging,omitempty"`
	StoragePhase StoragePhase `json:"storagePhase,omitempty"`
	StorageError string       `json:"storageError,omitempty"`
	// StoragePending is the benign counterpart of StorageError: the readiness
	// probe has not failed, it has not run yet because the daemon is still
	// clearing the previous pass's objects. It reads as work in progress, so
	// the operator is not shown a fault for a wait the daemon converges out of
	// on its own (#347).
	StoragePending string `json:"storagePending,omitempty"`
	// StorageGate names the convergence gate a running pass is currently held
	// at. Storage cannot go live until every gate ahead of it passes, and the
	// blocking one is frequently not the readiness probe at all — naming the
	// probe regardless sent diagnosis at the wrong subsystem (#391).
	StorageGate string       `json:"storageGate,omitempty"`
	VIP         string       `json:"vip,omitempty"`
	VIPLive     bool         `json:"vipLive"`
	Nodes       []NodeStatus `json:"nodes"`
	// Capabilities reports the host capabilities this cluster's configuration
	// depends on, so a file stays portable across host substrates and the gate
	// is visible instead of silently doing nothing.
	Capabilities []CapabilityStatus `json:"capabilities,omitempty"`
	// ConfigOrigin reports how the cluster came to exist, which decides
	// whether a recovery hint may name `tbx up` (#267). Absent for clusters
	// created before the origin was recorded.
	ConfigOrigin cluster.ConfigOrigin `json:"configOrigin,omitempty"`
	Hints        []string             `json:"hints,omitempty"`
	subnetIndex  int
}

// kubernetesUnreadyFor reports how long the daemon has been watching this
// cluster fail its readiness probe. Zero means it is ready, or that no
// observation window exists at all.
func (c ClusterStatus) kubernetesUnreadyFor(now time.Time) time.Duration {
	if c.KubernetesNotReadySince == nil {
		return 0
	}
	return now.Sub(*c.KubernetesNotReadySince)
}

// unreadyEscalationWindow is how long Kubernetes must stay unreachable before
// status is willing to recommend destroying the cluster. QA's blip recovered
// on its own inside a minute; anything shorter than this is a blip until
// proven otherwise (#418).
const unreadyEscalationWindow = 2 * time.Minute

// KubernetesUnreadyBriefly reports an unreadiness the daemon has watched for
// too little time to call it stuck. An unknown window is not brief: without an
// observation the daemon cannot vouch for the cluster either way, and the
// stuck-cluster advice is what the hint exists for.
func (c ClusterStatus) KubernetesUnreadyBriefly(now time.Time) bool {
	return c.KubernetesNotReadySince != nil && c.kubernetesUnreadyFor(now) < unreadyEscalationWindow
}

// CapabilityStatus is one host capability a cluster depends on, with the reason
// the active hypervisor backend cannot provide it.
type CapabilityStatus struct {
	Name      string `json:"name"`
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
}

// CachePullResult describes the images made ready by cache.pull. The scalar
// fields repeat the first image so a tbx predating Images still reports the
// single-combination pull it asked for.
type CachePullResult struct {
	Schematic    string                  `json:"schematic"`
	Version      string                  `json:"version"`
	Architecture hypervisor.Architecture `json:"architecture"`
	Path         string                  `json:"path"`
	Images       []CachePullImage        `json:"images,omitempty"`
	// Warm reports the mirror warming the pull performed for the clusters it
	// pulled for; nil means none was attempted.
	Warm *CacheWarmResult `json:"warm,omitempty"`
	// Strays are pinned combinations referenced by neither a cluster nor the
	// file that was pulled. They are reported and never deleted.
	Strays []CacheImageEntry `json:"strays,omitempty"`
}

// CachePullImage is one cached and pinned disk image.
type CachePullImage struct {
	Schematic    string                  `json:"schematic"`
	Version      string                  `json:"version"`
	Architecture hypervisor.Architecture `json:"architecture"`
	Path         string                  `json:"path"`
}

type CacheWarmStatus string

const (
	CacheWarmStatusWarmed           CacheWarmStatus = "warmed"
	CacheWarmStatusAlreadyComplete  CacheWarmStatus = "already-complete"
	CacheWarmStatusFailedMissing    CacheWarmStatus = "failed-missing"
	CacheWarmStatusFailedRevalidate CacheWarmStatus = "failed-revalidate"
	// CacheWarmStatusFailed is retained for tolerant rendering of a response
	// from a legacy daemon. Protocol 17 peers return a typed failure.
	CacheWarmStatusFailed CacheWarmStatus = "failed"
)

type CacheWarmEntry struct {
	Ref            string          `json:"ref"`
	Status         CacheWarmStatus `json:"status"`
	Reason         string          `json:"reason,omitempty"`
	RefreshWarning string          `json:"refreshWarning,omitempty"`
	ReResolvedTag  bool            `json:"reResolvedTag,omitempty"`
}

type CacheWarmResult struct {
	Entries          []CacheWarmEntry `json:"entries"`
	Warmed           int              `json:"warmed"`
	AlreadyComplete  int              `json:"alreadyComplete"`
	Failed           int              `json:"failed"`
	FailedMissing    int              `json:"failedMissing"`
	FailedRevalidate int              `json:"failedRevalidate"`
	ReResolvedTags   int              `json:"reResolvedTags,omitempty"`
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
	// AllocatedSize is the bytes the image occupies on disk; Size is the
	// apparent extent of a sparse disk.raw, which overstates it. An older
	// daemon reports no allocated size, which the client renders as before.
	AllocatedSize int64 `json:"allocatedSize,omitempty"`
	// Status and Clusters explain why the combination is kept; an older
	// daemon leaves them empty, which the client renders as no status.
	Status   CacheImageStatus `json:"status,omitempty"`
	Clusters []string         `json:"clusters,omitempty"`
	// Reasons is every keep-reason that applies, strongest last, so the
	// listing shows what a prune weighs instead of one masking reason
	// (#407). Status stays the single strongest one. An older daemon
	// reports none, which the client renders from Status alone.
	Reasons []CacheImageStatus `json:"reasons,omitempty"`
	// Incomplete marks a combination with prunable leftovers but no usable
	// image. It is listed so the preview covers everything prune removes.
	Incomplete bool `json:"incomplete,omitempty"`
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
	Scope      CachePruneScope `json:"scope"`
	ImageCount int             `json:"imageCount"`
	ImageBytes int64           `json:"imageBytes"`
	// KeptCount is how many combinations the reference-aware scope retained
	// (in use, pinned, or the default), so a zero-prune is explainable.
	KeptCount int `json:"keptCount,omitempty"`
	// Images is the removal plan the reference-aware scope executed, so the
	// client can name every combination it lost with its size.
	Images []CacheImageEntry `json:"images,omitempty"`
	Mirror MirrorCacheTotals `json:"mirror"`
}

type MirrorOfflineStatus struct {
	Enabled bool `json:"enabled"`
}

func (s *Server) createCluster(raw json.RawMessage, progress stageFunc) (ClusterSummary, error) {
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
	name, backend, err := s.hypervisorForCreate(args.Hypervisor)
	if err != nil {
		return ClusterSummary{}, err
	}
	// Charge the steady-state pressure verdict for the same guests the
	// provision-start gate is about to admit: stale swap is safe only when free
	// RAM covers both this allocation and the balloon reserve (#483).
	addMiB := controlPlanes*roleMemoryMiB(args.ControlPlane, args.Node) + workers*roleMemoryMiB(args.Worker, args.Node)
	hostPressureWarnings, err := s.checkHostPressure(dir, addMiB, args.Force)
	if err != nil {
		return ClusterSummary{}, err
	}
	if err := s.requireHelper(); err != nil {
		return ClusterSummary{}, err
	}
	// One gate, one arithmetic: charge each role what it will actually be
	// created with, so a create admits exactly the clusters a later
	// `cluster start` can reassemble (#398).
	overcommitWarning, err := s.checkOvercommit(addMiB, args.Force)
	if err != nil {
		return ClusterSummary{}, err
	}
	// The projected-start gate runs before anything is written: a create that
	// cannot safely boot must not leave a cluster directory behind (#334).
	provisionStartWarnings, preBalloonedMiB, err := s.checkProvisionStart(dir, addMiB, args.Force)
	if err != nil {
		return ClusterSummary{}, err
	}
	// Every step between the gate and the launch can fail — a domain clash, an
	// image fetch, a disk clone — and a start that never happened must not keep
	// memory out of the running guests for the rest of the hold's TTL (#398).
	launched := false
	defer func() {
		if !launched {
			s.releaseBalloonHold(preBalloonedMiB)
		}
	}()
	hostPressureWarnings = append(hostPressureWarnings, provisionStartWarnings...)
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
	item.Hypervisor = string(name)
	item.ImageArchitecture = string(backend.Architecture())
	// A create that names no talosbox.yaml is imperative — including one from
	// a CLI predating the flag, which is exactly what `tbx cluster create`
	// sends.
	item.ConfigOrigin = cluster.OriginImperative
	if args.ConfigManaged {
		item.ConfigOrigin = cluster.OriginManaged
	}
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
	// The image fetch is the long pole of a cold create — ~100 MB from the
	// Image Factory — and used to happen behind a silent request (#273).
	progress.stage("preparing the Talos %s image", item.TalosVersion)
	cachedDisk, err := s.cache.Ensure(item.Schematic, item.TalosVersion, imagecache.Architecture(backend.Architecture()))
	if err != nil {
		return ClusterSummary{}, err
	}
	progress.stage("cloning %d node disk(s)", len(item.Nodes))
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
	// The hold is a boot-window budget, so its clock has to start at the
	// launch, not at the admission: the image fetch and disk clones above are
	// unbounded and can outlast the TTL, and the balloon manager would then
	// inflate the reclaimed guests back before these ones have booted — the
	// squeeze the pre-balloon was taken to prevent (#398).
	s.rearmBalloonHold(preBalloonedMiB)
	progress.stage("starting %d node(s)", len(item.Nodes))
	startWarnings, err := s.start(item)
	if err != nil {
		// start rolled back whatever it launched, so nothing is booting and
		// the hold protects nothing — hand it back now, not at the TTL.
		result := summary(item, false)
		result.setWarnings(append([]string{talosVersionWarning, overcommitWarning}, append(hostPressureWarnings, longhornWarning, longhornCustomSchematicWarning, subnetWarning)...)...)
		startErr := fmt.Errorf("cluster created but failed to start: %w", err)
		if talosVersionWarning != "" {
			// the failure response drops the summary, and a boot failure on
			// an untested version is exactly where this warning is the
			// diagnosis — it must ride the error
			startErr = fmt.Errorf("%w (warning: %s)", startErr, talosVersionWarning)
		}
		return result, startErr
	}
	launched = true
	result := summary(item, true)
	result.setWarnings(append([]string{talosVersionWarning, overcommitWarning}, append(hostPressureWarnings, append([]string{longhornWarning, longhornCustomSchematicWarning, subnetWarning}, startWarnings...)...)...)...)
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
	bootingMiB := s.stoppedNodeMemoryMiB(item)
	hostPressureWarnings, err := s.checkHostPressure(dir, bootingMiB, args.Force)
	if err != nil {
		return ClusterSummary{}, err
	}
	// Same projected-start gate as create: bringing stopped nodes up beside
	// running guests is the same concurrent-bringup risk (#334). It is charged
	// for the nodes this start actually boots, so a partly-running cluster —
	// which start also boots the stopped half of — is gated too, and the
	// members already running are not counted twice.
	preBalloonedMiB := 0
	if bootingMiB > 0 {
		provisionStartWarnings, held, err := s.checkProvisionStart(dir, bootingMiB, args.Force)
		if err != nil {
			return ClusterSummary{}, err
		}
		preBalloonedMiB = held
		hostPressureWarnings = append(hostPressureWarnings, provisionStartWarnings...)
	}
	// The launch is right below, so the hold's clock already starts where it
	// should; but a start that fails — and rolls its own launches back — must
	// hand the pre-balloon back instead of pinning the running guests at their
	// reclaimed targets for the rest of the TTL (#398).
	startWarnings, err := s.start(item)
	if err != nil {
		s.releaseBalloonHold(preBalloonedMiB)
		return ClusterSummary{}, err
	}
	result := summary(item, true)
	result.setWarnings(append([]string{overcommitWarning}, append(hostPressureWarnings, append([]string{longhornWarning, longhornCustomSchematicWarning}, startWarnings...)...)...)...)
	return result, nil
}

func (s *Server) longhornCustomSchematicWarning(item cluster.Cluster, custom bool) string {
	if item.CSI != cluster.CSILonghorn || !custom {
		return ""
	}
	return "Longhorn on a custom Talos schematic requires siderolabs/iscsi-tools and siderolabs/util-linux-tools; tbx's default generated schematic already includes them"
}

func (s *Server) start(item cluster.Cluster) ([]string, error) {
	// The helper serves this cluster's DHCP from the copy tbxd pushes, and the
	// nodes below take their addresses from it: a node that attaches to a
	// subnet the helper has never heard of never gets one. The push therefore
	// precedes the first attach, and a failure fails the start.
	if err := SyncHelperState(); err != nil {
		return nil, err
	}
	// The subnet was decided at create time and belongs to this cluster, so it
	// is only inspected for advisory routing findings. Re-running the
	// create-time collision guard would refuse the cluster's own bridge, which
	// suspend leaves up and an unclean stop can strand (#271).
	subnetWarning, err := cluster.AttachedSubnetWarning(item.SubnetIndex, s.hostSubnetSources())
	if err != nil {
		return nil, err
	}
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		return nil, err
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
				return nil, fmt.Errorf("release inactive VM %s: %w", node.Name, err)
			}
			delete(nodes, node.Name)
		}
		machine, err := s.launchMachine(item, node, nil)
		if err != nil {
			rollbackErr := s.rollbackStarted(item.Name, nodes, started)
			// The rolled-back nodes did launch: they cold-booted and advanced
			// their disks, so their saved memory no longer matches what is on
			// disk and must not survive for a later `cluster resume` to restore.
			// The nodes that never launched keep theirs — nothing touched them.
			// A discard that itself fails must reach the operator: the stale
			// save resurrects the suspended status and the resume hint.
			var discardErrs []error
			for _, name := range started {
				if _, failure := discardSavedState(dir, name); failure != "" {
					discardErrs = append(discardErrs, errors.New(failure))
				}
			}
			return nil, errors.Join(append([]error{fmt.Errorf("create VM %s: %w", node.Name, err), rollbackErr}, discardErrs...)...)
		}
		nodes[node.Name] = machine
		started = append(started, node.Name)
	}
	// start is a cold boot: suspended memory left by an earlier suspend is
	// superseded by these launches and must not outlive them, or status keeps
	// reporting the cluster Suspended and the hint keeps recommending a restore
	// onto memory that no longer matches what is running. The saves are only
	// superseded once EVERY launch has succeeded — launchMachine never reads a
	// save, so a launch that never happened leaves its save intact. A failure
	// partway through discards only the saves of the nodes that did launch (see
	// the rollback above): those cold-booted and advanced their disks, while the
	// untouched members stay resumable.
	var discarded bool
	var discardFailures []string
	for _, name := range started {
		dropped, failure := discardSavedState(dir, name)
		if dropped {
			discarded = true
		}
		if failure != "" {
			discardFailures = append(discardFailures, failure)
		}
	}
	go s.bindMirrors(item.SubnetIndex) // async: don't hold opMu across the retry
	warnings := []string{subnetWarning}
	if discarded {
		warnings = append(warnings, discardedSaveStateWarning("the cluster"))
	}
	return append(warnings, discardFailures...), nil
}

// startAndLogWarning starts the cluster on an operation's behalf, logging any
// advisory finding and returning it so the operation can also carry it back to
// the operator — the daemon log is not somewhere the CLI user looks.
func (s *Server) startAndLogWarning(item cluster.Cluster) ([]string, error) {
	warnings, err := s.start(item)
	for _, warning := range warnings {
		if warning != "" {
			log.Printf("start %s: %s", item.Name, warning)
		}
	}
	return warnings, err
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
	_, backend, err := s.hypervisorForCluster(item)
	if err != nil {
		return nil, err
	}
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		return nil, err
	}
	sizing := item.DefaultsFor(node.Role)
	machine, err := backend.Launch(context.Background(), hypervisor.Spec{
		CPUs:           sizing.CPUs,
		MemoryMiB:      sizing.MemoryMiB,
		DiskPath:       filepath.Join(dir, node.Name+".img"),
		MAC:            node.MAC,
		DisableBalloon: s.balloonDisabled,
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
	if err != nil {
		return nil, err
	}
	s.recordVMStart(item.Name, node.Name)
	return machine, nil
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
	if helper.IsProtocolMismatch(err) {
		// The mismatch names the real fix (upgrade the helper); an "enable
		// the socket" preamble would be the wrong remediation read first.
		return err
	}
	return fmt.Errorf("network helper unavailable; %s: %w", helper.UnavailableAdvice(), err)
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
		s.forgetCluster(name)
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
	// no balloon retarget may land on a guest while its VM stops (#513)
	defer s.quiesceBalloon()()
	errs := closeEach(names, func(name string) hypervisor.Machine { return nodes[name] })
	errorsByName := make(map[string]error, len(names))
	for i, name := range names {
		errorsByName[name] = errs[i]
	}

	var resultErr error
	for _, name := range names {
		if err := errorsByName[name]; err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("stop VM %s: %w", name, err))
			continue
		}
		delete(nodes, name)
		s.forgetNode(clusterName, name)
	}
	if len(nodes) == 0 {
		delete(s.vms, clusterName)
		s.forgetCluster(clusterName)
	}
	return resultErr
}

// DestroySummary is cluster.destroy's response: what the destroy actually
// removed. The most destructive verb in the CLI used to answer with the
// cluster's name alone, which gave the operator nothing to check the scope of
// the destruction against (#422). Every count is measured before anything is
// deleted; a partially-destroyed cluster reports what could still be counted.
type DestroySummary struct {
	Name string `json:"name"`
	// Nodes is nil when the node count could not be established — a
	// partially-destroyed cluster whose cluster.json is gone still has its
	// disks removed, and reporting that as zero nodes would understate the
	// destruction. A count that is known is always sent, zero included.
	Nodes     *int `json:"nodes,omitempty"`
	Snapshots int  `json:"snapshots"`
	// DiskBytes is the cluster's whole state directory — node disks, snapshots
	// and configuration — as it stood before the removal. It is the sum of each
	// file's allocated blocks, so blocks cloned from the image cache or shared
	// with a snapshot count once per file: it reports state removed, not
	// capacity the host gets back.
	DiskBytes int64  `json:"diskBytes"`
	Domain    string `json:"domain,omitempty"`
	// ResolverWithdrawn is set only for a cluster whose own resolver file the
	// destroy removed; a cluster on the default domain shares one that stays.
	ResolverWithdrawn bool `json:"resolverWithdrawn,omitempty"`
	// BridgeRemoved names the host bridge the destroy took down, and is empty
	// wherever there was none to take down — a host whose subnet bridge is
	// vmnet-owned, or one that never built it. Reporting the name is what lets
	// an operator tell residue from design (#445).
	BridgeRemoved string `json:"bridgeRemoved,omitempty"`
	// BridgeWarning carries why a bridge that should have come down did not.
	// Without it a failed teardown reads exactly like a host that had nothing
	// to remove — the summary line is simply absent — and the operator has no
	// reason to go looking (#445).
	BridgeWarning string `json:"bridgeWarning,omitempty"`
}

func (s *Server) destroyCluster(raw json.RawMessage) (DestroySummary, error) {
	var args destroyArgs
	if err := decodeArgs(raw, &args); err != nil {
		return DestroySummary{}, err
	}
	if !args.Force {
		return DestroySummary{}, errors.New("cluster.destroy requires force=true")
	}
	s.cancelProvisionLocked(args.Name)
	dir, err := cluster.Dir(args.Name)
	if err != nil {
		return DestroySummary{}, err
	}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return DestroySummary{}, ClusterMissingError(args.Name)
	}
	if err := disableHostBGP(args.Name); err != nil {
		log.Printf("disable host BGP for %s during force destroy: %v", args.Name, err)
	}
	// Everything the summary reports is measured here, while it still exists.
	summary := DestroySummary{Name: args.Name, DiskBytes: directoryBytes(dir)}
	if snapshots, listErr := cluster.ListSnapshots(args.Name); listErr == nil {
		summary.Snapshots = len(snapshots)
	}
	var customDomain bool
	// A partially-destroyed cluster (state dir present, cluster.json gone) has
	// no recorded subnet, so there is no bridge this destroy can claim to own.
	subnetIndex := -1
	// stop what we can, but a partially-destroyed cluster (state dir present,
	// cluster.json gone) must still be removable — and still summarised
	if item, loadErr := cluster.Load(args.Name); loadErr == nil {
		nodes := len(item.Nodes)
		summary.Nodes = &nodes
		summary.Domain = item.EffectiveDomain()
		customDomain = item.Domain != ""
		subnetIndex = item.SubnetIndex
		if err := s.stop(args.Name); err != nil {
			return DestroySummary{}, err
		}
	}
	if err := cluster.Destroy(args.Name); err != nil {
		return DestroySummary{}, err
	}
	s.forgetCluster(args.Name)
	s.invalidateStoragePhaseLocked(args.Name)
	logHelperSyncFailure(fmt.Sprintf("sync helper state after destroying %s", args.Name))
	if subnetIndex >= 0 && hostBridgeGOOS == "linux" {
		bridge, err := releaseSubnetBridge(subnetIndex)
		if err != nil {
			log.Printf("release host bridge for %s subnet %s: %v", args.Name, cluster.SubnetCIDR(subnetIndex), err)
			summary.BridgeWarning = bridgeReleaseWarning(subnetIndex, err)
		}
		summary.BridgeRemoved = bridge
	}
	if err := SyncResolverFiles(); err != nil {
		log.Printf("resolver files after destroying %s: %v", args.Name, err)
	} else {
		summary.ResolverWithdrawn = customDomain
	}
	return summary, nil
}

// hostBridgeGOOS is the host platform the destroy's bridge release applies to.
// Only Linux owns a per-cluster bridge; macOS hands the subnet to vmnet, so
// there is nothing to take down and nothing to warn about. It is a variable so
// a test can exercise the Linux path from any host.
var hostBridgeGOOS = runtime.GOOS

// bridgeReleaseWarning phrases a failed teardown for the operator. A missing
// summary line reads exactly like a host that had nothing to remove, so the
// reason has to travel with the answer rather than only to tbxd.log (#445).
func bridgeReleaseWarning(subnetIndex int, cause error) string {
	return fmt.Sprintf(
		"the host bridge for subnet %s was not removed: %v; a leftover bridge keeps the gateway address, so the next create moves to another subnet",
		cluster.SubnetCIDR(subnetIndex), cause,
	)
}

// bridgeReleaseHelper is the slice of the host helper a bridge release uses:
// withdraw the subnet's resolver registration, then take the link down.
type bridgeReleaseHelper interface {
	UnregisterDNS(subnetIndex int) error
	TeardownSubnet(subnetIndex int) (bool, error)
	Close() error
}

// connectBridgeHelper is the release's helper connection, and the seam tests
// pin it through — the same shape as connectBGPHelper. It has to be a seam
// rather than a direct helper.Connect: the Linux client socket lives in
// /run/user/<uid>, not under $HOME, so the daemon tests' HOME isolation cannot
// contain it, and `go test ./internal/daemon` on a host with the helper
// installed would send a real dns.unregister and net.teardown for a live
// cluster's subnet (#445 follow-up). The package's TestMain pins it, so every
// test is hermetic by default and a test about the wiring installs its own
// recording client.
var connectBridgeHelper = func() (bridgeReleaseHelper, error) { return helper.Connect() }

// releaseSubnetBridge removes the host bridge a destroyed cluster's subnet
// leaves behind, and returns its name when there was one to remove. Without
// this the bridge keeps the subnet's gateway address, subnet allocation reads
// that address as a collision, and every create climbs to a fresh index
// (#445). Best-effort: the state is already gone, so a helper that cannot take
// the bridge down must not fail the destroy.
func releaseSubnetBridge(subnetIndex int) (string, error) {
	clusters, err := cluster.List()
	if err != nil {
		return "", fmt.Errorf("skip host bridge release: %w", err)
	}
	if subnetAllocated(clusters, subnetIndex) {
		return "", nil
	}
	client, err := connectBridgeHelper()
	if err != nil {
		return "", fmt.Errorf("skip host bridge release: %w", err)
	}
	defer func() { _ = client.Close() }()
	// Withdraw the resolver registration before the link it names disappears.
	// The DNS reconciler would withdraw it anyway, and tolerates an absent
	// link, but doing it in order keeps the host's resolver state ahead of the
	// teardown instead of behind it.
	if err := client.UnregisterDNS(subnetIndex); err != nil {
		log.Printf("withdraw DNS registration for subnet %s before host bridge release: %v", cluster.SubnetCIDR(subnetIndex), err)
	}
	removed, err := client.TeardownSubnet(subnetIndex)
	if err != nil {
		return "", err
	}
	if !removed {
		return "", nil
	}
	return cluster.LinuxBridgeName(subnetIndex), nil
}

// subnetAllocated reports whether any remaining cluster still owns a subnet
// index. Allocation gives an index to one cluster at a time, so this is only
// ever true for a destroy racing a create — checked rather than assumed,
// because the answer decides whether a live cluster's bridge comes down.
func subnetAllocated(clusters []cluster.Cluster, subnetIndex int) bool {
	for _, item := range clusters {
		if item.SubnetIndex == subnetIndex {
			return true
		}
	}
	return false
}

// directoryBytes sums what the files under dir occupy on disk. Node disks are
// sparse, so their apparent size would overstate the state being removed; the
// allocated block count is the closer number. It is a per-file sum, not a
// measure of capacity the destroy frees: APFS reports cloned extents at full
// size on both sides, so blocks a node disk shares with the image cache (which
// a destroy never touches) or with a snapshot in the same tree are counted once
// per file rather than once per physical block. Best effort: a file that cannot
// be stat'ed must not fail the verb that removes it.
func directoryBytes(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil //nolint:nilerr // an unreadable entry is skipped, not fatal
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return nil
		}
		total += imagecache.AllocatedSize(info)
		return nil
	})
	return total
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
		itemSummary := summary(item, s.clusterRunning(item.Name))
		if itemSummary.Suspended {
			_, backend, err := s.hypervisorForCluster(item)
			if err == nil {
				itemSummary.SuspendSurvivesDaemonRestart = backend.Capabilities().SuspendSurvivesDaemonRestart
			}
		}
		result = append(result, itemSummary)
	}
	return result, nil
}

func (s *Server) addNode(raw json.RawMessage) (NodeStatus, error) {
	status, _, err := s.addNodeLocked(raw, nil)
	return status, err
}

func (s *Server) addNodeLocked(raw json.RawMessage, progress stageFunc) (NodeStatus, []provisionTask, error) {
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
	running := s.clusterRunning(item.Name)
	incomingMiB := 0
	if running {
		incomingMiB = addMiB
	}
	hostPressureWarnings, err := s.checkHostPressure(dir, incomingMiB, args.Force)
	if err != nil {
		return NodeStatus{}, nil, err
	}
	preBalloonedMiB := 0
	if running {
		// A node added to a running cluster boots immediately, which is the
		// very allocation the projected-start gate exists for. A node added to
		// a stopped cluster starts nothing, so there is nothing to project
		// (#334).
		provisionStartWarnings, held, err := s.checkProvisionStart(dir, addMiB, args.Force)
		if err != nil {
			return NodeStatus{}, nil, err
		}
		preBalloonedMiB = held
		hostPressureWarnings = append(hostPressureWarnings, provisionStartWarnings...)
	}
	// As in createCluster: an add that fails before it launches must hand the
	// pre-balloon back rather than sit on it until the TTL runs out (#398).
	launched := false
	defer func() {
		if !launched {
			s.releaseBalloonHold(preBalloonedMiB)
		}
	}()
	var subnetWarning string
	if running {
		// The subnet is already fixed and attached, so it is only inspected for
		// advisory routing findings — never re-validated for collisions, which
		// the cluster's own bridge would always trip.
		subnetWarning, err = cluster.AttachedSubnetWarning(item.SubnetIndex, s.hostSubnetSources())
		if err != nil {
			return NodeStatus{}, nil, err
		}
	}
	// The image fetch is the long pole of an add against a cold cache, exactly
	// as it is for a create, and it runs before any other stage — narrating it
	// first is what re-arms the CLI's liveness deadline across it (#392).
	progress.stage("%s", talosImageStage(item.TalosVersion))
	cachedDisk, err := s.cachedDisk(item)
	if err != nil {
		return NodeStatus{}, nil, err
	}
	node, err := cluster.AddNode(&item, args.Role, args.Name)
	if err != nil {
		return NodeStatus{}, nil, err
	}
	progress.stage("cloning the disk for node %s", node.Name)
	if err := cluster.ProvisionDisks(item, cachedDisk); err != nil {
		_ = removeNodeFiles(item.Name, node.Name)
		return NodeStatus{}, nil, err
	}
	// The new node needs its reservation served before it attaches, exactly as
	// a cluster start does — and before the record is committed: a sync that
	// fails after the save would leave a node on disk the helper never heard
	// of, with nothing to roll it back.
	proposed, err := proposedClusterSet(item)
	if err != nil {
		_ = removeNodeFiles(item.Name, node.Name)
		return NodeStatus{}, nil, err
	}
	if err := SyncHelperClusters(proposed); err != nil {
		_ = removeNodeFiles(item.Name, node.Name)
		return NodeStatus{}, nil, err
	}
	if err := cluster.Save(item); err != nil {
		_ = removeNodeFiles(item.Name, node.Name)
		// The helper holds a reservation the record never took; hand it the
		// committed set again.
		logHelperSyncFailure("node add: revert helper reservations after a failed save")
		return NodeStatus{}, nil, err
	}
	if running {
		// Re-armed at the launch for the same reason create does: the hold's
		// TTL is a boot window, and the image fetch and disk clone above can
		// outlast it (#398).
		s.rearmBalloonHold(preBalloonedMiB)
		progress.stage("starting node %s", node.Name)
		machine, err := s.launchMachine(item, node, nil)
		if err != nil {
			// nothing booted; the deferred release hands the hold back
			return nodeStatus(node, item.SubnetIndex, false), nil, fmt.Errorf("node added but failed to create VM: %w", err)
		}
		launched = true
		s.vms[item.Name][node.Name] = machine
	}
	status := nodeStatus(node, item.SubnetIndex, s.nodeRunning(item.Name, node.Name))
	customSchematic := s.defaultSchematic != "" && item.Schematic != "" && item.Schematic != s.defaultSchematic
	tasks, deferredReconcile := s.beginNodeMutationProvisionLocked(item)
	var deferredWarning string
	if deferredReconcile {
		deferredWarning = nodeAddDeferredReconcileWarning(node.Name)
	}
	// The hint closes a verb that left work outstanding, so it only belongs
	// here when nothing else follows: a node.add that keeps its reconcile on
	// the request path blocks until the node is configured, and sending the
	// operator to `tbx status` mid-stream would promise an answer this very
	// call is still holding (#273).
	if running && !tasksReconcile(tasks) {
		progress.stage("%s", convergenceHint(item.Name))
	}
	status.setWarnings(append([]string{overcommitWarning}, append(hostPressureWarnings, subnetWarning, s.longhornCustomSchematicWarning(item, customSchematic), deferredWarning)...)...)
	return status, tasks, nil
}

// talosImageStage names the image-prepare stage. Stored state predating the
// recorded Talos version leaves the field empty, and a stage line with a hole
// in it reads worse than one that simply omits the version.
func talosImageStage(version string) string {
	if version == "" {
		return "preparing the Talos image"
	}
	return fmt.Sprintf("preparing the Talos %s image", version)
}

func (s *Server) removeNodeLocked(raw json.RawMessage, progress stageFunc) (NodeStatus, []provisionTask, error) {
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
		progress.stage("stopping node %s", node.Name)
		// an active VM is going down: no balloon retarget may land on it (#513)
		err := func() error {
			defer s.quiesceBalloon()()
			return closeMachine(machine)
		}()
		if err != nil {
			log.Printf("node.remove %s/%s: stop VM failed: %v", item.Name, node.Name, err)
			return NodeStatus{}, nil, fmt.Errorf("stop node %s: %w", node.Name, err)
		}
		delete(s.vms[item.Name], node.Name)
		log.Printf("node.remove %s/%s: VM stopped", item.Name, node.Name)
	}
	s.forgetNode(item.Name, node.Name)
	if err := cluster.Save(item); err != nil {
		return NodeStatus{}, nil, err
	}
	logHelperSyncFailure(fmt.Sprintf("sync helper state after removing %s/%s", item.Name, node.Name))
	progress.stage("deleting the disk for node %s", node.Name)
	if err := removeNodeFiles(item.Name, node.Name); err != nil {
		return NodeStatus{}, nil, err
	}
	tasks, deferredReconcile := s.beginNodeMutationProvisionLocked(item)
	status := nodeStatus(node, item.SubnetIndex, false)
	if deferredReconcile {
		status.setWarnings(nodeRemoveDeferredReconcileWarning(node.Name))
	}
	return status, tasks, nil
}

func (s *Server) handleNodeMutationLocked(request Request, progress stageFunc) (any, []provisionTask, error) {
	switch request.Op {
	case "node.add":
		result, tasks, err := s.addNodeLocked(request.Args, progress)
		if err != nil {
			return nil, nil, err
		}
		return result, tasks, nil
	case "node.remove":
		result, tasks, err := s.removeNodeLocked(request.Args, progress)
		if err != nil {
			return nil, nil, err
		}
		return result, tasks, nil
	case "node.start":
		result, tasks, err := s.startNodeLocked(request.Args, progress)
		if err != nil {
			return nil, nil, err
		}
		return result, tasks, nil
	case "node.stop":
		result, tasks, err := s.stopNodeLocked(request.Args, progress)
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
		running := s.clusterRunning(item.Name)
		clusterStatus := ClusterStatus{Name: item.Name, Subnet: cluster.SubnetCIDR(item.SubnetIndex), Domain: item.EffectiveDomain(), Hypervisor: s.hypervisorNameForCluster(item), AllowUnsafeDomain: item.AllowUnsafeDomain, TalosVersion: item.TalosVersion, Schematic: item.Schematic, BaseSchematic: item.BaseSchematic, TalosExtensions: item.TalosExtensions, ProvisioningIntent: item.ProvisioningIntent, BGP: item.BGP, Running: running,
			// derived from disk, not from daemon memory, so a restarted
			// daemon still reports its predecessor's suspension
			Suspended: !running && clusterHasSavedState(item.Name), SavedStateStale: !running && s.savedStateStale(item), Capabilities: s.clusterCapabilities(item), ConfigOrigin: item.ConfigOrigin, subnetIndex: item.SubnetIndex}
		for _, node := range item.Nodes {
			running := s.nodeRunning(item.Name, node.Name)
			clusterStatus.Nodes = append(clusterStatus.Nodes, NodeStatus{Name: node.Name, Role: node.Role, MAC: node.MAC, Phase: ClassifyPhase(running, ProbeResult{}), StartedAt: s.vmStartedAt(item.Name, node.Name),
				// per-node, not per-cluster: suspend saves memory only for
				// the nodes that were running, and the rest stay plain
				// stopped rather than inheriting the cluster's flag
				Suspended: !running && nodeHasSavedState(item.Name, node.Name)}.suspendedPhase())
		}
		clusterStatus.Hints = Hints(clusterStatus)
		result = append(result, clusterStatus)
	}
	return result, nil
}

// clusterCapabilities reports only the capabilities this cluster actually asked
// for, so a status listing stays silent about gates nobody depends on.
func (s *Server) clusterCapabilities(item cluster.Cluster) []CapabilityStatus {
	if !extensions.Requested(item.TalosExtensions, extensions.GuestAgent) {
		return nil
	}
	_, backend, err := s.hypervisorForCluster(item)
	if err != nil {
		return []CapabilityStatus{{Name: extensions.GuestAgent, Reason: err.Error()}}
	}
	guestAgent := backend.Capabilities().GuestAgent
	return []CapabilityStatus{{
		Name:      extensions.GuestAgent,
		Supported: guestAgent.Supported,
		Reason:    guestAgent.Reason,
	}}
}

func (s *Server) refreshNodeStatuses(statuses []ClusterStatus) {
	s.refreshNodeStatusesNarrated(statuses, nil)
}

func (s *Server) refreshNodeStatusesNarrated(statuses []ClusterStatus, progress stageFunc) {
	lookupIP := s.nodeIPLookup
	if lookupIP == nil {
		lookupIP = cluster.LookupIP
	}
	probe := s.nodeProbe
	if probe == nil {
		probe = probeAPID
	}
	now := time.Now()
	for i := range statuses {
		for j, snapshot := range statuses[i].Nodes {
			node := cluster.Node{Name: snapshot.Name, Role: snapshot.Role, MAC: snapshot.MAC}
			progress.stage("probing node %s/%s", statuses[i].Name, node.Name)
			refreshed := nodeStatusWith(node, statuses[i].subnetIndex, !snapshot.Phase.Stopped(), lookupIP, probe)
			refreshed.StartedAt = snapshot.StartedAt
			// Suspension is a disk fact the status handler already
			// established; the refresh only re-derives the live phase, and
			// dropping the flag here is what made a suspended cluster read as
			// plain stopped in the PHASE column (#360).
			refreshed.Suspended = snapshot.Suspended
			refreshed = refreshed.suspendedPhase()
			refreshed.UnreachableSince = s.reachability.observe(nodeKey(statuses[i].Name, node.Name), refreshed.Phase, now)
			statuses[i].Nodes[j] = refreshed
		}
	}
	s.refreshNodeDetails(statuses, now)
	for i := range statuses {
		statuses[i].Hints = hintsAt(statuses[i], now)
	}
	s.logNodeStalls(statuses, now)
}

func (s *Server) refreshNodeDetails(statuses []ClusterStatus, now time.Time) {
	var wait sync.WaitGroup
	for i := range statuses {
		status := &statuses[i]
		for j := range status.Nodes {
			node := &status.Nodes[j]
			if !node.Phase.Configured() || node.IP == "" {
				continue
			}
			wait.Add(1)
			go func(clusterName string, running bool, node *NodeStatus) {
				defer wait.Done()

				bootProbe := s.reboots.beginObserve(nodeKey(clusterName, node.Name))
				type bootResult struct {
					bootTime uint64
					err      error
				}
				bootDone := make(chan bootResult, 1)
				go func() {
					bootTime, err := probeNodeBootTime(clusterName, node.IP)
					bootDone <- bootResult{bootTime: bootTime, err: err}
				}()

				var (
					services []NodeService
					probe    ServiceProbe
				)
				if running {
					services, probe = probeNodeServices(clusterName, node.IP, now)
				}

				result := <-bootDone
				s.applyNodeReboot(clusterName, node, bootProbe, result.bootTime, result.err, now)
				if running {
					applyNodeServices(node, services, probe, now)
				}
			}(status.Name, status.Running, node)
		}
	}
	wait.Wait()
}

// refreshNodeServices asks each configured node's machine API about its
// kubelet, so a node that answers apid while its kubelet cannot exec stops
// reading as a healthy `configured` node (#357). Only a running cluster's
// configured nodes are asked — the periodic stall sweep builds statuses with
// Running unset, so it stays probe-free — and the probes run concurrently, so
// one silent node cannot add its timeout to every other node's.
func refreshNodeServices(status *ClusterStatus, now time.Time) {
	if !status.Running {
		return
	}
	var wait sync.WaitGroup
	for i := range status.Nodes {
		node := &status.Nodes[i]
		if !node.Phase.Configured() || node.IP == "" {
			continue
		}
		wait.Add(1)
		go func() {
			defer wait.Done()
			services, probe := probeNodeServices(status.Name, node.IP, now)
			applyNodeServices(node, services, probe, now)
		}()
	}
	wait.Wait()
}

func (s *Server) applyNodeReboot(clusterName string, node *NodeStatus, probe rebootProbe, bootTime uint64, err error, now time.Time) {
	key := nodeKey(clusterName, node.Name)
	node.RebootedAt = nil
	if node.Phase == PhaseRebooted {
		node.Phase = PhaseConfigured
	}
	if err == nil && bootTime != 0 {
		observation, previous, changed, applied := s.reboots.completeObserve(probe, bootTime, now)
		if applied && changed {
			log.Printf("status %s: node %s rebooted without a host VM restart (Talos boot_time changed %d -> %d); observed at %s",
				clusterName, node.Name, previous, observation.BootTime, observation.RebootedAt.UTC().Format(time.RFC3339))
		}
	}
	if active, ok := s.reboots.current(key, now); ok {
		rebootedAt := active.RebootedAt
		node.RebootedAt = &rebootedAt
		node.Phase = PhaseRebooted
	}
}

func applyNodeServices(node *NodeStatus, services []NodeService, probe ServiceProbe, now time.Time) {
	node.ServiceProbe = &probe
	sort.Slice(services, func(i, j int) bool { return services[i].Name < services[j].Name })
	node.Services = services
	for i := range node.Services {
		if node.StartedAt != nil && node.Services[i].Since != nil && node.Services[i].Since.Before(*node.StartedAt) {
			started := *node.StartedAt
			node.Services[i].Since = &started
		}
		if node.Services[i].Name == kubeletService {
			service := node.Services[i]
			node.Kubelet = &service
		}
	}
	node.StalledServices = stalledServices(node.Services, node.StartedAt, now)
}

const serviceStallThreshold = 3 * time.Minute

func stalledServices(services []NodeService, startedAt *time.Time, now time.Time) []StalledService {
	var stalled []StalledService
	for _, service := range services {
		if !strings.EqualFold(service.State, "Preparing") && !strings.EqualFold(service.State, "Starting") {
			continue
		}
		if service.Since == nil || service.Since.After(now) {
			continue
		}
		since := *service.Since
		if startedAt != nil && since.Before(*startedAt) {
			since = *startedAt
		}
		if now.Sub(since) <= serviceStallThreshold {
			continue
		}
		stalled = append(stalled, StalledService{Service: service.Name, State: service.State, Since: since})
	}
	sort.Slice(stalled, func(i, j int) bool {
		if stalled[i].Since.Equal(stalled[j].Since) {
			return stalled[i].Service < stalled[j].Service
		}
		return stalled[i].Since.Before(stalled[j].Since)
	})
	return stalled
}

func refreshKubernetesReadiness(statuses []ClusterStatus) {
	refreshKubernetesReadinessNarrated(statuses, nil)
}

func refreshKubernetesReadinessNarrated(statuses []ClusterStatus, progress stageFunc) {
	for index := range statuses {
		status := &statuses[index]
		if status.CNI != cluster.CNIFlannel && status.CNI != cluster.CNICilium {
			continue
		}
		progress.stage("checking Kubernetes readiness for %s", status.Name)
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

// allNodesRunning reports whether every member of the cluster has a running
// VM, which is what a reconcile needs: its request lists all members and its
// readiness gates wait on each of them.
func (s *Server) allNodesRunning(item cluster.Cluster) bool {
	return clusterReady(item, func(nodeName string) bool {
		return s.nodeRunning(item.Name, nodeName)
	})
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
	var args CachePullArgs
	if err := decodeArgs(raw, &args); err != nil {
		return CachePullResult{}, err
	}
	if args.Version == "" {
		args.Version = args.TalosVersion
	}
	combinations := args.Combinations
	if len(combinations) == 0 {
		combinations = []CachePullCombination{{Schematic: args.Schematic, Version: args.Version}}
	}
	_, backend, err := s.hypervisors.ResolveDefault()
	if err != nil {
		return CachePullResult{}, err
	}
	architecture := backend.Architecture()
	var result CachePullResult
	// Distinct clusters routinely share a pin, and re-composition resolves
	// spellings apart: dedupe on the resolved combination so each image is
	// downloaded once.
	fetched := make(map[CachePullImage]struct{}, len(combinations))
	pulledFor := make([]cluster.Cluster, 0, len(combinations))
	for _, combination := range combinations {
		// Resolution composes while online, which is what makes the same
		// combination answerable from cache afterwards.
		schematic, talosVersion, err := s.resolveImage(combination.Schematic, combination.Version, combination.Extensions)
		if err != nil {
			return CachePullResult{}, err
		}
		// Clusters sharing a disk image still install their own provisioning
		// path, so every combination contributes its images even when the
		// download collapses into one.
		pulledFor = append(pulledFor, cluster.Cluster{
			Schematic:          schematic,
			TalosVersion:       talosVersion,
			ProvisioningIntent: combination.Intent,
		})
		image := CachePullImage{Schematic: schematic, Version: talosVersion, Architecture: architecture}
		if _, duplicate := fetched[image]; duplicate {
			continue
		}
		fetched[image] = struct{}{}
		image.Path, err = s.cache.Ensure(schematic, talosVersion, imagecache.Architecture(architecture))
		if err != nil {
			return CachePullResult{}, err
		}
		// An explicit pull is the statement that this combination is
		// wanted; the pin is what keeps prune off it.
		if err := s.cache.Pin(schematic, talosVersion, imagecache.Architecture(architecture)); err != nil {
			return CachePullResult{}, err
		}
		result.Images = append(result.Images, image)
	}
	if len(result.Images) > 0 {
		first := result.Images[0]
		result.Schematic, result.Version = first.Schematic, first.Version
		result.Architecture, result.Path = first.Architecture, first.Path
	}
	if !args.FromFile {
		return result, nil
	}
	if !args.SkipImages {
		warm, err := s.warmClusterImages(pulledFor)
		if err != nil {
			return CachePullResult{}, err
		}
		result.Warm = warm
	}
	// Reporting only: a combination that lost its cluster and its place in the
	// file is still someone's deliberate pin until they prune it.
	strays, err := s.strayPinnedImages(result.Images)
	if err != nil {
		return CachePullResult{}, err
	}
	result.Strays = strays
	return result, nil
}

// warmClusterImages replays every image the pulled clusters will pull into the
// mirror cache, so the same file comes up offline afterwards.
func (s *Server) warmClusterImages(items []cluster.Cluster) (*CacheWarmResult, error) {
	if s.warmCache == nil {
		return nil, nil
	}
	seen := map[string]struct{}{}
	var refs []string
	for _, item := range items {
		images, err := provision.ClusterImages(item)
		if err != nil {
			return nil, err
		}
		for _, ref := range images {
			if _, duplicate := seen[ref]; duplicate {
				continue
			}
			seen[ref] = struct{}{}
			refs = append(refs, ref)
		}
	}
	if len(refs) == 0 {
		return nil, nil
	}
	slices.Sort(refs)
	for _, ref := range refs {
		if err := ValidateWarmRef(ref); err != nil {
			return nil, err
		}
	}
	ctx, cancel := s.lifecycleTimeoutContext(cacheWarmTimeout)
	defer cancel()
	architecture, err := s.imageArchitecture()
	if err != nil {
		return nil, err
	}
	result, err := s.warmCache(ctx, refs, architecture)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// strayPinnedImages names every pinned combination that this pull did not ask
// for and no cluster references.
func (s *Server) strayPinnedImages(pulled []CachePullImage) ([]CacheImageEntry, error) {
	classifier, err := s.cacheImageClassifier()
	if err != nil {
		return nil, err
	}
	requested := make(map[imagecache.Combination]struct{}, len(pulled))
	for _, image := range pulled {
		requested[imagecache.Combination{
			Schematic:    image.Schematic,
			Version:      image.Version,
			Architecture: imagecache.Architecture(image.Architecture),
		}] = struct{}{}
	}
	entries, err := s.cache.List()
	if err != nil {
		return nil, err
	}
	var strays []CacheImageEntry
	for _, entry := range entries {
		// List also reports combinations that hold only prunable leftovers;
		// they are not usable pinned images and were never strays here.
		if entry.Incomplete {
			continue
		}
		combination := imagecache.Combination{
			Schematic:    entry.Schematic,
			Version:      entry.Version,
			Architecture: entry.Architecture,
		}
		if _, wanted := requested[combination]; wanted {
			continue
		}
		// The built-in default combination is spared by prune and shown as
		// `default` by list even when it also carries a pin, so it is never a
		// stray — status() would report it as pinned because a pin outranks the
		// default, which is the wrong answer here.
		if classifier.hasDefault && combination == classifier.defaultCombination {
			continue
		}
		status, _, err := classifier.status(combination)
		if err != nil {
			return nil, err
		}
		if status != CacheImageStatusPinned {
			continue
		}
		strays = append(strays, CacheImageEntry{
			Schematic:    entry.Schematic,
			Version:      entry.Version,
			Architecture: string(entry.Architecture),
			Size:         entry.Size,
			Status:       status,
		})
	}
	return strays, nil
}

func (s *Server) warmMirrorCache(raw json.RawMessage, progress stageFunc) (CacheWarmResult, error) {
	var args CacheWarmArgs
	if err := decodeArgs(raw, &args); err != nil {
		return CacheWarmResult{}, err
	}
	if len(args.Refs) == 0 {
		return CacheWarmResult{}, errors.New("at least one image reference is required")
	}
	if args.Jobs < 0 || args.Jobs > MaxCacheWarmJobs {
		return CacheWarmResult{}, fmt.Errorf("jobs must be between 0 and %d, got %d", MaxCacheWarmJobs, args.Jobs)
	}
	for _, ref := range args.Refs {
		if err := ValidateWarmRef(ref); err != nil {
			return CacheWarmResult{}, err
		}
	}
	if s.warmCache == nil && s.warmCacheWithOptions == nil {
		return CacheWarmResult{}, errors.New("cache warm is not configured")
	}
	// the budget is per ref: the CLI used to send one request per ref and
	// now sends the list, and a long list on a slow link must not lose at the
	// end what each ref alone would have been allowed (#506)
	ctx, cancel := s.lifecycleTimeoutContext(cacheWarmTimeout * time.Duration(len(args.Refs)))
	defer cancel()
	if s.warmCacheWithOptions != nil {
		options := mirror.WarmOptions{Refresh: args.Refresh, Jobs: args.Jobs}
		if progress != nil {
			options.OnResult = func(result mirror.WarmResult) {
				encoded, err := json.Marshal(cacheWarmEntry(result))
				if err != nil {
					return
				}
				progress.stage("%s%s", CacheWarmEntryStagePrefix, encoded)
			}
		}
		architecture, err := s.imageArchitecture()
		if err != nil {
			return CacheWarmResult{}, err
		}
		summary, err := s.warmCacheWithOptions(ctx, args.Refs, architecture, options)
		return cacheWarmResult(summary), err
	}
	architecture, err := s.imageArchitecture()
	if err != nil {
		return CacheWarmResult{}, err
	}
	return s.warmCache(ctx, args.Refs, architecture)
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
	architecture, err := s.imageArchitecture()
	if err != nil {
		return CacheCheckResult{}, err
	}
	return s.checkCache(ctx, args.Refs, architecture, args.Deep)
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
	classifier, err := s.cacheImageClassifier()
	if err != nil {
		return CacheListResult{}, err
	}
	for _, entry := range entries {
		reasons, clusters, err := classifier.statuses(imagecache.Combination{
			Schematic:    entry.Schematic,
			Version:      entry.Version,
			Architecture: entry.Architecture,
		})
		if err != nil {
			return CacheListResult{}, err
		}
		result.Images = append(result.Images, CacheImageEntry{
			Schematic:     entry.Schematic,
			Version:       entry.Version,
			Architecture:  string(entry.Architecture),
			Size:          entry.Size,
			AllocatedSize: entry.AllocatedSize,
			Status:        primaryCacheImageStatus(reasons),
			Reasons:       reasons,
			Clusters:      clusters,
			Incomplete:    entry.Incomplete,
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
		// The default scope is reference-aware: only combinations no
		// cluster, no pin, and not the default combination claims go.
		classifier, err := s.cacheImageClassifier()
		if err != nil {
			return CachePruneResult{}, err
		}
		result, err := s.cache.PruneDiskExcept(classifier.keep)
		if err != nil {
			return CachePruneResult{}, err
		}
		return CachePruneResult{
			Scope:      args.Scope,
			ImageCount: result.ImageCount,
			ImageBytes: result.ImageBytes,
			KeptCount:  result.KeptImages,
			Images:     prunedImageEntries(result.Images),
			Mirror:     MirrorCacheTotals(result.Mirror),
		}, nil
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

func prunedImageEntries(removed []imagecache.PrunedCombination) []CacheImageEntry {
	entries := make([]CacheImageEntry, 0, len(removed))
	for _, combination := range removed {
		entries = append(entries, CacheImageEntry{
			Schematic:    combination.Schematic,
			Version:      combination.Version,
			Architecture: string(combination.Architecture),
			Size:         combination.Bytes,
			Status:       CacheImageStatusOrphan,
		})
	}
	return entries
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
			// The recorded default id is what an offline daemon
			// resolves from, so memoizing is only a shortcut.
			s.defaultSchematic, err = s.cache.DefaultSchematic()
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

func (s *Server) imageArchitecture() (imagecache.Architecture, error) {
	_, backend, err := s.hypervisors.ResolveDefault()
	if err != nil {
		return "", err
	}
	return imagecache.Architecture(backend.Architecture()), nil
}

func (s *Server) clusterImageArchitecture(item cluster.Cluster) (imagecache.Architecture, error) {
	architecture := item.ImageArchitecture
	if architecture == "" {
		architecture = cluster.LegacyImageArchitecture
	}
	_, backend, err := s.hypervisorForCluster(item)
	if err != nil {
		return "", err
	}
	active := backend.Architecture()
	if hypervisor.Architecture(architecture) != active {
		return "", fmt.Errorf("cluster %q uses %s images, but the active hypervisor targets %s", item.Name, architecture, active)
	}
	return imagecache.Architecture(architecture), nil
}

// roleMemoryMiB resolves the memory one node of a role will be created with:
// the per-role `controlPlane:`/`worker:` override when the spec carries one,
// otherwise the cluster-wide node defaults. It is the pre-creation twin of
// cluster.Cluster.DefaultsFor, which every other memory gate resolves through.
func roleMemoryMiB(role *cluster.NodeDefaults, base cluster.NodeDefaults) int {
	baseMiB := memoryOr(base.MemoryMiB, cluster.DefaultMemoryMiB)
	if role != nil {
		return memoryOr(role.MemoryMiB, baseMiB)
	}
	return baseMiB
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
		// derived from disk, not from daemon memory, so a restarted daemon
		// still reports the suspension its predecessor performed
		Suspended: !running && clusterHasSavedState(item.Name),
	}
}

// warningList drops empties and duplicates while keeping each warning a
// separate item, so the CLI can render one per line instead of scanning a
// semicolon-joined run-on (#291).
func warningList(warnings ...string) []string {
	var result []string
	seen := make(map[string]bool, len(warnings))
	for _, warning := range warnings {
		if warning == "" || seen[warning] {
			continue
		}
		seen[warning] = true
		result = append(result, warning)
	}
	return result
}

// setWarnings fills both the list and the legacy joined string, so a new
// daemon still speaks to an old CLI that only reads Warning.
func (s *ClusterSummary) setWarnings(warnings ...string) {
	s.Warnings = warningList(warnings...)
	s.Warning = strings.Join(s.Warnings, "; ")
}

// addWarnings records findings the operation only learned after its summary
// was built — the out-of-lock boot wait — behind the ones already there.
func (s *ClusterSummary) addWarnings(warnings ...string) {
	s.setWarnings(append(append([]string{}, s.Warnings...), warnings...)...)
}

// setWarnings mirrors ClusterSummary.setWarnings for a node verb: `node add`
// and `node remove` collect unrelated findings (overcommit, host pressure, a
// captured host subnet, a volume-copy loss) that must not fuse onto one line
// (#291).
func (n *NodeStatus) setWarnings(warnings ...string) {
	n.Warnings = warningList(warnings...)
	n.Warning = strings.Join(n.Warnings, "; ")
}

// addWarnings records findings the operation only learned after its status was
// built — the out-of-lock gates — behind the ones already there.
func (n *NodeStatus) addWarnings(warnings ...string) {
	n.setWarnings(append(append([]string{}, n.Warnings...), warnings...)...)
}
