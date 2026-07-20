# Hypervisor stack for running Talos Linux arm64 VMs on Apple Silicon

**Audience:** engineers implementing a Go CLI that spins up Talos Linux
`arm64` clusters on Apple Silicon Macs (M1+, 16 GB min).

## TL;DR recommendation

**Build directly on Apple Virtualization.framework through the
[`Code-Hex/vz`](https://github.com/Code-Hex/vz) Go bindings (option a), and
pair it with a `vmnet` shared-mode helper
([`lima-vm/socket_vmnet`](https://github.com/lima-vm/socket_vmnet) or
[`crc-org/vfkit`](https://github.com/crc-org/vfkit)'s `vmnet-helper`) attached
via `VZFileHandleNetworkDeviceAttachment` for networking.** This is the only
option that gives you native, in-process Go lifecycle control (create / start /
stop / destroy / add-remove-node) *and* macOS 14+ save/restore snapshots, while
sidestepping the restricted `com.apple.vm.networking` bridged entitlement that
Apple only grants to approved virtualization vendors. The `vmnet` shared mode
puts every Talos node on one real L2 subnet with real MACs and host-routable
IPs — which is what Cilium / BGP / LoadBalancer flows need — without that
entitlement. **`crc-org/vfkit` is the recommended fallback** if you would rather
shell out to a small, Red-Hat-maintained hypervisor over a REST API than carry a
cgo dependency. Confidence: **medium-high** on the recommendation, **medium** on
the specific claim that Talos boots unmodified on Virtualization.framework
(strong evidence it boots on QEMU-HVF; the vz path is mechanically sound but I
found no published end-to-end Talos-on-vz success, and there is a real
uncompressed-kernel gotcha on Apple Silicon).

## Comparison table

| Axis | (a) `Code-Hex/vz` (Virtualization.framework, native Go) | (b) QEMU + HVF | (c) Wrap a tool (vfkit / lima / tart) |
|---|---|---|---|
| 1. Talos arm64 boots | Mechanically sound (LinuxBootLoader = kernel+initrd, or EFI+raw disk); no published Talos-on-vz success found; Apple-Silicon needs an **uncompressed** kernel | **Confirmed**: Talos v1.8.3 arm64 boots under QEMU-HVF, got DHCP IP ([disc #9799](https://github.com/siderolabs/talos/discussions/9799)); but **regression** v1.12.6 hangs post-initramfs ([#13108](https://github.com/siderolabs/talos/issues/13108)) | vfkit = same vz engine (uncompressed-kernel constraint); lima/tart target their own images, not raw Talos metal | 
| 2. Boot/runtime efficiency on M1+ | Near-native; Apple's own VIRTIO stack | Good with HVF, but QEMU device-model overhead > vz; full emulation only if cross-arch | vfkit near-native (vz); lima-vz near-native; tart "near-native" (vz) |
| 3. Networking / L2 fidelity | vz NAT = per-VM NAT (no shared L2); **bridged needs `com.apple.vm.networking`**; **use `FileHandleNetworkDeviceAttachment` + socket_vmnet shared → real L2, real MAC, VMs see each other, no bridged entitlement** | vmnet-shared / vmnet-bridged via QEMU; #9799 used `vmnet-bridged` and got a routable IP; flexible but bridged needs entitlement/root | vfkit integrates `vmnet-helper` (shared/bridged/host) + gvproxy; lima uses socket_vmnet (shared/bridged); tart has NAT / `--net-softnet` / `--net-bridged` |
| 4. Snapshot / save-restore | `SaveMachineState`/`RestoreMachineState` (vz **v3.1.0+**, needs **macOS 14 Sonoma**, VM must be paused; no Virtio-GPU) | `savevm`/qcow2 snapshots (arch-independent, always available) | vfkit exposes Saving/Restoring states but **no documented save endpoint**; tart has suspend; lima has limited support |
| 5. Programmatic lifecycle from Go | **Native, in-process** Go API (start/stop/pause/resume/requestStop) — best fit | Shell out to `qemu-system-aarch64` + QMP socket | Shell out to CLI + REST (vfkit) / CLI (tart/lima); no in-proc control |
| 6. Signing / entitlements / notarization | Must code-sign Go binary with `com.apple.security.virtualization`; bridged adds restricted `com.apple.vm.networking`; vmnet shared/host does **not** need it | Homebrew QEMU is signed with the entitlement already; your wrapper doesn't touch the entitlement | vfkit/tart ship pre-signed; you sign only your own wrapper; bridged still restricted |
| 7. Maturity / maintenance | `Code-Hex/vz` v3.7.1, active, single-maintainer (Code-Hex); MIT | QEMU very mature, HVF-on-arm well-trodden but Talos-specific regressions exist | vfkit v0.6.4 (Jul 2026), crc-org/Red Hat, Apache-2.0; tart v2.33.0 (Jul 2026), OpenAI/Cirrus, Fair Source; lima mature, VZ default since v1.0 |

## Option (a) — Apple Virtualization.framework via `Code-Hex/vz`

**What it is.** Go bindings that wrap Apple's Virtualization.framework:
VIRTIO network/storage/serial/entropy/balloon devices, vsock, and both the
Linux (`NewLinuxBootLoader`, kernel + initrd + cmdline) and EFI bootloaders. It
requires macOS 11+ and supports the last two Go releases
([README](https://github.com/Code-Hex/vz)).

**Talos boot (axis 1).** The example Linux VM boots via
`vz.NewLinuxBootLoader(vmlinuz, WithCommandLine("console=hvc0 root=/dev/vda"),
WithInitrd(initrd))` plus a virtio block device for the disk and a NAT network
device
([example/linux/main.go](https://github.com/Code-Hex/vz/blob/main/example/linux/main.go)).
That maps cleanly onto Talos, which ships `vmlinuz` + `initramfs` alongside its
`metal-arm64.raw`/`.iso` images, so the mechanics are sound. **Caveat:** on Apple
Silicon the Virtualization.framework Linux bootloader requires an **uncompressed**
kernel — vfkit (same engine) "will exit with an error if it detects a compressed
kernel when running on Apple silicon" — so a vz-based tool must decompress the
Talos `vmlinuz` before boot, or boot via EFI from the raw/ISO image instead. I
found **no published end-to-end Talos-on-vz success**; the closest siderolabs
thread ([disc #9867](https://github.com/siderolabs/talos/discussions/9867)) is
about *amd64-on-arm via Rosetta*, which fails with a seccomp error and is not the
`arm64`-native path we care about.

**Efficiency (axis 2).** Uses Apple's own hypervisor and VIRTIO devices —
near-native, the lowest-overhead option on M1+.

**Networking (axis 3).** This is the decisive axis. `vz` exposes three
attachments
([API](https://pkg.go.dev/github.com/Code-Hex/vz/v3)): `NATNetworkDeviceAttachment`
(per-VM NAT — VMs are *not* on a shared L2, so nodes can't see each other and
host-routable pod/service IPs don't work), `BridgedNetworkDeviceAttachment`
(real L2 but **requires `com.apple.vm.networking`**, see axis 6), and
`FileHandleNetworkDeviceAttachment` (raw datagram socket). The escape hatch is
`FileHandleNetworkDeviceAttachment` connected to a **`vmnet` shared-mode** helper
(socket_vmnet / vmnet-helper): shared mode puts all VMs on one L2 subnet with
real MACs, mutual visibility, and host-routable IPs, and — critically —
**shared/host `vmnet` modes do not require the `com.apple.vm.networking`
entitlement** (only bridge-to-physical does)
([Apple forum summary](https://developer.apple.com/forums/thread/729686)). This
is exactly how Lima wires vz + socket_vmnet.

**Snapshots (axis 4).** `vz` added `SaveMachineState` / `RestoreMachineState` in
**v3.1.0** (Oct 2023), wrapping Apple's `saveMachineState(to:)` /
`restoreMachineState(from:)`. This requires **macOS 14 (Sonoma)**, the VM must be
**paused** first, and (as of Sonoma 14.0) it does not support the Virtio GPU used
by GUI Linux — irrelevant for headless Talos
([WWDC23 "Create seamless experiences with Virtualization"](https://developer.apple.com/videos/play/wwdc2023/10007/),
[vz releases](https://github.com/Code-Hex/vz/releases)). So: snapshot/restore is
available but gated on macOS 14+.

**Lifecycle from Go (axis 5).** Best-in-class — everything is in-process Go:
`Start`, `Pause`, `Resume`, `Stop`, `RequestStop`, `State`,
`StateChangedNotify`, `CanPause`/`CanResume`
([API](https://pkg.go.dev/github.com/Code-Hex/vz/v3)). No child process, no QMP,
no REST. Add/remove-node is just "instantiate another VZVirtualMachine".

**Signing (axis 6).** You must code-sign the Go binary with
`com.apple.security.virtualization`; bridged networking additionally needs
`com.apple.vm.networking`, which is "restricted to developers of virtualization
software … contact your Apple representative" and must be authorized by a
provisioning profile
([Apple Developer Forums #729686](https://developer.apple.com/forums/thread/729686),
[README](https://github.com/Code-Hex/vz)). Avoiding bridged (via `vmnet`
shared) keeps you on the freely-usable `com.apple.security.virtualization`
entitlement, which any Developer-ID-signed binary can carry.

**Maturity (axis 7).** `Code-Hex/vz` is at **v3.7.1**, actively maintained but
essentially a single maintainer (Code-Hex); MIT-licensed; 24 releases. The main
risk is bus-factor, not correctness.

## Option (b) — QEMU with HVF acceleration

**Talos boot (axis 1).** The **strongest concrete boot evidence** is here:
[siderolabs disc #9799](https://github.com/siderolabs/talos/discussions/9799)
shows **Talos v1.8.3 `arm64` booting successfully** under
`qemu-system-aarch64 -M virt,gic-version=3 -accel hvf` with EDK2 firmware, a
QCOW2 disk, and `vmnet-bridged` networking — it reached DHCP (`10.0.2.192/24`)
and the "run talosctl bootstrap" prompt. **But** there is a live regression:
[#13108](https://github.com/siderolabs/talos/issues/13108) reports Talos
**v1.12.6** `machined` hangs immediately after initramfs handoff on QEMU-aarch64
+ HVF (no console, no traffic, 0% CPU), while **v1.9.5 works** on the identical
config — a QEMU-HVF-specific hazard you'd have to pin around.

**Efficiency (axis 2).** HVF gives hardware-accelerated arm64-on-arm64, but
QEMU's user-space device model adds overhead relative to Apple's native VIRTIO
path; acceptable, not optimal.

**Networking (axis 3).** Flexible: user-mode, `vmnet-shared`, and
`vmnet-bridged`. #9799 used bridged and got a host-routable IP. Same entitlement
caveat for true bridged.

**Snapshots (axis 4).** QEMU `savevm` + qcow2 internal snapshots work
regardless of macOS version — the most portable snapshot story of the three.

**Lifecycle (axis 5).** You shell out to `qemu-system-aarch64` and drive it over
a **QMP** socket. Workable but out-of-process, and you own process supervision.

**Signing (axis 6).** Homebrew's QEMU is already signed with the HVF
entitlement, so your wrapper avoids the entitlement problem entirely — at the
cost of depending on an external, separately-installed binary.

**Maturity (axis 7).** QEMU itself is extremely mature; HVF-on-arm is
well-trodden. The Talos-specific regression above shows the guest/host combo
still needs version pinning and testing.

## Option (c) — Wrap an existing tool

### `crc-org/vfkit` (recommended fallback)
A small Go command-line hypervisor on Virtualization.framework, maintained by
crc-org (Red Hat), Apache-2.0, **v0.6.4 (Jul 2026)**, adopted by minikube,
Podman, and CRC
([repo](https://github.com/crc-org/vfkit)). It supports the Linux
(`--bootloader linux,kernel=,initrd=,cmdline=`), EFI, and macOS bootloaders, and
integrates **gvisor-tap-vsock (user-mode)** plus **`vmnet-helper`
(shared/bridged/host)** networking; you drive it over a **REST API** (`GET/POST
/vm/state` with `HardStop`/`Pause`/`Resume`/`Stop`)
([usage.md](https://github.com/crc-org/vfkit/blob/main/doc/usage.md)). Same
uncompressed-kernel-on-Apple-Silicon constraint as raw vz. Its state enum lists
`Saving`/`Restoring` but **no save/restore endpoint is documented**, so treat
snapshots as unavailable via vfkit today. Good fit if you prefer a signed,
externally-maintained hypervisor + REST over a cgo dependency, at the cost of
losing native Go lifecycle control and (currently) save/restore.

### `lima-vm/lima` + `socket_vmnet`
Mature, **VZ is the default driver since v1.0** (macOS ≥ 13.5), also supports
QEMU; Virtualization.framework can't run cross-arch guests ("intel guest on arm
and vice versa" unsupported)
([vmtype](https://lima-vm.io/docs/config/vmtype/),
[vz driver](https://lima-vm.io/docs/config/vmtype/vz/)). Networking via
`socket_vmnet` (shared/bridged) is the reference implementation of the
vmnet-shared L2 pattern we want — but Lima is oriented toward dev-VM/`cloud-init`
images, not orchestrating raw Talos `metal-arm64` nodes, and wrapping its YAML +
CLI to manage a multi-node Talos cluster is an awkward fit. Best used as a
**pattern reference** (and as the upstream for `socket_vmnet`) rather than a
wrap target.

### `cirruslabs/tart` (now `openai/tart`)
Virtualization.framework toolset, **v2.33.0 (Jul 2026)**, "near-native
performance", runs macOS and Linux VMs on macOS 13+, with an OCI-registry
push/pull workflow and `tart create --linux`; networking is NAT by default with
`--net-bridged` and `--net-softnet`
([repo](https://github.com/cirruslabs/tart),
[quick start](https://tart.run/quick-start/)). Strong for its intended
macOS-CI/OCI-image use case, but it is **Fair Source** (usage restrictions to
check before shipping a product on it), CLI-only, and its opinionated
image/registry model is a poor match for booting arbitrary Talos metal images
and doing node add/remove orchestration. Not recommended as the base.

## Recommendation

**Primary: option (a) — `Code-Hex/vz` + `vmnet` shared (socket_vmnet or
vfkit's vmnet-helper) via `FileHandleNetworkDeviceAttachment`.** It uniquely
satisfies the two hardest requirements simultaneously — **native in-process Go
lifecycle control** and **multi-VM shared-L2 networking without the restricted
bridged entitlement** — and adds macOS-14+ snapshots for free. Supporting
evidence:

- Native Go lifecycle (start/pause/resume/stop/state) with no child process or
  QMP/REST layer ([vz API](https://pkg.go.dev/github.com/Code-Hex/vz/v3)).
- `SaveMachineState`/`RestoreMachineState` shipped in vz v3.1.0, giving true
  save/restore on macOS 14+ (paused VM, headless OK)
  ([WWDC23](https://developer.apple.com/videos/play/wwdc2023/10007/),
  [vz releases](https://github.com/Code-Hex/vz/releases)).
- `vmnet` shared/host modes deliver real-L2, real-MAC, mutually-visible,
  host-routable networking **without** `com.apple.vm.networking`, which is
  restricted to approved vendors
  ([Apple forum](https://developer.apple.com/forums/thread/729686)) — the L2
  fidelity Cilium/BGP/LoadBalancer need.
- Only `com.apple.security.virtualization` is required to sign/distribute the
  binary; you avoid the entitlement Apple gatekeeps
  ([vz README](https://github.com/Code-Hex/vz)).

**Fallback: option (c) `crc-org/vfkit`** if the team would rather not carry cgo
and prefers a signed, Red-Hat-maintained hypervisor driven over REST, accepting
the loss of native Go control and (for now) save/restore
([vfkit usage](https://github.com/crc-org/vfkit/blob/main/doc/usage.md)).

**Not recommended as the base:** raw QEMU (out-of-process, and a real
Talos-v1.12.x HVF hang regression to pin around —
[#13108](https://github.com/siderolabs/talos/issues/13108)), lima (dev-VM
oriented), tart (Fair-Source, OCI-image oriented).

### Implications for related decisions

- **Distribution / signing:** commit to Developer ID + notarization and embed
  `com.apple.security.virtualization`. **Design the network path around `vmnet`
  shared, not bridged**, so you never need the restricted
  `com.apple.vm.networking` entitlement. Budget for the `vmnet` helper needing a
  privileged/root component (socket_vmnet installs a root helper; vfkit's
  `vmnet-helper` similarly) — plan an install/first-run privilege step.
- **Networking design:** attach Talos nodes through one shared `vmnet` interface
  so all nodes land on the same subnet with routable IPs; this is what makes
  Cilium, BGP peering, and `type: LoadBalancer` behave like a real cluster.
  Per-VM vz NAT will not.
- **Snapshot feasibility:** fast cluster save/restore is realistic **only on
  macOS 14+** and only when the VM is paused; set macOS 14 as the minimum for
  snapshot features (macOS 13 can still run VMs, just without save/restore).
  Have a cold-boot fallback for macOS 13 users.
- **Boot pipeline:** plan to **decompress the Talos `vmlinuz`** (or boot EFI from
  the raw/ISO image) before handing it to the Linux bootloader on Apple Silicon,
  because Virtualization.framework rejects compressed kernels there. Validate
  against a current Talos release and pin known-good versions given the
  v1.12.6 HVF regression.

## Sources

- Code-Hex/vz README — features, entitlements, macOS 11+, v3.7.1: <https://github.com/Code-Hex/vz>
- Code-Hex/vz Linux example (LinuxBootLoader kernel+initrd, NAT, virtio block): <https://github.com/Code-Hex/vz/blob/main/example/linux/main.go>
- Code-Hex/vz API (lifecycle, network attachments): <https://pkg.go.dev/github.com/Code-Hex/vz/v3>
- Code-Hex/vz releases (SaveMachineState/RestoreMachineState in v3.1.0): <https://github.com/Code-Hex/vz/releases>
- WWDC23 "Create seamless experiences with Virtualization" (pause→saveMachineState→restore, macOS 14): <https://developer.apple.com/videos/play/wwdc2023/10007/>
- Apple Developer Forums — com.apple.vm.networking is restricted to virtualization vendors: <https://developer.apple.com/forums/thread/729686>
- Talos disc #9799 — Talos v1.8.3 arm64 boots on QEMU-HVF, vmnet-bridged, got DHCP: <https://github.com/siderolabs/talos/discussions/9799>
- Talos issue #13108 — v1.12.6 machined hangs on QEMU aarch64 HVF (v1.9.5 works): <https://github.com/siderolabs/talos/issues/13108>
- Talos disc #9867 — amd64-on-arm (Rosetta) fails with seccomp error: <https://github.com/siderolabs/talos/discussions/9867>
- crc-org/vfkit repo — Apache-2.0, Red Hat, v0.6.4, adopters: <https://github.com/crc-org/vfkit>
- crc-org/vfkit usage.md — bootloaders, NAT/gvproxy/vmnet-helper networking, REST state API: <https://github.com/crc-org/vfkit/blob/main/doc/usage.md>
- Lima vmtype (VZ default since v1.0, no cross-arch): <https://lima-vm.io/docs/config/vmtype/> and <https://lima-vm.io/docs/config/vmtype/vz/>
- lima-vm/socket_vmnet — vmnet shared/bridged helper: <https://github.com/lima-vm/socket_vmnet>
- cirruslabs/tart repo — Virtualization.framework, Fair Source, v2.33.0, --net-bridged/--net-softnet: <https://github.com/cirruslabs/tart>
- tart quick start — `tart create --linux`, macOS 13+: <https://tart.run/quick-start/>
