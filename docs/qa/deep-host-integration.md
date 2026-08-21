# QA Runbook: Host integration & resources

| | |
|---|---|
| **Tier** | Deep (one feature area exercised against defaults) |
| **Platform** | macOS + Linux — several charters are single-platform (marked) |
| **Estimated duration** | 40–60 min |
| **Destructive** | Creates and destroys cluster `qa-host`; does NOT run `system uninstall` (would break the host setup) |
| **Runbook version** | against talos-box main @ the commit recorded in your report |

## How to execute this runbook (agent instructions)

You are running QA, not demos. For every charter: run the steps exactly, compare against **Expected observations**, and record **PASS**, **FAIL**, or **PASS-with-friction**. Friction — confusing messages, doc/behavior mismatch, missing knowledge, misleading output — is a first-class result even on passing charters. No improvised recovery: capture the **On failure** evidence, mark FAIL, continue unless a dependency broke.

**Report destination**: one `qa-run` issue, title `QA deep-host-integration <platform> <date>`.

## Preflight

BLOCKED unless: `tbx version` recorded; the platform's install story is already in place (macOS: `tbx system install` done; Linux: packages/units per docs/linux.md); no cluster `qa-host`.

## Charters

### C1 — Doctor honesty

**Goal**: every doctor line is truthful and remediation is copy-pasteable.

