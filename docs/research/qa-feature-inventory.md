# QA feature inventory — talos-box on main

Resolves #212 (part of #211). Raw material for the QA coverage matrix: every user-facing
feature and configuration knob, with platform presence, one-line contract, and risk notes
where history shows gotchas. Sources: `docs/SPEC.md`, `README.md`, `docs/linux.md`,
`cmd/tbx`, `internal/config`, `internal/cluster/provisioning.go`, `internal/daemon`,
`internal/mirror`, `internal/manifests`, `internal/provision`.

Platform columns: **mac** = macOS/Apple Silicon (Virtualization.framework via vz),
**lin** = Linux amd64/arm64 (QEMU/KVM over QMP). "both" means the contract is identical;
gates are noted where they are not.

## 1. Lifecycle verbs

| Feature | mac | lin | Contract | Risk notes |
|---|---|---|---|---|
| `tbx up [-f file] [--force] [--quiet]` | yes | yes | Idempotent reconcile of reality to `talosbox.yaml`; prints per-cluster actions (created/started/reconciled/up-to-date) plus warnings and narration | `--force` overrides overcommit guard, which only samples on macOS today; performs a protocol handshake (`daemon.info`) when the file declares provisioning intent — old `tbxd` is rejected with an upgrade message |
| `tbx down [-f file]` | yes | yes | Inverse of `up`: stops declared clusters; reports not-running and missing clusters distinctly | — |
| `tbx cluster create <name>` | yes | yes | Creates and starts; prints the equivalent `talosbox.yaml` stanza; flags: `--cp` (def 1), `--workers` (def 2), `--memory-mib` (2048), `--cpus` (2), `--disk-gib` (20), `--talos-version`, `--schematic`, `--domain`, `--allow-unsafe-domain`, `--cni`, `--csi`, `--lb` (def true), `--bgp`, `--hubble`, `--quiet`, `--force` | First run downloads the Talos image; intent flags trigger the tbx/tbxd protocol handshake; macOS node disks are APFS clonefile clones, Linux copies the cached raw image (slower, more disk) |
| `tbx cluster start/stop <name>` | yes | yes | Start supports `--force` and `--quiet`; stop keeps disks | Start replays the overcommit guard (macOS sampler only) |
| `tbx cluster suspend/resume <name>` | gated: macOS 14+ AND same `tbxd` process | gated: QEMU 8.2+ | Saves/restores guest memory; unsupported backends refuse with the capability reason from `hypervisor.Capabilities().Suspend` | **Suspend boundary is the classic gotcha.** macOS resume after a daemon restart warns and cold-boots (vz `ErrorRestore` when file-handle-backed device identity is gone). Linux save survives a daemon restart but restore requires the same QEMU version + architecture + machine type; mismatch warns and cold-boots. QEMU 6.2–8.1 supports everything except suspend |
| `tbx cluster destroy <name> --force` | yes | yes | `--force` mandatory (no interactive fallback); runs `cluster.destroy.inspect` first and prints a data-loss warning | Mirror cache deliberately survives destroy — cluster state and cache state are separate |
| `tbx cluster list [-o json]` | yes | yes | Table or JSON summary | — |
| `tbx node add <cluster> [node] [--role worker\|control-plane] [--force]` | yes | yes | Adds a node to a running cluster; deterministic MAC from cluster+node name keeps DHCP/DNS stable | Replays overcommit guard (macOS-only sampler) |
| `tbx node remove <cluster> <node>` | yes | yes | Removes a node | — |
| `tbx node start/stop` | **no** | **no** | Listed in SPEC §9 command block but **not implemented** in `cmd/tbx/main.go` (`runNode` accepts only add/remove) | Spec/code divergence — QA should pin down which is authoritative |
| `tbx version` | yes | yes | Prints the build version | — |

## 2. `talosbox.yaml` schema (v1) and validation

Strict decoding (`KnownFields(true)`): any unknown key is a hard error. `version: 1` required;
at least one cluster required.

