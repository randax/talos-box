# QA Scenario: Snapshot restore × provisioned cluster

| | |
|---|---|
| **Tier** | Combination scenario (cold snapshot × etcd/CNI/storage convergence) |
| **Platform** | macOS + Linux |
| **Estimated duration** | 60–75 min |
| **Destructive** | Creates and destroys cluster `qa-snap` and its snapshots |
| **Runbook version** | against talos-box main @ the commit recorded in your report |

## How to execute this runbook (agent instructions)

You are running QA, not demos. For every charter: run the steps exactly, compare against **Expected observations**, and record **PASS**, **FAIL**, or **PASS-with-friction**. Friction — confusing messages, doc/behavior mismatch, missing knowledge, misleading output — is a first-class result even on passing charters. No improvised recovery: capture the **On failure** evidence, mark FAIL, continue unless a dependency broke.

**Report destination**: one `qa-run` issue, title `QA scenario-snapshot-provisioned <platform> <date>`.

## Preflight

BLOCKED unless: doctor clean; no cluster `qa-snap`; ≥ 10 GiB RAM; ≥ 60 GiB free disk on Linux (full-copy snapshot fallback of provisioned disks).

## Charters

### C1 — Provisioned baseline with data

**Goal**: a full cilium+longhorn cluster with distinguishable pre-snapshot state.

Steps:
1. `tbx cluster create qa-snap --cni cilium --csi longhorn`; wait for the full end state.
2. Create namespace `epoch-one` with a PVC and write the string `epoch-one` to the volume; verify readback. Use the [PSA-compliant test pod](deep-storage.md#psa-compliant-test-pod) so the apply does not emit a `would violate PodSecurity "restricted:latest"` block — that block is a warning, not a rejection, and is not a finding.

Pass criteria: end state reached; epoch-one data committed.

### C2 — Snapshot the live provisioned cluster (depends on C1)

**Goal**: whole-cluster crash-consistent snapshot through a stop.

Steps:
1. `tbx snapshot create qa-snap epoch1 --yes` — cluster stops, snapshots, restarts.
2. After restart: cluster reconverges to the full end state (VIP live, Longhorn healthy) without intervention; `epoch-one` data intact. Record reconvergence time. Longhorn needs a settle window: the volume comes back `degraded` and rebuilds to `healthy` within ~10 s, so poll `kubectl -n longhorn-system get volumes.longhorn.io` for up to 60 s before scoring it — a single early sample recording `degraded` is a false FAIL. A second volume with no PVC or PV behind it may appear in `deleting`/`degraded` in the same window; it is Longhorn converging and clears itself, so score it only if it is still there after several minutes.

Expected observations: a provisioned cluster survives its own snapshot cycle: etcd quorum returns, CNI and storage come back on their own.

Pass criteria: end state and data intact post-snapshot-create.

On failure: capture status timeline, pod states, Longhorn volume health.

### C3 — Diverge, then restore (depends on C2)

**Goal**: restore returns the whole stack — Kubernetes objects AND volume data — to the snapshot point.

Steps:
1. Create namespace `epoch-two`; append `epoch-two` to the volume; delete namespace `epoch-one`'s workload (leave visible divergence at both the k8s and data layers).
2. `tbx snapshot restore qa-snap epoch1 --yes`; wait through the cold boot for full reconvergence.
3. Verify: namespace `epoch-two` does NOT exist; `epoch-one` workload is back; the volume reads exactly `epoch-one` (no `epoch-two` line); VIP live; Longhorn healthy — poll for up to 60 s, since the restored volume is transiently `degraded` and rebuilds to `healthy` within ~10 s; node IPs unchanged.

Expected observations: one crash-consistent set: etcd state and Longhorn disks agree with each other at the snapshot point — no "namespace exists but volume is newer" splits.

Pass criteria: both layers exactly at epoch1; cluster fully functional.

On failure: capture the divergence found (k8s objects vs volume contents), Longhorn events, etcd/apiserver pod states.

### C4 — Restore is repeatable (depends on C3)

**Goal**: a snapshot survives being restored more than once.

Steps:
1. Diverge again trivially (create any namespace), restore `epoch1` again, verify the same end state as C3.
2. `tbx snapshot delete qa-snap epoch1`; `tbx snapshot list qa-snap` empty; cluster unaffected by the delete.

Pass criteria: second restore identical; delete leaves the running cluster alone.

On failure: capture second-restore deltas.

### C5 — Destroy and cleanup (always run)

Steps: delete leftover snapshots; `tbx cluster destroy qa-snap --force` (record the data-loss warning); verify no residue including snapshot storage.

Pass criteria: no residue.

## Report template

```markdown
## QA scenario-snapshot-provisioned <platform> — <date>

- tbx version / commit; platform details; snapshot mechanism observed (clonefile vs copy):
- Preflight: OK | BLOCKED (<why>)

| Charter | Verdict | Duration | Notes |
|---|---|---|---|
| C1 provisioned + data | | | |
| C2 snapshot live | | | |
| C3 diverge + restore | | | |
| C4 repeat restore | | | |
| C5 destroy | | | |

### Friction log
### Failures
```