Steps:
1. `tbx doctor` on a healthy host — record every check name and result. Compare the set against the documented platform list: [docs/macos.md](../macos.md) (`helper`, `resolver`, `DNS`, `forwarding`, `port-179`, `host-pressure`, `system-dns`, `routes`, `guest-agent`, `mirror-health`, `image-cache`, `egress`, `security-inventory`) or [docs/linux.md](../linux.md) (the same daemon- and cluster-scoped checks plus `helper-unit`/`helper-access`/`helper-capabilities`, `kvm`, `qemu`, `bridge-netfilter`, `bridge-stp`, `rp-filter`, and `port-53`/`port-67`/`port-179`). A check the platform doc does not list — or a documented check the build never emits — is the finding.
2. Break one innocuous thing and confirm doctor catches it — the induced failure is platform-specific:
   - **[Linux]** occupy port 179 (`nc -l 179` as root or a high-privilege listener) → doctor `port-179` degrades with a specific message. Release it.
   - **[macOS]** the only port check is `port-179`, and it is the primary induced check here: **try `nc -l 179` as your normal user first** — macOS does not reserve ports below 1024 the way Linux does, and an unprivileged bind on `*:179` is expected to succeed (confirmed on macOS 26.6.2 as uid 501: `nc … TCP *:179 (LISTEN)`). With that listener held, `tbx doctor` must `WARN port-179` quoting the offending listener verbatim, print a runnable identification/remediation line (`sudo lsof -nP -iTCP:179 -sTCP:LISTEN`), and still exit 0. Kill the listener and confirm the check returns to `PASS port-179`. Only if the bind is refused on your host (record the error) fall back to `host-pressure`, where **observe-and-attest is the instruction**, not an induced failure. Inducing pressure on macOS is unreliable: a 14 GiB touched allocation moved free RAM only 63% → 59% on a compression-heavy host, so a failure to flip the verdict says nothing about the check. Attest instead: quote the `host-pressure` line next to the raw host numbers (`memory_pressure`, `sysctl vm.swapusage`, `df -h /System/Volumes/Data`) and confirm the verdict matches them — a faithful PASS is evidence, recorded as observe-and-attest, not SKIPPED. Optionally, if you want the induced variant too, run it with **no cluster running** (this runbook's `qa-host` is created in C2, so do this first) using a bounded allocation — a scratch process holding a few GiB, released the moment doctor has been read — and report whether `host-pressure` moved to WARN or FAIL with the measured numbers and a runnable remedy (`tbx down` / `tbx cache prune --all`). Not moving off PASS is not a FAIL for this charter. Never induce pressure while VMs are running: swap exhaustion corrupts guest writes, which is the very thing this check exists to prevent.
3. Confirm doctor never *executes* remediation — it prints commands only.

Expected observations: check set matches docs; the induced (or attested) pressure/port finding is reported with an exact, runnable remediation line and numbers that match the host; FAIL exits non-zero, WARN doesn't.

Pass criteria: set matches, the platform's induced-or-attested finding is faithful, remediation printed-not-run.

On failure: capture the full doctor transcript.

### C2 — Ballooning and overcommit guard **[macOS only]**

**Goal**: the macOS-only resource machinery behaves; Linux confirms its absence honestly.

Steps (macOS):
1. `tbx cluster create qa-host --cni cilium` (configured nodes are balloon-managed; maintenance nodes are exempt).
2. Verify printed config includes the balloon module: `tbx manifests qa-host machine` mentions `virtio_balloon` under `machine.kernel.modules` (the `balloon` section is deprecated and errors, pointing at `machine` — run it once and record the error text).
3. Create memory pressure (e.g. run a large `stress`-like allocation or record SKIPPED-if-impractical with reason); attest balloon activity from `~/.talosbox/tbxd.log`, which logs each target change as `balloon <cluster>/<node>: target=<n>MiB (configured=… hostFree=… reserve=… deficit=…)` — that log line, not `manifests`, is the runtime evidence that ballooning happened (guest-side `kubectl top nodes` deltas are corroboration at best). Observe-and-attest, not a hard assertion. Note that only TLS-configured nodes are balloon-managed on the VZ backend; a maintenance-phase `qa-host` produces no such lines by design.
4. Overcommit guard: attempt `tbx cluster create qa-big --cp 1 --workers 6 --memory-mib 4096` sized to exceed host RAM minus 6 GiB — expect a warning; confirm `--force` overrides; destroy/abort without creating if possible.

Steps (Linux):
1. `tbx doctor` reports `host-pressure: SKIP` — expected, record it.
2. Confirm the overcommit guard does NOT fire on an oversized create (document its absence — expected today, still worth a friction note reminding that nothing protects the host).

Expected observations: macOS — module printed by `machine`, `balloon` refused with a redirect, balloon targets visible in `tbxd.log` under pressure, guard warns and `--force` overrides; Linux — honest SKIP, no guard.

Pass criteria: platform-appropriate behavior exactly.

On failure: capture guard output / doctor lines.

### C3 — Spec-drift charters

**Goal**: verify the SPEC surfaces that historically drifted from the code, and pin down the one divergence that remains.

Steps (record verbatim output for each):
1. `tbx node stop qa-host qa-host-worker-1`, then `tbx status qa-host`, then `tbx node start qa-host qa-host-worker-1` — SPEC §9 lists both verbs and both are implemented. Expect: `stopped node ...` / `started node ...` on stdout, the node reported `stopped` in status between them, and the node back on its way up afterwards. A rejection or an "unknown node command" here is the finding.
2. `tbx status qa-host -o json`, `tbx cluster list -o json`, `tbx cache list -o json`, `tbx snapshot list qa-host -o json` — SPEC claims `-o json` on the list/status commands, and all four support it. Each must print valid JSON (`| python3 -m json.tool`). A rejected flag or non-JSON output is the finding.
3. **[Linux]** `tbx system install` — docs say never run it on Linux, but the code does not gate it. DO NOT actually complete it: run it WITHOUT sudo rights available and record how far it gets / what error appears (it may attempt sudo re-exec — decline the password prompt and record). If declining is not safely possible, mark SKIPPED-unsafe with reasoning.

Expected observations: steps 1 and 2 behave as described above; step 3's divergence documented with exact output. File/refresh one tracking issue per genuine divergence if none exists yet (search first).

Pass criteria: steps 1 and 2 behave as stated, and step 3 has verbatim evidence in the report.

On failure: capture the exact command and output; a SPEC-listed verb that rejects, or a `-o json` that does not parse, is a red-flag finding — report it prominently.

### C4 — Guided-output and quiet contracts

**Goal**: hints never execute; `--quiet` and `-o json` behave.

Steps:
1. `tbx status qa-host` — hints present and copy-pasteable; `tbx status qa-host --quiet` — hints gone, facts intact.
2. `tbx status qa-host -o json` — valid JSON (`| python3 -m json.tool`); `tbx cluster list -o json` — valid JSON.
3. `tbx cluster create qa-host --quiet` variant already covered implicitly — instead verify `tbx up --quiet` on a matching `talosbox.yaml` keeps **stdout** result-only (write a minimal yaml for qa-host and run it; it should reconcile as up-to-date). Stderr is allowed to carry the deadline preamble (`provisioning …; overall deadline Nm; progress suppressed by --quiet`) and, on long runs, a once-a-minute liveness line (`still provisioning …(elapsed Nm, overall deadline Nm)`) — that is the contract, not a violation. A run that turns out to be a pure no-op normally answers within the preamble's 5s grace, so it prints **no** preamble at all (#421); a no-op held longer than that by slow probes may still print one.

Expected observations: quiet suppresses narration/hints but never results or facts; stderr may carry the quiet-mode deadline preamble and liveness heartbeat; JSON parses; up is idempotent (up-to-date on rerun).

Pass criteria: all output contracts hold.

On failure: capture raw outputs.

### C5 — Destroy and cleanup (always run)

Steps: `tbx cluster destroy qa-host --force` (and any `qa-big` accident); verify no residue. Confirm the host install (helper, resolver files/units) is fully intact after the run — this runbook must never degrade the host.

Pass criteria: no residue; host integration untouched.

## Report template

```markdown
## QA deep-host-integration <platform> — <date>

- tbx version / commit; platform details:
- Preflight: OK | BLOCKED (<why>)

| Charter | Verdict | Duration | Notes |
|---|---|---|---|
| C1 doctor honesty | | | |
| C2 ballooning/guard | | | |
| C3 spec drift | | | |
| C4 output contracts | | | |
| C5 destroy | | | |

### Friction log
### Failures
```
