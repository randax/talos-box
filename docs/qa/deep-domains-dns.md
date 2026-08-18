# QA Runbook: Domains & DNS

| | |
|---|---|
| **Tier** | Deep (one feature area exercised against defaults) |
| **Platform** | macOS + Linux (host-integration charters marked) |
| **Estimated duration** | 40–60 min |
| **Destructive** | Creates and destroys clusters `qa-dom`, `qa-sub`; touches per-domain resolver state via normal tbx lifecycle only |
| **Runbook version** | against talos-box main @ the commit recorded in your report |

## How to execute this runbook (agent instructions)

You are running QA, not demos. For every charter: run the steps exactly, compare against **Expected observations**, and record **PASS**, **FAIL**, or **PASS-with-friction**. Friction — confusing messages, doc/behavior mismatch, missing knowledge, misleading output — is a first-class result even on passing charters. No improvised recovery: capture the **On failure** evidence, mark FAIL, continue unless a dependency broke.

**Report destination**: one `qa-run` issue, title `QA deep-domains-dns <platform> <date>`.

## Preflight

BLOCKED unless: `tbx version` recorded; `tbx doctor` exits 0; no clusters `qa-dom`/`qa-sub`; ≥ 10 GiB free RAM.

## Charters

### C1 — Custom safe domain end to end

**Goal**: a custom domain under a safe suffix works: wildcard, node records, no apex.

Steps:
1. `tbx cluster create qa-dom --domain lab.internal`
2. Resolve: `<node>.lab.internal` (each node), `anything.lab.internal` (wildcard → `.200`), and `lab.internal` itself (apex — expect NXDOMAIN/no record). Use `dscacheutil -q host -a name ...` on macOS, `resolvectl query` (or the gateway `dig` fallback) on Linux.

Expected observations: node records and wildcard resolve to status-reported IPs / the `.200` VIP; the apex has no record; **[macOS]** `/etc/resolver/lab.internal` exists with an ownership marker; **[Linux]** resolved shows a route-only `~lab.internal` on the bridge.

Pass criteria: all three resolution behaviors correct.

On failure: capture resolver file contents / `resolvectl status`, `tbx doctor` DNS lines.

### C2 — Unsafe domain gate

**Goal**: the safety boundary is enforced and the opt-in works, non-interactively.

Steps:
1. `tbx cluster create qa-bad --domain corp.example.com` — expect refusal naming the unsafe-domain opt-in.
2. `tbx cluster create qa-bad --domain foo.local` — expect unconditional refusal (`.local` never allowed), likewise `--domain foo.invalid` and a single-label `--domain foo`.
3. `tbx cluster create qa-bad --domain corp.example.com --allow-unsafe-domain` — expect success (then destroy it: `tbx cluster destroy qa-bad --force`).

Expected observations: refusals are specific and instant (no partial creation); the always-rejected list (`.local`, `.localhost`, `.invalid`, single-label) is not overridable even with the flag — verify at least `.local` with the flag set; the opt-in path succeeds without any interactive prompt.

Pass criteria: gate behavior exactly as documented, nothing half-created.

On failure: capture each error text.

### C3 — Nested domains resolve longest-suffix-wins (depends on C1)

**Goal**: domain nesting across clusters is owned correctly.

Steps:
1. `tbx cluster create qa-sub --domain deep.lab.internal --allow-unsafe-domain` if prompted-by-error, otherwise plain (record which was needed — `deep.lab.internal` nests under C1's domain).
2. Resolve `x.deep.lab.internal` (→ qa-sub's `.200`) and `x.lab.internal` (→ qa-dom's `.200`) — different VIPs.
3. Stop `qa-sub`; verify `x.deep.lab.internal` **still** resolves authoritatively to qa-sub's `.200` and its node records still answer (DNS reflects cluster existence, not run-state — SPEC §5), while `x.lab.internal` still resolves to qa-dom's `.200`. The stopped cluster's names now point at addresses that will not respond; that is expected.
4. Destroy `qa-sub`; verify `x.deep.lab.internal` now NXDOMAINs (withdrawal happens on destroy alone) while `x.lab.internal` still resolves to qa-dom's `.200`.

Expected observations: longest suffix wins; the owning cluster answers or NXDOMAINs alone; stop leaves records answering, destroy withdraws them; no cross-cluster bleed.

Pass criteria: both wildcards land on their own cluster's VIP; stop changes no resolution; destroy — and only destroy — produces NXDOMAIN for the nested domain.

On failure: capture both resolutions and the resolver state.

### C4 — Resolver lifecycle hygiene **[macOS]** / resolved hygiene **[Linux]**

**Goal**: per-domain host state is created, reconciled, and orphan-removed; unmarked files untouched.

Steps:
1. **[macOS]** Record `md5 /etc/resolver/k8s.test` and `ls -l /etc/resolver/` before. Destroy `qa-dom`; verify `/etc/resolver/lab.internal` (marked, owned) is removed while `/etc/resolver/k8s.test` — the shared default-domain file, which carries **no** tbx marker — survives byte-identical (same md5, same mtime). That unmarked survivor is the marker-gated-deletion evidence this charter needs, and it requires no root.
   - Optional, human runners with root only: additionally plant an unmarked decoy (`sudo tee /etc/resolver/qa-decoy` with minimal content and no tbx marker) before the destroy and confirm it too is untouched, then `sudo rm` it. Agent runs without sudo record this variant as NOT RUN — the k8s.test evidence above already covers the property; a missing decoy is not a gap in the verdict.
2. **[Linux]** Destroy `qa-dom`; verify the resolved route-only registration for `lab.internal` is gone and `/etc/resolv.conf` untouched throughout.

Expected observations: marker-gated deletion only — the owned, marked per-domain file goes and every unmarked file (at minimum the shared `k8s.test`, plus the decoy when the root variant ran) stays byte-identical; no foreign state touched.

Pass criteria: exactly the owned state removed, nothing else.

On failure: list resolver/resolved state before and after.

### C5 — Destroy and cleanup (always run)

Steps: destroy `qa-dom`, `qa-sub`, any `qa-bad` remnant with `--force`; verify no residue in status, disk, resolver/resolved state.

Pass criteria: no residue.

## Report template

```markdown
## QA deep-domains-dns <platform> — <date>

- tbx version / commit; platform details:
- Preflight: OK | BLOCKED (<why>)

| Charter | Verdict | Duration | Notes |
|---|---|---|---|
| C1 custom domain | | | |
| C2 unsafe gate | | | |
| C3 nesting | | | |
| C4 resolver hygiene | | | |
| C5 destroy | | | |

### Friction log
### Failures
```
