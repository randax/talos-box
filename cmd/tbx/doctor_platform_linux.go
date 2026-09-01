//go:build linux

package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/daemon"
	tbxdns "github.com/randax/talos-box/internal/dns"
	"github.com/randax/talos-box/internal/helper"
	"github.com/randax/talos-box/internal/wsl"
)

const (
	doctorKVMDevice                          = "/dev/kvm"
	doctorOSReleaseFile                      = "/proc/sys/kernel/osrelease"
	doctorHelperSocketUnit                   = "tbx-helper.socket"
	doctorHelperServiceUnit                  = "tbx-helper.service"
	doctorBridgeNFCall                       = "/proc/sys/net/bridge/bridge-nf-call-iptables"
	doctorForwardPolicyFix                   = "sudo iptables -I FORWARD 1 -i br-tbx+ -j ACCEPT && sudo iptables -I FORWARD 1 -o br-tbx+ -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT"
	doctorForwardPolicyInspect               = "sudo iptables -S FORWARD"
	doctorNFTForwardPolicyInspect            = "sudo nft list chain ip filter FORWARD"
	doctorHelperGroupFix                     = "sudo usermod -aG tbx $USER"
	doctorKVMGroupFix                        = "sudo usermod -aG kvm $USER"
	doctorRPFilterLooseFix                   = "sudo sysctl -w net.ipv4.conf.all.rp_filter=2"
	doctorIPTablesResourceProblemExit        = 4
	doctorQEMUMinimumMajor                   = 6
	doctorQEMUMinimumMinor                   = 2
	doctorQEMUSuspendMajor                   = 8
	doctorQEMUSuspendMinor                   = 2
	doctorHelperCapabilityMask        uint64 = 1<<10 | 1<<12 | 1<<13
)

type doctorQEMUSystem struct {
	binary  string
	machine string
}

type doctorQEMUVersion struct {
	major int
	minor int
	patch int
}

type doctorSocketListener struct {
	line    string
	process string
}

type helperCapabilityReport struct {
	Effective      uint64
	EffectiveNames []string
}

func (v doctorQEMUVersion) compare(other doctorQEMUVersion) int {
	switch {
	case v.major != other.major:
		return doctorCompareInt(v.major, other.major)
	case v.minor != other.minor:
		return doctorCompareInt(v.minor, other.minor)
	default:
		return doctorCompareInt(v.patch, other.patch)
	}
}

func (v doctorQEMUVersion) String() string { return fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.patch) }

func doctorCompareInt(left, right int) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}

// platformDoctorCheckNames names the Linux-only checks, in the order
// linuxPlatformDoctorFindings runs them, so `tbx doctor --help` lists exactly
// what this platform reports (#419).
func platformDoctorCheckNames() []string {
	return linuxPlatformDoctorCheckNames(wsl.GenerationFromOSRelease(doctorOSRelease(os.ReadFile)))
}

func linuxPlatformDoctorCheckNames(generation wsl.Generation) []string {
	names := []string{
		"kvm", "qemu", "bridge-netfilter", "bridge-stp", "rp-filter",
		"port-53", "port-67", "port-179",
		"helper-unit", "helper-access", "helper-capabilities",
	}
	if generation == wsl.NotWSL {
		return names
	}
	return append([]string{"wsl"}, names...)
}

func platformDoctorDependencies(deps *doctorDependencies) {
	if deps == nil {
		return
	}
	runningClusters := func() ([]cluster.Cluster, error) {
		return runningLinuxClusters(deps.listConfig, deps.listClusters)
	}
	deps.checkResolver = func() error {
		return checkLinuxResolver(runningClusters, deps.readFile, deps.command)
	}
	deps.checkDirectDNS = func() error {
		return checkLinuxDirectDNS(runningClusters, deps.readFile, tbxdns.Probe)
	}
	if deps.wslIdentity == nil {
		detector := wsl.SystemDetector(wsl.ReadFile(deps.readFile), wsl.Command(deps.command))
		deps.wslIdentity = sync.OnceValue(func() wsl.Identity { return wsl.Detect(detector) })
	}
	deps.platform = func() []doctorFinding {
		return linuxPlatformDoctorFindings(*deps, helperCapabilityReportFromHelper)
	}
}

