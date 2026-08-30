package main

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
	"time"

	"github.com/randax/talos-box/internal/balloon"
	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/daemon"
	"github.com/randax/talos-box/internal/extensions"
	"github.com/randax/talos-box/internal/helper"
	"github.com/randax/talos-box/internal/hostpressure"
	"github.com/randax/talos-box/internal/hypervisor"
	"github.com/randax/talos-box/internal/shellquote"
	"github.com/randax/talos-box/internal/version"
)

type doctorDependencies struct {
	runtimeIdentity func(context.Context) runtimeIdentity
	checkHelper     func() error
	checkResolver   func() error
	checkDirectDNS  func() error
	checkForwarding func() error
	listClusters    func() ([]daemon.ClusterSummary, error)
	listConfig      func() ([]cluster.Cluster, error)
	getStatus       func() ([]daemon.ClusterStatus, error)
	listCache       func() (daemon.CacheListResult, error)
	// mirrorOffline reports the mirror's offline mode, which persists across a
	// daemon restart and changes how every pull on the host fails (#403).
	mirrorOffline func() (bool, error)
	hostPressure  func() (hostpressure.Snapshot, error)
	// hostFreeMemory is the free-memory reading the provision-start gate
	// refuses on. The host-pressure snapshot does not carry it — Assess never
	// needed it — but the check is not self-attesting without it (#420). Nil
	// reports free memory as unmeasured rather than guessing.
	hostFreeMemory func() (int, error)
	// balloonReserveMiB is the provision-start gate's host-memory reserve as
	// the daemon itself reads it. TBX_BALLOON_RESERVE_MIB is read per process,
	// so the CLI's own default is only a guess about the gate; asking the
	// daemon is what makes the reported headroom the number that will decide
	// the next bringup (#397, #420). Nil or an error falls back to the default.
	balloonReserveMiB func() (int, error)
	// balloonDisabled reports whether the daemon launches guests without a
	// balloon device (TBX_DISABLE_BALLOON, #513); only a disabled balloon
	// prints a line, as INFO, so the operator's own choice never nags.
	balloonDisabled func() (bool, error)
	daemonInfo      func() (daemon.Info, error)
	hypervisors     func(context.Context) (hypervisor.Registry, error)
	command         commandOutput
	readFile        func(string) ([]byte, error)
	accessRW        func(string) error
	listenPacket    func(string, string) (net.PacketConn, error)
	listenStream    func(string, string) (io.Closer, error)
	doHTTP          httpDo
	// doVIPHTTP probes cluster VIPs. It is separate from doHTTP because those
	// addresses are host-local and must never go through an HTTP proxy.
	doVIPHTTP httpDo
	platform  func() []doctorFinding
}

// doctorHelp answers `tbx doctor --help`. It names every check the command can
// report and states the exit-code contract, which is the fact a script needs
// and used to live only in the platform docs (#419).
func doctorHelp() string {
	var b strings.Builder
	b.WriteString(`tbx doctor checks whether this host can run talos-box clusters, and whether the
clusters that already exist are wired up. Every check is read-only: doctor
reports, it never repairs. One line per finding, each PASS, WARN, FAIL, INFO, or
SKIP (the probe did not apply here). Most checks report a single line, but a
check may report several: host-pressure reports one per condition that fired,
security-inventory one per activated system extension. The runtime identity
block names the client, daemon, and helper before those findings.

Checks:
  runtime-compat       client, daemon, and helper version/protocol agreement
  Hypervisors          known backends, availability, default source, and feature gates
  installations       distinct tbx executables found on PATH (WARN only)
  helper              privileged helper is installed and answers
  resolver            per-domain resolver wiring for the cluster domains
  DNS                 cluster names resolve against the daemon's DNS directly
  forwarding (host)   current host sysctl, read directly by this client
  host-pressure       host memory and disk headroom (the same gate that blocks
                      cluster create)
  balloon             INFO only, printed when the daemon runs guests without a
                      memory balloon device (TBX_DISABLE_BALLOON)
  system-dns          the system resolver returns each cluster's domain
  routes              host routes reach the nodes of running clusters
  inter-cluster       running clusters reach each other's ingress VIPs, from the
                      host and from inside a sibling cluster
  talos-services      service state reported by configured running nodes
  guest-agent         host support for clusters that baked qemu-guest-agent
  mirror-health       registry-mirror listeners match the running clusters
  mirror-offline      whether the registry mirror is serving from cache only
  image-cache         cached Talos disk images, including incomplete pulls
  egress              image-factory reachability for schematic builds
  security-inventory  host security posture, informational only
`)
	if names := platformDoctorCheckNames(); len(names) > 0 {
		fmt.Fprintf(&b, "\nOn this platform doctor also checks: %s.\n", strings.Join(names, ", "))
	}
	b.WriteString(`
Exit code:
  tbx doctor exits non-zero when any check reports FAIL. WARN, INFO, SKIP, and
  PASS all leave the exit code 0, so a WARN never fails a scripted preflight.

usage: tbx doctor
`)
	return b.String()
}

