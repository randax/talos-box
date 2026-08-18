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

	// provisionStartSwapUsedBytes is the absolute companion to the percentage.
	// The percentage alone cannot tell a 2 GiB dynamic swapfile that macOS grew
	// once and never released from the multi-GiB file a host only ever ends up
	// with after sustained real pressure — 82% of 2 GiB is 1.7 GiB, which is
	// noise on a host with tens of gigabytes free (#231, #284), while #334's
	// host was 7.3 GiB into an 8 GiB file. A swap footprint this large *is* the
	// #84 corruption precondition: the pages are already out on disk and a
	// second bringup's boot-time faults have to compete with paging them back.
	// So it arms the rule without waiting for the kernel to still be reporting
	// pressure — macOS drops back to normal as soon as the burst that filled the
	// file ends, which is exactly the state #334 recorded at preflight. It does
	// still ask one thing of the present: that RAM is scarce now too, see
	// swapBlocksProvisionStart.
	provisionStartSwapUsedBytes = 4 << 30
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
	// HostTotalMiB is the host's physical memory, the scale HostFreeMiB is read
	// against: 8 GiB free is roomy on a 32 GiB host and nearly nothing on a
	// 128 GiB one. Zero means it could not be measured, and the swap rule then
	// stays armed rather than assume there is room.
	HostTotalMiB int
	// ReserveMiB is the balloon controller's host-memory reserve: the headroom
	// it tries to keep free once it can act at all.
	ReserveMiB int
	// MemoryPressure is the kernel's own verdict on the host right now. It is
	// what separates a swap file that is merely sticky from one the host is
	// actively leaning on.
	MemoryPressure MemoryPressure
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
//   - Swap: a host already deep into its swap file cannot absorb the
//     allocation burst of a second bringup. "Deep" is two readings, not one:
//     the file must be at least provisionStartSwapUsedPercent full *and* the
//     host must either still be off normal memory pressure or be carrying at
//     least provisionStartSwapUsedBytes of swapped-out pages while its free
//     memory is scarce (provisionStartMemoryIsScarce). macOS grows a swap file
//     on demand and keeps it allocated long after the pressure that filled it
//     cleared, so a sticky swapfile on a host with tens of gigabytes free and
//     normal pressure says nothing about capacity, at 2 GiB or at 8 (#231,
//     #284) — but #334's 7.3 GiB of an 8 GiB file said everything even though
//     the preflight host-pressure read came back PASS, which is why the
//     absolute rule exists alongside the pressure one. An unmeasurable pressure
//     reading counts as not-normal, the same way memoryFinding treats it.
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
	if swapBlocksProvisionStart(in) {
		findings = append(findings, Finding{
			Severity: SeverityBlock,
			Detail: fmt.Sprintf(
				"host swap is %d%% used (%.1f GiB of %.1f GiB, at or past the %d%% ceiling) with memory pressure %s and %d MiB of guests already running;"+
					" %s the %d MiB about to boot has nowhere to go, and %s",
				percentUsed(in.Swap), gib(in.Swap.usedBytes()), gib(in.Swap.TotalBytes), provisionStartSwapUsedPercent,
				provisionStartPressureLabel(in.MemoryPressure), in.RunningVMMiB,
				provisionStartSwapArmedBy(in), in.NewVMMiB, memoryConsequence,
			),
			Remedy: memoryRemedy,
		})
	}
	return findings
}

// swapBlocksProvisionStart applies the swap rule's two-reading test: a swap
// file at or past the percentage ceiling, armed either by the kernel still
// reporting non-normal pressure or by the swapped-out footprint being large
// enough to be a bringup hazard on its own.
func swapBlocksProvisionStart(in ProvisionStart) bool {
	if in.Swap.TotalBytes == 0 || percentUsed(in.Swap) < provisionStartSwapUsedPercent {
		return false
	}
	if in.MemoryPressure != MemoryPressureNormal {
		return true
	}
	return in.Swap.usedBytes() >= provisionStartSwapUsedBytes && provisionStartMemoryIsScarce(in)
}

// provisionStartMemoryIsScarce reads the absolute swap arm's second half. A
// multi-GiB swap file next to mostly-free RAM and normal pressure is a macOS
// artifact — the file was grown by some burst, never returned, and nothing is
// competing for memory now — so refusing on it alone re-runs #231's false
// positive at a larger file size: 8 GiB swapped out on a 64 GiB host with
// 40 GiB free says nothing about whether the next guest can boot. The same
// footprint while RAM is *also* scarce is live #84 pressure, which is the state
// #334 recorded. Half the host is the line, and it is a narrow one by design:
// this rule only ever runs with guests already resident, which is usually
// enough on its own to put free memory below half. What the check therefore
// buys is the one case that would otherwise be refused — a large stale swap
// file while most of RAM is still genuinely free — and once guests have claimed
// half the host it stands aside and the absolute arm decides.
//
// A host whose free or total memory could not be measured keeps the arm active:
// the swap footprint alone is the older, blunter reading, and falling back to it
// is the fail-safe direction.
func provisionStartMemoryIsScarce(in ProvisionStart) bool {
	if in.HostFreeMiB <= 0 || in.HostTotalMiB <= 0 {
		return true
	}
	return in.HostFreeMiB*2 < in.HostTotalMiB
}

// provisionStartSwapArmedBy names which half of the swap rule fired, so the
// operator can re-derive the refusal from the same two numbers the gate read.
func provisionStartSwapArmedBy(in ProvisionStart) string {
	if in.MemoryPressure != MemoryPressureNormal {
		return "pressure is off normal, so"
	}
	return fmt.Sprintf(
		"a swapped-out footprint at or past %.0f GiB with host memory this scarce only happens after sustained real pressure even when the kernel has since returned to normal, so",
		gib(provisionStartSwapUsedBytes),
	)
}

// provisionStartPressureLabel names the pressure reading in the refusal. An
// unmeasurable one is reported as such rather than silently omitted: it is part
// of why the rule fired.
func provisionStartPressureLabel(pressure MemoryPressure) string {
	if pressure == MemoryPressureUnknown {
		return "unmeasurable"
	}
	return pressure.String()
}
