package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/config"
	"github.com/randax/talos-box/internal/daemon"
)

const defaultConfigFile = "talosbox.yaml"

// Provisioning budgets are the daemon's own exported constants, so the stated
// deadline can never drift from what the request is actually held to. A create
// additionally waits for its nodes to boot before the provisioning budget
// starts, so its stated deadline carries that wait too (#307).
const (
	cniProvisionDeadline     = daemon.CNIProvisionTimeout
	storageProvisionDeadline = daemon.StorageProvisionTimeout
	nodeBootDeadline         = daemon.NodeBootTimeout
	kubernetesReadyDeadline  = daemon.KubernetesReadyWaitTimeout
)

// livenessInterval is how often a blocking lifecycle call reports it is still
// alive. Tests shorten it.
var livenessInterval = time.Minute

// livenessPreambleDelay is how long a --quiet call waits before announcing its
// provisioning window. The window is worth stating up front for real work, but
// a run the planner classifies as a no-op answers well inside this delay and
// must not open by announcing work it never does (#421). Tests shorten it.
var livenessPreambleDelay = 5 * time.Second

// livenessGrace is how far past its stated deadline a blocking call may run
// before the CLI stops waiting. The daemon enforces its own budgets; the grace
// covers the round trip and the daemon's teardown, so this bound only ever
// fires on a gate that hung past every budget it declared (#392).
var livenessGrace = 2 * time.Minute

// liveness keeps a blocking daemon call distinguishable from a hang. up,
// cluster create, cluster start and node add all send one request that the
// daemon answers only after provisioning converges — up to the deadline below —
// so without a heartbeat the terminal shows nothing at all for the whole pass
// (#307 #273).
// Everything it writes goes to stderr: stdout stays the scriptable result.
type liveness struct {
	// verb reads as "provisioning demo" in both the preamble and the beat.
	verb     string
	deadline time.Duration
	quiet    bool
}

// narrator serializes the progress stream's two writers: the liveness
// heartbeat, which beats from its own goroutine, and the daemon's stage
// narration, which arrives on the call goroutine (#273).
type narrator struct {
	mu     sync.Mutex
	output io.Writer
}

// line writes one progress line, ignoring a write failure: an operation must
// not fail because its narration could not be printed.
func (n *narrator) line(format string, args ...any) {
	n.mu.Lock()
	defer n.mu.Unlock()
	_, _ = fmt.Fprintf(n.output, format, args...)
}

// stages returns the sink that prints the daemon's stage lines, or nil when
// narration is suppressed — a nil sink is also what tells the daemon not to
// send any.
func (n *narrator) stages(quiet bool) func(string) {
	if quiet {
		return nil
	}
	return func(stage string) { n.line("%s\n", stage) }
}

// beat starts the heartbeat and returns the function that stops and joins it.
// Everything else writing to the same stream must go through the narrator.
func (l liveness) beat(output *narrator) func() {
	started := time.Now()
	ticker := time.NewTicker(livenessInterval)
	// --quiet drops narration, not the fact that this will take a while — but
	// the preamble waits out the no-op window first (#421).
	preamble := time.NewTimer(livenessPreambleDelay)
	if !l.quiet {
		preamble.Stop()
	}
	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			select {
			case <-stop:
				return
			case <-preamble.C:
				output.line("%s; overall deadline %s; progress suppressed by --quiet\n",
					l.verb, formatLivenessDuration(l.deadline))
			case <-ticker.C:
				// "overall deadline" names this budget apart from the
				// per-phase budgets the daemon narrates (#423).
				output.line("still %s (elapsed %s, overall deadline %s)\n",
					l.verb, formatLivenessDuration(time.Since(started)), formatLivenessDuration(l.deadline))
			}
		}
	}()
	return func() {
		ticker.Stop()
		preamble.Stop()
		close(stop)
		<-stopped
	}
}

// bound is the wall-clock limit the CLI holds the call to: the deadline it
// states plus the grace. Without it a gate that never returns leaves the verb
// hanging behind a heartbeat forever (#392).
func (l liveness) bound() time.Duration {
	return l.deadline + livenessGrace
}

// callWithLiveness runs one blocking lifecycle call under a heartbeat. The
// protocol handshake is settled first — retried, and recorded even when it is
// skipped — so call() cannot re-handshake mid-call and the heartbeat goroutine
// is the only writer to stderr while the call is in flight.
func (c cli) callWithLiveness(signal liveness, op string, args, destination any) error {
	return c.callWithLivenessNarrated(signal, op, args, destination, false)
}

