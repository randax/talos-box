# QA Runbook: macOS smoke

<!-- Graduated from prototype/qa-runbook-format; pilot corrections tracked in #217. -->

| | |
|---|---|
| **Tier** | Smoke (default everything, one cluster, happy path) |
| **Platform** | macOS, Apple Silicon, Virtualization.framework |
| **Estimated duration** | 20–30 min |
| **Destructive** | Creates and destroys cluster `qa-smoke`; does not touch other clusters, the cache, or host config |
| **Runbook version** | against talos-box main @ the commit recorded in your report |

## How to execute this runbook (agent instructions)

You are running QA, not demos. For every charter: run the steps exactly, compare what you see against **Expected observations**, and record a verdict — **PASS**, **FAIL**, or **PASS-with-friction**. Friction is anything a careful human would frown at: a confusing message, a doc/behavior mismatch, a step that needed knowledge this runbook didn't give you, output that is technically correct but misleading. Friction is a first-class result — report it even when the charter passes.

Never improvise recovery mid-charter: if a step fails, capture the evidence listed under **On failure**, mark FAIL, and continue to the next charter unless it depends on this one (dependencies are stated per charter).

**Report destination**: one GitHub issue per run, label `qa-run`, title `QA smoke macOS <date>`, using the report template at the bottom of this file.

## Preflight

**Obtain tbx first**: use the brew install if present; otherwise build from source in the checkout — `make build` (which ad-hoc signs `bin/tbx`/`bin/tbxd` with the virtualization entitlement; unsigned binaries cannot start VMs). Run all commands below as `./bin/tbx` when built from source.

Abort the run (report BLOCKED, not FAIL) if any of these don't hold:

1. `tbx version` prints a version; record it.
2. `git -C <talos-box checkout> rev-parse HEAD` — record the commit if running from source.
3. `tbx doctor` exits 0. Expected: `helper`, `resolver`, `DNS`, and `forwarding` all PASS, and `host-pressure` present — see the macOS check table in [docs/macos.md](../macos.md) for the full list. Record any WARN lines as friction.
   - Known BLOCKED cause — **helper protocol mismatch**: the LaunchDaemon plist (`/Library/LaunchDaemons/dev.talosbox.helper.plist`) pins the helper at an absolute path, so a moved/renamed checkout leaves an old helper running. Fix: `sudo <checkout>/bin/tbx system install` (full path — tbx may not be on PATH), then rerun doctor.
   - The `DNS` check is honest either way and never blocks the run: it `PASS`es when tbxd is already up (any earlier `tbx` command starts the on-demand daemon and it stays up), and `SKIP`s — never `FAIL`s — when the daemon simply isn't running, alongside the other daemon-dependent checks. Doctor exits 0 in both cases. A `FAIL` here means the daemon IS up and its embedded resolver is genuinely unreachable: real friction, record it.
4. `tbx status` shows no cluster named `qa-smoke` (destroy leftovers first: `tbx cluster destroy qa-smoke --force`).
5. Host has ≥ 8 GiB free RAM (`memory_pressure`: use the system-wide free percentage — macOS swap-used numbers stay high after pressure clears) — the default cluster wants 6 GiB.
6. Host volume holding `~/.talosbox` has ≥ 25 GiB free (`df -h ~`) — node disks are 20 GB sparse and doctor warns that low storage can corrupt guest writes.
7. Network can reach `factory.talos.dev` (smoke assumes online; offline behavior is a deep runbook).

If C1 fails with `dial unix ~/.talosbox/tbxd.sock: no such file`, tbx spawned tbxd and it died — read `~/.talosbox/tbxd.log` for the real error before reporting.

## Charters

### C1 — Create the default cluster

**Goal**: substrate-only happy path from nothing to three maintenance-mode nodes.

Steps:
1. `tbx cluster create qa-smoke`
2. When it returns, `tbx status qa-smoke`

Expected observations:
- Create narrates stages and returns without error in < 5 min (record actual duration).
- Status lists `qa-smoke-cp-1`, `qa-smoke-worker-1`, `qa-smoke-worker-2`, each phase `maintenance`, each with an IP in one `/24`.
- Status prints copy-pasteable next-step hints mentioning `talosctl --insecure` (guided output contract).

Pass criteria: all three nodes reach `maintenance` within 3 min of create returning.

On failure: capture full create output, `tbx status -o json qa-smoke`, `tbx doctor` output.

### C2 — DNS and reachability (depends on C1)

**Goal**: the cluster domain contract holds on the host.

Steps:
1. `ping -c1 qa-smoke-cp-1.qa-smoke.k8s.test`
2. `dscacheutil -q host -a name qa-smoke-worker-1.qa-smoke.k8s.test`
3. `ls /etc/resolver/` — expect `k8s.test` present.

Expected observations: node names resolve to the IPs status reported; ping gets replies; no resolver file for a custom domain (none was configured).

Pass criteria: both node lookups resolve to the status-reported IPs.

On failure: capture `scutil --dns` output and the resolver file contents.

### C3 — Console attach (depends on C1)

**Goal**: `tbx console` delivers a live hvc0 stream and detaches cleanly.

Steps:
1. `tbx console qa-smoke qa-smoke-cp-1`, watch ~10 s, detach with `Ctrl-]`.

Expected observations: a session banner states the detach key; console output is present — on a maintenance-phase node that is legitimately replay-only (the node is idle and has nothing new to say, so expect the buffered boot/machined output and possibly no new lines during the watch, which is not a defect); detach returns your shell without killing the VM (verify: node still `maintenance` in status).

Pass criteria: banner with the detach key shown, console output present (streaming or replay-only), clean `Ctrl-]` detach, node phase unchanged.

On failure: capture the banner and last 30 lines of console output.

### C4 — Lifecycle round-trip (depends on C1)

**Goal**: stop/start preserves identity; nodes come back.

Steps:
1. `tbx cluster stop qa-smoke`, confirm status shows all nodes `stopped`.
2. `tbx cluster start qa-smoke`, wait for status to settle.

Expected observations: after start, same node names, same IPs (deterministic MACs → stable leases), phases return to `maintenance` in < 2 min.

Pass criteria: IPs identical before/after; all nodes back to `maintenance`.

On failure: capture status before/after and note any IP drift precisely.

### C5 — Destroy and cleanup (always run, even after failures)

**Goal**: destroy leaves no residue.

Steps:
1. `tbx cluster destroy qa-smoke --force`
2. `tbx status` — cluster gone.
3. `ls ~/.talosbox/clusters/` — no `qa-smoke` directory.

Expected observations: destroy warns about permanence (or proceeds silently under `--force` — record which); no `qa-smoke` remnants in status or on disk; `/etc/resolver/k8s.test` still present (shared default-domain file must survive).

Pass criteria: no residue; shared resolver file intact.

On failure: list what was left behind, exactly.

## Report template

```markdown
## QA smoke macOS — <date>

- tbx version / commit:
- macOS version, hardware:
- Preflight: OK | BLOCKED (<why>)

| Charter | Verdict | Duration | Notes |
|---|---|---|---|
| C1 create | | | |
| C2 dns | | | |
| C3 console | | | |
| C4 lifecycle | | | |
| C5 destroy | | | |

### Friction log
<numbered; every PASS-with-friction and any doc drift, quoted exactly>

### Failures
<per failure: charter, step, expected vs observed, evidence>
```
