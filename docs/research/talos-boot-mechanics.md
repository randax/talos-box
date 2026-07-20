# Talos Linux (arm64) Boot & Run Mechanics Inside macOS VMs on Apple Silicon

Research compiled from primary sources: official Talos/Sidero docs (`docs.siderolabs.com`,
formerly `talos.dev`), the `siderolabs/talos` and `siderolabs/image-factory` source and issue
trackers, the Image Factory service (`factory.talos.dev`), and the tart/lima/UTM projects.
Talos docs were verified against **v1.10–v1.13** (the docs site moved from `talos.dev` to
`docs.siderolabs.com`; both hosts serve the same content and `talos.dev/v1.x/...` URLs now
301-redirect to `docs.siderolabs.com/talos/v1.x/...`).

Facts pulled directly from source code are cited to `github.com/siderolabs/talos`. Where a claim
is a community report rather than an official statement, it is labeled as such.

---

## TL;DR

- **Artifact for a generic arm64 VM:** the **`metal-arm64` ISO** from **Image Factory**
  (`factory.talos.dev`). The vanilla/no-customization schematic ID is
  `376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba`. The ISO boots entirely in
  RAM and does not touch disk until you apply a config. A raw disk image (`metal-arm64.raw.xz`) is
  the alternative if you want to pre-seed a virtual disk; kernel+initramfs direct boot is for PXE.
- **Maintenance mode:** an unconfigured node boots into maintenance mode and exposes the Talos gRPC
  API (`apid`) on **TCP 50000**, **unauthenticated** (encrypted-but-not-authenticated), reached via
  `talosctl --insecure`. Source-verified: the insecure client is granted only the **Reader** role,
  which permits read-only calls (`version`, `disks`, `dmesg`, `get`, …) plus the two write calls
  explicitly whitelisted for maintenance — `ApplyConfiguration` and `BlockDeviceWipe`. `bootstrap`,
  `reset`, `reboot`, `upgrade`, `kubeconfig` are Admin-only and require PKI (i.e., an applied config).
- **Install flow:** `talosctl apply-config` writes the config, Talos runs the installer, partitions
  the target disk (`machine.install.disk`) into **EFI / META / STATE / EPHEMERAL**, and reboots off
  disk. Upgrades are A/B via `talosctl upgrade --image <installer>`.
- **Apple Silicon gotcha (the big one):** Talos arm64 defaults to `console=ttyAMA0 console=tty0`
  and boots via UEFI/systemd-boot, both of which are fine under Apple's Virtualization.framework
  (VZ). **BUT** VZ's virtio-rng provides no entropy during the EFI/early-boot phase, so Talos hangs
  at `EFI stub: EFI_RNG_PROTOCOL unavailable` — an open bug affecting **tart, lima (vz driver), and
  vfkit** (`siderolabs/talos#11865`, open).
- **What actually works on a Mac today:** **QEMU with the `hvf` accelerator** (officially
  documented; `talosctl cluster create qemu` supports macOS since **v1.11**) and **UTM** (QEMU-based;
  a Sidero maintainer states it "works perfectly"). VZ-native tools (tart, lima-vz, vfkit) are
  blocked by the entropy hang unless you defeat headless operation by mashing the keyboard.

---

## 1. Which artifact to use

### The artifact families

