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
1. Write a list file with known-public, non-`latest` tagged refs from all three required real legs: Docker Hub (`docker.io/library/alpine:3.20`), `public.ecr.aws`, and `ghcr.io`. Also include `registry.k8s.io/pause:3.10`, one digest-pinned ref, one tag+digest ref, a `#` comment, and a blank line. Record the exact ECR and GHCR refs selected, then run `tbx cache warm <file>`. (`docker.io/library/pause` does NOT exist on Docker Hub and is not a valid probe.)
2. Observe upstream requests with a controlled proxy/access log or packet metadata, without recording credentials. Run the identical command again. Every complete ref must print `already complete`, and the second warm must make **zero upstream calls**.
3. Run `tbx cache warm --refresh <file>`. It revalidates only complete unpinned tags; digest-pinned refs need no freshness resolution. Record per-registry request counts, especially Docker Hub's, but do not loop, deliberately consume a quota, or claim a registry-specific quota.
4. Negative: a list containing `docker.io/library/alpine:latest` — expect rejection; a tagless ref — expect rejection; a tag+digest where the digest is wrong — expect a resolution-mismatch error.
5. `tbx cache warm --check <file>` then `--check --deep <file>`; also `--deep` without `--check` — expect refusal. Both check modes also verify the implicit CRI pod sandbox image (`registry.k8s.io/pause:<v>`) that no warm list names, so the two modes must agree on offline readiness.
6. Obtain the `qa-mir` gateway. While online, use `crane manifest --insecure <gateway>:5059/<ecr-authority>/<ecr-repository>:<tag>` for the exact Public ECR ref selected in step 1. Then issue GET and HEAD with curl against `http://<gateway>:5059/v2/<ecr-authority>/<ecr-repository>/manifests/<tag>` and its returned digest path. GET and HEAD must return the same `Docker-Content-Digest`. If the local Docker daemon permits this gateway as an insecure registry, also pull `<gateway>:5059/<ecr-authority>/<ecr-repository>:<tag>`; otherwise record Docker as BLOCKED while retaining the required crane and curl evidence.

Expected observations: all Docker Hub, Public ECR, and GHCR legs succeed and their request behavior is recorded; per-image progress continues through failures and exits non-zero on any gap; `latest`/tagless are rejected; a pinned-digest mismatch names the ref; the default second warm makes zero upstream calls; `--refresh` contacts only the unpinned tag legs; `--check` verifies offline with no upstream dials; `--deep` requires `--check`; host crane/curl requests reach the warmed Public ECR cache through the path-prefixed port-5059 form.

Pass criteria: all accept/reject behaviors as documented.

On failure: capture full command output per case.

