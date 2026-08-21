# QA Runbook: Storage (curated CSI)

| | |
|---|---|
| **Tier** | Deep (one feature area exercised against defaults) |
| **Platform** | macOS + Linux |
| **Estimated duration** | 60–90 min (Longhorn convergence dominates) |
| **Destructive** | Creates and destroys cluster `qa-sto`; writes and deletes test PVC data; does not touch other clusters |
| **Runbook version** | against talos-box main @ the commit recorded in your report |

## How to execute this runbook (agent instructions)

You are running QA, not demos. For every charter: run the steps exactly, compare against **Expected observations**, and record **PASS**, **FAIL**, or **PASS-with-friction**. Friction — confusing messages, doc/behavior mismatch, missing knowledge, misleading output — is a first-class result even on passing charters. No improvised recovery: capture the **On failure** evidence, mark FAIL, continue unless a dependency broke.

**Report destination**: one `qa-run` issue, title `QA deep-storage <platform> <date>`.

## Preflight

BLOCKED unless: `tbx version` recorded; `tbx doctor` exits 0; no cluster `qa-sto`; ≥ 10 GiB free RAM and ≥ 30 GiB free disk; online.

## Known transients (not findings)

A `volumes.longhorn.io` object with no PVC or PV behind it, in `deleting` or `degraded`, is
expected briefly after a probe pass, after deleting a PVC, and after a snapshot restart.
Longhorn converges it away within about a minute. Record it as friction only if it is still
there after several minutes — that is a leak, and a finding.

## Charters

### C1 — Longhorn end state: PVC write/readback

**Goal**: `csi: longhorn` delivers a default StorageClass and a working volume path.

Steps:
1. `tbx cluster create qa-sto --cni cilium --csi longhorn`
2. Follow `tbx status qa-sto` to the storage-ready end state; export credentials.
3. `kubectl get sc` — record which class is default.
4. Create a 1Gi PVC + writer pod (write a known string to the volume), then a reader pod on a different node reading it back. Use the PSA-compliant pod spec below so the apply is quiet.

<a id="psa-compliant-test-pod"></a>
**PSA-compliant test pod (canonical snippet — other runbooks link here).** Namespaces without their own `pod-security.kubernetes.io/*` labels — `default` included — are warned and audited at `restricted`, so any pod that is not restricted-compliant makes `kubectl apply` print a long `Warning: would violate PodSecurity "restricted:latest"` block. That block is a warning, not a rejection, and it is not a finding — but it is avoidable noise in every report, so keep test pods compliant:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: qa-writer
spec:
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    fsGroup: 65532
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: writer
      image: docker.io/library/busybox:1.36
      command: ["sh", "-c", "echo qa-data > /data/marker && sleep 3600"]
      securityContext:
        allowPrivilegeEscalation: false
        capabilities:
          drop: ["ALL"]
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: qa-pvc
```

A workload that genuinely needs `hostNetwork` or privileges (a hand-installed CNI, a BYO CSI) cannot be made compliant: label its own namespace instead, exactly as the `tbx manifests <cluster> storage` PSA guidance prints for a CSI namespace (`kubectl label namespace <ns> pod-security.kubernetes.io/enforce=privileged pod-security.kubernetes.io/audit=privileged pod-security.kubernetes.io/warn=privileged --overwrite`). Record the warning block only if it accompanies an actual rejection.

Expected observations: Longhorn pods Running; the curated StorageClass is the cluster default; PVC binds; readback matches the written string; replicas = worker count (a defaults 3-node cluster has 2 workers, so `numberOfReplicas=2` — the workers are the only schedulable storage nodes) — check `kubectl -n longhorn-system get volumes.longhorn.io -o yaml` for the replica number and record it.

Pass criteria: PVC bound, cross-node readback exact.

On failure: capture PVC/PV describe, longhorn-manager logs, `tbx status -o json`.

### C2 — CSI switching is volume-gated (depends on C1)

**Goal**: switching or removing `csi:` is refused while volumes exist, allowed when empty.

Steps:
1. With the C1 PVC still present, edit the cluster's declaration to `csi: local-path` (via `talosbox.yaml` + `tbx up`) — expect refusal naming the non-empty volumes.
2. Delete the PVC (and confirm the Longhorn volume is gone), retry — expect the transition to proceed.
3. After the switch: `kubectl get sc` shows local-path as default; create/write/readback a local-path PVC.

Expected observations: the refusal is specific (which volumes block it) and happens before any deletion; after the gated switch, local-path works. Record how long the transition took and what status showed during it.

Pass criteria: refusal while volumes exist; clean switch when empty; local-path PVC works.

On failure: capture the refusal text, `kubectl get pvc,pv -A`, status output.

### C3 — Destroy warns about data loss (depends on C1/C2)

**Goal**: the destroy path surfaces volume data loss.

Steps:
1. Create a fresh PVC with data on `qa-sto`.
2. `tbx cluster destroy qa-sto --force` — record the data-loss warning verbatim (it should include a best-effort volume count).

Expected observations: destroy inspects before deleting and prints a volume/data-loss warning even under `--force`; destruction completes; no residue.

Pass criteria: warning present and accurate (count matches reality); clean destroy.

On failure: capture full destroy output.

### C4 — Substrate-only storage guidance (no CSI declared)

**Goal**: the `storage` manifests section serves substrate-only clusters.

Steps:
1. `tbx cluster create qa-sto --cni cilium` (no csi).
2. `tbx manifests qa-sto storage` — expect kubelet mount prerequisite + PSA guidance even without `csi:`.
3. `tbx manifests qa-sto storage-machine` — expect the machine patch stream.
4. Negative: `tbx cluster create qa-bad --csi longhorn` (no cni) — expect refusal (`csi` requires `cni`).

Expected observations: storage guidance renders without a declared CSI; the csi-without-cni combination is rejected with the specific validation error and nothing is created.

Pass criteria: guidance renders; invalid combo rejected.

On failure: capture section output / validation error.

### C5 — Destroy and cleanup (always run)

Steps: `tbx cluster destroy qa-sto --force` (if alive); verify no residue.

Pass criteria: no residue.

## Report template

```markdown
## QA deep-storage <platform> — <date>

- tbx version / commit; platform details:
- Preflight: OK | BLOCKED (<why>)

| Charter | Verdict | Duration | Notes |
|---|---|---|---|
| C1 longhorn pvc | | | |
| C2 volume-gated switch | | | |
| C3 destroy warning | | | |
| C4 substrate-only guidance | | | |
| C5 destroy | | | |

### Friction log
### Failures
```
