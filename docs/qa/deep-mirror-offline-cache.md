# QA Runbook: Mirror, offline & cache

| | |
|---|---|
| **Tier** | Deep (one feature area exercised against defaults) |
| **Platform** | macOS + Linux (port-5000 hazard is macOS-flavored but checks run everywhere) |
| **Estimated duration** | 45–60 min |
| **Destructive** | Creates and destroys cluster `qa-mir`; **prunes the mirror cache and any orphan disk images** (C5 runs an unscoped `cache prune` and then a `--mirror` prune — do NOT run on a host whose cache you need to keep). In-use, pinned, and default disk images survive by design |
| **Runbook version** | against talos-box main @ the commit recorded in your report |

## How to execute this runbook (agent instructions)

You are running QA, not demos. For every charter: run the steps exactly, compare against **Expected observations**, and record **PASS**, **FAIL**, or **PASS-with-friction**. Friction — confusing messages, doc/behavior mismatch, missing knowledge, misleading output — is a first-class result even on passing charters. No improvised recovery: capture the **On failure** evidence, mark FAIL, continue unless a dependency broke.

**Report destination**: one `qa-run` issue, title `QA deep-mirror-offline-cache <platform> <date>`.

## Preflight

BLOCKED unless: `tbx version` recorded; `tbx doctor` exits 0 (egress OK); no cluster `qa-mir`; the host's mirror cache and any orphan disk images are expendable (see Destructive above); online.

## Charters

### C1 — Warm list contract

**Goal**: `cache warm` accepts pinned refs, rejects unpinned, and reports per-image progress.

