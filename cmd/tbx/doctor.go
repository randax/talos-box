package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/randax/talos-box/internal/balloon"
	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/daemon"
	"github.com/randax/talos-box/internal/extensions"
	"github.com/randax/talos-box/internal/helper"
	"github.com/randax/talos-box/internal/hostpressure"
	"github.com/randax/talos-box/internal/hypervisor"
)

type doctorDependencies struct {
	checkHelper     func() error
	checkResolver   func() error
	checkDirectDNS  func() error
	checkForwarding func() error
	listClusters    func() ([]daemon.ClusterSummary, error)
	listConfig      func() ([]cluster.Cluster, error)
	getStatus       func() ([]daemon.ClusterStatus, error)
	listCache       func() (daemon.CacheListResult, error)
	hostPressure    func() (hostpressure.Snapshot, error)
	// hostFreeMemory is the free-memory reading the provision-start gate
	// refuses on. The host-pressure snapshot does not carry it — Assess never
	// needed it — but the check is not self-attesting without it (#420). Nil
	// reports free memory as unmeasured rather than guessing.
	hostFreeMemory func() (int, error)
	// guestAgentSupport is the host capability, not a running backend's, so the
	// gate is explained even with the daemon down.
	guestAgentSupport func() hypervisor.FeatureStatus
	command           commandOutput
	readFile          func(string) ([]byte, error)
	accessRW          func(string) error
	listenPacket      func(string, string) (net.PacketConn, error)
	listenStream      func(string, string) (io.Closer, error)
	doHTTP            httpDo
	platform          func() []doctorFinding
}

// doctorHelp answers `tbx doctor --help`. It names every check the command can
// report and states the exit-code contract, which is the fact a script needs
// and used to live only in the platform docs (#419).
func doctorHelp() string {
	var b strings.Builder
	b.WriteString(`tbx doctor checks whether this host can run talos-box clusters, and whether the
clusters that already exist are wired up. Every check is read-only: doctor
reports, it never repairs. One line per check, each PASS, WARN, FAIL, INFO, or
SKIP (the probe did not apply here).

Checks:
  helper              privileged helper is installed and answers
  resolver            per-domain resolver wiring for the cluster domains
  DNS                 cluster names resolve against the daemon's DNS directly
  forwarding          host IP forwarding for cluster traffic
  host-pressure       host memory and disk headroom (the same gate that blocks
                      cluster create)
  system-dns          the system resolver returns each cluster's domain
  routes              host routes reach the nodes of running clusters
  guest-agent         host support for clusters that baked qemu-guest-agent
  mirror-health       registry-mirror listeners match the running clusters
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

	failed := false
	writeFindings := func(findings ...doctorFinding) error {
		for _, finding := range findings {
			if finding.level == "FAIL" {
				failed = true
			}
			if finding.detail == "" {
				if _, err := fmt.Fprintf(c.out, "%s %s\n", finding.level, finding.check); err != nil {
					return err
				}
				continue
			}
			if _, err := fmt.Fprintf(c.out, "%s %s: %s\n", finding.level, finding.check, finding.detail); err != nil {
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
		{name: "forwarding", run: deps.checkForwarding},
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
	} else if findings := hostpressure.Assess(snapshot); len(findings) == 0 {
		// A PASS states the three numbers it was decided on — free memory, swap
		// in use, free space on the ~/.talosbox volume — plus the headroom the
		// provision-start gate will measure the next bringup against, so the
		// verdict can be attested without re-running memory_pressure, sysctl and
		// df by hand, and so a host cannot read PASS here and still be refused a
		// second cluster without warning (#420, #397).
		snapshot.FreeMemoryMiB = doctorFreeMemoryMiB(deps)
		if err := writeFindings(doctorFinding{
			level: "PASS", check: "host-pressure", detail: snapshot.Summary(balloon.DefaultConfig().ReserveMiB),
		}); err != nil {
			return err
		}
	} else {
		// Same classification the daemon's start gate uses: a blocking finding
		// is what makes cluster create refuse, so it fails here too.
		for _, finding := range findings {
			level := "WARN"
			if finding.Severity == hostpressure.SeverityBlock {
				level = "FAIL"
			}
			if err := writeFindings(doctorFinding{
				level: level, check: "host-pressure", detail: finding.DoctorDetail(),
			}); err != nil {
				return err
			}
		}
	}

	clusters, clusterErr := deps.listClusters()
	anyRunning := false
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
			statuses, statusErr := deps.getStatus()
			routeFinding := doctorFinding{level: "PASS", check: "routes"}
			var routeProblems []string
			if statusErr != nil {
				routeProblems = append(routeProblems,
					fmt.Sprintf("cluster status unavailable; node routes could not be checked: %v", statusErr))
			}
			if err := checkClusterRoutes(clusters, statuses, deps.command); err != nil {
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
			if err := writeFindings(interClusterFinding(statuses, statusErr, deps.doHTTP)); err != nil {
				return err
			}
		}
	}

	if err := writeFindings(guestAgentFinding(deps.listConfig, deps.guestAgentSupport)); err != nil {
		return err
	}

	if err := writeFindings(cacheFindings(deps.listCache, clusters, clusterErr)...); err != nil {
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
	support func() hypervisor.FeatureStatus,
) doctorFinding {
	finding := doctorFinding{check: "guest-agent"}
	if listConfig == nil || support == nil {
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
	if status := support(); !status.Supported {
		finding.level = "WARN"
		finding.detail = fmt.Sprintf("cluster(s) %s request %s: %s", strings.Join(requesting, ", "), extensions.GuestAgent, status.Reason)
		return finding
	}
	finding.level = "PASS"
	finding.detail = fmt.Sprintf("channel available for cluster(s) %s", strings.Join(requesting, ", "))
	return finding
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
	deps := doctorDependencies{
		checkHelper:       checkHelper,
		checkResolver:     checkResolver,
		checkDirectDNS:    checkPlatformDirectDNS,
		checkForwarding:   checkForwarding,
		listConfig:        cluster.List,
		guestAgentSupport: hypervisor.GuestAgentSupport,
		listClusters: func() ([]daemon.ClusterSummary, error) {
			var result []daemon.ClusterSummary
			err := c.doctorCall("cluster.list", struct{}{}, &result)
			return result, err
		},
		getStatus: func() ([]daemon.ClusterStatus, error) {
			var result []daemon.ClusterStatus
			err := c.doctorCall("status", map[string]string{"cluster": ""}, &result)
			return result, err
		},
		listCache: func() (daemon.CacheListResult, error) {
			var result daemon.CacheListResult
			err := c.doctorCall("cache.list", struct{}{}, &result)
			return result, err
		},
		hostFreeMemory: balloon.HostFreeMiB,
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
		doHTTP: newDoctorHTTPClient().Do,
	}
	platformDoctorDependencies(&deps)
	return deps
}

// doctorCall deliberately uses exchange directly instead of cli.call: diagnostics
// must report an absent daemon as SKIP, not start one as a side effect.
func (c cli) doctorCall(op string, args, destination any) error {
	socketPath, err := daemon.SocketPath()
	if err != nil {
		return err
	}
	response, err := exchange(socketPath, op, args)
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
