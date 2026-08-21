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
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hypervisor"
)

// suspendCluster pauses and saves every running VM's state, then stops them
// while retaining their device configurations for same-daemon restoration.
func (s *Server) suspendCluster(raw json.RawMessage) (ClusterSummary, error) {
	var args nameArgs
	if err := decodeArgs(raw, &args); err != nil {
		return ClusterSummary{}, err
	}
	item, err := cluster.Load(args.Name)
	if err != nil {
		return ClusterSummary{}, err
	}
	if !s.clusterRunning(item.Name) {
		return ClusterSummary{}, fmt.Errorf("cluster %q is not running", item.Name)
	}
	capability := s.hypervisor.Capabilities().Suspend
	if !capability.Supported {
		return ClusterSummary{}, fmt.Errorf("%w: %s", hypervisor.ErrUnsupported, capability.Reason)
	}
	s.cancelProvisionLocked(item.Name)
	s.invalidateStoragePhaseLocked(item.Name)
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		return ClusterSummary{}, err
	}
	nodes := s.vms[item.Name]
	var errs []error
	for name, machine := range nodes {
		savePath := saveStatePath(dir, name)
		retain, err := prepareSavedMachine(machine, savePath)
		if err != nil {
			errs = append(errs, fmt.Errorf("suspend %s: %w", name, err))
			_ = os.Remove(savePath) // no partial save left behind
			if !retain {
				delete(nodes, name)
			}
		}
	}
	if len(nodes) == 0 {
		delete(s.vms, item.Name)
	}
	if len(errs) > 0 {
		return ClusterSummary{}, errors.Join(errs...)
	}
	return summary(item, false), nil
}

// resumeCluster brings a suspended cluster back: each node restores from its
// saved state, or cold-boots with a warning if the save is missing/corrupt.
func (s *Server) resumeCluster(raw json.RawMessage) (ClusterSummary, error) {
	var args startArgs
	if err := decodeArgs(raw, &args); err != nil {
		return ClusterSummary{}, err
	}
	item, err := cluster.Load(args.Name)
	if err != nil {
		return ClusterSummary{}, err
	}
	if s.clusterRunning(item.Name) {
		return ClusterSummary{}, fmt.Errorf("cluster %q is already running", item.Name)
	}
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		return ClusterSummary{}, err
	}
	// Resume answers to the same trio start does (#368). The projected-start
	// gate alone is inert here: it stands down when no other guest is resident,
	// and a resume target is by definition not running — so a lone suspended
	// cluster would resume straight into a host `cluster start` would refuse.
	// checkOvercommit sizes the restored footprint against host RAM and
	// checkHostPressure judges the host as it stands; --force is the documented
	// override for all three, as it is everywhere else.
	overcommitWarning, err := s.checkOvercommit(clusterMemoryMiB(item), args.Force)
	if err != nil {
		return ClusterSummary{}, err
	}
	pressureWarnings, err := s.checkHostPressure(dir, args.Force)
	if err != nil {
		return ClusterSummary{}, err
	}
	// A resume re-admits every suspended node's full allocation beside whatever
	// guests are already resident, which is the concurrent bringup the
	// projected-start gate exists for (#334).
	if bootingMiB := s.stoppedNodeMemoryMiB(item); bootingMiB > 0 {
		provisionStartWarnings, err := s.checkProvisionStart(dir, bootingMiB, args.Force)
		if err != nil {
			return ClusterSummary{}, err
		}
		pressureWarnings = append(pressureWarnings, provisionStartWarnings...)
	}
	// Suspend deliberately leaves the cluster's bridge up, so its own subnet
	// occupancy is expected here: inspect for advisory findings only, never
	// re-decide a subnet the cluster already owns (#271).
	subnetWarning, err := cluster.AttachedSubnetWarning(item.SubnetIndex, s.hostSubnetSources())
	if err != nil {
		return ClusterSummary{}, err
	}
	// Measured before the batch consumes the saves it dates.
	driftWarning := clockDriftWarning(dir, time.Now())
	nodes := s.vms[item.Name]
	if nodes == nil {
		nodes = make(map[string]hypervisor.Machine)
		s.vms[item.Name] = nodes
	}
	var attempted []string
	warnings, err := resumeNodeBatch(item.Nodes, func(node cluster.Node) (resumedNode, error) {
		savePath := saveStatePath(dir, node.Name)
		_, saveErr := os.Stat(savePath)
		var fallbackErr error
		delete(nodes, node.Name)
		machine, err := s.launchMachine(item, node, &hypervisor.Restore{
			Path: savePath,
			Fallback: func(err error) {
				fallbackErr = err
			},
		})
		if err != nil {
			return resumedNode{}, fmt.Errorf("resume %s: %w", node.Name, err)
		}
		nodes[node.Name] = machine
		attempted = append(attempted, node.Name)
		var nodeWarning string
		if fallbackErr != nil {
			nodeWarning = coldBootWarning(node.Name, saveErr != nil, fallbackErr)
			log.Printf("resume %s: %v", node.Name, fallbackErr)
		}
		return resumedNode{savePath: savePath, warning: nodeWarning}, nil
	}, func() error {
		return s.closeNodes(item.Name, nodes, attempted)
	})
	if err != nil {
		return ClusterSummary{}, err
	}
	go s.bindMirrors(item.SubnetIndex) // resume bypasses start(); rebind the gateway
	result := summary(item, true)
	result.setWarnings(append(append([]string{subnetWarning, overcommitWarning}, pressureWarnings...), append(warnings, driftWarning)...)...)
	return result, nil
}