// doctorFreeMemoryMiB reads host free memory for the host-pressure summary. It
// never fails the check: an unreadable probe reports the other two numbers and
// says free memory was not measured.
// doctorBalloonReserveMiB reports the reserve the daemon's provision-start gate
// measures against, and whether it came from the daemon: a reserve the CLI had
// to assume is worth saying so, because an env-set reserve in the daemon's
// environment makes the printed headroom disagree with the gate.
func doctorBalloonReserveMiB(deps doctorDependencies) (int, bool) {
	if deps.balloonReserveMiB == nil {
		return balloon.DefaultConfig().ReserveMiB, false
	}
	reserveMiB, err := deps.balloonReserveMiB()
	// A daemon predating the field answers zero, which is no answer at all.
	if err != nil || reserveMiB <= 0 {
		return balloon.DefaultConfig().ReserveMiB, false
	}
	return reserveMiB, true
}

func doctorFreeMemoryMiB(deps doctorDependencies) int {
	if deps.hostFreeMemory == nil {
		return 0
	}
	freeMiB, err := deps.hostFreeMemory()
	if err != nil {
		return 0
	}
	return freeMiB
}

type doctorFinding struct {
	level  string
	check  string
	detail string
}

type skippedDoctorCheck struct{ detail string }

func (e skippedDoctorCheck) Error() string { return e.detail }

func (c cli) runDoctor(args []string) error {
	return c.runDoctorWithDependencies(args, c.doctorDependencies())
}

