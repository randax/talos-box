# QA Scenario: Suspend/resume × stateful storage

| | |
|---|---|
| **Tier** | Combination scenario (suspend gates × Longhorn data integrity) |
| **Platform** | macOS 14+ / Linux QEMU 8.2+ (older QEMU: capability-refusal only — record and stop) |
| **Estimated duration** | 45–60 min |
| **Destructive** | Creates and destroys cluster `qa-sus`; kills the daemon once (macOS charter) |
| **Runbook version** | against talos-box main @ the commit recorded in your report |

## How to execute this runbook (agent instructions)

You are running QA, not demos. For every charter: run the steps exactly, compare against **Expected observations**, and record **PASS**, **FAIL**, or **PASS-with-friction**. Friction — confusing messages, doc/behavior mismatch, missing knowledge, misleading output — is a first-class result even on passing charters. No improvised recovery: capture the **On failure** evidence, mark FAIL, continue unless a dependency broke.

**Report destination**: one `qa-run` issue, title `QA scenario-suspend-storage <platform> <date>`.

## Preflight

BLOCKED unless: doctor clean and the platform suspend gate is open (macOS 14+; Linux doctor `qemu` line shows suspend available — if not, run only the refusal check from deep-cluster-state C3 and report the rest SKIPPED); no cluster `qa-sus`; ≥ 10 GiB RAM.

## Charters

### C1 — Stateful baseline

**Goal**: a provisioned Longhorn cluster with committed data.

Steps:
1. `tbx cluster create qa-sus --cni cilium --csi longhorn`; wait for storage-ready.
2. Create a PVC and a writer pod that appends timestamped lines to the volume every second; let it run ≥ 60 s; record the last line. Base it on the [PSA-compliant test pod](deep-storage.md#psa-compliant-test-pod) (swap the command for the append loop) so the apply does not emit a `would violate PodSecurity "restricted:latest"` block — that block is a warning, not a rejection, and is not a finding.

Pass criteria: data flowing, last-written line recorded.

### C2 — Suspend under write load (depends on C1)

**Goal**: suspend captures a consistent whole-cluster moment with storage mid-write.

Steps:
1. With the writer still running, `tbx cluster suspend qa-sus`.
2. `tbx status qa-sus` — suspended state.

Expected observations: suspend succeeds while I/O is in flight; status is unambiguous about the suspended state.

Pass criteria: clean suspend under load.

On failure: capture suspend output, status.

### C3 — Same-daemon resume: no reboot, no data loss (depends on C2)

**Goal**: memory-preserving resume with intact Longhorn volumes.

Steps:
1. `tbx cluster resume qa-sus`.
2. Verify no reboot (console kernel timestamps continue; pods did not restart — check `kubectl get pods -o wide` RESTARTS unchanged).
3. Verify the writer resumed appending; read the volume: the pre-suspend tail is present with no gap other than the suspended wall-clock window, no corruption (lines well-formed).
4. Longhorn health: volumes report healthy replicas.

Pass criteria: no reboot, writer continues, volume intact and healthy.

On failure: capture console boot evidence, pod restarts, Longhorn volume state, the volume tail.

### C4 — Daemon-restart degradation with data **[macOS]** / cross-daemon restore **[Linux]** (depends on C3)

**Goal**: the degradation boundary never costs data.

Steps:
1. `tbx cluster suspend qa-sus` again (writer running).
2. **[macOS]** Restart tbxd (record method). `tbx cluster resume qa-sus` — expect the documented warning + graceful cold boot. After boot: cluster converges, Longhorn volumes recover to healthy, the volume contains everything written before the suspend (the cold boot loses memory, never disk).
3. **[Linux]** `systemctl --user restart tbxd` (or equivalent), then resume — expect memory-preserving restore (same QEMU identity); verify as in C3.

Expected observations: macOS degrades exactly as documented and storage survives crash-consistently; Linux restores across the daemon boundary.

Pass criteria: no data loss in either flavor; behavior matches the platform contract.

On failure: capture resume output, post-boot convergence timeline, volume tail vs pre-suspend record, Longhorn events.

### C5 — Destroy and cleanup (always run)

Steps: `tbx cluster destroy qa-sus --force` (expect the storage data-loss warning — record it); verify no residue.

Pass criteria: no residue.

## Report template

```markdown
## QA scenario-suspend-storage <platform> — <date>

- tbx version / commit; platform details (QEMU version on Linux):
- Preflight: OK | BLOCKED (<why>)

| Charter | Verdict | Duration | Notes |
|---|---|---|---|
| C1 stateful baseline | | | |
| C2 suspend under load | | | |
| C3 same-daemon resume | | | |
| C4 degradation w/ data | | | |
| C5 destroy | | | |

### Friction log
### Failures
```
