# QA Runbook: Linux smoke

| | |
|---|---|
| **Tier** | Smoke (default everything, one cluster, happy path) |
| **Platform** | Linux amd64/arm64, QEMU/KVM |
| **Estimated duration** | 25–35 min (raw-image disk copy is slower than macOS clonefile) |
| **Destructive** | Creates and destroys cluster `qa-smoke`; does not touch other clusters, the cache, or host config |
| **Runbook version** | against talos-box main @ the commit recorded in your report |

## How to execute this runbook (agent instructions)

You are running QA, not demos. For every charter: run the steps exactly, compare what you see against **Expected observations**, and record a verdict — **PASS**, **FAIL**, or **PASS-with-friction**. Friction is anything a careful human would frown at: a confusing message, a doc/behavior mismatch, a step that needed knowledge this runbook didn't give you, output that is technically correct but misleading. Friction is a first-class result — report it even when the charter passes.

Never improvise recovery mid-charter: if a step fails, capture the evidence listed under **On failure**, mark FAIL, and continue to the next charter unless it depends on this one (dependencies are stated per charter).

**Report destination**: one GitHub issue per run, label `qa-run`, title `QA smoke linux <date>`, using the report template at the bottom of this file.

## Preflight

Abort the run (report BLOCKED, not FAIL) if any of these don't hold:

1. `tbx version` prints its identity line; record it verbatim. A source build prints the commit it was built from, not a semver — e.g. `tbx aa4ba4f (linux/arm64, daemon protocol 13)` — so this line and the `git rev-parse HEAD` below can carry the same commit. That is expected, not friction. Record distro and QEMU version (`qemu-system-$(uname -m) --version`).
   - **Record the host cache state too**, because C1's duration depends on it: `tbx cache list` (does a Talos disk image for the pinned version already exist?). Report it as `cold` (no matching disk image cached) or `warm` — a warm host skips the image download entirely, so a fast create is not evidence about first-run timing.
2. `git -C <talos-box checkout> rev-parse HEAD` — record the commit if running from source.
3. `test -r /dev/kvm -a -w /dev/kvm && echo kvm-ok` prints `kvm-ok`.
4. `tbx doctor` exits 0 — on a host installed with `tbx system install`. On a host without systemd (a CI sandbox) see the appendix: the `helper-*` checks FAIL by construction there, and that is a harness deviation to record, not a BLOCKED preflight. Expected on Linux: `helper-unit`, `helper-access`, `helper-capabilities`, `kvm`, `qemu`, `forwarding`, `bridge-netfilter`, `bridge-stp`, `rp-filter`, `port-53`/`port-67`/`port-179`, resolver checks, routes OK; `host-pressure: SKIP` is expected on Linux (not friction). Record any WARN as friction.
5. `tbx status` shows no cluster named `qa-smoke` (destroy leftovers first: `tbx cluster destroy qa-smoke --force`).
6. Host has ≥ 8 GiB free RAM (`free -h`) — the default cluster wants 6 GiB. Note: no overcommit guard exists on Linux; nothing will warn you.
7. Network can reach `factory.talos.dev` (smoke assumes online; offline behavior is a deep runbook).

## Charters

### C1 — Create the default cluster

**Goal**: substrate-only happy path from nothing to three maintenance-mode nodes.

Steps:
1. `tbx cluster create qa-smoke`
2. When it returns, `tbx status qa-smoke`

Expected observations:
- Create narrates stages and returns without error; first run may download the Talos image, and node disks are full raw copies (record actual duration **and the preflight cache state**). Minutes-not-seconds is the **cold**-cache expectation; on a **warm** host the image stage is a no-op and create can finish in seconds — a valid smoke run, but record it as `warm` so its timing is not compared against a cold one. A run that must exercise cold timing starts from `tbx cache prune --all` on an expendable cache.
- Status lists `qa-smoke-cp-1`, `qa-smoke-worker-1`, `qa-smoke-worker-2`, each phase `maintenance`, each with an IP in one `172.30.<n>.0/24`.
- A `br-tbx<n>` bridge exists (`ip link show | grep br-tbx`).
- Status prints copy-pasteable next-step hints mentioning `talosctl --insecure`.

Pass criteria: all three nodes reach `maintenance` within 3 min of create returning.

On failure: capture full create output, `tbx status -o json qa-smoke`, `tbx doctor` output, `ip addr show` for the bridge.

### C2 — DNS and reachability (depends on C1)

**Goal**: the cluster domain contract holds on the host.

Steps:
1. `ping -c1 qa-smoke-cp-1.qa-smoke.k8s.test`
2. `resolvectl query qa-smoke-worker-1.qa-smoke.k8s.test` (if systemd-resolved is absent, use the doctor-printed fallback: `dig @172.30.<n>.1 qa-smoke-worker-1.qa-smoke.k8s.test` — record which path you used)
3. `resolvectl domain` — expect a route-only entry `~qa-smoke.k8s.test` (or `~k8s.test`) on the cluster bridge when resolved is present.

Expected observations: node names resolve to the IPs status reported; ping gets replies. `/etc/resolv.conf` was NOT modified by tbx (check `git diff`-style: the file has no talosbox marker).

