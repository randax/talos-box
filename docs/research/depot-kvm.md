# Research: KVM on Depot.dev runners (issue #256)

Date: 2026-08-17. Method: Depot's official docs, blog, and Depot GitHub org only. Claims are cited; where Depot's docs are silent the gap is called out explicitly as "must verify empirically".

## Verdict

**Depot's GitHub Actions runners do NOT expose `/dev/kvm` — on any label, size, or generation.** Depot's own troubleshooting doc is unambiguous:

> "Depot GitHub Actions runners don't currently provide `/dev/kvm`. Nested virtualization support is available only in Depot CI sandboxes, where `/dev/kvm` is enabled by default with no extra configuration."
> — <https://depot.dev/docs/github-actions/troubleshooting>

The doc explicitly lists the failure modes we would hit (`qemu-system-x86_64: failed to initialize kvm: No such file or directory`, `Could not access KVM kernel module`) and names "QEMU/KVM workloads, VM-based end-to-end test environments" as the affected class, recommending migration of such jobs to **Depot CI** (a separate product, see below).

So for talos-box's QEMU e2e lanes:

- Simply swapping `runs-on: ubuntu-latest` → `runs-on: depot-ubuntu-24.04[-N]` **breaks KVM entirely** — worse than GitHub-hosted `ubuntu-latest`, which does provide `/dev/kvm` on x64 runners.
- There is no special label or config flag to enable KVM on Depot GitHub Actions runners. The runner-types doc (<https://depot.dev/docs/github-actions/runner-types>) lists labels of the form `depot-{os}-{version}[-{arch}][-{size}]` (e.g. `depot-ubuntu-24.04-8`, up to 64 vCPU/256 GB) and mentions no virtualization option.
- Depot's `depot-github-runners` agent skill repeats the same guidance: "Runners don't provide `/dev/kvm`; move KVM/QEMU/Android-emulator jobs to Depot CI" (<https://github.com/depot/skills/blob/main/skills/depot-github-runners/SKILL.md>).

## The KVM path Depot does offer: Depot CI sandboxes

Depot CI is **not** a GitHub Actions runner replacement via `runs-on`; it is a separate CI product that executes GitHub-Actions-syntax workflows placed under `.depot/workflows/` on Depot's own compute (<https://depot.dev/docs/ci/quickstart>).

- **KVM by default:** "Nested virtualization is enabled by default on every Depot CI sandbox, with no extra configuration and no extra cost" — announced in <https://depot.dev/blog/now-available-nested-virtualization-on-depot-ci>. The CI overview confirms "`/dev/kvm` is available in every Depot CI sandbox with no extra configuration" (<https://depot.dev/docs/ci/overview>).
- **udev-rule dance:** Depot documents "no extra configuration" for `/dev/kvm` in CI sandboxes. Whether the device node is world-writable or already group-accessible to the job user is **not documented** — must verify empirically (a `ls -l /dev/kvm` + `[ -w /dev/kvm ]` probe step). Keeping the existing best-effort udev/chmod step is harmless either way.
- **Sandbox labels/sizes:** x86_64 only, `depot-ubuntu-24.04` (2 vCPU/8 GB) through `depot-ubuntu-24.04-64` (64 vCPU/256 GB); default image is Ubuntu 24.04; Depot recommends ≥4 vCPU for emulator-class workloads (<https://depot.dev/docs/ci/overview>, blog post above).
- **Workflow compatibility caveats** (<https://depot.dev/docs/ci/compatibility>): most triggers, matrix, services, composite/Docker actions, and marketplace actions (`actions/checkout@v4` etc.) work; unsupported: cross-repo reusable workflows, deployment environments, fork-PR workflows (planned), `GITHUB_TOKEN` auth to GitHub Packages, and ~20 GitHub-specific triggers. Non-Depot `runs-on` labels are coerced to `depot-ubuntu-latest`.

## Environment differences that matter for talos-box networking

These are the bridged-QEMU-networking concerns (default FORWARD policy, whether egress from `br-tbx` is filtered, apt package availability). **Depot's docs are silent on all of them**, for both runner flavors:

- **iptables/nftables FORWARD policy:** Not documented anywhere in Depot's docs, changelog, or skills repo. Must verify empirically on a sandbox (`nft list ruleset` / `iptables -S FORWARD`).
- **Bridge egress filtering / NAT:** Not documented. Depot CI sandboxes are themselves VMs (hence "nested" virtualization), so the network path outside the sandbox is opaque; in-sandbox bridge + NAT rules must be probed empirically.
- **apt packages (`qemu-system-x86 qemu-utils ovmf socat nfs-kernel-server`):** Depot CI's default image is Ubuntu 24.04 and its docs describe building custom images by running normal setup steps on the base image (<https://depot.dev/docs/ci/how-to-guides/custom-images>), which implies working root/apt, but package installability is not explicitly documented — verify with a probe job. For GitHub Actions runners, Depot states it keeps images "in sync with GitHub's" runner images with possible slight delay (<https://depot.dev/docs/github-actions/runner-types>), so apt behavior there should match `ubuntu-latest`.
- A Depot CI **custom image** (snapshot of a sandbox after setup steps, pushed to the Depot registry) could pre-bake the QEMU/OVMF toolchain and Talos image cache to cut lane setup time.

## Recommendation

1. Do **not** move the QEMU e2e lanes to `depot-ubuntu-*` GitHub Actions labels — they lose `/dev/kvm`.
2. If Depot is attractive (bigger machines, faster cache), the e2e lanes would have to move to **Depot CI** (`.depot/workflows/`), which is a product migration, not a label swap — and its fork-PR limitation matters for an OSS repo.
3. Before any migration, run a one-off probe workflow on a Depot CI sandbox capturing: `ls -l /dev/kvm`, writability as the job user, `iptables -S`/`nft list ruleset`, bridge-egress reachability, and `apt-get install` of the five packages. All four networking/permission questions above are undocumented and empirical.

## Sources

- <https://depot.dev/docs/github-actions/troubleshooting> — no `/dev/kvm` on Depot GHA runners; KVM error catalog; Depot CI pointer
- <https://depot.dev/docs/github-actions/runner-types> — runner labels/sizes; image parity with GitHub's
- <https://depot.dev/blog/now-available-nested-virtualization-on-depot-ci> — nested virt on by default in every Depot CI sandbox; ≥4 vCPU guidance; `depot-ubuntu-24.04-4` example
- <https://depot.dev/docs/ci/overview> — sandbox sizes, Ubuntu 24.04 base, `/dev/kvm` in every sandbox
- <https://depot.dev/docs/ci/quickstart> — Depot CI is a separate product; `.depot/workflows/`
- <https://depot.dev/docs/ci/compatibility> — supported/unsupported GHA features; label coercion
- <https://depot.dev/docs/ci/how-to-guides/custom-images> — snapshot-based custom sandbox images
- <https://github.com/depot/skills/blob/main/skills/depot-github-runners/SKILL.md> — Depot's own agent guidance repeating the no-KVM rule; Windows Hyper-V limitation
