// Package hostpressure detects host resource exhaustion that can make guest
// writes unsafe even when the requested VM memory is not overcommitted.
package hostpressure

import (
	"fmt"
	"strings"
)

const (
	extremeSwapUsedPercent       = 90
	steadySwapUsedPercent        = 80
	extremeDataVolumeUsedPercent = 95
	bytesPerGiB                  = 1 << 30
	// lowFreeSwapBytes is the absolute headroom below which swap counts as
	// exhausted regardless of percentage. Percent-used alone hid the readings
	// that corrupted guest disks (#284): QA measured 87-88% used — a PASS by the
	// 90% rule — with only ~1.3 GiB of swap actually free, while the kernel
	// already reported warning memory pressure. 1.5 GiB is one Talos guest's
	// working set plus margin: below that, a single VM's dirty pages can fill
	// the remaining swap, which is when guests reset mid-write.
	lowFreeSwapBytes = 3 * bytesPerGiB / 2
	// substantialSwapUsedPercent gates the low-free-swap escalation: macOS
	// grows vm.swapusage on demand, so a small, mostly-free allocation is the
	// normal reading right when a host merely begins to swap. Requiring
	// substantial use keeps that state at Warn while both QA-observed
	// corruption readings (87-88% used) still block.
	substantialSwapUsedPercent = 75
)

// Usage describes capacity and immediately available bytes for one resource.
type Usage struct {
	TotalBytes     uint64
	AvailableBytes uint64
}

// MemoryPressure is the kernel's current memory-pressure verdict.
type MemoryPressure int

const (
	MemoryPressureUnknown MemoryPressure = iota
	MemoryPressureNormal
	MemoryPressureWarning
	MemoryPressureCritical
)

func (p MemoryPressure) String() string {
	switch p {
	case MemoryPressureNormal:
		return "normal"
	case MemoryPressureWarning:
		return "warning"
	case MemoryPressureCritical:
		return "critical"
	default:
		return "unknown"
	}
}

// Snapshot is the host capacity relevant to safe VM startup.
type Snapshot struct {
	Swap           Usage
	DataVolume     Usage
	MemoryPressure MemoryPressure
	// FreeMemoryMiB is host memory available right now. Assess weighs it against
	// the balloon reserve plus the guests about to start, because macOS can keep
	// old swap allocated after the pressure that filled it has cleared (#483).
	// Zero means it was not measured, and it is reported as such.
	FreeMemoryMiB  int
	TotalMemoryMiB int
	CompressorMiB  int
}

// Summary renders the numbers behind a host-pressure verdict — free memory,
// swap in use, and free space on the volume holding ~/.talosbox — plus the
// headroom arithmetic the provision-start gate will apply to the next bringup.
// A PASS with no numbers has to be re-derived by hand with memory_pressure,
// sysctl vm.swapusage and df before it can be attested, and it says nothing
// about whether the *next* cluster will be admitted (#420, #397).
func (s Snapshot) Summary(reserveMiB int) string {
	parts := []string{"free memory unmeasured"}
	if s.FreeMemoryMiB > 0 {
		parts[0] = fmt.Sprintf("%d MiB free memory", s.FreeMemoryMiB)
	}
	if s.TotalMemoryMiB > 0 {
		parts = append(parts, fmt.Sprintf("%d MiB in compressor of %d MiB physical memory", s.CompressorMiB, s.TotalMemoryMiB))
	} else if s.CompressorMiB > 0 {
		parts = append(parts, fmt.Sprintf("%d MiB in compressor", s.CompressorMiB))
	}
	if s.Swap.TotalBytes == 0 {
		parts = append(parts, "swap disabled or unmeasurable")
	} else {
		parts = append(parts, fmt.Sprintf("%.1f GiB of %.1f GiB swap in use (%d%%)", gib(s.Swap.UsedBytes()), gib(s.Swap.TotalBytes), s.Swap.PercentUsed()))
	}
	if s.DataVolume.TotalBytes == 0 {
		parts = append(parts, "~/.talosbox volume unmeasurable")
	} else {
		parts = append(parts, fmt.Sprintf("%.1f GiB free on the ~/.talosbox volume", gib(s.DataVolume.AvailableBytes)))
	}
	return strings.Join(parts, ", ") + "; " + s.headroomClause(reserveMiB)
}

// headroomClause states what the provision-start gate would admit from this
// reading: starting guests beside running ones must leave the balloon reserve
// free, so the room for the *next* cluster is measured free minus the reserve —
// the arithmetic a host that reads PASS and is then refused a second cluster
// never got to see (#397).
func (s Snapshot) headroomClause(reserveMiB int) string {
	if s.FreeMemoryMiB <= 0 || reserveMiB <= 0 {
		return "starting guests beside running ones must leave the " +
			fmt.Sprintf("%d MiB", reserveMiB) + " balloon reserve free, which needs a free-memory reading this host did not give"
	}
	roomMiB := s.FreeMemoryMiB - reserveMiB
	if roomMiB < 0 {
		roomMiB = 0
	}
	return fmt.Sprintf(
		"starting guests beside running ones must leave the %d MiB balloon reserve free, so there is room for %d MiB of new guests right now,"+
			" plus whatever the balloon controller can take back from the guests already running",
		reserveMiB, roomMiB,
	)
}

