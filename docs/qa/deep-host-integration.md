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
1. `tbx doctor` on a healthy host — record every check name and result. Compare the set against the documented platform list (macOS: helper, vmnet, DNS wiring, forwarding, routes, host-pressure, egress; Linux additionally: helper-unit/access/capabilities, kvm, qemu, forwarding, bridge-netfilter, bridge-stp, rp-filter, port-53/67/179, resolver trio, units).
2. Break one innocuous thing and confirm doctor catches it: occupy port 179 (`nc -l 179` as root or a high-privilege listener) → doctor `port-179` degrades with a specific message. Release it.
3. Confirm doctor never *executes* remediation — it prints commands only.

Expected observations: check set matches docs; the induced failure is caught with an exact, runnable remediation line; FAIL exits non-zero, WARN doesn't.

Pass criteria: set matches, induced failure caught, remediation printed-not-run.

On failure: capture the full doctor transcript.

### C2 — Ballooning and overcommit guard **[macOS only]**

**Goal**: the macOS-only resource machinery behaves; Linux confirms its absence honestly.

Steps (macOS):
1. `tbx cluster create qa-host --cni cilium` (configured nodes are balloon-managed; maintenance nodes are exempt).
2. Verify printed config includes the balloon module: `tbx manifests qa-host balloon` (or `machine`) mentions `virtio_balloon`.
3. Create memory pressure (e.g. run a large `stress`-like allocation or record SKIPPED-if-impractical with reason); watch for balloon inflation evidence in guest free memory (`kubectl top nodes` deltas) — this is an observe-and-attest check, not a hard assertion.
4. Overcommit guard: `cluster create` has no sizing flags — write a minimal `talosbox.yaml` (1 cp + 6 workers, `node.memory: 4GiB`) sized to exceed host RAM minus 6 GiB and run `tbx up -f <file>` — expect refusal with nothing created; confirm `--force` overrides. Note: while #231 is open, the swap-keyed guard may fire first and mask the RAM-sum guard.

Steps (Linux):
1. `tbx doctor` reports `host-pressure: SKIP` — expected, record it.
2. Confirm the overcommit guard does NOT fire on an oversized create (document its absence — expected today, still worth a friction note reminding that nothing protects the host).

Expected observations: macOS — module printed, guard warns, `--force` overrides; Linux — honest SKIP, no guard.

Pass criteria: platform-appropriate behavior exactly.

On failure: capture guard output / doctor lines.

### C3 — Spec-drift charters

**Goal**: pin down the known doc/code divergences so they are consciously resolved, not rediscovered.

Steps and expected current behavior (each records verbatim behavior; the drift itself is the finding):
1. `tbx node start qa-host qa-host-cp-1` / `tbx node stop ...` — SPEC §9 lists these; CLI expected to reject (unimplemented). Record the error.
2. `tbx snapshot list qa-host -o json` and `tbx cache list -o json` — SPEC claims `-o json` on all list/status commands; code supports it only on `status` and `cluster list`. Record actual behavior of each.
3. **[Linux]** `tbx system install` — docs say never run it on Linux, but the code does not gate it. DO NOT actually complete it: run it WITHOUT sudo rights available and record how far it gets / what error appears (it may attempt sudo re-exec — decline the password prompt and record). If declining is not safely possible, mark SKIPPED-unsafe with reasoning.

Expected observations: each divergence documented with exact output; file/refresh one tracking issue per genuine divergence if none exists yet (search first).

Pass criteria: all three drift items have verbatim evidence in the report.

On failure: n/a (this charter's failures are its findings) — but a *silent success* of a supposedly-unimplemented verb is a red-flag finding; report it prominently.

### C4 — Guided-output and quiet contracts

**Goal**: hints never execute; `--quiet` and `-o json` behave.

Steps:
1. `tbx status qa-host` — hints present and copy-pasteable; `tbx status qa-host --quiet` — hints gone, facts intact.
2. `tbx status qa-host -o json` — valid JSON (`| python3 -m json.tool`); `tbx cluster list -o json` — valid JSON.
3. `tbx cluster create qa-host --quiet` variant already covered implicitly — instead verify `tbx up --quiet` on a matching `talosbox.yaml` stays silent except the final result (write a minimal yaml for qa-host and run it; it should reconcile as up-to-date).

Expected observations: quiet suppresses narration/hints but never results; JSON parses; up is idempotent (up-to-date on rerun).

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