// minReportedClockDrift is the suspend window below which the guest clocks are
// close enough to the host that saying so would be noise.
const minReportedClockDrift = 5 * time.Second

// clockDriftWarning tells the operator how far the restored guests' clocks are
// behind the host. A saved guest's clock stops with it and restarts from the
// saved value, so the drift is exactly the wall-clock length of the suspend —
// which the save files themselves date, no extra state needed. The Talos
// machine API of the supported window (machinery v1.13) exposes only Time and
// TimeCheck, both read-only comparisons against an NTP server: there is no RPC
// that steps or slews the guest clock, so tbx cannot force the resync and says
// so instead of pretending (#416). Reported from the oldest save, the node that
// has been frozen longest.
func clockDriftWarning(dir string, now time.Time) string {
	matches, err := filepath.Glob(filepath.Join(dir, "*"+saveStateSuffix))
	if err != nil || len(matches) == 0 {
		return ""
	}
	var suspendedAt time.Time
	for _, path := range matches {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if suspendedAt.IsZero() || info.ModTime().Before(suspendedAt) {
			suspendedAt = info.ModTime()
		}
	}
	drift := now.Sub(suspendedAt).Round(time.Second)
	if suspendedAt.IsZero() || drift < minReportedClockDrift {
		return ""
	}
	return fmt.Sprintf(
		"guest clocks resume about %s behind the host and tbx cannot force a resync; "+
			"Talos corrects them at its next NTP poll (minutes away, and never on an offline host) — "+
			"reboot the cluster with stop/start if a workload cannot tolerate the gap",
		drift,
	)
}

// coldBootWarning explains a cold boot the way the daemon log already does:
// with the hypervisor's own reason. A warning that only says the restore
// failed leaves the operator nothing to act on (#291).
func coldBootWarning(nodeName string, saveMissing bool, cause error) string {
	if saveMissing {
		return fmt.Sprintf("%s: no saved state found; cold-booting instead", nodeName)
	}
	warning := fmt.Sprintf("%s: saved state could not be restored; cold-booting instead", nodeName)
	summary := platformErrorSummary(cause)
	if summary == "" {
		return warning + " (details: ~/.talosbox/tbxd.log)"
	}
	return fmt.Sprintf("%s: %s (details: ~/.talosbox/tbxd.log)", warning, summary)
}

