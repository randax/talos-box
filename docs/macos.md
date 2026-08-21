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
- administrator rights for `sudo tbx system install`, once per helper protocol version. Upgrading
  `tbx` does not replace the installed helper: when a release changes the helper protocol, every
  helper call fails with a protocol mismatch naming the `sudo … system install` to rerun.

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
| `port-179` | No foreign process holds every address on the BGP port, ahead of the per-cluster gateway bind the host speaker needs. The inventory comes from `netstat`, because an unprivileged `lsof -iTCP:179` cannot see a root-owned socket on macOS. `SKIP`s until a cluster exists | `WARN` only: quote the listener the check prints, identify its owner with `sudo lsof -nP -iTCP:179 -sTCP:LISTEN`, and stop it — a listener bound to a cluster gateway is talosbox's own speaker and is never reported |
| `host-pressure` | Host memory pressure, swap exhaustion, and free space on the volume holding `~/.talosbox` are outside the range that resets guests and corrupts Talos `EPHEMERAL` data | `FAIL`s where `tbx cluster create` would refuse and `WARN`s where it would only warn, printing the same runnable remediation — free host memory (`tbx down`) or host storage (`tbx cache prune --all`) — and noting that `--force` overrides the create gate at the risk of guest-disk corruption. A `PASS` prints the three numbers it was decided on — free memory, swap in use, free space on the `~/.talosbox` volume — and how much room is left for new guests once the balloon reserve is kept free, which is what the next `cluster create` or `cluster start` is gated on |
| `system-dns` | macOS itself resolves `<node>.<domain>` to the cluster's addresses through the scoped resolver files, and each custom-domain cluster's `/etc/resolver/<domain>` file is present and talosbox-managed | Remove the unmanaged or non-regular resolver file `doctor` names — talosbox never touches those; otherwise suspect a DNS filtering agent or browser/system DoH bypassing the scoped resolver |
| `routes` | Host routes to each running cluster's gateway and live nodes exit via a `bridge`/`vmnet` interface or `lo0`, not a tunnel | Disconnect or split-exclude the VPN/ZTNA client that captured `172.30.0.0/16`, then restart the cluster |
| `inter-cluster` | With more than one cluster running, every cluster's ingress VIP answers from the host **and** from each sibling cluster — the sibling leg is dialled by the `lb-probe` behind each VIP, so it travels the same pod-to-sibling-VIP path a workload would. `SKIP`s with the reason when fewer than two running clusters report a live VIP | `FAIL` names the dead direction (`qa-edge → qa-core VIP 172.30.0.200`). Check the announcement mode of the *target* cluster (`tbx bgp status <cluster>`) and the host route to its VIP; `routes` and `forwarding` can both pass while this path is dead |
| `guest-agent` | Clusters that requested the `qemu-guest-agent` extension have a working host channel | `WARN` only: the config stays valid and portable, the extension is simply inert on this host. `SKIP`s when no cluster requests it |
| `mirror-health` | Pull-through mirror listeners are bound on exactly the running clusters' gateway IPs, and reports the registry-mirror cache totals | Restart the affected cluster (or `tbxd`) so the bind set is reconverged with cluster lifecycle |
| `image-cache` | Reports the Talos disk-image cache totals, named apart from the registry-mirror cache so offline prep can tell the two stores under `~/.talosbox/cache` apart. Incomplete combinations — prunable leftovers with no usable image — are held out of the total and counted separately, and a cache holding nothing else is a `WARN` | Never `FAIL`s: a failed cache listing is reported once on `mirror-health` and skips this line. On `WARN`, rerun `tbx cache pull` before going offline; use `tbx cache list` for the per-combination breakdown |
| `mirror-offline` | Whether `tbx mirror offline` is on. The mode persists across a daemon restart and makes every uncached pull fail, so it is named rather than left to be remembered | `WARN` only: run `tbx mirror offline off` to restore upstream pulls; each miss is also logged as `mirror offline miss: …` in `tbx logs` |
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

### Node addressing

Every node's MAC address is derived deterministically from its identity —
`52:54:00` plus the first three bytes of `sha256("<cluster>/<node>")` — so a node keeps the same
MAC across stop/start, destroy/recreate, and remove/re-add. On macOS the address itself comes
from vmnet's own DHCP server, keyed by that MAC, not from a counter tbx controls: addresses are
**stable per node identity but not sequential**. A fourth node added to a cluster whose nodes sit
at `.31/.32/.33` may well come up at `.46`, and the same node gets `.46` again if it is removed
and re-added. That is intended behaviour, not a leak or a collision — stability, not adjacency,
is the contract. `tbx status` reports the address vmnet actually leased, read back from
`/var/db/dhcpd_leases` by MAC. (On Linux the helper serves static reservations from the cluster model instead,
starting at `172.30.<n>.2` and taking the lowest free host address, so addresses there are
sequential.) Always read a node's current address from `tbx status`; never infer it by counting.

### Checking tbx DNS

Verify tbx names with `dscacheutil -q host -a name <node>.<cluster>.<domain>` or `ping`. Do
**not** use `dig` or `nslookup`: they query the resolvers in `/etc/resolv.conf` directly and
bypass `/etc/resolver/`, which is exactly where tbx installs its per-domain entries — so they
return nothing for a perfectly healthy cluster. `scutil --dns` shows the per-domain resolver
macOS actually consults.

Cilium L2 announcements are the default ingress-VIP path and work here — ARP for addresses
vmnet never assigned passes unfiltered — but macOS ignores gratuitous ARP through vmnet and
converges only on its own ARP revalidation, so failover takes roughly 40–50 s. BGP mode is the
fast-failover path on macOS for that reason.

`tbx bgp enable|disable <cluster>` flips the announcement mode; it requires `--cni cilium` and
refuses anything else without touching the speaker. `tbx bgp status <cluster>` reports where the
mode actually stands: the recorded mode, whether the host speaker is running, the address it
binds, and the routes it announces.

The pull-through registry mirrors bind on each cluster gateway (`172.30.<n>.1`), never
`0.0.0.0`, with the catch-all on port `5059`. Port 5000 is deliberately unused: macOS AirPlay
Receiver answers there and would poison registry pulls.

After `tbx doctor` passes, continue with the common
[Cilium ingress walkthrough](walkthrough-cilium-ingress.md).
