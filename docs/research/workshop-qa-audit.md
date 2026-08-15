# Platform-Engineering-Workshop QA audit (ticket #213)

Audit of the local clone at `~/projects/Platform-Engineering-Workshop` (JavaZone 2026,
"Cloud on Your Terms", 240 min). Purpose: give the QA walkthrough (#216) an ordered
checkpoint skeleton, the list of steps with no verifiable outcome, external dependencies,
and rot-prone pins. Audited 2026-08-15 against the repo's `main`.

## 1. Setup / bootstrap architecture

**Attendee flow (at home, before the conference):**

1. `./scripts/dev-setup.sh` — installs mise (`curl https://mise.run`, pinned
   `MISE_VERSION=v2026.7.3`), then `mise install` for the pinned tools in `mise.toml`:
   talosctl 1.13.8, kubectl 1.36.2, helm 3.21.3, kind 0.32.0, crane 0.21.7,
   cilium-cli 0.19.7, jq 1.8.2, node 24.
2. `./scripts/cloudbox-init.sh` — the only big download (~7.5 GB arm64 / 7.7 GB amd64):
   - Preflights every ref in `scripts/images.txt` with `crane manifest` (fail-fast).
   - `[host]` images (3) → `docker pull` into the host engine (Talos node image,
     registry:3.1.1, kind fallback node).
   - `[mirror]` images (65) → `crane copy` into a local OCI registry container
     `cloudbox-mirror` on `localhost:5001` (persistent volume `cloudbox-mirror-data`).
     Tag-pinned refs are copied single-arch (`--platform linux/<daemon-arch>`);
     digest-pinned refs are copied as full indexes (containerd 2.x requires the whole
     index). Resumable/idempotent.
   - Optionally `ollama pull qwen3:4b` for module 10's kagent (warns if ollama absent;
     `--skip-model-pull` opt-out).
3. `./scripts/install.sh --check` — read-only go/no-go gate: arch (amd64/arm64), OS
   (macOS/Linux/WSL2), Docker daemon, ≥4 CPUs, ≥10 GB Docker memory, ≥40 GB disk,
   NodePorts free (30300/30080/30500/30600/30700/30900/31080), pinned CLI versions,
   all 3 host images in the Docker cache, all 65 mirror images present in the mirror
   **and** architecture-matched to the daemon (exempt list: the amd64-only Backstage
   image), mirror reachable from container context (not just localhost).

**Offline model:** `create-cluster.sh` points the Talos machine-config registry mirrors
at `cloudbox-mirror` with `skipFallback: false` — i.e. a missing mirror image silently
falls back to the internet (works at home, hangs at the venue). The preflight's
image-count and arch checks are therefore the real offline guarantee. The stated
contract is "no image downloads at the venue" with exactly one documented exception
(module 10 beat 2, hosted LLM API).

**images.txt discipline:** 68 image refs total; 27 digest-pinned (`@sha256`),
41 tag-pinned; zero `:latest`. Comments say verified upstream 2026-07-13 (sizes
re-measured 2026-08-11) with an explicit "re-verify late August" instruction. Two
checkers guard the file: `scripts/check-consistency.sh` and a crane-based CI gate
(`.github/workflows/images-gate.yaml`); `scripts/check-upstream.sh` (also weekly CI)
diffs every pin in `scripts/upstream.list` (35 tracked upstreams) against releases.
`scripts/versions.env` is the single source for script pins and must be kept in sync
with `mise.toml` by hand (documented, but a two-file invariant).

**Other bootstrap scripts:** `create-cluster.sh` (Talos-in-Docker, 1 CP @4 GB +
1 worker @6 GB, subnet 10.5.0.0/24, then vendored Cilium 1.20.0 chart),
`kind-fallback.sh` (kind 0.32.0 + Cilium, loses Talos content),
`bootstrap-gitops.sh` (local-path-provisioner imperatively, then vendored Gitea chart
12.7.0 + vendored ArgoCD v3.5.0 install.yaml), `seed-gitea.sh` (force-push checkout →
`cloudbox/platform` via push-to-create + applies the root app-of-apps),
`catch-up.sh <module>` (force-push canonical `solutions/module-NN` state, wait for
convergence, run `post.sh`; `--rebuild` = destroy/recreate first, ~10 min),
`destroy-cluster.sh` (keeps the mirror).