**Closed QA task** (no action needed): the [#242](https://github.com/randax/talos-box/issues/242) Docker Hub anonymous-auth fix was confirmed on 2026-08-19 — the `docker.io/library/alpine:3.20` leg warmed cleanly on the first run. Do not re-investigate it. If the Hub leg ever fails again, sanity-check Hub anonymous auth outside tbx first (a plain anonymous token fetch + `HEAD /v2/library/alpine/manifests/3.20`); a failure there is a host/network problem — record the leg as BLOCKED on the host rather than FAIL.

#### Controlled stale-on-429 charter

A real registry quota must not be deliberately exhausted. The hermetic unit test is authoritative:

```sh
go test ./internal/mirror -run 'TestOnlineCompleteTagHEADServesStaleOnTransientUpstreamStatus|TestOnlineStaleReplayLogsReasonAndReference' -count=1
```

The controlled upstream returns 429. A complete selected-platform cache must still answer 200,
and the operator log contract must contain
`mirror served stale: registry.example/demo:stable`, `upstream status 429`, and
`cache complete for linux/<arch>`. Record the test result; any incidental real 429 may be
observed, but never induced.

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

**Goal**: machine config points public/upstream pulls at the single catch-all with no bypass,
while syntactic loopback registries deliberately redirect back into the node.

The redirect uses containerd's localhost transport convention: HTTPS with no port or port 443,
and HTTP on every other port. It cannot pass through a TLS registry on a custom loopback port;
that registry needs an explicit `machine.registries.mirrors` entry. The redirect also changes the
host, so credential forwarding depends on containerd re-authorizing the redirected request through
its `CheckRedirect` hook.

Steps:
1. `tbx manifests qa-mir mirrors` — expect `"*"` → `http://172.30.<n>.1:5059`, `skipFallback: true`.
2. From a test pod, pull an image that is NOT cached and NOT on any warm list (while online): confirm it arrives via the mirror (mirror stats change: `tbx cache list` before/after shows the upstream's counters grow). Confirm the image was novel first with `tbx cache list <image-ref>`, which answers `cached` / `not cached` for that one reference — pick a tag-pinned ref for this (not `:latest`, not tagless: `cache list <ref>` applies the same ref validation `cache warm` does and rejects those forms with an error rather than an answer). Base the test pod on the [PSA-compliant test pod](deep-storage.md#psa-compliant-test-pod) (swap in the image under test) so the apply does not emit a `would violate PodSecurity "restricted:latest"` block — that block is a warning, not a rejection, and is not a finding.
3. Run pinned Talos/containerd pulls from an anonymous HTTP registry and a credentialed HTTP registry exposed at separate `localhost:<nodeport>` authorities. Both must succeed. Compare `tbx cache list` and `tbx logs` before and after: neither pull may create mirror cache activity or an upstream request. Record each authority, whether it was anonymous or credentialed, the pull command and result, and the before/after cache and log outcome. This live acceptance check is required because an HTTP unit test proves the `307` response but not containerd's redirect and credential behavior.

Expected observations: the rendered mirror config matches the live pull behavior; `cache list` per-upstream counters reflect only the public pull; the recorded anonymous and credentialed loopback pulls both remain direct.

Pass criteria: public pulls route through the mirror; anonymous and credentialed loopback pulls do not; config matches.

On failure: capture `manifests mirrors` output and `cache list` before/after.

### C4 — Offline mode semantics (depends on C3)

**Goal**: mirror cache-only behavior and node fallback policy remain distinct; mode survives restarts.

Steps:
1. Warm one specific tagged image from C1, then run `tbx cache warm --check <file>`. The check must report it complete for the selected `linux/<arch>` graph, not merely because a root manifest returns 200. Turn on offline mode; `tbx mirror offline` reports `on`.
2. Against the catch-all endpoint, issue both HEAD and GET for that tag through containerd's unchanged `?ns=` form and through `http://<gateway>:5059/v2/<upstream-authority>/<repository>/manifests/<tag>`. Record 200 plus the same `Docker-Content-Digest` from both. Repeat the path form by digest and with the C1 `crane manifest --insecure` command. There must be zero DNS or upstream calls. From the C3 PSA-compliant test pod, perform a fresh actual image pull by tag and then by digest. All observations — `--check`, both catch-all forms, crane, and containerd's blob pulls — must agree for the selected platform.
3. With the generated `"*"` entry (`skipFallback: true`) unchanged, pull an uncached public image. Also request an uncached path-prefixed host ref. Both catch-all forms return the same 404 with the offline/not-cached reason, create no upstream connection, and the node must fail quickly rather than bypassing the mirror.
4. On this disposable cluster, add a more-specific test registry mirror entry pointing at the same catch-all endpoint but setting `skipFallback: false`. Pull a different uncached image from that registry while host connectivity remains online. The mirror still returns its offline 404, but the node resolver may fall through to the real upstream and the pull must succeed. Record the exact machine-config patch and ref so the two policy layers are auditable.
5. Repeat one `localhost:<nodeport>` pull from C3 — it still succeeds because syntactic loopback passthrough performs no public-upstream mirror access.
6. Restart the daemon itself with `tbx system restart --force` (`--force` is required here: `qa-mir` is running, and a plain restart refuses rather than stop it; on a supervised install — packaged Linux units — restart the tbxd service via the service manager instead). If neither is available, exercise the cluster path: `tbx cluster stop qa-mir && tbx cluster start qa-mir`. Afterwards `tbx mirror offline` still reports `on`.
7. `tbx system restart --force` stops running clusters and does **not** bring them back (it narrates `stopped clusters: qa-mir`), so start the cluster again before the next step: `tbx cluster start qa-mir`, then wait for the API to answer — allow ~1 min to settle. Skip this step only if step 6 took the `cluster stop`/`start` path, which already leaves the cluster running.
8. `tbx mirror offline off`; the previously failing strict-catch-all public pull now succeeds. Confirm the Talos/containerd pull still uses the generated query-based wildcard endpoint and that fixed ports 5055–5058 still serve their legacy upstreams.

Expected observations: cache check, tag HEAD/GET, and real selected-platform blob pulls agree; the generated strict catch-all produces a clear 404-backed hard miss; the explicit `skipFallback: false` entry falls through after the same mirror 404; loopback remains direct; mode persists; `off` restores pull-through.

Pass criteria: all behaviors exact.

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
