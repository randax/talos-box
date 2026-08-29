package daemon

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/shellquote"
)

// Phase is a node's observed lifecycle state, derived without Talos credentials:
// both apid modes speak TLS, but maintenance mode presents the well-known
// "maintenance-service.talos.dev" certificate (verified empirically; a
// configured node presents a cluster-CA cert with the node's identity and
// additionally demands a client certificate).
type Phase string

const (
	PhaseStopped     Phase = "stopped"
	PhaseUnreachable Phase = "unreachable"
	PhaseMaintenance Phase = "maintenance"
	PhaseConfigured  Phase = "configured"
	PhaseRebooted    Phase = "rebooted"
	// PhaseSuspended is a stopped node whose own memory is saved on disk. It
	// is not a probe verdict — no VM is running to probe — but a promotion of
	// PhaseStopped applied where suspension is known, so the JSON surface
	// reports what the table has shown since #360 instead of the coarser
	// "stopped" a consumer keying on phase alone misread (#415).
	PhaseSuspended Phase = "suspended"
)

// Stopped reports a phase with no running VM behind it: plain stopped, or
// stopped with saved memory. Every rule that keyed on PhaseStopped means this,
// because the suspended promotion changed the spelling and not the fact.
func (p Phase) Stopped() bool { return p == PhaseStopped || p == PhaseSuspended }

// Configured reports phases backed by an authenticated configured Talos node.
// Rebooted is a transient observation layered on that same apid phase.
func (p Phase) Configured() bool { return p == PhaseConfigured || p == PhaseRebooted }

// ProbeResult is what one apid probe observed.
type ProbeResult struct {
	Dialed          bool // TCP connection to :50000 succeeded
	TLS             bool // TLS handshake completed (server presented a certificate)
	MaintenanceCert bool // the presented certificate is the maintenance-service identity
}

// maintenanceCN is the CommonName Talos maintenance mode presents.
const maintenanceCN = "maintenance-service.talos.dev"

// ClassifyPhase turns VM state plus a probe observation into a Phase.
func ClassifyPhase(vmRunning bool, probe ProbeResult) Phase {
	switch {
	case !vmRunning:
		return PhaseStopped
	case !probe.Dialed, !probe.TLS:
		return PhaseUnreachable
	case probe.MaintenanceCert:
		return PhaseMaintenance
	default:
		return PhaseConfigured
	}
}

// apidPort is Talos's machine API port.
const apidPort = "50000"

// probeTimeout bounds one dial. Two of them make up a probe, so a sweep over a
// large cluster's silent nodes costs this twice per node — which is why a
// caller on a deadline probes with a context instead.
const probeTimeout = time.Second

// probeAPID observes a node's apid: reachable? speaking TLS?
func probeAPID(ip string) ProbeResult {
	return probeAPIDContext(context.Background(), ip)
}

// probeAPIDContext is probeAPID bounded by the caller's context as well as by
// its own dial timeouts, so a caller sweeping many nodes under a deadline
// cannot overrun it by the length of one more dial.
func probeAPIDContext(ctx context.Context, ip string) ProbeResult {
	return probeHostPort(ctx, net.JoinHostPort(ip, apidPort))
}

func probeHostPort(ctx context.Context, address string) ProbeResult {
	dialer := &net.Dialer{Timeout: probeTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return ProbeResult{}
	}
	_ = conn.Close()
	tlsDialer := &tls.Dialer{
		NetDialer: dialer,
		Config:    &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // probing our own local VM
	}
	tlsConn, err := tlsDialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return ProbeResult{Dialed: true, TLS: false}
	}
	defer func() { _ = tlsConn.Close() }()
	// tls.Dialer hands back a net.Conn; the handshake state this probe reads
	// the identity from lives on the concrete connection.
	state, ok := tlsConn.(*tls.Conn)
	if !ok {
		return ProbeResult{Dialed: true, TLS: true}
	}
	certs := state.ConnectionState().PeerCertificates
	maintenance := len(certs) > 0 && certs[0].Subject.CommonName == maintenanceCN
	return ProbeResult{Dialed: true, TLS: true, MaintenanceCert: maintenance}
}

// kubeletService is the Talos service status reports on: a node answering apid
// can still be dead to Kubernetes, and kubelet is where that shows.
const kubeletService = "kubelet"

// serviceProbeTimeout bounds one machine-API service query, dial included.
const serviceProbeTimeout = 3 * time.Second

// probeNodeServices observes every Talos service on a configured node. It is a
// package var because the live implementation dials a node's machine API,
// which no hermetic test may do; tests replace it wholesale.
var probeNodeServices = probeNodeServicesLive

// lookupNodeTalosContext is also injectable because its default-path search
// reads user state outside the cluster directory. The package TestMain pins it
// even though the fully stubbed service probe normally prevents reaching it.
var lookupNodeTalosContext = lookupNodeTalosContextLive

// listNodeServices is the authenticated SDK boundary. Keeping context
// selection and endpoint selection visible in its arguments lets tests prove
// the exact context's credentials are paired with the observed lease.
var listNodeServices = listNodeServicesLive

var errTalosContextMissing = errors.New("talos context missing")

