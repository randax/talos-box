# talosbox

talosbox (`tbx`) is a workshop-environment tool that manages the full lifecycle of
hypervisor-based Talos Linux VM clusters with production-style networking. It supports Apple
Silicon macOS hosts through Virtualization.framework and Linux amd64/arm64 hosts through
QEMU/KVM. See the [specification](docs/SPEC.md) for the complete design.

**Status:** pre-v1 scaffold. The macOS build-from-source path is the currently validated path.
The Linux host runtime is implemented, but published packages and the Linux end-to-end CI gate
are still pending; use the documented source-preview setup rather than guessed package URLs.

## Quick start

### 1. Prepare the host

| Host | Requirements | Setup |
|---|---|---|
| Apple Silicon macOS | M1 or newer, macOS 14+, 16 GB RAM minimum | Build and install the launchd helper below |
| Linux | amd64 or arm64, KVM, QEMU 6.2+, EFI firmware, 16 GB RAM minimum | Follow the [Linux host setup](docs/linux.md) |

#### macOS

Released binaries come from the Homebrew tap:

```sh
brew install randax/tap/tbx
tbx system install
tbx doctor
```

Homebrew and mise/manual installs do not mix; choose one triad (`tbx`, `tbxd`, and
`tbx-helper`) and keep it together. `tbx system install` automatically re-executes the absolute
client path through sudo when privilege is needed. After `brew upgrade tbx`, run it again: the
launchd helper keeps running the previous binary until it is reinstalled, and `tbx` refuses a
helper that speaks an older protocol until then. For an explicit privileged invocation, use
`sudo "$(command -v tbx)" system install`, never a PATH-dependent `sudo tbx ...`.

Or build from source:

```sh
make build
bin/tbx system install
bin/tbx doctor
```

`make build` produces three binaries in `bin/`: the `tbx` CLI, the `tbxd` daemon
(started automatically by `tbx` on first use — keep it next to `tbx`), and `tbx-helper`
(the privileged networking helper). `system install` registers `tbx-helper` as a root launchd
daemon and installs the `/etc/resolver/k8s.test` resolver. `doctor` verifies the helper, resolver,
DNS wiring, forwarding, routes, host pressure, mirror health, and external image access
(the full check table is in `docs/macos.md` and `docs/linux.md`).

#### Linux

Linux uses a socket-activated systemd helper with only `CAP_NET_ADMIN`,
`CAP_NET_BIND_SERVICE`, and `CAP_NET_RAW`; do not run `tbx system install`, which is the macOS
launchd installer. The [Linux host setup](docs/linux.md) covers supported distributions,
QEMU/KVM prerequisites, source installation, systemd activation, and every Linux-specific
`tbx doctor` check.

