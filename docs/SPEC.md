# talosbox — Specification v1

**talosbox** (command: **`tbx`**) is a workshop-environment tool that attendees run on their own
Apple Silicon Macs to manage the full lifecycle of hypervisor-based Talos Linux VM clusters.
Nodes boot **unconfigured** (maintenance mode) on networking realistic enough for
production-style Cilium: shared L2 with host-routable IPs by default, optional BGP peer mode,
reachable ingress, and first-class inter-cluster routing.

Every decision in this spec was resolved on the
[wayfinder map](https://github.com/randax/talos-box/issues/1); each section links its ticket,
which holds the full rationale and evidence. Empirical claims were validated by prototypes on
the `prototype/talos-vz-boot` and `prototype/vmnet-arp` branches.

## 1. Goals and non-goals

The tool guarantees the **substrate** — VMs, networking, DNS, image delivery — and deliberately
does not touch what workshops teach. Generating and applying Talos machine config, installing
Cilium and ingress controllers, and bootstrapping clusters is the **attendees' work**; the tool
prints ready-to-paste configs and manifests but never applies them.

Out of scope ([map](https://github.com/randax/talos-box/issues/1)): workshop curriculum,
instructor-side orchestration, Intel Macs, Linux/Windows hosts, machines under 16 GB RAM, and
installing in-cluster software. No convenience bootstrap helpers in v1 — the guided hints
(§10) are the only hand-holding.

## 2. Supported platform

- **Hardware**: Apple Silicon (M1 or newer), **16 GB RAM minimum** (hard requirement).
- **macOS**: target floor **macOS 14 (Sonoma)**, with a verification gate (§12 G1): Talos EFI
  boot under Virtualization.framework is empirically proven only on macOS 26.5
  ([Confirm Talos boots under Virtualization.framework](https://github.com/randax/talos-box/issues/11));
  the historical entropy hang (talos#11865) was reported on earlier macOS. If G1 finds the hang
  on 14/15, either implement the direct-kernel-boot fallback (§4) or raise the floor — decide on
  evidence, not assumption.
- `tbx cluster suspend|resume` requires macOS 14+ regardless (vz save/restore API).

## 3. Architecture

**Language: Go** ([map Notes](https://github.com/randax/talos-box/issues/1) — deliberate
override of the owner's Rust default; ecosystem gravity: importable Talos machinery,
`Code-Hex/vz`).

Hypervisor: **Virtualization.framework directly via `Code-Hex/vz` v3**
([Select the hypervisor stack](https://github.com/randax/talos-box/issues/2)). Fallback if vz
becomes untenable: wrapping `vfkit` over REST. QEMU, lima, and tart are not used.

Three components:

| Component | Privilege | Responsibilities |
|---|---|---|
| `tbx` CLI | user | command surface, config parsing, talks to `tbxd` over a unix socket |
| `tbxd` daemon | user (launchd agent) | owns VM processes (clusters survive terminal close), embedded DNS server, registry mirror, GoBGP, balloon manager |
| `tbx-helper` | root (launchd daemon, installed once) | vmnet interface creation (FD passed to `tbxd`), `/etc/resolver/k8s.test`, `net.inet.ip.forwarding`, PF_ROUTE route injection |

Every VM gets a virtio serial port (`hvc0`) attached to a per-node unix socket owned by
`tbxd` — the transport for `tbx console` (§9).

Boot: **EFI boot loader** (`VZEFIBootLoader` + per-VM variable store) from the node's disk.
Designed fallback (G1): direct kernel boot via `VZLinuxBootLoader` — Image Factory kernels are
EFI-zboot wrappers whose payload (offset/size at header bytes 8–15) must be extracted and
decompressed; the technique is proven in the prototype harness.

## 4. Provisioning pipeline

([Diagnose the Talos installer pull stall](https://github.com/randax/talos-box/issues/12),
[Talos boot mechanics](https://github.com/randax/talos-box/issues/3))

**Raw disk images, never in-VM installs.** talosbox generates its **own default Image Factory
schematic** — vanilla plus `customization.extraKernelArgs: ["console=tty0", "console=hvc0"]` —
because the stock metal image logs only to `ttyAMA0`/`tty0`, neither of which exists under
Virtualization.framework; without the hvc0 arg, `tbx console` shows nothing. **Both args are
mandatory and ordered**: Factory's extraKernelArgs *replace* the image's default console args,
and `console=hvc0` alone bricks the boot under vz (verified: no boot, no output; with
`console=tty0 console=hvc0` the node boots and streams kernel+machined logs on hvc0 — gate G6,
closed). Schematics are content-addressed, so this is one deterministic POST to
`factory.talos.dev/schematics`; user-supplied schematics get the args appended the same way.
Per schematic + Talos version, `tbx` downloads Image Factory's `metal-arm64.raw.xz` once into
the cache, decompresses it, and provisions each
node disk as an **APFS `clonefile` clone** grown (sparse) to the configured disk size.
Validated results: node boots from disk straight to maintenance mode (unauthenticated apid on
TCP 50000, Reader role for `talosctl --insecure`); `apply-config` lands in ~10 s with no
reboot and zero network; a configured node cold-boots in ~16 s. The ISO+install path is
dropped.

- Cache: `~/.talosbox/cache/<schematic>/<version>/` — `tbx cache pull` (eager, pre-venue),
  `tbx cache prune`.
- Node disks: `~/.talosbox/clusters/<name>/<node>.img`, **20 GB sparse** default.
- **Talos version matrix**: each tbx release pins one tested default Talos version (initially
  v1.13.6, the validated one); `talosbox.yaml` may override `talos.version` and
  `talos.schematic` per file. Only the pinned default is CI-verified.

## 5. Networking

([Networking design](https://github.com/randax/talos-box/issues/6),
[macOS networking substrate](https://github.com/randax/talos-box/issues/4),
[Verify Cilium L2 announcements survive vmnet anti-spoofing](https://github.com/randax/talos-box/issues/10))

Substrate: **one vmnet shared-mode network per cluster, subnet pinned** via the
start/end/mask keys (no Apple entitlement; created by `tbx-helper`, FD handed to the VM's
`VZFileHandleNetworkDeviceAttachment`). vmnet provides DHCP and NAT egress. Empirically
verified: ARP for addresses vmnet never assigned passes unfiltered — L2-announced VIPs are
host-reachable.

**Subnets**: cluster *n* → `172.30.<n>.0/24` (base configurable). Layout, identical in every
cluster:

| Range | Use |
|---|---|
| `.1` | host: gateway, NAT, DNS/mirror bind, BGP peer, inter-cluster router |
| `.10–.179` | node DHCP range |
| `.200–.239` | Cilium LB-IPAM pool; **`.200` is the ingress VIP by convention** |
| `.240–.254` | reserved (tool-owned) |

**DNS**: embedded resolver in `tbxd` on `127.0.0.1`, wired once via `/etc/resolver/k8s.test`.
`*.<cluster>.k8s.test` → that cluster's `.200`; `<node>.<cluster>.k8s.test` → node IP.

**BGP mode** (`tbx bgp enable <cluster>`): "host as ToR" — one embedded GoBGP instance,
host **ASN 64512**, listening on each enabled cluster's `.1:179`; cluster *n* nodes speak
**ASN 64600+n**, eBGP to the host; learned routes are injected into the macOS FIB via
`tbx-helper` (PF_ROUTE). When enabled, BGP advertisement **replaces** L2 announcements for the
LB pool (each mechanism teachable in isolation). Pod-CIDR advertisement is accepted, not
guaranteed.

**Registry mirror** (required — see evidence in
[the installer-stall ticket](https://github.com/randax/talos-box/issues/12): corporate agents
such as GlobalProtect RST guest-originated TLS, so direct in-VM registry access must be treated
as unreliable on attendee machines): `tbxd` runs pull-through mirrors for `docker.io`,
`ghcr.io`, `quay.io`, `registry.k8s.io` (one listener per upstream, ports `5000+`), bound on
cluster gateway IPs; printed machine configs set `registryMirrors` accordingly. Mirror storage
lives in the cache and doubles as the offline-venue answer.

**Reachability guarantees** (the tested surface): host ↔ node IPs; host ↔ LB VIPs (L2 or BGP);
**cluster ↔ cluster** (nodes and VIPs) through the host as inter-subnet router — first-class,
per owner decision. Pod/service CIDRs stay internal; printed configs standardize
`10.244.0.0/16` / `10.96.0.0/12`.

## 6. Cluster model and VM lifecycle

A **cluster** is a named group of VMs on its own subnet; nodes are `<cluster>-cp-<i>` /
`<cluster>-worker-<i>`. Fixed, deterministic MACs per node (derived from cluster+node name) so
DHCP leases and DNS stay stable.

Lifecycle: `create/start/stop/destroy` per cluster and per node, `node add/remove` while the
cluster runs, `suspend/resume` (macOS 14+, vz save/restore, whole cluster). Nodes always come
up **unconfigured** — talosbox never generates or applies machine config. `tbx status` reports
each node's observed phase: `maintenance` (insecure apid answers), `configured` (TLS apid),
`unreachable`.

## 7. Snapshots and reset

([Snapshot and reset story](https://github.com/randax/talos-box/issues/7))

**Cold, whole-cluster, named, manual**: `tbx snapshot create|restore|list|delete <cluster>
[name]`. Create/restore stop the cluster (with confirmation), `clonefile` every node disk as
one crash-consistent set, and restart. No per-node snapshots (etcd split-brain bait), no
auto-snapshots, no live checkpoints — restore always passes through a stop; a restore costs a
~1-minute cold boot. Works on every supported macOS.

## 8. Resource model

([Resource model and efficiency](https://github.com/randax/talos-box/issues/8))

- **Default topology: 1 control plane + 2 workers, 2 GiB RAM / 2 vCPU per node** (6 GiB
  total). All sizes overridable per role in `talosbox.yaml`. HA control planes via scale-up,
  not default.
- **Active memory ballooning** (owner decision): `tbxd` monitors host memory pressure and
  inflates virtio balloons proportionally across running configured nodes when host free memory
  drops below the reserve, never below a **1 GiB per-node floor**, deflating on release.
  Verified: Talos arm64 kernel has `CONFIG_VIRTIO_BALLOON=m` — printed config snippets MUST
  include `machine.kernel.modules: [{name: virtio_balloon}]`; maintenance-mode nodes are exempt
  from balloon management.
- **Overcommit guard** (backstop): before `up`/`start`/`node add`, warn when the sum of
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
`talosctl --insecure` probe; configured-but-no-CNI → `tbx manifests demo | kubectl apply -f -`).
Hints **never execute anything**. `--quiet` suppresses them; all list/status commands support
`-o json`.

**`tbx console <cluster> <node>`** attaches interactively to the node's serial console (hvc0)
through the `tbxd`-owned socket — Talos renders its console dashboard and logs there, and
maintenance-mode debugging works before any config exists. Detach with **`Ctrl-]`**; the
session banner states the detach key. Attaching never blocks the VM; multiple attach/detach
cycles are supported. `tbx manifests` prints the cluster's matching `CiliumLoadBalancerIPPool`,
`CiliumBGPPeeringPolicy`, the `registryMirrors` machine-config patch, and the
`virtio_balloon` module patch.

## 11. Distribution

- **Homebrew** (`brew install randax/tap/talosbox`); binary is Developer-ID signed and
  notarized with the `com.apple.security.virtualization` entitlement — no restricted
  entitlements needed (bridged networking deliberately unused).
- **`sudo tbx system install`** (one-time) installs `tbx-helper` as a root launchd daemon and
  the `/etc/resolver/k8s.test` file; `tbx doctor` verifies helper, vmnet, DNS wiring, and
  forwarding. Everything else runs unprivileged.

## 12. Verification gates and risk register

Implementation must close these before v1 ships:

- **G1 — macOS floor**: boot the pinned Talos on macOS 14 and 15 under vz. Hang → implement
  direct-kernel-boot fallback (§3) or raise the floor to the oldest passing version.
- **G2 — GARP on failover**: L2-announcement VIP *failover* (gratuitous ARP moving a VIP
  between nodes) was not separately tested — verify against vmnet; BGP mode is the fallback.
- **G3 — balloon policy tuning**: validate the pressure thresholds under real workshop load.
- **G4 — mirror through security agents**: confirm host-bound mirror traffic passes on a
  GlobalProtect-managed machine (the attribution evidence is strong but circumstantial).
- **G5 — inter-cluster routing**: host forwarding across vmnet bridges is designed, not yet
  empirically verified — it is a first-class guarantee and needs a test.
- ~~G6 — Talos console on hvc0~~ **CLOSED**: with `console=tty0 console=hvc0` the node boots
  and streams kernel+machined logs on hvc0 (`console=hvc0` alone bricks boot — hence the
  mandatory arg pair in §4). Residual: the dashboard TUI's interactive rendering on hvc0 is
  unverified (logs confirmed); if it proves log-only, `tbx console` remains correct as a
  log-streaming + maintenance-input console.

## 13. Asset index

- Research: `docs/research/` on branches `research/hypervisor-stack`,
  `research/talos-boot-mechanics`, `research/macos-networking-substrate`
- Prototypes: `prototypes/talos-vz-boot/` on branches `prototype/talos-vz-boot` (boot
  validation) and `prototype/vmnet-arp` (ARP filter, Alpine serial harness, raw-image and
  registry experiments)
- Decision index: [the wayfinder map](https://github.com/randax/talos-box/issues/1)