## 2. Ordered checkpoint skeleton (module → verifiable observations)

Every module ships `verify.sh` (checks the *running cluster*, exit 0 = done) and
`solve.sh` (canonical end-to-end solution, used for CI regression). This is the QA
walkthrough's backbone: run `solve.sh` then `verify.sh` per module, in order.
From module 08 on, `http://localhost:30600/workshop` is a live (advisory) progress
dashboard; `verify.sh` stays authoritative.

**00 — Setup & pre-flight (gate).**
Observations: `install.sh --check` exits 0 (all items above); `lab/00-setup/verify.sh`
green: Docker up with ≥10 GB, disk free, CLIs present (talosctl, kubectl, helm, cilium,
jq, git, curl), mirror answering on :5001; `curl -s localhost:5001/v2/_catalog` lists
repos.

**01 — Talos + Cilium cluster (core, 35 min).**
Do: `./scripts/create-cluster.sh`, then talosctl exploration.
Observations: cloudbox Docker containers exist; `kubectl get nodes` → 2 Ready; Cilium
DaemonSet fully available; `cilium-dbg status` reports `KubeProxyReplacement: True`;
no kube-proxy pods/DS anywhere. Secondary probes: `talosctl -n 10.5.0.2 get members`,
`dashboard`, machineconfig shows `cni: none` + proxy disabled.

**02 — GitOps: Gitea + ArgoCD (core, 35 min).**
Do: `bootstrap-gitops.sh`, `seed-gitea.sh`, clone from Gitea, add
`gitops/apps/demo.yaml` + `gitops/components/demo/welcome.yaml` (owner = attendee
name), push; then a drift-revert experiment (`kubectl edit` the ConfigMap, watch
self-heal snap it back).
Observations: Gitea answers on :30300 and hosts `cloudbox/platform`; ArgoCD answers
on :30080, admin password readable from `argocd-initial-admin-secret`; root `platform`
app points at in-cluster Gitea (not GitHub) and is Healthy; wave-0 storage app healthy;
`demo` namespace + `welcome` ConfigMap exist with a real name in `owner`. Timing note:
ArgoCD polls ~3 min — QA should use UI Refresh or expect latency.

**03 — Data services: CNPG + RustFS (core, 35 min).**
Do: enable `cnpg-operator.yaml` + `rustfs.yaml` from the catalog via git; deliver
`postgres-cluster.yaml` (CNPG Cluster `app-db`) into `gitops/components/demo/`;
create bucket `app-assets` + upload + presign against :30900
(creds `cloudbox`/`cloudbox123`).
Observations: both ArgoCD apps Healthy; operator deployment up in `cnpg-system`;
`app-db` "Cluster in healthy state" 1/1; `SELECT 1` returns 1 inside `app-db-1`;
RustFS answers S3 on :30900; bucket `app-assets` exists with ≥1 object.

**04 — Self-service: Crossplane v2 (core, 35 min).**
Do: enable `crossplane.yaml`; ship XRD + Composition as component + app
(`platform-api-app.yaml`); push `examples/my-database.yaml` (a 10-line
`WorkshopDatabase`).
Observations: crossplane + platform-api apps Healthy; `function-patch-and-transform`
installed/healthy; XRD `ESTABLISHED True`; Composition exists; `my-db` Synced **and**
Ready; composed CNPG cluster `my-db-pg` healthy; bucket `my-db-assets` exists in
RustFS. Timing: Crossplane install 1–2 min; readiness bubbles up in 2–3 min.