Cloudsmith apt/dnf repositories, the AUR `tbx-bin` package, and the Nix flake are planned
release channels but are not published yet. Their availability is tracked by
[#95](https://github.com/randax/talos-box/issues/95),
[#96](https://github.com/randax/talos-box/issues/96), and
[#101](https://github.com/randax/talos-box/issues/101).

### 2. Create a cluster

```sh
tbx cluster create demo --cp 1 --workers 2
```

When running from the repository, use `bin/tbx` in place of `tbx`. The first run downloads the
Talos image into `~/.talosbox/cache/`, then creates and starts the VMs. Nodes boot
**unconfigured** into Talos maintenance mode when you choose the substrate-only path. If you
request the curated Cilium path (`--cni cilium` or `talosbox.yaml` with `cni: cilium`), `tbx`
applies the machine prerequisites, boots the cluster, installs Cilium, and waits for the curated
LoadBalancer VIP to become live. In both cases, talosbox owns the substrate (VMs, networking,
DNS, and image delivery); curated CNI and LoadBalancer provisioning are opt-in.

On the substrate-only path you generate the machine config yourself, and Talos assigns each node a
random `talos-*` hostname unless you set `machine.network.hostname`. That is expected Talos
behavior: `kubectl get nodes` will then show `talos-*` names that do not match the `<cluster>-<role>-<i>`
names in `tbx status`. Leaving that mismatch alone is the simplest path: on Talos 1.13,
`talosctl gen config` already emits a `kind: HostnameConfig` (`auto: stable`) document, and adding
`machine.network.hostname` alongside it makes every `apply-config` fail with `static hostname is
already set in v1alpha1 config`. To line the two views up you must remove or replace that generated
`HostnameConfig` document in the same bundle.

Substrate-only machine configs do not receive talosbox's curated guest-memory defaults. See
[Guest memory and reclaim](docs/guest-memory.md) for the equivalent manual patch, the kubelet
trade-off, and the bring-your-own-schematic procedure for disabling virtio ballooning.

Add `--csi longhorn` (multinode, replicated) or `--csi local-path` (lightweight, single-node)
— or the `csi:` key in `talosbox.yaml` — for persistent storage; it requires a curated CNI.
The engine's StorageClass becomes the cluster default and Longhorn's replica count derives
from the nodes that can hold data — the workers, or the control plane when it is the only
node — capped at 3, so a bare PVC just works. `tbx status` reports storage as
provisioning until a real write/readback probe passes, then live; a single-node Longhorn
cluster is reminded its volumes have no redundancy, and Longhorn on a memory-tight host gets a
soft warning. Adding `csi:` to an already-provisioned cluster is just `tbx up`; switching or
removing it is refused while the engine holds volumes, and the refusal names them. Only `tbx cluster destroy` deletes
data wholesale, and its confirmation reports the volume count. `tbx node remove` deletes that
node's disk, so it is refused while the node holds the only copy of volume data
(`--force` overrides); an unreachable cluster never blocks removal — it warns instead.
Reading Longhorn volume health straight after a probe, a snapshot restart or a deleted PVC
can show a `volumes.longhorn.io` object in `deleting`/`degraded` with no PVC or PV behind it.
That is Longhorn converging, not damage or a leak: it clears itself, and tbx deletes the
volume behind its own probe as soon as the claim it belongs to goes.

Every imperative command prints the equivalent `talosbox.yaml` stanza. Alternatively, work
declaratively from the start: write a `talosbox.yaml` and run `tbx up` (idempotent — it
reconciles reality to the file; `tbx down` is its inverse).

```yaml
version: 1
clusters:
  - name: demo
    controlPlanes: 1
    workers: 2
```

### 3. Inspect and work with the cluster

```sh
tbx status demo            # nodes, IPs, DNS names, and copy-pasteable next-step hints
tbx console demo demo-cp-1 # attach to a node's serial console (detach with Ctrl-])
tbx console demo demo-cp-1 --no-follow --lines 50  # dump the console ring buffer and exit
tbx manifests demo         # exact machine patch, chart values/objects, and LB/BGP extras
tbx logs demo --follow     # the daemon's narration for one cluster, as it happens
```

`tbx logs` (also `tbx system logs`) prints the daemon log the background work narrates into —
provisioning reconciles, node lifecycle, ballooning targets, mirror offline misses — instead of
requiring `~/.talosbox/tbxd.log` to be known out of band. It prints the last 200 lines by
default (`--lines n`, `--lines 0` for the whole log), follows with `--follow`, and a cluster
name (positional or `--cluster`) keeps only that cluster's lines. Kubernetes client-library
output (klog, API deprecation and PodSecurity warnings) is kept out of that log: it goes to
`~/.talosbox/tbxd.k8s.log`, and the API warning handler is off unless `TBXD_K8S_WARNINGS` is
set in the daemon's environment.

`status` is state-aware: it distinguishes provisioning, Ready-without-LB, a live VIP, and
storage provisioning from probe-verified live storage, while
printing credential exports and the flannel NetworkPolicy limitation. While a provisioning
pass is running, the storage hint and `-o json`'s `storageGate`/`storageError` name the
convergence gate the pass is actually held at — the CSI readiness probe is only one of them —
and `tbxd.log` narrates the same blocker periodically, so a long wait says what it is waiting
on. When a pass aborts, storage settles at `storagePhase: failed` carrying the cause, instead
of reporting provisioning that nothing is doing any more. Hints never execute
anything; suppress them with `--quiet`. `tbx up --quiet` and `tbx cluster create --quiet` keep
their final result and facts, suppress stage narration, state the operation's overall deadline
up front (after a 5s grace, so a run that answers immediately — the usual no-op — announces
nothing), and emit a once-a-minute liveness line on stderr (`still provisioning demo (elapsed
1m, overall deadline 23m)`) so a long provision never looks hung. That overall deadline covers
every phase the daemon holds the request for, image prepare and node boot included. Per-phase budgets the daemon narrates are named
for their phase (`reconciling cilium on cluster demo (CNI budget 10m)`), so they read apart
from that overall deadline; a call that goes silent past its overall deadline fails instead of
waiting forever, while one still narrating keeps its wait alive. `tbx manifests` is the exact inspection/fork
surface for the curated path: `machine`, `values`, `objects`, and `extras` match the machine
prerequisite patch, pinned Helm values, rendered chart objects, and LB/BGP resources `tbx`
applies. The substrate sections — `machine`, `mirrors`, `images`, and the `storage` streams —
render on a substrate-only cluster too, since the machine patch and the catch-all registry
mirror are substrate, not CNI. The CNI-derived sections (`values`, `objects`, `extras`, and the
`lb-pool`/`l2`/`bgp` filters) need a curated CNI: on a substrate-only cluster name the one you
intend to install by hand with `--cni cilium` or `--cni flannel`, and `tbx` prints exactly what
it would apply for it. The `storage` section prints the required kubelet mounts and Pod Security
Admission guidance, then prints exact curated CSI
inputs when `csi:` is declared. Use `storage-machine`, `storage-values`, `storage-namespaces`,
`storage-crds`, and `storage-objects` as the hand-application streams described in that output.

Bring-your-own CSI is unsupported above the talosbox substrate. The default image covers the curated storage requirements, and `talos.extensions` composes a closed curated set — `gvisor`, `nfs-utils`, `qemu-guest-agent` — on top of it, so an NFS-backed engine gets its client from `extensions: [nfs-utils]`. Names outside that set are rejected before anything is created; an engine needing any other Talos extension still uses the `talos.schematic` override in `talosbox.yaml`. The same curated list is available without a file as `tbx cluster create demo --extensions nfs-utils,gvisor`. A brought schematic combined with a curated list is re-composed through the Image Factory, so bringing your own schematic never silently drops the extensions you asked for — but that combination does require Factory access at create time unless the composed image is already cached.

For a substrate-only BYO install, first save the patch with `tbx manifests demo storage-machine > storage-machine.yaml` and get it onto **every node**. Which route applies depends on where the nodes are: nodes that have no machine config yet (maintenance mode) cannot be patched, so fold the document in at generation time with `talosctl gen config demo https://<cp-ip>:6443 --config-patch @storage-machine.yaml` and then `talosctl apply-config --insecure --nodes <node-ip> --file controlplane.yaml|worker.yaml`; nodes that are already configured take it directly with `talosctl patch mc -p @storage-machine.yaml --nodes <node-ip>`. Before installing the CSI, create its namespace if needed and label it with the PSA commands printed by `tbx manifests demo storage`; then apply the CSI's own manifests. Those labels cover the CSI's namespace only. Every namespace without `pod-security.kubernetes.io/*` labels of its own — `default` included — falls to the cluster-wide default: `baseline` enforcement with `restricted` warn/audit. A test workload applied to `default` is admitted but prints a `Warning: would violate PodSecurity "restricted:latest"` block, which is a warning and not a failure; a workload that genuinely needs `hostNetwork` or privileges exceeds `baseline` and needs its own namespace labelled `privileged`. The patch mounts `/var/local-path-provisioner` — the path tbx's curated local-path CSI uses — while upstream local-path-provisioner's shipped ConfigMap defaults to `/opt/local-path-provisioner`; if you install upstream by hand, run the `local-path-config` edit printed by `tbx manifests demo storage` so it writes into the mounted path. A declared curated CSI supplies exact streams (and Longhorn values through `storage-values`). For Longhorn, apply `storage-namespaces`, then `storage-crds`, wait for those CRDs to become Established, and only then apply post-CRD `storage-objects`; do not use a one-shot apply. Local-path has no CRD barrier: apply `storage-namespaces` before `storage-objects`.

### 4. Lifecycle

```sh
tbx cluster stop demo              # shut down VMs, keep disks
tbx cluster start demo
tbx cluster suspend demo           # save guest memory when the host backend supports it
tbx cluster resume demo
tbx snapshot create demo before-upgrade
tbx node add demo --role worker
tbx cluster destroy demo --force   # permanent
```

Node addresses are not tied to creation order. Each node's MAC is derived from
`sha256("<cluster>/<node>")` on both platforms, but the address behind it comes from a different
allocator per platform. On macOS the address is vmnet's own DHCP lease keyed by that MAC: stable
per node but not sequential, so a fourth node joining `.31/.32/.33` may come up at `.46`. On Linux
the helper serves a static reservation taken as the lowest free host address in the cluster's
`172.30.<n>.0/24` subnet, starting at `172.30.<n>.2`, so addresses normally read sequentially and a
removed node's address returns to the free pool for the next node added. Either way, read a node's
address from `tbx status` rather than inferring it from node order.

Suspend/resume is capability-gated by the host backend:

- macOS 14+ preserves memory while the same `tbxd` process remains alive. After a daemon
  restart, resume warns and safely cold-boots because Virtualization.framework device identity
  cannot be reconstructed.
- Linux requires QEMU 8.2+. A save can survive a daemon restart, but restore requires the same
  QEMU version, architecture, and machine type; an incompatible save warns and safely
  cold-boots. QEMU 6.2–8.1 remains supported for every operation except suspend/resume.
- A resumed guest's clock restarts where it stopped, so it comes back behind the host by the
  length of the suspend. The Talos machine API offers no resync call, so `tbx` cannot correct
  it: `resume` reports the expected gap as a warning, and Talos itself closes it at its next
  NTP poll — minutes away, and not at all on an offline host. Stop and start the cluster
  instead of resuming it when a workload cannot tolerate a clock jump (certificate validity
  windows, leases, time-series writers).

On macOS, `tbx system uninstall` removes the helper and resolver integration. On Linux,
remove or disable the systemd units installed by the selected installation method.

### 5. Prepare and inspect the offline cache

Warm the registry mirror before travel with a list of fully qualified image references. Each
non-comment line must use a non-`latest` tag or a `sha256`/`sha512` digest; use a tag plus
digest when the list must be immutable.

```text
# workshop-images.txt
docker.io/library/pause:3.10
ghcr.io/example/app:v2.4.1@sha256:<64-hex-digest>
```

```sh
tbx cache warm workshop-images.txt more-images.txt # any number of lists; `-` reads stdin once
tbx cache warm --refresh workshop-images.txt        # revalidate complete, unpinned tags
tbx cache warm --check workshop-images.txt         # verify locally, without downloading
tbx cache warm --check --deep workshop-images.txt  # also rehash cached blobs
tbx cache list                                     # disk images and mirror-cache totals
tbx cache list docker.io/library/busybox:1.37      # is this one image cached? (query; exits 0 whether or not it is)
```

`tbx manifests demo images` prints the exact pinned image set a cluster will pull — the curated
path's rendered objects for its declared intent plus the Talos system images for its pinned
version — in this same list format, so `tbx manifests demo images | tbx cache warm -` needs no
hand-maintained list at all.

A single-ref `tbx cache list` applies the same ref validation `cache warm` does: a pinned or
digested ref is answered `cached` or `not cached (<reason>)` and exits 0 either way, while an
unpinnable ref (`:latest`, tagless, no registry host) is rejected with an error and a non-zero
exit rather than an answer.

Blank lines and lines beginning with `#` are ignored. By default, warm resumes incomplete refs
and makes no upstream request for complete refs. `--refresh` revalidates complete unpinned tags;
digest-pinned refs do not need freshness resolution, and a transient refresh failure is nonfatal
when the existing cache is complete.

**Complete** means the tag mapping (when the ref uses a tag), root manifest, selected
`linux/<arch>` manifest, config, and every selected-platform layer are all present locally.
`--check` verifies exactly that graph offline; `--deep` is valid only with `--check` and adds only
a rehash of the cached blobs. Missing pieces are structured, for example
`✗ <ref> index present; linux/<arch> manifest <digest> not cached` or
`✗ <ref> <n> of <total> linux/<arch> layers not cached: <digest>, ...`; a manifest-only 200 is
not complete. Neither check runs a live-cluster pull test. Both modes also verify the
images every node needs before any pod can start but that no list can name — the CRI pod sandbox
(`registry.k8s.io/pause`) image — so neither mode reports the cache complete while that image is
missing, and the gap surfaces while it can still be pulled instead of deadlocking every static
pod offline. `--deep` still catches what a plain `--check` cannot — a blob that is present but
whose bytes no longer match its digest — so `--check --deep` remains the pre-travel gate.

While online, complete cached content remains pullable during transient upstream failures. The
daemon records that decision as
`mirror served stale: <host>/<repository>:<tag> (upstream <reason>; cache complete for linux/<arch>)`.
Anonymous Bearer challenges and `Retry-After` are handled generically; no registry-specific quota
is promised.

`tbx mirror offline` reports whether the pull-through mirror may reach upstream registries.
The mirror and the node resolver are two policy layers: offline stops the mirror from reaching
registries; cache misses return 404. An explicit mirror entry with `skipFallback: false` may fall
through to upstream. talos-box's generated `"*"` entry uses `skipFallback: true`, so it remains a
hard miss. `tbx mirror offline off` restores normal pull-through behavior. Syntactic
loopback registries (`localhost`, loopback IPv4, and loopback IPv6, optionally with a port) are
the deliberate exception: the mirror redirects the request back to that registry inside the
node, including while mirror-offline mode is on. Transport follows containerd's localhost
convention: HTTPS with no port or port 443, HTTP otherwise. A TLS registry on a custom loopback
port is not supported by this passthrough and needs an explicit `machine.registries.mirrors`
entry. The redirect changes the host, so credentials rely on containerd re-authorizing the
redirected request, which its `CheckRedirect` does. Hostnames that only resolve to loopback and
other private registries remain blocked rather than being redirected.

Offline mode persists across a daemon restart and changes how public/upstream pulls fail, so
while it is on `tbx status` heads its listing with a banner and `tbx doctor` reports it as a
`WARN mirror-offline` line. Each miss is also logged as
`mirror offline miss: <ref> (upstream namespace <host>)` in the daemon log, so an
`ImagePullBackOff` is greppable with `tbx logs`.

`tbx cache pull` with no flags reads `talosbox.yaml` the way `tbx up` does: it resolves every
cluster's Talos version, schematic, and extensions with inheritance applied, performs any
schematic re-composition while the Image Factory is still reachable, downloads each distinct
disk-image combination, and warms the container images those clusters will pull (`--no-images`
skips the warming). Everything it fetches is pinned, so a later fully offline `tbx up` of the
same file contacts nothing. `--talos-version`, `--schematic`, and `--extensions` still pull one
ad-hoc combination instead.

The cache is independent of cluster lifecycle: destroying a cluster leaves warmed mirror
content intact. To reclaim space safely, `tbx cache prune` is reference-aware: it removes only
disk-image combinations that no persisted cluster references, that were not pinned by an
explicit pull, and that are not the built-in default, listing each one with its size before
deleting. `tbx cache prune --mirror` removes mirror content only, and `tbx cache prune --all`
removes both and clears pins. Nothing is ever deleted automatically, and `tbx cache list`
labels every combination `in-use` (naming the clusters), `pinned`, `default`, or `orphan`, so
it reads as the prune preview. After upgrading from a build that used the original flat manifest-cache layout, run
`tbx cache warm` again before going offline: verified digest entries remain reusable, but legacy
tag entries are deliberately ignored because their old filenames cannot prove repository identity.

Run `tbx help` for the full command surface.

For the curated Cilium path and optional hand-managed inspection fork, see the
[Cilium walkthrough](docs/walkthrough-cilium-ingress.md).
