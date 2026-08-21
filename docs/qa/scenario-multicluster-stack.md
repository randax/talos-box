# QA Scenario: Multi-cluster stack

| | |
|---|---|
| **Tier** | Combination scenario (nested custom domains × inter-cluster VIPs × asymmetric BGP) |
| **Platform** | macOS + Linux |
| **Estimated duration** | 60–90 min |
| **Destructive** | Creates and destroys clusters `qa-core` and `qa-edge` |
| **Runbook version** | against talos-box main @ the commit recorded in your report |

## How to execute this runbook (agent instructions)

You are running QA, not demos. For every charter: run the steps exactly, compare against **Expected observations**, and record **PASS**, **FAIL**, or **PASS-with-friction**. Friction — confusing messages, doc/behavior mismatch, missing knowledge, misleading output — is a first-class result even on passing charters. No improvised recovery: capture the **On failure** evidence, mark FAIL, continue unless a dependency broke.

**Report destination**: one `qa-run` issue, title `QA scenario-multicluster-stack <platform> <date>`.

## Preflight

BLOCKED unless: doctor clean (`port-179` free); no clusters `qa-core`/`qa-edge`; enough free RAM for the **second** create, not just for both clusters' nominal memory — see below.

**Memory floor (macOS).** Two 3-node clusters are 12288 MiB of guests, but the binding constraint is the provision-start gate at the *second* cluster `tbx up` walks: starting 6144 MiB must still leave the 6144 MiB balloon reserve free, so the host needs **12288 MiB free with `qa-core` already running** — roughly **18–19 GiB free before you start**, not 14 GiB. The `host-pressure` line of `tbx doctor` prints the reading and the arithmetic ("room for N MiB of new guests right now"), where N is measured free minus the reserve. The gate does not refuse at N below 6144, though: before blocking it credits what it can pre-balloon out of the running guests, down to a 1024 MiB per-node floor — for a default 3-node `qa-core` that is 3 × (2048 − 1024) = **3072 MiB**. So the second create is refused only when N *measured with `qa-core` already running* drops below **~3072 MiB**; equivalently, a **preflight** (no clusters running) N between **9216 and 12288 MiB** means the second create proceeds on a pre-balloon rather than being blocked. Mind which reading a threshold belongs to: a preflight N of 3072–6144 MiB is a host that is 6144 MiB short at the second create and will be hard-refused there — mid-`tbx up`, after `qa-core` has already been built. Still hold to the 18–19 GiB recommendation: a marginal admission pins every `qa-core` node at the 1024 MiB floor for the hold window, which is a poor state to run the rest of the charters in. Record the `host-pressure` numbers at preflight and again after C1.

**Memory floor (Linux).** None of the above applies: `hostpressure.SystemSnapshot` and the host-memory readings are macOS-only, so `tbx doctor` prints `SKIP host-pressure` with no numbers (expected, not friction — see `docs/linux.md`), the provision-start gate never fires, and the overcommit check reads no host total and so cannot warn either. There is nothing to record at preflight and nothing to compare against, and nothing will ever report BLOCKED on memory — do not record BLOCKED on the strength of a gate that cannot fire. The 18–19 GiB physical recommendation still stands as advice (host swap pressure is what corrupts guest writes), it is simply unenforced here: self-police with `free -h` and `swapon --show`, record those at preflight and again after C1 in place of the `host-pressure` numbers, and keep watching them for the rest of the run.

## Charters

### C1 — The stack: nested domains, one BGP

**Goal**: build the whole shape in one go and reach both end states.

Steps:
1. Write one `talosbox.yaml` declaring both clusters: `qa-core` with `domain: platform.internal`, `cni: cilium`, `bgp: true`; `qa-edge` with `domain: edge.platform.internal`, `cni: cilium` (L2, no bgp). `tbx up -f <file>`.
2. Follow both to their end states.

Expected observations: `tbx up` reconciles both clusters from one file; qa-core's VIP is BGP-announced (host route present), qa-edge's is L2; both domains registered.

Pass criteria: both clusters at end state from a single declarative up.