// maxPlatformErrorSummary bounds a terminal-facing cause. Anything longer is
// detail the daemon log holds in full.
const maxPlatformErrorSummary = 200

// platformErrorSummary reduces a hypervisor cause to one terminal line. Cocoa's
// NSError renders as a multi-line plist once its UserInfo is expanded, so a
// three-node resume dumped three plists over the warnings the operator was
// meant to read. Keep the description, drop the dump: the warning already
// points at tbxd.log, which still logs the cause verbatim (#312).
func platformErrorSummary(cause error) string {
	if cause == nil {
		return ""
	}
	summary := cause.Error()
	if index := strings.IndexAny(summary, "\n\r"); index >= 0 {
		summary = summary[:index]
	}
	// " UserInfo={" opens the plist dump; everything after it is log material.
	if index := strings.Index(summary, " UserInfo={"); index >= 0 {
		summary = summary[:index]
	}
	summary = strings.TrimSpace(summary)
	if runes := []rune(summary); len(runes) > maxPlatformErrorSummary {
		cut := strings.TrimRight(strings.TrimSpace(string(runes[:maxPlatformErrorSummary])), "“")
		summary = closeTruncatedQuotes(strings.TrimSpace(cut) + "...")
	}
	return summary
}

// closeTruncatedQuotes appends the closers a truncation left open. Cutting at a
// fixed rune count lands inside the platform's own quoted message often enough
// that operators saw a dangling opening quote instead of a readable line
// (#361). Both curly and straight quoting reach us: Cocoa quotes its
// descriptions with ", while the messages it wraps sometimes use “ ”.
func closeTruncatedQuotes(summary string) string {
	var pending []rune
	for _, r := range summary {
		switch r {
		case '“':
			pending = append(pending, '”')
		case '”', '"':
			if n := len(pending); n > 0 && pending[n-1] == r {
				pending = pending[:n-1] // closes the quote this opened
			} else if r == '"' {
				pending = append(pending, '"') // straight quotes toggle
			}
		}
	}
	var builder strings.Builder
	builder.WriteString(summary)
	for i := len(pending) - 1; i >= 0; i-- {
		builder.WriteRune(pending[i])
	}
	return builder.String()
}

type resumedNode struct {
	savePath string
	warning  string
}

// resumeNodeBatch is the cluster-wide commit boundary: a failed node rolls
// back attempted VMs without consuming any saved states, while full success
// consumes the complete batch together.
func resumeNodeBatch[T any](nodes []T, resume func(T) (resumedNode, error), rollback func() error) ([]string, error) {
	var savePaths, warnings []string
	for _, node := range nodes {
		result, err := resume(node)
		if err != nil {
			return nil, errors.Join(err, rollback())
		}
		savePaths = append(savePaths, result.savePath)
		if result.warning != "" {
			warnings = append(warnings, result.warning)
		}
	}
	removeSaveStateFiles(savePaths)
	return warnings, nil
}

// removeSaveStateFiles commits a successful cluster-wide resume. Callers keep
// the batch intact on rollback so a later resume can retry every saved node.
func removeSaveStateFiles(paths []string) {
	for _, path := range paths {
		_ = os.Remove(path)
	}
}

// prepareSavedMachine leaves a successfully-saved VM stopped but otherwise
// intact. On failure, retain reports whether the daemon must keep tracking the
// machine. A failed stop is always retained because Close does not prove that
// the machine stopped.
func prepareSavedMachine(machine hypervisor.Machine, savePath string) (retain bool, err error) {
	if err := machine.Suspend(context.Background(), savePath); err != nil {
		closeErr := closeMachine(machine)
		return closeErr != nil, errors.Join(err, closeErr)
	}
	if err := stopMachine(machine); err != nil {
		closeErr := machine.Close()
		return true, errors.Join(err, closeErr)
	}
	return true, nil
}

// saveStateSuffix names every file suspend writes, so the presence of saved
// memory can be detected without knowing the node names.
const saveStateSuffix = ".vzstate"