| Knob | mac | lin | Contract | Risk notes |
|---|---|---|---|---|
| `talos.version` | both | both | Overrides the release-pinned default (currently v1.13.6); only the pinned default is CI-verified | Non-default versions are explicitly untested territory |
| `talos.schematic` | both | both | Image Factory schematic id; tbx appends the mandatory `console=tty0 console=hvc0` args | Arg order/pair is load-bearing: Factory extraKernelArgs *replace* defaults and `console=hvc0` alone bricks vz boot (gate G6 evidence). Schematic is also the only extension path — tbx never composes schematics from extension lists |
| `clusters[].name` | both | both | `^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`, unique | Name feeds the default domain; a name whose derived `<name>.k8s.test` is not a canonical DNS name is rejected |
| `controlPlanes` / `workers` | both | both | Defaults 1/2; CP >= 1, workers >= 0 | — |
| `domain` | both | both | Immutable at create, unique across clusters, default `<name>.k8s.test`; `*.<domain>` → `.200` VIP, `<node>.<domain>` → node IP, apex has no record; nesting resolves longest-suffix-wins | Spelling the default explicitly normalizes to empty (so it does not read as custom) |
| `allowUnsafeDomain` / `--allow-unsafe-domain` | both | both | Safe suffixes (`.test`, `.internal`, `home.arpa`) accepted; `.local`/`.localhost`/`.invalid`/single-label always rejected; anything else requires this explicit non-interactive opt-in | Unsafe domains can shadow real DNS — that is the point of the gate |
| `node.memory` / `cpus` / `diskSize` | both | both | `<n>GiB` or `<n>MiB` strings; disk must be whole GiB; defaults 2GiB/2/20GiB | Sparse 20 GB disks: real usage grows over time |
| `controlPlane:` / `worker:` | both | both | Per-role overlays of `node` | — |
| `cni: cilium\|flannel` | both | both | Opts into the curated path: machine config, bootstrap, pinned CNI, LB reconciled from the host; empty = substrate-only (nodes stay in maintenance mode, tbx never applies config) | Cilium's built-in ingress controller is disabled by design; flannel path uses MetalLB for LB and status prints the flannel NetworkPolicy limitation |
| `csi: longhorn\|local-path` | both | both | Curated storage on top of a curated CNI | **Requires `cni`** (validated with a specific error). BYO CSI above the substrate is unsupported; Longhorn hand-apply is *not* one-shot (namespaces → CRDs → Established wait → objects) |
| `lb` | both | both | Defaults **true when `cni` is set**; installs the LoadBalancer pool (`.200–.239`) | Requires `cni`; even an explicit `lb: false` without `cni` is rejected (pointer semantics distinguish omitted from false) |
| `bgp` | both | both | Cilium BGP announcements instead of L2 | **Requires `cni: cilium` AND `lb: true`**; see §4 for the runtime interaction |
| `hubble` | both | both | Cilium Hubble Relay + UI | **Requires `cni: cilium`** |

All five intent knobs are also `cluster create` flags; the CLI refuses them against a `tbxd`
whose protocol version predates provisioning intent (both directions: old daemon and old CLI
are each rejected with an explicit upgrade message).

## 3. Networking substrate

