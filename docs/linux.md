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
the device (normally `kvm`), then log out and back in.

The source preview requires Go 1.26. Install the repository's current toolchain from the
official Go archive when the distribution package is older:

```sh
GO_VERSION=1.26.5
case "$(uname -m)" in
  x86_64) GO_ARCH=amd64; GO_SHA256=5c2c3b16caefa1d968a94c1daca04a7ca301a496d9b086e17ad77bb81393f053 ;;
  aarch64|arm64) GO_ARCH=arm64; GO_SHA256=fe4789e92b1f33358680864bbe8704289e7bb5fc207d80623c308935bd696d49 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac
GO_ARCHIVE="go${GO_VERSION}.linux-${GO_ARCH}.tar.gz"
curl -fLO "https://go.dev/dl/${GO_ARCHIVE}"
printf '%s  %s\n' "$GO_SHA256" "$GO_ARCHIVE" | sha256sum --check
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf "$GO_ARCHIVE"
rm "$GO_ARCHIVE"
export PATH="/usr/local/go/bin:$PATH"
go version  # go1.26.5
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
systemctl --user daemon-reload
systemctl --user enable --now tbxd.socket
```

Log out and back in so the new `tbx` and `kvm` memberships apply, then run:

```sh
tbx doctor
```

Do not use `tbx system install` on Linux. That command currently installs the macOS launchd
helper; Linux installation is owned by packages and systemd units.

## What `tbx doctor` checks on Linux

Run `tbx doctor` once after host setup and again after creating a cluster. Checks that need a
configured bridge or running cluster report `SKIP` before one exists.

| Check | What it proves | Typical remediation |
|---|---|---|
| `helper`, `helper-unit`, `helper-access` | The helper socket is enabled, reachable, and accessible to the current `tbx` group member | Enable `tbx-helper.socket`; add the user to `tbx`; log out and back in |
| `helper-capabilities` | The helper has exactly `CAP_NET_ADMIN`, `CAP_NET_BIND_SERVICE`, and `CAP_NET_RAW` | Reinstall the current service unit and restart the helper |
| `kvm` | `/dev/kvm` exists and is readable+writable | Enable KVM or add the user to the device's group |
| `qemu` | QEMU meets the 6.2 floor, provides the required machine, and reports suspend availability | Install/upgrade the architecture-specific QEMU package |
| `forwarding` | IPv4 forwarding is enabled | Repair the host sysctl or restart the helper so desired state is reconverged |
| `bridge-netfilter` | Docker or another firewall has not combined bridge filtering with a default-DROP `FORWARD` policy | Apply the targeted rule printed by `doctor`; talosbox never edits foreign firewall tables |
| `bridge-stp` | Each `br-tbx<n>` bridge has STP disabled | Run the exact `ip link ... stp_state 0` command printed by `doctor` |
| `rp-filter` | Strict reverse-path filtering will not discard VIP traffic on a multi-homed or VPN host | Set loose mode (`2`) as directed, including per-interface values |
| `port-53`, `port-67`, `port-179` | DNS, DHCP, and optional BGP ports are available on each cluster gateway or already owned by talosbox | Stop the conflicting listener or keep BGP disabled when port 179 is intentionally occupied |
| `resolver`, `DNS`, `system-dns` | Guest DNS is listening and systemd-resolved routes `~<cluster>.k8s.test` to the cluster gateway | Run the per-link `resolvectl` command printed by `doctor` when resolved registration is unavailable |
| `routes` | Host routes to live nodes and cluster subnets use the talosbox bridge | Resolve overlapping VPN or host routes and restart the cluster |
| `host-pressure` | Currently reports `SKIP` because Linux host-pressure sampling is not implemented | Size the default cluster for at least 16 GB RAM and monitor the host separately |
| `guest-agent` | Clusters that requested the `qemu-guest-agent` extension have a working host channel | `WARN` only: the config stays valid and portable, the extension is simply inert on this host. `SKIP`s when no cluster requests it |
| `mirror-health` | Pull-through mirror listeners are bound on exactly the running clusters' gateway IPs, and reports the cache totals | Restart the affected cluster (or `tbxd`) so the bind set is reconverged with cluster lifecycle |
| `egress`, `security-inventory` | Image Factory access is usable and relevant security/VPN software is visible | Follow the specific warning or failure detail |

`FAIL` makes `tbx doctor` exit non-zero. `WARN` identifies a degraded but usable configuration,
such as QEMU 6.2 without suspend or a host without automatic systemd-resolved registration.

## Linux networking and ingress

Each cluster gets a `br-tbx<n>` bridge at `172.30.<n>.1/24` and one tap per node. The helper
serves static DHCP reservations, maintains the talosbox-owned `table inet tbx` for NAT and
forwarding, binds the cluster DNS listener, and registers the route-only DNS domain with
systemd-resolved when available. It does not modify foreign nftables tables or a Docker-owned
`FORWARD` policy.

Cilium L2 announcements are the default ingress-VIP path and work directly on the Linux bridge.
BGP is optional on Linux: use it for routed upstreams, ECMP, or
`externalTrafficPolicy: Local` when only nodes with local endpoints should advertise. It is not
required for fast L2 failover on Linux.

After `tbx doctor` passes, continue with the common
[Cilium ingress walkthrough](walkthrough-cilium-ingress.md).