**05 — Debug it (core, 25 min).**
Do: `./inject.sh <n>` for ≥2 of 4 faults (namespaces `faultlab-NN`), write a
one-sentence diagnosis, falsify against live state, fix; optional read-only-kubeconfig
AI loop (`make-readonly-kubeconfig.sh`, 4 h token).
Observations: `verify.sh` checks the *outcome* for every `faultlab-*` namespace that
exists (workload availability, DB readiness; fault 4 gets repeated connection attempts
so a half-fix fails) and that the platform (demo apps, ArgoCD health) survived.
CI shape: `solve.sh` = inject all → `restore.sh all`. Caveat: with zero injected
namespaces verify passes vacuously — QA must inject explicitly.

**06 — Serverless: Knative (stretch).**
Do: enable `knative-serving.yaml`; deliver `hello-ksvc.yaml` via git; curl through
Kourier :31080 with the ksvc's Host header; watch 0 → 1 → 0.
Observations: knative-serving app Healthy, deployments up; ksvc `hello` Ready; curl
via :31080 with correct Host returns 200 + expected body; revision scales back to zero
(verify waits up to ~2 min of quiet).

**07 — In-cluster CI: Argo Workflows + BuildKit + Zot (stretch; flagged
"least-rehearsed path" by the repo itself).**
Do: enable `zot.yaml` + `argo-workflows.yaml`; seed the base image into Zot
(`crane copy docker.io/library/busybox:1.37.0 localhost:30500/library/busybox:1.37.0`);
`kubectl create -f workflow-run.yaml`; deliver `hello-site.yaml` via git.
Observations: both apps Healthy; Zot API answering on :30500; ≥1 `build-hello-site-*`
Workflow Succeeded; `hello-site` in Zot's `/v2/_catalog`; hello-site Deployment
Available and serving the page. **Offline caveat:** the documented `crane copy` source
is Docker Hub, i.e. it needs internet at the venue even though the same
busybox:1.37.0 sits in cloudbox-mirror at :5001 — QA should flag/verify the
mirror-sourced alternative.

**08 — Portal: Cloudbox Console (stretch).**
Do: enable `portal.yaml`; grant `portal-access.yaml` via git; create `console-db`
(size small) through the "New database" form. Optional deeper paths need
`portal-functions-access.yaml`, `portal-applications-access.yaml`,
`portal-projects-access.yaml`, and (for build-from-repo) seeding a golang base image
into Zot.
Observations: portal app Synced/Healthy; deployment ready; `/healthz` → `ok` on
:30600; `portal` ServiceAccount exists; `console-db` is a Ready `WorkshopDatabase`
with a healthy CNPG cluster. Caveat: verify cannot distinguish form-created from
kubectl-created (`solve.sh` uses kubectl) — the UI path itself needs manual/browser QA.
Backstage is presenter-demo only (:30700, guest sign-in) — no verify.

**09 — Capstone: picture pipeline (stretch; needs 03+06+08 or `catch-up.sh 8`).**
Do: enable `knative-eventing.yaml` + `picture-pipeline.yaml`; upload a photo at
:30600/gallery; inspect `originals/`, `thumbs/`, `meta/` in bucket `images`; enable
the 5-app observability set (victoria-metrics/-logs/-traces, grafana, otel-collector)
and find the trace in Grafana :30030.
Observations: both apps Healthy; eventing control plane + Broker data plane up;
Broker `default` and Trigger `resize-on-upload` Ready; both ksvcs Ready; bucket
`images` exists; every batch of originals has ≥1 matching thumbnail. The upload
itself needs a human or `solve.sh` (curl multipart POST). Known flake with a
documented workaround: Trigger latching `BrokerNotConfigured` → annotate to
re-reconcile (solve.sh does this). The Grafana-trace flourish and the observability
enablement are NOT covered by verify.sh.