Pass criteria: both node lookups resolve to the status-reported IPs (via resolved or the gateway fallback).

On failure: capture `resolvectl status` (or `cat /etc/resolv.conf`), `tbx doctor` DNS lines, and `dig` output against the gateway.

### C3 — Console attach (depends on C1)

**Goal**: `tbx console` delivers a live hvc0 stream and detaches cleanly.

Steps:
1. `tbx console qa-smoke qa-smoke-cp-1`, watch ~10 s, detach with `Ctrl-]`.
   - Without a TTY (scripted or SSH-driven runs): `tbx console qa-smoke qa-smoke-cp-1 --no-follow --lines 30` dumps the ring buffer and exits; record that you used the non-interactive path, and that the detach key was therefore not exercised.

Expected observations: a session banner states the detach key; Talos kernel/machined log lines stream; detach returns your shell without killing the VM (verify: node still `maintenance` in status).

Pass criteria: logs visible, clean detach, node phase unchanged.

On failure: capture the banner and last 30 lines of console output.

### C4 — Lifecycle round-trip (depends on C1)

**Goal**: stop/start preserves identity; nodes come back.

Steps:
1. `tbx cluster stop qa-smoke`, confirm status shows all nodes `stopped`.
2. `tbx cluster start qa-smoke`, wait for status to settle.

Expected observations: after start, same node names, same IPs (deterministic MACs → static DHCP reservations), phases return to `maintenance` in < 2 min.

Pass criteria: IPs identical before/after; all nodes back to `maintenance`.

On failure: capture status before/after and note any IP drift precisely.

### C5 — Destroy and cleanup (always run, even after failures)

**Goal**: destroy leaves no residue.

Steps:
1. `tbx cluster destroy qa-smoke --force`
2. `tbx status` — cluster gone.
3. `ls ~/.talosbox/clusters/` — no `qa-smoke` directory.
4. `ip link show | grep br-tbx` — the cluster's bridge is gone (no orphaned `br-tbx<n>` for this cluster).

Expected observations: destroy prints a data-loss warning path (`--force` supplied — record what it printed); no `qa-smoke` remnants in status, on disk, or in host networking; `resolvectl domain` no longer lists the cluster's domain (when resolved is present) — the resolved registration is per-cluster and goes with the bridge; Linux has no install-scoped `/etc/resolver` file to check (that is the macOS equivalent, and there it is expected to persist).

Pass criteria: no per-cluster residue; no orphaned bridge or resolved registration.

**Known failure**: as of `520bd70` Linux leaves `br-tbx<n>` and its `172.30.<n>.1/24` address behind after destroy, and the next create moves to the next subnet index ([#445](https://github.com/randax/talos-box/issues/445)). Record it as a FAIL on step 4 with the `ip addr show br-tbx<n>` evidence; it is a known, tracked failure, not new information, until #445 closes — then this note goes.

On failure: list what was left behind, exactly.

## Appendix: running on a host without systemd (CI sandbox)

The Linux smoke was piloted on a Depot CI KVM sandbox (the host class the `E2E / KVM *` lanes use) driven over `depot ci ssh`. Such hosts have `/dev/kvm` but no systemd PID 1, so `tbx system install` is unavailable and the helper and daemon must be started the way `scripts/ci/linux-kvm-e2e.sh` does:

```sh
sudo env HOME="$HOME" TBX_HELPER_SOCKET="$PWD/tbx-helper.sock" bin/tbx-helper --allowed-uid "$(id -u)" >tbx-helper.log 2>&1 &
export TBX_HELPER_SOCKET="$PWD/tbx-helper.sock"
bin/tbxd >tbxd.log 2>&1 &
```

What this changes, so the deviations are recorded rather than filed:

- `tbx doctor`: `FAIL helper-unit`, `FAIL helper-access`, `FAIL helper-capabilities` are expected — no units, no `tbx` group, helper under sudo with full root capabilities. Everything else in the doctor list must still hold. Run doctor as the same unprivileged user that runs the charters; if it is root, `bridge-netfilter` cannot show #448.
- C2: there is usually no systemd-resolved, so host-level lookups fail and the gateway fallback (`dig @172.30.<n>.1`) is the path that proves the DNS contract. `tbxd.log` will carry `host DNS for cluster … is unavailable … manual step: sudo resolvectl …` on every reconcile; that is the daemon describing the host, not a defect.
- C3: no TTY — use the `--no-follow` step.
- Driving over `depot ci ssh`: the session lands as **root** (`runuser -u runner -- …` to run as the sandbox user) and **drops remote stderr** — merge it remotely (`exec 2>&1` at the top of the remote command) or the create narration and every error message silently vanish.
- Always `depot ci cancel <run>` when done; the sandbox is billed until the idle step ends.

## Report template

```markdown
## QA smoke linux — <date>

- `tbx version` line (verbatim) / `git rev-parse HEAD`:
- Distro, kernel, QEMU version, arch:
- Cache state at preflight: cold | warm (`tbx cache list` evidence)
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