func runningLinuxClusters(
	listConfig func() ([]cluster.Cluster, error),
	listRuntime func() ([]daemon.ClusterSummary, error),
) ([]cluster.Cluster, error) {
	configured, err := listConfig()
	if err != nil {
		return nil, fmt.Errorf("list configured clusters: %w", err)
	}
	runtimeClusters, err := listRuntime()
	if isDaemonUnavailable(err) {
		return nil, skippedDoctorCheck{detail: daemonUnavailableDetail(err)}
	}
	if err != nil {
		return nil, fmt.Errorf("list running clusters: %w", err)
	}
	running := make(map[string]bool, len(runtimeClusters))
	for _, item := range runtimeClusters {
		running[item.Name] = item.Running
	}
	result := make([]cluster.Cluster, 0, len(configured))
	for _, item := range configured {
		if running[item.Name] {
			result = append(result, item)
		}
	}
	return result, nil
}

func checkResolver() error {
	return checkLinuxResolver(cluster.List, os.ReadFile, execCombinedOutput)
}

func checkLinuxResolver(
	listClusters func() ([]cluster.Cluster, error),
	readFile func(string) ([]byte, error),
	command commandOutput,
) error {
	clusters, err := listClusters()
	if err != nil {
		return fmt.Errorf("list clusters: %w", err)
	}
	if len(clusters) == 0 {
		// Nothing was probed, so there is nothing to pass: a PASS here reads as
		// "cluster names resolve", which is exactly the claim doctor must not
		// make without evidence (#447).
		return skippedDoctorCheck{detail: "no clusters are running"}
	}
	// Separate "resolved is not there at all" from "resolved is there but this
	// link is unregistered": the first is the host-DNS-unavailable WARN the
	// daemon already logs, with the same manual step and the by-gateway
	// fallback (#447).
	if _, err := command("resolvectl", "status"); err != nil {
		return optionalHostDNSError{detail: hostDNSUnavailableDetail(err, clusters)}
	}
	var missing []string
	for _, item := range clusters {
		bridge := cluster.LinuxBridgeName(item.SubnetIndex)
		if _, err := readFile("/sys/class/net/" + bridge + "/ifindex"); err != nil {
			return fmt.Errorf("inspect %s: %w", bridge, err)
		}
		output, err := command("resolvectl", "status", bridge)
		if err != nil {
			return optionalHostDNSError{
				detail: fmt.Sprintf(
					"systemd-resolved is reachable, but %s is not registered: %v; manual step: %s; fallback: %s",
					bridge,
					err,
					strings.Join(resolvedManualSteps(clusters), "; "),
					strings.Join(resolvedDigFallbacks(clusters), "; "),
				),
			}
		}
		domain := "~" + item.EffectiveDomain()
		if resolvedLinkHasGatewayAndDomain(output, cluster.Gateway(item.SubnetIndex), domain) {
			continue
		}
		missing = append(missing, fmt.Sprintf("%s is missing DNS server %s or route-only domain %s", bridge, cluster.Gateway(item.SubnetIndex), domain))
	}
	if len(missing) == 0 {
		return nil
	}
	return optionalHostDNSError{
		detail: fmt.Sprintf(
			"systemd-resolved is reachable, but %s; manual step: %s; fallback: %s",
			strings.Join(missing, "; "),
			strings.Join(resolvedManualSteps(clusters), "; "),
			strings.Join(resolvedDigFallbacks(clusters), "; "),
		),
	}
}

func resolvedLinkHasGatewayAndDomain(output []byte, gateway, domain string) bool {
	text := string(output)
	return strings.Contains(text, gateway) && strings.Contains(text, domain)
}