// lookupNodeTalosContextLive selects credentials without ever honoring a
// talosconfig's current context. A merged config may point current at an
// unrelated cluster, and talosctl may rename conflicts to <name>-1; neither is
// authority to inspect this cluster.
func lookupNodeTalosContextLive(clusterName string) (string, *clientconfig.Context, error) {
	dir, err := cluster.Dir(clusterName)
	if err != nil {
		return "", nil, err
	}
	local := filepath.Join(dir, "talosconfig")
	if info, statErr := os.Stat(local); statErr == nil {
		if !info.Mode().IsRegular() {
			return "", nil, fmt.Errorf("read cluster talosconfig %s: not a regular file", local)
		}
		return contextFromTalosconfig(local, clusterName)
	} else if !os.IsNotExist(statErr) {
		return "", nil, fmt.Errorf("inspect cluster talosconfig %s: %w", local, statErr)
	}
	paths, err := clientconfig.GetDefaultPaths()
	if err != nil {
		return "", nil, fmt.Errorf("find default talosconfig: %w", err)
	}
	// A default path that exists but lacks the exact context is an expected
	// per-candidate outcome, not the end of the search: TALOSCONFIG may name
	// a file merged for another cluster while ~/.talos/config holds this one.
	// Unreadable, non-regular, or malformed candidates still fail outright —
	// a broken file is not permission to fall through to unrelated credentials.
	var searched []string
	for _, candidate := range paths {
		info, statErr := os.Stat(candidate.Path)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil {
			return "", nil, fmt.Errorf("inspect talosconfig %s: %w", candidate.Path, statErr)
		}
		if !info.Mode().IsRegular() {
			return "", nil, fmt.Errorf("read talosconfig %s: not a regular file", candidate.Path)
		}
		source, ctx, err := contextFromTalosconfig(candidate.Path, clusterName)
		if err == nil {
			return source, ctx, nil
		}
		if !errors.Is(err, errTalosContextMissing) {
			return "", nil, err
		}
		searched = append(searched, candidate.Path)
	}
	if len(searched) > 0 {
		return "", nil, fmt.Errorf("%w: exact context %q was not found in %s", errTalosContextMissing, clusterName, strings.Join(searched, ", "))
	}
	return "", nil, fmt.Errorf("%w: exact context %q was not found", errTalosContextMissing, clusterName)
}

func contextFromTalosconfig(path, clusterName string) (string, *clientconfig.Context, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil, fmt.Errorf("read talosconfig %s: %w", path, err)
	}
	config, err := clientconfig.FromBytes(data)
	if err != nil {
		return "", nil, fmt.Errorf("parse talosconfig %s: %w", path, err)
	}
	ctx := config.Contexts[clusterName]
	if ctx == nil {
		return "", nil, fmt.Errorf("%w: exact context %q was not found in %s", errTalosContextMissing, clusterName, path)
	}
	return path, ctx, nil
}

// probeNodeServicesLive asks the machine API once and classifies the returned
// list. Missing credentials are distinct from a real authenticated probe that
// failed, because only the former has an actionable merge instruction.
func probeNodeServicesLive(clusterName, ip string, now time.Time) ([]NodeService, ServiceProbe) {
	source, configContext, err := lookupNodeTalosContext(clusterName)
	if err != nil {
		status := ServiceProbeFailed
		if errors.Is(err, errTalosContextMissing) {
			status = ServiceProbeMissingCredentials
		}
		return nil, ServiceProbe{Status: status, Error: err.Error()}
	}
	ctx, cancel := context.WithTimeout(context.Background(), serviceProbeTimeout)
	defer cancel()
	response, err := listNodeServices(ctx, configContext, ip)
	if err != nil {
		return nil, ServiceProbe{Status: ServiceProbeFailed, Source: source, Error: err.Error()}
	}
	var services []NodeService
	for _, message := range response.GetMessages() {
		for _, info := range message.GetServices() {
			services = append(services, classifyService(info.GetId(), observeServiceAt(info, now)))
		}
	}
	sort.Slice(services, func(i, j int) bool { return services[i].Name < services[j].Name })
	return services, ServiceProbe{Status: ServiceProbeSucceeded, Source: source}
}

func listNodeServicesLive(ctx context.Context, configContext *clientconfig.Context, ip string) (*machineapi.ServiceListResponse, error) {
	// The observed endpoint wins over the talosconfig's generated list for the
	// same reason the provisioning path prefers it: a node can hold a different
	// lease than the one its config was written from.
	connection, err := talosclient.New(ctx,
		talosclient.WithConfigContext(configContext),
		talosclient.WithDefaultGRPCDialOptions(),
		talosclient.WithEndpoints(ip),
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = connection.Close() }()
	response, err := connection.ServiceList(talosclient.WithNode(ctx, ip))
	if err != nil {
		return nil, err
	}
	return response, nil
}

// observeService reduces the machine API's service report to the facts the
// classification rules read. A failure event's message is preferred over the
// health verdict's: a kubelet that cannot exec never gets far enough to
// publish a health message, and the exec error is the diagnosis.
func observeServiceAt(info *machineapi.ServiceInfo, now time.Time) ServiceObservation {
	if info == nil {
		return ServiceObservation{}
	}
	observation := ServiceObservation{State: info.GetState(), HealthUnknown: true}
	if health := info.GetHealth(); health != nil {
		observation.Healthy = health.GetHealthy()
		observation.HealthUnknown = health.GetUnknown()
		observation.Message = health.GetLastMessage()
	}
	failureMessage := ""
	for _, event := range info.GetEvents().GetEvents() {
		if strings.EqualFold(event.GetState(), info.GetState()) {
			if timestamp := event.GetTs(); timestamp != nil && timestamp.CheckValid() == nil {
				value := timestamp.AsTime()
				if !value.After(now) && (observation.Since == nil || value.After(*observation.Since)) {
					observation.Since = &value
				}
			}
		}
		if !strings.EqualFold(event.GetState(), serviceStateFailed) {
			continue
		}
		observation.Failures++
		failureMessage = event.GetMsg()
	}
	if failureMessage != "" {
		observation.Message = failureMessage
	}
	return observation
}

// ServiceHealth is a Talos service's condition in status's own vocabulary. The
// machine API reports a state string plus an optional health verdict; the
// per-node surface needs one word for it, because the phase is derived from
// apid alone and a node whose kubelet cannot even exec answers apid perfectly
// well (#357).
type ServiceHealth string

