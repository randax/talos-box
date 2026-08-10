# talosbox — Specification v1

**talosbox** (command: **`tbx`**) is a workshop-environment tool that attendees run on their own
Apple Silicon macOS or amd64/arm64 Linux hosts to manage the full lifecycle of hypervisor-based
Talos Linux VM clusters. Nodes boot **unconfigured** (maintenance mode) on networking realistic
enough for production-style Cilium: shared L2 with host-routable IPs by default, optional BGP
peer mode, reachable ingress, and first-class inter-cluster routing.

Every decision in this spec was resolved on the
[wayfinder map](https://github.com/randax/talos-box/issues/1); each section links its ticket,
which holds the full rationale and evidence. Empirical claims were validated by prototypes on
the `prototype/talos-vz-boot` and `prototype/vmnet-arp` branches.

## 1. Goals and non-goals

The tool guarantees the **substrate** — VMs, networking, DNS, image delivery — and deliberately
does not touch what workshops teach. Generating and applying Talos machine config, installing
Cilium and ingress controllers, and bootstrapping clusters is the **attendees' work**; the tool
prints ready-to-paste configs and manifests but never applies them.

Out of scope ([original map](https://github.com/randax/talos-box/issues/1),
[Linux map](https://github.com/randax/talos-box/issues/71)): workshop curriculum,
instructor-side orchestration, Intel Macs, Windows/WSL2 hosts, machines under 16 GB RAM,
rootless Linux networking, and installing in-cluster software. No convenience bootstrap
helpers in v1 — the guided hints (§10) are the only hand-holding.

## 2. Supported platforms

All supported hosts require **16 GB RAM minimum**.

| Host | Architecture | Version floor | Hypervisor | Notes |
|---|---|---|---|---|
| macOS | Apple Silicon (arm64) | macOS 14 (Sonoma) | Virtualization.framework | The macOS 14/15 boot gate remains open (§12 G1); Intel Macs are unsupported |
| Ubuntu LTS | amd64, arm64 | Ubuntu 22.04 / QEMU 6.2 | QEMU/KVM | Tier one; suspend requires QEMU 8.2+, normally Ubuntu 24.04+ |
| Fedora | amd64, arm64 | Current stable / QEMU 6.2 | QEMU/KVM | Tier one |
| Arch Linux | amd64, arm64 | Rolling / QEMU 6.2 | QEMU/KVM | Tier one |
| NixOS | amd64, arm64 | Current stable / QEMU 6.2 | QEMU/KVM | Tier-one design target; release support waits for the flake/module in #96 |
| Debian, openSUSE | amd64, arm64 | QEMU 6.2 | QEMU/KVM | Best effort |

The Linux host and guest architectures must match; TCG emulation is never a fallback. KVM must
be available through a readable+writable `/dev/kvm`, and the installed QEMU package must provide
`q35` on amd64 or `virt` on arm64 plus matching OVMF/AAVMF firmware.

Suspend/resume is capability-gated rather than part of the general QEMU floor
([decision #83](https://github.com/randax/talos-box/issues/83)):

- macOS requires macOS 14+ and retains the exact in-process Virtualization.framework device
  graph; a daemon restart degrades resume to a warned cold boot.
- Linux requires QEMU 8.2+ (`migrate` to/from a file with raw disks). QEMU 6.2–8.1 supports
  every other operation, and `tbx doctor` reports the unavailable capability with an upgrade
  hint. Restore requires the same QEMU version, architecture, and machine type.

## 3. Architecture

**Language: Go** ([map Notes](https://github.com/randax/talos-box/issues/1) — deliberate
override of the owner's Rust default; ecosystem gravity: importable Talos machinery,
`Code-Hex/vz`).

The platform-neutral `internal/hypervisor` boundary has one host backend per operating system:

- macOS uses **Virtualization.framework directly via `Code-Hex/vz` v3**
  ([Select the macOS hypervisor stack](https://github.com/randax/talos-box/issues/2)). Fallback
  if vz becomes untenable: wrapping `vfkit` over REST.
- Linux uses **QEMU/KVM directly over QMP**, with no libvirt
  ([Linux hypervisor design](https://github.com/randax/talos-box/issues/77)).

Lima and tart are not used on either platform.

| Component | macOS | Linux | Responsibilities |
|---|---|---|---|
| `tbx` CLI | user | user | command surface, config parsing, talks to `tbxd` over a Unix socket |
| `tbxd` daemon | launchd user agent / on-demand child | systemd user socket/service / on-demand child | owns VM processes, embedded DNS, registry mirror, GoBGP, balloon manager |
| `tbx-helper` | root launchd daemon | `tbx` system user with only `CAP_NET_ADMIN`, `CAP_NET_BIND_SERVICE`, `CAP_NET_RAW` | creates platform network attachments, converges forwarding/DNS integration, and updates host routes |

Every VM gets a virtio serial port (`hvc0`) attached to a per-node unix socket owned by
`tbxd` — the transport for `tbx console` (§9).

Boot is EFI from the node's disk with a per-VM variable store: `VZEFIBootLoader` on macOS and
QEMU pflash with OVMF/AAVMF on Linux. The macOS designed fallback (G1) is direct kernel boot via
`VZLinuxBootLoader` — Image Factory kernels are EFI-zboot wrappers whose payload (offset/size
at header bytes 8–15) must be extracted and decompressed; the technique is proven in the
prototype harness.

## 4. Provisioning pipeline

([Diagnose the Talos installer pull stall](https://github.com/randax/talos-box/issues/12),
[Talos boot mechanics](https://github.com/randax/talos-box/issues/3))

**Raw disk images, never in-VM installs.** talosbox generates its **own default Image Factory
schematic** — vanilla plus `customization.extraKernelArgs: ["console=tty0", "console=hvc0"]` —
because both backends expose the Talos console as virtio `hvc0`. **Both args are mandatory and
ordered**: Factory's extraKernelArgs *replace* the image's default console args, and
`console=hvc0` alone bricks the boot under vz (verified: no boot, no output; with
`console=tty0 console=hvc0` the node boots and streams kernel+machined logs on hvc0 — gate G6,
closed). Schematics are content-addressed, so this is one deterministic POST to
`factory.talos.dev/schematics`; user-supplied schematics get the args appended the same way.
Per schematic + Talos version + target hypervisor architecture, `tbx` downloads Image Factory's
`metal-arm64.raw.xz` or `metal-amd64.raw.xz` once into the cache and decompresses it. macOS
provisions each node disk as an APFS `clonefile` clone; Linux currently copies the cached raw
image, then both platforms grow the result sparsely to the configured disk size. On the
validated macOS path, a node boots from disk straight to maintenance mode (unauthenticated
apid on TCP 50000, Reader role for `talosctl --insecure`); `apply-config` lands in ~10 s with
no reboot and zero network; a configured node cold-boots in ~16 s. The equivalent Linux
end-to-end timing and cluster-up gate remains #97. The ISO+install path is dropped.

- Cache: `~/.talosbox/cache/<schematic>/<version>/<architecture>/` — `tbx cache pull` (eager, pre-venue),
  `tbx cache prune`.
- Node disks: `~/.talosbox/clusters/<name>/<node>.img`, **20 GB sparse** default.
- **Talos version matrix**: each tbx release pins one tested default Talos version (initially
  v1.13.6, the validated one); `talosbox.yaml` may override `talos.version` and
  `talos.schematic` per file. Only the pinned default is CI-verified.

## 5. Networking

([Networking design](https://github.com/randax/talos-box/issues/6),
[macOS networking substrate](https://github.com/randax/talos-box/issues/4),
[Verify Cilium L2 announcements survive vmnet anti-spoofing](https://github.com/randax/talos-box/issues/10),
[Linux networking design](https://github.com/randax/talos-box/issues/78))

The logical model is identical on both platforms: one shared L2 domain and one pinned `/24`
per cluster, host-routable node addresses, NAT egress, and a gateway that joins cluster
subnets. The attachment and host integration are platform-specific.

### macOS substrate

One vmnet shared-mode network per cluster, pinned via the start/end/mask keys (no Apple
entitlement; created by `tbx-helper`, with a datagram FD handed to the VM's
`VZFileHandleNetworkDeviceAttachment`). vmnet provides DHCP and NAT egress. Empirically
verified: ARP for addresses vmnet never assigned passes unfiltered, so L2-announced VIPs are
host-reachable.

### Linux substrate

One `br-tbx<n>` bridge per cluster with STP disabled and gateway `172.30.<n>.1/24`; one
helper-created tap per node is enslaved to the bridge and passed to QEMU as a tap FD. The
helper serves static DHCP reservations derived from each node's deterministic MAC, enables
IPv4 forwarding, and converges a talosbox-owned `table inet tbx` containing only the required
forwarding and masquerade rules. It never edits foreign nftables tables or chains. Subnet
selection rejects collisions with existing host addresses/routes and suggests a free subnet.

The bridge is ordinary Linux L2, so Cilium L2 announcements and their failover work without
host-side GARP machinery. Host networking is declarative desired state: bridge, taps, nftables,
DHCP, DNS, and resolved registration are reconverged on helper startup because kernel state
does not survive a reboot.

**Subnets**: cluster *n* → `172.30.<n>.0/24` (base configurable). Layout, identical in every
cluster:

| Range | Use |
|---|---|
| `.1` | host: gateway, NAT, DNS/mirror bind, BGP peer, inter-cluster router |
| `.2–.179` | node DHCP range (vmnet dynamic leases on macOS; deterministic reservations on Linux) |
| `.200–.239` | Cilium LB-IPAM pool; **`.200` is the ingress VIP by convention** |
| `.240–.254` | reserved (tool-owned) |

On macOS, pinned shared-mode vmnet interfaces only intercommunicate within the same subnet. To
preserve the per-cluster `/24` model, `tbx-helper` routes `172.30.0.0/16` frames between
helper-owned attachments before they enter vmnet, learning node and VIP ownership from DHCP
and ARP. On Linux, the kernel routes between the helper-owned bridges through the converged
forwarding policy.

**DNS**: `*.<cluster>.k8s.test` resolves to that cluster's `.200`, while
`<node>.<cluster>.k8s.test` resolves to the node IP. On macOS, the embedded resolver listens on
loopback and is wired through `/etc/resolver/k8s.test`. On Linux, the helper binds the cluster
gateway's port 53 and passes the listener FD to the unprivileged DNS server; guest queries are
forwarded upstream, and the helper registers `~<cluster>.k8s.test` as a route-only domain on
the bridge through systemd-resolved D-Bus. Hosts without resolved retain guest and by-IP
access, and `tbx doctor` prints the required `resolvectl` fallback without writing
`/etc/resolv.conf`.

**BGP mode** (`tbx bgp enable <cluster>`): "host as ToR" — one embedded GoBGP instance,
host **ASN 64512**, listening on each enabled cluster's `.1:179`; cluster *n* nodes speak
**ASN 64600+n**, eBGP to the host; learned routes are injected into the host FIB via
`tbx-helper` (PF_ROUTE on macOS, rtnetlink `RTM_NEWROUTE`/`RTM_DELROUTE` on Linux). When
enabled, BGP advertisement **replaces** L2 announcements for the LB pool (each mechanism
teachable in isolation). Pod-CIDR advertisement is accepted, not guaranteed. On Linux, this
mode is mainly for routed upstreams, ECMP, or `externalTrafficPolicy: Local` where only nodes
with local endpoints should advertise. The slow-L2 failover caveat is macOS/vmnet-specific:
macOS ignores gratuitous ARP through vmnet and converges only via its own ARP revalidation,
so BGP remains the fast-failover path there.

**Registry mirror** (required — see evidence in
[the installer-stall ticket](https://github.com/randax/talos-box/issues/12): corporate agents
such as GlobalProtect RST guest-originated TLS, so direct in-VM registry access must be treated
as unreliable on attendee machines): `tbxd` runs pull-through mirrors for `docker.io`,
`ghcr.io`, `quay.io`, `registry.k8s.io` (one listener per upstream, ports `5055+` — port 5000
is macOS AirPlay Receiver, which answers 403 and poisons smoke tests; gate G4), bound on
each **cluster gateway IP** (`172.30.<n>.1`) — not `0.0.0.0` — with the bind set added when a
cluster starts and removed when it stops (#39), so the ports never surface on the host's
physical/VPN interfaces and distinct gateways share the fixed ports without conflict. Printed
machine configs set `registryMirrors` accordingly. Mirror storage lives in the cache and
doubles as the offline-venue answer.

**Reachability contract**: host ↔ node IPs; host ↔ LB VIPs (L2 or BGP); **cluster ↔ cluster**
(nodes and VIPs) through the host as inter-subnet router — first-class, per owner decision.
The complete contract is hardware-validated on macOS and component/integration-tested for the
Linux helper; the Linux full-cluster CI gate remains #97. Pod/service CIDRs stay internal;
printed configs standardize `10.244.0.0/16` / `10.96.0.0/12`.

## 6. Cluster model and VM lifecycle

A **cluster** is a named group of VMs on its own subnet; nodes are `<cluster>-cp-<i>` /
`<cluster>-worker-<i>`. Fixed, deterministic MACs per node (derived from cluster+node name) so
DHCP leases and DNS stay stable.

Lifecycle: `create/start/stop/destroy` per cluster and per node, `node add/remove` while the
cluster runs, and capability-gated whole-cluster `suspend/resume`. macOS same-daemon resume
preserves memory; after a daemon restart it warns and gracefully cold-boots because the
file-handle-backed device identity required by vz restore no longer exists. Linux QEMU 8.2+
saves to a versioned file and can restore after a daemon restart when the QEMU version,
architecture, and machine type match; older QEMU refuses suspend with the capability reason.
Nodes always come up **unconfigured** — talosbox never generates or applies machine config.
`tbx status` reports each node's observed phase — `stopped`, `unreachable`, `maintenance`,
`configured` — derived from a credential-free TLS probe of apid: **both** apid modes serve TLS
(empirical correction, #31 — the earlier "insecure = maintenance" model was wrong);
maintenance mode presents the well-known `maintenance-service.talos.dev` certificate, a
configured node presents its cluster-CA identity and demands a client certificate.

## 7. Snapshots and reset

([Snapshot and reset story](https://github.com/randax/talos-box/issues/7))

**Cold, whole-cluster, named, manual**: `tbx snapshot create|restore|list|delete <cluster>
[name]`. Create/restore stop the cluster (with confirmation), `clonefile` every node disk as
one crash-consistent set, and restart. No per-node snapshots (etcd split-brain bait), no
auto-snapshots, no live checkpoints — restore always passes through a stop; a restore costs a
~1-minute cold boot. macOS uses APFS clonefile; Linux falls back to a full raw-image copy when
the filesystem clone primitive is unavailable.

## 8. Resource model

([Resource model and efficiency](https://github.com/randax/talos-box/issues/8))

- **Default topology: 1 control plane + 2 workers, 2 GiB RAM / 2 vCPU per node** (6 GiB
  total). All sizes overridable per role in `talosbox.yaml`. HA control planes via scale-up,
  not default.
- **Active memory ballooning** (owner decision): on macOS, `tbxd` monitors host memory pressure and
  inflates virtio balloons proportionally across running configured nodes when host free memory
  drops below the reserve, never below a **1 GiB per-node floor**, deflating on release.
  Verified: Talos arm64 kernel has `CONFIG_VIRTIO_BALLOON=m` — printed config snippets MUST
  include `machine.kernel.modules: [{name: virtio_balloon}]`; maintenance-mode nodes are exempt
  from balloon management. The QEMU backend supports balloon target/readback and tolerates an
  inactive guest device, but the Linux host-free-memory sampler is not implemented yet, so the
  automatic pressure policy is presently inactive there.
- **Overcommit guard** (backstop, currently macOS): before `up`/`start`/`node add`, warn when the sum of
  configured VM memory exceeds host RAM minus a 6 GiB host reserve; `--force` overrides.

## 9. CLI and `talosbox.yaml`

([CLI UX and config model](https://github.com/randax/talos-box/issues/5))

**Declarative-first**: `talosbox.yaml` is the source of truth; **`tbx up` is idempotent** and
reconciles reality to the file; `tbx down` is its inverse. Imperative one-shots emit the
equivalent YAML.

```
tbx up / tbx down
tbx cluster create|start|stop|destroy|list [name] [--cp N --workers N]
tbx node add|remove|start|stop <cluster> [node]
tbx cluster suspend|resume <cluster>
tbx snapshot create|restore|list|delete <cluster> [name]
tbx status [cluster]      tbx manifests <cluster>
tbx console <cluster> <node>
tbx bgp enable|disable <cluster>
tbx cache pull|prune      tbx doctor      tbx system install|uninstall
```

Schema (v1):

```yaml
version: 1
talos:
  version: v1.13.6        # optional; defaults to the release's pinned version
  schematic: ""           # optional Image Factory schematic id
clusters:
  - name: demo
    controlPlanes: 1
    workers: 2
    bgp: false
    node:                  # defaults for all nodes
      memory: 2GiB
      cpus: 2
      diskSize: 20GiB
    controlPlane: {}       # optional per-role overrides of `node`
    worker: {}
```

## 10. Guided output

`tbx status` is **state-aware**: alongside nodes/IPs/DNS names/LB pool/BGP state it appends
copy-pasteable next-step hints keyed to observed state (maintenance node → the
`talosctl --insecure` probe; configured-but-no-CNI → `tbx manifests demo k8s | kubectl apply -f -`).
Hints **never execute anything**. `--quiet` suppresses them; all list/status commands support
`-o json`.

**`tbx console <cluster> <node>`** attaches interactively to the node's serial console (hvc0)
through the `tbxd`-owned socket — Talos renders its console dashboard and logs there, and
maintenance-mode debugging works before any config exists. Detach with **`Ctrl-]`**; the
session banner states the detach key. Attaching never blocks the VM; multiple attach/detach
cycles are supported. `tbx manifests` prints the cluster's curated Cilium Helm values,
matching `CiliumLoadBalancerIPPool`,
`CiliumBGPClusterConfig`/`CiliumBGPPeerConfig`/`CiliumBGPAdvertisement` resources, the
`registryMirrors` machine-config patch, and the
`virtio_balloon` module patch.

## 11. Distribution

### macOS

- **Homebrew** (`brew install randax/tap/talosbox`); binary is Developer-ID signed and
  notarized with the `com.apple.security.virtualization` entitlement — no restricted
  entitlements needed (bridged networking deliberately unused).
- **`sudo tbx system install`** (one-time) installs `tbx-helper` as a root launchd daemon and
  the `/etc/resolver/k8s.test` file. Everything else runs unprivileged.

### Linux

- goreleaser cross-builds `tbx`, `tbxd`, and `tbx-helper` for amd64/arm64; nfpm emits `.deb`
  and `.rpm` packages with systemd units, sysusers, and the resolved polkit rule.
- Cloudsmith hosts the public apt/dnf repository; GitHub Releases holds canonical artifacts;
  AUR `tbx-bin` pins those artifacts; the in-repo Nix flake exposes packages, an overlay, and
  `nixosModules.default` under `virtualisation.talosbox`.
- Packages install the socket-activated helper as the `tbx` system user with only
  `CAP_NET_ADMIN`, `CAP_NET_BIND_SERVICE`, and `CAP_NET_RAW`, plus a socket-activated `tbxd`
  user service. Linux must not use the macOS `tbx system install` command.

These Linux channels are the release design, not a statement of current publication. Until
#95, #96, and #101 close, the only documented Linux installation is the source-preview path
in [Linux host setup](linux.md).

On both platforms, `tbx doctor` verifies the platform helper, hypervisor, DNS/forwarding,
routes, and external image access; host-capacity sampling is currently macOS-only. Linux adds
the detailed KVM/QEMU, bridge/firewall, reverse-path filter, port, systemd-unit, group, and
capability checks listed in
[Linux host setup](linux.md#what-tbx-doctor-checks-on-linux).

## 12. Verification gates and risk register

Implementation must close these before v1 ships:

- **G1 — macOS floor**: boot the pinned Talos on macOS 14 and 15 under vz. Hang → implement
  direct-kernel-boot fallback (§3) or raise the floor to the oldest passing version.
- ~~G2 — GARP on failover~~ **CLOSED**: on macOS, the host ignores GARP through vmnet; L2
  failover converges via macOS ARP revalidation in ~40–50 s (§5 documents the latency; BGP mode
  for fast failover there). Linux shared-bridge mode does not have this gate.
  Residual: repeated-GARP bursts untested.
- ~~G3 — balloon policy tuning~~ **CLOSED** (#38): defaults are **6 GiB host reserve, 1 GiB
  per-node floor, 5s poll** (`TBX_BALLOON_RESERVE_MIB` overrides the reserve). Verified live:
  under a synthetic deficit a configured node's balloon inflated, dropping guest free memory
  from ~2.45 GiB to ~0.6 GiB (≈ the deficit), and deflated back on release. Maintenance-mode
  nodes are apid-probed out and exempt; the overcommit guard warns on create/start/node-add
  with `--force` to override.
- **G4 — mirror through security agents**: confirm host-bound mirror traffic passes on a
  GlobalProtect-managed machine (the attribution evidence is strong but circumstantial).
- ~~G5 — inter-cluster routing~~ **CLOSED** (physical Mac): two helper-backed vmnet bridges on
  pinned `/24`s pass bidirectional node↔node routing immediately after DHCP, host↔node/VIP,
  node↔remote-VIP, and clean detach in the PID-gated `TestHelperNetworkingE2E` regression.
- ~~G7 — suspend/resume memory restore~~ **CLOSED (same daemon)**: after saving, talosbox stops
  the VM but retains the exact vz configuration and file-handle-backed devices. Restoring that
  stopped VM preserves memory (#42); hardware verification continued the same Talos boot from
  kernel timestamp 4.20 s to 34.20 s with no reboot. The boundary is a daemon restart: fresh
  console/serial handles produce vz `ErrorRestore` "invalid argument", so cross-daemon resume
  emits a warning and gracefully cold-boots. The network fd alone was previously ruled out;
  the complete retained device graph is what makes same-daemon restore compatible.
- ~~G6 — Talos console on hvc0~~ **CLOSED**: with `console=tty0 console=hvc0` the node boots
  and streams kernel+machined logs on hvc0 (`console=hvc0` alone bricks boot — hence the
  mandatory arg pair in §4). Residual: the dashboard TUI's interactive rendering on hvc0 is
  unverified (logs confirmed); if it proves log-only, `tbx console` remains correct as a
  log-streaming + maintenance-input console.
- **G8 — Linux release channels**: publish and smoke-test Cloudsmith apt/dnf, AUR `tbx-bin`,
  and the Nix flake/module (#95, #96, #101). Documentation must not expose placeholder URLs.
- **G9 — Linux full-cluster CI**: run build/unit on amd64+arm64 and the substrate-only
  1-control-plane + 2-worker KVM e2e on amd64, with a hard writable-`/dev/kvm` gate (#97).

## 13. Asset index

- Research: `docs/research/` on branches `research/hypervisor-stack`,
  `research/talos-boot-mechanics`, `research/macos-networking-substrate`,
  `research/qemu-qmp-parity`, `research/linux-l2-bgp-vip`, and
  `research/distro-packaging-lsm`
- Prototypes: `prototypes/talos-vz-boot/` on branches `prototype/talos-vz-boot` (boot
  validation) and `prototype/vmnet-arp` (ARP filter, Alpine serial harness, raw-image and
  registry experiments)
- Decision indexes: [macOS wayfinder map](https://github.com/randax/talos-box/issues/1) and
  [Linux parity map](https://github.com/randax/talos-box/issues/71)