func (c cli) runDoctorWithDependencies(args []string, deps doctorDependencies) error {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		_, err := fmt.Fprint(c.out, doctorHelp())
		return err
	}
	if len(args) != 0 {
		return errors.New("usage: tbx doctor")
	}
	if deps.daemonInfo != nil {
		// One daemon.info call feeds the balloon findings and the hypervisor
		// inventory; its bound lives in doctorCall like every other diagnostic.
		deps.daemonInfo = sync.OnceValues(deps.daemonInfo)
		if deps.balloonReserveMiB == nil {
			deps.balloonReserveMiB = func() (int, error) {
				info, err := deps.daemonInfo()
				return info.BalloonReserveMiB, err
			}
		}
		if deps.balloonDisabled == nil {
			deps.balloonDisabled = func() (bool, error) {
				info, err := deps.daemonInfo()
				return info.BalloonDisabled, err
			}
		}
	}

	identity := runtimeIdentity{
		Client: componentIdentity{Name: "client", Path: "unknown", Version: version.Version, Protocol: daemon.ProtocolVersion, Available: true},
		Daemon: componentIdentity{Name: "daemon", Detail: "not running"},
		Helper: componentIdentity{Name: "helper", Detail: "not installed"},
	}
	if deps.runtimeIdentity != nil {
		identity = deps.runtimeIdentity(context.Background())
	}
	if err := renderRuntimeIdentity(c.out, identity); err != nil {
		return err
	}
	hypervisorInventory := doctorHypervisors(deps)
	for _, finding := range hypervisorFindings(hypervisorInventory) {
		if err := writeRuntimeIdentityFinding(c.out, finding); err != nil {
			return err
		}
	}
	failed := false
	for _, finding := range identity.Findings {
		if finding.level == "FAIL" {
			failed = true
		}
	}
	writeFindings := func(findings ...doctorFinding) error {
		for _, finding := range findings {
			if finding.level == "FAIL" {
				failed = true
			}
			if err := writeRuntimeIdentityFinding(c.out, finding); err != nil {
				return err
			}
		}
		return nil
	}

	for _, check := range []struct {
		name string
		run  func() error
	}{
		{name: "helper", run: deps.checkHelper},
		{name: "resolver", run: deps.checkResolver},
		{name: "DNS", run: deps.checkDirectDNS},
		{name: "forwarding (host)", run: deps.checkForwarding},
	} {
		finding := doctorFinding{level: "PASS", check: check.name}
		if err := check.run(); err != nil {
			var skipped skippedDoctorCheck
			if errors.As(err, &skipped) {
				finding.level, finding.detail = "SKIP", skipped.Error()
			} else if check.name == "resolver" {
				finding.level, finding.detail = classifyResolverFailure(err)
			} else {
				finding.level, finding.detail = "FAIL", err.Error()
			}
		}
		if err := writeFindings(finding); err != nil {
			return err
		}
	}
	if deps.platform != nil {
		if err := writeFindings(deps.platform()...); err != nil {
			return err
		}
	}

	if deps.hostPressure == nil {
		if err := writeFindings(doctorFinding{level: "SKIP", check: "host-pressure", detail: "probe unavailable"}); err != nil {
			return err
		}
	} else if snapshot, err := deps.hostPressure(); err != nil {
		if err := writeFindings(doctorFinding{level: "SKIP", check: "host-pressure", detail: err.Error()}); err != nil {
			return err
		}
	} else {
		// Free memory and the daemon's reserve are classification inputs, not
		// PASS-only decoration: enough present RAM is what distinguishes stale
		// allocated swap from the active swapping that corrupted guests (#483).
		if snapshot.FreeMemoryMiB <= 0 {
			snapshot.FreeMemoryMiB = doctorFreeMemoryMiB(deps)
		}
		reserveMiB, fromDaemon := doctorBalloonReserveMiB(deps)
		findings := hostpressure.Assess(snapshot, reserveMiB)
		pendingGuestsClause := ""
		if snapshot.FreeMemoryMiB > 0 {
			if snapshot.FreeMemoryMiB < reserveMiB {
				pendingGuestsClause = fmt.Sprintf(" (measured with no guests pending: the host is already %d MiB below the balloon reserve; any new guest start will be refused until memory frees)", reserveMiB-snapshot.FreeMemoryMiB)
			} else {
				pendingGuestsClause = fmt.Sprintf(" (measured with no guests pending: a start larger than %d MiB of new guests will still be refused)", hostpressure.GuestHeadroomMiB(snapshot.FreeMemoryMiB, reserveMiB))
			}
		}
		if len(findings) == 0 {
			// A PASS states the three numbers it was decided on — free memory,
			// swap, data-volume space — plus the next bringup's headroom.
			detail := snapshot.Summary(reserveMiB) + pendingGuestsClause
			if !fromDaemon {
				detail += " (daemon unreachable; assuming the default reserve)"
			}
			if err := writeFindings(doctorFinding{
				level: "PASS", check: "host-pressure", detail: detail,
			}); err != nil {
				return err
			}
		} else {
			// Same classification the daemon's start gate uses: a blocking
			// finding is what makes cluster create refuse, so it fails here too.
			for _, finding := range findings {
				level := "WARN"
				if finding.Severity == hostpressure.SeverityBlock {
					level = "FAIL"
				}
				detail := finding.DoctorDetail()
				if level == "WARN" {
					detail += pendingGuestsClause
				}
				if err := writeFindings(doctorFinding{
					level: level, check: "host-pressure", detail: detail,
				}); err != nil {
					return err
				}
			}
		}
	}

	// right after host-pressure, where the check list documents it
	if finding, ok := balloonDisabledFinding(deps.balloonDisabled); ok {
		if err := writeFindings(finding); err != nil {
			return err
		}
	}

	clusters, clusterErr := deps.listClusters()
	anyRunning := false
	var statuses []daemon.ClusterStatus
	var statusErr error
	if isDaemonUnavailable(clusterErr) {
		detail := daemonUnavailableDetail(clusterErr)
		if err := writeFindings(
			doctorFinding{level: "SKIP", check: "system-dns", detail: detail},
			doctorFinding{level: "SKIP", check: "routes", detail: detail},
			doctorFinding{level: "SKIP", check: "inter-cluster", detail: detail},
		); err != nil {
			return err
		}
	} else if clusterErr != nil {
		detail := fmt.Sprintf("list clusters: %v", clusterErr)
		if err := writeFindings(
			doctorFinding{level: "FAIL", check: "system-dns", detail: detail},
			doctorFinding{level: "FAIL", check: "routes", detail: detail},
			doctorFinding{level: "SKIP", check: "inter-cluster", detail: detail},
		); err != nil {
			return err
		}
	} else if len(clusters) == 0 {
		if err := writeFindings(
			doctorFinding{level: "SKIP", check: "system-dns", detail: "no clusters exist"},
			doctorFinding{level: "SKIP", check: "routes", detail: "no clusters exist"},
			doctorFinding{level: "SKIP", check: "inter-cluster", detail: "no clusters exist"},
		); err != nil {
			return err
		}
	} else {
		dnsFinding := doctorFinding{level: "PASS", check: "system-dns"}
		if err := checkSystemDNS(clusters, deps.command); err != nil {
			dnsFinding.level, dnsFinding.detail = classifySystemDNSFailure(err)
		}
		if err := writeFindings(dnsFinding); err != nil {
			return err
		}

		for _, item := range clusters {
			if item.Running {
				anyRunning = true
				break
			}
		}
		if !anyRunning {
			// stopped clusters have no interfaces to route through
			if err := writeFindings(
				doctorFinding{level: "SKIP", check: "routes", detail: "no clusters are running"},
				doctorFinding{level: "SKIP", check: "inter-cluster", detail: "no clusters are running"},
			); err != nil {
				return err
			}
		} else {
			statuses, statusErr = deps.getStatus()
			routeFinding := doctorFinding{level: "PASS", check: "routes"}
			var routeProblems []string
			if statusErr != nil {
				routeProblems = append(routeProblems,
					fmt.Sprintf("cluster status unavailable; node routes could not be checked: %v", statusErr))
			}
			if err := checkClusterRoutes(clusters, statuses, platformRouteProbe(deps.command)); err != nil {
				routeProblems = append(routeProblems, err.Error())
			}
			if len(routeProblems) != 0 {
				routeFinding.level, routeFinding.detail = "FAIL", strings.Join(routeProblems, "; ")
			}
			if err := writeFindings(routeFinding); err != nil {
				return err
			}
			// Routes and forwarding assert only that the host could carry the
			// traffic; this asks whether it actually does (#388).
			if err := writeFindings(interClusterFinding(statuses, statusErr, deps.doVIPHTTP)); err != nil {
				return err
			}
		}
	}
	if err := writeFindings(talosServicesFindings(statuses, statusErr, clusterErr, time.Now())...); err != nil {
		return err
	}

	if err := writeFindings(guestAgentFinding(deps.listConfig, deps.getStatus, statuses, statusErr, hypervisorInventory)); err != nil {
		return err
	}

	if err := writeFindings(cacheFindings(deps.listCache, clusters, clusterErr)...); err != nil {
		return err
	}

	if err := writeFindings(mirrorOfflineFinding(deps.mirrorOffline)); err != nil {
		return err
	}

	if err := writeFindings(egressFinding(probeFactoryEgress(deps.doHTTP))); err != nil {
		return err
	}
	if err := writeFindings(securityInventoryFindings(deps.command)...); err != nil {
		return err
	}

	if failed {
		return errors.New("one or more doctor checks failed")
	}
	return nil
}

