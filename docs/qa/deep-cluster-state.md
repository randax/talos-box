# QA Runbook: Cluster state machine

| | |
|---|---|
| **Tier** | Deep (one feature area exercised against defaults) |
| **Platform** | macOS + Linux — suspend/resume gates differ per platform (charters marked) |
| **Estimated duration** | 60–75 min |
| **Destructive** | Creates and destroys cluster `qa-sta` and its snapshots |
| **Runbook version** | against talos-box main @ the commit recorded in your report |

## How to execute this runbook (agent instructions)

You are running QA, not demos. For every charter: run the steps exactly, compare against **Expected observations**, and record **PASS**, **FAIL**, or **PASS-with-friction**. Friction — confusing messages, doc/behavior mismatch, missing knowledge, misleading output — is a first-class result even on passing charters. No improvised recovery: capture the **On failure** evidence, mark FAIL, continue unless a dependency broke.

**Report destination**: one `qa-run` issue, title `QA deep-cluster-state <platform> <date>`.

## Preflight

BLOCKED unless: `tbx version` recorded; `tbx doctor` exits 0 — on Linux record the `qemu` doctor line (it states suspend availability; QEMU 6.2–8.1 hosts will legitimately refuse suspend, turning C3 into a capability-refusal check rather than a suspend test); no cluster `qa-sta`; ≥ 10 GiB free RAM; free disk per platform — **[Linux]** ≥ 40 GiB, because the snapshot path may fall back to full copies of every node disk; **[macOS/APFS]** ≥ 5 GiB, because APFS clonefiles make snapshot create and restore copy-on-write (measured disk delta ≈ 0). If `~/.talosbox` lives on a non-APFS volume on macOS, use the Linux floor.

## Charters

### C1 — Snapshot round-trip

**Goal**: cold whole-cluster snapshot create/restore/list/delete behaves as promised.

Steps:
1. `tbx cluster create qa-sta` (substrate-only default). Record node IPs.
2. `tbx snapshot create qa-sta baseline --yes` — expect the cluster to stop, snapshot, restart.
3. `tbx snapshot list qa-sta` — `baseline` with timestamp.
4. Make an observable change: `tbx node add qa-sta --role worker` (a 4th node).
5. `tbx snapshot restore qa-sta baseline --yes` — expect stop → restore → restart (~1 min cold boot).
6. `tbx status qa-sta` — the added node is gone; the original three are back at `maintenance` with their original IPs.
7. `tbx snapshot delete qa-sta baseline`; `tbx snapshot list qa-sta` empty.

Expected observations: create/restore pass through a stop (interactive confirmation suppressed by `--yes`); restore returns the whole set to the snapshot point as one crash-consistent unit; timing recorded (Linux full-copy fallback may be much slower — record which mechanism you observed via duration and disk usage).

Pass criteria: post-restore topology and IPs match the snapshot point exactly.

On failure: capture snapshot list/status before-after, durations, free-disk before/after.

### C2 — Node add/remove on a running cluster

**Goal**: live topology changes keep identities stable.

Steps:
1. `tbx node add qa-sta --role worker` — a new `qa-sta-worker-<i>` appears, reaches `maintenance`; existing node IPs unchanged.
2. `tbx node remove qa-sta qa-sta-worker-<i>` — it disappears from status; the others untouched.
3. Per-node lifecycle: `tbx node stop qa-sta qa-sta-worker-1` then `tbx node start qa-sta qa-sta-worker-1` — both verbs are implemented and expected to succeed. After `node stop` the node shows `stopped` in `tbx status qa-sta` (the rest of the cluster untouched); after `node start` it comes back and rejoins.

Expected observations: add/remove clean and narrated; deterministic MACs keep DHCP/DNS stable for survivors; `node stop` leaves the node in `stopped` and `node start` returns it to a running state, both narrated.

Pass criteria: add/remove correct with stable survivor identity; `node stop`/`node start` both succeed and the status state follows.

On failure: capture status before/after each verb.

### C3 — Suspend/resume, same-daemon happy path **[platform gates differ]**

**Goal**: the capability-gated suspend works (or refuses correctly).

Steps:
1. `tbx cluster suspend qa-sta`:
   - **[macOS 14+]** expect success; status shows a suspended state.
   - **[Linux QEMU ≥ 8.2]** expect success.
   - **[Linux QEMU 6.2–8.1]** expect a refusal quoting the capability reason (this refusal IS the pass condition on such hosts; skip C4).
2. `tbx cluster resume qa-sta` — nodes return without reboot: verify via `tbx console` that the kernel timestamp continued (no fresh boot banner), or via uptime once configured clusters are involved.

Expected observations: memory-preserving resume on capable hosts (console log continues mid-stream, no new boot); clean capability refusal otherwise.

Pass criteria: resume without reboot on capable hosts; correct refusal text on incapable ones.

On failure: capture suspend/resume output and console evidence.

### C4 — Suspend degradation boundary **[platform-marked]** (depends on C3 success)

**Goal**: the documented degradation is a warned cold boot, never an error-out.

Steps:
1. **[macOS]** Suspend `qa-sta`, then restart the daemon with `tbx system restart` (implemented end to end; it refuses while clusters are running and names `--force`, so use `tbx system restart --force` here and record which path you took). `tbx cluster resume qa-sta` — expect a warning and a graceful cold boot, not an error.
2. **[Linux]** Suspend survives a daemon restart by design; restore after restarting the user service (`systemctl --user restart tbxd` or equivalent) — expect memory-preserving success when QEMU identity is unchanged. (Degradation on QEMU-identity mismatch is acknowledged as untestable in place — mark that sub-case SKIPPED with reason.)

Expected observations: exactly the documented boundary behavior; the warning names why memory could not be preserved (macOS).

Pass criteria: warned cold boot (macOS) / successful cross-daemon restore (Linux); never a hard error leaving the cluster stuck.

On failure: capture resume output and final status.

### C5 — Destroy and cleanup (always run)

Steps: `tbx snapshot list qa-sta` → delete any leftovers; `tbx cluster destroy qa-sta --force`; verify no residue (status, `~/.talosbox/clusters/`, snapshots).

Pass criteria: no residue.

## Report template

```markdown
## QA deep-cluster-state <platform> — <date>

- tbx version / commit; platform details (incl. QEMU version on Linux):
- Preflight: OK | BLOCKED (<why>)

| Charter | Verdict | Duration | Notes |
|---|---|---|---|
| C1 snapshot round-trip | | | |
| C2 node add/remove | | | |
| C3 suspend/resume | | | |
| C4 degradation boundary | | | |
| C5 destroy | | | |

### Friction log
### Failures
```
