# macOS host setup

talosbox runs Talos guests on macOS through Virtualization.framework or QEMU/HVF. VZ is the
zero-install default on Apple Silicon; QEMU/HVF is optional and best-effort there and is the only
backend on Intel Macs. The unprivileged `tbxd` daemon owns the VMs, while a root launchd
`tbx-helper` daemon owns the small set of host operations that require privilege: vmnet
attachments, `/etc/resolver` files, IP forwarding, and route injection.

## Availability

| Channel | Intended users | Current state |
|---|---|---|
| Homebrew `randax/tap/talosbox` | Everyone on Apple Silicon | Planned; the Developer-ID-signed, notarized build is not published yet |
| Source build (`make build`) | Apple Silicon and best-effort Intel users | The currently validated path — use the setup below |

`make build` ad-hoc signs `bin/tbx` and `bin/tbxd` with the
`com.apple.security.virtualization` entitlement. Unsigned binaries cannot start VMs, so always
build through `make build` rather than a bare `go build`.

## Supported hosts

| Host | Support level | Notes |
|---|---|---|
| Apple Silicon (M1 or newer), macOS 14+ | Tier one | The validated development and workshop target |
| Apple Silicon, macOS 13 | Unsupported | Suspend/resume and the current Virtualization.framework surface need macOS 14 |
| Intel Macs, macOS 15+ | Best effort | QEMU/HVF-only and community-verified; outside the parity bar |

macOS requires:

- an Apple Silicon Mac running macOS 14 or newer for the default VZ path, or macOS 15+ for the
  optional QEMU/HVF path; Intel Macs require macOS 15+, `kern.hv_support=1`,
  `qemu-system-x86_64`, and matching x86_64 edk2 firmware;
