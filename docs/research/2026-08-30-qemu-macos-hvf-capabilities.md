# QEMU on macOS/HVF: firmware, balloon readback, save-state and version floor

Research for [#522](https://github.com/randax/talos-box/issues/522) (map [#520](https://github.com/randax/talos-box/issues/520)). Date: 2026-08-30.

**Bottom line: a QEMU/HVF backend on macOS can do everything the Linux/KVM backend
does today — balloon readback *and* suspend/resume — which VZ cannot.** Every
capability in question was verified by running QEMU 11.1.1 under `-accel hvf` on
this machine, not just read out of source.

Sources are either upstream source/manifests (linked inline) or first-hand
measurement on the host described below. Where a claim is inference rather than
observation it is labelled as such.

## Test host

| | |
|---|---|
| Hardware | Apple Silicon (arm64) |
| macOS | 26.6.2 (build 25G83) |
| QEMU | 11.1.1, Homebrew bottle, `/opt/homebrew/opt/qemu` |
| `sysctl kern.hv_support` | `1` |
| `qemu-system-aarch64 -accel help` | `hvf`, `tcg` |

Reproduction scripts are not committed; each result below quotes the exact
command and its output.

---

## 1. EDK2/OVMF firmware: which images, where, and do EFI vars persist?

### What Homebrew installs

`brew --prefix qemu` → `/opt/homebrew/opt/qemu`. Firmware lands in
`share/qemu/`, exactly the upstream default set — the formula has no
firmware-specific install logic at all and delegates to QEMU's Meson defaults
(`install_blobs=true`).
Sources: [qemu.rb](https://raw.githubusercontent.com/Homebrew/homebrew-core/master/Formula/q/qemu.rb),
[pc-bios/meson.build](https://gitlab.com/qemu-project/qemu/-/raw/master/pc-bios/meson.build).

Observed (`ls -l /opt/homebrew/opt/qemu/share/qemu/edk2-*.fd`), the entries that matter:

```
67108864  edk2-aarch64-code.fd     <- 64 MiB
67108864  edk2-arm-vars.fd         <- 64 MiB
 3653632  edk2-x86_64-code.fd
  540672  edk2-i386-vars.fd
```

### There is no `edk2-aarch64-vars.fd`, and that is correct

The aarch64 code volume is paired with the **32-bit ARM** vars template. This is
not a packaging accident — it is what QEMU's own firmware descriptor says. The
installed `share/qemu/firmware/60-edk2-aarch64.json` on this host reads:

```json
"executable":     { "filename": ".../share/qemu/edk2-aarch64-code.fd", "format": "raw" },
"nvram-template": { "filename": ".../share/qemu/edk2-arm-vars.fd",     "format": "raw" }
```

matching upstream
[`pc-bios/descriptors/60-edk2-aarch64.json`](https://gitlab.com/qemu-project/qemu/-/raw/master/pc-bios/descriptors/60-edk2-aarch64.json).
The vars store is a blank, architecture-independent NVRAM region, so one
template serves both `edk2-arm-code.fd` and `edk2-aarch64-code.fd`.

This is the same pairing `qemuFirmwareCandidates` already uses for NixOS in
`internal/hypervisor/qemu_config.go:161`
(`edk2-aarch64-code.fd` + `edk2-arm-vars.fd`), so the existing candidate-list
shape ports to macOS unchanged — only the prefix differs.

### Both halves must be 64 MiB

`hw/arm/virt.c` maps the single `VIRT_FLASH` region (`0x08000000` = 128 MiB) in
half across two `TYPE_PFLASH_CFI01` devices — `flashsize = memmap[VIRT_FLASH].size / 2`
— so each pflash device is fixed at 64 MiB regardless of the backing file. Unit 0
is mapped only into `secure_sysmem` (code); unit 1 is visible to both worlds
(vars).
Source: [hw/arm/virt.c](https://gitlab.com/qemu-project/qemu/-/raw/master/hw/arm/virt.c).

Both Homebrew blobs are already exactly 67108864 bytes, so no padding step is
needed — unlike some Linux distro AAVMF images. nixpkgs' standalone `OVMF`
package does its own `truncate -s 64M` for ARM/AArch64 for the same reason
([OVMF/default.nix](https://raw.githubusercontent.com/NixOS/nixpkgs/master/pkgs/applications/virtualization/OVMF/default.nix)).

### Persistent EFI vars: verified working

Copied `edk2-arm-vars.fd` to a per-VM file, booted `virt` under `-accel hvf`
with the pair on pflash unit 0/1, let EDK2 reach the UEFI shell, then compared:

```
before: sha256 b3b855c5a8031016   size 67108864
after:  sha256 c676c9b7ee2f9bb4   size 67108864
```

The vars file was written in place by the firmware and kept its size. Serial log
confirms EDK2 reached `Shell>`. So the existing `ensureQEMUVars` copy-template-per-VM
approach (`qemu_config.go:~170`) works as-is on macOS.

### nixpkgs on darwin

`pkgs/by-name/qe/qemu/package.nix` (the old
`pkgs/applications/virtualization/qemu/default.nix` path now 404s) sets
`--enable-hvf` unconditionally on darwin — there is no `hvfSupport` toggle. It
performs no firmware customization, so it inherits the same upstream blob set at
`${pkgs.qemu}/share/qemu/`.
Source: [package.nix](https://raw.githubusercontent.com/NixOS/nixpkgs/master/pkgs/by-name/qe/qemu/package.nix).
*(Inference from the derivation source; not verified against a built store path —
Nix is not installed on the test host.)*

### Practical note for firmware discovery

The Homebrew-generated descriptor JSON contains **versioned Cellar paths**:

```
/opt/homebrew/Cellar/qemu/11.1.1/share/qemu/edk2-aarch64-code.fd
```

These break on every QEMU upgrade. Discovery should either use the stable
`$(brew --prefix qemu)/share/qemu/` symlink or parse
`share/qemu/firmware/60-edk2-aarch64.json` at runtime rather than hardcoding a
path — the latter is the portable option that also covers nixpkgs and any
future prefix.

---

## 2. virtio-balloon readback under HVF: **works** (the VZ blocker does not apply)

This is the headline result. `internal/hypervisor/backend_vz_darwin.go:57` records
that Virtualization.framework "does not report the guest-visible balloon size".
QEMU/HVF does.

Measured on the host, `virtio-balloon-pci` on `virt` + `-accel hvf`:

```
{"execute":"query-balloon"}  ->  {"return": {"actual": 2147483648}}
{"execute":"query-kvm"}      ->  {"return": {"enabled": false, "present": false}}
```

`query-balloon` answers normally with `enabled:false, present:false` for KVM —
i.e. under HVF, with no KVM anywhere in the picture. The exact device line the
Linux backend builds (`qemu_config.go:306-311`) is accepted verbatim under HVF:

```
-device virtio-balloon-pci,deflate-on-oom=on,free-page-reporting=on
```

`free-page-hint=on` still requires an iothread (`'free-page-hint' requires 'iothread' to be set`),
same as on Linux — an upstream device constraint, not an HVF one.

**Why this is accelerator-independent, from source:**
[`hw/virtio/virtio-balloon.c`](https://gitlab.com/qemu-project/qemu/-/raw/master/hw/virtio/virtio-balloon.c)
contains no `kvm`/`hvf`/`tcg` conditionals. The readback path is entirely
device-level: the guest driver writes its accepted size into virtio config space
(`virtio_balloon_set_config`: `dev->actual = le32_to_cpu(config.actual)`), and
QMP derives the reported figure from it:

```c
info->actual = get_current_ram_size() - ((uint64_t) dev->actual << VIRTIO_BALLOON_PFN_SHIFT);
```

`qapi/machine.json` documents `BalloonInfo.actual` as "the logical size of the VM
in bytes", and `query-balloon` returning `DeviceNotActive` when no balloon exists.
The only accelerator-conditional code in `system/balloon.c` is a `kvm_enabled() &&
!kvm_has_sync_mmu()` guard, which is simply false under HVF.

**Caveat on the measured number.** `actual` above equals full RAM because no
guest driver was attached (the VM sat in the UEFI shell); QEMU initialises
`actual` to the configured RAM size. Confirming that `actual` *tracks a real
Talos guest's driver* still needs an end-to-end run with a Talos image — the
mechanism is proven, the guest-side loop is not yet exercised on macOS. That
said, since the path has no accelerator branch, there is no plausible mechanism
by which HVF would diverge.

---

## 3. Save/restore under HVF: **full round-trip verified**

### Result

`migrate` to file and `-incoming file:` both work under `-accel hvf` on aarch64.

Save (2 GiB guest, `stop` then migrate):

```
{"execute":"migrate","arguments":{"uri":"file:...save.bin,offset=4096"}} -> {"return": {}}
query-migrate -> "status": "completed", "downtime": 6, "total-time": 633,
                 "transferred": 71377464, "mbps": 903.5
query-status  -> "postmigrate"
```

Restore, fresh QEMU process with `-S -incoming file:...,offset=4096`:

```
query-migrate -> "completed"
query-status  -> "paused"
{"execute":"cont"} -> {"return": {}}
query-status  -> "running"
```

So `qemu_save.go`'s design — a metadata header in a reserved page-aligned
prefix, then QEMU's file transport writing at an offset, fed back as `-incoming`
on the next launch — transfers to macOS intact. `SuspendSurvivesDaemonRestart`
(`qemu_probe.go:46`) holds for the same reason it does on Linux: no handle from
the writing process is involved.

### Source corroboration

No `migrate_add_blocker` exists in
[`accel/hvf/hvf-accel-ops.c`](https://gitlab.com/qemu-project/qemu/-/raw/master/accel/hvf/hvf-accel-ops.c)
or [`target/arm/hvf/hvf.c`](https://gitlab.com/qemu-project/qemu/-/raw/master/target/arm/hvf/hvf.c).
HVF implements the CPUAccelOps sync hooks the generic migration machinery needs
(`hvf_cpu_synchronize_state`, `_post_reset`, `_post_init`, `_pre_loadvm`), and
`target/arm/hvf/hvf.c` registers a dedicated `vmstate_hvf_vtimer` plus a
VM-state-change handler to recompute the virtual-timer offset across stop/resume.

### GIC state is Apple's in-kernel vGIC, and it is save/restore-aware

Worth flagging because it is easy to assume HVF uses a QEMU-emulated GIC (it does
not, on current macOS). `strings` on the shipped binary shows Apple `hv_gic_*`
API use with explicit state serialization:

```
hvf: vgic: failed to get GIC state size.
hvf: vgic: failed to get GIC state.
hvf: vgic: failed to restore GIC state.
hvf: vgic: failed to create hv_gic_state_create.
hv_gic_get_distributor_reg(...), hv_gic_get_redistributor_reg(...), hv_gic_get_icc_reg(...)
```

That save/restore support existing is precisely what makes the round-trip above
work. It also constrains configuration — see §5.

### Known bug, and how relevant it is

[qemu#1893](https://gitlab.com/qemu-project/qemu/-/issues/1893) (macOS 13.5.2,
arm64, QEMU 8.1.0, `-accel hvf`): monitor `savevm` asserts
`qemu_get_current_aio_context() == qemu_get_aio_context()` in `bdrv_poll_co`.
Closed 2024-02-09, labelled `Storage`. This is a block-layer/AIO assertion class,
not HVF-specific, and it is the `savevm` **snapshot-into-qcow2** path — not the
`migrate`-to-file path talos-box uses. Our path was exercised successfully above
on 11.1.1. Low risk, but a reason to keep using `migrate` rather than `savevm`.

---

## 4. Bug found: the repo's `migrate` offset is a hex **string**, which real QEMU rejects

Not an HVF question, but it surfaced while validating the production call path
and it affects Linux too.

`internal/hypervisor/qemu_backend.go:482-491` sends:

```go
"offset": fmt.Sprintf("0x%x", qemuSaveOffset),   // qemuSaveOffset = 1 << 20
```

Against real QEMU 11.1.1, using the repo's exact `channels` payload:

```
"offset": "0x100000"  -> {"error": {"class": "GenericError",
                          "desc": "Parameter 'channels[0].addr.offset' expects uint64"}}
"offset": 4096        -> {"return": {}}   ... query-migrate -> "completed"
```

QMP's `FileMigrationArgs.offset` is `uint64`; the QObject input visitor in plain
QMP mode will not coerce a `QString`. `internal/hypervisor/qemu_backend_test.go:346`
decodes the field as `Offset string`, so the in-repo fake QMP server accepts what
real QEMU refuses — the unit tests cannot catch this.

**This needs verifying against the QEMU version the Linux e2e actually runs**
(the floor is 8.2 for suspend per `qemu_probe.go:14`). Either older QEMU accepted
the string and 11.x tightened it, or the suspend path has been broken against
real QEMU and only the fake covers it. Worth its own ticket either way; the fix
is to send the offset as a number.

---

## 5. Version floor and configuration constraints

### QEMU

| | |
|---|---|
| aarch64 HVF support merged | QEMU **6.2.0** ([release announcement](https://www.qemu.org/2021/12/14/qemu-6-2-0/): "macOS hosts with Apple Silicon CPUs now support 'hvf' accelerator for AArch64 guests") |
| `migrate` `channels` argument | 8.2 (already the repo's suspend floor, `qemu_probe.go:14`) |
| Verified working here | 11.1.1 |

`qemuMinimumVersion` is 6.2 and `qemuSuspendVersion` is 8.2 — both already
correct for macOS; no new floor is needed on the QEMU side.

### macOS — the real floor is set by Homebrew, not by QEMU

Homebrew explicitly builds **without** HVF on Apple Silicon Sonoma and earlier:

```ruby
# The arm64 HVF backend needs the macOS 15 SDK for its EL2 sysregs and vGIC
args << "--disable-hvf" if OS.mac? && Hardware::CPU.arm? && MacOS.version <= :sonoma
```
([qemu.rb](https://raw.githubusercontent.com/Homebrew/homebrew-core/master/Formula/q/qemu.rb))

So with the current formula, **`brew install qemu` on macOS 14 or earlier gives an
Apple Silicon binary with no HVF at all**. Bottles exist for `arm64_sonoma`,
`arm64_sequoia`, `arm64_tahoe` — the Sonoma bottle is the HVF-less one. The
effective floor for a Homebrew-provisioned QEMU/HVF backend is **macOS 15
(Sequoia)**. Apple's underlying arm64 vCPU API surface (`hv_vm_create(_:)` config
overload, `hv_vcpu_create`) is `introducedAt: 11.0`, so the constraint is
Homebrew's build, not the OS.

### Configuration constraints under HVF (all measured)

**CPU model — `-cpu host` is effectively mandatory:**

```
-cpu cortex-a72  -> Invalid CPU model: cortex-a72
                    The valid models are: cortex-a53, cortex-a57, host, max
-cpu host        -> starts
-cpu max         -> starts
```

The Linux backend already passes `-cpu host` (`qemu_config.go:~252`), so no change.

**GIC — GICv3 only:**

```
gic-version=2     -> HVF does not support GICv2 emulation
gic-version=3     -> starts
gic-version=4     -> HVF does not support GICv4 emulation, is virtualization=on?
gic-version=max   -> starts
gic-version=host  -> gic-version=host requires KVM
```

Note the last line: `gic-version=host` is a **KVM-only** value. If the config
builder ever grows a `gic-version` argument for Linux, it must not be shared with
the macOS path. The default (unspecified) works under HVF and selects v3.

**Intel Macs:** `qemu-system-x86_64 -accel hvf` on this Apple Silicon host returns
`invalid accelerator hvf` — the x86 HVF backend is simply not compiled into an
arm64 build. x86-on-Intel-Mac HVF exists upstream (since QEMU 2.12) but could not
be tested here; treat it as untested rather than unsupported.

---

## 6. Detecting HVF availability, and what failure looks like

### Two independent things must both hold

**(a) The CPU/OS supports it.** `sysctl kern.hv_support` → `1` on this host.
This is the conventional check, but note it is **not in Apple's formal API
reference** — the only Apple-sourced description is a DTS engineer's forum answer
([thread 97409](https://developer.apple.com/forums/thread/97409)) explaining that
it reflects OS-level hypervisor infrastructure rather than raw CPU virtualization
extensions. Apple's documented mechanism is instead to call `hv_vm_create` and
inspect the `hv_return_t`. For a doctor-style preflight, `kern.hv_support` is the
pragmatic choice; it should be treated as a strong hint, with the authoritative
answer coming from actually launching QEMU.

`kern.hv_vmm_present` is a *different* sysctl (it was `0` here) — it reports
whether *this* machine is itself running under a VMM. Do not confuse the two.

**(b) The QEMU binary is signed with the hypervisor entitlement.** This is the
failure mode most likely to bite in practice:

```
$ codesign -d --entitlements - /opt/homebrew/opt/qemu/bin/qemu-system-aarch64
[Key] com.apple.security.hypervisor
[Value] [Bool] true
```

The Homebrew bottle carries it. A self-built or re-signed QEMU will not, and the
binary's own error string spells out the consequence:

```
HV_DENIED
Could not access HVF. Is the executable signed with com.apple.security.hypervisor entitlement?
```

The entitlement was introduced at macOS 11.0
([Apple entitlement reference](https://developer.apple.com/documentation/bundleresources/entitlements/com.apple.security.hypervisor)).

### Failure shapes to detect

| Condition | Observable |
|---|---|
| QEMU built without HVF (e.g. Homebrew on arm64 Sonoma) | `-accel hvf` → `invalid accelerator hvf`; `-accel help` lists only `tcg` |
| Binary lacks the entitlement | `HV_DENIED` / `Could not access HVF. Is the executable signed with com.apple.security.hypervisor entitlement?` |
| Host has no hypervisor support | `kern.hv_support` = 0 |

`-accel help` is the cheapest positive probe and distinguishes "not built in"
from "built in but denied" — the two need different remediation advice
(upgrade macOS / reinstall QEMU vs. fix code signing). Recommend adding it
alongside the existing `qemuProbe` version and machine-type queries.

---

## 7. Out of scope but noted: networking

`qemu_config.go` builds `-netdev tap,fd=N`. macOS has no tap device. The darwin
build offers `vmnet-host`, `vmnet-shared`, `vmnet-bridged` (from
`qemu-system-aarch64 -machine virt -netdev help`), all of which need root or the
`com.apple.vm.networking` entitlement and none of which take a pre-opened fd the
way the Linux path does. This is the largest porting gap and belongs to the
networking ticket, not this one.

---

## Summary against the ticket's five questions

1. **Firmware.** `edk2-aarch64-code.fd` + `edk2-arm-vars.fd`, both exactly 64 MiB,
   under `$(brew --prefix qemu)/share/qemu/` (same filenames on nixpkgs). No
   `edk2-aarch64-vars.fd` exists by design. Persistent EFI vars verified working.
   Discover via `share/qemu/firmware/60-edk2-aarch64.json` — Homebrew's copy
   hardcodes versioned Cellar paths, so don't cache them.
2. **Balloon readback.** Works under HVF. The device path has no accelerator
   branch; `query-balloon` verified returning `actual` with KVM absent. The VZ
   limitation does not carry over. Guest-driver-side tracking still wants a Talos
   e2e run.
3. **Save/restore.** Full `migrate`-to-file → `-incoming` round trip verified on
   11.1.1/HVF. No HVF migration blocker in source. Apple's in-kernel vGIC has
   explicit state save/restore. The one known `savevm` bug is a block-layer
   assertion on a path we don't use.
4. **Version floor.** QEMU 6.2 (HVF aarch64) / 8.2 (suspend) — both already the
   repo's floors. macOS **15 Sequoia** in practice, because Homebrew disables HVF
   on arm64 Sonoma and earlier.
5. **Detection.** `kern.hv_support` = 1 *and* `-accel help` lists `hvf` *and* the
   binary carries `com.apple.security.hypervisor`. Failures are
   `invalid accelerator hvf` (not built in) vs. `HV_DENIED` (not entitled).

Plus one incidental find: [§4](#4-bug-found-the-repos-migrate-offset-is-a-hex-string-which-real-qemu-rejects)
— the `migrate` offset is sent as a hex string that QEMU 11.1.1 rejects, masked
by a fake QMP server in the tests. Needs a ticket of its own.