| Feature | mac | lin | Contract | Risk notes |
|---|---|---|---|---|
| Per-cluster subnet | vmnet shared-mode net, pinned `/24`, helper-created | `br-tbx<n>` bridge, STP off, gateway `172.30.<n>.1/24`, tap per node | Cluster *n* → `172.30.<n>.0/24`; layout identical: `.1` host services, `.2–.179` DHCP, `.200–.239` LB pool (`.200` ingress VIP by convention), `.240–.254` reserved | Linux subnet selection rejects collisions with existing host routes/addresses and suggests a free subnet; macOS relies on vmnet pinning |
| DHCP | vmnet dynamic leases | helper static reservations from deterministic MACs | Node IPs stable across restarts via fixed MACs | macOS leases are vmnet-owned (dynamic); Linux reservations are deterministic — different failure modes for IP churn |
| Inter-cluster routing | helper routes `172.30.0.0/16` between attachments (learns node/VIP ownership from DHCP+ARP) | kernel routes between bridges via converged forwarding | First-class contract: host↔node, host↔VIP, cluster↔cluster (nodes and VIPs) | macOS path is hardware-validated (G5 closed); **Linux full-cluster contract is only component/integration-tested — e2e gate is #97** |
| Host firewall/forwarding | helper converges forwarding | helper owns `table inet tbx` only, never edits foreign nftables; reconverges all state on startup (survives reboot by re-creation) | NAT egress via `.1` | Docker default-DROP `FORWARD` + bridge netfilter breaks forwarding — `doctor` prints a targeted rule, tbx never edits foreign tables. Strict rp_filter discards VIP traffic on multi-homed/VPN hosts |
| DNS | `tbxd` resolver on `127.0.0.1`; `/etc/resolver/k8s.test` static + `/etc/resolver/<domain>` per custom domain, ownership-marked, `killall -HUP mDNSResponder` after changes | helper binds gateway `:53`, FD passed to unprivileged server; systemd-resolved route-only `~<domain>` via D-Bus when available | Wildcard + node records per cluster domain; upstream forwarding for guests | **Resolver files are a known gotcha area:** tbxd reconciles (recreate missing, remove marked orphans, never touch unmarked files) — orphan/marker behavior needs coverage. Linux hosts without resolved keep guest/by-IP access only; `doctor` prints the `resolvectl` fallback and never writes `/etc/resolv.conf` |
| `tbx bgp enable\|disable <cluster>` | yes | yes | "Host as ToR": GoBGP in tbxd, host ASN 64512 on `.1:179`, cluster *n* nodes ASN 64600+n eBGP; learned routes → host FIB via helper (PF_ROUTE / rtnetlink) | **When enabled, BGP replaces L2 announcements for the LB pool** (mechanisms teachable in isolation). Pod-CIDR advertisement accepted, not guaranteed. Port 179 conflicts surface in `doctor` |
| L2 VIP failover | slow: macOS ignores GARP through vmnet; converges via ARP revalidation in ~40–50 s (G2) | normal bridge L2; GARP works, no host-side GARP machinery needed | Default ingress-VIP path when BGP is off | **The slow-failover caveat is macOS/vmnet-specific** — BGP is the fast-failover path there; on Linux BGP is only for routed upstreams/ECMP/`externalTrafficPolicy: Local`. Residual: repeated-GARP bursts untested |

## 4. Registry mirror and offline mode

