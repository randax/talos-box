// Package hostpressure detects host resource exhaustion that can make guest
// writes unsafe even when the requested VM memory is not overcommitted.
package hostpressure

import "fmt"

const (
	extremeSwapUsedPercent       = 90
	extremeDataVolumeUsedPercent = 95
	bytesPerGiB                  = 1 << 30
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
// Memory pressure is the primary memory signal: macOS keeps swap allocated
// long after pressure clears, so a high swap-used percentage alone says
// nothing about current availability. Swap exhaustion is secondary — it blocks
// while pressure is elevated or could not be measured, and is advisory only
// while pressure is normal.
func Assess(snapshot Snapshot) []Finding {
	var findings []Finding
	if snapshot.MemoryPressure == MemoryPressureCritical {
		findings = append(findings, Finding{
			Severity: SeverityBlock,
			Detail:   "host memory pressure is critical; " + memoryConsequence,
			Remedy:   memoryRemedy,
		})
	}
	if percentUsed(snapshot.Swap) >= extremeSwapUsedPercent {
		findings = append(findings, swapFinding(snapshot))
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

func swapFinding(snapshot Snapshot) Finding {
	usage := fmt.Sprintf(
		"host swap is %d%% used (%.1f GiB of %.1f GiB)",
		percentUsed(snapshot.Swap), gib(snapshot.Swap.usedBytes()), gib(snapshot.Swap.TotalBytes),
	)
	// sticky swap with memory to spare is not a fault, but it is one pressure
	// tick away from one, so it stays reportable
	if snapshot.MemoryPressure == MemoryPressureNormal {
		return Finding{
			Severity: SeverityWarn,
			Detail: usage + " while memory pressure is normal; macOS keeps swap allocated after pressure clears," +
				" but starting more guests from here can push the host back into swapping, where " + memoryConsequence,
			Remedy: memoryRemedy,
		}
	}
	pressureClause := " and memory pressure could not be measured"
	if snapshot.MemoryPressure != MemoryPressureUnknown {
		pressureClause = fmt.Sprintf(" while memory pressure is %s", snapshot.MemoryPressure)
	}
	return Finding{
		Severity: SeverityBlock,
		Detail:   usage + pressureClause + "; " + memoryConsequence,
		Remedy:   memoryRemedy,
	}
}

func (u Usage) usedBytes() uint64 {
	if u.AvailableBytes >= u.TotalBytes {
		return 0
	}
	return u.TotalBytes - u.AvailableBytes
}

func percentUsed(usage Usage) int {
	if usage.TotalBytes == 0 {
		return 0
	}
	return int(float64(usage.usedBytes()) * 100 / float64(usage.TotalBytes))
}

func gib(bytes uint64) float64 {
	return float64(bytes) / bytesPerGiB
}
