package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/config"
	"github.com/randax/talos-box/internal/daemon"
	"github.com/randax/talos-box/internal/extensions"
	"github.com/randax/talos-box/internal/imagecache"
	"github.com/randax/talos-box/internal/talosversion"
	"github.com/randax/talos-box/internal/version"
)

type cli struct {
	out io.Writer
	err io.Writer
	in  io.Reader
	// daemon carries the connect-time protocol gate; a nil session skips it
	daemon *daemonSession
}

func main() {
	command := cli{out: os.Stdout, err: os.Stderr, in: os.Stdin, daemon: newDaemonSession()}
	if err := command.run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "tbx: %v\n", err)
		os.Exit(1)
	}
}

func (c cli) run(args []string) error {
	if len(args) == 0 {
		c.printHelp(c.out)
		return nil
	}
	// A multi-verb command's own `--help` is answered here, before dispatch:
	// each group parses its verb by hand, so the flag would otherwise land as
	// an unknown verb.
	if handled, err := c.printGroupUsage(args[0], args[1:]); handled {
		return err
	}
	switch args[0] {
	case "cluster":
		return c.runCluster(args[1:])
	case "node":
		return c.runNode(args[1:])
	case "up":
		return c.runUp(args[1:])
	case "down":
		return c.runDown(args[1:])
	case "status":
		return c.runStatus(args[1:])
	case "manifests":
		return c.runManifests(args[1:])
	case "bgp":
		return c.runBGP(args[1:])
	case "mirror":
		return c.runMirror(args[1:])
	case "snapshot":
		return c.runSnapshot(args[1:])
	case "console":
		return c.runConsole(args[1:])
	case "cache":
		return c.runCache(args[1:])
	case "system":
		return c.runSystem(args[1:])
	case "doctor":
		return c.runDoctor(args[1:])
	case "version", "--version", "-v":
		_, err := fmt.Fprintf(c.out, "tbx %s (%s/%s, daemon protocol %d)\n", version.Version, runtime.GOOS, runtime.GOARCH, daemon.ProtocolVersion)
		return err
	case "help", "-h", "--help":
		c.printHelp(c.out)
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// groupUsages is the usage line every multi-verb command prints for both its
// bare and its `--help` form. Every group reads its line from here — no verb
// list is spelled out twice — so the two forms cannot drift apart.
var groupUsages = map[string]string{
	"cluster":  "usage: tbx cluster create|start|stop|suspend|resume|destroy|list",
	"node":     "usage: tbx node add|remove <cluster> [node]",
	"snapshot": "usage: tbx snapshot create|restore|list|delete",
	"cache":    "usage: tbx cache pull|prune|warm|list",
	"system":   "usage: tbx system install|uninstall|restart [--force]|status",
	"mirror":   "usage: tbx mirror offline [on|off]",
	"bgp":      "usage: tbx bgp enable|disable <cluster>",
}

// printGroupUsage answers `tbx <group> --help` with the group's usage. It is
// deliberately narrow: only the help flag alone, so a verb still dispatches and
// carries its own flag parsing, including its own --help.
func (c cli) printGroupUsage(command string, args []string) (bool, error) {
	if len(args) != 1 || (args[0] != "-h" && args[0] != "--help") {
		return false, nil
	}
	usage, ok := groupUsages[command]
	if !ok {
		return false, nil
	}
	_, err := fmt.Fprintln(c.out, usage)
	return true, err
}

func (c cli) runCluster(args []string) error {
	if len(args) == 0 {
		return errors.New(groupUsages["cluster"])
	}
	switch args[0] {
	case "create":
		return c.createCluster(args[1:])
	case "start":
		return c.startCluster(args[1:])
	case "stop", "suspend", "resume":
		if len(args) != 2 {
			return fmt.Errorf("usage: tbx cluster %s <name>", args[0])
		}
		var result daemon.ClusterSummary
		if err := c.call("cluster."+args[0], map[string]string{"name": args[1]}, &result); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(c.out, "%s cluster %s\n", pastTense(args[0]), result.Name); err != nil {
			return err
		}
		return printWarnings(c.err, result.Warnings, result.Warning)
	case "destroy":
		return c.destroyCluster(args[1:])
	case "list":
		flags := flag.NewFlagSet("cluster list", flag.ContinueOnError)
		flags.SetOutput(c.err)
		outputFormat := flags.String("o", "table", "output format: table|json")
		positionals, err := parseInterspersed(flags, args[1:])
		if err != nil {
			return err
		}
		if len(positionals) != 0 {
			return errors.New("usage: tbx cluster list [-o json]")
		}
		if err := validateOutputFormat(*outputFormat); err != nil {
			return err
		}
		var result []daemon.ClusterSummary
		if err := c.call("cluster.list", struct{}{}, &result); err != nil {
			return err
		}
		if *outputFormat == "json" {
			return encodeJSON(c.out, result)
		}
		return printClusters(c.out, result)
	default:
		return fmt.Errorf("unknown cluster command %q", args[0])
	}
}

func parseClusterStartArgs(args []string, output io.Writer) (string, bool, error) {
	name, force, _, err := parseClusterStartOptions(args, output)
	return name, force, err
}

func parseClusterStartOptions(args []string, output io.Writer) (string, bool, bool, error) {
	flags := flag.NewFlagSet("cluster start", flag.ContinueOnError)
	flags.SetOutput(output)
	force := flags.Bool("force", false, "proceed despite an overcommit or host-pressure warning")
	quiet := flags.Bool("quiet", false, "suppress provisioning narration")
	positionals, err := parseInterspersed(flags, args)
	if err != nil {
		return "", false, false, err
	}
	if len(positionals) != 1 {
		return "", false, false, errors.New("usage: tbx cluster start <name> [--force] [--quiet]")
	}
	return positionals[0], *force, *quiet, nil
}

// storedClustersQuery reports what the daemon has on disk. It is a var so
// tests can answer without a daemon.
var storedClustersQuery = storedClusters

// storedClusters asks the running daemon for the stored clusters. The exchange
// is deadlined: cluster.list is served under the daemon's operation lock, and
// this is only a courtesy lookup ahead of the real call.
func storedClusters() ([]daemon.ClusterSummary, error) {
	socketPath, err := daemon.SocketPath()
	if err != nil {
		return nil, err
	}
	response, err := exchangeWithin(socketPath, "cluster.list", struct{}{}, daemonHandshakeTimeout)
	if err != nil {
		return nil, err
	}
	if !response.OK {
		if response.Error == "" {
			return nil, errors.New("cluster.list failed")
		}
		return nil, errors.New(response.Error)
	}
	var clusters []daemon.ClusterSummary
	if len(response.Data) > 0 {
		if err := json.Unmarshal(response.Data, &clusters); err != nil {
			return nil, err
		}
	}
	return clusters, nil
}

// startProvisionDeadline reports the budget the daemon will hold this start to:
// a declared storage engine buys the larger one, exactly as the daemon's own
// provisionTimeout decides it. The CLI cannot read the stored cluster directly,
// so it asks the daemon what it has.
//
// Any failure — no daemon yet, a busy one, an unknown cluster — falls back to
// the storage bound. Overstating the budget only makes the heartbeat
// pessimistic; understating it would advertise a deadline the daemon does not
// honor, which is the drift the heartbeat exists to avoid (#307).
func startProvisionDeadline(name string) time.Duration {
	clusters, err := storedClustersQuery()
	if err != nil {
		return storageProvisionDeadline
	}
	for _, item := range clusters {
		if strings.EqualFold(item.Name, name) {
			return provisionDeadline(item.CSI != "")
		}
	}
	return storageProvisionDeadline
}

func (c cli) startCluster(args []string) error {
	name, force, quiet, err := parseClusterStartOptions(args, c.err)
	if err != nil {
		return err
	}
	request := struct {
		Name  string `json:"name"`
		Force bool   `json:"force"`
	}{Name: name, Force: force}
	var result daemon.ClusterSummary
	// A start reconciles the cluster's declared CNI/CSI on the same blocking
	// call as create, so the stated bound must be the one the daemon budgets
	// this request at (#307).
	signal := liveness{verb: "starting " + name, deadline: startProvisionDeadline(name), quiet: quiet}
	if err := c.callWithLiveness(signal, "cluster.start", request, &result); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(c.out, "started cluster %s\n", result.Name); err != nil {
		return err
	}
	if err := printWarnings(c.err, result.Warnings, result.Warning); err != nil {
		return err
	}
	if quiet {
		return nil
	}
	for _, line := range result.Narration {
		if _, err := fmt.Fprintln(c.out, line); err != nil {
			return err
		}
	}
	return nil
}

func (c cli) createCluster(args []string) error {
	flags := flag.NewFlagSet("cluster create", flag.ContinueOnError)
	flags.SetOutput(c.err)
	controlPlanes := flags.Int("cp", 1, "number of control planes")
	workers := flags.Int("workers", 2, "number of workers")
	memory := flags.Int("memory-mib", cluster.DefaultMemoryMiB, "memory per node in MiB")
	cpus := flags.Int("cpus", cluster.DefaultCPUs, "CPUs per node")
	disk := flags.Int("disk-gib", cluster.DefaultDiskGiB, "disk size per node in GiB")
	talosVersion := flags.String("talos-version", daemon.DefaultTalosVersion, "Talos version")
	schematic := flags.String("schematic", "", "Image Factory schematic")
	extensionList := flags.String("extensions", "", "curated Talos extensions, comma-separated: "+strings.Join(extensions.Names(), "|"))
	domainFlag := flags.String("domain", "", "cluster domain (default <name>.k8s.test)")
	allowUnsafeDomain := flags.Bool("allow-unsafe-domain", false, "allow a domain that can shadow real DNS")
	cni := flags.String("cni", "", "CNI to provision: cilium|flannel")
	csi := flags.String("csi", "", "CSI to provision: longhorn|local-path")
	lb := flags.Bool("lb", true, "install LoadBalancer support with the CNI")
	bgp := flags.Bool("bgp", false, "enable Cilium BGP LoadBalancer announcements")
	hubble := flags.Bool("hubble", false, "enable Cilium Hubble Relay and UI")
	quiet := flags.Bool("quiet", false, "suppress provisioning narration")
	force := flags.Bool("force", false, "proceed despite an overcommit or host-pressure warning")
	positionals, err := parseInterspersed(flags, args)
	if err != nil {
		return err
	}
	if len(positionals) != 1 {
		return errors.New("usage: tbx cluster create <name> [--cp N --workers N]")
	}
	provided := map[string]bool{}
	flags.Visit(func(item *flag.Flag) { provided[item.Name] = true })
	var lbArg, bgpArg, hubbleArg *bool
	if *cni != "" || provided["lb"] {
		lbArg = lb
	}
	if *cni != "" || provided["bgp"] {
		bgpArg = bgp
	}
	if *cni != "" || provided["hubble"] {
		hubbleArg = hubble
	}
	intentInput := cluster.ProvisioningIntentInput{CNI: *cni, CSI: *csi, LB: lbArg, BGP: bgpArg, Hubble: hubbleArg}
	if _, err := intentInput.Intent(); err != nil {
		return err
	}
	// Set membership is local: a typo must be reported here, before the
	// daemon is started or the Image Factory is contacted at all.
	requestedExtensions := parseExtensionList(*extensionList)
	if _, err := extensions.Resolve(requestedExtensions); err != nil {
		return err
	}
	if err := c.ensureProvisioningIntentSupport(intentInput); err != nil {
		return err
	}
	if len(requestedExtensions) > 0 {
		if err := c.ensurePerClusterTalosSupport(); err != nil {
			return err
		}
	}
	// An empty value means "daemon default", matching the daemon boundary.
	// Version validation is local; it runs before schematic resolution,
	// which may contact the Image Factory.
	if *talosVersion != "" {
		if err := talosversion.Validate(*talosVersion); err != nil {
			return err
		}
	}
	// Composing extensions is the daemon's job: it owns the cache that makes
	// an already-composed schematic resolvable offline. Resolving the
	// default here would pin the extension-free schematic instead.
	resolvedSchematic := *schematic
	if len(requestedExtensions) == 0 {
		resolvedSchematic, err = resolveSchematic(*schematic)
		if err != nil {
			return err
		}
	}
	request := struct {
		Name              string               `json:"name"`
		ControlPlanes     int                  `json:"controlPlanes"`
		Workers           int                  `json:"workers"`
		Node              cluster.NodeDefaults `json:"node"`
		Domain            string               `json:"domain,omitempty"`
		AllowUnsafeDomain bool                 `json:"allowUnsafeDomain,omitempty"`
		cluster.ProvisioningIntentInput
		Force      bool     `json:"force"`
		Schematic  string   `json:"schematic"`
		Version    string   `json:"version"`
		Extensions []string `json:"extensions,omitempty"`
	}{
		Name: positionals[0], ControlPlanes: *controlPlanes, Workers: *workers,
		Node:   cluster.NodeDefaults{MemoryMiB: *memory, CPUs: *cpus, DiskGiB: *disk},
		Domain: *domainFlag, AllowUnsafeDomain: *allowUnsafeDomain,
		ProvisioningIntentInput: intentInput,
		Force:                   *force, Schematic: resolvedSchematic, Version: *talosVersion,
		Extensions: requestedExtensions,
	}
	var result daemon.ClusterSummary
	signal := liveness{
		verb:     "provisioning " + positionals[0],
		deadline: provisionDeadline(*csi != ""),
		quiet:    *quiet,
	}
	if err := c.callWithLiveness(signal, "cluster.create", request, &result); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(c.out, "created and started cluster %s (%d control plane, %d workers)\n",
		result.Name, result.ControlPlanes, result.Workers); err != nil {
		return err
	}
	if err := printWarnings(c.err, result.Warnings, result.Warning); err != nil {
		return err
	}
	if !*quiet {
		for _, line := range result.Narration {
			if _, err := fmt.Fprintln(c.out, line); err != nil {
				return err
			}
		}
	}
	// The pin goes in the file-level block: for a single cluster it is
	// semantically identical to a per-cluster stanza, and the emitted file
	// replays against daemons older than the per-cluster talos protocol.
	stanza := config.Marshal(config.Config{
		Talos: config.TalosSpec{Version: result.TalosVersion, Schematic: result.Schematic},
		Clusters: []config.ClusterSpec{{
			Name: result.Name, ControlPlanes: result.ControlPlanes, Workers: result.Workers,
			ProvisioningIntent: result.ProvisioningIntent,
			Domain:             result.Domain, AllowUnsafeDomain: result.AllowUnsafeDomain,
			Node: result.NodeDefaults,
		}}})
	_, err = fmt.Fprintf(c.out, "\nequivalent talosbox.yaml:\n%s", stanza)
	return err
}

func (c cli) destroyCluster(args []string) error {
	flags := flag.NewFlagSet("cluster destroy", flag.ContinueOnError)
	flags.SetOutput(c.err)
	force := flags.Bool("force", false, "confirm permanent deletion")
	positionals, err := parseInterspersed(flags, args)
	if err != nil {
		return err
	}
	if len(positionals) != 1 {
		return errors.New("usage: tbx cluster destroy <name> --force")
	}
	if !*force {
		return errors.New("cluster destroy requires --force")
	}
	request := struct {
		Name  string `json:"name"`
		Force bool   `json:"force"`
	}{Name: positionals[0], Force: true}
	if err := c.inspectDestroy(positionals[0], request); err != nil {
		return err
	}
	if err := c.call("cluster.destroy", request, nil); err != nil {
		return err
	}
	_, err = fmt.Fprintf(c.out, "destroyed cluster %s\n", positionals[0])
	return err
}

func (c cli) inspectDestroy(name string, request struct {
	Name  string `json:"name"`
	Force bool   `json:"force"`
}) error {
	var inspection daemon.DestroyInspection
	if err := c.call("cluster.destroy.inspect", request, &inspection); err != nil {
		return printWarning(c.err, daemon.DestroyInspectionDataLossWarning(name, ""))
	}
	return printWarning(c.err, inspection.Warning)
}

func (c cli) runNode(args []string) error {
	if len(args) == 0 {
		return errors.New(groupUsages["node"])
	}
	switch args[0] {
	case "add":
		flags := flag.NewFlagSet("node add", flag.ContinueOnError)
		flags.SetOutput(c.err)
		role := flags.String("role", string(cluster.RoleWorker), "worker or control-plane")
		force := flags.Bool("force", false, "proceed despite an overcommit or host-pressure warning")
		positionals, err := parseInterspersed(flags, args[1:])
		if err != nil {
			return err
		}
		if len(positionals) < 1 || len(positionals) > 2 {
			return errors.New("usage: tbx node add <cluster> [node] [--role worker|control-plane] [--force]")
		}
		name := ""
		if len(positionals) == 2 {
			name = positionals[1]
		}
		request := map[string]any{"cluster": positionals[0], "name": name, "role": *role, "force": *force}
		var result daemon.NodeStatus
		if err := c.call("node.add", request, &result); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(c.out, "added node %s to cluster %s\n", result.Name, positionals[0]); err != nil {
			return err
		}
		return printWarnings(c.err, result.Warnings, result.Warning)
	case "remove":
		flags := flag.NewFlagSet("node remove", flag.ContinueOnError)
		flags.SetOutput(c.err)
		force := flags.Bool("force", false, "remove the node even when it holds the only copy of volume data")
		positionals, err := parseInterspersed(flags, args[1:])
		if err != nil {
			return err
		}
		if len(positionals) != 2 {
			return errors.New("usage: tbx node remove <cluster> <node> [--force]")
		}
		if err := c.ensureNodeRemoveSupport(); err != nil {
			return err
		}
		request := map[string]any{"cluster": positionals[0], "name": positionals[1], "force": *force}
		var result daemon.NodeStatus
		if err := c.call("node.remove", request, &result); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(c.out, "removed node %s from cluster %s\n", positionals[1], positionals[0]); err != nil {
			return err
		}
		return printWarnings(c.err, result.Warnings, result.Warning)
	default:
		return fmt.Errorf("unknown node command %q", args[0])
	}
}

func (c cli) runStatus(args []string) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(c.err)
	quiet := flags.Bool("quiet", false, "suppress hints")
	outputFormat := flags.String("o", "table", "output format: table|json")
	positionals, err := parseInterspersed(flags, args)
	if err != nil {
		return err
	}
	if len(positionals) > 1 {
		return errors.New("usage: tbx status [cluster] [--quiet] [-o json]")
	}
	if err := validateOutputFormat(*outputFormat); err != nil {
		return err
	}
	name := ""
	if len(positionals) == 1 {
		name = positionals[0]
	}
	var result []daemon.ClusterStatus
	if err := c.call("status", map[string]string{"cluster": name}, &result); err != nil {
		return err
	}
	if *quiet {
		for i := range result {
			result[i].Hints = nil
		}
	}
	if *outputFormat == "json" {
		return encodeJSON(c.out, result)
	}
	return printStatus(c.out, result, *quiet)
}

func (c cli) runCache(args []string) error {
	if len(args) == 0 {
		return errors.New(groupUsages["cache"])
	}
	switch args[0] {
	case "pull":
		return c.runCachePull(args[1:])
	case "prune":
		flags := flag.NewFlagSet("cache prune", flag.ContinueOnError)
		flags.SetOutput(c.err)
		pruneMirror := flags.Bool("mirror", false, "remove only mirror cache")
		pruneAll := flags.Bool("all", false, "remove disk and mirror cache")
		positionals, err := parseInterspersed(flags, args[1:])
		if err != nil {
			return err
		}
		// The scopes are alternatives, not a widening pair: naming both is a
		// conflict the user must resolve, not a usage typo.
		if *pruneMirror && *pruneAll {
			return errors.New("--mirror and --all are mutually exclusive: --mirror prunes the mirror cache only, --all prunes disk and mirror")
		}
		if len(positionals) != 0 {
			return errors.New("usage: tbx cache prune [--mirror|--all]")
		}
		scope := daemon.CachePruneScopeImages
		if *pruneMirror {
			scope = daemon.CachePruneScopeMirror
		} else if *pruneAll {
			scope = daemon.CachePruneScopeAll
		}
		var result daemon.CachePruneResult
		if err := c.call("cache.prune", daemon.CachePruneArgs{Scope: scope}, &result); err != nil {
			return err
		}
		switch result.Scope {
		case daemon.CachePruneScopeImages:
			if err := printPrunedImages(c.out, result.Images); err != nil {
				return err
			}
			kept := ""
			if result.KeptCount > 0 {
				kept = fmt.Sprintf(" (kept %d image(s) in use, pinned, or default)", result.KeptCount)
			}
			_, err = fmt.Fprintf(c.out, "pruned disk cache: %d image(s), %d bytes%s; mirror cache untouched\n", result.ImageCount, result.ImageBytes, kept)
		case daemon.CachePruneScopeMirror:
			_, err = fmt.Fprintf(c.out, "pruned mirror cache: %d blob(s) %d bytes, %d manifest(s) %d bytes; disk cache untouched\n",
				result.Mirror.BlobCount, result.Mirror.BlobBytes, result.Mirror.ManifestCount, result.Mirror.ManifestBytes)
		case daemon.CachePruneScopeAll:
			_, err = fmt.Fprintf(c.out, "pruned all cache: %d image(s), %d bytes; %d blob(s) %d bytes, %d manifest(s) %d bytes\n",
				result.ImageCount, result.ImageBytes, result.Mirror.BlobCount, result.Mirror.BlobBytes, result.Mirror.ManifestCount, result.Mirror.ManifestBytes)
		default:
			return fmt.Errorf("cache prune returned unknown scope %q", result.Scope)
		}
		return err
	case "list":
		if len(args) != 1 {
			return errors.New("usage: tbx cache list")
		}
		var result daemon.CacheListResult
		if err := c.call("cache.list", struct{}{}, &result); err != nil {
			return err
		}
		if len(result.Images) == 0 {
			if _, err := fmt.Fprintln(c.out, "Talos disk images: empty"); err != nil {
				return err
			}
		} else {
			if _, err := fmt.Fprintln(c.out, "Talos disk images:"); err != nil {
				return err
			}
			for _, entry := range result.Images {
				if _, err := fmt.Fprintf(c.out, "- %s%s\n", cacheImageLine(entry), cacheImageStatusSuffix(entry)); err != nil {
					return err
				}
			}
		}
		if len(result.Mirror) == 0 {
			if _, err := fmt.Fprintln(c.out, "Mirror cache: empty"); err != nil {
				return err
			}
		} else {
			if _, err := fmt.Fprintln(c.out, "Mirror cache:"); err != nil {
				return err
			}
			for _, entry := range result.Mirror {
				if _, err := fmt.Fprintf(c.out, "- %s: %d blob(s) %d bytes, %d manifest(s) %d bytes\n",
					entry.Upstream, entry.BlobCount, entry.BlobBytes, entry.ManifestCount, entry.ManifestBytes); err != nil {
					return err
				}
			}
		}
		_, err := fmt.Fprintf(c.out, "Mirror total: %d blob(s) %d bytes, %d manifest(s) %d bytes\n",
			result.MirrorTotal.BlobCount, result.MirrorTotal.BlobBytes, result.MirrorTotal.ManifestCount, result.MirrorTotal.ManifestBytes)
		return err
	case "warm":
		return c.runCacheWarm(args[1:])
	default:
		return fmt.Errorf("unknown cache command %q", args[0])
	}
}

func (c cli) printHelp(output io.Writer) {
	const help = `Usage: tbx <command>

Commands:
  up [-f talosbox.yaml] [--force]
  down [-f talosbox.yaml]
  cluster create|start|stop|suspend|resume|destroy|list
  node add|remove
  snapshot create|restore|list|delete
  status [cluster]
  manifests <cluster> [section] [--cni cilium|flannel]
  console <cluster> <node>
  bgp enable|disable <cluster>
  mirror offline [on|off]
  cache pull|prune|warm|list
  system install|uninstall|restart [--force]|status
  doctor
  version (also --version, -v)
`
	_, _ = fmt.Fprint(output, help)
}

func printWarning(w io.Writer, warning string) error {
	if warning == "" {
		return nil
	}
	_, err := fmt.Fprintf(w, "warning: %s\n", warning)
	return err
}

// printWarnings renders one warning per line. Unrelated findings used to be
// fused onto a single semicolon-joined line (#291); joined is the legacy
// single-string field, used only when talking to a daemon that predates the
// per-warning list.
func printWarnings(w io.Writer, warnings []string, joined string) error {
	if len(warnings) == 0 {
		return printWarning(w, joined)
	}
	for _, warning := range warnings {
		if err := printWarning(w, warning); err != nil {
			return err
		}
	}
	return nil
}

func pastTense(command string) string {
	switch command {
	case "stop":
		return "stopped"
	case "suspend":
		return "suspended"
	case "resume":
		return "resumed"
	default:
		return "started"
	}
}

// parseExtensionList splits the --extensions value; an empty value means no
// extensions were requested, which is distinct from the config-level opt-out.
func parseExtensionList(value string) []string {
	var names []string
	for _, name := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			names = append(names, trimmed)
		}
	}
	return names
}

func resolveSchematic(schematic string) (string, error) {
	if schematic != "" {
		return schematic, nil
	}
	cache, err := imagecache.NewDefault()
	if err != nil {
		return "", err
	}
	// The recorded default id keeps a create resolvable after `tbx cache
	// pull`, without the Factory.
	return cache.DefaultSchematic()
}
