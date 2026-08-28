# QA Scenario: Offline venue rehearsal

| | |
|---|---|
| **Tier** | Combination scenario (mirror × skipFallback × digest-serving × provisioning) |
| **Platform** | macOS + Linux |
| **Estimated duration** | 60–90 min |
| **Destructive** | Creates and destroys cluster `qa-venue`; requires cutting upstream network mid-run |
| **Runbook version** | against talos-box main @ the commit recorded in your report |

## How to execute this runbook (agent instructions)

You are running QA, not demos. For every charter: run the steps exactly, compare against **Expected observations**, and record **PASS**, **FAIL**, or **PASS-with-friction**. Friction — confusing messages, doc/behavior mismatch, missing knowledge, misleading output — is a first-class result even on passing charters. No improvised recovery: capture the **On failure** evidence, mark FAIL, continue unless a dependency broke.

**Report destination**: one `qa-run` issue, title `QA scenario-offline-venue <platform> <date>`.

## Preflight

BLOCKED unless: doctor clean; no cluster `qa-venue`; you have a way to cut upstream connectivity for the tbx host that you can also undo (Wi-Fi off / unplug / firewall rule) — record the mechanism; ≥ 10 GiB RAM.

## Charters

### C1 — Prepare while online

**Goal**: everything a provisioned cluster needs enters the cache before the network goes away.

Steps:
1. `tbx cache pull` (default Talos disk image for this host's arch).
2. Create the provisioned cluster once online to let the mirror warm organically: `tbx cluster create qa-venue --cni cilium --csi longhorn`, wait for the full end state (VIP live, storage ready), exercise a PVC write.
3. If you maintain a pin list for extra workload images, run `tbx cache warm <list>` twice: the second default warm must make zero upstream requests for complete refs. Use `tbx cache warm --refresh <list>` only when you explicitly want to revalidate complete unpinned tags, then run `tbx cache warm --check --deep <list>`. Digest-pinned refs need no freshness resolution. Otherwise record that the mirror was warmed organically.
4. `tbx cache list` — record disk-image and per-upstream mirror totals.
5. `tbx cluster destroy qa-venue --force` (the venue rehearsal starts from nothing).

Expected observations: cache list shows the Talos image and substantial mirror content across the upstreams the curated path uses. A complete cached tag (mapping, selected `linux/<arch>` manifest, config, and every layer) remains pullable online during a transient upstream failure; when that occurs naturally, `tbx logs` records `mirror served stale: <host>/<repository>:<tag> (upstream <reason>; cache complete for linux/<arch>)`. Do not induce a real 429 or exhaust a registry quota to prove this; the controlled charter in [deep-mirror-offline-cache.md](deep-mirror-offline-cache.md#controlled-stale-on-429-charter) is authoritative.

Pass criteria: cache populated; cluster destroyed clean.

On failure: capture cache list and create output.

### C2 — Go dark

**Goal**: the offline boundary is real.

Steps:
1. `tbx mirror offline on`; confirm with `tbx mirror offline`.
2. Cut upstream connectivity (your recorded mechanism). Verify: `curl --max-time 5 https://factory.talos.dev` fails.

Pass criteria: host provably offline; mirror mode on.

### C3 — Create the full provisioned cluster with zero upstream (depends on C1, C2)

**Goal**: the venue promise: disk image from cache, every container pull served by the offline mirror, cluster reaches the same end state as online.

Steps:
1. `tbx cluster create qa-venue --cni cilium --csi longhorn`
2. Follow status to the end state; export credentials; verify VIP answers and a PVC write/readback works.
3. Watch for any stall or image-pull backoff: `kubectl get events -A | grep -i pull` — a single upstream-dial attempt is a FAIL even if retried successfully later.

Expected observations: create uses the cached disk image (no factory download); all pods reach Running with images served from the mirror cache; total time comparable to the online run (record both); zero pull failures.

Pass criteria: full end state (VIP + storage probe) with the network cut the entire time.

On failure: capture the stalled pod's describe/events, `tbx cache list`, mirror mode, and exactly which image reference missed.

### C4 — Miss behavior stays honest (depends on C3)

**Goal**: the generated strict node policy turns the mirror's local cache miss into a fast, clear pull failure.

Steps:
1. Deploy a pod with an image that was never warmed. Base it on the [PSA-compliant test pod](deep-storage.md#psa-compliant-test-pod) (swap in the uncached image) so the apply does not emit a `would violate PodSecurity "restricted:latest"` block — that block is a warning, not a rejection, and is not a finding. Offline mode stops the mirror from reaching registries and its miss returns 404. talos-box's generated `"*"` entry independently sets `skipFallback: true`, so the node cannot bypass that 404; expect a clear `offline: content not cached` pull failure within a bounded time, with no infinite hang. An explicit mirror entry with `skipFallback: false` could fall through, but the physically disconnected host would still be unable to reach upstream.

Pass criteria: hard, legible failure; cluster otherwise unaffected.

On failure: capture pod events and how long it took to surface.

### C5 — Return to daylight and cleanup (always run)

Steps: restore connectivity; `tbx mirror offline off`; confirm the C4 pod now pulls; `tbx cluster destroy qa-venue --force`; verify no residue; leave the mirror cache intact (do NOT prune — the cache is the venue asset).

Pass criteria: pull-through restored; no residue; cache preserved.

## Report template

```markdown
## QA scenario-offline-venue <platform> — <date>

- tbx version / commit; platform details; offline mechanism used:
- Preflight: OK | BLOCKED (<why>)

| Charter | Verdict | Duration | Notes |
|---|---|---|---|
| C1 prepare online | | | |
| C2 go dark | | | |
| C3 offline create | | | |
| C4 miss behavior | | | |
| C5 daylight + cleanup | | | |

### Friction log
### Failures
```
