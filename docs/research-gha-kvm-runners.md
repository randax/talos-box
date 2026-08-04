# KVM availability on GitHub Actions runners

Research for [#74](https://github.com/randax/talos-box/issues/74), part of the
[Linux parity wayfinder map (#71)](https://github.com/randax/talos-box/issues/71).
Sources checked 2026-08-04.

## Answer in one paragraph

`/dev/kvm` **is present and hardware acceleration works on standard GitHub-hosted amd64 Linux
runners** (free tier, public repos), but it is undocumented, unsupported, and requires a udev
rule plus installing QEMU yourself — the runner image ships neither `qemu-system-x86` nor
membership in the `kvm` group. On **arm64 runners — standard or larger — KVM is not exposed at
all**, because Azure's Arm VM SKUs do not offer nested virtualization; this is an open, unplanned
request upstream. The practical ceiling on a standard amd64 runner is **4 vCPU / 16 GB RAM /
~14 GB documented SSD**, which fits a *small* multi-node Talos cluster (1 control plane + 2
workers at Talos minimums) but not upstream Talos's own QEMU e2e defaults. Consequently the map's
plan — amd64 e2e cluster-up in CI, arm64 e2e manual — is confirmed correct, and arm64 e2e should
stay off GitHub-hosted runners until Azure ships Arm nested virt.

## 1. amd64 standard runners

### Is `/dev/kvm` there?

Yes, on the free/standard Linux runners, and it has been for a while:

- GitHub's changelog of 2023-02-23,
  [Hardware accelerated Android virtualization on Actions Linux larger hosted runners](https://github.blog/changelog/2023-02-23-hardware-accelerated-android-virtualization-on-actions-windows-and-linux-larger-hosted-runners/),
  first enabled it, but only on **larger** runners (4+ vCPU).
- GitHub's changelog of 2024-04-02,
  [GitHub Actions: Hardware accelerated Android virtualization now available](https://github.blog/changelog/2024-04-02-github-actions-hardware-accelerated-android-virtualization-now-available/),
  extended it to **2-vCPU standard GitHub-hosted Linux runners**. This is the closest thing to an
  official statement that KVM is usable on the free tier.
- Still true in practice as of July 2026: runner-images issue
  [#14482](https://github.com/actions/runner-images/issues/14482) is a bug report from a user
  running Android emulators on `ubuntu-latest` **standard** runners that explicitly rules out
  "KVM fallback" as the cause because boot completed with "no acceleration errors" — i.e. KVM
  acceleration was live on the standard runner pool on both image builds compared.

Caveat on trust level: neither the
[GitHub-hosted runners reference](https://docs.github.com/en/actions/reference/runners/github-hosted-runners)
nor the [larger runners reference](https://docs.github.com/en/actions/reference/runners/larger-runners)
documents KVM or nested virtualization for Linux at all. Both changelogs frame the capability
narrowly as "Android virtualization". There is no SLA; the capability has been observed to come
and go with image/host rollouts (see
[community discussion #8305](https://github.com/orgs/community/discussions/8305)). Treat it as
"works, unsupported".

### Setup steps required

Two things are missing from the image and must be done in the workflow:

**a) Permissions.** The `runner` user is not in the `kvm` group, and `usermod -aG kvm runner`
does not help because group membership needs a new login session, which does not happen between
Actions steps. Both facts are stated by contributors in runner-images
[#8542](https://github.com/actions/runner-images/issues/8542) (closed as duplicate) and
[#7670](https://github.com/actions/runner-images/issues/7670). The sanctioned workaround, given
verbatim in the 2024-04-02 GitHub changelog, is a udev rule:

```yaml
- name: Enable KVM group perms
  run: |
    echo 'KERNEL=="kvm", GROUP="kvm", MODE="0666", OPTIONS+="static_node=kvm"' \
      | sudo tee /etc/udev/rules.d/99-kvm4all.rules
    sudo udevadm control --reload-rules
    sudo udevadm trigger --name-match=kvm
```

The `OPTIONS+="static_node=kvm"` part matters — runner-images
[#8670](https://github.com/actions/runner-images/issues/8670) is a report of the mode being reset
when it is omitted. (An earlier version of the GitHub changelog itself shipped the rule without
it and was corrected after community feedback; noted in
[#7670](https://github.com/actions/runner-images/issues/7670).)

**b) QEMU itself.** The Ubuntu images do not ship it. Grepping the published
[Ubuntu 24.04](https://github.com/actions/runner-images/blob/main/images/ubuntu/Ubuntu2404-Readme.md)
and Ubuntu 22.04 image manifests for `kvm`, `qemu`, or `libvirt` returns nothing — the images'
virtualization story is containers (Docker, Podman, kind, minikube). A runner-images maintainer
declined to preinstall it in
[#7541 "Add qemu-kvm to Ubuntu runners"](https://github.com/actions/runner-images/issues/7541):
*"given that we do not load kvm modules during runners preparation and do not see high demand to
do so we would not like to provide an emu package by default as well. Please continue installing
in runtime."* Likewise [#7670](https://github.com/actions/runner-images/issues/7670) (libvirt) was
closed. So: `sudo apt-get install -y qemu-system-x86` (plus `bridge-utils`, `iptables`,
`cni-plugins` as needed) at job time, costing ~20-60 s.

Note that libvirt/`virsh` users additionally need the `libvirt` group, which has the same
session problem — another argument for talos-box's plan to drive QEMU over QMP directly with no
libvirt.

## 2. arm64 runners

**KVM is not available on any GitHub-hosted arm64 runner tier — standard or larger.**

The decisive primary source is runner-images
[#14062 "Please support KVM on ARM runners"](https://github.com/actions/runner-images/issues/14062)
(opened 2025-12-03, still **open** as of 2026-08-04). The reporter shows
`qemu-system-arm64 -enable-kvm` on `ubuntu-24.04-arm` failing with:

```
qemu-system-arm64: Could not access KVM kernel module: No such file or directory
qemu-system-arm64: failed to initialize kvm: No such file or directory
```

The thread establishes the cause and the outlook:

- "GitHub Actions run on virtualized servers and KVM is not exposed to users… You would need to
  use a self-hosted runner on a bare-metal system to access kvm in your workflows."
- Nested virt on AArch64 is possible in principle since Linux 6.16, "but not yet available in
  Azure. If the feature appears in Azure Arm-based VMs it should be available to Actions runners
  also."

So the blocker is the Azure Arm VM SKU, not the runner image or the Actions product. There is no
roadmap date; a later comment asking whether it is planned is unanswered. The same root cause was
given for x64 back in 2023 ("as current azure linux sku we are using do not support nested
virtualisation", runner-images [#7670](https://github.com/actions/runner-images/issues/7670)) —
that was eventually fixed for x64 by a host SKU change, which is the shape of fix arm64 would
need too.

Larger arm64 runners are the same Azure Arm (Cobalt-class) family, so buying a bigger arm64
runner does not change this. Nothing in the
[larger runners reference](https://docs.github.com/en/actions/reference/runners/larger-runners)
claims otherwise.

**macOS runners are also out**, for a different reason: GitHub documents that on arm64 macOS
runners "Nested-virtualization is not supported due to the limitation of Apple's Virtualization
Framework"
([GitHub-hosted runners reference](https://docs.github.com/en/actions/reference/runners/github-hosted-runners)).
This is the same wall the repo's current `.github/workflows/ci.yml` already notes ("E2E tests
require Virtualization.framework, which is unavailable on GitHub-hosted runners") — so the macOS
job stays build+unit-test only regardless of what happens with Linux.

### Consequence for the map

The map's note — "full e2e cluster-up on amd64 GitHub runners (KVM); arm64 e2e manual until
runner research says otherwise" — is **confirmed**. Runner research says *not otherwise*. Options
for arm64 e2e, in order of cost:

1. Manual / on-demand runs on a developer Apple Silicon or Ampere box (map's current plan).
2. Self-hosted runner on bare-metal arm64 (Hetzner/Ampere/Oracle A1 bare metal, or a Mac mini
   running Linux). Only path to arm64 KVM today, per the maintainer answer above.
3. `qemu-system-aarch64` **without** KVM (TCG emulation) on an amd64 runner. Functionally
   possible, but full-system cross-arch emulation of a booting Talos node is roughly an order of
   magnitude slower; booting three nodes to a healthy Kubernetes cluster within a job timeout is
   not realistic. Useful at most for a single-node smoke test with a generous timeout.

## 3. Resource limits vs. a multi-node Talos cluster

Documented specs
([GitHub-hosted runners reference](https://docs.github.com/en/actions/reference/runners/github-hosted-runners),
[larger runners reference](https://docs.github.com/en/actions/reference/runners/larger-runners)):

| Runner | vCPU | RAM | Disk (documented) |
|---|---|---|---|
| Standard Linux x64, **public** repo | 4 | 16 GB | 14 GB SSD |
| Standard Linux x64, **private** repo | 2 | 8 GB | 14 GB SSD |
| Standard Linux arm64 (public) | 4 | 16 GB | 14 GB SSD — but **no KVM** |
| Larger Linux x64 | 2–96 | 8–384 GB | 75–2040 GB |
| Larger Linux arm64 | 2–64 | 8–208 GB | 75–2040 GB — **no KVM** |

Talos node minimums
([Talos system requirements](https://docs.siderolabs.com/talos/v1.13/getting-started/system-requirements)):

| Role | Memory | Cores | System disk |
|---|---|---|---|
| Control plane | 2 GiB | 2 | 10 GiB |
| Worker | 1 GiB | 1 | 10 GiB |

Recommended is 4 GiB/4 cores/100 GiB for control plane, 2 GiB/2 cores/100 GiB for workers.

For comparison, upstream Talos's own QEMU e2e (`hack/test/e2e-qemu.sh` in `siderolabs/talos`)
defaults to **3 control planes × 4096 MB / 4 vCPU + 2 workers × 2048 MB / 2 vCPU, 15 GB system
disk each** — ~16 GB RAM and 16 vCPU of guests, plus disk. That does not fit a standard runner,
and indeed upstream runs those workflows on `runs-on: {group: large}`, i.e. their own
self-hosted runner group, not GitHub-hosted runners
(`.github/workflows/integration-qemu-triggered.yaml`).

### What actually fits on a 4 vCPU / 16 GB public-repo runner

- **1 control plane (2 vCPU, 2–3 GB) + 2 workers (1 vCPU, 1.5–2 GB each)** — ~4 vCPU, ~7 GB of
  guest RAM, leaving headroom for the host, the QEMU processes, the container image pulls inside
  the guests, and the test harness. This is a real multi-node cluster and is the right target.
- **3 control planes** (HA etcd) is possible on RAM (~9 GB) but oversubscribes 4 vCPU badly;
  etcd is latency-sensitive and will flap. Prefer 1 CP + 2 workers, and leave HA/etcd-quorum
  scenarios to manual or self-hosted runs.
- Private-repo runners (2 vCPU / 8 GB) are too tight for three nodes. If e2e ever needs to run
  from a private fork, budget for a larger runner.

Disk is the sharpest constraint. 14 GB of documented free SSD does not hold three 10 GiB Talos
system disks. Mitigations, in order of preference:

1. **Sparse / qcow2 backing files.** Talos's EPHEMERAL partition grows to fill the disk, but a
   qcow2 or sparse raw file only consumes what is written — a booted Talos node plus a small
   workload is on the order of 1–2 GB actual. Size the virtual disk at 10 GiB (the Talos minimum)
   and let it stay sparse. Do **not** preallocate.
2. **Put VM images on the larger scratch mount.** GitHub runners have substantially more free
   space than the documented 14 GB (a second, larger data volume is mounted, conventionally
   `/mnt`), but the amount is undocumented and has changed across image releases — so probe it in
   the job (`df -h`) and fail loudly rather than assuming.
3. Free space up front by removing unused toolcaches (the well-known "maximize build space"
   pattern) if 1+2 prove insufficient.

### Other CI-relevant requirements

Talos's QEMU provisioner needs, per the
[Talos QEMU docs](https://docs.siderolabs.com/talos/v1.11/platform-specific-installations/local-platforms/qemu):
KVM-enabled QEMU; kernel `CONFIG_NET_SCH_NETEM` and `CONFIG_NET_SCH_INGRESS`; the `bridge`,
`static` and `firewall` CNI plugins plus `tc-redirect-tap`; `iptables`; `/var/run/netns`; and
`CAP_SYS_ADMIN` + `CAP_NET_ADMIN` (in practice, root/sudo). All of these are satisfiable on a
GitHub-hosted Ubuntu runner — `sudo` is unrestricted, the stock Ubuntu kernel has the netem/
ingress schedulers, and the CNI plugins are a download. talos-box's own Linux backend will have a
similar list; the point is that nothing here is an additional CI blocker beyond KVM itself.

Timing expectations: with KVM, a Talos node boots to maintenance mode in seconds and a 3-node
cluster reaches a healthy Kubernetes API in roughly 2–4 minutes plus image-pull time — well
inside a job. The dominant cost is pulling Kubernetes component images inside each guest, so a
registry pull-through cache (which the Talos provisioner supports) or a pre-baked image cache is
worth wiring in early.

## Risks and things to re-check

- **Undocumented capability.** KVM on standard runners is not in GitHub's runner documentation
  and both changelogs scope it to Android emulation. It could regress in an image rollout with no
  notice. Mitigation: make the e2e job assert `test -w /dev/kvm` and `kvm-ok`-style checks as an
  explicit first step so a regression fails fast and legibly rather than silently falling back to
  TCG and timing out.
- **No `kvm` group membership** — the udev rule is load-bearing, and the variant without
  `OPTIONS+="static_node=kvm"` has been observed to get reset. Copy the rule exactly.
- **arm64 has no path on GitHub-hosted runners**, and no announced plan. Re-check runner-images
  [#14062](https://github.com/actions/runner-images/issues/14062) and Azure Arm VM release notes
  before committing to arm64 e2e in CI.
- **Disk is the binding constraint**, not CPU or RAM. Sparse images are mandatory, not an
  optimization.
- **Private-repo runners are half the size** of public-repo runners; a 3-node cluster budget that
  works today would break if the repo went private.

## Sources

- GitHub changelog, [Hardware accelerated Android virtualization on Actions Linux larger hosted runners](https://github.blog/changelog/2023-02-23-hardware-accelerated-android-virtualization-on-actions-windows-and-linux-larger-hosted-runners/) (2023-02-23)
- GitHub changelog, [GitHub Actions: Hardware accelerated Android virtualization now available](https://github.blog/changelog/2024-04-02-github-actions-hardware-accelerated-android-virtualization-now-available/) (2024-04-02)
- GitHub Docs, [GitHub-hosted runners reference](https://docs.github.com/en/actions/reference/runners/github-hosted-runners)
- GitHub Docs, [Larger runners reference](https://docs.github.com/en/actions/reference/runners/larger-runners)
- actions/runner-images [#14062 — Please support KVM on ARM runners](https://github.com/actions/runner-images/issues/14062) (open)
- actions/runner-images [#7541 — Add qemu-kvm to Ubuntu runners](https://github.com/actions/runner-images/issues/7541)
- actions/runner-images [#7670 — Add libvirt to run virtual machines](https://github.com/actions/runner-images/issues/7670)
- actions/runner-images [#8542 — runner user is not in the kvm group](https://github.com/actions/runner-images/issues/8542)
- actions/runner-images [#8670 — Permissions on /dev/kvm set in composite action are reset](https://github.com/actions/runner-images/issues/8670)
- actions/runner-images [#14482](https://github.com/actions/runner-images/issues/14482) (evidence KVM live on standard runners, July 2026)
- actions/runner-images [Ubuntu2404-Readme.md](https://github.com/actions/runner-images/blob/main/images/ubuntu/Ubuntu2404-Readme.md) (no qemu/kvm/libvirt)
- GitHub community discussion [#8305 — Revisiting KVM support for Hosted GitHub Actions](https://github.com/orgs/community/discussions/8305)
- Sidero Labs, [Talos system requirements](https://docs.siderolabs.com/talos/v1.13/getting-started/system-requirements)
- Sidero Labs, [Talos QEMU local platform docs](https://docs.siderolabs.com/talos/v1.11/platform-specific-installations/local-platforms/qemu)
- siderolabs/talos, `hack/test/e2e-qemu.sh` and `.github/workflows/integration-qemu-triggered.yaml` (upstream e2e defaults; `runs-on: {group: large}`)