Talos publishes several boot-asset shapes for `metal` on arm64. Per the boot-assets docs and the
Image Factory API, the relevant ones are:
[boot-assets](https://docs.siderolabs.com/talos/v1.13/platform-specific-installations/boot-assets),
[image-factory API](https://github.com/siderolabs/image-factory/blob/main/docs/api.md)

| Artifact | Path (Image Factory) | Use case |
|---|---|---|
| **Metal ISO** | `…/metal-arm64.iso` | Boot media for a VM/bare-metal node; **runs in RAM**, installs to disk on config apply |
| **SecureBoot ISO** | `…/metal-arm64-secureboot.iso` | Same, but UKI + systemd-boot signed for SecureBoot |
| **Raw disk image** | `…/metal-arm64.raw.xz` | Pre-written virtual disk (dd/attach), boots directly to disk |
| **Kernel / initramfs / cmdline** | `…/kernel-arm64`, `…/initramfs-arm64.xz`, `…/cmdline-metal-arm64` | PXE / direct kernel boot |
| **UKI (SecureBoot EFI)** | `…/metal-arm64-secureboot-uki.efi` | Direct UEFI/HTTP-boot of a Unified Kernel Image |
| **Installer image (OCI)** | `factory.talos.dev/metal-installer/<schematic>:<version>` | Referenced by `machine.install.image` and `talosctl upgrade --image` |

Append `.sha256` / `.sha512` to any path for a checksum.
([image-factory API](https://github.com/siderolabs/image-factory/blob/main/docs/api.md))

**Which to pick for a generic VM (bare-metal-like) on arm64:** the **`metal-arm64` ISO**. The
Getting Started guide describes booting nodes from the Talos ISO into maintenance mode, noting the
ISO "runs entirely in RAM and won't modify your disks until you apply a configuration."
([getting-started](https://docs.siderolabs.com/talos/v1.13/getting-started/getting-started)).
This is the closest match to "a bare-metal box with an installer disc." The **raw disk image**
(`metal-arm64.raw.xz`) is the alternative when you'd rather attach a pre-populated virtual disk than
an installer ISO. **Kernel+initramfs** direct boot is intended for PXE/network boot, not typical for
a single VM.

> Note on nomenclature: the `metal` platform is the right one for a generic hypervisor VM. Talos
> also ships cloud-specific images (e.g., `aws-arm64.raw.xz`, `nocloud-arm64.raw.xz`), but for a
> plain VZ/QEMU VM with no cloud metadata service, `metal` is the correct target. The Image Factory
> docs illustrate the raw path with an `aws-arm64.raw.xz` example, but the `metal-*` variants exist
> for the same asset types. ([image-factory](https://docs.siderolabs.com/talos/v1.13/learn-more/image-factory))

### Image Factory & schematics

**Image Factory** (`https://factory.talos.dev`) is a Sidero-run service that "generates customized
Talos Linux images based on configured schematics." A **schematic** is a small YAML document
describing customizations (extra kernel args, system extensions, META values, SecureBoot, overlay
for SBCs). It is **content-addressable**: uploading a schematic returns a SHA-256 ID, and the same
schematic always yields the same ID.
([image-factory](https://docs.siderolabs.com/talos/v1.13/learn-more/image-factory))

Create a schematic and get its ID:

```bash
curl -X POST --data-binary @schematic.yaml https://factory.talos.dev/schematics
# → {"id":"<64-hex schematic id>"}
```

([image-factory API](https://github.com/siderolabs/image-factory/blob/main/docs/api.md))

The **default "vanilla" schematic** — an empty customization — has the well-known ID:

```
376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba
```

corresponding to the schematic body:

```yaml
customization:
```

([image-factory](https://docs.siderolabs.com/talos/v1.13/learn-more/image-factory);
[image-factory API](https://github.com/siderolabs/image-factory/blob/main/docs/api.md))

Image download URL pattern:

```
https://factory.talos.dev/image/<schematic-id>/<talos-version>/<path>
```

Example — vanilla arm64 metal ISO for a given version:

```
https://factory.talos.dev/image/376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba/v1.10.5/metal-arm64.iso
```

Schematics do **not** pin a Talos version — the version comes from the URL, and Image Factory picks
the matching extension versions. ([boot-assets](https://docs.siderolabs.com/talos/v1.13/platform-specific-installations/boot-assets))

**System extensions** are optional packages (e.g., `siderolabs/gvisor`, `siderolabs/amd-ucode`,
`siderolabs/intel-ucode`) baked into the image at build time by listing them under
`customization.systemExtensions.officialExtensions` in the schematic. Not every extension is
available for every Talos version. ([image-factory](https://docs.siderolabs.com/talos/v1.13/learn-more/image-factory))

**Sidero's recommendation** for producing boot assets is Image Factory (the alternative, `imager`,
is a container-based tool for custom/unreleased builds).
([boot-assets](https://docs.siderolabs.com/talos/v1.13/platform-specific-installations/boot-assets)).
Metal ISOs are also attached to GitHub releases, but the Image Factory route is preferred because it
lets you add extensions/kernel args without rebuilding.

---

## 2. Maintenance mode & the unconfigured node

### What maintenance mode is

When a Talos node boots **without a machine config** (e.g., freshly booted from the metal ISO), it
enters **maintenance mode**: it runs a minimal set of services, the Talos API is up, but there is no
Kubernetes and no PKI yet. It "waits for a configuration to be applied."
([getting-started](https://docs.siderolabs.com/talos/v1.13/getting-started/getting-started))

Internally, "maintenance" is simply the state where the machine config is not yet complete for boot.
The gRPC server computes `inMaintenance := !s.Controller.Runtime().ConfigCompleteForBoot()` and
branches accordingly.
([v1alpha1_server.go](https://github.com/siderolabs/talos/blob/main/internal/app/machined/internal/server/v1alpha1/v1alpha1_server.go))

### Discovery, port, and the insecure client

- The Talos API service (`apid`) listens on **TCP port 50000**.
- The node gets its IP from **DHCP**; you read it from the node's console/dashboard or from your
  DHCP server's leases. ([getting-started](https://docs.siderolabs.com/talos/v1.13/getting-started/getting-started))
- Because no PKI has been provisioned yet, you must connect with **`talosctl --insecure`**. The
  connection is **encrypted but not authenticated**. ([getting-started](https://docs.siderolabs.com/talos/v1.13/getting-started/getting-started))

Typical apply into maintenance mode:

```bash
talosctl apply-config --insecure \
  --nodes 10.0.0.10 \
  --file controlplane.yaml
```

### API surface — exactly what works in maintenance mode (source-verified)

The security model is enforced by RBAC roles. In maintenance mode `apid` runs its role injector in
**ReadOnly** mode, which grants the connecting (insecure) client only the **`Reader`** role. (When
SideroLink is configured, the mode is `ReadOnlyWithAdminOnSiderolink` — a SideroLink peer gets
`Admin`, everyone else still gets `Reader`.)
([authz/injector.go](https://github.com/siderolabs/talos/blob/main/pkg/grpc/middleware/authz/injector.go))

The method→role table in `machined.go` then decides which calls a `Reader` may make. Two write
methods are explicitly extended to `Reader` **for maintenance only** (with a handler-level check):

```go
"/machine.MachineService/ApplyConfiguration": role.MakeSet(role.Admin, /* maintenance only */ role.Reader),
"/storage.StorageService/BlockDeviceWipe":    role.MakeSet(role.Admin, /* maintenance only */ role.Reader),
```

([machined.go](https://github.com/siderolabs/talos/blob/main/internal/app/machined/pkg/system/services/machined.go))

**Works over `--insecure` in maintenance mode (Reader role):**

- `talosctl apply-config` → `MachineService/ApplyConfiguration`
- `talosctl version` → `MachineService/Version`
- `talosctl disks` → `storage.StorageService/Disks`
- `talosctl dmesg` → `MachineService/Dmesg`
- `talosctl get <resource>` / `list` → `cosi.resource.State/{Get,List,Watch}`
- Read-only diagnostics: `CPUInfo`, `Memory`, `DiskStats`, `Mounts`, `Netstat`, `Processes`,
  `ServiceList`, `Logs`, `Hostname`, `Stats`, `Events`, `NetworkDeviceStats`
- `talosctl wipe disk` → `storage.StorageService/BlockDeviceWipe`

**Does NOT work over `--insecure` (Admin/Operator-only → needs an applied config + PKI):**

- `talosctl bootstrap` (`Bootstrap`, Admin)
- `talosctl reset` (`Reset`, Admin)
- `talosctl reboot` / `shutdown` (`Reboot`/`Shutdown`, Admin+Operator)
- `talosctl upgrade` (`Upgrade`, Admin) — note the server *does* implement a
  `SequenceMaintenanceUpgrade` path, so an upgrade in maintenance is logically possible, but RBAC
  requires Admin, i.e., only reachable via a SideroLink Admin peer, not plain `--insecure`
  ([v1alpha1_server.go](https://github.com/siderolabs/talos/blob/main/internal/app/machined/internal/server/v1alpha1/v1alpha1_server.go))
- `talosctl kubeconfig` (`Kubeconfig`, Admin), `talosctl rollback` (`Rollback`, Admin),
  `GenerateClientConfiguration` (Admin)

Also note: in maintenance mode the `Reboot` **REBOOT mode** is rejected — the server returns
`Unimplemented: the REBOOT mode is not supported, please use AUTO or NO_REBOOT modes instead`.
([v1alpha1_server.go](https://github.com/siderolabs/talos/blob/main/internal/app/machined/internal/server/v1alpha1/v1alpha1_server.go))

### Security model summary

- Maintenance API is **unauthenticated by design** — anyone who can reach `:50000` before you apply
  a config can apply one (or wipe a disk). The connection is TLS-encrypted but the server does not
  verify the client, hence `--insecure`. ([getting-started](https://docs.siderolabs.com/talos/v1.13/getting-started/getting-started))
- The blast radius is deliberately limited to Reader + `ApplyConfiguration` + `BlockDeviceWipe`
  (source above). Once a config with secrets (PKI) is applied, `apid` switches to mutual-TLS and the
  full authenticated API (with Admin/Operator/Reader roles) becomes available.
- For a hardened bring-up, Sidero's SideroLink/Omni path tunnels the maintenance API over a
  point-to-point WireGuard link and grants Admin only to the SideroLink peer.
  ([authz/injector.go](https://github.com/siderolabs/talos/blob/main/pkg/grpc/middleware/authz/injector.go))

---

## 3. Disk layout & install flow

### What `apply-config` does to a maintenance-mode node

Applying a machine config to a maintenance-mode node causes Talos to **install itself to disk**:
"This step installs Talos to the target disk." Talos partitions the disk named by
`machine.install.disk`, writes the system, and reboots off disk. After the node comes back up (now
configured), you run `talosctl bootstrap` once on a single control-plane node to init etcd/Kubernetes.
([getting-started](https://docs.siderolabs.com/talos/v1.13/getting-started/getting-started))

### `machine.install.disk`

`machine.install.disk` designates which block device receives the Talos partition scheme, e.g.
`/dev/vda` (virtio disk on KVM/QEMU) or `/dev/sda`.
([getting-started](https://docs.siderolabs.com/talos/v1.13/getting-started/getting-started),
[kvm](https://docs.siderolabs.com/talos/v1.13/platform-specific-installations/virtualized-platforms/kvm)).
Under Apple VZ/QEMU with a virtio-blk disk this is typically `/dev/vda`.

### Partition layout

Talos hardcodes a four-partition layout on the boot disk:
([disk layout](https://docs.siderolabs.com/talos/v1.13/configure-your-talos-cluster/storage-and-disk-management/disk-management/layout))

| Partition | Approx size | Purpose |
|---|---|---|
| **EFI** | ~1 GiB | EFI system partition used for booting |
| **META** | ~1 MiB | Talos metadata (the META store) |
| **STATE** | ~100 MiB | System state, **including the machine configuration** (persisted across reboots) |
| **EPHEMERAL** | remainder | Container data, pulled images, logs, `etcd` data, runtime state |

The `EFI`, `META`, and `STATE` partitions are fixed; `EPHEMERAL` by default consumes all remaining
space but can be resized or placed on another disk, and additional user volumes can be defined.
([disk layout](https://docs.siderolabs.com/talos/v1.13/configure-your-talos-cluster/storage-and-disk-management/disk-management/layout)).
Minimum disk is **10 GiB**; production recommendation is 100 GiB, because EPHEMERAL holds images and
runtime data. ([system-requirements](https://docs.siderolabs.com/talos/v1.13/getting-started/system-requirements))

### "Ephemeral" vs "installed to disk"

- **ISO-booted, no config applied:** Talos runs **entirely in RAM**; nothing is written to disk.
  This is the maintenance-mode state. ([getting-started](https://docs.siderolabs.com/talos/v1.13/getting-started/getting-started))
- **Installed:** applying a config triggers the installer, which lays down the partitions above and
  makes the node boot from disk on the next reboot. STATE persists the machine config; EPHEMERAL
  persists container/etcd data.

### The installer image

The installer is an OCI image (`ghcr.io/siderolabs/installer:<version>` for vanilla, or
`factory.talos.dev/metal-installer/<schematic>:<version>` for a customized schematic). It contains
the code that partitions the disk and writes the Talos system; it is referenced by
`machine.install.image` at install time and by `talosctl upgrade --image` at upgrade time.
([image-factory API](https://github.com/siderolabs/image-factory/blob/main/docs/api.md),
[upgrading-talos](https://docs.siderolabs.com/talos/v1.13/configure-your-talos-cluster/lifecycle-management/upgrading-talos))

### Upgrade path (A/B)

Upgrades are performed via an API call carrying an installer image reference:

```bash
talosctl upgrade --nodes 10.20.30.40 \
  --image ghcr.io/siderolabs/installer:v1.13.6
```

- Talos keeps an **A/B** scheme: the previous kernel + OS image are retained after each upgrade, so
  a failed boot **auto-rolls-back**; you can also `talosctl rollback` manually.
- The upgrade sequence cordons/drains the node, shuts down services, unmounts filesystems, writes the
  new image, and reboots (via `kexec` where enabled).
- v1.13 introduced a streaming upgrade API with `--progress`, `--namespace`, and `--reboot-mode`
  (`default|powercycle|force`); the old `--force`, `--insecure`, `--preserve`, `--stage` flags are
  deprecated (removal targeted for 1.18).
- For non-adjacent versions, upgrade through each intermediate minor release's latest patch.

([upgrading-talos](https://docs.siderolabs.com/talos/v1.13/configure-your-talos-cluster/lifecycle-management/upgrading-talos))

> On arm64 specifically, `kexec` has been unstable in the local dev provisioner — see §4/§5 for
> `siderolabs/talos#13769`.

---

## 4. Apple Silicon / Virtualization.framework gotchas

### Does Talos arm64 boot via UEFI? Yes.

For Talos **≥ v1.10**, all arm64 boot assets boot via **UEFI** using **systemd-boot** (GRUB is only
used for legacy x86 BIOS; before v1.10 GRUB was the default except for SecureBoot images). The modern
path uses a **UKI** (Unified Kernel Image) — kernel + initramfs + cmdline in one signed EFI binary.
([bootloader](https://docs.siderolabs.com/talos/v1.13/platform-specific-installations/bare-metal-platforms/bootloader)).
Talos therefore **needs a UEFI environment** on arm64. Apple's Virtualization.framework does provide
a UEFI/EFI environment for Linux guests, and QEMU provides it via `edk2-aarch64-code.fd` (EDK2/OVMF).

### Serial console behavior (ttyAMA0 vs ttyS0)

The **default arm64 `metal` kernel command line** (fetched live from Image Factory,
`cmdline-metal-arm64`, v1.10.5, vanilla schematic) is verbatim:

```
talos.platform=metal console=ttyAMA0 console=tty0 init_on_alloc=1 slab_nomerge pti=on \
  consoleblank=0 nvme_core.io_timeout=4294967295 printk.devkmsg=on ima_template=ima-ng \
  ima_appraise=fix ima_hash=sha512 selinux=1
```

(Source: `https://factory.talos.dev/image/376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba/v1.10.5/cmdline-metal-arm64`)

Key point: on the ARM `virt` machine the primary serial UART is the **PL011 → `ttyAMA0`**, *not* the
x86 `ttyS0`. Talos's default arm64 cmdline already targets `ttyAMA0` (plus `tty0`), which matches
what both Apple VZ and QEMU-`virt` expose, so the serial console generally "just works" on arm64
without extra args. The classic gotcha appears if you try to force `console=ttyS0` (x86 style) on
arm64, or when a UKI is used and you must inject an extra console arg via systemd-boot's SMBIOS
mechanism — see the QEMU example in `#13108`:
`-smbios type=11,value=io.systemd.stub.kernel-cmdline-extra=console=ttyS0`
([talos#13108](https://github.com/siderolabs/talos/issues/13108)).

> UKI caveat: with systemd-boot/UKI the `machine.install.extraKernelArgs` field is **ignored** —
> kernel args are embedded in the UKI. To add args you either rebuild via Image Factory
> (`extraKernelArgs` in the schematic) or use the systemd-boot SMBIOS `kernel-cmdline-extra`.
> ([bootloader](https://docs.siderolabs.com/talos/v1.13/platform-specific-installations/bare-metal-platforms/bootloader))

### The headline VZ bug: EFI RNG / entropy hang

This is the single most important Apple-Silicon-specific issue.

**`siderolabs/talos#11865` — "Talos v1.11.1 boot hang with 'EFI_RNG_PROTOCOL unavailable' on macOS
Lima/Tart virtualization" — OPEN** (created 2025-09-21).
([talos#11865](https://github.com/siderolabs/talos/issues/11865))

- Symptom: Talos arm64 hangs at boot right after `EFI stub: EFI_RNG_PROTOCOL unavailable`, sitting on
  the Sidero splash indefinitely. Pressing random keyboard keys unblocks it (generates entropy).
- **smira (Sidero maintainer, official):** "QEMU on Mac, and UTM (which is QEMU-based) works
  perfectly"; also reported the same issue with VirtualBox. He noted Talos "enforces security by
  default" and won't relax its entropy requirement.
- **Community root-cause (devunt, not an official Sidero conclusion but consistent with the above):**
  Apple's Virtualization.framework advertises a virtio-rng device
  (`VZVirtioEntropyDeviceConfiguration`) that works *after* the machine has booted but **provides no
  entropy during the EFI phase**, whereas QEMU implements it properly. The Linux kernel then blocks
  waiting for CRNG init.

**Related: `siderolabs/talos#11837`** — splash screen hangs without keyboard input under an Apple VM
using **vfkit** — **CLOSED (stale/not_planned)**.
([talos#11837](https://github.com/siderolabs/talos/issues/11837)).
Here **frezbo (Sidero maintainer)** diagnosed: "`random: crng init done` explains the issue, seems
the vfkit is not providing enough random entropy so the kernel is waiting … look into some config
issues with vfkit," and noted the 10-second systemd-boot menu timeout.

**Bottom line:** any hypervisor built on Apple's Virtualization.framework (tart, lima's `vz` driver,
vfkit, UTM's Apple-Virtualization backend) will hit this early-boot entropy stall for headless/
unattended boots. QEMU (and QEMU-backed UTM) do not.

### SecureBoot support under VZ

- SecureBoot images exist for Talos releases from **v1.5.0** onward, implemented with
  **systemd-boot + UKI + TPM 2.0**; measurements go into PCR 11 (UKI sections/boot phases) and PCR 7
  (SecureBoot state / enrolled keys). ([secureboot](https://docs.siderolabs.com/talos/v1.13/platform-specific-installations/bare-metal-platforms/secureboot))
- **Automatic key enrollment only happens when running in a VM**; on bare metal you enroll from the
  boot menu in setup mode, or set `secure-boot-enroll=force` in the ISO.
  ([secureboot](https://docs.siderolabs.com/talos/v1.13/platform-specific-installations/bare-metal-platforms/secureboot))
- Talos supports UKI/SecureBoot on **arm64 UEFI** systems. (Some ARM SBCs boot via U-Boot, which
  does not support UKI — not relevant to VZ/QEMU, which are UEFI.)
- Image Factory provides SecureBoot images signed with the Sidero Labs key
  (`metal-arm64-secureboot.iso`, `metal-arm64-secureboot-uki.efi`).
- The docs frame the chain as **SBAT/systemd-boot** signed with the enrolled key; there is no
  guarantee Apple VZ's EFI implementation enrolls/verifies keys the way a full UEFI firmware does.
  There is **no primary source confirming a working SecureBoot Talos boot specifically under Apple
  Virtualization.framework** — treat VZ SecureBoot as unverified. SecureBoot is **not** supported on
  x86 BIOS mode, and there is no in-place upgrade path from a GRUB (non-UKI) install to UKI/SecureBoot
  (a fresh install is required). ([secureboot](https://docs.siderolabs.com/talos/v1.13/platform-specific-installations/bare-metal-platforms/secureboot))

### Device/driver support inside VZ

VZ exposes virtio devices (virtio-blk disk, virtio-net networking, virtio-rng, virtio serial/console)
to Linux guests; Talos ships virtio drivers and boots fine on virtio hardware (this is the same
device model QEMU `virt` uses, and the KVM guide installs to `/dev/vda` over a virtio bus)
([kvm](https://docs.siderolabs.com/talos/v1.13/platform-specific-installations/virtualized-platforms/kvm)).
The **only** VZ-specific breakage documented in primary sources is the **EFI-phase entropy** problem
above — not a missing-driver problem.

---

## 5. How people run Talos on Macs today

> The tool-by-tool findings below come from GitHub searches of `siderolabs/talos`, `cirruslabs/tart`,
> `lima-vm/lima`, and `utmapp/UTM`, plus official Sidero docs. Community reports are labeled as such.

### QEMU (officially supported path)

- The **official "Local platforms → QEMU"** docs list **"Apple Silicon Mac"** as a supported host,
  install via `brew install qemu`, and state: *"On MacOS the `hvf` accelerator (Apple Hypervisor
  Framework) is utilized. Networking is created by QEMU via the apple vmnet framework."* The docs
  mention **hvf** but **not** vz/vfkit/Virtualization.framework.
  ([qemu local platform](https://docs.siderolabs.com/talos/v1.11/platform-specific-installations/local-platforms/qemu))
- **`talosctl cluster create qemu` gained macOS support in Talos v1.11** via
  **PR #11110 "feat: support qemu provisioner on darwin"** (merged 2025-05-29).
  ([talos#11110](https://github.com/siderolabs/talos/pull/11110))
- Working manual QEMU invocations from community discussions/issues (arm64 + hvf):
  - **Discussion #9799** — Talos v1.8.3 arm64 booted with
    `-M virt,highmem=on,gic-version=3 -cpu cortex-a72 -accel hvf -bios edk2-aarch64-code.fd`,
    console on `ttyAMA0`, reaching the etcd-wait stage (needs `talosctl bootstrap`). *Community.*
    ([talos#9799](https://github.com/siderolabs/talos/discussions/9799))
  - **Issue #13108** (CLOSED, misconfig — resolved): Talos v1.12.6 machined "hung" on QEMU aarch64
    HVF until the reporter added **`-cpu max`**, **`gic-version=max`**, and passed
    `console=ttyS0` via SMBIOS `io.systemd.stub.kernel-cmdline-extra`. **shanduur (Sidero
    maintainer)** posted the exact working params Sidero uses. Not a Talos regression.
    ([talos#13108](https://github.com/siderolabs/talos/issues/13108))
- Known **open** macOS/QEMU rough edges (all QEMU path, not VZ):
  - **#13769 (OPEN)** — `talosctl cluster create qemu` on darwin/arm64 regressed in v1.13.1 after
    kexec was re-enabled on arm64 (PR #13265). frezbo: fixed for 1.14, won't backport; workaround is
    setting `kernel.kexec_load_disabled` on the cmdline.
    ([talos#13769](https://github.com/siderolabs/talos/issues/13769))
  - **#12727 (OPEN)** — macOS `cluster create qemu` hits a Linux-bridge code path ("interface
    bridge103 not found") instead of vmnet in the dhcpd step.
    ([talos#12727](https://github.com/siderolabs/talos/issues/12727))
  - **#12834 (OPEN)** — feature request for `socket_vmnet` / `vmnet-bridged` networking for the QEMU
    provisioner. ([talos#12834](https://github.com/siderolabs/talos/issues/12834))

### UTM

- No Talos-specific issue exists in `utmapp/UTM`. The authoritative statement is from Sidero:
  **smira (maintainer)** in `#11865` — **"UTM (which is QEMU-based) works perfectly."**
  ([talos#11865](https://github.com/siderolabs/talos/issues/11865))
- Caveat: that endorsement is for UTM's **QEMU** backend. UTM's alternative Apple-Virtualization
  backend would inherit the VZ entropy hang.

### tart (`cirruslabs/tart`, VZ-based)

- **No Talos issues or discussions exist in the tart repo** (GitHub search returns zero). tart is
  built on Apple Virtualization.framework and supports generic arm64 Linux VMs from an ISO, but there
  is **no primary source demonstrating a clean Talos boot under tart**. Because tart is VZ-based, a
  `metal-arm64.iso` boot is expected to hit the EFI entropy hang from `#11865` (whose title
  explicitly names "Tart"). **Verdict: unverified / likely blocked by the entropy issue.**
  ([talos#11865](https://github.com/siderolabs/talos/issues/11865))

### lima (`lima-vm/lima`)

- **There is no Talos template** in lima — the `templates/` directory has alpine, debian, fedora,
  ubuntu, k3s, k8s, k0s, etc., but no `talos.yaml`.
  ([lima templates](https://github.com/lima-vm/lima/tree/master/templates))
- On macOS ≥ 13.5 lima defaults to the **`vz` (Virtualization.framework)** driver, so a lima-vz Talos
  attempt hits the same EFI entropy hang (`#11865` names "Lima"). lima can be switched to its **QEMU**
  driver, which would follow the working QEMU path (inferred; no dedicated source).
  ([talos#11865](https://github.com/siderolabs/talos/issues/11865))

### vfkit / colima / krunkit

- **vfkit** (`crc-org/vfkit`, a thin Virtualization.framework CLI): no Talos issues in its own repo;
  the only evidence is `siderolabs/talos#11837`, where vfkit exhibited the VZ entropy hang.
  **Broken for headless Talos boot** (same VZ limitation). ([talos#11837](https://github.com/siderolabs/talos/issues/11837))
- **colima** (`abiosoft/colima`): zero Talos issues; oriented at container/docker/k3s runtimes, not
  raw Talos VM images. **No evidence** it runs Talos.
- **krunkit** (`containers/krunkit`, libkrun): **no primary source** connecting it to Talos.
  **Unverified.**

### Summary table

| Tool | Backend | Talos on Apple Silicon? | Primary-source status |
|---|---|---|---|
| **QEMU** (`talosctl cluster create qemu` / manual) | QEMU + hvf | **Yes — officially documented** | Docs list "Apple Silicon Mac"; PR #11110 (v1.11); working configs in #13108, #9799 |
| **UTM** | QEMU (default) | **Yes** | smira (Sidero): "works perfectly" (#11865) |
| **tart** | Virtualization.framework | **Unverified / blocked** | 0 Talos issues in repo; VZ entropy hang (#11865 names "Tart") |
| **lima (vz)** | Virtualization.framework | **No template; blocked** | no `talos.yaml`; #11865 names "Lima" |
| **lima (qemu driver)** | QEMU | Likely works (inferred) | no dedicated source |
| **vfkit** | Virtualization.framework | **Broken (entropy hang)** | #11837 (frezbo confirms) |
| **Apple VZ directly** | Virtualization.framework | **No official support** | talosctl uses QEMU; no vz/vfkit provisioner |
| **colima** | Lima/QEMU | No evidence | 0 Talos issues |
| **krunkit** | libkrun | Unverified | no source |

---

## Uncertainties / could not verify

- **A clean, automated Talos boot under Apple Virtualization.framework** (tart, lima-vz, vfkit): no
  primary source shows success, and `#11865`/`#11837` predict the EFI entropy hang. Whether a future
  Talos or VZ change fixes the EFI-phase RNG is unresolved (#11865 is still open).
- **SecureBoot Talos under Apple VZ specifically:** docs confirm SecureBoot exists and auto-enrolls
  "in a VM," but there is no source confirming it works under Apple's VZ EFI implementation.
- **lima QEMU-driver + Talos:** expected to work like plain QEMU, but no dedicated source demonstrates
  it.
- **Exact Talos version cut where systemd-boot became the arm64 default:** docs say "prior to v1.10
  GRUB dominated except for SecureBoot"; treat "arm64 = systemd-boot/UKI by default" as accurate for
  **≥ v1.10**. ([bootloader](https://docs.siderolabs.com/talos/v1.13/platform-specific-installations/bare-metal-platforms/bootloader))
- **`krunkit` + Talos:** no evidence either way.

## Primary sources

Talos / Sidero docs (`docs.siderolabs.com`):
- Getting Started — https://docs.siderolabs.com/talos/v1.13/getting-started/getting-started
- Boot assets — https://docs.siderolabs.com/talos/v1.13/platform-specific-installations/boot-assets
- Image Factory — https://docs.siderolabs.com/talos/v1.13/learn-more/image-factory
- Disk layout — https://docs.siderolabs.com/talos/v1.13/configure-your-talos-cluster/storage-and-disk-management/disk-management/layout
- Upgrading Talos — https://docs.siderolabs.com/talos/v1.13/configure-your-talos-cluster/lifecycle-management/upgrading-talos
- Bootloader — https://docs.siderolabs.com/talos/v1.13/platform-specific-installations/bare-metal-platforms/bootloader
- SecureBoot — https://docs.siderolabs.com/talos/v1.13/platform-specific-installations/bare-metal-platforms/secureboot
- System requirements — https://docs.siderolabs.com/talos/v1.13/getting-started/system-requirements
- KVM — https://docs.siderolabs.com/talos/v1.13/platform-specific-installations/virtualized-platforms/kvm
- VMware — https://docs.siderolabs.com/talos/v1.13/platform-specific-installations/virtualized-platforms/vmware
- QEMU local platform — https://docs.siderolabs.com/talos/v1.11/platform-specific-installations/local-platforms/qemu

Source code (`github.com/siderolabs/talos`):
- machined.go (RBAC method→role map) — https://github.com/siderolabs/talos/blob/main/internal/app/machined/pkg/system/services/machined.go
- authz/injector.go (ReadOnly = Reader role in maintenance) — https://github.com/siderolabs/talos/blob/main/pkg/grpc/middleware/authz/injector.go
- v1alpha1_server.go (inMaintenance branches, ApplyConfiguration/Upgrade) — https://github.com/siderolabs/talos/blob/main/internal/app/machined/internal/server/v1alpha1/v1alpha1_server.go

Image Factory:
- https://factory.talos.dev/ and API — https://github.com/siderolabs/image-factory/blob/main/docs/api.md
- Live cmdline — https://factory.talos.dev/image/376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba/v1.10.5/cmdline-metal-arm64

Issues / discussions:
- talos#11865 (VZ EFI entropy hang; Lima/Tart) — https://github.com/siderolabs/talos/issues/11865
- talos#11837 (vfkit entropy hang) — https://github.com/siderolabs/talos/issues/11837
- talos#11110 (qemu provisioner on darwin, v1.11) — https://github.com/siderolabs/talos/pull/11110
- talos#13108 (QEMU aarch64 HVF config) — https://github.com/siderolabs/talos/issues/13108
- talos#9799 (QEMU arm64 hvf working boot) — https://github.com/siderolabs/talos/discussions/9799
- talos#13769 (darwin/arm64 kexec regression) — https://github.com/siderolabs/talos/issues/13769
- talos#12727 (macOS vmnet/bridge) — https://github.com/siderolabs/talos/issues/12727
- talos#12834 (socket_vmnet feature request) — https://github.com/siderolabs/talos/issues/12834
- lima templates (no talos.yaml) — https://github.com/lima-vm/lima/tree/master/templates