func linuxPlatformDoctorFindings(
	deps doctorDependencies,
	helperCaps func() (helperCapabilityReport, error),
) []doctorFinding {
	// Establish the WSL snapshot before any Linux capability probe. Every
	// later consumer in this doctor run shares the same OnceValue (#553).
	identity := doctorWSLIdentity(deps)
	findings := []doctorFinding{
		linuxKVMFinding(deps.accessRW, identity.KernelRelease),
		linuxQEMUFinding(deps.command),
		linuxBridgeNetfilterFinding(deps.readFile, deps.command),
		linuxBridgeSTPFinding(deps.listConfig, deps.readFile),
		linuxRPFilterFinding(deps.command, deps.readFile),
		linuxPortFinding(53, "udp", deps.listConfig, deps.readFile, deps.command, deps.listenPacket, deps.listenStream),
		linuxPortFinding(67, "udp", deps.listConfig, deps.readFile, deps.command, deps.listenPacket, deps.listenStream),
		linuxPortFinding(179, "tcp", deps.listConfig, deps.readFile, deps.command, deps.listenPacket, deps.listenStream),
		linuxHelperUnitFinding(deps.command),
		linuxHelperAccessFinding(deps.command, identity.KernelRelease),
		linuxHelperCapabilitiesFinding(helperCaps),
	}
	if finding, ok := wslDoctorFinding(identity); ok {
		findings = append([]doctorFinding{finding}, findings...)
	}
	return findings
}

// doctorOSRelease reads the running kernel's release string. An unreadable
// file is not a finding of its own: it only costs the remediation its WSL
// branch, so it degrades to the empty string.
func doctorOSRelease(readFile func(string) ([]byte, error)) string {
	if readFile == nil {
		return ""
	}
	content, err := readFile(doctorOSReleaseFile)
	if err != nil {
		return ""
	}
	return string(content)
}

// linuxSessionRefreshHint names the step that actually applies a new group
// membership on this host. "Log out and back in" is wrong under WSL — there is
// no login session to cycle — and insufficient when the user lingers
// (`loginctl enable-linger`), because the lingering session outlives every
// logout and keeps handing out the old supplementary groups (#468).
func linuxSessionRefreshHint(osrelease string) string {
	if strings.Contains(strings.ToLower(osrelease), "microsoft") {
		return "run `wsl --shutdown` from Windows, then reopen the distro"
	}
	return "log out and back in (a lingering user session — `loginctl enable-linger` — needs `loginctl terminate-user $USER`)"
}

func linuxKVMFinding(accessRW func(string) error, osrelease string) doctorFinding {
	finding := doctorFinding{level: "PASS", check: "kvm"}
	if accessRW == nil {
		finding.level, finding.detail = "FAIL", "KVM probe is unavailable"
		return finding
	}
	if err := accessRW(doctorKVMDevice); err != nil {
		finding.level = "FAIL"
		if errors.Is(err, os.ErrNotExist) {
			finding.detail = "/dev/kvm is missing; enable hardware virtualization and load the KVM kernel modules"
			return finding
		}
		finding.detail = fmt.Sprintf("%s is not readable+writable by this user: %v; add your user to the kvm group with `%s`, then %s", doctorKVMDevice, err, doctorKVMGroupFix, linuxSessionRefreshHint(osrelease))
	}
	return finding
}

func linuxQEMUFinding(command commandOutput) doctorFinding {
	finding := doctorFinding{check: "qemu"}
	system, err := doctorQEMUSystemForArchitecture(runtime.GOARCH)
	if err != nil {
		finding.level, finding.detail = "FAIL", err.Error()
		return finding
	}
	output, err := command(system.binary, "--version")
	if err != nil {
		finding.level = "FAIL"
		finding.detail = fmt.Sprintf("%s is unavailable: %v; install the QEMU package that provides %s", system.binary, err, system.binary)
		return finding
	}
	version, err := parseDoctorQEMUVersion(output)
	if err != nil {
		finding.level, finding.detail = "FAIL", fmt.Sprintf("parse %s --version: %v; repair or reinstall the QEMU package that provides %s", system.binary, err, system.binary)
		return finding
	}
	machines, err := command(system.binary, "-machine", "help")
	if err != nil {
		finding.level, finding.detail = "FAIL", fmt.Sprintf("inspect %s machine types: %v; repair or reinstall the QEMU package that provides %s", system.binary, err, system.binary)
		return finding
	}
	requiredVersion := doctorQEMUVersion{major: doctorQEMUMinimumMajor, minor: doctorQEMUMinimumMinor}
	if version.compare(requiredVersion) < 0 {
		finding.level, finding.detail = "FAIL", fmt.Sprintf("QEMU >= %d.%d is required (found %s)", doctorQEMUMinimumMajor, doctorQEMUMinimumMinor, version)
		return finding
	}
	if !qemuMachinePresent(machines, system.machine) {
		finding.level = "FAIL"
		finding.detail = fmt.Sprintf("QEMU %s does not provide required machine type %q", version, system.machine)
		return finding
	}
	suspendVersion := doctorQEMUVersion{major: doctorQEMUSuspendMajor, minor: doctorQEMUSuspendMinor}
	if version.compare(suspendVersion) < 0 {
		finding.level = "WARN"
		finding.detail = fmt.Sprintf(
			"%s %s is usable, but suspend requires QEMU >= %d.%d (found %d.%d) — upgrade to Ubuntu 24.04+",
			system.binary,
			version,
			doctorQEMUSuspendMajor,
			doctorQEMUSuspendMinor,
			version.major,
			version.minor,
		)
		return finding
	}
	finding.level = "PASS"
	finding.detail = fmt.Sprintf("%s %s provides %s and supports suspend", system.binary, version, system.machine)
	return finding
}