// callWithLivenessNarrated is callWithLiveness with the daemon's stage
// narration folded into the same stream. The heartbeat proves the call is
// alive; the stages say what it is doing (#263 #273). --quiet drops the stages
// and keeps the heartbeat.
func (c cli) callWithLivenessNarrated(signal liveness, op string, args, destination any, narrate bool) error {
	if err := c.resolveDaemonProtocol(true); err != nil {
		return err
	}
	stream := &narrator{output: c.err}
	stop := signal.beat(stream)
	defer stop()
	err := c.callNarratedWithin(op, args, destination, stream.stages(!narrate || signal.quiet), signal.bound())
	if err != nil && isTimeout(err) {
		return fmt.Errorf("tbxd did not finish %s within %s (overall deadline %s plus %s grace); "+
			"the daemon may still be working — check: tbx status",
			signal.verb, formatLivenessDuration(signal.bound()),
			formatLivenessDuration(signal.deadline), formatLivenessDuration(livenessGrace))
	}
	return err
}

// formatLivenessDuration renders a budget the way the runbook states it: whole
// seconds under a minute, whole minutes above it, hours once it reads better.
func formatLivenessDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Round(time.Second).Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Truncate(time.Minute).Minutes()))
	default:
		hours := int(d.Truncate(time.Minute).Hours())
		minutes := int(d.Truncate(time.Minute).Minutes()) - hours*60
		if minutes == 0 {
			return fmt.Sprintf("%dh", hours)
		}
		return fmt.Sprintf("%dh%dm", hours, minutes)
	}
}

// provisionDeadline reports the budget the daemon will hold this request to: a
// declared storage engine buys the larger one.
func provisionDeadline(storage bool) time.Duration {
	if storage {
		return storageProvisionDeadline
	}
	return cniProvisionDeadline
}

// createProvisionDeadline is provisionDeadline plus the boot wait a create runs
// ahead of provisioning: the daemon holds a create for both, and a heartbeat
// stating only the provisioning half reports an elapsed time past its own
// deadline on any host where a node is slow to lease DHCP.
func createProvisionDeadline(storage bool) time.Duration {
	return provisionDeadline(storage) + nodeBootDeadline
}

// startedProvisionDeadline is createProvisionDeadline plus the Kubernetes
// readiness wait a started cluster additionally runs before its reconcile
// (#364): a start — direct or planned by `up` — can spend that whole window
// before the provisioning budget begins, and a heartbeat that omits it reports
// a healthy call as past its own deadline.
func startedProvisionDeadline(storage bool) time.Duration {
	return createProvisionDeadline(storage) + kubernetesReadyDeadline
}

// upProvisionDeadline is the request-wide bound for an up: the daemon walks
// the file's clusters one at a time — each boot wait, readiness wait and
// provisioning pass runs before the next cluster's begins — so a deadline
// stating one cluster's budget under-promises every multi-cluster file. The
// sum is conservative (a cluster already up spends almost none of its share),
// which is the safe direction: overstating only makes the heartbeat
// pessimistic, understating reports a healthy call as past its own deadline.
func upProvisionDeadline(cfg config.Config) time.Duration {
	var total time.Duration
	for _, spec := range cfg.Clusters {
		total += startedProvisionDeadline(spec.Input().CSI != "")
	}
	if total == 0 {
		// A file with no clusters has nothing to provision, but the heartbeat
		// still needs a bound to state.
		return startedProvisionDeadline(false)
	}
	return total
}

func (c cli) runUp(args []string) error {
	cfg, force, quiet, err := loadUpConfigFile(args)
	if err != nil {
		return err
	}
	if input, ok := strongestProvisioningIntent(cfg); ok {
		if err := c.ensureProvisioningIntentSupport(input); err != nil {
			return err
		}
	}
	if requiresPerClusterTalosHandshake(cfg) {
		if err := c.ensurePerClusterTalosSupport(); err != nil {
			return err
		}
	}
	var actions []daemon.Action
	request := struct {
		config.Config
		Force bool `json:"force"`
	}{Config: cfg, Force: force}
	// An up may create or start clusters, and both wait for their nodes to boot
	// before the provisioning budget starts — a start since #364 also waits for
	// Kubernetes readiness. The clusters are handled one after another, so the
	// stated deadline carries every cluster's share of those waits, not just
	// one pass's (#307).
	signal := liveness{
		verb:     "provisioning " + upSubject(cfg),
		deadline: upProvisionDeadline(cfg),
		quiet:    quiet,
	}
	if err := c.callWithLiveness(signal, "up", request, &actions); err != nil {
		return err
	}
	return c.printActions(actions, map[daemon.ActionKind]string{
		daemon.ActionCreate:    "created %s",
		daemon.ActionStart:     "started %s",
		daemon.ActionReconcile: "reconciled %s",
		daemon.ActionNone:      "%s is up to date",
	}, quiet)
}