- 16 GB RAM minimum for the default 1-control-plane + 2-worker topology;
- 25 GB free on the volume holding `~/.talosbox` (node disks are 20 GB sparse);
- the Xcode command line tools and [Go 1.26](https://go.dev/doc/install) for the source build;
- administrator rights for `tbx system install`, once per helper protocol version. The command
  automatically re-executes its absolute path through sudo. Upgrading
  `tbx` does not replace the installed helper: when a release changes the helper protocol, every
  helper call fails with a protocol mismatch naming the `system install` to rerun.

Suspend/resume is capability-gated rather than version-gated here: memory is preserved while
the same `tbxd` process stays alive. VZ still warns and safely cold-boots after a daemon
restart because Virtualization.framework device identity cannot be reconstructed, while QEMU
retains its saved state across that restart.

## Hypervisor selection

VZ stays the zero-install default on Apple Silicon. `TBX_HYPERVISOR` selects QEMU only when you
want the optional path; otherwise the compiled default stays in effect. The resolved choice is recorded on the
cluster, so a cluster created as VZ stays VZ even if the daemon default changes later.

QEMU/HVF is best effort on macOS. Homebrew QEMU needs macOS 15+ to expose HVF; install it with
`brew install qemu`, or use the QEMU package from nixpkgs. The installed binary must retain the
`com.apple.security.hypervisor` entitlement. QEMU still runs unprivileged on the helper-owned
datagram FD that vmnet hands to the VM. Intel Macs run amd64 guests through
`qemu-system-x86_64` with HVF and the `edk2-x86_64-code.fd` / `edk2-i386-vars.fd` firmware pair.
There is no supported TCG fallback or cross-architecture guest promise.

```yaml
clusters:
  - name: demo
    hypervisor: qemu
```

```sh
TBX_HYPERVISOR=qemu tbx system restart --force
```

For a self-supervised launchd job, put `TBX_HYPERVISOR` in the job plist's
`EnvironmentVariables` dictionary, then kickstart that job with `launchctl kickstart -k <domain>/<label>`.

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

bin/tbx system install
bin/tbx doctor
```

Homebrew and mise/manual installs do not mix; choose one `tbx`/`tbxd`/`tbx-helper` triad. If an
explicit privileged form is needed, use `sudo "$(command -v tbx)" system install`, never a
PATH-dependent `sudo tbx ...`.

`system install` registers `tbx-helper` as the root launchd daemon `dev.talosbox.helper` and
installs the shared `/etc/resolver/k8s.test` file. Everything else runs unprivileged. The
launchd plist pins the helper at an absolute path, so a moved or renamed checkout leaves the
old helper running: rerun `<checkout>/bin/tbx system install` after relocating the
checkout. `tbx system uninstall` removes the helper and the resolver integration.

Do not use the Linux instructions on macOS, and do not run `tbx system install` on Linux —
that command is the macOS launchd installer.

## What `tbx doctor` checks on macOS

Run `tbx doctor` once after host setup and again after creating a cluster. Checks that need a
running daemon or an existing cluster report `SKIP` before one exists. The checks are printed
in this order:

| Check | What it proves | Typical remediation |
|---|---|---|
| `runtime-compat` | The client, daemon, and helper agree on their versioned protocols. The runtime block above the findings names each executable path, version, protocol, and PID when available | A mismatch is a `FAIL` and makes doctor exit non-zero. Run the exact absolute-path `system restart --force` command printed by doctor, or use the client matching the running components |
| `installations` | The active daemon is the client's sibling and PATH does not contain multiple distinct `tbx` executables | `WARN` only: choose one Homebrew or mise/manual triad and remove the competing PATH entries |
| `helper` | The root launchd helper is reachable on its socket and answers a ping | Run `<checkout>/bin/tbx system install`, then rerun `doctor` |
| `resolver` | The shared `/etc/resolver/k8s.test` file exists and is a regular file | Reinstall with `tbx system install`; never replace the file with a symlink or directory |
| `DNS` | The resolver embedded in `tbxd` answers on `127.0.0.1:5399` | Run any `tbx` command to start the on-demand daemon; the check `SKIP`s instead of failing when only the daemon is down |
| `forwarding (host)` | This client read `net.inet.ip.forwarding` as `1` directly from the host, so host-to-guest and inter-cluster traffic is routed | Restart the helper so it reconverges the sysctl |
| `port-179` | No foreign process holds every address on the BGP port, ahead of the per-cluster gateway bind the host speaker needs. The inventory comes from `netstat`, because an unprivileged `lsof -iTCP:179` cannot see a root-owned socket on macOS. `SKIP`s until a cluster exists | `WARN` only: quote the listener the check prints, identify its owner with `sudo lsof -nP -iTCP:179 -sTCP:LISTEN`, and stop it — a listener bound to a cluster gateway is talosbox's own speaker and is never reported |
| `host-pressure` | Host memory pressure, compressor occupancy, swap use, and free space on the volume holding `~/.talosbox` are outside the range that resets guests and corrupts Talos `EPHEMERAL` data | Swap at least 80% used is always a `WARN`, including a small 3 GiB swap file. Critical pressure blocks. Warning pressure warns, escalating to a block when swap is exhausted and measured free RAM does not cover the reserve plus incoming guests; the existing 90%, low-free-swap, and storage rules can still escalate independently. The line prints free/total memory, compressor, swap percentage, and the same runnable remediation as the start gate. Doctor measures with no incoming guests, so its PASS/WARN detail states the largest pending allocation that can change the start verdict. The start gate applies only while guests are running and credits reclaimable balloon memory before refusing |
| `balloon` | INFO only: the daemon was started with `TBX_DISABLE_BALLOON` and launches guests without a memory balloon device. Set it if the Mac itself kernel-panics (`Kernel tag check fault` in `tbxd`) during cluster destroy or `tbx system restart` (#513) | Never a WARN or FAIL; the line is absent when ballooning is on |
| `system-dns` | macOS itself resolves `<node>.<domain>` to the cluster's addresses through the scoped resolver files, and each custom-domain cluster's `/etc/resolver/<domain>` file is present and talosbox-managed | Remove the unmanaged or non-regular resolver file `doctor` names — talosbox never touches those; otherwise suspect a DNS filtering agent or browser/system DoH bypassing the scoped resolver |
| `routes` | Host routes to each running cluster's gateway and live nodes exit via a `bridge`/`vmnet` interface or `lo0`, not a tunnel | Disconnect or split-exclude the VPN/ZTNA client that captured `172.30.0.0/16`, then restart the cluster |
| `inter-cluster` | With more than one cluster running, every cluster's ingress VIP answers from the host **and** from each sibling cluster — the sibling leg is dialled by the `lb-probe` behind each VIP, so it travels the same pod-to-sibling-VIP path a workload would. `SKIP`s with the reason when fewer than two running clusters report a live VIP | `FAIL` names the dead direction (`qa-edge → qa-core VIP 172.30.0.200`). Check the announcement mode of the *target* cluster (`tbx bgp status <cluster>`) and the host route to its VIP; `routes` and `forwarding` can both pass while this path is dead |
| `talos-services` | Talos services on configured running nodes are not stalled in `Preparing`/`Starting` for more than three minutes, crashlooping, unhealthy, or recovering from an unexpected guest reboot | `PASS` reports the number of configured nodes inspected. A `rebootedAt` observation is a `WARN` and names `tbx console`; it does not make doctor exit nonzero by itself. Missing credentials or a failed probe is also a `WARN`; for missing credentials, run `talosctl config merge <path-to-talosconfig>`. A stalled, crashlooping, or unhealthy service is a `FAIL`. `SKIP` means there are no configured running nodes or cluster state is unavailable. Reboot baselines are process-local, so restarting `tbxd` loses history before its first new sample |
| `guest-agent` | Clusters that requested the `qemu-guest-agent` extension have a working host channel | `WARN` only: the config stays valid and portable, the extension is simply inert on this host. `SKIP`s when no cluster requests it |
| `mirror-health` | Pull-through mirror listeners are bound on exactly the running clusters' gateway IPs, and reports the registry-mirror cache totals | Restart the affected cluster (or `tbxd`) so the bind set is reconverged with cluster lifecycle |
| `image-cache` | Reports the Talos disk-image cache totals, named apart from the registry-mirror cache so offline prep can tell the two stores under `~/.talosbox/cache` apart. Incomplete combinations — prunable leftovers with no usable image — are held out of the total and counted separately, and a cache holding nothing else is a `WARN` | Never `FAIL`s: a failed cache listing is reported once on `mirror-health` and skips this line. On `WARN`, rerun `tbx cache pull` before going offline; use `tbx cache list` for the per-combination breakdown |
| `mirror-offline` | Whether `tbx mirror offline` is on. The mode persists across a daemon restart, prevents the mirror from contacting upstream, and makes its misses return 404; node fallback still depends on `skipFallback`, while syntactic loopback registries remain direct | `WARN` only: run `tbx mirror offline off` to restore mirror pull-through; each public/upstream miss is also logged as `mirror offline miss: …` in `tbx logs` |
| `egress` | A fresh TLS handshake to `factory.talos.dev` completes | Install the trusted corporate CA in the System keychain, or set `HTTPS_PROXY` in the shell that starts `tbx`, per the printed detail |
| `security-inventory` | Lists activated system extensions — VPN, EDR, and content-filter software known to interfere with guest TLS, DNS, or routes | Informational only; use it to explain mirror, DNS, or route findings above |

`FAIL` makes `tbx doctor` exit non-zero. `WARN` identifies a degraded but usable configuration,
such as an inert guest-agent channel or filtered Image Factory egress. `INFO` lines
(`security-inventory`) never affect the exit code.

`tbx doctor` also prints an INFO `Hypervisors` section before the rest of the checks. Each line
names one hypervisor, its availability, the current default source (`default=yes (source=compiled)`
or `default=yes (source=TBX_HYPERVISOR)`), and the feature gates used by `tbx status`. The
macOS QEMU hypervisor currently emits these availability/remediation pairs. On a usable Intel
Mac, the QEMU line begins exactly:

```text
qemu: availability=available (best-effort platform)
```

An unavailable Intel probe keeps its actual reason and remediation instead of the best-effort
tag. In particular, verify that `sysctl kern.hv_support` prints `1`,
`qemu-system-x86_64 --version` succeeds, and the x86_64 code/vars firmware pair is installed.

- `qemu-system-<arch> was not found on PATH` -> `install QEMU: brew install qemu, then restart tbxd`
- `QEMU does not list the hvf accelerator` -> `HVF not built in: Homebrew builds without HVF on macOS 14; upgrade to macOS 15+ and reinstall QEMU`
- `HVF denied: kern.hv_support is not 1` -> `use a Mac with Hypervisor.framework support enabled`
- `HVF denied: <binary> lacks com.apple.security.hypervisor` -> `install or reinstall a signed Homebrew/nixpkgs QEMU; do not re-sign it without the hypervisor entitlement`
- `probe QEMU: <error>` -> `upgrade QEMU to 6.2 or newer: brew upgrade qemu, then restart tbxd`
- `hypervisor feature unsupported: QEMU >= 6.2 is required (found <version>)` -> `upgrade QEMU to 6.2 or newer: brew upgrade qemu, then restart tbxd`
- `hypervisor feature unsupported: QEMU <version> does not provide required machine type "<machine>"` -> `upgrade QEMU to 6.2 or newer: brew upgrade qemu, then restart tbxd`
- `resolve QEMU binary path: <error>` -> `upgrade QEMU to 6.2 or newer: brew upgrade qemu, then restart tbxd`
- `no matching EFI firmware pair found for <arch>` -> `reinstall QEMU so its edk2 firmware is present`

### Report an Intel run

Intel support is community-verified and does not set the Apple Silicon parity bar. Capture the
host and tool identity before running the applicable QA charters:

```sh
uname -m
sw_vers
qemu-system-x86_64 --version
tbx version
sysctl kern.hv_support
tbx doctor | grep 'qemu: availability='
```

Follow the report convention in the [QA coverage matrix](qa/MATRIX.md): open one GitHub issue
labelled `qa-run`, title it `QA Intel macOS <date>`, and include the command output above plus each
QA charter's PASS, FAIL, PASS-with-friction, or BLOCKED result and supporting evidence.

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

Verify tbx names with `dscacheutil -q host -a name <node>.<cluster-domain>` or `ping`, where
`<cluster-domain>` is the domain the cluster is actually reachable under: `<cluster>.k8s.test` on
the default domain (so `<node>.<cluster>.k8s.test`), or exactly the `--domain` value for a cluster
created with one (so `<node>.<domain>` — `qa-fork-cp-1.forge.internal`, not
`qa-fork-cp-1.qa-fork.forge.internal`). A wrong three-part name is still a suffix of the owning
domain, so it resolves to the ingress wildcard address instead of failing, and looks like a
successful check. Do
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

After `tbx doctor` passes, continue with the common
[Cilium ingress walkthrough](walkthrough-cilium-ingress.md).