const (
	ServiceHealthUnknown      ServiceHealth = "unknown"
	ServiceHealthStarting     ServiceHealth = "starting"
	ServiceHealthHealthy      ServiceHealth = "healthy"
	ServiceHealthUnhealthy    ServiceHealth = "unhealthy"
	ServiceHealthCrashLooping ServiceHealth = "crashlooping"
)

// NodeService is one Talos service on one node, as status reports it.
type NodeService struct {
	Name string `json:"name"`
	// State is Talos's own service state string (Running, Failed, Preparing…),
	// kept verbatim beside the derived verdict.
	State  string        `json:"state,omitempty"`
	Health ServiceHealth `json:"health"`
	// Message is the service's last failure or health message — the line that
	// says what is actually wrong ("exec /usr/local/bin/kubelet: input/output
	// error"), which is the whole point of surfacing this.
	Message string `json:"message,omitempty"`
	// Restarts counts the failure events the node still retains for the
	// service: a service Talos restarts forever accumulates them.
	Restarts int `json:"restarts,omitempty"`
	// Since is the newest retained transition into the current state. Missing
	// means Talos supplied no trustworthy clock, never that the state began at
	// probe time.
	Since *time.Time `json:"since,omitempty"`
}

// CrashLooping reports a service Talos keeps restarting without it ever
// becoming healthy — the state no rerun of tbx up can clear.
func (s NodeService) CrashLooping() bool { return s.Health == ServiceHealthCrashLooping }

// Degraded reports a service that is observably not doing its job, as opposed
// to one that is still coming up or has no verdict yet.
func (s NodeService) Degraded() bool {
	return s.Health == ServiceHealthCrashLooping || s.Health == ServiceHealthUnhealthy
}

// ServiceObservation is the raw shape a service probe reads off the machine
// API, kept apart from the classified NodeService so the rules stay testable
// without a live node.
type ServiceObservation struct {
	State         string
	Healthy       bool
	HealthUnknown bool
	Message       string
	Failures      int
	Since         *time.Time
}

const (
	serviceStateRunning = "Running"
	serviceStateFailed  = "Failed"
	// crashLoopFailures is how many retained failure events make a loop: one
	// is a restart, two in the window Talos keeps is a service that is not
	// coming up.
	crashLoopFailures = 2
	// serviceMessageMaxLen keeps one node's message from taking over the hint
	// or the table note it lands in.
	serviceMessageMaxLen = 160
)

// classifyService turns one observation into the verdict status renders. A
// service that is running and reports itself healthy is healthy however many
// failures it accumulated on the way there; anything else reads off the state
// first, because a failing service's health verdict is usually just unknown.
func classifyService(name string, observation ServiceObservation) NodeService {
	service := NodeService{
		Name:     name,
		State:    observation.State,
		Message:  truncateServiceMessage(strings.TrimSpace(observation.Message)),
		Restarts: observation.Failures,
		Since:    observation.Since,
	}
	running := strings.EqualFold(observation.State, serviceStateRunning)
	switch {
	case observation.State == "":
		service.Health = ServiceHealthUnknown
	case running && !observation.HealthUnknown && observation.Healthy:
		service.Health = ServiceHealthHealthy
	case strings.EqualFold(observation.State, serviceStateFailed), observation.Failures >= crashLoopFailures:
		service.Health = ServiceHealthCrashLooping
	case !running:
		service.Health = ServiceHealthStarting
	case observation.HealthUnknown:
		service.Health = ServiceHealthUnknown
	default:
		service.Health = ServiceHealthUnhealthy
	}
	return service
}

func truncateServiceMessage(message string) string {
	if len(message) <= serviceMessageMaxLen {
		return message
	}
	return message[:serviceMessageMaxLen] + "…"
}

// nodeBootWindow is the boot budget the calm unreachable hint promises.
const nodeBootWindow = time.Minute

// formatBootWindow renders the promised boot budget for the hint that states
// it, so the prose and the constant it describes cannot desync.
func formatBootWindow(window time.Duration) string {
	if window >= time.Minute && window%time.Minute == 0 {
		minutes := int(window / time.Minute)
		if minutes == 1 {
			return "1 minute"
		}
		return fmt.Sprintf("%d minutes", minutes)
	}
	return fmt.Sprintf("%d seconds", int(window.Round(time.Second).Seconds()))
}

// nodeStallThreshold is how far past that promise a node must stay silent
// before the hint stops counselling patience and starts naming the node: a
// node still unreachable at 3× the stated window is not booting slowly, it is
// stuck, and the operator needs evidence instead of reassurance (#288).
const nodeStallThreshold = 3 * nodeBootWindow

func anyHealthyKubelet(nodes []NodeStatus) bool {
	for _, node := range nodes {
		if node.Kubelet != nil && node.Kubelet.Health == ServiceHealthHealthy {
			return true
		}
	}
	return false
}

// Hints returns copy-pasteable next steps for a cluster, keyed on its nodes'
// phases. Hints describe; they never execute (SPEC §10).
func Hints(status ClusterStatus) []string {
	return hintsAt(status, time.Now())
}