// Severity ranks a finding for the two consumers of Assess.
type Severity int

const (
	// SeverityWarn is advisory: operations proceed and diagnostics report WARN.
	SeverityWarn Severity = iota + 1
	// SeverityBlock refuses guest starts unless --force is given, and
	// diagnostics report FAIL.
	SeverityBlock
)

const (
	memoryConsequence = "guest VMs may reset and corrupt Talos EPHEMERAL data"
	memoryRemedy      = "free host memory: run `tbx down` to stop running clusters, or quit memory-heavy apps, then retry"
	storageRemedy     = "free host storage: run `tbx cache prune --all`, or destroy unused clusters, then retry"
)

// MemoryRemedy is the runnable remediation every host-memory refusal carries.
// It is exported so a caller that refuses on its own memory arithmetic — the
// daemon, when a pre-balloon it was counting on fails to apply — ends its
// message the same way this package's findings do.
const MemoryRemedy = memoryRemedy

// Finding is one host-pressure condition: what was measured, and what to run
// about it.
type Finding struct {
	Severity Severity
	// Detail states the condition with the measured numbers.
	Detail string
	// Remedy is a runnable remediation line.
	Remedy string
}

// String renders the finding for operation warnings and refusals.
func (f Finding) String() string { return f.Detail + "; " + f.Remedy }

// DoctorDetail renders the finding for diagnostics: the same numbers the gate
// prints, the same runnable remedy, plus the override semantics when the
// finding is what makes an operation refuse.
func (f Finding) DoctorDetail() string {
	if f.Severity != SeverityBlock {
		return f.String()
	}
	return f.String() + "; --force overrides this gate and risks guest-disk corruption"
}

// Assess is the single host-pressure classification. Every operation that
// starts guests refuses on SeverityBlock findings unless forced, and tbx doctor
// reports those same findings as FAIL, so the gate and the diagnostic can never
// disagree about the same reading.
//
// Memory pressure is the primary memory signal. Elevated pressure is always
// reportable, and swap — measured both as a percentage and as absolute free
// bytes — escalates it only when the host lacks the required free memory.
// requiredFreeMiB is the balloon reserve plus the memory of guests the caller
// is about to start; doctor passes the reserve alone.
func Assess(snapshot Snapshot, requiredFreeMiB int) []Finding {
	var findings []Finding
	if finding, ok := memoryFinding(snapshot, requiredFreeMiB); ok {
		findings = append(findings, finding)
	}
	if percentUsed(snapshot.DataVolume) >= extremeDataVolumeUsedPercent {
		findings = append(findings, Finding{
			Severity: SeverityBlock,
			Detail: fmt.Sprintf(
				"talosbox data volume is %d%% used (%.1f GiB free); low host storage can corrupt guest writes after a reset",
				percentUsed(snapshot.DataVolume), gib(snapshot.DataVolume.AvailableBytes),
			),
			Remedy: storageRemedy,
		})
	}
	return findings
}

// memoryFinding classifies the host's memory situation as one finding: pressure
// and swap are two readings of the same condition, so reporting them separately
// would only duplicate the remedy.
//
// The rules, in the order they were learned:
//   - critical pressure blocks outright;
//   - warning pressure is always at least advisory — it is the reading that was
//     live while guest disks were being corrupted (#284) — and blocks once swap
//     is exhausted by either measure unless measured free memory covers the
//     balloon reserve plus the guests about to start;
//   - swap at or past extremeSwapUsedPercent keeps its original verdicts: a
//     block while pressure is elevated or unmeasurable and headroom is absent,
//     advisory while pressure is normal or measured headroom is sufficient;
//   - low absolute free swap only escalates alongside a *measured* elevated
//     pressure AND substantial swap use: macOS grows vm.swapusage on demand,
//     so a small, mostly-free allocation is the normal reading right when a
//     host merely begins to swap — that must not block. The ≥75% floor keeps
//     both QA-observed corruption readings (87–88% used) blocking.
func memoryFinding(snapshot Snapshot, requiredFreeMiB int) (Finding, bool) {
	swapEnabled := snapshot.Swap.TotalBytes > 0
	swapExhausted := swapEnabled && percentUsed(snapshot.Swap) >= extremeSwapUsedPercent
	swapNearlyFull := swapEnabled && snapshot.Swap.AvailableBytes < lowFreeSwapBytes &&
		percentUsed(snapshot.Swap) >= substantialSwapUsedPercent
	hasRequiredFreeMemory := snapshot.FreeMemoryMiB > 0 && snapshot.FreeMemoryMiB >= requiredFreeMiB

	var severity Severity
	switch snapshot.MemoryPressure {
	case MemoryPressureCritical:
		severity = SeverityBlock
	case MemoryPressureWarning:
		severity = SeverityWarn
		if (swapExhausted || swapNearlyFull) && !hasRequiredFreeMemory {
			severity = SeverityBlock
		}
	case MemoryPressureNormal:
		if _, ok := SteadySwapFinding(snapshot); ok {
			// sticky swap with memory to spare is not a fault, but it is one
			// pressure tick away from one, so it stays reportable
			severity = SeverityWarn
		}
	default:
		if swapExhausted {
			severity = SeverityWarn
			if !hasRequiredFreeMemory {
				severity = SeverityBlock
			}
		} else if _, ok := SteadySwapFinding(snapshot); ok {
			severity = SeverityWarn
		}
	}
	if severity == 0 {
		return Finding{}, false
	}
	headroomDecidesSeverity := (snapshot.MemoryPressure == MemoryPressureWarning && (swapExhausted || swapNearlyFull)) ||
		(snapshot.MemoryPressure == MemoryPressureUnknown && swapExhausted)
	return Finding{Severity: severity, Detail: memoryDetail(snapshot, requiredFreeMiB, headroomDecidesSeverity), Remedy: memoryRemedy}, true
}