func talosServicesFindings(statuses []daemon.ClusterStatus, statusErr, clusterErr error, now time.Time) []doctorFinding {
	if clusterErr != nil {
		detail := fmt.Sprintf("list clusters: %v", clusterErr)
		if isDaemonUnavailable(clusterErr) {
			detail = daemonUnavailableDetail(clusterErr)
		}
		return []doctorFinding{{level: "SKIP", check: "talos-services", detail: detail}}
	}
	if statusErr != nil {
		return []doctorFinding{{level: "WARN", check: "talos-services", detail: fmt.Sprintf("cluster status unavailable: %v", statusErr)}}
	}
	configured := 0
	var findings []doctorFinding
	for _, status := range statuses {
		if !status.Running {
			continue
		}
		for _, node := range status.Nodes {
			if !node.Phase.Configured() {
				continue
			}
			configured++
			if node.RebootedAt != nil {
				findings = append(findings, doctorFinding{level: "WARN", check: "talos-services", detail: fmt.Sprintf(
					"%s/%s rebooted at %s; Talos boot identity changed while the VM process stayed running; inspect recovery with: tbx console %s %s",
					status.Name, node.Name, node.RebootedAt.UTC().Format(time.RFC3339), shellquote.Quote(status.Name), shellquote.Quote(node.Name))})
			}
			clusterName := shellquote.Quote(status.Name)
			nodeName := shellquote.Quote(node.Name)
			stalledNames := make(map[string]struct{}, len(node.StalledServices))
			for _, stalled := range node.StalledServices {
				stalledNames[stalled.Service] = struct{}{}
				age := now.Sub(stalled.Since)
				if age < 0 {
					age = 0
				}
				findings = append(findings, doctorFinding{level: "FAIL", check: "talos-services", detail: fmt.Sprintf(
					"%s/%s %s %s for %s — image pull may be stalled; inspect with: tbx console %s %s; cold-restart with: tbx node stop %s %s && tbx node start %s %s",
					status.Name, node.Name, stalled.Service, stalled.State, age.Round(time.Second), clusterName, nodeName, clusterName, nodeName, clusterName, nodeName)})
			}
			for _, service := range node.Services {
				if _, alreadyReported := stalledNames[service.Name]; alreadyReported {
					continue
				}
				if !service.Degraded() {
					continue
				}
				detail := fmt.Sprintf("%s/%s %s %s (%s)", status.Name, node.Name, service.Name, service.State, service.Health)
				if service.Message != "" {
					detail += ": " + service.Message
				}
				findings = append(findings, doctorFinding{level: "FAIL", check: "talos-services", detail: detail})
			}
			switch {
			case node.ServiceProbe == nil:
				findings = append(findings, doctorFinding{level: "WARN", check: "talos-services", detail: fmt.Sprintf("%s/%s service probe result unavailable", status.Name, node.Name)})
			case node.ServiceProbe.Status == daemon.ServiceProbeMissingCredentials:
				findings = append(findings, doctorFinding{level: "WARN", check: "talos-services", detail: fmt.Sprintf("%s/%s Talos credentials missing: exact context %q was not found; run talosctl config merge <path-to-talosconfig>", status.Name, node.Name, status.Name)})
			case node.ServiceProbe.Status == daemon.ServiceProbeFailed:
				detail := node.ServiceProbe.Error
				if detail == "" {
					detail = "probe failed"
				}
				findings = append(findings, doctorFinding{level: "WARN", check: "talos-services", detail: fmt.Sprintf("%s/%s service probe failed: %s", status.Name, node.Name, detail)})
			}
		}
	}
	if configured == 0 {
		return []doctorFinding{{level: "SKIP", check: "talos-services", detail: "no configured running nodes"}}
	}
	if len(findings) == 0 {
		return []doctorFinding{{level: "PASS", check: "talos-services", detail: fmt.Sprintf("%d configured node(s) inspected", configured)}}
	}
	return findings
}