// hintsAt is Hints with an injectable observation time.
func hintsAt(status ClusterStatus, now time.Time) []string {
	var stopped, unreachable, maintenance, configured []NodeStatus
	for _, node := range status.Nodes {
		switch node.Phase {
		case PhaseStopped, PhaseSuspended:
			stopped = append(stopped, node)
		case PhaseUnreachable:
			unreachable = append(unreachable, node)
		case PhaseMaintenance:
			maintenance = append(maintenance, node)
		case PhaseConfigured, PhaseRebooted:
			configured = append(configured, node)
		}
	}

	var hints []string
	if missingTalosCredentials(status) {
		hints = append(hints, fmt.Sprintf(
			"Talos service state unavailable: configured nodes answer apid, but no talosconfig context %q was found. Run: talosctl config merge <path-to-talosconfig> and ensure `talosctl config contexts` lists exactly %q.",
			status.Name, status.Name))
	}
	hints = append(hints, serviceStallHints(status, now)...)
	for _, node := range status.Nodes {
		if node.Phase == PhaseRebooted && node.RebootedAt != nil {
			hints = append(hints, fmt.Sprintf("node %s rebooted without a host VM restart at %s; inspect the recovery with: tbx console %s %s",
				node.Name, node.RebootedAt.UTC().Format(time.RFC3339), shellquote.Quote(status.Name), shellquote.Quote(node.Name)))
		}
	}
	// Capability gates hold whatever the cluster is doing: the config is
	// accepted and the extension baked, but this host cannot honour it.
	for _, capability := range status.Capabilities {
		if !capability.Supported {
			hints = append(hints, fmt.Sprintf("%s is unavailable on this host: %s", capability.Name, capability.Reason))
		}
	}
	if len(status.Nodes) > 0 && len(stopped) == len(status.Nodes) {
		// Hints are meant to be pasted, and a cluster name is only validated as
		// a single path element, so quote it like every other command-bearing
		// hint in this file.
		name := shellquote.Quote(status.Name)
		// Saved memory outranks the stopped reading: start boots the nodes
		// cold and drops the suspended state on the floor, so the hint names
		// resume and says what start would cost (#272).
		if status.Suspended {
			// Saved memory the owning daemon no longer backs is memory in name
			// only: resume will cold-boot every node, so the hint must stop
			// contrasting resume with start as if there were something to lose
			// (#413).
			if status.SavedStateStale {
				return append(hints, fmt.Sprintf(
					"cluster is suspended, but the daemon that saved its memory has been replaced — the saved memory will not survive, so tbx cluster resume %[1]s will cold-boot the nodes (tbx cluster start %[1]s does the same)",
					name))
			}
			return append(hints, fmt.Sprintf("cluster is suspended — resume it with: tbx cluster resume %s (tbx cluster start discards the saved memory)", name))
		}
		return append(hints, fmt.Sprintf("cluster is stopped — start it with: tbx cluster start %s", name))
	}
	if hint := storageHint(status); hint != "" {
		hints = append(hints, hint)
	}
	if hint := longhornSingleNodeHint(status); hint != "" {
		hints = append(hints, hint)
	}
	// A cluster with an unfinished provisioning intent is one tbx is driving:
	// it applies the config and bootstraps etcd itself. The manual
	// talosctl-bootstrap hint below is gated on the same fact so the two can
	// never contradict each other — following the manual one mid-provision
	// would race tbx (#366).
	provisioning := status.CNI != "" && !status.KubernetesReady
	if provisioning {
		hints = append(hints, fmt.Sprintf("%s provisioning is in progress; tbx will apply machine config, bootstrap, and reconcile the CNI. %s;%s", status.CNI, provisioningRecoveryHintAt(status, now), credentialExports(status.Name)))
	}
	if len(maintenance) > 0 {
		first := maintenance[0]
		endpointHost := nodeHost(status, status.controlPlaneOr(first))
		hints = append(hints,
			// A read-only probe comes first so a reader can confirm a
			// maintenance node answers without configuring it: apply-config
			// below is the only other --insecure command printed, and it
			// mutates the node (#435).
			fmt.Sprintf("%d node(s) await machine config. Probe one read-only (changes nothing): talosctl version --insecure --nodes %s",
				len(maintenance), first.IP),
			fmt.Sprintf("generate a machine config: talosctl gen config %s https://%s:6443 --output-dir .%s",
				status.Name, endpointHost, resolverBypassNote(endpointHost)),
			fmt.Sprintf("then apply it: talosctl apply-config --insecure --nodes %s --file controlplane.yaml (workers get worker.yaml)",
				first.IP),
		)
	}
	// Whether a hint above has already named the announced-but-silent VIP. The
	// converging hint below repeats the same fact otherwise, so tbx status
	// printed it twice in consecutive hints (#427).
	vipNoted := false
	if len(configured) == len(status.Nodes) && len(status.Nodes) > 0 {
		cp := status.controlPlaneOr(status.Nodes[0])
		if status.CNI == cluster.CNIFlannel && status.LB && status.KubernetesReady {
			if status.VIPLive {
				hints = append(hints,
					fmt.Sprintf("Kubernetes is Ready; MetalLB L2 VIP is live at http://%s/. Flannel does not enforce NetworkPolicies; use cilium to exercise policies.%s", status.VIP, credentialExports(status.Name)),
				)
			} else {
				hints = append(hints,
					fmt.Sprintf("Kubernetes is Ready; %s. Flannel does not enforce NetworkPolicies; use cilium to exercise policies.%s", vipSettlingNote(status, "MetalLB L2"), credentialExports(status.Name)),
				)
				vipNoted = true
			}
		}
		if status.CNI == cluster.CNIFlannel && !status.LB && status.KubernetesReady {
			hints = append(hints,
				"Kubernetes is Ready with Talos-managed flannel; LoadBalancer support is disabled by lb: false, so no VIP is provisioned."+credentialExports(status.Name),
			)
		}
		if status.CNI == cluster.CNICilium && status.LB && status.KubernetesReady {
			if status.VIPLive {
				hints = append(hints, fmt.Sprintf("Kubernetes is Ready; Cilium LB-IPAM VIP is live at http://%s/.%s", status.VIP, credentialExports(status.Name)))
			} else {
				hints = append(hints, fmt.Sprintf("Kubernetes is Ready; %s.%s", vipSettlingNote(status, "Cilium LB-IPAM"), credentialExports(status.Name)))
				vipNoted = true
			}
		}
		if status.CNI == cluster.CNICilium && !status.LB && status.KubernetesReady {
			hints = append(hints, "Kubernetes is Ready with Cilium; LoadBalancer support is disabled by lb: false, so no VIP is provisioned."+credentialExports(status.Name))
		}
		if !status.KubernetesReady {
			// Bootstrapping by hand belongs to the substrate-only path and to a
			// cluster nobody is driving; while tbx is provisioning, the hint
			// above owns the bootstrap and this one would race it (#366).
			if !provisioning && !anyHealthyKubelet(status.Nodes) {
				hints = append(hints,
					fmt.Sprintf("all nodes configured. If etcd is not yet bootstrapped: talosctl bootstrap --talosconfig ./talosconfig --nodes %[1]s --endpoints %[1]s, then talosctl kubeconfig . --talosconfig ./talosconfig --nodes %[1]s --endpoints %[1]s", cp.IP),
				)
			}
			hints = append(hints,
				fmt.Sprintf("node TUI (the Talos dashboard): talosctl dashboard --talosconfig ./talosconfig --nodes %[1]s --endpoints %[1]s", cp.IP),
			)
		}
	}
	// convergingReasons also feeds the machine-readable "converging" array, so
	// the VIP reason is dropped here rather than at its source (#396, #427).
	reasons := convergingReasons(status, now)
	if vipNoted {
		reasons = withoutReason(reasons, vipConvergingReason(status))
	}
	if len(reasons) > 0 {
		hints = append(hints, fmt.Sprintf(
			"nodes are up but the cluster is still settling — %s; a single sample of tbx status can read green while these are in flight",
			strings.Join(reasons, "; ")))
	}
	booting, stalled := splitStalledNodes(unreachable, now)
	if len(booting) > 0 {
		hints = append(hints,
			fmt.Sprintf("%d node(s) not answering yet — boot takes ~%s; if it persists, run: tbx doctor", len(booting), formatBootWindow(nodeBootWindow)),
		)
	}
	if hint := stalledNodesHint(status.Name, stalled, now); hint != "" {
		hints = append(hints, hint)
	}
	return hints
}

