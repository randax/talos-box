# Research: Depot.dev macOS runners for GitHub Actions

Issue: #257. Researched 2026-08-17 against Depot primary sources (docs, changelog, blog).

## Question

Can the CI `build` job (currently `runs-on: macos-15`, running `make build`,
golangci-lint v2.12 via golangci-lint-action, and `make test` with Go 1.26)
move to a Depot macOS runner unchanged?

## Verdict

**Yes — swap `runs-on: macos-15` for `runs-on: depot-macos-15` and the job
should run unchanged.** Depot's macOS images are built to track GitHub's own
`actions/runner-images`, so setup-go, golangci-lint-action, make, Homebrew,
etc. are all present/compatible. See caveats on capacity and pricing below.

## Runner types, hardware, and pricing

Source: <https://depot.dev/docs/github-actions/runner-types> (macOS section).

| Label | Chip | CPUs | RAM | Disk | OS | Price |
|---|---|---|---|---|---|---|
| `depot-macos-26` | Apple M4 | 8 | 24 GB | 400 GB (+ disk accelerator) | macOS 26 (Tahoe) | $0.08/min |
| `depot-macos-15` | Apple M2 | 8 | 24 GB | 400 GB (+ disk accelerator) | macOS 15 (Sequoia) | $0.08/min |
| `depot-macos-14` | Apple M2 | 8 | 24 GB | 400 GB (+ disk accelerator) | macOS 14 (Sonoma) | $0.08/min |

- `depot-macos-latest` currently points to `depot-macos-26` (same runner-types
  doc). During the macOS 26 beta it pointed to macOS 15
  (<https://depot.dev/changelog/2026-05-26-macos-26-beta-github-actions>), so
  the alias moves over time — pin `depot-macos-15` for parity with today's
  `macos-15`.
- All macOS runners include a memory-backed "disk accelerator" for faster I/O
  (runner-types doc).
- Billing is "billed per minute, tracked per second" (no per-minute rounding)
  — <https://depot.dev/pricing>; the launch post also states no per-minute
  rounding (<https://depot.dev/blog/mac-github-actions-runners>).
- Depot markets these as half the cost of GitHub-hosted macOS runners
  (<https://depot.dev/blog/mac-github-actions-runners>). Note: GitHub's own
  macOS per-minute price changed in Jan 2026, so re-check the current delta
  for private-repo billing. For **public** repos, GitHub-hosted `macos-15` is
  free, so Depot is a cost *increase* there — talos-box is public, so this
  matters unless the motivation is speed/queue time rather than cost.

## Image contents / job compatibility

Source: <https://depot.dev/docs/github-actions/runner-types>.

- Depot documents that its macOS images track GitHub's official runner
  images: "We do our best to keep our images in sync with GitHub's, but there
  may be a slight delay between when GitHub updates their images and when we
  update ours." The doc links each label to the corresponding
  `actions/runner-images` manifest, e.g. `depot-macos-15` →
  <https://github.com/actions/runner-images/blob/main/images/macos/macos-15-Readme.md>.
- Consequences for our job:
  - **setup-go / Go 1.26**: works — `actions/setup-go` downloads its own
    toolchain and the base image is the same family as GitHub's; the manifest
    also ships preinstalled Go.
  - **golangci-lint-action v8 / v2.12**: works — it is a plain composite
    action with no runner-specific requirements.
  - **make / clang / CLT / Homebrew**: present, same as `macos-15` (per the
    linked runner-images manifest).
  - **Architecture**: `depot-macos-15` is Apple Silicon (M2, arm64) — same as
    GitHub's `macos-15`, which is also arm64. No arch change.
- Virtualization.framework availability is irrelevant here: e2e is already
  excluded on macOS in CI (`.github/workflows/ci.yml` comment).

## macOS version implications

- `depot-macos-15` matches `macos-15` exactly (same Sequoia base, same
  runner-images manifest lineage) → no Xcode/SDK drift beyond Depot's "slight
  delay" syncing images.
- Moving to `depot-macos-26` instead would mean macOS 26 (Tahoe) on M4 with
  Xcode 26 preinstalled
  (<https://depot.dev/blog/now-available-macos-26-github-actions>). For a Go
  project with no Xcode dependency this is low-risk, but it is a newer SDK
  surface than what we test today.

## Caveats

- **Capacity is not elastic**: "due to licensing constraints from Apple, our
  macOS runner capacity is not fully elastic like our other runner types...
  macOS jobs can experience longer queue times during times of high demand"
  (<https://depot.dev/docs/github-actions/runner-types>).
- **Plan gating (gap)**: the 2024 launch post said macOS runners required the
  Startup plan or above and were beta
  (<https://depot.dev/blog/mac-github-actions-runners>); the current pricing
  page's plan matrix was not conclusive from this research pass. Verify which
  plan tiers include macOS runners before migrating.
- **Included minutes (gap)**: plan-included GitHub Actions minutes on
  <https://depot.dev/pricing> are documented for Ubuntu runners; whether/how
  macOS minutes draw from an included allowance was not clearly documented.
  Confirm with Depot.
- Setup itself is not zero-config: the Depot GitHub app must be installed and
  the org connected before `depot-*` labels resolve
  (<https://depot.dev/docs/github-actions/overview>) — the *job definition*
  is unchanged, but the org needs one-time onboarding.
