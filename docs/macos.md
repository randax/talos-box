# macOS host setup

talosbox runs Talos guests on Apple Silicon through Virtualization.framework. The unprivileged
`tbxd` daemon owns the VMs, while a root launchd `tbx-helper` daemon owns the small set of host
operations that require privilege: vmnet attachments, `/etc/resolver` files, IP forwarding, and
route injection.

## Availability

| Channel | Intended users | Current state |
|---|---|---|
| Homebrew `randax/tap/talosbox` | Everyone on Apple Silicon | Planned; the Developer-ID-signed, notarized build is not published yet |
| Source build (`make build`) | Everyone today | The currently validated path — use the setup below |

`make build` ad-hoc signs `bin/tbx` and `bin/tbxd` with the
`com.apple.security.virtualization` entitlement. Unsigned binaries cannot start VMs, so always
build through `make build` rather than a bare `go build`.

## Supported hosts

| Host | Support level | Notes |
|---|---|---|
| Apple Silicon (M1 or newer), macOS 14+ | Tier one | The validated development and workshop target |
| Apple Silicon, macOS 13 | Unsupported | Suspend/resume and the current Virtualization.framework surface need macOS 14 |
| Intel Macs | Unsupported | talosbox does not emulate; the guest architecture must match the host |

macOS requires:

- an Apple Silicon Mac running macOS 14 or newer;
- 16 GB RAM minimum for the default 1-control-plane + 2-worker topology;
- 25 GB free on the volume holding `~/.talosbox` (node disks are 20 GB sparse);
- the Xcode command line tools and [Go 1.26](https://go.dev/doc/install) for the source build;
- one-time administrator rights for `sudo tbx system install`.

Suspend/resume is capability-gated rather than version-gated here: memory is preserved while
the same `tbxd` process stays alive, and a resume after a daemon restart warns and safely
cold-boots because Virtualization.framework device identity cannot be reconstructed.

## Install host dependencies

```sh
xcode-select --install
brew install go   # or install Go 1.26 from https://go.dev/dl/
go version        # go1.26 or newer
```

## Build and install the source preview

```sh
git clone https://github.com/randax/talos-box.git
cd talos-box
make build

sudo bin/tbx system install
bin/tbx doctor
```

`system install` registers `tbx-helper` as the root launchd daemon `dev.talosbox.helper` and
installs the shared `/etc/resolver/k8s.test` file. Everything else runs unprivileged. The
launchd plist pins the helper at an absolute path, so a moved or renamed checkout leaves the
old helper running: rerun `sudo <checkout>/bin/tbx system install` after relocating the
checkout. `sudo tbx system uninstall` removes the helper and the resolver integration.

Do not use the Linux instructions on macOS, and do not run `tbx system install` on Linux —
that command is the macOS launchd installer.

## What `tbx doctor` checks on macOS

Run `tbx doctor` once after host setup and again after creating a cluster. Checks that need a
running daemon or an existing cluster report `SKIP` before one exists. The checks are printed
in this order:

| Check | What it proves | Typical remediation |
|---|---|---|
| `helper` | The root launchd helper is reachable on its socket and answers a ping | Run `sudo <checkout>/bin/tbx system install`, then rerun `doctor` |
| `resolver` | The shared `/etc/resolver/k8s.test` file exists and is a regular file | Reinstall with `sudo tbx system install`; never replace the file with a symlink or directory |
| `DNS` | The resolver embedded in `tbxd` answers on `127.0.0.1:5399` | Run any `tbx` command to start the on-demand daemon; the check `SKIP`s instead of failing when only the daemon is down |
| `forwarding` | `net.inet.ip.forwarding` is `1`, so host-to-guest and inter-cluster traffic is routed | Restart the helper so it reconverges the sysctl |
| `host-pressure` | Host memory pressure, swap exhaustion, and free space on the volume holding `~/.talosbox` are outside the range that resets guests and corrupts Talos `EPHEMERAL` data | `FAIL`s where `tbx cluster create` would refuse and `WARN`s where it would only warn, printing the same runnable remediation — free host memory (`tbx down`) or host storage (`tbx cache prune --all`) — and noting that `--force` overrides the create gate at the risk of guest-disk corruption |
| `system-dns` | macOS itself resolves `<node>.<domain>` to the cluster's addresses through the scoped resolver files, and each custom-domain cluster's `/etc/resolver/<domain>` file is present and talosbox-managed | Remove the unmanaged or non-regular resolver file `doctor` names — talosbox never touches those; otherwise suspect a DNS filtering agent or browser/system DoH bypassing the scoped resolver |
| `routes` | Host routes to each running cluster's gateway and live nodes exit via a `bridge`/`vmnet` interface or `lo0`, not a tunnel | Disconnect or split-exclude the VPN/ZTNA client that captured `172.30.0.0/16`, then restart the cluster |
| `guest-agent` | Clusters that requested the `qemu-guest-agent` extension have a working host channel | `WARN` only: the config stays valid and portable, the extension is simply inert on this host. `SKIP`s when no cluster requests it |
| `mirror-health` | Pull-through mirror listeners are bound on exactly the running clusters' gateway IPs, and reports the registry-mirror cache totals | Restart the affected cluster (or `tbxd`) so the bind set is reconverged with cluster lifecycle |
| `image-cache` | Reports the Talos disk-image cache totals, named apart from the registry-mirror cache so offline prep can tell the two stores under `~/.talosbox/cache` apart | Informational only; use `tbx cache list` for the per-combination breakdown |
| `egress` | A fresh TLS handshake to `factory.talos.dev` completes | Install the trusted corporate CA in the System keychain, or set `HTTPS_PROXY` in the shell that starts `tbx`, per the printed detail |
| `security-inventory` | Lists activated system extensions — VPN, EDR, and content-filter software known to interfere with guest TLS, DNS, or routes | Informational only; use it to explain mirror, DNS, or route findings above |

`FAIL` makes `tbx doctor` exit non-zero. `WARN` identifies a degraded but usable configuration,
such as an inert guest-agent channel or filtered Image Factory egress. `INFO` lines
(`security-inventory`) never affect the exit code.

## macOS networking and ingress

Each cluster gets one pinned vmnet shared-mode network at `172.30.<n>.0/24`, created by the
helper and handed to each VM as a datagram FD. vmnet provides DHCP and NAT egress. Because
pinned shared-mode interfaces only intercommunicate within one subnet, `tbx-helper` routes
`172.30.0.0/16` frames between its own attachments before they enter vmnet, learning node and
VIP ownership from DHCP and ARP.

Cilium L2 announcements are the default ingress-VIP path and work here — ARP for addresses
vmnet never assigned passes unfiltered — but macOS ignores gratuitous ARP through vmnet and
converges only on its own ARP revalidation, so failover takes roughly 40–50 s. BGP mode is the
fast-failover path on macOS for that reason.

The pull-through registry mirrors bind on each cluster gateway (`172.30.<n>.1`), never
`0.0.0.0`, with the catch-all on port `5059`. Port 5000 is deliberately unused: macOS AirPlay
Receiver answers there and would poison registry pulls.

After `tbx doctor` passes, continue with the common
[Cilium ingress walkthrough](walkthrough-cilium-ingress.md).