func missingTalosCredentials(status ClusterStatus) bool {
	for _, node := range status.Nodes {
		if node.ServiceProbe != nil && node.ServiceProbe.Status == ServiceProbeMissingCredentials {
			return true
		}
	}
	return false
}

func serviceStallHints(status ClusterStatus, now time.Time) []string {
	var hints []string
	clusterName := shellquote.Quote(status.Name)
	for _, node := range status.Nodes {
		nodeName := shellquote.Quote(node.Name)
		for _, stalled := range node.StalledServices {
			age := now.Sub(stalled.Since)
			if age < 0 {
				continue
			}
			hints = append(hints, fmt.Sprintf(
				"%s: %s has remained %s for %s — image pull may be stalled; inspect with: tbx console %s %s; cold-restart with: tbx node stop %s %s && tbx node start %s %s",
				node.Name, stalled.Service, stalled.State, formatStallDuration(age), clusterName, nodeName, clusterName, nodeName, clusterName, nodeName))
		}
	}
	return hints
}

// vipSettlingNote separates "no VIP announced yet" from "announced but not
// answering". The probe used to collapse both into a bare wait, and after a
// snapshot restore the VIP is announced ~10-20s before it answers — the window
// a single-sample check had nothing honest to key on (#427).
func vipSettlingNote(status ClusterStatus, announcer string) string {
	if status.VIP == "" {
		return fmt.Sprintf("waiting for the %s LoadBalancer VIP to be announced", announcer)
	}
	return fmt.Sprintf("the %s LoadBalancer VIP %s is announced but not answering yet", announcer, status.VIP)
}

// clusterSettleWindow is how long after a node's VM launched the cluster is
// still described as settling even when everything the daemon can probe reads
// green. It covers the two windows QA measured at ~2.5 minutes each: kubelet
// serving certificates being re-issued after a cold boot, and CSI drivers
// re-registering with the kubelet after a snapshot restore (#396). Neither is
// observable from the daemon's Kubernetes client — nodes report Ready
// throughout — so the honest signal is the age of the boot, stated as such.
const clusterSettleWindow = 3 * time.Minute

// convergingReasons names what is still coming back on a cluster whose nodes
// are up and whose Kubernetes reports Ready. It is the difference between
// "converged" and "nodes up, still settling": empty means the daemon has
// nothing outstanding, and every entry is derived from a fact status already
// holds, so it costs no extra probe.
func convergingReasons(status ClusterStatus, now time.Time) []string {
	if !status.Running || !status.KubernetesReady {
		return nil
	}
	var reasons []string
	// A terminal storage failure is not something that is still settling: the
	// pass that owned it has ended and the storage hint already says so, so
	// claiming the CSI probe has yet to pass would re-create the "provisioning
	// in progress for work that has already ended" friction (#395).
	if status.CSI != "" && status.StoragePhase != StoragePhaseLive && status.StoragePhase != StoragePhaseFailed {
		reasons = append(reasons, fmt.Sprintf("the %s CSI drivers have not passed the readiness probe yet, so PVC mounts can fail", status.CSI))
	}
	if reason := vipConvergingReason(status); reason != "" {
		reasons = append(reasons, reason)
	}
	if age, booting := youngestNodeAge(status, now); booting {
		reasons = append(reasons, fmt.Sprintf(
			"a node booted %s ago, and kubelet serving certificates and CSI driver registrations can take ~%s to settle (kubectl exec and PVC mounts may fail until they do)",
			formatStallDuration(age), formatBootWindow(clusterSettleWindow)))
	}
	return reasons
}

// vipConvergingReason names an announced VIP that is not answering yet, or ""
// when there is nothing outstanding to say about it.
func vipConvergingReason(status ClusterStatus) string {
	if !status.LB || status.VIP == "" || status.VIPLive {
		return ""
	}
	return fmt.Sprintf("the LoadBalancer VIP %s is announced but not answering yet", status.VIP)
}