// upSubject names what the up request is about to work on, so the heartbeat
// says which clusters are still in flight.
func upSubject(cfg config.Config) string {
	names := make([]string, 0, len(cfg.Clusters))
	for _, spec := range cfg.Clusters {
		names = append(names, spec.Name)
	}
	if len(names) == 0 {
		return "the declared clusters"
	}
	return strings.Join(names, ", ")
}

func strongestProvisioningIntent(cfg config.Config) (cluster.ProvisioningIntentInput, bool) {
	var strongest cluster.ProvisioningIntentInput
	found := false
	for _, spec := range cfg.Clusters {
		input := spec.Input()
		if !requiresProvisioningIntentHandshake(input) {
			continue
		}
		if !found || minimumProvisioningIntentProtocol(input) > minimumProvisioningIntentProtocol(strongest) {
			strongest = input
			found = true
		}
	}
	return strongest, found
}

// requiresPerClusterTalosHandshake reports whether the up request depends on
// protocol-5 talos handling: a cluster whose resolved spec diverges from the
// file-level block, or any extensions at all — the extensions field is new at
// protocol 5, so an older daemon would silently drop it even at file level.
// Pure version/schematic inheritance needs no handshake: every daemon version
// applies those file-level fields correctly.
func requiresPerClusterTalosHandshake(cfg config.Config) bool {
	if cfg.Talos.Extensions != nil {
		return true
	}
	for _, spec := range cfg.Clusters {
		if !spec.Talos.Equal(cfg.Talos) {
			return true
		}
	}
	return false
}

func (c cli) runDown(args []string) error {
	cfg, err := loadConfigFile(args, "down")
	if err != nil {
		return err
	}
	var actions []daemon.Action
	if err := c.call("down", cfg, &actions); err != nil {
		return err
	}
	return c.printActions(actions, map[daemon.ActionKind]string{
		daemon.ActionStop:    "stopped %s",
		daemon.ActionNone:    "%s is not running",
		daemon.ActionMissing: "%s does not exist (skipped)",
	})
}

func (c cli) printActions(actions []daemon.Action, wording map[daemon.ActionKind]string, quiet ...bool) error {
	suppressNarration := len(quiet) > 0 && quiet[0]
	for _, action := range actions {
		format, ok := wording[action.Kind]
		if !ok {
			format = string(action.Kind) + " %s"
		}
		if _, err := fmt.Fprintf(c.out, format+"\n", action.Cluster); err != nil {
			return err
		}
		if err := printWarnings(c.err, action.Warnings, action.Warning); err != nil {
			return err
		}
		if suppressNarration {
			continue
		}
		for _, line := range action.Narration {
			if _, err := fmt.Fprintln(c.out, line); err != nil {
				return err
			}
		}
	}
	return nil
}

func loadUpConfigFile(args []string) (config.Config, bool, bool, error) {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	path := fs.String("f", defaultConfigFile, "path to talosbox.yaml")
	force := fs.Bool("force", false, "override overcommit or host-pressure safeguards")
	quiet := fs.Bool("quiet", false, "suppress provisioning narration")
	if err := fs.Parse(args); err != nil {
		return config.Config{}, false, false, err
	}
	data, err := os.ReadFile(*path)
	if err != nil {
		return config.Config{}, false, false, fmt.Errorf("read %s: %w", *path, err)
	}
	cfg, err := config.Parse(data)
	return cfg, *force, *quiet, err
}

func loadConfigFile(args []string, verb string) (config.Config, error) {
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	path := fs.String("f", defaultConfigFile, "path to talosbox.yaml")
	if err := fs.Parse(args); err != nil {
		return config.Config{}, err
	}
	data, err := os.ReadFile(*path)
	if err != nil {
		return config.Config{}, fmt.Errorf("read %s: %w", *path, err)
	}
	return config.Parse(data)
}
