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

This downloads the Talos image on first run (cached under `~/.talosbox/cache/`), then creates and starts the VMs. Nodes boot **unconfigured** into Talos maintenance mode — generating and applying machine config with `talosctl` is deliberately left to you; talosbox only provides the substrate (VMs, networking, DNS, image delivery).

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
bin/tbx manifests demo         # print Cilium LB pool / BGP manifests and machine-config patches
```

`status` is state-aware: it suggests the next step for the state it observes (e.g. the `talosctl --insecure` probe for maintenance-mode nodes). Hints never execute anything; suppress them with `--quiet`.

### 5. Lifecycle

```sh
bin/tbx cluster stop demo              # shut down VMs, keep disks
bin/tbx cluster start demo
bin/tbx cluster suspend demo           # save state; resume with `cluster resume`
bin/tbx snapshot create demo before-upgrade
bin/tbx node add demo --role worker
bin/tbx cluster destroy demo --force   # permanent
sudo bin/tbx system uninstall          # remove the helper and resolver file
```

Run `bin/tbx help` for the full command surface.