// SteadySwapFinding returns the table/doctor advisory threshold independently
// of kernel pressure. Assess folds it into its one combined memory finding.
func SteadySwapFinding(snapshot Snapshot) (Finding, bool) {
	if snapshot.Swap.PercentUsed() < steadySwapUsedPercent {
		return Finding{}, false
	}
	return Finding{
		Severity: SeverityWarn,
		Detail: fmt.Sprintf("host swap is %d%% used (%.1f GiB of %.1f GiB, %.1f GiB free)",
			snapshot.Swap.PercentUsed(), gib(snapshot.Swap.UsedBytes()), gib(snapshot.Swap.TotalBytes), gib(snapshot.Swap.AvailableBytes)),
		Remedy: memoryRemedy,
	}, true
}

// memoryDetail states the condition with every number the gate measured, so an
// operator can check the same readings by hand.
func memoryDetail(snapshot Snapshot, requiredFreeMiB int, includeHeadroom bool) string {
	pressureClause := "memory pressure could not be measured"
	if snapshot.MemoryPressure != MemoryPressureUnknown {
		pressureClause = fmt.Sprintf("memory pressure is %s", snapshot.MemoryPressure)
	}
	if snapshot.Swap.TotalBytes == 0 {
		return "host " + pressureClause + "; " + memoryConsequence
	}
	usage := fmt.Sprintf(
		"host swap is %d%% used (%.1f GiB of %.1f GiB, %.1f GiB free)",
		percentUsed(snapshot.Swap), gib(snapshot.Swap.usedBytes()),
		gib(snapshot.Swap.TotalBytes), gib(snapshot.Swap.AvailableBytes),
	)
	if snapshot.TotalMemoryMiB > 0 {
		usage += fmt.Sprintf(" with %d MiB in compressor of %d MiB physical memory", snapshot.CompressorMiB, snapshot.TotalMemoryMiB)
	} else if snapshot.CompressorMiB > 0 {
		usage += fmt.Sprintf(" with %d MiB in compressor", snapshot.CompressorMiB)
	}
	if includeHeadroom {
		if snapshot.FreeMemoryMiB > 0 {
			usage += fmt.Sprintf(" with %d MiB free memory against %d MiB required", snapshot.FreeMemoryMiB, requiredFreeMiB)
		} else {
			usage += fmt.Sprintf(" with free memory unmeasured against %d MiB required", requiredFreeMiB)
		}
	}
	switch snapshot.MemoryPressure {
	case MemoryPressureNormal:
		return usage + " while memory pressure is normal; macOS keeps swap allocated after pressure clears," +
			" but starting more guests from here can push the host back into swapping, where " + memoryConsequence
	case MemoryPressureUnknown:
		return usage + " and " + pressureClause + "; " + memoryConsequence
	default:
		return usage + " while " + pressureClause + "; " + memoryConsequence
	}
}

func (u Usage) UsedBytes() uint64 {
	if u.AvailableBytes >= u.TotalBytes {
		return 0
	}
	return u.TotalBytes - u.AvailableBytes
}

func (u Usage) usedBytes() uint64 { return u.UsedBytes() }

func (u Usage) PercentUsed() int { return percentUsed(u) }

func percentUsed(usage Usage) int {
	if usage.TotalBytes == 0 {
		return 0
	}
	return int(float64(usage.usedBytes()) * 100 / float64(usage.TotalBytes))
}

func gib(bytes uint64) float64 {
	return float64(bytes) / bytesPerGiB
}
