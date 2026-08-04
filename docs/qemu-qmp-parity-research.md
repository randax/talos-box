# QEMU/QMP parity for the talos-box vz backend

**Date of research: 2026-08-04. Every version claim below was verified against a primary source at that date; URLs are inline.**

Short answer: every capability talos-box uses today has a QEMU equivalent, and the *lifecycle, virtio-net, virtio-blk, virtio-rng, serial console and balloon* pieces are straightforward and old enough to exist in every distro QEMU we care about (all ≤ QEMU 1.2, except the balloon knobs we may want, which are 5.1). The two genuinely hard parts are (a) **suspend/resume**, where QEMU's closest analogue — `migrate` to a `file:` URI — needs QEMU **8.2+** and QEMU's *other* analogue, `snapshot-save`/`snapshot-load` (6.0+), requires at least one writable **qcow2** disk, which talos-box does not have (it uses raw sparse `.img` files); and (b) **networking**, where vz + Apple's `vmnet` gives talos-box a free NAT'd /24 with a host-readable DHCP lease file (`/var/db/dhcpd_leases`), and QEMU on Linux/Windows gives you nothing equivalent — you must pick between `user`/libslirp (optional at build time since 7.2), `passt` (netdev only since 10.1), or a privileged tap+dnsmasq setup, and none of them reproduce the "read the lease file to learn a node's IP by MAC" trick that `internal/vm/lease.go` depends on. Secondary risks: no built-in EFI firmware (you must ship/locate OVMF/AAVMF and a per-VM varstore), and `internal/imagecache` currently hardcodes `metal-arm64` Talos images, so an x86 Linux QEMU backend has no image source today. Ubuntu 22.04 LTS (QEMU 6.2) falls below the practical floor.

---

## 1. What talos-box actually uses from vz (grounding)

Read: `internal/vm/vm.go`, `internal/vm/console.go`, `internal/vm/lease.go`, `internal/vm/ring.go`, `internal/daemon/balloon.go`, `internal/daemon/suspend.go`, `internal/balloon/*`, plus `grep -r 'vz\.'` (all vz usage lives in `internal/vm/vm.go` — no other package touches the framework).

vz API surface, exhaustively:

| vz API | Where | Notes / behavioural assumption |
|---|---|---|
| `vz.NewVirtualMachineConfiguration(bootLoader, cpus, memoryBytes)` | `internal/vm/vm.go:52` | CPUs and memory fixed at create; memory in bytes = `MemoryMiB * 1024 * 1024` |
| `vz.NewEFIBootLoader` + `vz.NewEFIVariableStore` / `vz.WithCreatingEFIVariableStore` | `internal/vm/vm.go:131-150` | Per-VM EFI variable store file, created on first boot, reused after. Requires macOS 13+ ([Code-Hex/vz bootloader.go](https://github.com/Code-Hex/vz/blob/main/bootloader.go)) |
| `vz.NewDiskImageStorageDeviceAttachment(path, readOnly=false)` + `vz.NewVirtioBlockDeviceConfiguration` | `internal/vm/vm.go:153-161` | **Exactly one** disk, RAW format only ([pkg.go.dev](https://pkg.go.dev/github.com/Code-Hex/vz/v3#NewDiskImageStorageDeviceAttachment): "using a disk image in RAW format"). Image is a sparse file created by `os.Truncate` in `internal/cluster/disk.go:42,74` |
| `vz.NewFileHandleNetworkDeviceAttachment(file)` + `vz.NewVirtioNetworkDeviceConfiguration` + `vz.NewMACAddress` / `SetMACAddress` | `internal/vm/vm.go:163-180` | **Exactly one** NIC with a stable, cluster-assigned MAC. The `*os.File` is one end of a socketpair pumped by the privileged helper's `vmnet` interface (`internal/helper/vmnet_darwin.go`); the attachment expects a connected datagram socket |
| `vz.NewVirtioEntropyDeviceConfiguration` | `internal/vm/vm.go:182-186` | One virtio-rng |
| `vz.NewVirtioTraditionalMemoryBalloonDeviceConfiguration` | `internal/vm/vm.go:188-192` | One balloon |
| `machine.MemoryBalloonDevices()` → `*vz.VirtioTraditionalMemoryBalloonDevice` | `internal/vm/vm.go:92-95` | Grabbed **once** — repeated calls mint fresh finalized wrappers and over-release the objc object (repo issue #38) |
| `SetTargetVirtualMachineMemorySize(bytes)` | `internal/vm/vm.go:223` | "sets the target memory size in bytes for the virtual machine… The target memory size must be less than the total memory configured" ([memory_balloon.go](https://github.com/Code-Hex/vz/blob/main/memory_balloon.go)). i.e. **target = guest-visible RAM**, not balloon size |
| `vz.NewFileHandleSerialPortAttachment(read, write)` + `vz.NewVirtioConsoleDeviceSerialPortConfiguration` | `internal/vm/vm.go:194-202` | One **virtio-console** port, wired to two `os.Pipe()` pairs; the host side is proxied to a Unix socket with 64 KiB scrollback and a single-client lock (`internal/vm/console.go`) |
| `machineConfig.Validate()` | `internal/vm/vm.go:77` | — |
| `vz.NewVirtualMachine`, `Start`, `Pause`, `Resume`, `Stop`, `RequestStop`, `CanRequestStop`, `State`, `StateChangedNotify` | `internal/vm/vm.go:85,208,231,248,328,263-264,335,271` | Graceful stop = `RequestStop` then 30 s timeout then hard `Stop` |
| `SaveMachineStateToPath` / `RestoreMachineStateFromURL` | `internal/vm/vm.go:234,245` | macOS **14+**, and in Code-Hex/vz these live in `virtualization_arm64.go` — Apple-silicon only. Save requires the VM to be **paused**; restore requires the VM to be **Stopped** and leaves it **paused** ([virtualization_arm64.go](https://github.com/Code-Hex/vz/blob/main/virtualization_arm64.go)). Repo issue #37: restore fails with "invalid argument" against talos-box's device set, so resume in practice cold-boots (`internal/daemon/suspend.go:111-124`) |
| `vz.VirtualMachineState{Running,Stopped,Error}` | `internal/vm/vm.go:217,257,269-272,321,325` | Balloon target only settable while `Running` |

Behavioural assumptions the QEMU backend must reproduce:

1. **Balloon target semantics**: `internal/balloon/plan.go` computes a *target guest RAM size in MiB per node*, floored at `FloorMiB` (1024) and never above `ConfiguredMiB`. `internal/balloon/manager.go` re-applies targets every 5 s. Setting a target is fire-and-forget; nothing reads back the actual balloon size.
2. **Balloon safety gate**: `internal/daemon/balloon.go:16` only balloons nodes that apid-probe as `PhaseConfigured`, because maintenance-mode Talos has no `virtio_balloon` driver and "setting a target on one crashes vz".
3. **Suspend is whole-cluster, per-node file**: `<clusterdir>/<node>.vzstate`, VM closed afterwards, save file deleted on resume regardless of outcome (`internal/daemon/suspend.go:37,94`).
4. **Console is a reconnectable Unix socket** with scrollback replay and exactly one attached client.
5. **Node IP discovery is out-of-band**: `internal/vm/lease.go` parses `/var/db/dhcpd_leases` for `hw_address=1,<mac>` and validates the IP is `172.30.<subnetIndex>.2-179`. There is no guest agent and no in-band address channel.
6. **One of everything**: 1 disk, 1 NIC, 1 serial, 1 rng, 1 balloon. No hotplug anywhere.

---

## 2. Parity table

| vz capability | talos-box usage (file ref) | QEMU/QMP equivalent — amd64 | QEMU/QMP equivalent — arm64 | Min QEMU version | Parity risk |
|---|---|---|---|---|---|
| VM create (cpus/mem) | `vm.go:52` | `-machine q35 -accel kvm -smp N -m XM` | `-machine virt -accel kvm -cpu host -smp N -m XM` | any | none |
| EFI boot + per-VM NVRAM | `vm.go:131-150` | OVMF `-drive if=pflash,unit=0,readonly=on,file=OVMF_CODE.fd` + `unit=1,file=<vm>_VARS.fd` | AAVMF/edk2-aarch64 `-drive if=pflash,unit=0,readonly=on,file=AAVMF_CODE.fd` + `unit=1,file=<vm>_VARS.fd` | any (firmware is a distro package, not QEMU) | **Medium** — firmware file paths differ per distro; no built-in equivalent of `VZEFIVariableStore` |
| Start | `vm.go:208` | launch process; `-S` + QMP `cont` for controlled start | same | `cont` since 0.14 | none |
| Graceful stop | `vm.go:263` `RequestStop` | QMP `system_powerdown` | same | 0.14 | Low — ACPI-based; guest must respond |
| Force stop | `vm.go:328` `Stop` | QMP `quit` (or SIGKILL) | same | 0.14 | none |
| State | `vm.go:335` | QMP `query-status` + `STOP`/`RESUME`/`SHUTDOWN` events | same | 0.14 / 0.12 | none — richer than vz |
| Pause / Resume | `vm.go:231,248` | QMP `stop` / `cont` | same | 0.14 | none |
| virtio-blk, raw image | `vm.go:153-161` | `-blockdev driver=file,node-name=f0,filename=disk.img -blockdev driver=raw,node-name=d0,file=f0 -device virtio-blk-pci,drive=d0` | same (`virtio-blk-pci` on `virt`'s PCIe bridge) | any | none for boot; **raw blocks `snapshot-save`** (see §3.7) |
| virtio-net + fixed MAC | `vm.go:163-180` | `-device virtio-net-pci,netdev=n0,mac=…` + a netdev | same | any | none for the device; **High** for the backend (§3.3) |
| NAT'd /24 + DHCP + lease lookup | `vmnet_darwin.go`, `lease.go` | `-netdev user` (libslirp) / `-netdev passt` / tap+dnsmasq | same | user: any *if built*; passt netdev: **10.1**; stream/dgram: 7.2 | **High** (§3.3) |
| virtio-rng | `vm.go:182-186` | `-object rng-random,id=rng0,filename=/dev/urandom -device virtio-rng-pci,rng=rng0` | same | any | none |
| Serial console (virtio-console) | `vm.go:194-202`, `console.go` | `-chardev socket,id=con0,path=…,server=on,wait=off` + `-device virtio-serial-pci,id=vs0 -device virtconsole,chardev=con0`; or simply `-serial chardev:con0` (isa-serial) | same, but `-serial` on `virt` is **PL011**, not isa-serial | any | Low–Medium — Talos console kernel arg differs per transport (§3.5) |
| Memory balloon device | `vm.go:188-192` | `-device virtio-balloon-pci,id=balloon0[,deflate-on-oom=on][,free-page-reporting=on]` | same | device: any; `free-page-reporting`: **5.1** | Low |
| Set balloon target | `vm.go:223` | QMP `balloon` `{"value": <target bytes>}` — same semantics as vz | same | 0.14 | **None** (semantics match, §3.6) |
| Read balloon actual | *(not used — vz can't)* | QMP `query-balloon` + `BALLOON_CHANGE` event | same | 0.14 / 1.2 | improvement over vz |
| Suspend to file | `vm.go:230-238` | `stop` → `migrate` `{"channels":[{"channel-type":"main","addr":{"transport":"file","filename":"…"}}]}` → `query-migrate` | same | **8.2** (file transport) | **High** (§3.7) |
| Resume from file | `vm.go:244-252` | new process with `-incoming file:<path>` (or `-incoming defer` + `migrate-incoming`), then `cont` | same | **8.2** | **High** (§3.7) |
| (alternative) snapshot | — | QMP `snapshot-save` / `snapshot-load` / `snapshot-delete` | same | **6.0** | requires **qcow2** disk (§3.7) |
| Guest-driven S3 | *(not used)* | qemu-guest-agent `guest-suspend-ram` | same | qga 1.1 | n/a — Talos has no qemu-ga |
| Machine type enumeration | *(vz has none)* | QMP `query-machines` | same | `compat-props` arg since 9.1 | none |

---

## 3. Per-capability detail

### 3.1 Control channel

vz gives you an in-process object; QEMU gives you a socket. Start every VM with a QMP unix socket:

```
-qmp unix:/run/tbx/<cluster>/<node>.qmp,server=on,wait=off
```

`-qmp dev` is "like `-monitor` but opens in 'control' mode"; the `server=on,wait=off` form is the documented shape ([qemu-options.hx, `-qmp`](https://gitlab.com/qemu-project/qemu/-/blob/master/qemu-options.hx)). Handshake first:

```json
{"execute": "qmp_capabilities"}
```

`qmp_capabilities` — Since 0.13 ([qapi/control.json](https://gitlab.com/qemu-project/qemu/-/blob/master/qapi/control.json)).

### 3.2 Lifecycle

```json
{"execute": "query-status"}
{"return": {"running": true, "status": "running"}}
```
`query-status`/`StatusInfo` — Since 0.14; `RunState` includes `prelaunch`, `paused`, `running`, `inmigrate`, `postmigrate`, `save-vm`, `restore-vm`, `suspended`, `shutdown`, `guest-panicked` ([qapi/run-state.json](https://gitlab.com/qemu-project/qemu/-/blob/master/qapi/run-state.json)).

```json
{"execute": "stop"}      // vz Pause()  — Since 0.14
{"execute": "cont"}      // vz Resume() — Since 0.14
{"execute": "system_powerdown"}  // vz RequestStop() — Since 0.14
{"execute": "system_reset"}      // Since 0.14
{"execute": "quit"}              // vz Stop() — Since 0.14
```

`stop`/`cont` docs: [qapi/misc.json](https://gitlab.com/qemu-project/qemu/-/blob/master/qapi/misc.json); `system_reset`, `system_powerdown` (Since 0.14) and `system_wakeup` (Since 1.1) live in [qapi/machine.json](https://gitlab.com/qemu-project/qemu/-/blob/master/qapi/machine.json); `quit` in [qapi/control.json](https://gitlab.com/qemu-project/qemu/-/blob/master/qapi/control.json).

`system_powerdown` carries the same caveat as `RequestStop`: *"A guest may or may not respond to this command. This command returning does not indicate that a guest has accepted the request or that it has shut down."* ([qapi/machine.json](https://gitlab.com/qemu-project/qemu/-/blob/master/qapi/machine.json)). The existing 30 s timeout-then-force pattern in `vm.go:266-282` maps directly: `system_powerdown`, wait for the `SHUTDOWN` event (Since 0.12), else `quit`.

`vz.StateChangedNotify()` maps to the QMP event stream — `STOP`, `RESUME`, `SHUTDOWN` (all Since 0.12, [qapi/run-state.json](https://gitlab.com/qemu-project/qemu/-/blob/master/qapi/run-state.json)).

### 3.3 Networking — the biggest gap

vz + the talos-box helper give: a per-cluster shared-mode `vmnet` interface on `172.30.<idx>.0/24` with Apple's built-in DHCP server, packets pumped over a socketpair fd (`internal/helper/vmnet_darwin.go:176-184`), plus a host-readable lease file. On Linux/Windows none of that exists as one piece.

Device side is trivial and identical on both arches:

```
-device virtio-net-pci,netdev=n0,mac=52:54:00:12:34:56
```

Backend options, with verified minimum versions:

* **`-netdev user`** (libslirp): NAT + DHCP + DNS with no privilege. **Caveat:** the bundled slirp submodule was removed after 7.1 — `.gitmodules` at [v7.1.0](https://gitlab.com/qemu-project/qemu/-/blob/v7.1.0/.gitmodules) contains `[submodule "slirp"]`, and at v7.2.0 it does not. It is now an external dependency with meson feature default `auto`: `option('slirp', type: 'feature', value: 'auto', description: 'libslirp user mode network backend support')` ([meson_options.txt](https://gitlab.com/qemu-project/qemu/-/blob/master/meson_options.txt)), and the CLI itself is `#ifdef CONFIG_SLIRP`-gated ([qemu-options.hx](https://gitlab.com/qemu-project/qemu/-/blob/master/qemu-options.hx)). **So `-netdev user` may be absent from a given distro build and must be probed at runtime, not assumed.**
  ```
  -netdev user,id=n0,net=172.30.7.0/24,host=172.30.7.1,dhcpstart=172.30.7.2
  ```
* **`-netdev passt`**: *"Configure a passt network backend which requires no administrator privilege to run"*, spawning the `passt` binary ([qemu-options.hx](https://gitlab.com/qemu-project/qemu/-/blob/master/qemu-options.hx)). QAPI marks `@passt: since 10.1` ([qapi/net.json](https://gitlab.com/qemu-project/qemu/-/blob/master/qapi/net.json)); `net/passt.c` is absent at [v10.0.0](https://gitlab.com/qemu-project/qemu/-/blob/v10.0.0/net/passt.c) (404) and present at [v10.1.0](https://gitlab.com/qemu-project/qemu/-/blob/v10.1.0/net/passt.c). On older QEMU you can still drive passt manually through `-netdev stream,addr.type=unix,...` (stream/dgram: `@stream: since 7.2`, `@dgram: since 7.2`, [qapi/net.json](https://gitlab.com/qemu-project/qemu/-/blob/master/qapi/net.json)).
  ```
  -netdev passt,id=n0,address=172.30.7.2,netmask=255.255.255.0,gateway=172.30.7.1
  ```
* **tap** (`-netdev tap,id=n0,ifname=tbx7,script=no,downscript=no` or `fd=N`): needs CAP_NET_ADMIN / a setuid helper — i.e. the Linux analogue of today's `tbx-helper`. Pair with a bridge + dnsmasq to get DHCP **and a lease file** (`/var/lib/misc/dnsmasq.leases`), which is the only option that preserves the `lease.go` IP-discovery model.
* **fd hand-off (closest structural analogue to `NewFileHandleNetworkDeviceAttachment`)**: `-netdev stream,id=n0,addr.type=fd,addr.str=<fd>` or `-netdev dgram,id=n0,local.type=fd,local.str=<fd>` ([qemu-options.hx](https://gitlab.com/qemu-project/qemu/-/blob/master/qemu-options.hx)) — 7.2+.
* **macOS only** (relevant if the QEMU backend is also offered on macOS): `-netdev vmnet-shared,id=n0[,start-address=…,end-address=…,subnet-mask=…]` ([qemu-options.hx](https://gitlab.com/qemu-project/qemu/-/blob/master/qemu-options.hx)). `@vmnet-host/@vmnet-shared/@vmnet-bridged: since 7.1` ([qapi/net.json](https://gitlab.com/qemu-project/qemu/-/blob/master/qapi/net.json)); `net/vmnet-shared.c` is absent at [v7.0.0](https://gitlab.com/qemu-project/qemu/-/blob/v7.0.0/net/vmnet-shared.c) and present at [v7.1.0](https://gitlab.com/qemu-project/qemu/-/blob/v7.1.0/net/vmnet-shared.c). Notably this exposes exactly the same `start-address`/`end-address`/`subnet-mask` knobs talos-box already sets on the raw vmnet API, and it uses the same system DHCP server → the `/var/db/dhcpd_leases` lease lookup keeps working on macOS.

**Not verified:** whether `-netdev user`'s built-in DHCP server exposes leases in any host-readable form. I found no such mechanism in the docs; assume it does not.

### 3.4 virtio-blk

```
-blockdev driver=file,node-name=f0,filename=/…/cp-1.img,cache.direct=on \
-blockdev driver=raw,node-name=d0,file=f0 \
-device virtio-blk-pci,drive=d0,id=blk0,bootindex=0
```

Identical on both arches (`virt` provides a PCIe host bridge; see [docs/system/arm/virt](https://www.qemu.org/docs/master/system/arm/virt.html)). The legacy shorthand `-drive file=…,if=virtio,format=raw` also works. The existing sparse-`os.Truncate` provisioning in `internal/cluster/disk.go` needs no change for boot — only for snapshots (§3.7).

### 3.5 Serial console

talos-box uses `VZVirtioConsoleDeviceSerialPort`, i.e. **virtio-console** (guest device `/dev/hvc0`). Two QEMU shapes:

Virtio-console (byte-for-byte the same device class as vz):
```
-chardev socket,id=con0,path=/run/tbx/c1/cp-1.console,server=on,wait=off \
-device virtio-serial-pci,id=vser0 \
-device virtconsole,chardev=con0
```
`virtconsole` + `-chardev` example is in [qemu-options.hx](https://gitlab.com/qemu-project/qemu/-/blob/master/qemu-options.hx); the PCI transport type is `virtio-serial-pci` ([hw/virtio/virtio-serial-pci.c](https://gitlab.com/qemu-project/qemu/-/blob/master/hw/virtio/virtio-serial-pci.c)).

Platform serial (often easier because it matches Talos's default console kernel arg):
```
-serial chardev:con0            # amd64: isa-serial → ttyS0
-serial chardev:con0            # arm64 virt: PL011 → ttyAMA0
```
The `virt` board provides *"Either one or two PL011 UARTs for the NonSecure World"* ([docs/system/arm/virt](https://www.qemu.org/docs/master/system/arm/virt.html)).

Talos's kernel supports all three: `CONFIG_VIRTIO_CONSOLE=m`, `CONFIG_SERIAL_AMBA_PL011_CONSOLE=y` (arm64), `CONFIG_SERIAL_8250_CONSOLE=y` (amd64) — verified in [siderolabs/pkgs kernel/build/config-arm64](https://raw.githubusercontent.com/siderolabs/pkgs/main/kernel/build/config-arm64) and [config-amd64](https://raw.githubusercontent.com/siderolabs/pkgs/main/kernel/build/config-amd64).

The host side of `internal/vm/console.go` (Unix listener, single client, 64 KiB scrollback ring) is transport-agnostic and can be reused unchanged — except that QEMU's `-chardev socket,server=on` *is itself* the listener, so either (a) let QEMU listen and have tbxd dial it, keeping the proxy in front for scrollback/single-client, or (b) keep tbxd's listener and hand QEMU a pre-connected fd via `-chardev socket,fd=N`. Option (a) is simpler; option (b) preserves today's ownership model.

### 3.6 Memory balloon — semantics actually match

Device:
```
-device virtio-balloon-pci,id=balloon0,deflate-on-oom=on,free-page-reporting=on
```

Set a target (this is the direct replacement for `SetTargetVirtualMachineMemorySize`):
```json
{"execute": "balloon", "arguments": {"value": 2147483648}}
```

QEMU's definition: *"@value: the target logical size of the VM in bytes. We can deduce the size of the balloon using this formula: logical_vm_size = vm_ram_size - balloon_size"* ([qapi/machine.json](https://gitlab.com/qemu-project/qemu/-/blob/master/qapi/machine.json), Since 0.14). Code-Hex/vz: *"sets the target memory size in bytes for the virtual machine… The target memory size must be less than the total memory configured for the virtual machine"* ([memory_balloon.go](https://github.com/Code-Hex/vz/blob/main/memory_balloon.go)).

**Both are "target guest-visible RAM", not "balloon size".** So `internal/balloon/plan.go` needs no semantic change — only `targetMiB * 1024 * 1024` → the `value` argument. This is the cleanest 1:1 mapping in the whole exercise.

QEMU also gives things vz cannot:
```json
{"execute": "query-balloon"}
{"return": {"actual": 2147483648}}
```
`query-balloon`/`BalloonInfo` Since 0.14; errors `DeviceNotActive` *"If no balloon device is present"* and `KVMMissingCap` ([qapi/machine.json](https://gitlab.com/qemu-project/qemu/-/blob/master/qapi/machine.json)). Plus the `BALLOON_CHANGE` event (Since 1.2, same file). That turns today's fire-and-forget loop into a closed loop, and — importantly — replaces the fragile apid-probe gate in `internal/daemon/balloon.go`: if the guest has no `virtio_balloon` driver, QEMU returns a `DeviceNotActive` error instead of crashing the host process. **The vz-crash workaround can be deleted on the QEMU backend.**

Guest stats (vz has no analogue) via QOM:
```json
{"execute": "qom-set", "arguments": {"path": "/machine/peripheral/balloon0",
  "property": "guest-stats-polling-interval", "value": 2}}
{"execute": "qom-get", "arguments": {"path": "/machine/peripheral/balloon0",
  "property": "guest-stats"}}
```
`guest-stats` and `guest-stats-polling-interval` are `object_property_add`-ed in [hw/virtio/virtio-balloon.c](https://gitlab.com/qemu-project/qemu/-/blob/master/hw/virtio/virtio-balloon.c); `qom-get`/`qom-set` Since 1.2 ([qapi/qom.json](https://gitlab.com/qemu-project/qemu/-/blob/master/qapi/qom.json)).

Device properties, verbatim from [hw/virtio/virtio-balloon.c](https://gitlab.com/qemu-project/qemu/-/blob/master/hw/virtio/virtio-balloon.c):
```c
DEFINE_PROP_BIT("deflate-on-oom", ...  VIRTIO_BALLOON_F_DEFLATE_ON_OOM, false),
DEFINE_PROP_BIT("free-page-hint", ...  VIRTIO_BALLOON_F_FREE_PAGE_HINT, false),
DEFINE_PROP_BIT("page-poison",    ...  VIRTIO_BALLOON_F_PAGE_POISON,    true),
DEFINE_PROP_BIT("free-page-reporting", ... VIRTIO_BALLOON_F_REPORTING,  false),
```
`free-page-reporting` first appears in **5.1**: the string is absent from [v5.0.0/hw/virtio/virtio-balloon.c](https://gitlab.com/qemu-project/qemu/-/blob/v5.0.0/hw/virtio/virtio-balloon.c) and present at [v5.1.0](https://gitlab.com/qemu-project/qemu/-/blob/v5.1.0/hw/virtio/virtio-balloon.c) (the enabling commit *"virtio-balloon: Provide an interface for free page reporting"* is dated 2020-06-09, in the 5.1 cycle — [commit list](https://gitlab.com/qemu-project/qemu/-/commits/master/hw/virtio/virtio-balloon.c)). `deflate-on-oom` is present at v5.1.0; **I did not verify the exact release it was introduced in** — it predates 5.0, which is below every distro version in §4, so it does not matter in practice.

Recommended: set `deflate-on-oom=on` (matches the intent of the 1 GiB per-node floor — a guest under memory pressure can claw back), and `free-page-reporting=on` on QEMU ≥ 5.1 so idle guest pages are returned to the host without any explicit ballooning at all.

Guest driver: Talos ships `CONFIG_VIRTIO_BALLOON=m` on both arches ([config-arm64 line 7499](https://raw.githubusercontent.com/siderolabs/pkgs/main/kernel/build/config-arm64), [config-amd64 line 6083](https://raw.githubusercontent.com/siderolabs/pkgs/main/kernel/build/config-amd64)). It is a **module**, which is consistent with the observed "maintenance mode has no balloon driver" behaviour. **Not verified:** whether `virtio_balloon.ko` is present in / autoloaded from the Talos maintenance-mode initramfs.

### 3.7 Suspend / resume — the hard one

vz semantics to reproduce (`internal/vm/vm.go:230-252`, `internal/daemon/suspend.go`): pause → write RAM+device state to one file → close VM; later, create a fresh VM with the same config → restore → resume; disks are *not* part of the saved state and are left as-is.

**Option A — `migrate` to a file (recommended; closest semantics).**
```json
{"execute": "stop"}
{"execute": "migrate", "arguments": {
   "channels": [{"channel-type": "main",
                 "addr": {"transport": "file", "filename": "/…/cp-1.migstate"}}]}}
{"execute": "query-migrate"}      // poll until status == "completed"
{"execute": "quit"}
```
Resume:
```
qemu-system-aarch64 …same args… -incoming file:/…/cp-1.migstate
```
then `{"execute": "cont"}`.

`migrate` Since 0.14; `MigrationAddressType` includes `@file: Direct the migration stream to a file` with **Since: 8.2** ([qapi/migration.json](https://gitlab.com/qemu-project/qemu/-/blob/master/qapi/migration.json)). `-incoming file:filename[,offset=offset]` and `-incoming defer` are documented in [qemu-options.hx](https://gitlab.com/qemu-project/qemu/-/blob/master/qemu-options.hx). On QEMU < 8.2 the equivalent is `"exec:cat > file"` / `-incoming "exec:cat file"`, which is documented but shells out.

This matches vz exactly: RAM+device state only, disks untouched, raw images fine. Cost: **QEMU 8.2 minimum** for the clean form.

**Option B — internal snapshots.**
```json
{"execute": "snapshot-save", "arguments": {
   "job-id": "snapsave0", "tag": "tbx-suspend", "vmstate": "d0", "devices": ["d0"]}}
{"execute": "query-jobs"}         // poll until status == "concluded"
{"execute": "snapshot-load", "arguments": {
   "job-id": "snapload0", "tag": "tbx-suspend", "vmstate": "d0", "devices": ["d0"]}}
{"execute": "snapshot-delete", "arguments": {
   "job-id": "snapdel0", "tag": "tbx-suspend", "devices": ["d0"]}}
```
All three **Since: 6.0** ([qapi/migration.json](https://gitlab.com/qemu-project/qemu/-/blob/master/qapi/migration.json)). They are asynchronous jobs: *"Applications should not assume that the snapshot save is complete when this command returns. The job commands / events must be used to determine completion"* (same source).

**Blocker:** *"In order to use VM snapshots, you must have at least one non removable and writable block device using the qcow2 disk image format."* ([docs/system/images.rst](https://gitlab.com/qemu-project/qemu/-/blob/master/docs/system/images.rst)). talos-box disks are raw (`internal/cluster/disk.go`, `internal/imagecache/cache.go` → `disk.raw`). Adopting Option B means migrating node disks to qcow2, which also changes `internal/cluster/snapshot.go`'s file-copy/clone logic. Option B does buy something Option A does not: disk state is snapshotted too, so resume is genuinely consistent.

**Option C — guest S3 via qemu-guest-agent** (`guest-suspend-ram`, Since 1.1, [qga/qapi-schema.json](https://gitlab.com/qemu-project/qemu/-/blob/master/qga/qapi-schema.json)). Requires `qemu-guest-agent` inside the guest, which Talos does not ship. Also only suspends; QEMU keeps running. **Not applicable.**

Worth noting: because of repo issue #37, vz restore already fails and resume cold-boots today (`internal/daemon/suspend.go:111-124`). Option A on QEMU would likely be the *first working* suspend/resume talos-box has.

### 3.8 Machine types, accelerators, firmware

Enumerate what a given build supports:
```json
{"execute": "query-machines"}
```
`MachineInfo` carries `name`, `alias`, `is-default`, `cpu-max`, `deprecated`, `default-cpu-type`, `default-ram-id`, `acpi` (since 8.0), `compat-props` (since 9.1) ([qapi/machine.json](https://gitlab.com/qemu-project/qemu/-/blob/master/qapi/machine.json)).

| | amd64 | arm64 |
|---|---|---|
| binary | `qemu-system-x86_64` | `qemu-system-aarch64` |
| machine | `-machine q35` (modern PCIe; `pc`/i440fx is the legacy [i440fx board](https://www.qemu.org/docs/master/system/i386/pc.html)) | `-machine virt` — *"PCI/PCIe devices"*, *"Either one or two PL011 UARTs"*, *"32 virtio-mmio transport devices"*, flash for firmware ([docs](https://www.qemu.org/docs/master/system/arm/virt.html)) |
| CPU | `-cpu host` (KVM/HVF/WHPX) or `-cpu max` (TCG) | `-cpu host` required in practice — the `virt` default is `cortex-a15`, so 64-bit guests **must** set `-cpu`; *"VM migration is not guaranteed when using -cpu max, as features supported may change between QEMU versions"* ([docs](https://www.qemu.org/docs/master/system/arm/virt.html)) — relevant because suspend/resume is a migration stream |
| firmware | OVMF (`OVMF_CODE.fd` + per-VM `OVMF_VARS.fd` via `-drive if=pflash`) | AAVMF / `edk2-aarch64-code.fd` (WHPX doc example uses `-bios edk2-aarch64-code.fd` — [docs/system/whpx.rst](https://gitlab.com/qemu-project/qemu/-/blob/master/docs/system/whpx.rst)); use pflash pairs to get a writable varstore |
| accelerators | Linux `kvm`; macOS `hvf`; Windows `whpx`; NetBSD `nvmm`; fallback `tcg` | same set |

Accelerator list is verbatim from [qemu-options.hx](https://gitlab.com/qemu-project/qemu/-/blob/master/qemu-options.hx): *"select accelerator (kvm, xen, hvf, nitro, nvmm, whpx, mshv or tcg; use 'help' for a list)"*, and *"By default, tcg is used. If there is more than one accelerator specified, the next one is used if the previous one fails to initialize."* — so `-accel kvm:tcg` is a safe pattern, though TCG fallback would be unusably slow for a Talos cluster and should probably be a hard error instead.

Windows specifics ([docs/system/whpx.rst](https://gitlab.com/qemu-project/qemu/-/blob/master/docs/system/whpx.rst)):
* WHPX *"enables using QEMU with hardware acceleration on both x86_64 and arm64 Windows machines."*
* Requires the Windows Hypervisor Platform feature: `DISM /online /Enable-Feature /FeatureName:HypervisorPlatform /All`.
* x86_64: tested from Windows 10 version 2004. **arm64: Windows 11 24H2 with the April 2025 optional updates or May 2025 security updates is the minimum** — earlier 24H2 shipped a pre-release WHP API that QEMU does not support.
* Documented arm64 launch: `qemu-system-aarch64.exe -accel whpx -M virt -cpu host -smp cores=2 -m 2G -bios edk2-aarch64-code.fd …`
* Known x86_64 issue relevant to us: on Windows 10 the Hyper-V interrupt controller is disabled by default; `-M q35,pic=off` enables it and *"In that configuration, using a UEFI is recommended"* — which we do anyway.

---

## 4. Distro QEMU versions (verified 2026-08-04)

| Distro / channel | QEMU version | Source URL | Note |
|---|---|---|---|
| Ubuntu 22.04 LTS (jammy) | `1:6.2+dfsg-2ubuntu6.30` (updates: `…6.31`) | https://packages.ubuntu.com/search?keywords=qemu&searchon=sourcenames&suite=all&section=all | no qemu in jammy-backports (https://packages.ubuntu.com/search?keywords=qemu-system-x86&searchon=names&suite=jammy-backports&section=all → no results) |
| Ubuntu 24.04 LTS (noble) | `1:8.2.2+ds-0ubuntu1.16` (updates: `…1.17`) | https://packages.ubuntu.com/search?keywords=qemu&searchon=sourcenames&suite=all&section=all | no qemu in noble-backports |
| **Ubuntu 26.04 LTS (resolute) — released** | `1:10.2.1+ds-1ubuntu3` (updates: `…3.1`) | https://packages.ubuntu.com/source/resolute/qemu | page identifies the suite as "resolute (26.04LTS)" |
| Ubuntu 25.10 (questing) | `1:10.1.0+ds-5ubuntu2.6` | https://packages.ubuntu.com/search?keywords=qemu&searchon=sourcenames&suite=all&section=all | previous non-LTS |
| Fedora 44 | `10.2.2-1.fc44` | https://packages.fedoraproject.org/pkgs/qemu/qemu/ | GA tree live: https://dl.fedoraproject.org/pub/fedora/linux/releases/44/Everything/x86_64/os/ |
| Fedora 43 | `10.1.5-1.fc43` | https://packages.fedoraproject.org/pkgs/qemu/qemu/ | |
| Fedora Rawhide (F45) | `11.1.0-0.1.rc2.fc45` | https://packages.fedoraproject.org/pkgs/qemu/qemu/ | reference only |
| Arch (`qemu-base`, `qemu-full`, `qemu-system-x86`, …) | `11.0.3-1` | https://archlinux.org/packages/?q=qemu-base | repo `extra`, updated 2026-07-31 |
| nixpkgs `nixos-unstable` | `11.0.2` | https://raw.githubusercontent.com/NixOS/nixpkgs/nixos-unstable/pkgs/by-name/qe/qemu/package.nix | branch HEAD |
| nixpkgs `nixos-26.05` (current stable) | `10.2.4` | https://raw.githubusercontent.com/NixOS/nixpkgs/nixos-26.05/pkgs/by-name/qe/qemu/package.nix | channel confirmed live: `channels.nixos.org/nixos-26.05` → `releases.nixos.org/nixos/26.05/nixos-26.05.6815.531670d871c0` |

Feature availability against those versions:

| Feature | Min QEMU | 22.04 (6.2) | 24.04 (8.2) | 26.04 (10.2) | F43 (10.1) | F44 (10.2) | Arch (11.0) | nix stable (10.2) / unstable (11.0) |
|---|---|---|---|---|---|---|---|---|
| lifecycle, balloon set/query, virtio devices | ≤1.2 | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| `free-page-reporting` | 5.1 | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| `snapshot-save`/`-load`/`-delete` | 6.0 | ✅ (needs qcow2) | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| `-netdev stream/dgram` (fd hand-off) | 7.2 | ❌ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| `migrate` `file:` transport / `-incoming file:` | 8.2 | ❌ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| `query-machines` `compat-props` | 9.1 | ❌ | ❌ | ✅ | ✅ | ✅ | ✅ | ✅ |
| `-netdev passt` | 10.1 | ❌ | ❌ | ✅ | ✅ | ✅ | ✅ | ✅ |

**Not verified:** whether any given distro build enables `CONFIG_SLIRP` (i.e. ships `-netdev user`). Debian/Ubuntu and Fedora historically do, but since the meson option is `auto` this must be probed at runtime (`query-command-line-options`, or simply attempting the netdev) rather than assumed. Also not verified: `-security` pocket and `-proposed` versions for Ubuntu.

---

## 5. Parity risks, ranked

1. **Networking + IP discovery (highest).** vz/vmnet gives NAT, DHCP, a deterministic per-cluster `172.30.<idx>.0/24`, and a *host-readable lease file* that `internal/vm/lease.go` mines to map MAC → IP. QEMU has no single backend that provides all four. `-netdev user` provides NAT+DHCP but is (a) optional at build time since 7.2 (libslirp is an external `auto` dependency, [meson_options.txt](https://gitlab.com/qemu-project/qemu/-/blob/master/meson_options.txt)) and (b) has no lease file. `passt` is netdev-native only from 10.1. A bridge + tap + dnsmasq preserves the lease-file model but reintroduces a privileged helper on Linux — the same architecture as today's `tbx-helper`, which is arguably the right call. **Recommendation: make node IP assignment explicit (static per-MAC DHCP reservations or `-netdev passt,address=`) rather than porting the lease-file scrape.**
2. **Suspend/resume (high).** `migrate`+`file:` is the semantic match but needs **8.2**, ruling out Ubuntu 22.04. `snapshot-save` reaches back to 6.0 but requires a **qcow2** disk ([docs/system/images.rst](https://gitlab.com/qemu-project/qemu/-/blob/master/docs/system/images.rst)) — talos-box uses raw sparse images end to end. Also note `-cpu max` is explicitly called out as migration-unsafe across QEMU versions ([virt docs](https://www.qemu.org/docs/master/system/arm/virt.html)), so a QEMU backend that saves state must pin `-cpu host` (or a named model) and refuse to resume a save produced by a different QEMU version. **Practically: set the floor at QEMU 8.2 and use `migrate file:`.**
3. **Ubuntu 22.04 is below the floor (high, but easily mitigated by policy).** QEMU 6.2 lacks `file:` migration, `stream`/`dgram` netdevs and `passt`. If 22.04 must be supported, suspend/resume degrades to `exec:cat` migration or is disabled entirely.
4. **Guest image architecture (high — repo-side, not QEMU-side).** `internal/imagecache/cache.go` hardcodes `metal-arm64.raw.xz` at lines 113/115/214. An x86_64 Linux QEMU backend has no Talos image source today; the cache layout (`<schematic>/<version>/disk.raw`) is also arch-blind, so this needs an arch dimension before any amd64 work.
5. **EFI firmware and NVRAM (medium).** vz bundles the firmware and gives a first-class `VZEFIVariableStore`. QEMU needs distro-provided OVMF/AAVMF plus a per-VM copy of the VARS file wired through `-drive if=pflash`. Paths differ across Ubuntu (`/usr/share/OVMF/…`, `/usr/share/AAVMF/…`), Fedora (`/usr/share/edk2/…`), Arch and nixpkgs. Needs a firmware-discovery probe (`tbx doctor` is the natural home, given it already does corporate-environment diagnostics).
6. **Console transport choice (medium-low).** virtio-console (`virtconsole` on `virtio-serial-pci`) is a true 1:1 with vz and Talos supports it (`CONFIG_VIRTIO_CONSOLE=m`), but the platform UART (`ttyS0` on q35, `ttyAMA0` on `virt`) is the better-trodden path for Talos and is built in (`=y`) rather than a module. Whichever is chosen, the Talos kernel `console=` argument must match, and the existing scrollback/single-client proxy needs a small rework because `-chardev socket,server=on` makes QEMU the listener.
7. **Balloon (low — actually a net improvement).** Target semantics are identical (target = guest-visible RAM), so `internal/balloon/plan.go` ports unchanged. QEMU adds `query-balloon` and `BALLOON_CHANGE` for closed-loop control, and returns a clean `DeviceNotActive` error where vz crashes the host process — meaning the maintenance-mode apid-probe gate in `internal/daemon/balloon.go` and the "grab the device once" workaround for issue #38 both become unnecessary on this backend. Remaining risk: `CONFIG_VIRTIO_BALLOON=m` means the driver may not be loaded in Talos maintenance mode (unverified), so the manager should still tolerate per-node errors, as it already does (`manager.go:35-37`).
8. **Accelerator availability on Windows arm64 (low but sharp).** WHPX on arm64 requires **Windows 11 24H2 + April 2025 optional / May 2025 security updates**; earlier 24H2 builds shipped a pre-release WHP API QEMU rejects ([docs/system/whpx.rst](https://gitlab.com/qemu-project/qemu/-/blob/master/docs/system/whpx.rst)). Also SVE/SME are unsupported under WHPX arm64. Worth a preflight check.
9. **No `Validate()` equivalent (low).** vz's `machineConfig.Validate()` catches bad configurations before launch; QEMU tells you by failing to start. Mitigate by probing `query-machines`, `-accel help`, and `query-command-line-options` at daemon start and caching the result.

---

## 6. Sources

QEMU (primary — QAPI schema and source at `master` unless a tag is given):
* QMP command reference: https://www.qemu.org/docs/master/interop/qemu-qmp-ref.html
* `qapi/run-state.json` (query-status, RunState, STOP/RESUME/SHUTDOWN): https://gitlab.com/qemu-project/qemu/-/blob/master/qapi/run-state.json
* `qapi/misc.json` (stop, cont): https://gitlab.com/qemu-project/qemu/-/blob/master/qapi/misc.json
* `qapi/control.json` (qmp_capabilities, quit): https://gitlab.com/qemu-project/qemu/-/blob/master/qapi/control.json
* `qapi/machine.json` (balloon, query-balloon, BALLOON_CHANGE, query-machines, system_reset, system_powerdown, system_wakeup): https://gitlab.com/qemu-project/qemu/-/blob/master/qapi/machine.json
* `qapi/migration.json` (snapshot-save/load/delete Since 6.0; MigrationAddressType `file` Since 8.2): https://gitlab.com/qemu-project/qemu/-/blob/master/qapi/migration.json
* `qapi/qom.json` (qom-get/qom-set Since 1.2): https://gitlab.com/qemu-project/qemu/-/blob/master/qapi/qom.json
* `qapi/net.json` (`@vmnet-* : since 7.1`, `@stream/@dgram: since 7.2`, `@passt: since 10.1`): https://gitlab.com/qemu-project/qemu/-/blob/master/qapi/net.json
* `qga/qapi-schema.json` (guest-suspend-ram Since 1.1): https://gitlab.com/qemu-project/qemu/-/blob/master/qga/qapi-schema.json
* `hw/virtio/virtio-balloon.c` (deflate-on-oom, free-page-reporting, guest-stats): https://gitlab.com/qemu-project/qemu/-/blob/master/hw/virtio/virtio-balloon.c — absent at v5.0.0 / present at v5.1.0: https://gitlab.com/qemu-project/qemu/-/blob/v5.1.0/hw/virtio/virtio-balloon.c
* `hw/virtio/virtio-serial-pci.c`: https://gitlab.com/qemu-project/qemu/-/blob/master/hw/virtio/virtio-serial-pci.c
* `net/vmnet-shared.c` — 404 at v7.0.0, 200 at v7.1.0: https://gitlab.com/qemu-project/qemu/-/blob/v7.1.0/net/vmnet-shared.c
* `net/passt.c` — 404 at v10.0.0, 200 at v10.1.0: https://gitlab.com/qemu-project/qemu/-/blob/v10.1.0/net/passt.c
* `.gitmodules` slirp submodule at v7.1.0 (removed by v7.2.0): https://gitlab.com/qemu-project/qemu/-/blob/v7.1.0/.gitmodules
* `meson_options.txt` (`slirp`/`passt`/`vmnet` = `auto` features): https://gitlab.com/qemu-project/qemu/-/blob/master/meson_options.txt
* `qemu-options.hx` (-accel, -qmp, -serial, -chardev, -netdev *, -incoming, virtconsole): https://gitlab.com/qemu-project/qemu/-/blob/master/qemu-options.hx
* `docs/system/images.rst` (VM snapshots require qcow2): https://gitlab.com/qemu-project/qemu/-/blob/master/docs/system/images.rst
* `docs/system/whpx.rst`: https://gitlab.com/qemu-project/qemu/-/blob/master/docs/system/whpx.rst
* `docs/system/arm/virt`: https://www.qemu.org/docs/master/system/arm/virt.html
* `docs/system/i386/pc`: https://www.qemu.org/docs/master/system/i386/pc.html

Apple Virtualization.framework / Code-Hex/vz:
* https://pkg.go.dev/github.com/Code-Hex/vz/v3
* `memory_balloon.go` (target semantics, macOS 11+): https://github.com/Code-Hex/vz/blob/main/memory_balloon.go
* `virtualization_arm64.go` (SaveMachineStateToPath / RestoreMachineStateFromURL, macOS 14+): https://github.com/Code-Hex/vz/blob/main/virtualization_arm64.go
* Apple docs referenced from the binding: https://developer.apple.com/documentation/virtualization/vzvirtiotraditionalmemoryballoondevice

Talos guest kernel:
* https://raw.githubusercontent.com/siderolabs/pkgs/main/kernel/build/config-arm64
* https://raw.githubusercontent.com/siderolabs/pkgs/main/kernel/build/config-amd64

Distro packages: see the URLs in the §4 table.

Explicitly **not verified** (do not treat as fact):
* Whether Ubuntu/Fedora/Arch/nixpkgs QEMU builds enable `CONFIG_SLIRP` (`-netdev user`).
* Whether `-netdev user`'s DHCP server exposes leases in any host-readable file.
* The exact QEMU release that introduced `deflate-on-oom` (present by 5.1; predates all distro versions here).
* Whether `virtio_balloon.ko` is loaded in Talos maintenance mode.
* QEMU wiki ChangeLog pages (wiki.qemu.org) were unreachable — behind an Anubis challenge — so all version-introduced claims above were derived from the QAPI `Since:` annotations and from file presence/absence at release tags instead.
