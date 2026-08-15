# Research: candidate curated extensions for the KVM/QEMU rig

Resolves #192 (part of #190). Feeds the "pick the initial curated list" grilling ticket (#193).

## Question

Which official Talos system extensions are plausible members of talos-box's initial
curated set — extensions users can toggle per cluster on top of the always-baked
`siderolabs/iscsi-tools` + `siderolabs/util-linux-tools` (`internal/imagecache/schematic.go`,
`requiredExtensions`)?

## Sources

- Official catalog: [siderolabs/extensions README](https://github.com/siderolabs/extensions)
  (also the source of the Image Factory's `officialExtensions` list — talos-box already
  builds schematics against it).
- Per-extension READMEs in that repo (notably `guest-agents/qemu-guest-agent`,
  `storage/nfs-utils`).
- Talos docs at talos.dev (system extensions, extension services).
- talos-box code: `internal/imagecache/schematic.go`, `internal/hypervisor/qemu_config.go`,
  `internal/daemon/protocol.go` (`DefaultTalosVersion = "v1.13.6"`).

## Constraints that shaped the shortlist

- **VM shape**: virtio-everything QEMU/KVM guests (Linux hosts) and Virtualization.framework
  guests (macOS hosts). No physical NICs, no GPUs, no NVMe, no exotic hardware — so the
  entire Firmware, DRM, Drivers, DVB, and NVIDIA categories are irrelevant or unverifiable.
- **Verifiability**: every curated extension must be checkable by the existing KVM cluster
  e2e in CI. The generic probe is `talosctl get extensions` (extension present in the image)
  plus `talosctl service ext-<name>` reaching `Running` for extensions that ship a service.
  A curated extension should additionally admit a cheap *functional* probe.
- **Cost**: no external accounts/secrets in CI (rules out immediate curation of Tailscale,
  ZeroTier, NetBird, cloudflared), no GPUs (rules out all `nvidia-*`, cost of a GPU runner
  plus passthrough plumbing dwarfs the value), no nested-virt plumbing (defers
  `kata-containers`, `qemu`).
- **Version pin**: talos-box defaults to Talos v1.13.6. All candidates below are published
  by the Image Factory for v1.13.x; the schematic API fails fast if an extension name is
  unknown for the pinned version, so version drift is caught at image-build time.

## Prime candidate: `siderolabs/qemu-guest-agent`

**What it does.** Runs the QEMU guest agent as an extension service
(`ext-qemu-guest-agent`). Gives the hypervisor guest-cooperative shutdown, guest IP
reporting, fs-freeze, and `guest-ping` — exactly the "is the guest actually alive and
what is its address" channel talos-box currently reconstructs from DHCP/ARP.

**QEMU requirement.** The agent waits for a virtio-serial port named
`org.qemu.guest_agent.0` (`/dev/virtio-ports/org.qemu.guest_agent.0` in the guest).

**Gap in talos-box today.** `internal/hypervisor/qemu_config.go` creates one
`virtio-serial-pci` controller with `max_ports=1`, consumed entirely by the `virtconsole`
(hvc0). There is **no guest-agent channel**. Wiring it up means: bump `max_ports` (or add
a second controller), add a `-chardev socket,id=qga0,path=<qga.sock>,server=on,wait=off`
and a `-device virtserialport,chardev=qga0,name=org.qemu.guest_agent.0`. Small, contained
change; the QMP socket plumbing in `qemu_backend.go` is the pattern to copy.

**Host asymmetry.** This only does anything on the Linux/QEMU backend. The macOS backend
is Virtualization.framework (`backend_vz_darwin.go`), where the extension's service will
sit in "waiting for device" forever (harmless, but the toggle is a no-op on macOS). The
curated-set UX must state per-extension host applicability.

**e2e verification.** On the KVM rig: enable the toggle, boot, then (a) `talosctl service
ext-qemu-guest-agent` is `Running`, (b) send `{"execute":"guest-ping"}` and
`guest-network-get-interfaces` over the qga UNIX socket and assert the reported IP matches
the cluster's view, (c) optionally issue `guest-shutdown` and assert clean VM exit. All
local, no external deps, seconds of wall time.

**Verdict: recommend** — contingent on the small hypervisor change to expose the channel.

## Shortlist

| Extension | What it does | e2e verification | Talos-version notes | Verdict |
|---|---|---|---|---|
| `qemu-guest-agent` | Guest agent: clean shutdown, IP reporting, fs-freeze, ping | `ext-qemu-guest-agent` Running + `guest-ping`/`guest-network-get-interfaces` over qga socket; needs virtserialport `org.qemu.guest_agent.0` added to `qemu_config.go` (today `max_ports=1`, console only) | Published for v1.13.x; no config needed | **Recommend** (Linux/QEMU hosts; no-op on macOS VZ) |
| `gvisor` | Sandboxed container runtime via containerd runtime handler | Apply `RuntimeClass runsc`, run a pod with it, assert `uname -r`/dmesg shows gVisor kernel | Needs matching containerd config Talos ships with the extension; stable across 1.13 | **Recommend** — cheap, fully local, exercises the runtime-handler class of extensions |
| `nfs-utils` | rpcbind + rpc.statd for NFSv3 mounts with locking (feeds NFS CSIs, Trident) | In-cluster NFS server pod + NFSv3 PV mount with a lock test; or minimal: both services Running | No config; services auto-start | **Recommend** — fits tbx's storage story; moderate but bounded e2e cost |
| `util-linux-tools` | Core Linux utilities | Already covered by Longhorn e2e | — | Already always-on (baseline, not curated) |
| `iscsi-tools` | iSCSI initiator | Already covered by Longhorn e2e | — | Already always-on (baseline, not curated) |
| `fuse3` | /dev/fuse + fusermount3 for FUSE-based CSIs (s3fs, gcsfuse, juicefs) | Pod that mounts a trivial FUSE fs and reads back | No service; presence check + functional mount | **Defer** — useful but no curated CSI consumes it yet; add when one does |
| `btrfs` | btrfs kernel modules + tools | Attach scratch disk, mkfs.btrfs from privileged pod, mount, write/read | Module must match pinned kernel — Factory handles this | **Defer** — no curated engine needs it; revisit if a btrfs-backed engine joins the CSI set |
| `crun` | Alternative OCI runtime handler | RuntimeClass + pod run | Trivial | **Defer** — verifiable but adds little over runc for tbx users |
| `spin` / `wasmedge` | Wasm runtime handlers | RuntimeClass + wasm workload | Fine on 1.13 | **Defer** — niche; same verification pattern as gvisor, add on demand |
| `tailscale` | Mesh VPN into the node network | Would need an auth key secret in CI + external control plane | ExtensionServiceConfig required | **Defer** — high user demand but e2e needs external account/secrets; revisit with a mock/headscale rig |
| `zerotier`, `netbird`, `nebula`, `newt`, `cloudflared` | Overlay/tunnel networking | All need external controllers or accounts | — | **Reject** for initial set — unverifiable locally |
| `lldpd` / `bird2` | LLDP daemon / BGP routing | Possible against host bridge but rig lacks an LLDP/BGP peer harness; tbx already owns BGP via `internal/bgp` | — | **Reject** — overlaps tbx's own networking; costly harness |
| `nfsd` | In-kernel NFS server | Export from node, mount from pod | Niche | **Reject** — serving NFS *from* Talos nodes is an anti-pattern for tbx's audience |
| `zfs` | ZFS modules + tools | mkfs-level test possible | Heavy module, licensing weight, large image delta | **Reject** — cost/size outweighs demand on throwaway VMs |
| `drbd`, `px-fuse`, `trident-iscsi-tools`, `multipath-tools`, `cachefilesd`, `nfsrahead` | Vendor/edge storage plumbing | Each needs its vendor stack to verify meaningfully | — | **Reject** — bring-your-own-CSI is out of scope per CONTEXT.md; `talos.schematic` escape hatch covers these |
| `mdadm` | Software RAID | — | Upstream marks it deprecated/no-op | **Reject** |
| `kata-containers`, `qemu` (hypervisor ext) | VM-based runtimes / nested virt | Requires enabling nested KVM on the L0 host and in tbx's QEMU config; flaky on CI runners; impossible on macOS VZ | — | **Reject** for initial set — real testing cost, tiny audience |
| `nvidia-*` (all 9) | GPU kernel modules, container toolkit, fabric manager | Needs GPU hardware + passthrough; no GPU CI runner | LTS/production split doubles the matrix | **Reject** — testing cost (GPU runners, vfio passthrough, driver/kernel matrix) is the whole budget; VMs in the rig have no GPU |
| `amd-ucode`, `intel-ucode`, all firmware/DRM/driver/DVB extensions | Physical-hardware enablement | Nothing to load in a virtio guest | — | **Reject** — inert on tbx's VM shapes |
| `hyperv-/vmtoolsd-/xen-guest-agent`, `metal-agent` | Agents for other hypervisors | Wrong hypervisor | — | **Reject** |
| `ecr-/harbor-credential-provider`, `soci-/stargz-snapshotter` | Registry/cloud integrations | Need cloud accounts | — | **Reject** for initial set |
| `binfmt-misc`, `glibc`, `joydev`, `uinput`, `uhid`, `nut-client`, `ctr`, `nvme-cli` | Misc tools/kernel shims | Presence-only checks; no functional story on tbx shapes | — | **Reject/defer** — presence-only verification is below the curation bar |

## Recommended initial curated set

1. **`siderolabs/qemu-guest-agent`** — prime candidate; requires the small
   `qemu_config.go` change to add the `org.qemu.guest_agent.0` virtserialport, and a
   documented no-op on macOS VZ hosts.
2. **`siderolabs/gvisor`** — cheapest fully-functional e2e of the runtime-handler class;
   real user value (workload sandboxing) on throwaway clusters.
3. **`siderolabs/nfs-utils`** — completes the storage story next to the always-on iSCSI
   pair; verifiable with an in-cluster NFS server, no external deps.

Everything else is defer (fuse3, btrfs, tailscale, wasm runtimes, crun — clear re-entry
criteria noted above) or reject. The `talos.schematic` escape hatch already covers users
who need an uncurated extension, so the curated set can stay small and every member can
stay green in CI.

## Mechanics for the curation feature (informational)

- The schematic builder (`internal/imagecache/schematic.go`) only needs
  `OfficialExtensions` to become `required + toggled` — the Image Factory validates names
  against the pinned Talos version, so bad toggles fail at image build, not at boot.
- Per-extension e2e probes slot into the existing KVM cluster e2e: generic
  `talosctl get extensions` / `talosctl service ext-<name>` assertions plus the
  per-candidate functional probe listed in the table.