func saveStatePath(dir, node string) string {
	return filepath.Join(dir, node+saveStateSuffix)
}

// discardSavedState drops a node's suspended memory before it cold-boots. A
// save is only consumed by a successful resume, so a start that boots the node
// from disk would otherwise leave a stale save behind: status keeps reporting
// the cluster Suspended and the resume hint invites a restore onto memory that
// no longer matches what is running. It reports whether a save was discarded,
// plus an operator-visible warning when a save was found but could not be
// removed — the cold boot proceeds either way, and the survivor would silently
// resurrect the suspended status and the resume hint.
func discardSavedState(dir, nodeName string) (bool, string) {
	path := saveStatePath(dir, nodeName)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, ""
		}
		// The save may well be there; a stat that failed for any other reason
		// (permissions, an unreadable directory) proves nothing, and silently
		// treating it as absent would leave the survivor to resurrect the
		// suspended status and the resume hint.
		log.Printf("discard saved state %s: %v", path, err)
		return false, undiscardedSaveStateWarning(nodeName, err)
	}
	if err := os.Remove(path); err != nil {
		log.Printf("discard saved state %s: %v", path, err)
		return false, undiscardedSaveStateWarning(nodeName, err)
	}
	log.Printf("discarded saved state %s: cold boot", path)
	return true, ""
}

// discardClusterSavedStates drops every save in a cluster directory, node names
// unknown. A snapshot restore rewrites the node disks underneath the saved
// memory, so every save in the directory is invalid afterwards — including one
// belonging to a node the snapshot does not even contain. It reports whether
// anything was discarded plus one warning per save that survived, exactly like
// the per-node discard.
func discardClusterSavedStates(dir string) (bool, []string) {
	matches, err := filepath.Glob(filepath.Join(dir, "*"+saveStateSuffix))
	if err != nil {
		log.Printf("discard saved states in %s: %v", dir, err)
		return false, []string{undiscardedSaveStateWarning("this cluster", err)}
	}
	var discarded bool
	var failures []string
	for _, path := range matches {
		if err := os.Remove(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			log.Printf("discard saved state %s: %v", path, err)
			node := strings.TrimSuffix(filepath.Base(path), saveStateSuffix)
			failures = append(failures, undiscardedSaveStateWarning(node, err))
			continue
		}
		log.Printf("discarded saved state %s: disks were replaced", path)
		discarded = true
	}
	return discarded, failures
}

// discardedSaveStateWarning tells the operator that a cold boot threw suspended
// memory away, because the discard is otherwise invisible: the next status just
// stops saying Suspended.
func discardedSaveStateWarning(subject string) string {
	return fmt.Sprintf("discarded suspended memory state; %s cold-booted", subject)
}

// undiscardedSaveStateWarning is the other half: the cold boot happened but the
// save outlived it, so status will keep calling the node suspended and the hint
// will keep offering a resume onto memory the running VM no longer matches.
func undiscardedSaveStateWarning(nodeName string, err error) string {
	return fmt.Sprintf(
		"could not discard suspended memory state for %s: %v; ignore the suspended status and do not resume",
		nodeName, err,
	)
}

// nodeHasSavedState reports whether one node's own memory is saved on disk.
// suspend only writes a save for nodes that were running, so this is what
// separates the members a resume restores from those it cold-boots.
func nodeHasSavedState(clusterName, nodeName string) bool {
	dir, err := cluster.Dir(clusterName)
	if err != nil {
		return false
	}
	_, err = os.Stat(saveStatePath(dir, nodeName))
	return err == nil
}

// clusterHasSavedState reports whether a cluster still holds suspended VM
// memory on disk. Suspension is otherwise invisible after a daemon restart —
// the tracked VMs are gone — so this on-disk signal is what tells a client that
// restarting tbxd would discard the cluster's saved memory.
func clusterHasSavedState(name string) bool {
	dir, err := cluster.Dir(name)
	if err != nil {
		return false
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*"+saveStateSuffix))
	return err == nil && len(matches) > 0
}