// withoutReason drops one already-printed reason from a converging list.
func withoutReason(reasons []string, drop string) []string {
	if drop == "" {
		return reasons
	}
	kept := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		if reason != drop {
			kept = append(kept, reason)
		}
	}
	return kept
}

// youngestNodeAge reports how long ago the most recently launched node's VM
// started, and whether that is still inside the settle window. A cluster whose
// launches this daemon never saw reports no age at all — it cannot prove the
// cluster is fresh, so it stays quiet.
func youngestNodeAge(status ClusterStatus, now time.Time) (time.Duration, bool) {
	youngest := time.Duration(-1)
	for _, node := range status.Nodes {
		if node.StartedAt == nil {
			continue
		}
		age := now.Sub(*node.StartedAt)
		if youngest < 0 || age < youngest {
			youngest = age
		}
	}
	if youngest < 0 || youngest >= clusterSettleWindow {
		return 0, false
	}
	return youngest, true
}

// provisioningRecoveryHint names the recovery an unfinished provisioning pass
// can actually take. `tbx up` needs a talosbox.yaml; a cluster created
// imperatively has none, so pointing at up would dead-end and the honest path
// is destroy and recreate (#267). A cluster whose origin is unknown — created
// by a tbx predating the recorded flag — keeps the `tbx up` wording: its file
// may well be sitting right there, and guessing "imperative" would advise
// destroying a cluster a later up could simply resume.
func provisioningRecoveryHint(status ClusterStatus) string {
	return provisioningRecoveryHintAt(status, time.Now())
}

// provisioningRecoveryHintAt is provisioningRecoveryHint with an injectable
// observation time, which the destruction debounce below is measured against.
func provisioningRecoveryHintAt(status ClusterStatus, now time.Time) string {
	// A node whose kubelet cannot start is not a pass that failed to finish;
	// it is a node that will never join. Rerunning up against it only burns
	// another provisioning deadline, so the crashloop names the node and the
	// two moves that can actually change the outcome (#357).
	if hint := kubeletCrashLoopHint(status); hint != "" {
		return hint
	}
	if status.ConfigOrigin != cluster.OriginImperative {
		return "Rerun: tbx up"
	}
	// Destroying a cluster is the most expensive advice status gives, and a
	// single unreachable observation is not evidence for it: an apiserver blip
	// on a converged cluster read exactly like a provisioning pass that never
	// finished, and the hint told the operator to destroy a working cluster
	// (#418). Escalate only once the daemon has watched the condition persist;
	// until then name the cheap moves. An unknown observation window — an
	// older daemon, a status built outside the readiness log — keeps the
	// escalation, because suppressing it there would silence the hint for the
	// stuck cluster it exists for.
	if status.KubernetesUnreadyBriefly(now) {
		return fmt.Sprintf(
			"Kubernetes has been unreachable for %s, which is short enough to be a transient control-plane blip: rerun tbx status %s in a moment, and if it persists check tbx console %s <node> or ~/.talosbox/tbxd.log before considering a destroy",
			formatStallDuration(status.kubernetesUnreadyFor(now)), shellquote.Quote(status.Name), shellquote.Quote(status.Name),
		)
	}
	// No concrete `tbx cluster create` line: status carries the intent but not
	// the node sizing, so a rendered command would rebuild a materially
	// different cluster after the destroy already happened. The recorded
	// intent is named as an observation, not as the command to paste.
	return fmt.Sprintf(
		"No talosbox.yaml backs this cluster, so tbx up cannot resume it; destroy it with: tbx cluster destroy %s --force, then recreate it with the tbx cluster create flags you used originally (recorded intent: %s; node sizing is not recorded here)",
		shellquote.Quote(status.Name), recordedIntentFlags(status),
	)
}

// kubeletCrashLoopHint names the nodes whose kubelet Talos keeps restarting,
// carries the message that says why, and points at the console and at
// remove+add — the remedies that exist for a node whose disk is bad (#357).
// It is empty when no node reports a crashlooping kubelet, including when the
// daemon has no reading at all.
func kubeletCrashLoopHint(status ClusterStatus) string {
	var crashed []NodeStatus
	for _, node := range status.Nodes {
		if node.Kubelet != nil && node.Kubelet.CrashLooping() {
			crashed = append(crashed, node)
		}
	}
	if len(crashed) == 0 {
		return ""
	}
	name := shellquote.Quote(status.Name)
	first := crashed[0]
	// The node name is as operator-supplied as the cluster name is
	// (tbx node add <cluster> <node>), and these commands are printed to be
	// pasted, so it needs the same quoting the cluster name gets (#357).
	node := shellquote.Quote(first.Name)
	subject := fmt.Sprintf("%s's kubelet is crashlooping", first.Name)
	if len(crashed) > 1 {
		names := make([]string, 0, len(crashed))
		for _, node := range crashed {
			names = append(names, node.Name)
		}
		subject = fmt.Sprintf("kubelet is crashlooping on %d node(s): %s", len(crashed), strings.Join(names, ", "))
	}
	detail := ""
	if first.Kubelet.Message != "" {
		detail = fmt.Sprintf(" (%s)", first.Kubelet.Message)
	}
	return fmt.Sprintf("%s%s, so it will not join Kubernetes and rerunning tbx up cannot fix it: inspect it live with tbx console %s %s, then replace it: tbx node remove %s %s, tbx node add %s %s --role %s",
		subject, detail, name, node, name, node, name, node, first.Role)
}