func doctorQEMUSystemForArchitecture(goarch string) (doctorQEMUSystem, error) {
	switch goarch {
	case "amd64":
		return doctorQEMUSystem{binary: "qemu-system-x86_64", machine: "q35"}, nil
	case "arm64":
		return doctorQEMUSystem{binary: "qemu-system-aarch64", machine: "virt"}, nil
	default:
		return doctorQEMUSystem{}, fmt.Errorf("unsupported Linux architecture %q for QEMU", goarch)
	}
}

func parseDoctorQEMUVersion(output []byte) (doctorQEMUVersion, error) {
	text := string(output)
	for i := 0; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			continue
		}
		var major, minor, patch int
		if _, err := fmt.Sscanf(text[i:], "%d.%d.%d", &major, &minor, &patch); err == nil {
			return doctorQEMUVersion{major: major, minor: minor, patch: patch}, nil
		}
		if _, err := fmt.Sscanf(text[i:], "%d.%d", &major, &minor); err == nil {
			return doctorQEMUVersion{major: major, minor: minor}, nil
		}
	}
	return doctorQEMUVersion{}, errors.New("version string not found")
}

func qemuMachinePresent(output []byte, required string) bool {
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 0 && fields[0] == required {
			return true
		}
	}
	return false
}

func linuxBridgeNetfilterFinding(readFile func(string) ([]byte, error), command commandOutput) doctorFinding {
	finding := doctorFinding{level: "PASS", check: "bridge-netfilter"}
	if readFile == nil {
		finding.level, finding.detail = "FAIL", "bridge netfilter probe is unavailable"
		return finding
	}
	raw, err := readFile(doctorBridgeNFCall)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return finding
		}
		finding.level, finding.detail = "FAIL", fmt.Sprintf("read %s: %v", doctorBridgeNFCall, err)
		return finding
	}
	if strings.TrimSpace(string(raw)) != "1" {
		return finding
	}
	rules, err := command("iptables", "-S", "FORWARD")
	if err != nil {
		// A host whose PATH has no iptables cannot be inspected through it at
		// all. That says nothing about how FORWARD is configured, so it must
		// not fail an otherwise healthy host (#448). "not on PATH" is as much
		// as the probe knows: on Debian and Ubuntu the binary usually exists
		// and it is a non-root shell's PATH that omits /usr/sbin, so claiming
		// it is not installed would send the operator to install what they
		// already have.
		if errors.Is(err, exec.ErrNotFound) {
			finding.level = "WARN"
			finding.detail = fmt.Sprintf(
				"br_netfilter is active but iptables was not found on PATH, so the FORWARD policy cannot be read; read it with `%s` (a non-root shell often omits /usr/sbin), or inspect the chain with `%s`",
				doctorForwardPolicyInspect, doctorNFTForwardPolicyInspect,
			)
			return finding
		}
		// Doctor runs as the invoking user, and iptables refuses to list a
		// chain without root; the same exit status also covers a missing table
		// or a contended xtables lock. None of that is evidence about the
		// policy either (#448).
		if iptablesPolicyUnreadable(rules, err) {
			finding.level = "WARN"
			finding.detail = fmt.Sprintf(
				"br_netfilter is active but the FORWARD policy cannot be read (%v); inspect it with `%s` (listing the chain needs root)",
				err, doctorForwardPolicyInspect,
			)
			return finding
		}
		finding.level, finding.detail = "FAIL", fmt.Sprintf("inspect FORWARD policy: %v", err)
		return finding
	}
	text := string(rules)
	if !strings.Contains(text, "-P FORWARD DROP") {
		return finding
	}
	if linuxRoutedForwardRulesPresent(text) {
		return finding
	}
	finding.level = "FAIL"
	finding.detail = fmt.Sprintf(
		"br_netfilter is active with a default-DROP FORWARD policy; routed cluster traffic will hang. Add targeted egress and established-return rules such as `%s`, or disable bridge netfilter on this host",
		doctorForwardPolicyFix,
	)
	return finding
}