// cacheFindings reports the two stores that share the cache root and used to
// share the bare word "cache": the registry mirror's blob store and the Talos
// disk-image cache. They are named apart and reported on their own lines, so an
// empty mirror cache is never read as an empty image cache during offline prep.
func cacheFindings(
	listCache func() (daemon.CacheListResult, error),
	clusters []daemon.ClusterSummary,
	clusterErr error,
) []doctorFinding {
	mirrorFinding := doctorFinding{check: "mirror-health"}
	imageFinding := doctorFinding{check: "image-cache"}
	if listCache == nil {
		mirrorFinding.level, mirrorFinding.detail = "SKIP", "probe unavailable"
		imageFinding.level, imageFinding.detail = "SKIP", "probe unavailable"
		return []doctorFinding{mirrorFinding, imageFinding}
	}
	cacheResult, err := listCache()
	if isDaemonUnavailable(err) {
		mirrorFinding.level, mirrorFinding.detail = "SKIP", daemonUnavailableDetail(err)
		imageFinding.level, imageFinding.detail = "SKIP", daemonUnavailableDetail(err)
		return []doctorFinding{mirrorFinding, imageFinding}
	}
	if err != nil {
		// One listing feeds both lines, so a failed listing is one problem,
		// not two: it fails the mirror line that owns the health verdict and
		// skips the image line, which is informational and has nothing to
		// report (#269).
		mirrorFinding.level, mirrorFinding.detail = "FAIL", err.Error()
		imageFinding.level, imageFinding.detail = "SKIP", "cache listing unavailable"
		return []doctorFinding{mirrorFinding, imageFinding}
	}

	totalBytes := cacheResult.MirrorTotal.BlobBytes + cacheResult.MirrorTotal.ManifestBytes
	cacheDetail := fmt.Sprintf(
		"registry-mirror cache %d bytes (%d blob(s), %d manifest(s))",
		totalBytes,
		cacheResult.MirrorTotal.BlobCount,
		cacheResult.MirrorTotal.ManifestCount,
	)
	var expectedGateways []string
	for _, item := range clusters {
		if item.Running {
			expectedGateways = append(expectedGateways, cluster.Gateway(item.SubnetIndex))
		}
	}
	sort.Strings(expectedGateways)
	actualGateways := append([]string(nil), cacheResult.MirrorBoundGatewayIPs...)
	sort.Strings(actualGateways)
	switch {
	case clusterErr == nil && len(expectedGateways) == 0 && len(actualGateways) > 0:
		mirrorFinding.level = "FAIL"
		mirrorFinding.detail = fmt.Sprintf("mirror listeners bound on %v while no clusters are running; %s", actualGateways, cacheDetail)
	case clusterErr == nil && len(clusters) == 0:
		mirrorFinding.level = "SKIP"
		mirrorFinding.detail = fmt.Sprintf("no clusters exist; %s", cacheDetail)
	case clusterErr == nil && len(expectedGateways) == 0:
		mirrorFinding.level = "SKIP"
		mirrorFinding.detail = fmt.Sprintf("no clusters are running; %s", cacheDetail)
	case clusterErr == nil && stringSlicesEqual(actualGateways, expectedGateways):
		mirrorFinding.level = "PASS"
		mirrorFinding.detail = fmt.Sprintf("mirror serving on %d gateway(s) %v, %s", len(actualGateways), actualGateways, cacheDetail)
	case clusterErr == nil && len(actualGateways) == 0:
		mirrorFinding.level = "FAIL"
		mirrorFinding.detail = fmt.Sprintf("no mirror listeners bound for running cluster(s); expected %v; %s", expectedGateways, cacheDetail)
	case clusterErr == nil:
		mirrorFinding.level = "FAIL"
		mirrorFinding.detail = fmt.Sprintf(
			"mirror listeners bound on %v, expected %v; %s",
			actualGateways,
			expectedGateways,
			cacheDetail,
		)
	default:
		mirrorFinding.level = "SKIP"
		mirrorFinding.detail = fmt.Sprintf("cluster state unavailable; %s", cacheDetail)
	}

	imageFinding.level, imageFinding.detail = imageCacheFinding(cacheResult.Images)
	return []doctorFinding{mirrorFinding, imageFinding}
}

// imageCacheFinding sizes the Talos disk-image cache the way `cache list` does:
// apparent bytes, plus what the sparse images actually occupy when the daemon
// reports it. Incomplete combinations — prunable leftovers with no usable
// disk.raw — are held out of the total and counted apart, because an operator
// preparing to go offline must never read a half-finished pull as a warm image
// (#269). A cache holding nothing but leftovers warns for the same reason.
func imageCacheFinding(images []daemon.CacheImageEntry) (level, detail string) {
	var imageBytes, allocatedBytes int64
	usable, incomplete := 0, 0
	for _, entry := range images {
		if entry.Incomplete {
			incomplete++
			continue
		}
		usable++
		imageBytes += entry.Size
		allocatedBytes += entry.AllocatedSize
	}
	counts := fmt.Sprintf("%d image(s)", usable)
	if incomplete > 0 {
		counts += fmt.Sprintf(", %d incomplete", incomplete)
	}
	level = "PASS"
	if usable == 0 && incomplete > 0 {
		level = "WARN"
	}
	if allocatedBytes > 0 {
		return level, fmt.Sprintf("image cache %d bytes (%d bytes on disk, %s)", imageBytes, allocatedBytes, counts)
	}
	return level, fmt.Sprintf("image cache %d bytes (%s)", imageBytes, counts)
}

