package hostpressure

import "fmt"

const (
	// provisionStartSwapUsedPercent is the swap-used ceiling for starting new
	// guests (#334). It is deliberately lower than the steady-state thresholds
	// in Assess: #231 removed swap-used% from the steady-state verdict because
	// macOS keeps swap allocated long after pressure clears, so a high
	// percentage says nothing about a *running* host. Bringup is the opposite
	// situation — a booting guest faults in its whole nominal working set
	// within seconds and virtio_balloon is not loaded yet, so nothing can
	// reclaim — and a host that is already 80% into its swap file while running
	// other guests has nowhere to put those pages. This threshold therefore
	// exists only on the provision-start path, only while guests are already
	// running, and must not be folded back into Assess.
	provisionStartSwapUsedPercent = 80
)

// ProvisionStart is the host state measured at guest-start admission, plus the
// memory the pending operation is about to commit.
//
// Nominal memory — not current guest usage — is the right unit here: a guest
// that has not booted yet will touch its whole configured allocation, and the
// balloon controller cannot shrink it back until the guest loads
// virtio_balloon (~90s into boot on Talos), which is exactly the window in
// which #84-style guest panics happen.
type ProvisionStart struct {
	// RunningVMMiB is the nominal memory of guests already running.
	RunningVMMiB int
	// NewVMMiB is the nominal memory the pending operation starts.
	NewVMMiB int
	// HostFreeMiB is host memory available right now. Zero means it could not
	// be measured, and the headroom rule is then skipped rather than guessed.
	HostFreeMiB int
	// ReserveMiB is the balloon controller's host-memory reserve: the headroom
	// it tries to keep free once it can act at all.
	ReserveMiB int
	// Swap is the host swap file's capacity and free bytes. A zero TotalBytes
	// means swap is disabled or unmeasurable, and the swap rule is skipped.
	Swap Usage
}

// AssessProvisionStart classifies whether it is safe to *start* the guests an
// operation is about to launch. It complements Assess, which judges the host as
// it stands: Assess cannot see the allocation that is about to arrive, and the
// nominal-versus-total overcommit ceiling cannot see how much of the host is
// already spoken for by non-VM processes. #334 slipped through precisely that
// gap — a second 3×6 GiB create on a 32 GiB host passed both the overcommit
// ceiling and a PASS host-pressure reading, then drove host swap to 7.3/8 GiB
// and panicked the new cluster's workers mid-boot.
//
// Two rules, both SeverityBlock so callers refuse unless forced:
//
//   - Headroom: the binding constraint is measured free memory, not the
//     configured ceiling. Starting NewVMMiB must leave at least the balloon
//     reserve free.
//   - Swap: a host already deep into its swap file cannot absorb the allocation
//     burst of a second bringup.
//
// Both rules apply only while guests are already running, which is deliberate.
// A lone cluster on an otherwise idle host is the case the balloon reserve and
// the controller are sized for, and macOS keeps swap allocated long after
// pressure clears — so on an idle host a high swap percentage says nothing
// (#231, #284) and blocking on it would refuse healthy creates. Guests already
// resident change both facts: free memory is genuinely spoken for, and a second
// bringup's boot-time faults are what tip such a host into thrashing.
func AssessProvisionStart(in ProvisionStart) []Finding {
	if in.RunningVMMiB <= 0 || in.NewVMMiB <= 0 {
		return nil
	}
	var findings []Finding
	if in.HostFreeMiB > 0 {
		projectedFreeMiB := in.HostFreeMiB - in.NewVMMiB
		if projectedFreeMiB < in.ReserveMiB {
			findings = append(findings, Finding{
				Severity: SeverityBlock,
				Detail: fmt.Sprintf(
					"starting %d MiB of guests while %d MiB of guests are already running leaves %d MiB free of the %d MiB measured now, below the %d MiB balloon reserve;"+
						" a booting guest claims its full allocation before virtio_balloon can reclaim anything, so the host swaps and %s",
					in.NewVMMiB, in.RunningVMMiB, projectedFreeMiB, in.HostFreeMiB, in.ReserveMiB, memoryConsequence,
				),
				Remedy: memoryRemedy,
			})
		}
	}
	if in.Swap.TotalBytes > 0 && percentUsed(in.Swap) >= provisionStartSwapUsedPercent {
		findings = append(findings, Finding{
			Severity: SeverityBlock,
			Detail: fmt.Sprintf(
				"host swap is %d%% used (%.1f GiB free of %.1f GiB) with %d MiB of guests already running;"+
					" the %d MiB about to boot has nowhere to go, and %s",
				percentUsed(in.Swap), gib(in.Swap.AvailableBytes), gib(in.Swap.TotalBytes),
				in.RunningVMMiB, in.NewVMMiB, memoryConsequence,
			),
			Remedy: memoryRemedy,
		})
	}
	return findings
}
