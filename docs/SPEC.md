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

The tool always guarantees the **substrate** — VMs, networking, DNS, and image delivery.
Without `cni`, it deliberately stops there so workshops can teach Talos bootstrap and CNI
installation themselves. Declaring `cni: cilium|flannel` opts into the curated path: `tbx`
generates and applies Talos machine config, bootstraps Kubernetes, and reconciles the pinned CNI
and optional LoadBalancer resources from the host. Ingress controllers and attendee workloads
remain **attendee work**; Cilium's built-in ingress controller is disabled.

Out of scope ([map](https://github.com/randax/talos-box/issues/1)): workshop curriculum,
instructor-side orchestration, Intel Macs, Linux/Windows hosts, machines under 16 GB RAM, and
arbitrary application or ingress installation. The curated CNI/LoadBalancer path is an explicit
opt-in; substrate-only clusters retain the original manual workflow and guided hints (§10).

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
Per schematic + Talos version + target hypervisor architecture, `tbx` downloads Image Factory's
`metal-arm64.raw.xz` or `metal-amd64.raw.xz` once into the cache, decompresses it, and provisions each
node disk as an **APFS `clonefile` clone** grown (sparse) to the configured disk size.
Validated results: node boots from disk straight to maintenance mode (unauthenticated apid on
TCP 50000, Reader role for `talosctl --insecure`); `apply-config` lands in ~10 s with no
reboot and zero network; a configured node cold-boots in ~16 s. The ISO+install path is
dropped.

- Cache: `~/.talosbox/cache/<schematic>/<version>/<architecture>/` — `tbx cache pull` (eager, pre-venue),
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
| `.2–.179` | node DHCP range (vmnet assigns every address after the `.1` gateway) |
| `.200–.239` | Cilium LB-IPAM pool; **`.200` is the ingress VIP by convention** |
| `.240–.254` | reserved (tool-owned) |

Pinned shared-mode vmnet interfaces only intercommunicate within the same subnet. To preserve
the per-cluster `/24` model, `tbx-helper` routes `172.30.0.0/16` frames between helper-owned
attachments before they enter vmnet, learning node and VIP ownership from DHCP and ARP. vmnet
continues to provide same-subnet switching, DHCP, NAT egress, and host reachability.

**DNS**: embedded resolver in `tbxd` on `127.0.0.1`. Every cluster has a **cluster domain** —
chosen at create (`--domain` / `domain:`), immutable, unique across clusters, defaulting to
`<cluster>.k8s.test`. `*.<domain>` → that cluster's `.200`; `<node>.<domain>` → node IP;
the domain apex itself has no record. Domains may nest across clusters and resolve
longest-suffix-wins — the owning cluster answers (or NXDOMAINs) alone. Safe domains (`.test`,
`.internal`, `home.arpa`) are accepted outright; `.local`/`.localhost`/`.invalid`/single-label
are always rejected; anything else can shadow real DNS and requires the explicit
`--allow-unsafe-domain` / `allowUnsafeDomain: true` opt-in (non-interactive, so scripted paths
stay deterministic). Host wiring: default-domain clusters share the static
`/etc/resolver/k8s.test` file; each custom-domain cluster gets `/etc/resolver/<domain>`,
written with an ownership marker by the helper, reconciled by `tbxd` (recreate missing,
remove marked orphans, never touch unmarked files), with a `killall -HUP mDNSResponder`
after changes.

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
`ghcr.io`, `quay.io`, `registry.k8s.io`, plus a catch-all listener on port `5059` (port 5000
is macOS AirPlay Receiver, which answers 403 and poisons smoke tests; gate G4), bound on each
**cluster gateway IP** (`172.30.<n>.1`) — not `0.0.0.0` — with the bind set added when a
cluster starts and removed when it stops (#39), so the ports never surface on the host's
physical/VPN interfaces and distinct gateways share the fixed ports without conflict. New
printed machine configs set Talos `machine.registries.mirrors."*"` to a single endpoint
`http://172.30.<n>.1:5059` with `skipFallback: true`; legacy fixed listeners on `5055–5058`
remain only so older clusters keep working until they are recreated. Mirror storage lives in
the cache and doubles as the offline-venue answer.

**Reachability guarantees** (the tested surface): host ↔ node IPs; host ↔ LB VIPs (L2 or BGP);
**cluster ↔ cluster** (nodes and VIPs) through the host as inter-subnet router — first-class,
per owner decision. Pod/service CIDRs stay internal; printed configs standardize
`10.244.0.0/16` / `10.96.0.0/12`.

## 6. Cluster model and VM lifecycle

A **cluster** is a named group of VMs on its own subnet; nodes are `<cluster>-cp-<i>` /
`<cluster>-worker-<i>`. Fixed, deterministic MACs per node (derived from cluster+node name) so
DHCP leases and DNS stay stable.

Lifecycle: `create/start/stop/destroy` per cluster and per node, `node add/remove` while the
cluster runs, and whole-cluster `suspend/resume` (macOS 14+, vz save/restore). Same-daemon
resume preserves memory; after a daemon restart it warns and gracefully cold-boots because
the file-handle-backed device identity required by vz restore no longer exists. Nodes always come
up **unconfigured** — talosbox never generates or applies machine config. `tbx status` reports
each node's observed phase — `stopped`, `unreachable`, `maintenance`, `configured` — derived
from a credential-free TLS probe of apid: **both** apid modes serve TLS (empirical correction,
#31 — the earlier "insecure = maintenance" model was wrong); maintenance mode presents the
well-known `maintenance-service.talos.dev` certificate, a configured node presents its
cluster-CA identity and demands a client certificate.

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
    domain: lab.internal   # optional cluster domain; default <name>.k8s.test
    allowUnsafeDomain: false # explicit opt-in for domains that can shadow real DNS
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
`talosctl --insecure` probe; provisioned clusters report convergence progress and a safe `tbx up` rerun; substrate-only clusters retain manual guidance).
Hints **never execute anything**. `--quiet` suppresses them; all list/status commands support
`-o json`.

**`tbx console <cluster> <node>`** attaches interactively to the node's serial console (hvc0)
through the `tbxd`-owned socket — Talos renders its console dashboard and logs there, and
maintenance-mode debugging works before any config exists. Detach with **`Ctrl-]`**; the
session banner states the detach key. Attaching never blocks the VM; multiple attach/detach
cycles are supported. `tbx manifests` is the exact inspection/fork surface for the declared
curated path: its `machine`, `values`, `objects`, and `extras` sections match the patch,
pinned Helm release, rendered objects, and LB/BGP/MetalLB probe resources tbx applies. The
output can be applied manually on a substrate-only cluster; it does not turn substrate-only
clusters into a curated path by itself.

## 11. Distribution

- **Homebrew** (`brew install randax/tap/talosbox`); binary is Developer-ID signed and
  notarized with the `com.apple.security.virtualization` entitlement — no restricted
  entitlements needed (bridged networking deliberately unused).
- **`sudo tbx system install`** (one-time) installs `tbx-helper` as a root launchd daemon and
  the `/etc/resolver/k8s.test` file; `tbx doctor` verifies helper, vmnet, DNS wiring, and
  forwarding. Everything else runs unprivileged. The helper's macOS filesystem writes are
  confined to `/etc/resolver/k8s.test` plus `/etc/resolver/<domain>` for canonical validated
  domain names; it refuses non-canonical names, never follows symlinks or touches unmanaged
  files, and only ever deletes files carrying its ownership marker. Like every helper
  operation, the domain set itself is trusted from the authorized client (the daemon derives
  it from cluster state) — the helper validates shape, not provenance.

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

## 13. Asset index

- Research: `docs/research/` on branches `research/hypervisor-stack`,
  `research/talos-boot-mechanics`, `research/macos-networking-substrate`
- Prototypes: `prototypes/talos-vz-boot/` on branches `prototype/talos-vz-boot` (boot
  validation) and `prototype/vmnet-arp` (ARP filter, Alpine serial harness, raw-image and
  registry experiments)
- Decision index: [the wayfinder map](https://github.com/randax/talos-box/issues/1)
