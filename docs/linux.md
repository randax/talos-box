# Linux host setup

talosbox runs Linux guests directly on QEMU/KVM. The unprivileged `tbxd` daemon owns QEMU,
while a socket-activated `tbx-helper` service owns the small set of host-network operations
that require capabilities.

## Availability

The Linux runtime and systemd assets are in the repository, but the release channels are not
published yet:

| Channel | Intended users | Current state |
|---|---|---|
| Cloudsmith apt/dnf | Ubuntu, Fedora, Debian, openSUSE | Pending [#95](https://github.com/randax/talos-box/issues/95) and [#101](https://github.com/randax/talos-box/issues/101) |
| AUR `tbx-bin` | Arch Linux | Pending [#95](https://github.com/randax/talos-box/issues/95) and [#101](https://github.com/randax/talos-box/issues/101) |
| Nix flake and `virtualisation.talosbox` module | NixOS | Pending [#96](https://github.com/randax/talos-box/issues/96) |

Until those issues close, use the source-preview setup below on a conventional systemd
distribution. Do not substitute guessed Cloudsmith URLs: no public talosbox repository exists
there yet.

## Supported hosts

| Distribution | Support level | Notes |
|---|---|---|
| Ubuntu LTS | Tier one | Ubuntu 22.04's QEMU 6.2 works except for suspend/resume; Ubuntu 24.04+ supplies a new enough QEMU for suspend |
| Fedora | Tier one | Current stable release; install QEMU and edk2 firmware from Fedora packages |
| Arch Linux | Tier one | Rolling release; the AUR package is not published yet |
| NixOS | Tier one design target | Wait for the flake/module in #96; the `/usr`-based source-preview procedure does not apply |
| Debian, openSUSE | Best effort | The runtime is expected to work when the requirements below are met, but these are not release-gating distributions |

Both `amd64` and `arm64` are supported. The host and guest architecture must match: talosbox
does not use emulation as a fallback. Linux requires:

- a writable `/dev/kvm` using KVM API version 12;
- QEMU 6.2 or newer, including the `q35` machine on amd64 or `virt` on arm64;
- matching OVMF/AAVMF EFI firmware;
- systemd, polkit, iproute2 (`ip` and `ss`), and iptables compatibility tooling;
- 16 GB RAM minimum for the default 1-control-plane + 2-worker topology;
- [Go 1.26](https://go.dev/doc/install) and Git when building the source preview.

Suspend/resume is the sole QEMU-version-gated feature. It requires QEMU 8.2 or newer; all
other supported operations work on QEMU 6.2–8.1.

## Install host dependencies

Use the package set for the host architecture:

### Ubuntu

```sh
# amd64
sudo apt update
sudo apt install ca-certificates curl git make openssl policykit-1 \
  qemu-system-x86 ovmf iproute2 iptables

# arm64 (use instead of the amd64 QEMU/firmware packages)
sudo apt install ca-certificates curl git make openssl policykit-1 \
  qemu-system-arm qemu-efi-aarch64 iproute2 iptables
```

### Fedora

```sh
# amd64
sudo dnf install ca-certificates curl git make openssl polkit \
  qemu-system-x86-core edk2-ovmf iproute iptables-nft

# arm64 (use instead of the amd64 QEMU/firmware packages)
sudo dnf install ca-certificates curl git make openssl polkit \
  qemu-system-aarch64-core edk2-aarch64 iproute iptables-nft
```

### Arch Linux

```sh
# amd64
sudo pacman -S base-devel ca-certificates curl git openssl polkit \
  qemu-system-x86 edk2-ovmf iproute2 iptables-nft

# arm64 (use instead of the amd64 QEMU/firmware packages)
sudo pacman -S base-devel ca-certificates curl git openssl polkit \
  qemu-system-aarch64 edk2-aarch64 iproute2 iptables-nft
```

Confirm that hardware acceleration is available before installing talosbox:

```sh
test -r /dev/kvm && test -w /dev/kvm
qemu-system-x86_64 --version  # amd64
# qemu-system-aarch64 --version  # arm64
```

If `/dev/kvm` is missing, enable virtualization in firmware and load the appropriate KVM
kernel module. If it exists but is not writable, add the current user to the group that owns
the device (normally `kvm`), then start a session that carries the new membership: log out and
back in, `loginctl terminate-user $USER` where the user lingers (`loginctl enable-linger`), or
`wsl --shutdown` from Windows under WSL. `tbx doctor` prints whichever step applies to the host.

The source preview requires Go 1.26. Install the repository's current toolchain from the
official Go archive when the distribution package is older:

```sh
GO_VERSION=1.26.6
case "$(uname -m)" in
  x86_64) GO_ARCH=amd64; GO_SHA256=708effb774be8237570d0add163225abbdfaf4fca28b2611df167beba4feef89 ;;
  aarch64|arm64) GO_ARCH=arm64; GO_SHA256=d0507e9e9d7fe012aae570108cbd76c15de879e17130ab8cb90d4d7445cb1f2e ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac
GO_ARCHIVE="go${GO_VERSION}.linux-${GO_ARCH}.tar.gz"
curl -fLO "https://go.dev/dl/${GO_ARCHIVE}"
printf '%s  %s\n' "$GO_SHA256" "$GO_ARCHIVE" | sha256sum --check
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf "$GO_ARCHIVE"
rm "$GO_ARCHIVE"
export PATH="/usr/local/go/bin:$PATH"
go version  # go1.26.6
```

Add `export PATH="/usr/local/go/bin:$PATH"` to the shell profile before continuing.

## Build and install the source preview

Create a talosbox checkout, then build the three binaries:

```sh
git clone https://github.com/randax/talos-box.git
cd talos-box
make binaries

sudo install -Dm0755 bin/tbx /usr/bin/tbx
sudo install -Dm0755 bin/tbxd /usr/bin/tbxd
sudo install -Dm0755 bin/tbx-helper /usr/bin/tbx-helper

sudo install -Dm0644 packaging/linux/usr/lib/systemd/system/tbx-helper.socket \
  /usr/lib/systemd/system/tbx-helper.socket
sudo install -Dm0644 packaging/linux/usr/lib/systemd/system/tbx-helper.service \
  /usr/lib/systemd/system/tbx-helper.service
sudo install -Dm0644 packaging/linux/usr/lib/systemd/user/tbxd.socket \
  /usr/lib/systemd/user/tbxd.socket
sudo install -Dm0644 packaging/linux/usr/lib/systemd/user/tbxd.service \
  /usr/lib/systemd/user/tbxd.service
sudo install -Dm0644 packaging/linux/usr/lib/sysusers.d/talos-box.conf \
  /usr/lib/sysusers.d/talos-box.conf
sudo install -Dm0644 packaging/linux/usr/share/polkit-1/rules.d/90-talos-box-resolved.rules \
  /usr/share/polkit-1/rules.d/90-talos-box-resolved.rules

sudo systemd-sysusers /usr/lib/sysusers.d/talos-box.conf
sudo usermod -aG tbx "$USER"
getent group kvm >/dev/null && sudo usermod -aG kvm "$USER"

sudo systemctl daemon-reload
sudo systemctl enable --now tbx-helper.socket
loginctl enable-linger "$USER"
systemctl --user daemon-reload
systemctl --user enable --now tbxd.socket
```

Log out and back in so the new `tbx` and `kvm` memberships apply, then run:

```sh
tbx doctor
```

A socket-activated `tbxd` writes its narration to `~/.talosbox/tbxd.log` — the file `tbx logs`
reads — as well as to the journal, so both `tbx logs` and `journalctl --user -u tbxd` work.
The packaged `tbxd.socket` unit is the preferred daemon-start path.

If the socket unit is not installed or enabled, `tbx` still starts its sibling `tbxd` on
demand. On a systemd host this fallback always uses a transient `systemd-run --user` service
in `app.slice`, never a process left in the terminal's session scope. If user lingering cannot
be confirmed, `tbx` warns that guests will stop at logout and points to both
`loginctl enable-linger` and the packaged socket unit, but it still starts the transient
service. On a host without systemd, the fallback keeps using the detached `setsid` process used
by earlier releases.

Under WSL2, enable systemd before installing the units and enable lingering as shown above.
Lingering keeps the daemon and its guests alive when the last terminal closes; `wsl --shutdown`,
a WSL restart, and a Windows reboot remain host shutdowns and stop the guests.

Do not use `tbx system install` on Linux. That command currently installs the macOS launchd
helper; Linux installation is owned by packages and systemd units.

Upgrading `tbx`/`tbxd` can also require a new `tbx-helper`: the helper speaks a versioned
protocol, and a mismatch is refused with `helper protocol mismatch`. Recover by upgrading the
`tbx-helper` package (or reinstalling the binary and units as above) and restarting the service:
`sudo systemctl restart tbx-helper.service`. Restart the *service*, not the socket: restarting
`tbx-helper.socket` leaves an already-running helper — and its stale binary — in place.

## Helper state

`tbxd` owns cluster state under `~/.talosbox`; the helper never reads it. The helper runs as the
unprivileged `tbx` system user, which cannot open another user's home, so `tbxd` pushes the
clusters' DHCP reservations (name, subnet, and each node's MAC and IP) to the helper over the
`net.sync` operation — at daemon start and after every change to cluster state.

The helper persists that copy as `/var/lib/tbx/reservations.json` (the unit's
`StateDirectory=tbx`, mode `0700`) and reconverges from it at startup, so bridges, nftables rules,
and DHCP listeners survive a helper restart with no daemon running. Running the helper by hand
without a state directory is supported — set `TBX_HELPER_STATE_DIR` to keep the file elsewhere, or
leave both unset and the reservations live in memory only until the next sync.

A cluster start fails if the helper cannot be synced: without its reservations the nodes would boot
onto a subnet with no DHCP and never get addresses.

## What `tbx doctor` checks on Linux

Run `tbx doctor` once after host setup and again after creating a cluster. Checks that need a
configured bridge or running cluster report `SKIP` before one exists.

| Check | What it proves | Typical remediation |
|---|---|---|
| `runtime-compat` | The client, daemon, and helper agree on their versioned protocols. The runtime block above the findings names each executable path, version, protocol, and PID when available | A mismatch is a `FAIL` and makes doctor exit non-zero. Run the exact absolute-path `system restart --force` command printed by doctor, or use the client matching the running components |
| `installations` | The active daemon is the client's sibling and PATH does not contain multiple distinct `tbx` executables | `WARN` only: choose one installation and remove competing PATH entries |
| `helper`, `helper-unit`, `helper-access` | The helper socket is enabled, reachable, and accessible to the current `tbx` group member | Enable `tbx-helper.socket`; add the user to `tbx`; then apply the group as `doctor` says: log out and back in, `loginctl terminate-user $USER` under a lingering session, or `wsl --shutdown` under WSL |
| `helper-capabilities` | The helper has exactly `CAP_NET_ADMIN`, `CAP_NET_BIND_SERVICE`, and `CAP_NET_RAW` | Reinstall the current service unit and restart the helper |
| `kvm` | `/dev/kvm` exists and is readable+writable | Enable KVM or add the user to the device's group |
| `qemu` | QEMU meets the 6.2 floor, provides the required machine, and reports suspend availability | Install/upgrade the architecture-specific QEMU package |
| `forwarding (host)` | This client read the current host IPv4-forwarding sysctl directly and found it enabled | Repair the host sysctl or restart the helper so desired state is reconverged |
| `bridge-netfilter` | Docker or another firewall has not combined bridge filtering with a default-DROP `FORWARD` policy. Listing the `FORWARD` chain needs root, and a host without `iptables` cannot be inspected at all, so a run that cannot read the chain is a `WARN`, never a `FAIL` | Apply the targeted rule printed by `doctor`; talosbox never edits foreign firewall tables. On the `WARN`, inspect the chain with `sudo iptables -S FORWARD` (or `sudo nft list chain ip filter FORWARD` where `iptables` is not installed) |
| `bridge-stp` | Each `br-tbx<n>` bridge has STP disabled | Run the exact `ip link ... stp_state 0` command printed by `doctor` |
| `rp-filter` | Strict reverse-path filtering will not discard VIP traffic on a multi-homed or VPN host | Set loose mode (`2`) as directed, including per-interface values |
| `port-53`, `port-67`, `port-179` | DNS, DHCP, and optional BGP ports are available on each cluster gateway or already owned by talosbox | Stop the conflicting listener or keep BGP disabled when port 179 is intentionally occupied |
| `resolver`, `DNS`, `system-dns` | Guest DNS is listening and the host actually resolves cluster names — systemd-resolved routes `~<cluster>.k8s.test` to the cluster gateway. `PASS` means the names resolve, and an absent or unreachable systemd-resolved is a `WARN`, never a `PASS`. `resolver` and `DNS` `SKIP` unless a cluster is running (nothing was probed); `system-dns` probes every cluster, so it `SKIP`s only when no cluster exists at all and can `FAIL` while a cluster is merely stopped | Run the per-link `resolvectl` command printed by `doctor` — the same manual step `tbxd` logs. Without systemd-resolved, use the `dig @<gateway> <node>.<domain>` fallback `doctor` prints; guest DNS and by-IP access are unaffected |
| `routes` | Host routes to live nodes and cluster subnets use the talosbox bridge — each address is probed with `ip -o route get` and must leave via `br-tbx<n>` (a cluster gateway may also resolve via `lo`) | Resolve overlapping VPN or host routes and restart the cluster |
| `inter-cluster` | With more than one cluster running, every cluster's ingress VIP answers from the host **and** from each sibling cluster — the sibling leg is dialled by the `lb-probe` behind each VIP, so it travels the same pod-to-sibling-VIP path a workload would. `SKIP`s with the reason when fewer than two running clusters report a live VIP | `FAIL` names the dead direction (`qa-edge → qa-core VIP 172.30.0.200`). Check the announcement mode of the *target* cluster (`tbx bgp status <cluster>`) and the host route to its VIP; `routes` and `forwarding` can both pass while this path is dead |
| `talos-services` | Talos services on configured running nodes are not stalled in `Preparing`/`Starting` for more than three minutes, crashlooping, or unhealthy | `PASS` reports the number of configured nodes inspected. Missing credentials or a failed probe is a `WARN`; for missing credentials, run `talosctl config merge <path-to-talosconfig>`. A stalled, crashlooping, or unhealthy service is a `FAIL`. `SKIP` means there are no configured running nodes or cluster state is unavailable |
| `host-pressure` | Currently reports `SKIP` because Linux host-pressure sampling is not implemented | Size the default cluster for at least 16 GB RAM and monitor the host separately |
| `guest-agent` | Clusters that requested the `qemu-guest-agent` extension have a working host channel | `WARN` only: the config stays valid and portable, the extension is simply inert on this host. `SKIP`s when no cluster requests it |
| `mirror-health` | Pull-through mirror listeners are bound on exactly the running clusters' gateway IPs, and reports the registry-mirror cache totals | Restart the affected cluster (or `tbxd`) so the bind set is reconverged with cluster lifecycle |
| `image-cache` | Reports the Talos disk-image cache totals, named apart from the registry-mirror cache so offline prep can tell the two stores under `~/.talosbox/cache` apart. Incomplete combinations — prunable leftovers with no usable image — are held out of the total and counted separately, and a cache holding nothing else is a `WARN` | Never `FAIL`s: a failed cache listing is reported once on `mirror-health` and skips this line. On `WARN`, rerun `tbx cache pull` before going offline; use `tbx cache list` for the per-combination breakdown |
| `mirror-offline` | Whether `tbx mirror offline` is on. The mode persists across a daemon restart, prevents the mirror from contacting upstream, and makes its misses return 404; node fallback still depends on `skipFallback`, while syntactic loopback registries remain direct | `WARN` only: run `tbx mirror offline off` to restore mirror pull-through; each public/upstream miss is also logged as `mirror offline miss: …` in `tbx logs` |
| `egress` | Image Factory access is usable | Follow the specific warning or failure detail. There is no `security-inventory` line here: the system-extension inventory it reports is macOS-only |

`FAIL` makes `tbx doctor` exit non-zero. `WARN` identifies a degraded but usable configuration,
such as QEMU 6.2 without suspend or a host without automatic systemd-resolved registration.

Host-memory readings are macOS-only for the same reason, so memory ballooning is inactive on
Linux: `tbxd` records one `balloon: manager inactive: …` line in `tbx logs` at startup and never
polls, and the provision-start host-pressure gate stands down silently instead of warning on every
operation. Size clusters against the host by hand and watch `free -h` and `swapon --show`.

## Linux networking and ingress

Each cluster gets a `br-tbx<n>` bridge at `172.30.<n>.1/24` and one tap per node. The bridge lives
exactly as long as the cluster: `tbx cluster destroy` takes it down with the cluster's state — the
destroy summary reports `host bridge br-tbx<n> removed` — and the freed subnet index goes back to
the pool, so the next create reuses it instead of climbing to a new subnet. The helper
serves static DHCP reservations, maintains the talosbox-owned `table inet tbx` for NAT and
forwarding, binds the cluster DNS listener, and registers the route-only DNS domain with
systemd-resolved when available. It does not modify foreign nftables tables or a Docker-owned
`FORWARD` policy.

Each node's MAC is derived deterministically from its identity — `52:54:00` plus the first three
bytes of `sha256("<cluster>/<node>")` — and the helper's DHCP reservation follows that MAC, so a
node keeps its address across stop/start, and across a remove/re-add as long as nothing else claims
it in between. Reservations are taken from the lowest free host address in the cluster subnet,
starting at `172.30.<n>.2`, so on Linux they normally read sequentially, and a removed node's
address goes back to the free pool for the next node added; on macOS the address comes from vmnet's
own DHCP server and is stable per node but not sequential. Either way, read a node's address from
`tbx status` rather than inferring it from node order.

Cilium L2 announcements are the default ingress-VIP path and work directly on the Linux bridge.
BGP is optional on Linux: use it for routed upstreams, ECMP, or
`externalTrafficPolicy: Local` when only nodes with local endpoints should advertise. It is not
required for fast L2 failover on Linux.

While online, a complete cached tag remains pullable during a transient upstream failure; the
daemon records `mirror served stale: <host>/<repository>:<tag> (upstream <reason>; cache complete
for linux/<arch>)`. Offline mode stops the mirror from reaching registries and its misses return
404. Node fallback is a separate policy: an explicit `skipFallback: false` entry may continue to
upstream, while talos-box's generated `"*"` entry is `skipFallback: true` and remains a hard miss.

### Host access to warmed registry images

Host OCI tools cannot supply containerd's `?ns=` query. Put the upstream authority immediately
after the catch-all address instead. For example, after warming
`public.ecr.aws/docker/library/golang:1.25-alpine`, replace `<gateway>` with the cluster gateway:

```sh
crane manifest --insecure \
  <gateway>:5059/public.ecr.aws/docker/library/golang:1.25-alpine

curl -I \
  http://<gateway>:5059/v2/public.ecr.aws/docker/library/golang/manifests/1.25-alpine
```

The path form and containerd's query form share the same cache. Both commands therefore continue
to work for a complete warmed image after `tbx mirror offline on`; an uncached path returns the
same offline 404 as the query form.

`tbx bgp enable|disable <cluster>` flips the announcement mode; it requires `--cni cilium` and
refuses anything else without touching the speaker. `tbx bgp status <cluster>` reports where the
mode actually stands: the recorded mode, whether the host speaker is running, the address it
binds, and the routes it announces.

After `tbx doctor` passes, continue with the common
[Cilium ingress walkthrough](walkthrough-cilium-ingress.md).