// iptablesPolicyUnreadable reports whether an `iptables -S` failure means the
// chain could not be read rather than that the firewall is broken. Exit status
// 4 is xtables' RESOURCE_PROBLEM — a missing permission, a missing table, or a
// contended lock; the nftables-backed binary can also exit with other statuses,
// so the message it printed counts too.
func iptablesPolicyUnreadable(output []byte, err error) bool {
	if err == nil {
		return false
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == doctorIPTablesResourceProblemExit {
		return true
	}
	lowered := strings.ToLower(string(output) + " " + err.Error())
	return strings.Contains(lowered, "permission denied") ||
		strings.Contains(lowered, "operation not permitted") ||
		strings.Contains(lowered, "must be root")
}

func linuxRoutedForwardRulesPresent(text string) bool {
	outbound := map[string]struct{}{}
	establishedReturn := map[string]struct{}{}
	scanner := bufio.NewScanner(strings.NewReader(text))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		switch {
		case len(fields) == 6 &&
			fields[0] == "-A" && fields[1] == "FORWARD" && fields[2] == "-i" &&
			strings.HasPrefix(fields[3], "br-tbx") && fields[4] == "-j" && fields[5] == "ACCEPT":
			outbound[fields[3]] = struct{}{}
		case len(fields) == 10 &&
			fields[0] == "-A" && fields[1] == "FORWARD" && fields[2] == "-o" &&
			strings.HasPrefix(fields[3], "br-tbx") && fields[4] == "-m" && fields[5] == "conntrack" &&
			fields[6] == "--ctstate" &&
			(fields[7] == "RELATED,ESTABLISHED" || fields[7] == "ESTABLISHED,RELATED") &&
			fields[8] == "-j" && fields[9] == "ACCEPT":
			establishedReturn[fields[3]] = struct{}{}
		}
	}
	for bridge := range outbound {
		if _, ok := establishedReturn[bridge]; ok {
			return true
		}
	}
	return false
}

func linuxBridgeSTPFinding(listClusters func() ([]cluster.Cluster, error), readFile func(string) ([]byte, error)) doctorFinding {
	finding := doctorFinding{check: "bridge-stp"}
	if listClusters == nil || readFile == nil {
		finding.level, finding.detail = "FAIL", "bridge STP probe is unavailable"
		return finding
	}
	clusters, err := listClusters()
	if err != nil {
		finding.level, finding.detail = "FAIL", fmt.Sprintf("list clusters: %v", err)
		return finding
	}
	if len(clusters) == 0 {
		return doctorFinding{level: "SKIP", check: "bridge-stp", detail: "no clusters exist"}
	}
	var problems []string
	for _, bridge := range configuredLinuxBridges(clusters) {
		raw, err := readFile("/sys/class/net/" + bridge + "/bridge/stp_state")
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			problems = append(problems, fmt.Sprintf("%s: %v", bridge, err))
			continue
		}
		if strings.TrimSpace(string(raw)) != "0" {
			problems = append(problems, fmt.Sprintf("%s has STP enabled; run `sudo ip link set dev %s type bridge stp_state 0`", bridge, bridge))
		}
	}
	if len(problems) == 0 {
		return doctorFinding{level: "PASS", check: "bridge-stp"}
	}
	return doctorFinding{level: "FAIL", check: "bridge-stp", detail: strings.Join(problems, "; ")}
}

