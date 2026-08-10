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
**unconfigured** into Talos maintenance mode — generating and applying machine config with
`talosctl` is deliberately left to you; talosbox only provides the substrate (VMs,
networking, DNS, and image delivery).

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
tbx manifests demo         # Cilium LB/BGP resources and Talos machine-config patches
```

`status` is state-aware: it suggests the next step for the state it observes, such as the
`talosctl --insecure` probe for maintenance-mode nodes. Hints never execute anything; suppress
them with `--quiet`.

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

Run `tbx help` for the full command surface.

For the complete path from `cluster create` to a browsable nginx URL — Talos configuration,
Cilium, ingress, and the `172.30.<n>.200` VIP — see the
[Cilium ingress walkthrough](docs/walkthrough-cilium-ingress.md).
