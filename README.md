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

```sh
make build
sudo bin/tbx system install
bin/tbx doctor
```

`make build` produces three binaries in `bin/`: the `tbx` CLI, the `tbxd` daemon
(started automatically by `tbx` on first use — keep it next to `tbx`), and `tbx-helper`
(the privileged networking helper). `system install` registers `tbx-helper` as a root launchd
daemon and installs the `/etc/resolver/k8s.test` resolver. `doctor` verifies the helper, vmnet,
DNS wiring, forwarding, routes, host pressure, and external image access.

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
tbx manifests demo         # exact machine patch, chart values/objects, and LB/BGP extras
```

`status` is state-aware: it distinguishes provisioning, Ready-without-LB, and a live VIP, while
printing credential exports and the flannel NetworkPolicy limitation. Hints never execute
anything; suppress them with `--quiet`. `tbx up --quiet` and `tbx cluster create --quiet` keep
their final result but suppress stage narration. `tbx manifests` is the exact inspection/fork
surface for the curated path: `machine`, `values`, `objects`, and `extras` match the machine
prerequisite patch, pinned Helm values, rendered chart objects, and LB/BGP resources `tbx`
applies. Its `storage` section is also available on substrate-only clusters: it prints the
required kubelet mounts and Pod Security Admission guidance, then prints exact curated CSI
inputs when `csi:` is declared. Use `storage-machine`, `storage-values`, `storage-namespaces`,
`storage-crds`, and `storage-objects` as the hand-application streams described in that output.

Bring-your-own CSI is unsupported above the talosbox substrate. The default image covers the curated storage requirements; an engine requiring other Talos extensions must use the existing `talos.schematic` override in `talosbox.yaml`. talosbox does not compose schematics from extension lists.

For a substrate-only BYO install, first save the patch with `tbx manifests demo storage-machine > storage-machine.yaml` and apply it to **every node** with `talosctl patch mc -p @storage-machine.yaml --nodes <node-ip>`. Before installing the CSI, create its namespace if needed and label it with the PSA commands printed by `tbx manifests demo storage`; then apply the CSI's own manifests. A declared curated CSI supplies exact streams (and Longhorn values through `storage-values`). For Longhorn, apply `storage-namespaces`, then `storage-crds`, wait for those CRDs to become Established, and only then apply post-CRD `storage-objects`; do not use a one-shot apply. Local-path has no CRD barrier: apply `storage-namespaces` before `storage-objects`.

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

Suspend/resume is capability-gated by the host backend:

- macOS 14+ preserves memory while the same `tbxd` process remains alive. After a daemon
  restart, resume warns and safely cold-boots because Virtualization.framework device identity
  cannot be reconstructed.
- Linux requires QEMU 8.2+. A save can survive a daemon restart, but restore requires the same
  QEMU version, architecture, and machine type; an incompatible save warns and safely
  cold-boots. QEMU 6.2–8.1 remains supported for every operation except suspend/resume.

On macOS, `sudo tbx system uninstall` removes the helper and resolver integration. On Linux,
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
tbx cache warm --check workshop-images.txt         # verify locally, without downloading
tbx cache warm --check --deep workshop-images.txt  # also rehash cached blobs
tbx cache list                                     # disk images and mirror-cache totals
```

Blank lines and lines beginning with `#` are ignored. `--check` verifies the cached manifest
graph and host-platform image offline; `--deep` is valid only with `--check` and additionally
rehashes blobs. It does not run a live-cluster pull test.

`tbx mirror offline` reports whether the pull-through mirror may reach upstream registries;
`tbx mirror offline on` serves cached content only (uncached content fails), and `off` restores
normal pull-through behavior. New machine configs use the catch-all mirror at the cluster
gateway with `skipFallback: true`, so nodes do not bypass that mirror directly.

The cache is independent of cluster lifecycle: destroying a cluster leaves warmed mirror
content intact. To reclaim space safely, `tbx cache prune` removes Talos disk images only,
`tbx cache prune --mirror` removes mirror content only, and `tbx cache prune --all` removes
both. After upgrading from a build that used the original flat manifest-cache layout, run
`tbx cache warm` again before going offline: verified digest entries remain reusable, but legacy
tag entries are deliberately ignored because their old filenames cannot prove repository identity.

Run `tbx help` for the full command surface.

For the curated Cilium path and optional hand-managed inspection fork, see the
[Cilium walkthrough](docs/walkthrough-cilium-ingress.md).
