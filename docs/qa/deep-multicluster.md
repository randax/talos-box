# QA Runbook: Multi-cluster networking

| | |
|---|---|
| **Tier** | Deep (one feature area exercised against defaults) |
| **Platform** | macOS + Linux (failover expectations differ; charters marked) |
| **Estimated duration** | 60–75 min |
| **Destructive** | Creates and destroys clusters `qa-a` and `qa-b`; enables/disables BGP on `qa-a` only |
| **Runbook version** | against talos-box main @ the commit recorded in your report |

## How to execute this runbook (agent instructions)

You are running QA, not demos. For every charter: run the steps exactly, compare against **Expected observations**, and record **PASS**, **FAIL**, or **PASS-with-friction**. Friction — confusing messages, doc/behavior mismatch, missing knowledge, misleading output — is a first-class result even on passing charters. No improvised recovery: capture the **On failure** evidence, mark FAIL, continue unless a dependency broke.

**Report destination**: one `qa-run` issue, title `QA deep-multicluster <platform> <date>`.

## Preflight

BLOCKED unless: `tbx version` recorded; `tbx doctor` exits 0 (including `port-179` free on Linux); no clusters `qa-a`/`qa-b`; enough free RAM for the **second** create, not just for both clusters' nominal memory — see below.

**Memory floor.** Two 3-node clusters are 12288 MiB of guests, but the binding constraint is the provision-start gate at the *second* `cluster create`: starting 6144 MiB must still leave the 6144 MiB balloon reserve free, so the host needs **12288 MiB free with `qa-a` already running** — roughly **18–19 GiB free before you start**, not 14 GiB. The `host-pressure` line of `tbx doctor` prints the reading and the arithmetic ("room for N MiB of new guests right now"): if N is below 6144 with `qa-a` running, the second create is refused. The gate will pre-balloon the running guests down to cover a small shortfall (down to a 1024 MiB per-node floor) before refusing, so a host a few hundred MiB short still proceeds — a host several GiB short does not. Record the `host-pressure` numbers at preflight and again after C1.

`port-179` is checked on both platforms, but macOS reports only an any-address squatter (`*:179`) and reports it as WARN, so `tbx doctor` still exits 0 with it. Read the `port-179` line, and confirm by hand with `netstat -an | grep '\.179'` (no foreign `LISTEN` line) or `sudo lsof -iTCP:179 -sTCP:LISTEN`. Note that unprivileged `lsof -iTCP:179` cannot see the root-owned helper socket, so an empty unprivileged listing is not evidence — use `netstat` or `sudo`.

## Charters

### C1 — Two clusters, two subnets

**Goal**: cluster n → `172.30.<n>.0/24`, no collisions, independent DNS.

Steps:
1. `tbx cluster create qa-a --cni cilium` and `tbx cluster create qa-b --cni cilium`
2. `tbx status` — record each cluster's subnet, node IPs, VIP.
3. Resolve one node name from each domain (`<node>.qa-a.k8s.test`, `<node>.qa-b.k8s.test`).

Expected observations: distinct `/24`s; both hit their provisioned end state; DNS answers per-cluster with no bleed.

Pass criteria: two healthy clusters on distinct subnets, both VIPs live.

On failure: capture status, subnet assignments, doctor routes section.

### C2 — Inter-cluster reachability contract (depends on C1)

**Goal**: host↔node, host↔VIP, cluster↔cluster (nodes AND VIPs) all hold.

Steps:
1. From the host: ping a `qa-a` node and a `qa-b` node; curl both VIPs.
2. From inside `qa-a` (a test pod or `kubectl debug` node shell): ping a `qa-b` node IP; curl `qa-b`'s VIP (`172.30.<m>.200`).
3. Reverse: from `qa-b`, reach `qa-a`'s node and VIP.

Expected observations: all six paths work — the host routes between cluster subnets as a first-class contract; record round-trip times (gross anomalies are friction).

Pass criteria: 6/6 paths reachable.

On failure: capture which path failed, traceroute/mtr from the failing side, doctor forwarding/rp-filter lines (Linux), and whether Docker is present on the host (known FORWARD-policy interaction).

### C3 — BGP on one cluster only (depends on C1)

**Goal**: `bgp enable` switches announcement mechanism per cluster; L2 stays on the other.

Steps:
1. `tbx bgp enable qa-a`
2. Confirm `qa-a` VIP still reachable (now BGP-announced: learned route present — macOS `netstat -rn | grep 172.30.<n>.200` / Linux `ip route | grep 172.30.<n>.200`). A bare grep hit is NOT sufficient evidence on macOS: ordinary L2 traffic to the VIP leaves an ARP-cache entry (flags `UHLWI`, interface `bridge*`) that matches the same grep. Demand a static, helper-injected route: the signature is `172.30.<n>.200  <node-ip>  UGHS` (gateway = the cluster's announcing node, `S` = static). Record the full route line, not just that grep matched.
3. Confirm `qa-b` VIP still reachable via L2 (no such `UGHS` host route for `qa-b`'s VIP — an ARP-cache `UHLWI` entry there is expected and is not a route).
4. `tbx manifests qa-a bgp` and `tbx manifests qa-a l2` — record what each renders under BGP mode (BGP replaces L2 for the pool; the l2 section should reflect that).
5. `tbx bgp disable qa-a` — VIP reachability survives the switch back (allow the L2 convergence window); the injected route disappears.

Expected observations: per-cluster independence: qa-a announcement mechanism flips, qa-b never flickers; host FIB shows/loses the learned route; ASNs per contract (host 64512, cluster nodes 64600+n) — verify via manifests or status output if surfaced.

Pass criteria: both VIPs reachable in every phase; route appears/disappears with enable/disable.

On failure: capture host routing table before/after, `tbx status`, manifests bgp section.

### C4 — Failover comparison under BGP vs L2 **[platform-marked]** (depends on C3 knowledge)

**Goal**: BGP is the fast-failover path on macOS; L2 baseline measured in deep-cilium C4.

Steps:
1. Re-enable BGP on `qa-a`. Identify the node holding the VIP path, remove it (`tbx node remove qa-a <node>`), and time VIP unreachability as in deep-cilium C4.

Expected observations: **[macOS]** BGP failover markedly faster than the ~40–50 s L2 baseline (seconds, not tens of seconds). **[Linux]** both mechanisms fast; record numbers.

Pass criteria: VIP recovers; on macOS, BGP beats the documented L2 window clearly.

On failure: capture timing log and host route table during the gap.

### C5 — Destroy ordering and residue (always run)

Steps:
1. `tbx cluster destroy qa-a --force`; confirm `qa-b` is completely unaffected (VIP still answers, DNS intact).
2. `tbx cluster destroy qa-b --force`; verify no residue: no leftover routes to either subnet, no resolver/resolved entries, no bridges (Linux), status clean.

Pass criteria: destroying one cluster never disturbs another; zero residue at the end.

On failure: list surviving state exactly (routes, DNS, bridges, files).

## Report template

```markdown
## QA deep-multicluster <platform> — <date>

- tbx version / commit; platform details:
- Preflight: OK | BLOCKED (<why>)

| Charter | Verdict | Duration | Notes |
|---|---|---|---|
| C1 two clusters | | | |
| C2 reachability 6/6 | | | |
| C3 bgp one-sided | | | |
| C4 failover compare | | | |
| C5 destroy ordering | | | |

### Friction log
### Failures
```
