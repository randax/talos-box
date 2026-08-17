# Depot CI mechanics (issue #316)

Research date: 2026-08-17. Primary sources only: depot.dev/docs (Depot CI product docs, not the GitHub Actions runners docs), depot.dev changelog/blog, and the `depot` GitHub org (including the doc sources in `depot/docs`). Gaps are marked explicitly as "docs silent — verify empirically".

Companion research on the *GitHub Actions runners* product (a different Depot product): `docs/research/depot-linux-catalog.md` (branch `research/depot-linux-catalog`), `docs/research/depot-kvm.md`, `docs/research/depot-macos.md`. Do not mix the two products' labels/pricing — the same label strings (`depot-ubuntu-24.04*`) exist in both with different specs and prices.

## 1. Workflow definition format

**Verdict: GitHub-Actions-compatible YAML, executed by Depot's own engine, with a documented compatibility matrix and a few Depot-only extensions.**

- "Depot CI executes GitHub Actions YAML workflows." Workflows live in `.depot/workflows/` in the repo; local composite actions go in `.depot/actions/`. Merging workflow files to the default branch registers their triggers automatically. Sources: [Compatibility](https://depot.dev/docs/ci/compatibility), [Quickstart](https://depot.dev/docs/ci/quickstart), [Overview](https://depot.dev/docs/ci/overview).
- `depot ci migrate` copies workflows from `.github/workflows/` to `.depot/workflows/`, rewriting runner labels (`ubuntu-latest`/`ubuntu-24.x` → Depot labels; nonstandard labels → `depot-ubuntu-latest`; expressions left as-is) and annotating changes inline. [Quickstart](https://depot.dev/docs/ci/quickstart).
- **Marketplace actions work.** All three action types are supported: "JavaScript (Node 12/16/20/24 actions)", "Composite", and "Docker" container actions. The reusable-workflows limitation explicitly carves actions out: "You can still use `uses` to reference actions from the GitHub Actions Marketplace (for example, `uses: actions/checkout@v4`)." So `actions/checkout`, `actions/setup-go`, and `golangci/golangci-lint-action` are in-scope by type; none is individually name-checked in the docs beyond `actions/checkout`. [Compatibility](https://depot.dev/docs/ci/compatibility).
- Supported syntax (per the compatibility matrix): workflow/job/step `name`, `run-name`, `on`, `permissions`, `env`, `defaults`, `concurrency` (workflow- and job-level), `needs`/DAG, `if`, `outputs`, `timeout-minutes`, `continue-on-error`, `container`, `services`, `shell` (bash/pwsh/python), `working-directory`; expressions with contexts `github`, `env`, `vars`, `secrets`, `needs`, `strategy`, `matrix`, `steps`, `job`, `runner`, `inputs` and functions `always/success/failure/cancelled/case/hashFiles/contains/startsWith/endsWith/format/join/toJSON/fromJSON`. [Compatibility](https://depot.dev/docs/ci/compatibility).
- Not supported: `runs-on` with non-Depot labels (remapped to `depot-ubuntu-latest`), `jobs.<job_id>.environment` (deployment environments), cross-repository reusable workflows (`jobs.<job_id>.uses` against another repo). Same-repo `workflow_call` reusable workflows (inputs/outputs/secrets, `secrets: inherit`) are supported. [Compatibility](https://depot.dev/docs/ci/compatibility).
- Depot-only extensions: `jobs.<job_id>.snapshot` (custom images built from sandbox snapshots — [Custom images](https://depot.dev/docs/ci/how-to-guides/custom-images)), parallel steps within a job ([Parallel steps](https://depot.dev/docs/ci/how-to-guides/parallel-steps), [GA announcement](https://depot.dev/blog/now-available-depot-ci)), step retries ([Retry steps](https://depot.dev/docs/ci/how-to-guides/retry-steps)), and test splitting ([Split tests](https://depot.dev/docs/ci/how-to-guides/split-tests)).
- GitHub Actions YAML is described as "the first syntax Depot CI supports", implying other syntaxes are planned but none is documented yet. [Overview](https://depot.dev/docs/ci/overview).

## 2. Triggers

All of what we need is supported, including the nightly schedule-only e2e lane:

- ✅ `push` (with `branches`, `tags`, `paths` filters), `pull_request` (with `branches`, `paths`), `pull_request_target`, `pull_request_review`, `merge_group`, `deployment_status`, `workflow_run`, `workflow_call`, `repository_dispatch` (with `types`; delivered against default branch only, `client_payload` available via `github.event`). [Compatibility](https://depot.dev/docs/ci/compatibility).
- ✅ **`on.schedule` (cron) is explicitly listed as supported** — a schedule-only nightly workflow is expressible. [Compatibility](https://depot.dev/docs/ci/compatibility).
- ✅ Manual runs: `on.workflow_dispatch` with `inputs` is supported; dispatch via `depot ci dispatch`, plus local ad-hoc runs of any workflow (including uncommitted changes, auto-patched) with `depot ci run --workflow .depot/workflows/ci.yml [--job <name>]`. Reruns (`depot ci rerun`, full or failed-only) and cancellation at run/workflow/job level via CLI/API/dashboard. [Manage workflow runs](https://depot.dev/docs/ci/how-to-guides/manage-workflow-runs), [CLI reference](https://depot.dev/docs/cli/reference/depot-ci).
- ❌ GitHub-only events: `release`, `issues`, `create`/`delete`, `check_run`/`check_suite`, `discussion*`, `status`, `label`, `milestone`, `page_build`, `registry_package`, `watch`, `fork`, `gollum`, `branch_protection_rule`, `deployment`, `pull_request_comment`, `pull_request_review_comment`. [Compatibility](https://depot.dev/docs/ci/compatibility).
- Bonus: native GitHub stacked-PR support (`github.event.pull_request.stack` context; `branches` filter evaluated against the stack's ultimate base). [Compatibility](https://depot.dev/docs/ci/compatibility).

Cron timezone/granularity semantics: docs silent — verify empirically (assume GitHub-compatible UTC cron until proven).

## 3. GitHub integration (checks, required checks, forks)

- "When Depot CI runs a workflow, it automatically reports a check for each job on the corresponding commit." Checks are **per-job**, named **`Workflow / job-name`** (e.g. `CI / build`), update in real time, show a Depot icon, and the Details link goes to the Depot CI job page. Reruns "update the existing check rather than creating a duplicate." [GitHub checks](https://depot.dev/docs/ci/observability/github-checks), [Compatibility § GitHub checks](https://depot.dev/docs/ci/compatibility).
- **Branch protection / required checks: docs silent — verify empirically.** The docs never state that Depot CI checks can be selected as required status checks. Since they are real check runs created by the Depot Code Access GitHub App, GitHub's branch-protection UI should offer them (required checks are keyed on check-run name + app), but confirm on a test branch rule before relying on it. Note the check-name format differs from GitHub Actions' `workflow / job` naming only in provenance, so migrating required checks means re-selecting them under the Depot app.
- **Fork PRs: not supported today.** "GitHub allows `pull_request` and `pull_request_target` workflows to run when triggered from forked repositories. Support for this is planned." No timeline, no details on how secrets/permissions will be handled for forks. [Compatibility](https://depot.dev/docs/ci/compatibility). (Roadmap beyond "planned": docs silent.)
- Permissions available to `GITHUB_TOKEN`-equivalent app tokens: `actions`, `checks`, `code-quality` (must be requested explicitly; excluded from `read-all`/`write-all`), `contents`, `id-token`, `metadata`, `pull_requests`, `statuses`, `workflows`. **GitHub Packages (GHCR) with the app token does not work** — GitHub's registry only accepts PATs, not GitHub App tokens; Depot suggests Depot Registry instead. [Compatibility](https://depot.dev/docs/ci/compatibility).
- Setup: install the "Depot Code Access" GitHub App from the org settings; during migration "the workflows run in both GitHub and Depot CI" until you delete the `.github/workflows/` copies — plan for a dual-running window. [Quickstart](https://depot.dev/docs/ci/quickstart).
- OIDC for cloud auth is supported; issuer `https://identity.depot.dev`. [OIDC](https://depot.dev/docs/ci/oidc), [depot/skills depot-ci skill](https://github.com/depot/skills/blob/main/skills/depot-ci/SKILL.md).

## 4. Sandboxes

x86_64 only, Ubuntu 24.04 base image, six sizes. Billing is per-second ($0.00005/second/vCPU), no one-minute minimum; plan minutes are denominated in 2-CPU-sandbox minutes.

| Label | Sandbox size | CPUs | Memory | $/second | Plan-minutes multiplier |
|---|---|---|---|---|---|
| `depot-ubuntu-24.04` | `2x8` | 2 | 8 GB | $0.0001 | 1x |
| `depot-ubuntu-24.04-4` | `4x16` | 4 | 16 GB | $0.0002 | 2x |
| `depot-ubuntu-24.04-8` | `8x32` | 8 | 32 GB | $0.0004 | 4x |
| `depot-ubuntu-24.04-16` | `16x64` | 16 | 64 GB | $0.0008 | 8x |
| `depot-ubuntu-24.04-32` | `32x128` | 32 | 128 GB | $0.0016 | 16x |
| `depot-ubuntu-24.04-64` | `64x256` | 64 | 256 GB | $0.0032 | 32x |

Sources: [Overview § Depot CI sandboxes](https://depot.dev/docs/ci/overview#depot-ci-sandboxes), [Pricing](https://depot.dev/pricing).

- **`/dev/kvm`: yes.** "`/dev/kvm` is available in every Depot CI sandbox with no extra configuration" — Android Emulator, QEMU/KVM, VM-based test environments are called out explicitly. This matches our earlier finding in `docs/research/depot-kvm.md` (Depot CI sandboxes have KVM; the GitHub Actions runners product does not). [Overview](https://depot.dev/docs/ci/overview).
- **Disk size: docs silent — verify empirically.** Unlike the GitHub Actions runners docs (100–250 GB per size), the Depot CI sandbox table lists no disk allotment.
- **arm64/macOS/Windows: not available.** "Depot CI doesn't provide sandboxes for Arm, macOS, or Windows. Labels for these runner types aren't compatible with Depot CI." `windows-latest` gets remapped to Ubuntu and may fail. No documented roadmap for arm64/macOS sandboxes (docs silent). [Overview](https://depot.dev/docs/ci/overview), [depot-ci skill](https://github.com/depot/skills/blob/main/skills/depot-ci/SKILL.md).
- Startup: pre-warmed sandboxes, "from commit to a running job in 2-3 seconds". Custom images via `snapshot` (build once, boot pre-provisioned) are supported, as is SSH into a running job and per-job CPU/memory metrics. [Overview](https://depot.dev/docs/ci/overview), [GA changelog](https://depot.dev/changelog/2026-03-24-depot-ci-now-available).
- Plans: Developer $20/mo (2,000 included CI minutes, 25 GB cache), Startup $200/mo (20,000 minutes, 250 GB cache), Business custom; overage billed automatically; cache storage $0.20/GB/month beyond included. Usage tracking on the org usage page began 2026-06-01. [Pricing](https://depot.dev/pricing), [Overview § Pricing](https://depot.dev/docs/ci/overview), [Changelog](https://depot.dev/changelog).

## 5. Caching

- Headline: "Depot Cache is built in with no configuration required." What that concretely covers inside a Depot CI sandbox is only partially documented. [Overview](https://depot.dev/docs/ci/overview).
- **Cache disks (the documented Depot CI mechanism):** a durable filesystem mounted into a job via the [`depot/cache-mount`](https://github.com/depot/cache-mount) action with `name` (org-globally-unique) and `path` (e.g. `/mnt/cache`) inputs. Auto-created on first use; persists across runs; shared across workflows/repos in the org; supports concurrent multi-writer access from parallel jobs. Documented use cases explicitly include Go: "tool caches (GOCACHE, GOMODCACHE, Cargo, Maven, Gradle)", package/build caches, and apt-style package caches fit the same pattern. Security note: any build in the org using the same disk name can read it — no secrets/untrusted output on cache disks. Disks are managed (view/delete) from the Depot CI settings page. [Cache disks](https://depot.dev/docs/ci/how-to-guides/cache-disks).
- **Go via GOCACHEPROG:** Depot Cache supports the Go 1.24+ `GOCACHEPROG` protocol through `depot gocache` (`export GOCACHEPROG="depot gocache"`, needs a Depot API token; `--verbose`, `--organization` flags). Docs describe this for local machines, Depot *GitHub Actions runners* (pre-configured there), and container builds. **Whether Depot CI sandboxes come with GOCACHEPROG pre-configured: docs silent — verify empirically** (fallback is either `depot gocache` manually or GOCACHE/GOMODCACHE on a cache disk). [Go cache integration](https://depot.dev/docs/cache/integrations/gocache), [GOCACHEPROG changelog](https://depot.dev/changelog/2025-03-03-gocacheprog), [Go remote cache blog](https://depot.dev/blog/go-remote-cache).
- **`actions/cache`:** the GitHub-Actions-cache-API interception ("any action that uses the GitHub Actions cache API automatically uses Depot Cache") is documented **only for the Depot GitHub Actions runners product**: "Depot Cache for GitHub Actions is only available when using Depot GitHub Actions runners." No Depot CI doc states that `actions/cache` works in Depot CI sandboxes (and GHCR-style `GITHUB_TOKEN` caveats may apply). **Docs silent — verify empirically**; the supported `hashFiles()` function suggests intent, and cache disks are the documented substitute either way. [GitHub Actions cache integration](https://depot.dev/docs/cache/integrations/github-actions).
- **apt caching:** no dedicated mechanism documented — docs silent; a cache disk mounted over an apt cache dir, or a custom/snapshot image with packages pre-installed (the docs' recommended pattern: "jobs start with everything pre-installed"), are the available options. [Cache disks](https://depot.dev/docs/ci/how-to-guides/cache-disks), [Custom images](https://depot.dev/docs/ci/how-to-guides/custom-images).

## 6. Matrix, parallelism, concurrency

- `strategy.matrix` fully supported, including `fail-fast` and `max-parallel`. [Compatibility](https://depot.dev/docs/ci/compatibility).
- `concurrency` groups supported at workflow and job level (cancel-in-progress semantics not spelled out — assume GitHub-compatible, verify). [Compatibility](https://depot.dev/docs/ci/compatibility).
- Depot-only intra-job parallelism: "Parallel steps: run steps concurrently within a single job" ([GA changelog](https://depot.dev/changelog/2026-03-24-depot-ci-now-available), [Parallel steps guide](https://depot.dev/docs/ci/how-to-guides/parallel-steps)); per the [depot-ci skill](https://github.com/depot/skills/blob/main/skills/depot-ci/SKILL.md), `parallel:` blocks cannot be nested and step IDs must be unique job-wide.
- **Org-level concurrent-job limits: docs silent — verify empirically.** Neither the overview, compatibility page, nor pricing page documents a cap on simultaneous jobs/sandboxes; billing language ("included plan minutes aren't a hard cap") implies elasticity but is not a concurrency statement.

## 7. Implications for talos-box migration (summary)

- Existing workflows should port near-verbatim: checkout/setup-go/golangci-lint actions work, cron schedules work, `workflow_dispatch` works, matrix works. Main rewrites: `runs-on` labels, replacing `actions/cache` with cache disks (or empirically verifying actions/cache), and dropping any fork-PR expectations.
- KVM in every sandbox removes the biggest blocker for our QEMU-based e2e lane (contrast with the Actions-runners product, see `docs/research/depot-kvm.md`).
- Verify-empirically checklist before committing: (1) Depot checks selectable as branch-protection required checks; (2) `actions/cache` behavior in sandboxes; (3) GOCACHEPROG preconfiguration; (4) sandbox disk size; (5) cron timezone semantics; (6) org concurrency limits.
