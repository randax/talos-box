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

The tool always guarantees the **substrate** — VMs, networking, DNS, and image delivery.
Without `cni`, it deliberately stops there so workshops can teach Talos bootstrap and CNI
installation themselves. Declaring `cni: cilium|flannel` opts into the curated path: `tbx`
generates and applies Talos machine config, bootstraps Kubernetes, and reconciles the pinned CNI
and optional LoadBalancer resources from the host. Declaring `csi: longhorn|local-path`
(requires `cni`) adds curated persistent storage to the same path (§9). Ingress controllers and attendee workloads
remain **attendee work**; Cilium's built-in ingress controller is disabled.

Out of scope ([original map](https://github.com/randax/talos-box/issues/1),
[Linux map](https://github.com/randax/talos-box/issues/71)): workshop curriculum,
instructor-side orchestration, Intel Macs, Windows/WSL2 hosts, machines under 16 GB RAM,
rootless Linux networking, and arbitrary application or ingress installation. The curated
CNI/LoadBalancer path is an explicit opt-in; substrate-only clusters retain the original manual
workflow and guided hints (§10).

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
schematic** — vanilla plus `customization.extraKernelArgs: ["console=tty0", "console=hvc0"]`
and the `siderolabs/iscsi-tools` and `siderolabs/util-linux-tools` system extensions, so every
tbx image is storage-capable from birth. The kernel args exist because both backends expose the
Talos console as virtio `hvc0`. **Both args are mandatory and
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

- Cache: `~/.talosbox/cache/` stores Talos disk images by
  `<schematic>/<version>/<architecture>/` and registry-mirror content separately. `tbx cache pull`
  fetches Talos disks eagerly: with no flags it reads `talosbox.yaml` the way `tbx up` does,
  resolves every cluster's combination with inheritance applied, performs any schematic
  re-composition while the Factory is reachable, and warms each cluster's container-image set
  (`--no-images` opts out); explicit flags pull one ad-hoc combination. Either way what is
  fetched is **pinned**. `tbx cache warm <list>...` prepares registry content from one or
  more lists (use `-` for stdin once); blank lines and `#` comments are ignored. Each entry is a
  fully qualified image reference with a non-`latest` tag or a `sha256`/`sha512` digest; a
  tag-plus-digest entry is the immutable list form. `tbx cache warm --check [--deep] <list>...`
  verifies tag mapping, selected `linux/<arch>` manifest, config, and all layers locally;
  `--deep` only adds a blob rehash and requires `--check`. A normal warm resumes incomplete refs
  without contacting upstream for complete refs; `--refresh` explicitly revalidates only complete
  unpinned tags, because digest-pinned refs need no freshness resolution. A transient refresh
  failure is nonfatal when the cached graph is complete. Both check modes also verify the
  implicit bootstrap-required set no list can name
  (the CRI pod sandbox image), so neither mode reports the cache complete while that image is
  missing; only `--deep` detects a cached blob whose bytes no longer match its digest, so
  `--check --deep` remains the pre-travel gate.
  This verifies cache completeness, not a live-cluster pull.
- Node disks: `~/.talosbox/clusters/<name>/<node>.img`, **20 GB sparse** default.
- **Talos version matrix**: each tbx release pins one tested default Talos version (currently
  v1.13.9, the validated one); `talosbox.yaml` may override `talos.version`, `talos.schematic`,
  and `talos.extensions` at file level and per cluster, inheriting field-wise (set fields
  override, lists override rather than concatenate, `extensions: []` opts out). Only the
  pinned default is CI-verified on every change; the floor of the supported window is booted by
  a scheduled nightly e2e lane running the same curated-extension probes, whose failure blocks
  a **release**, not a merge.
- **Curated extensions**: `talos.extensions` names members of a closed curated set —
  `gvisor`, `nfs-utils`, `qemu-guest-agent` — by bare short name; tbx owns the mapping to
  official Image Factory refs and composes them on top of the always-baked storage pair. Unknown
  names are rejected offline against the set (with a near-miss suggestion) before anything is
  created; availability for the cluster's Talos version is checked against the Factory's
  official-extension catalog. A brought `talos.schematic` plus extensions is **re-composed**:
  tbx fetches that schematic's definition, merges in only the requested extensions, and POSTs
  for the composed id — the brought schematic stays sovereign (no kernel args, no storage
  extensions injected) and requested extensions are never silently dropped. Composition is
  content-addressed, so the same inputs yield the same id, and a cached composed image creates
  a cluster with no Factory contact at all. Extensions whose usefulness depends on the host
  backend (`qemu-guest-agent` under Virtualization.framework) are capability-gated and reported
  by `tbx status` and `tbx doctor`, never rejected: the file stays portable. On the provisioning
  path, requesting `gvisor` also sets `user.max_user_namespaces` in the generated machine config
  (Talos's hardening pins it to 0, which would fail every runsc sandbox); substrate-only clusters
  bring their own machine config and must apply that sysctl themselves.

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

**DNS**: every cluster has a **cluster domain** — chosen at create (`--domain` / `domain:`),
immutable, unique across clusters, defaulting to `<cluster>.k8s.test`. `*.<domain>` → that
cluster's `.200`; `<node>.<domain>` → node IP; the domain apex itself has no record. Because the
wildcard covers every name under the domain, a name that is not a node's — a typo'd node name
included — answers with the VIP rather than NXDOMAIN; only the apex and names outside the domain
NXDOMAIN. Records
are substrate tied to the cluster's **existence, not its run-state**: the wildcard and the node
A records exist for the cluster's lifetime and keep answering authoritatively while it is
stopped — a stopped cluster's names resolve to addresses that will not respond — and only
`destroy` withdraws the records and the resolver files. Domains
may nest across clusters and resolve longest-suffix-wins — the owning cluster answers (or
NXDOMAINs) alone. Safe domains (`.test`, `.internal`, `home.arpa`) are accepted outright;
`.local`/`.localhost`/`.invalid`/single-label are always rejected; anything else can shadow
real DNS and requires the explicit `--allow-unsafe-domain` / `allowUnsafeDomain: true` opt-in
(non-interactive, so scripted paths stay deterministic). On macOS, the embedded resolver in
`tbxd` listens on `127.0.0.1`; default-domain clusters share the static
`/etc/resolver/k8s.test` file, while each custom-domain cluster gets `/etc/resolver/<domain>`,
written with an ownership marker by the helper, reconciled by `tbxd` (recreate missing,
remove marked orphans, never touch unmarked files), with a `killall -HUP mDNSResponder` after
changes. On Linux, the helper binds the cluster gateway's port 53 and passes the listener FD
to the unprivileged DNS server; guest queries are forwarded upstream, and when
systemd-resolved is available the helper registers `~<domain>` as a route-only domain on the
bridge through D-Bus. Hosts without resolved retain guest and by-IP access, and `tbx doctor`
prints the required `resolvectl` fallback without writing `/etc/resolv.conf`.

**BGP mode** (`tbx bgp enable <cluster>`): changing the mode on a live cluster reconciles both
sides — the host speaker, then a forced CNI pass that re-renders Cilium with `bgpControlPlane`
on (or off), rolls the agents so they read the new `cilium-config`, and applies the matching
announcement objects. The verb holds its answer for that pass and reports success only after it
converges; a cluster whose members are stopped records the mode and warns that Cilium still
announces the old way until every member runs. It is "host as ToR" — one embedded GoBGP instance,
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
the cache and doubles as the offline-venue answer. Containerd selects a catch-all upstream with
the standard `?ns=<upstream-authority>` query. Host OCI clients which cannot add that query use
`/v2/<upstream-authority>/<repository>/...` on port 5059 instead; the mirror strips exactly the
authority segment and applies the same canonicalization, cache namespace, offline policy, dynamic
handler limit, and public-address checks as the query form. A nonempty `ns` query wins when both
forms appear, while `/v2/` remains the namespace-free registry ping. Without `?ns=`, a first
repository path segment containing `.` or `:` is interpreted as the upstream host, so repositories
whose first segment looks like a host must use the `?ns=` form. A complete cached tag — its mapping, selected
`linux/<arch>` manifest, config, and every layer — remains available online during transient
upstream failures; the daemon logs
`mirror served stale: <host>/<repository>:<tag> (upstream <reason>; cache complete for linux/<arch>)`.
Anonymous Bearer challenges and `Retry-After` handling are registry-generic. The catch-all
deliberately redirects only
syntactic loopback authorities (`localhost`, loopback IPv4, or loopback IPv6, optionally with a
port) back to the same registry path inside the node, removing containerd's `ns` parameter. This
lets an in-cluster registry such as `localhost:30500` remain direct without weakening the
mirror-only rule for public registries. The passthrough infers transport from the port, following
containerd's localhost convention: HTTPS with no port or port 443, HTTP on every other port. A TLS
registry on a custom loopback port is therefore unsupported by this redirect and needs an explicit
`machine.registries.mirrors` entry. Because the redirect changes the host, credentials depend on
containerd re-authorizing the redirected request; its `CheckRedirect` does so. Hostnames which
merely resolve to loopback, and all other
private or non-public authorities, remain blocked by the host mirror. `tbx mirror offline`
reports its current mode. Offline stops the mirror from reaching registries and makes its cache
misses return 404; the node resolver then applies its separate fallback policy. An explicit mirror
entry with `skipFallback: false` may fall through to upstream, while talos-box's generated `"*"`
entry uses `skipFallback: true` and therefore remains a hard miss. Syntactic loopback registries
remain direct; `tbx mirror offline off` restores pull-through behavior. Mirror content is shared
cache state, not cluster state: it survives cluster destruction and recreation.

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
On the substrate-only path, nodes always come up **unconfigured** — talosbox generates and
applies machine config only when a curated `cni:` is declared (§1). Hand-generated configs that
leave `machine.network.hostname` unset get random `talos-*` hostnames from Talos, so the names in
`kubectl get nodes` will not match the `<cluster>-<role>-<i>` names `tbx status` shows; that is
Talos behavior, and leaving the mismatch alone is the simplest path. On Talos 1.13 `talosctl gen
config` already emits a `kind: HostnameConfig` (`auto: stable`) document, so adding
`machine.network.hostname` alongside it makes every `apply-config` fail with `static hostname is
already set in v1alpha1 config`; lining the two views up requires removing or replacing that
generated document in the same bundle.
`tbx status` reports each node's observed phase — `stopped`, `suspended`, `unreachable`,
`maintenance`, `configured`, or transiently `rebooted` — derived from a credential-free TLS probe of apid: **both** apid modes serve TLS
(empirical correction, #31 — the earlier "insecure = maintenance" model was wrong);
maintenance mode presents the well-known `maintenance-service.talos.dev` certificate, a
configured node presents its cluster-CA identity and demands a client certificate. `suspended`
is not a probe verdict — no VM is running to probe — but a stopped node holding its own saved
memory, and it still counts as "not running" for every rule keyed on stopped (#415).
For configured running nodes, the daemon also samples authenticated Talos `SystemStat.boot_time`
on every status refresh: the watcher's 30-second cadence and every `tbx status` request. A changed
nonzero value without a host VM restart is logged once
and shown as `rebooted` for 15 minutes; deliberate VM starts clear the baseline. The baseline is
in-memory, so a daemon restart cannot classify a guest reboot that occurred before the first new
sample, and missing exact-context Talos credentials leave reboot classification unavailable.

## 7. Snapshots and reset

([Snapshot and reset story](https://github.com/randax/talos-box/issues/7))

**Cold, whole-cluster, named, manual**: `tbx snapshot create|restore|list|delete <cluster>
[name]`. Create/restore stop the cluster (with confirmation), `clonefile` every node disk as
one crash-consistent set, and restart. No per-node snapshots (etcd split-brain bait), no
auto-snapshots, no live checkpoints — restore always passes through a stop; a restore costs a
~1-minute cold boot. macOS uses APFS clonefile; Linux falls back to a full raw-image copy when
the filesystem clone primitive is unavailable.

A restore reverts the surviving nodes' disks to the snapshot as well, so by design every
volume write made after the snapshot is rolled back — the gate below covers only the disks that
disappear entirely, not that intended rollback. A restore deletes every live node the snapshot
did not capture, disks included, so those nodes are volume-gated like `tbx node remove` (§9):
when the engine reports volume data on them, the restore refuses and names each node with its
volume count; `--force` overrides and warns about the data it deletes. No surviving node can
vouch for a copy, precisely because its disk is reverted too. The observation is best-effort —
a stopped or unreachable cluster never blocks a restore and degrades to a warning that the
deleted disks' volume data was unverified.

## 8. Resource model

([Resource model and efficiency](https://github.com/randax/talos-box/issues/8))

- **Default topology: 1 control plane + 2 workers, 2 GiB RAM / 2 vCPU per node** (6 GiB
  total). All sizes overridable per role in `talosbox.yaml`. HA control planes via scale-up,
  not default.
- **Active memory ballooning** (owner decision): on macOS, `tbxd` monitors host memory pressure and
  inflates virtio balloons proportionally across running configured nodes when host free memory
  drops at least **256 MiB** below the reserve, never below a **1 GiB per-node floor**, and applies
  successful aggregate retargets at most once per minute. Swap at least 80% used, compressor
  occupancy at least 20% of RAM (4 GiB when total RAM is unavailable), or warning/critical kernel
  pressure latches a release veto: those signals never manufacture reclaim, but prevent a real
  reclaim from flapping back out until swap is below 70%, compressor occupancy is below 15%
  (3 GiB fallback), and kernel pressure has cleared. Admission pre-balloon holds bypass both the
  deadband and rate limit.
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
                  [--cni cilium|flannel] [--csi longhorn|local-path] [--lb] [--bgp] [--hubble]
                  [--talos-version VERSION] [--schematic ID] [--extensions LIST]
tbx node add <cluster> [node] [--role worker|control-plane] [--force] [--quiet]
tbx node remove <cluster> <node> [--force] [--quiet]
tbx node start <cluster> <node> [--force] [--quiet]
tbx node stop <cluster> <node> [--quiet]
tbx cluster suspend|resume <cluster>
tbx snapshot create <cluster> [name] [--yes] [--quiet]
tbx snapshot restore <cluster> <name> [--yes] [--force] [--quiet]
tbx snapshot list [cluster] [-o json]      tbx snapshot delete <cluster> <name>
tbx status [cluster]      tbx manifests <cluster> [section|images] [--cni cilium|flannel]
tbx console <cluster> <node> [--no-follow [--lines N]]
tbx bgp enable|disable <cluster> [--quiet]      tbx bgp status <cluster>
tbx mirror offline [on|off]
tbx cache pull [-f talosbox.yaml] [--no-images]
               [--talos-version VERSION --schematic ID --extensions LIST]
tbx cache warm [--refresh] [--jobs N] <list-file> [<list-file>...]
tbx cache warm --check [--deep] <list-file> [<list-file>...]
tbx cache list [<image-ref>] [-o json]
tbx cache prune [--mirror|--all]
tbx logs [cluster] [--cluster name] [--follow] [--lines n]
tbx doctor      tbx system install|uninstall|restart [--force]|status|logs
tbx version (also --version, -v)
```

`tbx cache list` reports Talos disk images and mirror-cache totals, labelling each disk-image
combination with every keep-reason that applies — `in-use` (naming the clusters that reference
it), `pinned`, `default` — or `orphan` when none does; an image that is both pinned and in use
reads `pinned, in-use (<cluster>)`, because a prune keeps it for both reasons.
`tbx cache list <image-ref>` answers a different question: whether that one container image is
cached completely enough to be served offline (`cached` / `not cached (<reason>)`). It is a
query, not a gate, so it exits 0 either way; `tbx cache warm --check` is the gate.
A combination holding only leftovers — a compressed archive, an unusable disk image, or a lone
pin marker — is listed too, marked `(incomplete)`, so the listing is a complete prune preview.
Cache pruning is scope-limited: without a flag it removes disk images only and leaves mirror
content intact; `--mirror` removes mirror content only; `--all` removes both and clears pins.
These are the only cache-prune scopes. The default scope is additionally **reference-aware**:
it deletes only the combinations `cache list` calls `orphan` — referenced by no persisted
cluster, not pinned by an explicit pull, and not the built-in default — and prints each one with
its size before deleting. Nothing in the cache is ever deleted automatically; a file-aware pull
reports stray pinned combinations but removes nothing.

Schema (v1):

```yaml
version: 1
talos:
  version: v1.13.9        # optional; defaults to the release's pinned version
  schematic: ""           # optional Image Factory schematic id
  extensions: []          # optional curated Talos extensions, bare short names:
                          # gvisor|nfs-utils|qemu-guest-agent
clusters:
  - name: demo
    controlPlanes: 1
    workers: 2
    hypervisor: qemu      # optional per-cluster hypervisor: vz | qemu
    talos: {}              # optional per-cluster override of the file-level talos
                           # block; unset fields inherit, set fields override,
                           # extension lists override (extensions: [] = none)
    cni: cilium            # optional curated CNI: cilium|flannel; absent = substrate only
    csi: longhorn          # optional curated storage: longhorn|local-path; requires cni
    lb: true               # LoadBalancer support with the curated CNI
    bgp: false
    hubble: false          # Cilium Hubble Relay and UI
    kubeletMemoryProtection: false # optional curated-provisioning opt-out; default true
    domain: lab.internal   # optional cluster domain; default <name>.k8s.test
    allowUnsafeDomain: false # explicit opt-in for domains that can shadow real DNS
    node:                  # defaults for all nodes
      memory: 2GiB
      cpus: 2
      diskSize: 20GiB
    controlPlane: {}       # optional per-role overrides of `node`
    worker: {}
```

**Guest-memory protection semantics.** Curated provisioning adds reclaim-oriented sysctls to
every generated machine config. It scales `vm.min_free_kbytes` from each role's memory at
32 KiB per MiB, clamped to 16,384–262,144 KiB, and defaults
`vm.watermark_scale_factor` to `200` and `vm.vfs_cache_pressure` to `50`. On nodes with at least
2 GiB it also defaults kubelet's hard `memory.available` eviction threshold to `300Mi` and its
system memory reservation to `512Mi`; smaller nodes keep the sysctls only, since that
reservation would leave them almost nothing schedulable. User-supplied values win and unrelated settings are preserved.
`kubeletMemoryProtection: false` omits only those kubelet eviction and reservation defaults;
the reclaim sysctls still apply. Unlike `hubble`, the setting is baked into machine config at
node creation: changing it in `talosbox.yaml` is persisted by `tbx up` and shapes nodes added
afterwards, but already-configured nodes are not re-patched (use `talosctl patch mc`). Like `hubble`, the field is provisioning-only and is rejected
without a curated `cni:`. Substrate-only users own their machine config and can apply the same
policy manually; [Guest memory and reclaim](guest-memory.md) documents the patch, trade-offs,
and the bring-your-own-schematic balloon opt-out.

**Hypervisor selection.** Cluster `hypervisor` overrides `TBX_HYPERVISOR`, which overrides the
compiled default. The daemon stores the resolved choice in cluster state when a cluster is
created; an empty legacy state value means the platform's compiled default, not the current
environment-selected default. `tbx up` refuses drift with `cluster "<name>": hypervisor is
immutable (cluster has "<old>", talosbox.yaml wants "<new>"); destroy and recreate the cluster
to change the hypervisor`. `tbx status` reports the resolved choice in its `HYPERVISOR` column.

**Curated CSI semantics.** `csi:` is a flat scalar and rejects any value without a curated
`cni:` before anything mutates. Longhorn (pinned v1-series) is the multinode engine; local-path
(pinned) is the lightweight single-node option. Both keep their data under `/var` on the node
disks, mark their StorageClass as the cluster default, and derive everything else from cluster
facts: Longhorn's replica count follows the nodes that can host replicas — the workers, or the
control planes of a worker-less cluster (which tbx makes schedulable) — capped at 3, so volumes
are healthy by construction rather than pinned above what can ever schedule. Storage counts as **live** only
after a real end-state probe — a bare PVC binds against the default StorageClass, a writer pod
writes, a reader pod reads the data back, and the probe objects are cleaned up. Until then
`tbx status` reports storage as **provisioning**; a single-node Longhorn cluster gains a
status hint that its volumes have no redundancy, and choosing Longhorn on a memory-tight host
prints a soft pre-flight warning (never a hard gate). Storage is ordinary provisioned state —
`tbx up` converges the storage stage from any interruption and `tbx down`/restarts preserve
volume data. Cluster-level operations never delete user data except `tbx cluster destroy`:
switching or removing `csi:` is allowed only while the engine holds zero volumes, and the hard
error otherwise names the blocking volumes by `namespace/name` — capped, with an `and N more`
for a long list — so the operator can go and delete exactly what holds the switch. A switch that proceeds
tears the old engine down completely: leaving Longhorn removes every cluster-scoped object
Longhorn installed for itself — its validating and mutating webhook configurations, the
`longhorn-static` StorageClass, its CSIDriver registration — **before** the admission Service
they point at, because a webhook configuration outliving its Service fails closed and would
reject every PVC bind in the cluster. The destroy confirmation reports a best-effort volume count without ever
blocking the destroy of an unreachable cluster. A destroy closes with a summary of what it
removed — nodes, bytes of cluster state removed (a per-file allocated-block sum, so extents
cloned from the image cache or shared with a snapshot count once per file rather than once per
physical block: it is not a measure of capacity freed), snapshots deleted, the DNS or resolver entry withdrawn,
and any volumes it warned about — so the scope of the destruction can be checked without a
residue check by hand. `tbx node remove` deletes that node's disk, so
it is volume-gated the same best-effort way: when the engine reports the node holds the only
copy of volume data — local-path volumes pinned to it, Longhorn volumes with no healthy replica
elsewhere — the removal is refused unless rerun with `--force`, and a stopped or unreachable
cluster never blocks removal but degrades to a data-loss warning instead. `tbx snapshot
restore` deletes the disks of every node the snapshot did not capture, and is gated the same
way — except that no surviving node counts as a safe copy, since a restore reverts its disk
too (§7).

## 10. Guided output

`tbx status` is **state-aware**: alongside nodes/IPs/DNS names/hypervisor/LB pool/BGP state and
the storage phase (provisioning → live, gated by the write/readback probe, §9) it appends
copy-pasteable next-step hints keyed to observed state (maintenance node → the
`talosctl --insecure` probe; provisioned clusters report convergence progress and the
recovery that actually applies — a safe `tbx up` rerun for clusters a `talosbox.yaml` backs,
and destroy-and-recreate guidance naming the recorded intent, never a fabricated `cluster
create` line, for imperatively created ones; a cluster created before the origin was recorded
keeps the `tbx up` wording, because tbx cannot prove no file backs it and advising a destroy on
a guess is the worse error; substrate-only clusters retain manual guidance).
Hints **never execute anything**. `--quiet` suppresses hints and narration but keeps facts
(schematic/extensions lines, reboot observations, and the table-only host swap advisory when at
least one returned cluster is running) and liveness (the deadline preamble, held back for a 5s
grace so the usual no-op — which answers immediately — announces nothing, and worded
conditionally (`checking demo; if provisioning is needed it may take up to 14m`) until the
daemon narrates a stage, after which it states the window outright, plus a periodic
stderr heartbeat during blocking provisioning calls); all list/status commands support
`-o json`. The heartbeat names the request-wide bound `overall deadline`, covering every phase
the daemon holds the request for — image prepare, node boot, readiness, provisioning — and
distinct from the per-phase budgets the daemon narrates (`CNI budget`, `CNI+storage budget`).
The CLI stops waiting once that bound plus a grace passes **without any sign of life**: a
narrated call re-arms the wait on every stage the daemon sends, so only a gate that goes silent
fails the verb instead of hanging it.

`tbx doctor` also prints an INFO `Hypervisors` section before the rest of the checks. Each line
names one hypervisor, its availability, the current default source (`default=yes (source=compiled)`
or `default=yes (source=TBX_HYPERVISOR)`), and the feature gates used by `tbx status`. When a
hypervisor is unavailable, the line carries its exact reason and, if the hypervisor supplied
one, `remediation: <text>` inside the parentheses.

**State-changing verbs narrate their stages.** `cluster create`, `snapshot create|restore`,
`node add|remove` and `node start|stop` stream the daemon's stages to stderr as the work
proceeds — stopping the cluster, cloning disks, restarting, waiting for nodes — closing with a
convergence hint where the verb left nodes booting, and `--quiet` suppresses the stages while keeping the result and
its warnings. A verb's success line is past tense because it is true when printed: `cluster
create` holds its answer until the nodes it started answer on apid (maintenance or configured),
up to a bounded boot budget — when the budget runs out, or the daemon's lifecycle is cancelled,
it answers anyway with an advisory naming the nodes that stayed silent, so a successful exit
never proves the nodes answered. `cluster create`, `cluster resume`, `snapshot create|restore`
and `node add|remove` print their warnings above the success line, not after it; `cluster
start|stop|suspend` and `node start|stop` print theirs below it. `cluster resume` warns first
because its success line carries the cold-booted count, which the warnings explain.

**`tbx console <cluster> <node>`** attaches interactively to the node's serial console (hvc0)
through the `tbxd`-owned socket — Talos renders its console dashboard and logs there, and
maintenance-mode debugging works before any config exists. Detach with **`Ctrl-]`**; the
session banner states the detach key. Attaching never blocks the VM; multiple attach/detach
cycles are supported. **`--no-follow`** makes the console scriptable: it dumps the ring buffer
the attach replays, writes it to stdout and exits, with **`--lines N`** keeping only the last N
lines. There is no `--since`: the ring buffer holds the guest's raw bytes with no host
timestamps to cut a duration on. `tbx manifests` is the exact inspection/fork surface for the declared
curated path: its `machine`, `values`, `objects`, and `extras` sections match the patch,
pinned Helm release, rendered objects, and LB/BGP/MetalLB probe resources tbx applies. The gate
is per section: `machine`, `mirrors`, `images`, and the `storage` streams are substrate and
render for a cluster that declares no CNI, while the CNI-derived sections (`values`, `objects`,
`extras`, `lb-pool`, `l2`, `bgp`) refuse with a message naming `--cni`. `tbx manifests <cluster>
<section> --cni cilium|flannel` renders what tbx **would** apply for that curated CNI on a
substrate-only cluster; naming a CNI that contradicts a declared one is refused. Its
`storage` section always includes the kubelet mount prerequisite and privileged-namespace PSA
guidance, including for a substrate-only cluster. That guidance is about the CSI's own namespace;
every other namespace is governed by the cluster-wide default, which enforces `baseline` and
warns/audits at `restricted` wherever a namespace carries no `pod-security.kubernetes.io/*`
labels of its own — `default` included. A workload applied there that is not `restricted`-compliant
is still admitted and prints a `would violate PodSecurity "restricted:latest"` block: a warning,
not a failure. A workload that genuinely needs `hostNetwork` or privileges exceeds `baseline`, so
it needs its own namespace labelled `privileged` the same way a CSI namespace does. If `csi:` is declared it also includes the
exact Longhorn values and renderer-derived namespace, CRD, and post-CRD object streams, or
local-path namespace and object streams, that tbx applies; use `storage-machine`,
`storage-values`, `storage-namespaces`, `storage-crds`, and `storage-objects` as the clean
hand-application streams described in the output. Bring-your-own CSI is unsupported above the substrate. Engines needing extensions
beyond the default schematic use the existing `talos.schematic` override; tbx does not compose
schematics from extension lists.

For a hand-managed substrate-only CSI, save `tbx manifests <cluster> storage-machine > storage-machine.yaml`
and get the resulting patch onto **every node**. The route depends on node state, and the printed
header presents both because it cannot tell which the reader has: nodes still in maintenance mode
have no machine config to patch, so the document is folded in at generation time with `talosctl gen
config <cluster> https://<cp-ip>:6443 --config-patch @storage-machine.yaml` before `talosctl
apply-config --insecure`; already-configured nodes take it directly with `talosctl patch mc -p
@storage-machine.yaml --nodes <node-ip>`. Then, before installing the CSI, create its namespace if needed and apply the
printed PSA labels. Curated CSI namespace streams carry their own PSA labels. Unlabelled
namespaces — `default` included — fall to the cluster-wide default of `baseline` enforcement with
`restricted` warn/audit, so an ordinary test workload applied there is admitted but prints a
`would violate PodSecurity "restricted:latest"` warning block, while a `hostNetwork` or privileged
one needs its namespace labelled `privileged`. The printed
prerequisite mounts `/var/local-path-provisioner`, the path tbx's curated local-path CSI writes
to; upstream local-path-provisioner's shipped ConfigMap defaults to `/opt/local-path-provisioner`,
so the `storage` section also prints the `local-path-config` edit that repoints it at the mounted
path. A BYO install that skips that edit gets a bind mount nothing uses. For Longhorn,
apply `storage-namespaces`, then `storage-crds`, run the printed Established wait against the
CRD stream, and only then apply post-CRD `storage-objects`; it is not a one-shot apply.
Longhorn's `storage-values` stream records the exact values used to render those objects.
The inspection streams render the cluster's **declared** topology, which is what tbx applies in
every case but one: a cluster that lost its last worker while control planes still hold Longhorn
replicas keeps the control-plane toleration in the applied render until those replicas drain,
because the components serving them cannot be evicted from where the data is. Inspection does not
reach into a live cluster to observe replica placement, so for that one case `storage-values` and
`storage-objects` omit a toleration the applied render still carries; it disappears from the
applied render too once the replicas have moved.
Local-path has no CRD barrier, so apply `storage-namespaces` before `storage-objects`.

## 11. Distribution

### macOS

- **Homebrew** (`brew install randax/tap/talosbox`); binary is Developer-ID signed and
  notarized with the `com.apple.security.virtualization` entitlement — no restricted
  entitlements needed (bridged networking deliberately unused).
- **`tbx system install`** (one-time; automatic absolute-path sudo re-exec) installs
  `tbx-helper` as a root launchd daemon and
  the `/etc/resolver/k8s.test` file; `tbx doctor` verifies helper, resolver, DNS wiring, and
  forwarding (full check table: `docs/macos.md` / `docs/linux.md`). Everything else runs unprivileged. The helper's macOS filesystem writes are
  confined to `/etc/resolver/k8s.test` plus `/etc/resolver/<domain>` for canonical validated
  domain names; it refuses non-canonical names, never follows symlinks or touches unmanaged
  files, and only ever deletes files carrying its ownership marker. Like every helper
  operation, the domain set itself is trusted from the authorized client (the daemon derives
  it from cluster state) — the helper validates shape, not provenance.

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
  per-node floor, 5s poll, 256 MiB deadband, and 60s minimum successful-retarget interval**
  (`TBX_BALLOON_RESERVE_MIB` overrides the reserve). Verified live:
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