// recordedIntentFlags renders what the cluster's stored state actually knows
// about how it was created, so the operator recreating it has the shape in
// front of them instead of reconstructing it from memory.
func recordedIntentFlags(status ClusterStatus) string {
	flags := []string{fmt.Sprintf("--cni %s", status.CNI)}
	if status.CSI != "" {
		flags = append(flags, fmt.Sprintf("--csi %s", status.CSI))
	}
	if !status.LB {
		flags = append(flags, "--lb=false")
	}
	if status.BGP {
		flags = append(flags, "--bgp")
	}
	if status.Hubble {
		flags = append(flags, "--hubble")
	}
	controlPlanes, workers := 0, 0
	for _, node := range status.Nodes {
		if node.Role == cluster.RoleControlPlane {
			controlPlanes++
			continue
		}
		workers++
	}
	flags = append(flags, fmt.Sprintf("--cp %d", controlPlanes), fmt.Sprintf("--workers %d", workers))
	if status.Domain != "" && status.Domain != status.Name+"."+cluster.DefaultDomainSuffix {
		flags = append(flags, fmt.Sprintf("--domain %s", status.Domain))
		// Without the opt-in the create would refuse the very domain this
		// line names, so the flags stay replayable as printed.
		if status.AllowUnsafeDomain {
			flags = append(flags, "--allow-unsafe-domain")
		}
	}
	// The image is as much of the recorded intent as the topology is. Dropping
	// it would send the operator back to the daemon's current default version
	// with no extensions at all — a materially different cluster, built after
	// the destroy already made the original unrecoverable (#267).
	if status.TalosVersion != "" {
		flags = append(flags, fmt.Sprintf("--talos-version %s", status.TalosVersion))
	}
	if len(status.TalosExtensions) > 0 {
		flags = append(flags, fmt.Sprintf("--extensions %s", strings.Join(status.TalosExtensions, ",")))
		// Schematic is the composed id once extensions were folded into it, so
		// replaying it alongside --extensions would compose them a second time.
		// The base is the schematic the create actually took, and an empty one
		// means the create took none.
		if status.BaseSchematic != "" {
			flags = append(flags, fmt.Sprintf("--schematic %s", status.BaseSchematic))
		}
	} else if status.Schematic != "" {
		flags = append(flags, fmt.Sprintf("--schematic %s", status.Schematic))
	}
	return strings.Join(flags, " ")
}

// splitStalledNodes separates nodes still inside their boot budget from the
// ones that have blown well past it. A node whose start time is unknown counts
// as booting: the daemon cannot prove it is stuck, so it stays calm.
func splitStalledNodes(unreachable []NodeStatus, now time.Time) (booting, stalled []NodeStatus) {
	for _, node := range unreachable {
		if node.UnreachableFor(now) > nodeStallThreshold {
			stalled = append(stalled, node)
			continue
		}
		booting = append(booting, node)
	}
	return booting, stalled
}

// stalledNodesHint names the stuck nodes, says how long they have been silent,
// and points at the console — the only live evidence of what a node that never
// answers apid is actually doing.
func stalledNodesHint(clusterName string, stalled []NodeStatus, now time.Time) string {
	switch len(stalled) {
	case 0:
		return ""
	case 1:
		node := stalled[0]
		if node.answeredSinceStart() {
			return fmt.Sprintf("%s stopped answering %s ago — inspect it live: tbx console %s %s; then run: tbx doctor",
				node.Name, formatStallDuration(node.UnreachableFor(now)), clusterName, node.Name)
		}
		return fmt.Sprintf("%s has not answered for %s since its VM started — watch it boot: tbx console %s %s; then run: tbx doctor",
			node.Name, formatStallDuration(node.UnreachableFor(now)), clusterName, node.Name)
	default:
		descriptions := make([]string, 0, len(stalled))
		for _, node := range stalled {
			age := formatStallDuration(node.UnreachableFor(now))
			if node.answeredSinceStart() {
				descriptions = append(descriptions, fmt.Sprintf("%s (stopped answering %s ago)", node.Name, age))
				continue
			}
			descriptions = append(descriptions, fmt.Sprintf("%s (%s since its VM started)", node.Name, age))
		}
		return fmt.Sprintf("%d node(s) are not answering: %s — inspect one live: tbx console %s <node>; then run: tbx doctor",
			len(stalled), strings.Join(descriptions, ", "), clusterName)
	}
}

// formatStallDuration keeps stall ages readable: seconds are noise once a node
// is minutes past its boot window.
func formatStallDuration(d time.Duration) string {
	return d.Round(time.Second).String()
}

func credentialExports(name string) string {
	name = shellquote.Quote(name)
	return fmt.Sprintf(" export TALOSCONFIG=~/.talosbox/clusters/%s/talosconfig; export KUBECONFIG=~/.talosbox/clusters/%s/kubeconfig", name, name)
}

// storageRecoveryHint names the recovery for a terminal storage failure. It is
// deliberately not provisioningRecoveryHint: a failed storage stage does not
// condemn the cluster, so recommending a destroy-and-recreate for it is both
// far too expensive and — whenever the provisioning hint fires alongside this
// one — the same ~200-character paragraph printed twice in consecutive hints,
// the duplicate-fact defect #427 removed for the VIP note. The move that
// actually re-drives the storage pass is a stop/start: Server.stop invalidates
// the storage phase, and start provisions again.
func storageRecoveryHint(status ClusterStatus) string {
	name := shellquote.Quote(status.Name)
	if status.ConfigOrigin != cluster.OriginImperative {
		return fmt.Sprintf("Re-run the storage pass with: tbx up (or tbx cluster stop %[1]s && tbx cluster start %[1]s)", name)
	}
	return fmt.Sprintf("Re-run the storage pass with: tbx cluster stop %[1]s && tbx cluster start %[1]s", name)
}

