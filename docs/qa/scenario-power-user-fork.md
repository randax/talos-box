# QA Scenario: Substrate-only power-user fork

| | |
|---|---|
| **Tier** | Combination scenario (substrate-only × custom domain × hand-applied manifests streams × BYO) |
| **Platform** | macOS + Linux |
| **Estimated duration** | 60–90 min (manual talosctl work) |
| **Destructive** | Creates and destroys cluster `qa-fork` |
| **Runbook version** | against talos-box main @ the commit recorded in your report |

## How to execute this runbook (agent instructions)

You are running QA, not demos. For every charter: run the steps exactly, compare against **Expected observations**, and record **PASS**, **FAIL**, or **PASS-with-friction**. Friction — confusing messages, doc/behavior mismatch, missing knowledge, misleading output — is a first-class result even on passing charters. This scenario plays the power user following printed guidance; where the printed hints are ambiguous or wrong is exactly what it exists to find. No improvised recovery beyond what the printed hints themselves suggest.

**Prerequisite tooling**: `talosctl`, `kubectl`, `helm` available on the host.

**Report destination**: one `qa-run` issue, title `QA scenario-power-user-fork <platform> <date>`.

## Preflight

BLOCKED unless: doctor clean; no cluster `qa-fork`; `talosctl` installed; ≥ 8 GiB RAM.

## Charters

### C1 — Substrate-only cluster on a custom domain

**Goal**: the substrate contract alone: maintenance nodes, custom domain, guided hints.

Steps:
1. `tbx cluster create qa-fork --domain forge.internal`
2. `tbx status qa-fork` — three `maintenance` nodes; hints show the `talosctl --insecure` probe.
3. Run the printed probe verbatim against a node.

Expected observations: tbx applied no machine config (nodes stay maintenance indefinitely); the printed hint works exactly as printed; DNS for `forge.internal` live.

Pass criteria: probe works as printed; nodes remain unconfigured.

On failure: capture the hint text and probe output.

### C2 — Hand-bootstrap using the substrate (depends on C1)

**Goal**: an attendee-style manual bootstrap works on the substrate tbx provides.

Steps:
1. Generate config with talosctl (`talosctl gen config qa-fork https://<cp-ip>:6443`), patch in the mirror registry config printed by `tbx manifests qa-fork mirrors` (hand-apply the equivalent) and the storage machine patch from `tbx manifests qa-fork storage-machine` (save to file, apply per the printed instructions: `talosctl patch mc -p @storage-machine.yaml --nodes <each-node>` — note nodes are unconfigured, so fold the patches into the generated config before apply instead; record which route the printed docs led you to).
2. `talosctl apply-config` to all three nodes; bootstrap the control plane; fetch kubeconfig.
3. `tbx status qa-fork` — nodes now `configured` (tbx observes, doesn't own).
4. Install any CNI by hand (e.g. flannel manifest) so nodes go Ready.

Expected observations: substrate never fights the manual flow; status phase tracking flips to `configured` purely from observation; mirror config from the printed stream works for the hand-built cluster (pulls go through the gateway mirror).

Pass criteria: hand-built cluster Ready on tbx's substrate with mirror-routed pulls.

On failure: capture where the manual flow and the printed guidance diverged — this is the scenario's core finding surface.

### C3 — BYO storage per the printed storage streams (depends on C2)

**Goal**: the documented substrate-only CSI path: PSA guidance + kubelet mounts printed by `manifests storage` are sufficient and correct.

Steps:
1. `tbx manifests qa-fork storage` — follow it: apply PSA labels to your CSI namespace as printed.
2. Install a BYO CSI (local-path-provisioner upstream manifest is the cheap honest choice).
3. PVC write/readback.

Expected observations: the printed prerequisites are complete — the BYO CSI works without undocumented extra steps; anything you had to figure out yourself is friction by definition.

Pass criteria: BYO PVC write/readback with only printed guidance + upstream docs.

On failure: capture the missing prerequisite precisely.

### C4 — The fork boundary holds (depends on C2)

**Goal**: tbx respects hand-managed clusters; curated verbs fail legibly.

Steps:
1. `tbx manifests qa-fork objects` — expect the documented refusal (curated sections need a declared CNI; this cluster has none declared).
2. `tbx bgp enable qa-fork` — expect refusal (requires curated cilium).
3. `tbx snapshot create qa-fork forked --yes` then restore it — substrate verbs must still work on a hand-built cluster; post-restore, the hand-built control plane comes back (cold boot) and Ready returns.

Expected observations: curated-only surfaces refuse with specific errors; substrate verbs (snapshot, console, status, stop/start) work regardless of who configured the cluster.

Pass criteria: refusals legible; snapshot round-trip preserves the hand-built cluster.

On failure: capture each refusal/verb output.

### C5 — Destroy and cleanup (always run)

Steps: `tbx cluster destroy qa-fork --force`; verify no residue (status, disk, `forge.internal` DNS state).

Pass criteria: no residue.

## Report template

```markdown
## QA scenario-power-user-fork <platform> — <date>

- tbx version / commit; platform details; talosctl version:
- Preflight: OK | BLOCKED (<why>)

| Charter | Verdict | Duration | Notes |
|---|---|---|---|
| C1 substrate + domain | | | |
| C2 hand bootstrap | | | |
| C3 BYO storage | | | |
| C4 fork boundary | | | |
| C5 destroy | | | |

### Friction log
### Failures
```