// guestAgentFinding reports the capability gate for clusters that baked
// qemu-guest-agent. The gate is a warning, never a failure: the config is valid
// and portable, the extension is simply inert on this host.
func guestAgentFinding(
	listConfig func() ([]cluster.Cluster, error),
	getStatus func() ([]daemon.ClusterStatus, error),
	statuses []daemon.ClusterStatus,
	statusErr error,
	inventory doctorHypervisorInventory,
) doctorFinding {
	finding := doctorFinding{check: "guest-agent"}
	if listConfig == nil {
		finding.level, finding.detail = "SKIP", "probe unavailable"
		return finding
	}
	items, err := listConfig()
	if err != nil {
		finding.level, finding.detail = "SKIP", fmt.Sprintf("cluster state unavailable: %v", err)
		return finding
	}
	var requesting []string
	for _, item := range items {
		if extensions.Requested(item.TalosExtensions, extensions.GuestAgent) {
			requesting = append(requesting, item.Name)
		}
	}
	if len(requesting) == 0 {
		finding.level, finding.detail = "SKIP", "no cluster requests "+extensions.GuestAgent
		return finding
	}
	if len(statuses) == 0 && statusErr == nil && getStatus != nil {
		statuses, statusErr = getStatus()
	}

	statusByName := make(map[string]daemon.ClusterStatus, len(statuses))
	for _, status := range statuses {
		statusByName[status.Name] = status
	}
	supported := make([]string, 0, len(requesting))
	unsupported := make(map[string][]string)
	for _, item := range items {
		if !extensions.Requested(item.TalosExtensions, extensions.GuestAgent) {
			continue
		}
		support, ok := guestAgentSupportForCluster(item, statusByName[item.Name], inventory)
		if !ok {
			reason := "probe unavailable"
			if statusErr != nil {
				reason = "cluster status unavailable: " + statusErr.Error()
			} else if inventory.err != nil {
				reason = "hypervisor inventory unavailable: " + inventory.err.Error()
			}
			unsupported[reason] = append(unsupported[reason], item.Name)
			continue
		}
		if support.Supported {
			supported = append(supported, item.Name)
			continue
		}
		reason := support.Reason
		if reason == "" {
			reason = "backend has no guest-agent channel"
		}
		unsupported[reason] = append(unsupported[reason], item.Name)
	}
	if len(supported) == 0 && len(unsupported) == 0 {
		finding.level, finding.detail = "SKIP", "probe unavailable"
		return finding
	}
	details := make([]string, 0, 1+len(unsupported))
	if len(supported) != 0 {
		sort.Strings(supported)
		details = append(details, fmt.Sprintf("channel available for cluster(s) %s", strings.Join(supported, ", ")))
	}
	for _, reason := range sortedMapKeys(unsupported) {
		names := unsupported[reason]
		sort.Strings(names)
		details = append(details, fmt.Sprintf("cluster(s) %s request %s: %s", strings.Join(names, ", "), extensions.GuestAgent, reason))
	}
	finding.level = "PASS"
	if len(unsupported) != 0 {
		finding.level = "WARN"
	}
	finding.detail = strings.Join(details, "; ")
	return finding
}

const hypervisorProbeTimeout = 2 * time.Second

var (
	doctorGOOS   = runtime.GOOS
	doctorGOARCH = runtime.GOARCH
)

// hypervisorPlatformTag labels support tiers that are a property of the
// platform rather than of a probe. platform is the GOOS/GOARCH pair of the
// process that owns the hypervisors: the daemon's when the inventory came over
// the wire, this client's for a local probe.
func hypervisorPlatformTag(platform string, name string) string {
	if platform == "darwin/amd64" && name == string(hypervisor.NameQEMU) {
		return "best-effort platform"
	}
	return ""
}

type doctorHypervisorInventory struct {
	// platform is the inventory owner's GOOS/GOARCH pair (see
	// hypervisorPlatformTag). Empty when an older daemon did not report one.
	platform      string
	items         []daemon.HypervisorInfo
	defaultName   hypervisor.Name
	defaultSource hypervisor.DefaultSource
	compiledName  hypervisor.Name
	localProbeTag string
	err           error
}

func doctorHypervisors(deps doctorDependencies) doctorHypervisorInventory {
	if deps.daemonInfo == nil {
		return doctorHypervisorInventory{err: errors.New("daemon hypervisor inventory probe unavailable")}
	}
	info, err := deps.daemonInfo()
	if err == nil && len(info.Hypervisors) > 0 {
		// A daemon predating Info.Platform reports none; fall back to this
		// client's runtime, the pre-platform-field behaviour.
		platform := info.Platform
		if platform == "" {
			platform = doctorGOOS + "/" + doctorGOARCH
		}
		inventory := doctorHypervisorInventory{
			platform:      platform,
			items:         info.Hypervisors,
			defaultName:   info.DefaultHypervisor,
			defaultSource: info.DefaultHypervisorSource,
			compiledName:  info.CompiledDefaultHypervisor,
		}
		if inventory.defaultSource == hypervisor.DefaultSourceCompiled {
			inventory.compiledName = inventory.defaultName
		}
		return inventory
	}
	if deps.hypervisors == nil {
		if err == nil {
			err = errors.New("daemon does not report hypervisor inventory")
		}
		return doctorHypervisorInventory{err: err}
	}
	localProbeTag := "probed locally; daemon unavailable"
	switch {
	case err == nil:
		localProbeTag = "probed locally; daemon does not report hypervisor inventory"
	case !isDaemonUnavailable(err):
		localProbeTag = "probed locally; daemon info unavailable: " + err.Error()
	}
	probeCtx, probeCancel := context.WithTimeout(context.Background(), hypervisorProbeTimeout)
	defer probeCancel()
	registry, probeErr := deps.hypervisors(probeCtx)
	if probeErr != nil {
		return doctorHypervisorInventory{err: probeErr}
	}
	items := make([]daemon.HypervisorInfo, 0, len(registry.Backends))
	for _, name := range registry.Names() {
		entry := registry.Backends[name]
		item := daemon.HypervisorInfo{
			Name:                    name,
			Available:               entry.Availability.Available,
			AvailabilityReason:      entry.Availability.Reason,
			AvailabilityRemediation: entry.Availability.Remediation,
		}
		if entry.Availability.Available && entry.Hypervisor != nil {
			capabilities := entry.Hypervisor.Capabilities()
			item.BalloonReadback = daemon.NewFeatureStatusInfo(capabilities.BalloonReadback)
			suspend := daemon.NewFeatureStatusInfo(capabilities.Suspend)
			item.Suspend = &suspend
			item.SuspendSurvivesDaemonRestart = capabilities.SuspendSurvivesDaemonRestart
			item.GuestAgent = daemon.NewFeatureStatusInfo(capabilities.GuestAgent)
		}
		items = append(items, item)
	}
	return doctorHypervisorInventory{
		platform: doctorGOOS + "/" + doctorGOARCH,
		items:    items, defaultName: registry.Default.Name, defaultSource: registry.Default.Source, compiledName: registry.CompiledDefault, localProbeTag: localProbeTag,
	}
}

