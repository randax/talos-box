# talosbox

talosbox (`tbx`) is a workshop-environment tool for Apple Silicon Macs that manages the full lifecycle of hypervisor-based Talos Linux VM clusters with production-style networking. See the [specification](docs/SPEC.md) for the design and planned feature set.

**Status:** pre-v1 scaffold.

## Quick start

### Requirements

- Apple Silicon Mac (M1 or newer) with **16 GB RAM minimum**
- macOS 14 (Sonoma) or newer

### 1. Build

```sh
make build
```

This produces three binaries in `bin/`: the `tbx` CLI, the `tbxd` daemon (started automatically by `tbx` on first use — keep it next to `tbx`), and `tbx-helper` (privileged networking helper).

### 2. One-time host setup

```sh
sudo bin/tbx system install
bin/tbx doctor
```

`system install` registers `tbx-helper` as a root launchd daemon and sets up the `/etc/resolver/k8s.test` DNS resolver. `doctor` verifies the helper, vmnet, DNS wiring, and IP forwarding. Everything else runs unprivileged.

### 3. Create a cluster

```sh
bin/tbx cluster create demo --cp 1 --workers 2
```

This downloads the Talos image on first run (cached under `~/.talosbox/cache/`), then creates and starts the VMs. Nodes boot **unconfigured** into Talos maintenance mode when you choose the substrate-only path; if you request the curated Cilium path (`--cni cilium` or `talosbox.yaml` with `cni: cilium`), tbx applies the machine prerequisites, boots the cluster, installs Cilium, and waits for the curated LoadBalancer VIP to become live. In both cases, talosbox owns the substrate (VMs, networking, DNS, image delivery); curated CNI and LoadBalancer provisioning are opt-in.

Every imperative command prints the equivalent `talosbox.yaml` stanza. Alternatively, work declaratively from the start: write a `talosbox.yaml` and run `tbx up` (idempotent — it reconciles reality to the file; `tbx down` is its inverse).

```yaml
version: 1
clusters:
  - name: demo
    controlPlanes: 1
    workers: 2
```

### 4. Inspect and work with the cluster

```sh
bin/tbx status demo            # nodes, IPs, DNS names, plus copy-pasteable next-step hints
bin/tbx console demo demo-cp-1 # attach to a node's serial console (detach with Ctrl-])
bin/tbx manifests demo         # inspect exact machine patch, pinned chart values/objects, and LB/BGP/MetalLB extras
```

`status` is state-aware: it distinguishes provisioning, Ready-without-LB, and a live VIP, while printing credential exports and the flannel NetworkPolicy limitation. Hints never execute anything; suppress them with `--quiet`. `tbx up --quiet` and `tbx cluster create --quiet` keep their final result but suppress stage narration. `tbx manifests` is the exact inspection/fork surface for the curated path: `machine`, `values`, `objects`, and `extras` match the machine prerequisite patch, pinned Helm values, rendered chart objects, and LB/BGP resources tbx applies. Substrate-only clusters stay manual.

### 5. Lifecycle

```sh
bin/tbx cluster stop demo              # shut down VMs, keep disks
bin/tbx cluster start demo
bin/tbx cluster suspend demo           # save state; same-daemon resume preserves memory
bin/tbx snapshot create demo before-upgrade
bin/tbx node add demo --role worker
bin/tbx cluster destroy demo --force   # permanent
sudo bin/tbx system uninstall          # remove the helper and resolver file
```

Suspend/resume preserves guest memory while the same `tbxd` process remains alive. Restarting
the daemon loses vz's file-handle-backed device identity, so resume reports a warning and safely
cold-boots instead.

Run `bin/tbx help` for the full command surface.

For the curated Cilium path and optional hand-managed inspection fork, see the [Cilium walkthrough](docs/walkthrough-cilium-ingress.md).