func configuredLinuxBridges(clusters []cluster.Cluster) []string {
	bridges := make([]string, 0, len(clusters))
	for _, item := range clusters {
		bridges = append(bridges, cluster.LinuxBridgeName(item.SubnetIndex))
	}
	slices.Sort(bridges)
	return slices.Compact(bridges)
}

func linuxRPFilterFinding(command commandOutput, readFile func(string) ([]byte, error)) doctorFinding {
	finding := doctorFinding{check: "rp-filter"}
	if command == nil || readFile == nil {
		finding.level, finding.detail = "FAIL", "rp_filter probe is unavailable"
		return finding
	}
	defaultRoutes, err := command("ip", "-o", "route", "show", "default")
	if err != nil {
		finding.level, finding.detail = "FAIL", fmt.Sprintf("inspect default routes: %v", err)
		return finding
	}
	activeLinks, err := command("ip", "-o", "link", "show", "up")
	if err != nil {
		finding.level, finding.detail = "FAIL", fmt.Sprintf("inspect active links: %v", err)
		return finding
	}
	interfaces := rpFilterRelevantInterfaces(defaultRoutes, activeLinks)
	if len(interfaces) < 2 && !containsVPNInterface(interfaces) {
		return doctorFinding{level: "SKIP", check: "rp-filter", detail: "single-homed host"}
	}
	values := make(map[string]int, len(interfaces)+1)
	if values["all"], err = readLinuxSysctlInt(readFile, "/proc/sys/net/ipv4/conf/all/rp_filter"); err != nil {
		finding.level, finding.detail = "FAIL", fmt.Sprintf("read rp_filter: %v", err)
		return finding
	}
	worstName := "all"
	worstValue := values["all"]
	for _, iface := range interfaces {
		value, readErr := readLinuxSysctlInt(readFile, "/proc/sys/net/ipv4/conf/"+iface+"/rp_filter")
		if readErr != nil {
			finding.level, finding.detail = "FAIL", fmt.Sprintf("read rp_filter for %s: %v", iface, readErr)
			return finding
		}
		values[iface] = value
		if value > worstValue {
			worstName, worstValue = iface, value
		}
	}
	if worstValue == 1 {
		return doctorFinding{
			level: "FAIL",
			check: "rp-filter",
			detail: fmt.Sprintf(
				"strict rp_filter is active on %s for a multi-homed/VPN host; set loose mode with `%s` (and matching per-interface sysctls) before using Talos VIPs",
				worstName,
				doctorRPFilterLooseFix,
			),
		}
	}
	return doctorFinding{level: "PASS", check: "rp-filter"}
}

func readLinuxSysctlInt(readFile func(string) ([]byte, error), path string) (int, error) {
	raw, err := readFile(path)
	if err != nil {
		return 0, err
	}
	var value int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(raw)), "%d", &value); err != nil {
		return 0, fmt.Errorf("parse %s: %w", path, err)
	}
	return value, nil
}