On failure: capture up narration, per-cluster status.

### C2 — Nested-domain resolution across the stack (depends on C1)

**Goal**: longest-suffix ownership with live traffic.

Steps:
1. Resolve and curl: `app.platform.internal` (→ qa-core `.200`) and `app.edge.platform.internal` (→ qa-edge `.200`).
2. From a pod in qa-edge: resolve/curl `app.platform.internal` — record whether guests resolve the sibling cluster's domain via the host DNS path, and what the actual behavior is (this documents the guest-side cross-domain contract; observe-and-attest).

Expected observations: host-side split is exact (each wildcard to its own VIP); guest-side behavior recorded verbatim.

Pass criteria: host-side resolution and reachability exact for both domains.

On failure: capture resolutions and DNS server answers per domain.

### C3 — Cross-cluster service consumption (depends on C1)

**Goal**: the inter-cluster contract carries a real workload path.

Steps:
1. Deploy a plain HTTP echo service behind the LB on qa-core (it gets a pool IP, e.g. `.201`).
2. From a qa-edge pod, curl the qa-core service's LB IP and the `.200` VIP by both IP and `app.platform.internal` name (name resolution per C2 findings — use IP if guests can't resolve siblings).
3. Reverse direction once (qa-core pod → qa-edge VIP).

Expected observations: cross-cluster node↔VIP traffic flows both ways through the host router; BGP-announced (core) and L2-announced (edge) VIPs are equally reachable from the sibling cluster.

Pass criteria: both directions serve HTTP 200 by IP.

On failure: capture which leg fails, host routing table, doctor forwarding/rp-filter lines. The
sibling-pod → BGP-VIP leg specifically regressed once (#387): on macOS that leg does not use the
host FIB at all — guest-to-guest traffic crosses subnets through the helper's userspace frame
router, which learns an L2-announced VIP from the owning node's ARP but can only learn a
BGP-announced one from the speaker's host-route writes. If it fails again, record whether the
host itself still reaches the VIP (it will, over the injected route) and whether the sibling
reaches the *node* IP, since that pair separates the router binding from real routing.

### C4 — Asymmetry survives lifecycle churn (depends on C3)

**Goal**: the BGP/L2 split and routing survive a stop/start cycle of one member.

Steps:
1. `tbx cluster stop qa-edge`, then start it again; wait for its end state.
2. Verify C3's cross-cluster paths again (both directions).
3. `tbx bgp disable qa-core`; verify qa-core's VIP remains reachable from qa-edge after L2 convergence; re-verify host route withdrawal with `tbx bgp status qa-core`.
4. `tbx doctor` while both clusters run — the `inter-cluster` check must report every host→VIP and cluster→sibling-VIP path, and `FAIL` naming the dead direction whenever one is down (#388).

Expected observations: stop/start of one cluster never disturbs the other's announcements or routes; the announcement-mechanism flip mid-life keeps the VIP served; `bgp disable` narrates only the flip, never the create-time equivalent-command block (#400).

Pass criteria: all paths recover; no cross-cluster interference.

On failure: capture per-phase route table and reachability matrix.

### C5 — Destroy and cleanup (always run)

Steps: `tbx down -f <file>` first (declarative inverse — record what it does to both), then `tbx cluster destroy qa-core --force` and `tbx cluster destroy qa-edge --force`; verify zero residue: routes, per-cluster domains (`platform.internal` tree fully gone), bridges/attachments, status. The shared `/etc/resolver/k8s.test` file (macOS) is install-scoped and is expected to persist — it is not residue.

Pass criteria: no residue; down behaved as documented (stops, does not destroy).

## Report template

```markdown
## QA scenario-multicluster-stack <platform> — <date>

- tbx version / commit; platform details:
- Preflight: OK | BLOCKED (<why>)

| Charter | Verdict | Duration | Notes |
|---|---|---|---|
| C1 stack up | | | |
| C2 nested DNS | | | |
| C3 cross-cluster service | | | |
| C4 churn survival | | | |
| C5 down + destroy | | | |

### Friction log
### Failures
```