Steps:
1. Write a list file with: one tag-pinned ref (`registry.k8s.io/pause:3.10` — `docker.io/library/pause` does NOT exist on Docker Hub and answers 401 to anonymous pulls, so it is not a valid probe), one tag-pinned ref on Docker Hub itself (`docker.io/library/alpine:3.20`, so the run exercises Hub's anonymous token flow), one digest-pinned ref, one tag+digest ref, a `#` comment, and a blank line. Run `tbx cache warm <file>`.
2. Run again — record what a re-warm does. It downloads nothing but still re-resolves every tag-pinned ref upstream, so it costs about a full run's wall time; expect the summary to be followed by a `note: re-resolved <n> tag(s) upstream ...` line steering to `--check` for a cheap gate.
3. Negative: a list containing `docker.io/library/alpine:latest` — expect rejection; a tagless ref — expect rejection; a tag+digest where the digest is wrong — expect a resolution-mismatch error.
4. `tbx cache warm --check <file>` then `--check --deep <file>`; also `--deep` without `--check` — expect refusal. Both check modes also verify the implicit CRI pod sandbox image (`registry.k8s.io/pause:<v>`) that no warm list names, so the two modes must agree on offline readiness.

Expected observations: per-image progress with continue-through-failures and non-zero exit on any gap; `latest`/tagless rejected by design; mismatch pinned-digest error names the ref; `--check` verifies offline (no upstream dials — verify by watching for network errors with upstream unreachable if feasible, else trust exit + wording); `--deep` requires `--check`.

Pass criteria: all accept/reject behaviors as documented.

On failure: capture full command output per case.

**Closed QA task** (no action needed): the [#242](https://github.com/randax/talos-box/issues/242) Docker Hub anonymous-auth fix was confirmed on 2026-08-19 — the `docker.io/library/alpine:3.20` leg warmed cleanly on the first run. Do not re-investigate it. If the Hub leg ever fails again, sanity-check Hub anonymous auth outside tbx first (a plain anonymous token fetch + `HEAD /v2/library/alpine/manifests/3.20`); a failure there is a host/network problem — record the leg as BLOCKED on the host rather than FAIL.

### C2 — Gateway-only binds and port layout (depends on a running cluster)

**Goal**: mirror ports live on cluster gateways only, added/removed with cluster lifecycle.

Steps:
1. `tbx cluster create qa-mir --cni cilium` (provisioned so nodes actually pull through the mirror).
2. On the host: check listeners — `lsof -iTCP:5059 -sTCP:LISTEN` (macOS) / `ss -tlnp | grep 5059` (Linux). Also ports 5055–5058.
3. Confirm binds are on the cluster gateway IP (`172.30.<n>.1`), NOT `0.0.0.0` and NOT the host's LAN address.
4. `tbx cluster stop qa-mir`; re-check — binds for that gateway gone. `tbx cluster start qa-mir` to continue.
5. macOS only: confirm nothing tbx-owned listens on 5000 (AirPlay hazard).

Expected observations: catch-all `:5059` on the gateway; legacy 5055–5058 present per current behavior (record exactly which); binds tied to cluster start/stop; no wildcard binds ever.

Pass criteria: gateway-only binds, lifecycle-tied.

On failure: capture the listener table verbatim.

### C3 — skipFallback contract on the node (depends on C2)

**Goal**: machine config points all pulls at the single catch-all with no upstream bypass.

Steps:
1. `tbx manifests qa-mir mirrors` — expect `"*"` → `http://172.30.<n>.1:5059`, `skipFallback: true`.
2. From a test pod, pull an image that is NOT cached and NOT warmable (while online): confirm it arrives via the mirror (mirror stats change: `tbx cache list` before/after shows the upstream's counters grow). Confirm the image was novel first with `tbx cache list <image-ref>`, which answers `cached` / `not cached` for that one reference.

Expected observations: the rendered mirror config matches the live pull behavior; `cache list` per-upstream counters reflect the pull.

Pass criteria: pulls route through the mirror; config matches.

On failure: capture `manifests mirrors` output and `cache list` before/after.

### C4 — Offline mode semantics (depends on C3)

**Goal**: `mirror offline on` serves cache-only and fails misses hard; mode survives restarts.

Steps:
1. Warm one specific tag-pinned image (C1's list). `tbx mirror offline on`; `tbx mirror offline` reports `on`.
2. From a test pod, pull the warmed image by tag — succeeds from cache. Pull it by digest — also succeeds (digest/tag parity).
3. Pull an uncached image — expect a hard failure mentioning offline/not-cached (skipFallback means the node cannot bypass; the pull fails, it does not hang).
4. Restart the daemon itself with `tbx system restart --force` (`--force` is required here: `qa-mir` is running, and a plain restart refuses rather than stop it; on a supervised install — packaged Linux units — restart the tbxd service via the service manager instead). If neither is available, exercise the cluster path: `tbx cluster stop qa-mir && tbx cluster start qa-mir`. Afterwards `tbx mirror offline` still reports `on`.
5. `tbx system restart --force` stops running clusters and does **not** bring them back (it narrates `stopped clusters: qa-mir`), so start the cluster again before the next step: `tbx cluster start qa-mir`, then wait for the API to answer — allow ~1 min to settle. Skip this step only if step 4 took the `cluster stop`/`start` path, which already leaves the cluster running.
6. `tbx mirror offline off`; the previously failing pull now succeeds.

Expected observations: cached tag AND digest served offline; miss = clear hard failure, not a hang; mode persists; `off` restores pull-through.

Pass criteria: all five behaviors exact.

On failure: capture pod events (`kubectl describe pod`), mirror mode outputs.

### C5 — Prune scopes (depends on C4)

**Goal**: prune scope boundaries hold.

Steps:
1. `tbx cache list` — record disk images and mirror totals, and record each image's labels — every keep-reason that applies (`in-use`, `pinned`, `default`, e.g. `pinned, in-use (qa-mir)`), or `orphan` when none does — this listing is the prune preview.
2. `tbx cache prune` (no flag) — reference-aware, so it removes only the `orphan` combinations and keeps every `in-use`, `pinned`, and `default` one; the mirror is untouched. Expect it to name each removed combination with its size, then a summary of the form `pruned disk cache: <n> image(s), <bytes> bytes (kept <m> image(s) in use, pinned, or default); mirror cache untouched`, with `<m>` matching the labels recorded in step 1. On a default cache with nothing orphaned, `<n>` is 0 and the default combination survives — that is the documented outcome, not a failure. Confirm with `cache list` that exactly the `orphan` rows disappeared.
3. `tbx cache prune --mirror` — mirror content gone, remaining disk images (if any re-pulled) intact.
4. `tbx cache prune --mirror --all` — expect flag-exclusivity refusal.

Expected observations: scopes exactly as documented; the unscoped prune reports what it kept as well as what it removed, and never deletes an in-use, pinned, or default combination; explicit output about what was and wasn't touched.

Pass criteria: scope boundaries, keep rules, and exclusivity hold.

On failure: capture `cache list` before/after each prune.

### C6 — Destroy and cleanup (always run)

Steps: `tbx mirror offline off` (leave pull-through); `tbx cluster destroy qa-mir --force`; verify no residue; note that mirror cache emptiness is expected here only because C5 pruned it.

Pass criteria: no residue; mirror mode back to off.

## Report template

```markdown
## QA deep-mirror-offline-cache <platform> — <date>

- tbx version / commit; platform details:
- Preflight: OK | BLOCKED (<why>)

| Charter | Verdict | Duration | Notes |
|---|---|---|---|
| C1 warm contract | | | |
| C2 gateway binds | | | |
| C3 skipFallback | | | |
| C4 offline semantics | | | |
| C5 prune scopes | | | |
| C6 destroy | | | |

### Friction log
### Failures
```