func defaultRouteInterfaces(output []byte) []string {
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	var names []string
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		for i := 0; i+1 < len(fields); i++ {
			if fields[i] == "dev" {
				names = append(names, fields[i+1])
				break
			}
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

func activeLinkInterfaces(output []byte) []string {
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	var names []string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		colon := strings.Index(line, ":")
		if colon == -1 {
			continue
		}
		rest := strings.TrimSpace(line[colon+1:])
		colon = strings.Index(rest, ":")
		if colon == -1 {
			continue
		}
		name := strings.TrimSpace(rest[:colon])
		if name == "" || name == "lo" {
			continue
		}
		if at := strings.Index(name, "@"); at != -1 {
			name = name[:at]
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

func rpFilterRelevantInterfaces(defaultRoutes, activeLinks []byte) []string {
	names := defaultRouteInterfaces(defaultRoutes)
	for _, name := range activeLinkInterfaces(activeLinks) {
		if containsVPNInterface([]string{name}) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

func containsVPNInterface(names []string) bool {
	for _, name := range names {
		for _, prefix := range []string{"tun", "tap", "ppp", "wg", "tailscale", "zt", "utun"} {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
	}
	return false
}

func linuxPortFinding(
	port int,
	protocol string,
	listClusters func() ([]cluster.Cluster, error),
	readFile func(string) ([]byte, error),
	command commandOutput,
	listenPacket func(string, string) (net.PacketConn, error),
	listenStream func(string, string) (io.Closer, error),
) doctorFinding {
	check := fmt.Sprintf("port-%d", port)
	if listClusters == nil || readFile == nil || command == nil {
		return doctorFinding{level: "FAIL", check: check, detail: "port preflight is unavailable"}
	}
	clusters, err := listClusters()
	if err != nil {
		return doctorFinding{level: "FAIL", check: check, detail: fmt.Sprintf("list clusters: %v", err)}
	}
	if len(clusters) == 0 {
		return doctorFinding{level: "SKIP", check: check, detail: "no clusters exist"}
	}
	commandArgs := []string{"-H"}
	if protocol == "udp" {
		commandArgs = append(commandArgs, "-lnup")
	} else {
		commandArgs = append(commandArgs, "-lntp")
	}
	output, err := command("ss", commandArgs...)
	if err != nil {
		return doctorFinding{level: "FAIL", check: check, detail: fmt.Sprintf("inspect listening sockets: %v", err)}
	}
	var conflicts, unknownOwners []string
	activeBridgeCount := 0
	for _, item := range clusters {
		bridge := cluster.LinuxBridgeName(item.SubnetIndex)
		if _, err := readFile("/sys/class/net/" + bridge + "/ifindex"); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			conflicts = append(conflicts, fmt.Sprintf("%s: inspect %s: %v", item.Name, bridge, err))
			continue
		}
		activeBridgeCount++
		target := fmt.Sprintf("%s:%d", cluster.Gateway(item.SubnetIndex), port)
		listener := findSocketListener(output, cluster.Gateway(item.SubnetIndex), port)
		switch {
		case listener.line == "":
			if err := linuxBindPreflight(protocol, target, listenPacket, listenStream); err != nil {
				// Doctor normally runs without the helper's elevated bind capability.
				// A permission failure therefore says nothing about whether the
				// helper can claim the port; the listener inventory and live helper
				// capability check cover those cases independently.
				if errors.Is(err, os.ErrPermission) {
					continue
				}
				conflicts = append(conflicts, fmt.Sprintf("%s cannot bind %s: %v", item.Name, target, err))
			}
		case isTalosBoxListener(listener.process):
			continue
		case listener.process == "":
			unknownOwners = append(unknownOwners, fmt.Sprintf("%s has a listener on %s but its owner is hidden; run `sudo ss %s` to identify it", item.Name, target, strings.Join(commandArgs, " ")))
		default:
			conflicts = append(conflicts, fmt.Sprintf("%s is already listening on %s (%s)", item.Name, target, listener.line))
		}
	}
	if activeBridgeCount == 0 {
		return doctorFinding{level: "SKIP", check: check, detail: "no cluster bridges exist"}
	}
	if len(conflicts) == 0 {
		if len(unknownOwners) != 0 {
			return doctorFinding{level: "WARN", check: check, detail: strings.Join(unknownOwners, "; ")}
		}
		return doctorFinding{level: "PASS", check: check}
	}
	return doctorFinding{level: "FAIL", check: check, detail: strings.Join(conflicts, "; ")}
}

func findSocketListener(output []byte, host string, port int) doctorSocketListener {
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		fields := strings.Fields(line)
		localIndex := 3
		if len(fields) != 0 && (fields[0] == "tcp" || fields[0] == "udp") {
			localIndex = 4
		}
		if localIndex < len(fields) && socketEndpointMatches(fields[localIndex], host, port) {
			return doctorSocketListener{line: line, process: parseSocketListenerProcess(line)}
		}
	}
	return doctorSocketListener{}
}

func socketEndpointMatches(endpoint, host string, port int) bool {
	separator := strings.LastIndex(endpoint, ":")
	if separator == -1 {
		return false
	}
	gotPort, err := strconv.Atoi(endpoint[separator+1:])
	if err != nil || gotPort != port {
		return false
	}
	gotHost := strings.Trim(endpoint[:separator], "[]")
	return gotHost == host || gotHost == "*" || gotHost == "0.0.0.0" || gotHost == "::"
}

func parseSocketListenerProcess(line string) string {
	start := strings.Index(line, "\"")
	if start == -1 {
		return ""
	}
	rest := line[start+1:]
	end := strings.Index(rest, "\"")
	if end == -1 {
		return ""
	}
	return rest[:end]
}

func isTalosBoxListener(process string) bool {
	return process == "tbx" || process == "tbxd" || process == "tbx-helper"
}

func linuxBindPreflight(
	protocol, address string,
	listenPacket func(string, string) (net.PacketConn, error),
	listenStream func(string, string) (io.Closer, error),
) error {
	if protocol == "udp" {
		if listenPacket == nil {
			return errors.New("UDP bind probe is unavailable")
		}
		connection, err := listenPacket("udp4", address)
		if err != nil {
			return err
		}
		return connection.Close()
	}
	if listenStream == nil {
		return errors.New("TCP bind probe is unavailable")
	}
	listener, err := listenStream("tcp4", address)
	if err != nil {
		return err
	}
	return listener.Close()
}

func linuxHelperUnitFinding(command commandOutput) doctorFinding {
	finding := doctorFinding{check: "helper-unit"}
	output, err := command("systemctl", "is-enabled", doctorHelperSocketUnit)
	state := strings.TrimSpace(string(output))
	if err == nil && state == "enabled" {
		finding.level = "PASS"
		return finding
	}
	finding.level = "FAIL"
	switch {
	case strings.Contains(state, "disabled"), strings.Contains(state, "static"):
		finding.detail = fmt.Sprintf("%s is %s; run `sudo systemctl enable --now %s`", doctorHelperSocketUnit, state, doctorHelperSocketUnit)
	case strings.Contains(state, "not-found"), strings.Contains(string(output), "No such file"):
		finding.detail = fmt.Sprintf("%s is not installed; install the Linux integration package", doctorHelperSocketUnit)
	default:
		finding.detail = fmt.Sprintf("inspect %s: %v", doctorHelperSocketUnit, err)
	}
	return finding
}

func linuxHelperAccessFinding(command commandOutput, osrelease string) doctorFinding {
	finding := doctorFinding{level: "PASS", check: "helper-access"}
	output, err := command("id", "-Gn")
	if err != nil {
		finding.level, finding.detail = "FAIL", fmt.Sprintf("inspect current groups: %v", err)
		return finding
	}
	for _, group := range strings.Fields(string(output)) {
		if group == "tbx" {
			return finding
		}
	}
	finding.level = "FAIL"
	finding.detail = fmt.Sprintf("current user is not in group tbx; run `%s`, then %s", doctorHelperGroupFix, linuxSessionRefreshHint(osrelease))
	return finding
}

func linuxHelperCapabilitiesFinding(report func() (helperCapabilityReport, error)) doctorFinding {
	if report == nil {
		return doctorFinding{level: "SKIP", check: "helper-capabilities", detail: "helper capability self-report is unavailable in this build"}
	}
	caps, err := report()
	if err != nil {
		return doctorFinding{level: "FAIL", check: "helper-capabilities", detail: fmt.Sprintf("inspect helper capabilities: %v", err)}
	}
	if caps.Effective == doctorHelperCapabilityMask {
		return doctorFinding{level: "PASS", check: "helper-capabilities"}
	}
	return doctorFinding{
		level: "FAIL",
		check: "helper-capabilities",
		detail: fmt.Sprintf(
			"running helper has effective capabilities %v (mask %#x), want exactly CAP_NET_BIND_SERVICE, CAP_NET_ADMIN, and CAP_NET_RAW (mask %#x); restart %s after installing the current unit",
			caps.EffectiveNames,
			caps.Effective,
			doctorHelperCapabilityMask,
			doctorHelperServiceUnit,
		),
	}
}

func helperCapabilityReportFromHelper() (helperCapabilityReport, error) {
	client, err := helper.Connect()
	if err != nil {
		return helperCapabilityReport{}, err
	}
	defer func() { _ = client.Close() }()
	info, err := client.Info()
	if err != nil {
		return helperCapabilityReport{}, err
	}
	return helperCapabilityReport{
		Effective:      info.EffectiveCapabilities,
		EffectiveNames: info.EffectiveCapabilityNames,
	}, nil
}