**10 — Day-2 ops: roll back a bad release (stretch; needs only 02).**
Do: two-phase `./inject.sh <1|2|3>` (first run seeds `demo-web` baseline into
`cloudbox/platform` and stops; second run pushes the fault commit); diagnose;
`git revert` + push (live edits explicitly don't count).
Observations: `verify.sh` is two-sided — fails while Git still contains the poisoned
value, and independently requires the live rollout complete with no
crashlooping/restarting pods. `restore.sh <n>` / `solve.sh` are the canonical
reverts. The kagent beats (below) have no verify coverage.

## 3. Steps with NO verifiable outcome (QA walkthrough should flag)

- **All explain-backs** (every module) — by design human-only, but nothing marks a
  module "done" beyond verify.sh; fine, just note it.
- **01:** the entire talosctl exploration (machineconfig, dashboard, members,
  services, KubePrism) is unchecked; verify only covers cluster/Cilium state.
- **02 step 4:** the self-heal "cheat" experiment (manual edit → snap-back) is not
  verified; also the going-deeper orphan/re-adopt exercise (which even warns to re-run
  verify afterwards) has no check of its own.
- **03 step 3 (partial):** verify checks bucket + object, but not that the **presigned
  URL** actually works in a browser — the module's stated trophy.
- **04 going-deeper:** size ripple (small→medium→large), `xlarge` rejection, teardown
  on delete — all unchecked.
- **05:** the written one-sentence diagnosis and the "agent claimed X; I checked Y"
  deliverable are unverifiable; and verify passes vacuously if no fault was injected
  (QA harness must drive inject.sh itself).
- **06 step 3 (partial):** the cold-start observation (0→1 on first curl, latency) is
  unchecked; verify covers 200 + scale-to-zero only.
- **07 step 4 (partial):** "ask Zot's API what's in the registry" is attendee
  exploration; covered indirectly by verify's catalog check.
- **08 step 2:** exploring the console pages ("which Kubernetes API is this?") — no
  check; **step 4's form-vs-API distinction** is unverifiable (solve.sh bypasses the
  form); the Backstage presenter demo (catalog → template → repo → app) has no verify
  at all; all four "going deeper" console flows (functions, applications, projects,
  build-from-repo/Redeploy) are unchecked.
- **09 step 2:** the live "moment" (watch pods 0→1→0 during upload) needs a human;
  **step 5** (enable observability + find the trace in Grafana) has zero verify
  coverage — the whole Victoria/OTel enablement can silently fail QA.
- **10 kagent half:** beat 1 (watch qwen3:4b flail, write down how) and beat 2
  (Zen ModelConfig switch → real diagnosis) have no verify; the empty-Secret footgun
  is documented but only manually checkable; the Linux
  `host.docker.internal → 10.5.0.1` ModelConfig fix is unchecked.

## 4. External dependencies (beyond the mirror)