func storageHint(status ClusterStatus) string {
	if status.CSI == "" {
		return ""
	}
	switch status.StoragePhase {
	case StoragePhaseFailed:
		// Terminal: the pass that would have made storage live has ended, so
		// the hint says so and points at the recovery instead of describing a
		// wait nothing is serving (#395).
		if status.StorageError != "" {
			return fmt.Sprintf("storage provisioning failed: %s. Nothing is retrying. %s", status.StorageError, storageRecoveryHint(status))
		}
		return fmt.Sprintf("storage provisioning failed. Nothing is retrying. %s", storageRecoveryHint(status))
	case StoragePhaseProvisioning:
		// A running pass knows which gate is holding it, and that gate is
		// frequently not the readiness probe — naming the probe regardless
		// pointed diagnosis at a subsystem that had not even started (#391).
		if status.StorageGate != "" {
			if status.StorageError != "" {
				return fmt.Sprintf("storage provisioning: waiting on the %s gate: %s.", status.StorageGate, status.StorageError)
			}
			return fmt.Sprintf("storage provisioning: waiting on the %s gate.", status.StorageGate)
		}
		if status.StorageError != "" {
			return fmt.Sprintf("storage provisioning: CSI readiness probe failed: %s; retrying after backoff.", status.StorageError)
		}
		if status.StoragePending != "" {
			// Nothing failed: the probe is waiting on its own previous pass to
			// finish clearing, so the wording stays "still working". The pending
			// note itself is the probe's advisory, written for the verb that
			// raised it — it says "cleanup is still finishing" a second time and
			// sends the reader to `tbx status`, which is where they already are.
			// The hint says it once, in status's own voice, but keeps the one
			// fact status cannot derive: which object is still terminating.
			if detail := storagePendingDetail(status.StoragePending); detail != "" {
				return fmt.Sprintf("storage provisioning: the storage probe's cleanup from a previous pass is still finishing (%s); the daemon retries automatically.", detail)
			}
			return "storage provisioning: the storage probe's cleanup from a previous pass is still finishing; the daemon retries automatically."
		}
		return "storage provisioning: waiting for the CSI readiness probe to pass."
	case StoragePhaseLive:
		return "storage live: the CSI readiness probe passed."
	default:
		return ""
	}
}

// storagePendingDetail lifts the one fact out of the probe's pending advisory
// that status cannot derive for itself: the object whose termination the next
// pass is waiting on. Everything around it — the repeated "still finishing",
// the pointer back at `tbx status` — is the advisory talking to the verb that
// raised it, so the parenthetical the probe wraps the object in is preferred
// and the deadline the wait finally hit is dropped: it says nothing the "still
// finishing" lead clause has not already said. An advisory that carries no
// such detail, or whose detail would repeat the sentence or send the reader
// back here, yields nothing and the hint stays as it was.
func storagePendingDetail(pending string) string {
	detail := pending
	if _, after, found := strings.Cut(detail, "still finishing: "); found {
		detail = after
	}
	if open := strings.Index(detail, "("); open >= 0 {
		if width := strings.Index(detail[open:], ")"); width > 0 {
			detail = detail[open+1 : open+width]
		}
	}
	detail = strings.ReplaceAll(detail, "`", "")
	// The advisory may carry an errors.Join of several waits that all hit the
	// same deadline: one observation per line, each ending in the deadline the
	// lead clause already reports. Fold them into one line and drop every
	// deadline mention, not just a trailing one.
	detail = strings.ReplaceAll(detail, ": "+context.DeadlineExceeded.Error(), "")
	lines := make([]string, 0, 4)
	for line := range strings.SplitSeq(detail, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == context.DeadlineExceeded.Error() {
			continue
		}
		lines = append(lines, line)
	}
	detail = strings.Join(lines, "; ")
	const storagePendingDetailMaxLen = 200
	if len(detail) > storagePendingDetailMaxLen {
		detail = detail[:storagePendingDetailMaxLen] + "…"
	}
	if detail == "" || strings.Contains(detail, "still finishing") || strings.Contains(detail, "tbx ") {
		return ""
	}
	return detail
}

func longhornSingleNodeHint(status ClusterStatus) string {
	if status.CSI == cluster.CSILonghorn &&
		storageNodeCount(status) == 1 &&
		status.Running &&
		status.StoragePhase == StoragePhaseLive {
		return "Longhorn is running with a single replica on one node, so volumes have no redundancy."
	}
	return ""
}

// storageNodeCount mirrors the provisioning replica policy: replicas live on
// workers, or on the control planes of a worker-less cluster.
func storageNodeCount(status ClusterStatus) int {
	workers := 0
	for _, node := range status.Nodes {
		if node.Role == cluster.RoleWorker {
			workers++
		}
	}
	if workers == 0 {
		return len(status.Nodes)
	}
	return workers
}

// controlPlaneOr returns the cluster's first control-plane node, or fallback.
func (c ClusterStatus) controlPlaneOr(fallback NodeStatus) NodeStatus {
	for _, node := range c.Nodes {
		if node.Role == cluster.RoleControlPlane {
			return node
		}
	}
	return fallback
}

// hintGOOS is the host platform the hints are written for. It is a variable so
// a test can exercise the macOS-only wording from any host.
var hintGOOS = runtime.GOOS

// resolverBypassNote warns that the most obvious DNS tools do not see the name
// the hint just handed out. macOS keeps tbx's per-domain entries under
// /etc/resolver, which dig and nslookup bypass entirely, so the first tool
// anyone reaches for reads a working name as dead (#438). Empty everywhere
// else: no other host substrate has the split.
func resolverBypassNote(host string) string {
	if hintGOOS != "darwin" {
		return ""
	}
	return fmt.Sprintf(" (dig/nslookup bypass /etc/resolver on macOS; check that name with: dscacheutil -q host -a name %s, or ping)", shellquote.Quote(host))
}

// nodeHost prefers the DNS name talosbox serves for a node.
func nodeHost(status ClusterStatus, node NodeStatus) string {
	domain := status.Domain
	if domain == "" {
		domain = status.Name + "." + cluster.DefaultDomainSuffix
	}
	return node.Name + "." + domain
}