func hypervisorFindings(inventory doctorHypervisorInventory) []doctorFinding {
	items := inventory.items
	if len(items) == 0 {
		detail := "inventory unavailable"
		if inventory.err != nil {
			detail += ": " + inventory.err.Error()
		}
		return []doctorFinding{{level: "INFO", check: "Hypervisors", detail: detail}}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	findings := make([]doctorFinding, 0, len(items))
	for _, item := range items {
		availability := "available"
		if tag := hypervisorPlatformTag(inventory.platform, string(item.Name)); item.Available && tag != "" {
			availability += " (" + tag + ")"
		}
		balloonReadback := featureGate(item.BalloonReadback)
		// A daemon predating the suspend gate omits the field; that is not
		// evidence of "unsupported", so say so.
		suspend := "unknown"
		if item.Suspend != nil {
			suspend = featureGate(*item.Suspend)
		}
		suspendRestart := "unsupported"
		if item.SuspendSurvivesDaemonRestart {
			suspendRestart = "supported"
		}
		guestAgent := featureGate(item.GuestAgent)
		if !item.Available {
			availability = "unavailable"
			details := make([]string, 0, 2)
			if item.AvailabilityReason != "" {
				details = append(details, item.AvailabilityReason)
			}
			if item.AvailabilityRemediation != "" {
				details = append(details, "remediation: "+item.AvailabilityRemediation)
			}
			if len(details) != 0 {
				availability += " (" + strings.Join(details, "; ") + ")"
			}
			balloonReadback, suspend, suspendRestart, guestAgent = "unavailable", "unavailable", "unavailable", "unavailable"
		}
		defaultStatus := "no"
		if item.Name == inventory.defaultName {
			defaultStatus = "yes (source=" + string(inventory.defaultSource) + ")"
		}
		detail := fmt.Sprintf("%s: availability=%s; default=%s; balloon-readback=%s; suspend=%s; suspend-survives-restart=%s; guest-agent=%s", item.Name, availability, defaultStatus, balloonReadback, suspend, suspendRestart, guestAgent)
		if inventory.localProbeTag != "" {
			detail += "; " + inventory.localProbeTag
		}
		findings = append(findings, doctorFinding{level: "INFO", check: "Hypervisors", detail: detail})
	}
	return findings
}

func featureGate(status daemon.FeatureStatusInfo) string {
	if status.Supported {
		return "supported"
	}
	if status.Reason != "" {
		return "unsupported (" + status.Reason + ")"
	}
	return "unsupported"
}

func guestAgentSupportFromInventory(name hypervisor.Name, inventory doctorHypervisorInventory) (hypervisor.FeatureStatus, bool) {
	for _, item := range inventory.items {
		if item.Name != name {
			continue
		}
		if !item.Available {
			reason := item.AvailabilityReason
			if reason == "" {
				reason = "backend is unavailable"
			}
			return hypervisor.FeatureStatus{Reason: reason}, true
		}
		return hypervisor.FeatureStatus{Supported: item.GuestAgent.Supported, Reason: item.GuestAgent.Reason}, true
	}
	return hypervisor.FeatureStatus{}, false
}

func guestAgentSupportFromStatus(status daemon.ClusterStatus) (hypervisor.FeatureStatus, bool) {
	for _, capability := range status.Capabilities {
		if capability.Name == extensions.GuestAgent {
			return hypervisor.FeatureStatus{Supported: capability.Supported, Reason: capability.Reason}, true
		}
	}
	return hypervisor.FeatureStatus{}, false
}

func guestAgentSupportForCluster(item cluster.Cluster, status daemon.ClusterStatus, inventory doctorHypervisorInventory) (hypervisor.FeatureStatus, bool) {
	if support, ok := guestAgentSupportFromStatus(status); ok {
		return support, true
	}
	name := status.Hypervisor
	if name == "" {
		name = hypervisor.Name(item.Hypervisor)
	}
	if name == "" {
		name = inventory.compiledName
		if name == "" && inventory.defaultSource == hypervisor.DefaultSourceCompiled {
			name = inventory.defaultName
		}
	}
	if name == "" {
		return hypervisor.FeatureStatus{}, false
	}
	return guestAgentSupportFromInventory(name, inventory)
}

func sortedMapKeys(values map[string][]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func stringSlicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func daemonUnavailableDetail(err error) string {
	return fmt.Sprintf("daemon unavailable: %v", err)
}

func isDaemonUnavailable(err error) bool {
	if err == nil {
		return false
	}
	var connectionError dialError
	return errors.As(err, &connectionError)
}

func (c cli) doctorDependencies() doctorDependencies {
	command := execCombinedOutput
	call := c.doctorCaller()
	deps := doctorDependencies{
		runtimeIdentity: c.collectRuntimeIdentity,
		checkHelper:     checkHelper,
		checkResolver:   checkResolver,
		checkDirectDNS:  checkPlatformDirectDNS,
		checkForwarding: checkForwarding,
		listConfig:      cluster.List,
		daemonInfo: func() (daemon.Info, error) {
			var result daemon.Info
			err := call("daemon.info", struct{}{}, &result)
			return result, err
		},
		hypervisors: hypervisor.NewAllFromEnvironment,
		listClusters: func() ([]daemon.ClusterSummary, error) {
			var result []daemon.ClusterSummary
			err := call("cluster.list", struct{}{}, &result)
			return result, err
		},
		getStatus: func() ([]daemon.ClusterStatus, error) {
			var result []daemon.ClusterStatus
			err := call("status", map[string]string{"cluster": ""}, &result)
			return result, err
		},
		listCache: func() (daemon.CacheListResult, error) {
			var result daemon.CacheListResult
			err := call("cache.list", struct{}{}, &result)
			return result, err
		},
		// bounded like every other diagnostic subprocess: a stalled vm_stat
		// must not hang doctor past its exit code
		hostFreeMemory: func() (int, error) {
			ctx, cancel := context.WithTimeout(context.Background(), commandProbeTimeout)
			defer cancel()
			return balloon.HostFreeMiBContext(ctx)
		},
		mirrorOffline: func() (bool, error) {
			var result daemon.MirrorOfflineStatus
			err := call("mirror.offline.get", struct{}{}, &result)
			return result.Enabled, err
		},
		hostPressure: func() (hostpressure.Snapshot, error) {
			home, err := os.UserHomeDir()
			if err != nil {
				return hostpressure.Snapshot{}, fmt.Errorf("find home directory: %w", err)
			}
			return hostpressure.SystemSnapshot(filepath.Join(home, ".talosbox"))
		},
		command:  command,
		readFile: os.ReadFile,
		accessRW: func(path string) error {
			file, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				return err
			}
			return file.Close()
		},
		listenPacket: func(network, address string) (net.PacketConn, error) {
			return net.ListenPacket(network, address)
		},
		listenStream: func(network, address string) (io.Closer, error) {
			return net.Listen(network, address)
		},
		doHTTP:    newDoctorHTTPClient().Do,
		doVIPHTTP: newDoctorVIPHTTPClient().Do,
	}
	platformDoctorDependencies(&deps)
	return deps
}

// doctorCallTimeout bounds the daemon's silence on each diagnostic RPC, not
// the RPC itself: doctor asks for narration, so a status walking many nodes
// re-arms the deadline on every probe it reports and a request queued behind
// the operation lock re-arms it on every queue heartbeat — which is why the
// bound must outlast that heartbeat. Only a daemon that accepts the socket
// and then says nothing trips it (#392 for the mechanism).
var doctorCallTimeout = 2 * daemon.OpWaitNarrationInterval

// doctorCaller is doctorCall with memory: once the daemon has gone silent,
// every later diagnostic fails fast with that error instead of each waiting
// out doctorCallTimeout, so a stalled daemon costs the report one wait.
func (c cli) doctorCaller() func(op string, args, destination any) error {
	var mu sync.Mutex
	var stalled error
	return func(op string, args, destination any) error {
		mu.Lock()
		err := stalled
		mu.Unlock()
		if err != nil {
			return err
		}
		err = c.doctorCall(op, args, destination)
		if isTimeout(err) {
			mu.Lock()
			stalled = err
			mu.Unlock()
		}
		return err
	}
}

// doctorCall deliberately uses exchangeWithin directly instead of cli.call:
// diagnostics must report an absent daemon as SKIP, not start one as a side
// effect, and must finish even when the daemon stalls.
func (c cli) doctorCall(op string, args, destination any) error {
	socketPath, err := daemon.SocketPath()
	if err != nil {
		return err
	}
	response, err := exchangeDeadlined(socketPath, op, args, doctorCallTimeout, func(string) {})
	if err != nil {
		return err
	}
	if !response.OK {
		if response.Error == "" {
			return errors.New("daemon operation failed")
		}
		return errors.New(response.Error)
	}
	if destination != nil && len(response.Data) > 0 {
		if err := json.Unmarshal(response.Data, destination); err != nil {
			return fmt.Errorf("decode daemon result: %w", err)
		}
	}
	return nil
}

func checkHelper() error {
	client, err := helper.Connect()
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	return client.Ping()
}