| Feature | mac | lin | Contract | Risk notes |
|---|---|---|---|---|
| Pull-through mirror | yes | yes | Mirrors `docker.io`, `ghcr.io`, `quay.io`, `registry.k8s.io` on legacy fixed ports 5055–5058 plus catch-all `:5059`, bound on each **cluster gateway IP** only (binds added at cluster start, removed at stop), never `0.0.0.0` | Mirror exists because corporate agents (GlobalProtect) RST guest-originated TLS — **G4 (mirror through security agents) is still open**. **Port 5000 is macOS AirPlay Receiver** (answers 403, poisons smoke tests) — hence 5059; any test touching mirror ports on macOS must avoid 5000 |
| `skipFallback: true` | yes | yes | New machine configs set `machine.registries.mirrors."*"` → single endpoint `http://172.30.<n>.1:5059` with `skipFallback: true`, so nodes never bypass the mirror | Interaction by design: with skipFallback, a mirror outage or offline-mode cache miss is a hard image-pull failure — nothing falls back to upstream from inside the node. Legacy 5055–5058 listeners exist only so pre-catch-all clusters keep working until recreated |
| `tbx mirror offline [on\|off]` | yes | yes | No arg reports the mode; `on` serves cached content only (misses fail with "mirror offline: content not cached", no upstream dial); `off` restores pull-through | Mode survives daemon restarts (tested); offline serving covers both tag and digest request paths — digest-vs-tag serving parity is a tested invariant worth keeping in the matrix |
| Legacy cache layout fallback | yes | yes | The mirror falls back to the old flat underscore-flattened manifest filenames when reading | **After upgrading from the flat layout, rerun `tbx cache warm` before going offline:** verified digest entries remain reusable, but legacy *tag* entries are deliberately ignored (old filenames cannot prove repository identity) (#185) |
| Cache/cluster independence | yes | yes | Mirror content is shared cache state: survives cluster destroy/recreate | — |

## 5. Cache commands

| Feature | mac | lin | Contract | Risk notes |
|---|---|---|---|---|
| `tbx cache pull [--talos-version --schematic]` | yes | yes | Eagerly fetches one Talos disk image per schematic+version+arch into `~/.talosbox/cache/<schematic>/<version>/<arch>/` | Arch follows the host (no emulation); default schematic is computed (vanilla + console args) |
| `tbx cache warm <list>... [-]` | yes | yes | Warms mirror content from one or more list files (`-` = stdin, once); blank lines and `#` comments ignored; each entry fully qualified with a non-`latest` tag or `sha256`/`sha512` digest; tag+digest = immutable form (resolution mismatch against the pinned digest is an error) | Requires a current-protocol tbxd (handshake). `latest` tags are rejected by design |
| `tbx cache warm --check [--deep]` | yes | yes | Verifies the cached manifest graph and host-platform image **offline** (tested to touch no network); `--deep` also rehashes blobs and is valid only with `--check` | Verifies cache completeness, **not** a live-cluster pull — G4 remains uncovered by it |
| `tbx cache list` | yes | yes | Talos disk images (schematic/version/arch/size) and per-upstream + total mirror blob/manifest counts and bytes | — |
| `tbx cache prune [--mirror\|--all]` | yes | yes | No flag: disk images only, mirror untouched; `--mirror`: mirror only; `--all`: both; flags are mutually exclusive; these are the only scopes | Scope confusion is the historical hazard the explicit output ("mirror cache untouched") defends against |

## 6. Snapshots

| Feature | mac | lin | Contract | Risk notes |
|---|---|---|---|---|
| `tbx snapshot create\|restore <cluster> [name] [--yes]` | yes | yes | Cold, whole-cluster, named, manual; stops the cluster (interactive `[y/N]` confirmation unless `--yes`), snapshots every node disk as one crash-consistent set, restarts; restore costs a ~1 min cold boot | macOS uses APFS `clonefile`; **Linux falls back to full raw-image copy when the filesystem clone primitive is unavailable** — time and disk cost differ wildly. No per-node snapshots (etcd split-brain bait), no auto-snapshots, no live checkpoints |
| `tbx snapshot list\|delete <cluster> [name]` | yes | yes | List shows name + timestamp; delete removes one | — |

## 7. Resource model

| Feature | mac | lin | Contract | Risk notes |
|---|---|---|---|---|
| Default topology | both | both | 1 CP + 2 workers, 2 GiB/2 vCPU per node (6 GiB total); HA via scale-up | 16 GB host RAM minimum on both platforms |
| Active memory ballooning | **active** | **inactive** | tbxd samples host pressure every 5 s; inflates balloons proportionally across running *configured* nodes when free memory drops below the 6 GiB reserve, never below the 1 GiB per-node floor; deflates on release; `TBX_BALLOON_RESERVE_MIB` overrides the reserve | **Platform asymmetry:** the QEMU backend supports balloon target/readback, but the Linux host-free-memory sampler is not implemented, so the automatic policy does nothing on Linux. Maintenance-mode nodes are apid-probed out and exempt. Printed configs MUST include `machine.kernel.modules: [{name: virtio_balloon}]` (Talos ships it as a module) |
| Overcommit guard | **yes** | **no (currently)** | Before `up`/`start`/`node add`: warn when configured VM memory sum exceeds host RAM minus 6 GiB reserve; `--force` overrides | Backstop is macOS-only today; Linux `doctor` reports `host-pressure: SKIP` |

## 8. Observation and guided output

| Feature | mac | lin | Contract | Risk notes |
|---|---|---|---|---|
| `tbx status [cluster] [--quiet] [-o json]` | yes | yes | Per-node phase `stopped`/`unreachable`/`maintenance`/`configured` from a credential-free TLS probe of apid; state-aware copy-pasteable hints (never executed); distinguishes provisioning / Ready-without-LB / live VIP; prints credential exports and the flannel NetworkPolicy limitation | Phase detection was empirically corrected (#31): **both** apid modes serve TLS — maintenance presents the `maintenance-service.talos.dev` cert, configured presents cluster-CA and demands a client cert; the old "insecure = maintenance" model is wrong and must not creep back into tests |
| `tbx manifests <cluster> [section]` | yes | yes | Exact inspection/fork surface, rendered from the same functions the reconcilers consume. Sections: `all`, `machine`, `values`, `objects`, `extras`, `storage`, `storage-machine`, `storage-values`, `storage-namespaces`, `storage-crds`, `storage-objects`, `talos`, `cilium-values`, `metallb-values`, `metallb-extras`, `k8s`, `mirrors`, `balloon`, `lb-pool`, `bgp`, `l2` | Non-storage sections require a declared curated CNI (error otherwise); `storage` works on substrate-only clusters (kubelet mount prerequisite + PSA guidance always included); `metallb-*` only on the flannel path; Longhorn hand-apply ordering (CRD Established barrier) is documented in the output itself |
| `tbx console <cluster> <node>` | yes | yes | Interactive attach to the node's hvc0 serial socket owned by tbxd; detach with Ctrl-] (0x1d), banner states the key; never blocks the VM; repeated attach/detach supported; works in maintenance mode before any config | Residual from G6: the Talos dashboard TUI's interactive rendering on hvc0 is unverified (logs confirmed) |
| `tbx doctor` | yes | yes | Verifies platform helper, hypervisor, DNS/forwarding, routes, external image access; FAIL = non-zero exit, WARN = degraded-but-usable; checks needing a cluster report SKIP before one exists | Platform check sets differ. macOS: helper, vmnet, DNS wiring, forwarding, routes, **host pressure**, egress. Linux adds: `helper-unit`/`helper-access`/`helper-capabilities` (exactly the 3 caps), `kvm`, `qemu` (floor + machine + suspend availability), `forwarding`, `bridge-netfilter` (Docker FORWARD policy), `bridge-stp`, `rp-filter`, `port-53`/`port-67`/`port-179`, `resolver`/`DNS`/`system-dns` (resolvectl fallback), `routes`, `host-pressure` (always SKIP), egress + security inventory. Doctor prints exact remediation commands but never runs them |
| `--quiet` narration | yes | yes | `up`/`cluster create`/`cluster start --quiet` keep the final result but suppress stage narration; `status --quiet` drops hints | — |
| `-o json` | yes | yes | On `cluster list` and `status` (SPEC says "all list/status commands") | `snapshot list` and `cache list` have **no** `-o json` in code — spec/code divergence for the matrix |

## 9. Host integration and distribution

| Feature | mac | lin | Contract | Risk notes |
|---|---|---|---|---|
| `tbx system install [helper-path]` / `uninstall` | yes | **must not be used** | One-time root install of `tbx-helper` as launchd daemon + `/etc/resolver/k8s.test`; auto re-execs with sudo; validates the helper binary; uninstall removes helper and resolver integration | **The command is not GOOS-gated in `cmd/tbx/system.go`** — docs (README, linux.md) tell Linux users not to run it, but nothing in the code refuses; it would attempt launchd paths on Linux. QA should decide whether "documented-only" is acceptable |
| Helper privilege model | root launchd daemon; filesystem writes confined to `/etc/resolver/*` for canonical validated names, ownership-marker-gated deletes, no symlink following | `tbx` system user with exactly `CAP_NET_ADMIN`, `CAP_NET_BIND_SERVICE`, `CAP_NET_RAW`; socket-activated; polkit rule for resolved | Helper validates shape, not provenance — the domain set is trusted from the authorized client | `doctor helper-capabilities` asserts the exact cap set on Linux |
| Install channels | Homebrew (signed/notarized, `com.apple.security.virtualization` entitlement) | **source-preview only** today; Cloudsmith apt/dnf, AUR `tbx-bin`, Nix flake/`virtualisation.talosbox` are designed but unpublished (#95, #96, #101; gate G8) | — | Docs must not expose placeholder URLs; NixOS `/usr`-based source preview does not apply |
| Version/arch floors | macOS 14+, Apple Silicon only (G1 macOS 14/15 boot gate still open) | QEMU 6.2+, writable `/dev/kvm` (KVM API 12), `q35`/`virt` machine, matching OVMF/AAVMF, host/guest arch must match (no TCG) | 16 GB RAM both | Go 1.26 needed for the Linux source build |

## 10. Interaction flags (by design — must be tested as pairs)

1. **BGP replaces L2 for the LB pool.** `bgp: true` switches the announcement mechanism for
   `.200–.239`; L2 and BGP are never simultaneously active for the pool. On macOS this is also
   the fast-failover path (vmnet drops GARP); on Linux it is optional and for routed
   upstreams/ECMP/`externalTrafficPolicy: Local`.
2. **Intent dependency chain:** `csi` requires `cni`; `lb`/`bgp`/`hubble` require `cni`
   (even explicit `false` without `cni` is rejected); `bgp` requires `lb: true` AND
   `cni: cilium`; `hubble` requires `cni: cilium`; `lb` defaults to true only once `cni` is set.
3. **`skipFallback: true` × mirror availability × offline mode:** nodes have exactly one
   registry path (`.1:5059`). Offline mode turns a cache miss into a hard pull failure by
   design; the pair (skipFallback, offline) is the offline-venue contract and its failure mode.
4. **Offline mode × digest-vs-tag serving × legacy layout:** offline must serve both tag and
   digest requests from cache with zero upstream dials, including through the legacy
   flat-layout fallback; post-upgrade, legacy tag entries are ignored and `cache warm` must be
   rerun (digest entries survive).
5. **Suspend/resume × daemon lifetime × QEMU identity:** macOS memory-preserving resume is
   valid only within one tbxd process; Linux restore is valid only under identical QEMU
   version/arch/machine type. Both degrade to a warned cold boot, never an error-out.
6. **Ballooning × node phase:** balloon management applies only to *configured* nodes;
   maintenance-mode nodes are exempt (apid probe decides). Balloon policy also depends on the
   printed `virtio_balloon` kernel module reaching machine config.
7. **Cache prune scopes × offline prep:** default prune keeps mirror content (offline prep
   survives); `--mirror`/`--all` destroy it — pruning before travel and offline mode interact.
8. **Domain × resolver files × cluster lifecycle:** default-domain clusters share the static
   `/etc/resolver/k8s.test`; each custom domain gets its own marked file that must be created,
   reconciled, and orphan-removed as clusters come and go (macOS); on Linux the analog is the
   resolved route-only registration per bridge.
9. **Mirror binds × cluster lifecycle:** gateway-IP binds are added at cluster start and
   removed at stop; multiple clusters share the fixed ports on distinct gateways.
10. **Protocol handshake × intent knobs:** any cni/csi/lb/bgp/hubble use requires a
    version-compatible tbx↔tbxd pair, checked in both directions.

## 11. Honest platform-asymmetry summary

| Area | macOS (vz) | Linux (QEMU/KVM) |
|---|---|---|
| Validation status | build-from-source path validated end-to-end on hardware | runtime implemented; full-cluster e2e CI gate open (#97); packages unpublished (#95/#96/#101) |
| Suspend/resume | macOS 14+, same-daemon only | QEMU 8.2+ only; cross-daemon OK if QEMU identity matches |
| Ballooning | active policy | policy inactive (no host sampler); QEMU balloon plumbing present |
| Overcommit guard / host-pressure doctor check | active | absent / `SKIP` |
| Node disk provisioning | APFS clonefile (instant) | raw copy (slow) |
| Snapshot mechanism | clonefile | clone primitive when available, else full copy |
| L2 VIP failover | slow (~40–50 s, GARP ignored by vmnet) — BGP is the fast path | normal bridge L2; BGP optional |
| DNS host integration | `/etc/resolver/*` files + mDNSResponder HUP | resolved route-only domain via D-Bus; `resolvectl` fallback printed, `/etc/resolv.conf` never written |
| `tbx system install` | required one-time step | must not be run (macOS launchd installer; not code-gated) |
| Port 5000 hazard | AirPlay Receiver answers 403 (why catch-all is 5059) | n/a, but same 5059 layout everywhere |
| Doctor surface | helper/vmnet/DNS/forwarding/routes/pressure/egress | + kvm, qemu, caps, bridge-netfilter, bridge-stp, rp-filter, port-53/67/179, resolver trio, units |
| DHCP | vmnet dynamic leases | helper static reservations |
| Still-open gates | G1 (macOS 14/15 boot), G4 (mirror vs security agents) | G8 (release channels), G9 (full-cluster CI); G4 applies too |