- **Accounts:** none required for core. Optional: OpenCode Zen account + API key
  (opencode.ai/auth; signup asks for billing details; free models "for a limited
  time") for module 10 beat 2 — the one documented at-venue network exception.
  Fallback: any personal Anthropic/OpenAI key. GitHub account only for the
  Codespaces lifeboat.
- **Downloads at home:** repo clone from GitHub; mise installer (`curl mise.run`);
  mise tool downloads (aqua/GitHub releases); ~7.5 GB images from 8 registries
  (ghcr.io, registry.k8s.io, quay.io, docker.io, gcr.io, public.ecr.aws,
  xpkg.crossplane.io, docker.gitea.com); Docker Desktop/OrbStack/docker-ce itself.
- **Models:** Ollama (separate install from ollama.com, not mise-managed) +
  `qwen3:4b` pull, host-side, for module 10 beat 1; explicitly does not fit 16 GB
  machines. Beat 2 hosted models: `deepseek-v4-flash-free` / `mimo-v2.5-free` /
  `nemotron-3-ultra-free` (names will drift).
- **Tools assumed but not pinned/installed by dev-setup:** `git`, `curl` (checked by
  verify 00 only), `aws` CLI or `mc` for modules 03/09 host-side S3 (documented
  in-cluster fallback uses the pre-pulled aws-cli image — good), `crossplane` CLI
  (optional, hint-only), `k8sgpt`/`kubectl-ai`/Claude Code (optional, module 05).
- **At-venue network leaks to check:** module 07's documented
  `crane copy docker.io/library/busybox…` (Docker Hub, despite the mirror having the
  image); module 08 going-deeper's
  `crane copy public.ecr.aws/docker/library/golang:1.25-alpine` (golang base is not
  in images.txt at all); module 10 beat 2 (documented exception).

## 5. Rot-prone spots

- **Doc drift already present:** README architecture diagram says "Cilium 1.19";
  versions.env/images.txt pin **1.20.0**. Small but exactly the class of rot QA
  should sweep for.
- **The late-August re-verify debt:** mise.toml, versions.env and images.txt all
  carry "verified 2026-07-13, re-verify late August" — the workshop is Sept 2–3.
  `mise run upstream` / weekly CI is the mitigation; MAINTENANCE.md is the runbook.
- **41 tag-pinned (mutable) image refs** vs 27 digest-pinned; tag re-pushes upstream
  would silently change content (preflight only checks existence).
- **Prerelease/beta components:** RustFS `1.0.0-rc.1` (Docker Hub only, needs the
  vendored log-flood workaround for rustfs/rustfs#5927, rough CVE history —
  acknowledged in-repo); kagent 0.9.11 (upstream "latest release" resolves to an
  unmarked beta, hence the manual pin); Backstage pinned to a CNOE image by commit
  SHA, amd64-only (arch-exempt).
- **First-party images** `ghcr.io/randax/cloudbox-{portal,uploader,resizer,grafana}`
  at v0.1.0, rewritten by release-please between load-bearing annotation comments —
  fragile to careless edits.
- **Upstream URLs that can move/expire:** opencode.ai/auth + /docs/zen (free tier
  explicitly time-boxed — module 10 beat 2 may be dead on workshop day; fallback
  documented), mise.run installer, helm.cilium.io, dl.gitea.com/charts,
  ollama.com, kagent.dev docs, rustfs.com.
- **Planned repo rename** to `jz-2026-platform-engineering` (README says old URL
  will redirect) — every doc link and the clone instructions rot the day it happens.
- **Version-coupling invariants maintained by hand:** mise.toml ↔ versions.env;
  images.txt `[mirror]` ↔ what `gitops/` actually deploys; vendored charts
  (`scripts/manifests/cilium-*.tgz`, `gitea-*.tgz`) ↔ their version variables;
  kubectl ↔ Talos-shipped Kubernetes (1.36.2). check-consistency.sh + CI gates
  cover much of this, but they're the things to re-run in the QA pass.
- **Fragile at-the-time-of-writing facts baked into lab text:** Zen model names,
  aws-cli image tag `2.36.20` in a hint, `qwen3:4b`, ArgoCD ~3 min poll interval,
  "Talos never 1.12.x (talos#12885)".

## 6. Suggested QA walkthrough shape

1. Clean machine (or wiped mirror volume): dev-setup → cloudbox-init → preflight;
   assert each script's own exit code and the preflight counters (3/3 host, 65/65
   mirror, arch match).
2. Per module 01→10 in order: run `solve.sh`, then `verify.sh`, capture both exit
   codes and timings; for 05 and 10 drive `inject.sh` explicitly before solving so
   verify isn't vacuous.
3. Manual/browser passes for the unverifiable list in §3 (portal form, gallery
   upload moment, Grafana trace, Backstage demo, presigned URL in a browser).
4. Offline rehearsal: after init, cut network (or block egress) and re-run modules
   01–09; expect exactly the three leaks in §4 to surface.
5. Rot sweep: `mise run upstream`, `scripts/check-consistency.sh`, and a grep for
   the doc-drift class (README "Cilium 1.19").
