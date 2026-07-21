# Research: Talos on Hyper-V (Windows 11, Gen2) — wayfinder #49

Part of #48. Question: does current Talos (v1.13.x) boot and run correctly under Hyper-V
on Windows 11 x64, and what are the mechanics (Gen2, Secure Boot, boot artifacts, serial
console, synthetic devices, known issues)?

## Verdict

**Yes, Talos boots and runs on Hyper-V Gen2 on Windows 11, and it is an officially
documented platform** — but with two load-bearing caveats: **Secure Boot must be turned
off** (Talos's self-signed boot chain doesn't match any of Hyper-V's three hardcoded
Secure Boot templates), and there is **no dedicated Hyper-V image artifact** — you either
boot the generic `metal-amd64.iso` (the path Sidero's own docs use) or convert the
`metal-amd64.raw` disk image to VHDX yourself with `qemu-img`. Serial console over a
Hyper-V COM port/named pipe works but is **not on by default in Talos ≥1.8** (`console=ttyS0`
was dropped from the metal images in v1.8), so — analogous to the vz `hvc0` case in
SPEC §4 — getting kernel/console output out of the named pipe requires adding
`extraKernelArgs` (e.g. `console=ttyS0,115200n8` or `console=tty0 console=ttyS0`) back in
via Image Factory/Imager or machine config. Core synthetic devices (network via
`hv_netvsc`, disk via `hv_storvsc`, balloon via `hv_balloon`) are mainline-kernel drivers
and work out of the box; the optional Hyper-V "integration services" experience (guest IP
shown in Hyper-V Manager, VSS-consistent checkpoints, clean shutdown on host-initiated
power-off) is materially incomplete without extra work, and Talos ships an official
`hyperv-guest-agent` system extension for part of that gap. Host-initiated graceful
shutdown is a known, currently-unresolved issue as of mid-2026.

## Facts (with sources)

### Official support status

- Hyper-V has its own page in the official Talos documentation, grouped under
  "Virtualized Platforms" alongside KVM, Proxmox, VMware, Xen, etc. — the same tier as
  other first-party-documented hypervisors, with no "community-only" or unsupported
  disclaimer on the page.
  Source: [Hyper-V — Talos v1.13 docs](https://docs.siderolabs.com/talos/v1.13/platform-specific-installations/virtualized-platforms/hyper-v),
  [docs sidebar config (talos-v1.13.yaml)](https://github.com/siderolabs/docs/blob/main/talos-v1.13.yaml)
  listing `virtualized-platforms/hyper-v` under "Virtualized Platforms".
- Conversely, Microsoft's own "Supported Linux and FreeBSD virtual machines for Hyper-V"
  page and the "Should I create a generation 1 or 2 VM" guest-OS support tables **do not
  list Talos** at all (they enumerate Ubuntu, RHEL/CentOS, SUSE, Debian, Oracle Linux,
  FreeBSD). A Talos maintainer (smira) and community members confirm Talos is simply not
  a Microsoft-recognized/certified distro, though the needed kernel drivers are upstream.
  Source: [Supported Linux and FreeBSD VMs for Hyper-V — Microsoft Learn](https://learn.microsoft.com/en-us/windows-server/virtualization/hyper-v/supported-linux-and-freebsd-virtual-machines-for-hyper-v-on-windows),
  [Should I create a generation 1 or 2 VM — Microsoft Learn](https://learn.microsoft.com/en-us/windows-server/virtualization/hyper-v/plan/should-i-create-a-generation-1-or-2-virtual-machine-in-hyper-v),
  [siderolabs/talos discussion #12721](https://github.com/siderolabs/talos/discussions/12721)
  (brantgurga: "Talos is not a supported Linux distribution for Hyper-V").
- The Sidero Hyper-V guide's install flow (verified on v1.9, v1.10, v1.13 — content is
  effectively unchanged across versions): download `metal-amd64.iso`, install the
  `New-TalosVM.psm1` PowerShell module, create an **External** virtual switch named
  `LAB`, then run `New-TalosVM -VMNamePrefix ... -CPUCount ... -StartupMemory ...
  -SwitchName LAB -TalosISOPath ... -VMDestinationBasePath ...`, generate/apply configs
  with `talosctl`, bootstrap, and — critically — **remove the ISO from the VM after a
  successful bootstrap, or Talos may fail to boot on subsequent restarts**.
  Sources: [Hyper-V — Talos v1.13 docs](https://docs.siderolabs.com/talos/v1.13/platform-specific-installations/virtualized-platforms/hyper-v),
  [Hyper-V — Talos v1.10 docs](https://docs.siderolabs.com/talos/v1.10/platform-specific-installations/virtualized-platforms/hyper-v),
  [Hyper-V — Talos v1.9 docs](https://docs.siderolabs.com/talos/v1.9/platform-specific-installations/virtualized-platforms/hyper-v).
- A 2023 doc-improvement issue pointed out the guide omits the virtual-switch creation
  detail and the PowerShell execution-policy relaxation step; it was closed as
  stale/not-planned and never merged into the docs, so the official guide still has these
  gaps as of today.
  Source: [siderolabs/talos issue #9290](https://github.com/siderolabs/talos/issues/9290).

### Gen1 vs Gen2, and Secure Boot

- Per Microsoft, Gen2 is UEFI-only, boots the OS disk from a **SCSI** controller (no IDE
  controller exists in Gen2), and is the recommended generation unless you have an
  existing non-UEFI-compatible VHD, need an OS Gen2 doesn't support, or need a Gen2-only
  boot method. Talos's disk images are GPT/UEFI already, so Gen2 is the natural choice
  (and is what this ticket is scoped to).
  Source: [Should I create a generation 1 or 2 VM — Microsoft Learn](https://learn.microsoft.com/en-us/windows-server/virtualization/hyper-v/plan/should-i-create-a-generation-1-or-2-virtual-machine-in-hyper-v)
  (device-support and boot-method comparison tables).
- **Secure Boot is enabled by default on Gen2 VMs.** Hyper-V exposes exactly three
  hardcoded Secure Boot template options: Windows, "Microsoft UEFI Certificate Authority"
  (covers a short enumerated list of Linux distros: Ubuntu ≥14.04, SLES ≥12, RHEL/CentOS
  ≥7.0 — not Talos), and Shielded VM. There is **no way to enroll custom Secure Boot
  keys** through Hyper-V's management surface.
  Source: [Hyper-V feature compatibility by generation and guest — Microsoft Learn](https://learn.microsoft.com/en-us/windows-server/virtualization/hyper-v/Hyper-V-feature-compatibility-by-generation-and-guest)
  (Security table: "Secure boot | 2 | Linux: Ubuntu 14.04+, SLES 12+, RHEL/CentOS 7.0+...").
- Talos does support UEFI SecureBoot generally (since v1.5) by self-signing its
  bootloader/kernel and enrolling its own keys into the platform's key database — but
  that requires a platform that allows custom key enrollment (real hardware, QEMU/OVMF,
  etc.). Hyper-V doesn't allow this, so **Talos + Hyper-V Secure Boot is a hard
  incompatibility today, not a configuration gap**: a Talos maintainer (smira) confirmed
  in the discussion thread that "SecureBoot is not a feature of Talos itself" (it's the
  firmware's job), and the practical guidance from the community is to disable Secure
  Boot for Talos Gen2 VMs via `Set-VMFirmware -VMName <name> -EnableSecureBoot Off`.
  Sources: [SecureBoot — Talos docs](https://www.talos.dev/v1.5/talos-guides/install/bare-metal-platforms/secureboot/),
  [siderolabs/talos discussion #12279 "Hyper-V SecureBoot with Talos"](https://github.com/siderolabs/talos/discussions/12279).

### Boot artifacts: ISO / raw / VHDX

- There is **no Hyper-V-specific output format** in Sidero's Image Factory or in Talos's
  own imager — checked the `siderolabs/image-factory` and `siderolabs/talos` source and
  found no `hyper-v`/`vhd` platform target (only formats like raw, ISO, QCOW2/VMDK-style
  cloud images, etc., keyed to platforms like `metal`, `aws`, `gcp`, `nocloud`). The
  official Sidero guide simply boots the platform-agnostic `metal-amd64.iso` from GitHub
  Releases as a Gen2 virtual DVD and lets Talos install itself to the Gen2 VM's SCSI
  disk — no VHDX conversion needed for that path.
  Source: [Hyper-V — Talos v1.13 docs](https://docs.siderolabs.com/talos/v1.13/platform-specific-installations/virtualized-platforms/hyper-v)
  (uses `metal-amd64.iso` directly); no hits searching `siderolabs/image-factory` or
  `siderolabs/talos` source for Hyper-V/VHD platform targets.
- A separate, community-documented path (not in Sidero's own docs) converts the `metal`
  raw disk image straight to VHDX and attaches it as the Gen2 VM's boot disk, skipping
  the ISO-install step entirely: `qemu-img convert -f raw -O vhdx metal-amd64.raw
  talos.vhdx`. This is consistent with the "raw disk image → VHDX" pattern used for other
  hypervisors and is the mechanism a repo like this one would need to replicate the
  macOS/vz "convert Image Factory raw output" flow for Hyper-V.
  Source: community write-up on converting the Image Factory raw output for Hyper-V
  (`qemu-img convert -f raw -O vhdx ...`) — treat as **inference-adjacent / secondary
  source**, not verified against a Sidero-maintained document; flagged as an open
  question below.

### Serial console: COM port / named pipe, and whether extraKernelArgs are needed

- **Gen2 VMs have no COM port by default** — it must be explicitly added via PowerShell.
  Microsoft's documented mechanism: `Set-VMComPort -VMName <name> -Number 1 -Path
  '\\.\pipe\<pipe-name>'` maps COM1 to a local named pipe that a serial client (e.g.
  PuTTY, `socat`, kdnet) can attach to. Configured COM ports are not shown in Hyper-V
  Manager's VM settings UI — PowerShell is required to inspect/manage them.
  Source: [Should I create a generation 1 or 2 VM — Microsoft Learn, "Add a COM port for
  kernel debugging"](https://learn.microsoft.com/en-us/windows-server/virtualization/hyper-v/plan/should-i-create-a-generation-1-or-2-virtual-machine-in-hyper-v),
  [Set-VMComPort — Microsoft Learn](https://learn.microsoft.com/en-us/powershell/module/hyper-v/set-vmcomport).
- **This is the direct analogue of SPEC §4's `console=hvc0` requirement, and it is
  load-bearing for the same reason.** As of **Talos v1.8**, `console=ttyS0` was removed
  from the default kernel cmdline baked into the metal images/installer (verified
  directly against Talos v1.8.0's own release notes): *"Starting with Talos 1.8,
  `console=ttyS0` kernel argument is removed from the metal images and installer... This
  should fix slow boot or no console output issues on most bare metal hardware."* The
  release notes explicitly call out that virtualized users (their example: QEMU/Proxmox)
  who want serial console output need to **add it back as an extra kernel argument via
  Image Factory or Imager**. Hyper-V is architecturally the same case: to get anything
  out of the COM1 named pipe wired up above, v1.13.x needs
  `extraKernelArgs: ["console=ttyS0,115200n8"]` (or `console=tty0 console=ttyS0` if you
  also want the display-adapter/EFI console) added at image-build or machine-config time
  — it will not "just work" the way it might have pre-1.8.
  Sources: [Talos v1.8.0 release notes](https://github.com/siderolabs/talos/releases/tag/v1.8.0)
  (verbatim quote above), [Modify kernel arguments — Sidero/Omni docs](https://docs.siderolabs.com/omni/infrastructure-and-extensions/modify-kernel-arguments)
  (shows `console=ttyS0,115200n8` as the canonical worked example for adding a serial
  console kernel arg).
- Unlike vz's `hvc0` case, there's no evidence of a "single arg alone bricks the boot"
  failure mode for Hyper-V's `ttyS0` — no GitHub issue found describing that. The one
  related bare-metal issue (`console=ttyS0` hanging boot on certain physical Hetzner
  servers lacking a real serial port) is explicitly bare-metal/hardware-specific, not a
  Hyper-V/virtualized-console problem, and is a different failure mode (hang, not
  silent-no-output).
  Source: [siderolabs/talos issue #7883](https://github.com/siderolabs/talos/issues/7883).

### Synthetic device support (network, disk, balloon)

- Gen2 VMs only expose **synthetic** devices (no IDE, no legacy NIC, no emulated PS/2 —
  see Microsoft's Gen1→Gen2 device-replacement table). The relevant Linux drivers
  (`hv_vmbus`, `hv_netvsc` for network, `hv_storvsc` for the synthetic SCSI controller,
  `hv_utils` for the base host-integration channel, `hv_balloon` for Dynamic
  Memory/ballooning) are all mainline-kernel drivers, so a stock upstream-kernel distro
  like Talos gets them "for free" without needing Hyper-V's separately-downloadable LIS
  package (that download path is only for older/vendor kernels that predate LIS being
  merged upstream).
  Sources: [Hyper-V feature compatibility by generation and guest](https://learn.microsoft.com/en-us/windows-server/virtualization/hyper-v/Hyper-V-feature-compatibility-by-generation-and-guest)
  (device tables), [Supported Linux and FreeBSD VMs for Hyper-V](https://learn.microsoft.com/en-us/windows-server/virtualization/hyper-v/supported-linux-and-freebsd-virtual-machines-for-hyper-v-on-windows)
  ("LIS has been added to the Linux kernel... For other Linux distributions LIS changes
  are regularly integrated into the operating system kernel...no separate download or
  installation is required"), community confirmation in
  [siderolabs/talos discussion #12721](https://github.com/siderolabs/talos/discussions/12721)
  that `hv_balloon` is observed loading in the Talos boot log.
- What does **not** come for free is the userspace half of Hyper-V's "Integration
  Services" experience — guest IP/hostname surfaced in Hyper-V Manager
  (`Get-VMNetworkAdapter`), VSS-consistent checkpoints, DNS/DHCP status reporting. That
  needs `hv_kvp_daemon` / `hv_vss_daemon`, which Talos does **not** ship in the base
  image (immutable OS, no package manager). Sidero maintains an official system
  extension, **`hyperv-guest-agent`** (`ghcr.io/siderolabs/hyperv-guest-agent`), that runs
  these as Talos extension services (`ext-hyperv-kvp`, `ext-hyperv-vss`). Verified
  directly by reading the extension's README:
  - It explicitly documents that Talos already ships the kernel-side drivers
    (`hv_vmbus`, `hv_utils`); the extension only adds the missing daemons.
  - Requires the corresponding Hyper-V integration services to be turned on host-side:
    `Enable-VMIntegrationService -VMName '<vm>' -Name 'Key-Value Pair Exchange'` /
    `'VSS'`.
  - Known limitations it documents itself: DNS/DHCP reporting fields stay empty (the
    distro helper scripts `hv_get_dns_info`/`hv_get_dhcp_info` aren't shipped); **host →
    guest static-IP injection via KVP is not supported** (Talos networking is managed via
    machine config instead); VSS only freezes the ephemeral `/var` partition (the
    read-only squashfs root needs no freeze) — validate checkpoint consistency for your
    workload before relying on it.
  Source: [siderolabs/extensions — guest-agents/hyperv-guest-agent README](https://github.com/siderolabs/extensions/tree/main/guest-agents/hyperv-guest-agent)
  (fetched directly; extension confirmed present in repo tree alongside `qemu-guest-agent`
  and `metal-agent`).
- Dynamic Memory (ballooning) is listed by Microsoft as supported "for specific versions
  of supported guests" — since Talos isn't in Microsoft's guest support matrix at all,
  whether Dynamic Memory is validated/safe for Talos specifically is **not verified**
  either way by a primary source (flagged as an open question below); the kernel module
  loading is not the same as Microsoft having validated the ballooning behavior for this
  guest.

### Known issues

- **Hyper-V graceful shutdown is currently broken/unresolved.** A user reports (June
  2026) that clicking Shutdown in Hyper-V Manager causes Talos to log that `hv_utils`
  received a shutdown-initiated signal, but the VM never actually powers off. Maintainer
  smira: *"It's hard to say - we don't have enough debugging in this version. Can you try
  this with 1.14.0-alpha.2 once it drops... You can always shutdown via Talos API though,
  this will work correctly."* No root cause or fix has landed as of this writing — it's
  an open/unresolved discussion, not a closed bug.
  Source: [siderolabs/talos discussion #13520](https://github.com/siderolabs/talos/discussions/13520)
  (fetched verbatim via GitHub GraphQL API; thread created 2026-06-04).
- **Guest IP is not visible to the Hyper-V host / PowerShell automation out of the box.**
  Multiple independent reports of the same underlying gap: without the KVP daemon, tools
  like `Get-VMNetworkAdapter` can't see the guest's IP, which breaks IP-address-based
  automation (Packer/Vagrant/Terraform workflows that poll the hypervisor for the guest
  IP rather than DHCP-lease introspection). Maintainer guidance was to bundle the needed
  driver/daemon as a system extension baked into the install image to avoid a
  chicken-and-egg bootstrapping problem — this is exactly what `hyperv-guest-agent` (see
  above) now provides.
  Source: [siderolabs/talos discussion #6018](https://github.com/siderolabs/talos/discussions/6018).
  Note this specific discussion predates the extension's existence (originally reported
  against Talos v1.1); the extension is the current, maintained answer to the same
  complaint.
- **Official docs have known, unaddressed gaps**: missing virtual-switch setup and
  PowerShell execution-policy steps (issue #9290, closed stale/not-planned, never fixed);
  no mention anywhere in the official guide of Gen1-vs-Gen2, Secure Boot, serial console,
  or synthetic-device caveats — all of the above had to be sourced from
  discussions/issues/Microsoft docs rather than the primary how-to guide itself.

## Open questions

- Whether converting `metal-amd64.raw` → VHDX via `qemu-img` and booting it directly on
  a Gen2 VM (skipping the ISO-install step) actually works end-to-end on current
  v1.13.x — this repo's use case (Image Factory raw output → hypervisor artifact,
  matching the macOS/vz pattern) is not the path Sidero's own docs exercise or verify;
  it's inferred from the general "raw disk images are the same content as what an
  install produces" pattern used for cloud-platform images, not confirmed against a
  Sidero-maintained source for Hyper-V specifically. Recommend a hands-on validation
  spike before relying on it.
- Whether `console=ttyS0` alone (without `console=tty0`) is safe under Hyper-V Gen2, or
  whether Hyper-V has an equivalent "single console arg bricks boot" failure mode the way
  vz's `hvc0`-alone does (SPEC §4). No such report was found in Talos issues/discussions,
  but absence of a bug report is not the same as a verified "works with just one console
  arg" — worth confirming empirically alongside the VHDX-boot spike above.
- Whether the `hyperv-guest-agent` extension resolves or is even related to the
  host-initiated-shutdown bug (#13520) — the extension's README doesn't mention shutdown
  handling at all (only KVP/VSS), so this is likely a separate, still-open gap in the
  kernel-side `hv_utils` shutdown-notifier integration with Talos's `machined` (PID 1) —
  this is inference, not a confirmed root cause; no maintainer statement ties the two
  together.
- Whether Talos's Dynamic Memory / ballooning behavior under Hyper-V has been validated
  by Sidero for production use (vs. just "the module loads") — no primary source
  confirms or denies this either way for Talos specifically.
- Whether newer Talos releases (post-#13520, e.g. 1.14.x) have since fixed the shutdown
  issue — worth a follow-up check once 1.14 stabilizes, since this ticket is scoped to
  v1.13.x where the issue is confirmed still open.
