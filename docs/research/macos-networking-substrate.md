# macOS Networking Substrate for Apple‑Silicon Talos VMs (Cilium‑realistic L2)

Research date: 2026‑07‑20. Author host: macOS 26.5.2 (Tahoe), Apple Silicon.

Primary sources are cited inline. The single most authoritative source used here is the
`vmnet.framework` public header shipped in the macOS SDK
(`.../MacOSX.sdk/System/Library/Frameworks/vmnet.framework/Versions/A/Headers/vmnet.h`,
Copyright 2013–2025 Apple Inc.), read directly rather than from the JS‑rendered
developer.apple.com pages (which do not render usable text via fetch). Version/availability
gates quoted below (`API_AVAILABLE(macos(x))`) come straight from that header.

---

## TL;DR / Recommendation

- **Default substrate: vmnet **shared mode** (`VMNET_SHARED_MODE`) with an explicitly pinned
  subnet** via `vmnet_start_address_key` / `vmnet_end_address_key` / `vmnet_subnet_mask_key`.
  All shared‑mode interfaces on the same subnet form **one L2 broadcast domain** that also
  includes the macOS host, so VMs get host‑routable RFC‑1918 IPs and can ARP each other and
  the host. This needs **no special Apple entitlement** — only the ability to create a vmnet
  interface (root, or a root helper). This is the realistic‑enough substrate for
  production‑style Cilium on a laptop.
- **LB‑IPAM works without extra plumbing if you size the pool correctly.** Per Apple's header,
  addresses **above `vmnet_end_address_key` up to the end of the subnet are "available for
  static assignment."** Put the Cilium LB‑IPAM pool in that static range and it lives on the
  same L2 segment the host is already attached to (`bridge100`), so it is directly host‑routable
  once a node answers ARP for it.
- **Cilium L2 Announcements is the mechanism that makes LB IPs reachable**: a node ARP‑replies
  for the LB VIP with the node's own MAC. This *should* traverse the vmnet shared‑mode switch
  because it forwards broadcast/ARP among shared‑mode peers and to the host. **The one real
  uncertainty (flagged, not resolved by any source I found): whether vmnet's host/shared‑mode
  anti‑spoofing filter — which drops packets "sent from a different IPv4 address" — also
  inspects the *ARP sender‑protocol‑address*.** If it does, gratuitous/reply ARP claiming the
  LB VIP could be dropped. **Prototype this early**; it is the biggest architectural risk.
- **Multiple named clusters → multiple isolated L2 segments.** Two viable paths:
  (a) run several independent shared‑mode vmnet networks, each pinned to a distinct subnet; or
  (b) on **macOS 11+** use `VMNET_HOST_MODE` + `vmnet_network_identifier_key` (a per‑network
  UUID) for hard isolation (no DHCP, no NAT — you own addressing). On **macOS 26 (this host)**
  the new `vmnet_network_*` object API (`vmnet_network_configuration_create`,
  `vmnet_network_create`, `vmnet_interface_start_with_network`) gives first‑class multi‑network
  support with `set_ipv4_subnet`, `disable_dhcp`, `disable_nat44`, and DHCP reservations.
- **Host‑side BGP peer:** prefer **embedding GoBGP** (`github.com/osrg/gobgp/v3`) in the Go CLI.
  GoBGP has **no native macOS FIB installer** (it programs kernels only via FRR/zebra, which is
  Linux‑centric), so the CLI watches GoBGP's RIB and injects routes into the macOS kernel itself
  — exactly the `route`/PF_ROUTE technique docker‑mac‑net‑connect uses. FRR‑via‑Homebrew is
  *not* recommended: zebra runs on Darwin but its kernel programming path is not a supported/
  reliable target.
- **Wildcard ingress DNS:** ship a **dnsmasq**‑style resolver, but the cleanest self‑contained
  option is an **embedded DNS server in the CLI** bound to `127.0.0.1`, wired in via an
  **`/etc/resolver/<domain>` file containing `nameserver 127.0.0.1`**. macOS `resolver(5)` +
  `mDNSResponder` route the whole domain (and all subdomains — effectively wildcard) to it, and
  the system resolver path means browsers honor it.

Recommended combo for the workshop default: **shared‑mode vmnet, subnet pinned, LB pool in the
static range, Cilium L2 Announcements on; optional BGP mode via embedded GoBGP + route
injection; embedded DNS + `/etc/resolver`. Multi‑cluster via one vmnet network per cluster
(host‑mode+UUID or, on macOS 26, the new network‑object API).**

---

## 1. vmnet.framework modes: L2 topology and fidelity

Source: `vmnet.h` header (SDK), constant values and doc comments quoted verbatim.

```
VMNET_HOST_MODE    = 1000
VMNET_SHARED_MODE  = 1001
VMNET_BRIDGED_MODE = 1002   // API_AVAILABLE(macos(10.15))
```

Header discussion of the three modes (verbatim):

- **Host mode** — *"the VM can communicate with other VMs and the host OS, but is unable to
  communicate with the outside network."* Constant comment: *"Allows the vmnet interface to
  communicate with other vmnet interfaces that are in host mode and also with the native host."*
- **Shared mode** — *"the VM can reach the Internet through a network address translator (NAT),
  as well as communicate with other VMs and the host OS."* Constant comment: *"By default, the
  vmnet interface is able to communicate with other shared mode interfaces. If a subnet range is
  specified, the vmnet interface can communicate with other shared mode interfaces on the same
  subnet."*
- **Bridged mode** — *"the VM traffic is bridged directly to a particular physical network
  interface."* Requires `vmnet_shared_interface_name_key` naming the physical NIC; the list of
  bridgeable NICs comes from `vmnet_copy_shared_interface_list()`.

**Broadcast domain / L2 topology.** Both host mode and shared mode create a virtual switch
(surfaced on the host as `bridge100`/`bridgeN`) where **all interfaces in that mode/subnet share
a single broadcast domain that also includes the macOS host.** That is exactly the shared L2
segment we want. Bridged mode instead places the VM directly onto the physical LAN's broadcast
domain (its IP comes from the LAN's DHCP, not macOS's internal server).

**L2 fidelity and the anti‑spoofing filter.** vmnet is a *filtering* virtual switch, not a dumb
hub. Per the Apple DTS engineer on the developer forum (thread 693146, "Use multiple IPs with
vmnet"): in **host and shared modes** *"Packets sent from a different IPv4 address are dropped by
the system."* The engineer's recommended escape hatch is bridged mode: *"A vmnet interface in
bridged mode … can acquire its own IP address from the same network infrastructure as the host,
and that IP address isn't subject to this restriction."*
(<https://developer.apple.com/forums/thread/693146>)

Consequences for our use case:
- The switch **does forward broadcast and ARP** among shared‑mode peers and to the host — this
  is required for basic VM‑to‑VM and VM‑to‑host connectivity, which the header explicitly
  promises. Gratuitous ARP and ARP requests/replies for on‑segment IPs therefore propagate.
- MAC learning: vmnet allocates/expects a MAC per interface (`vmnet_mac_address_key`,
  `vmnet_allocate_mac_address_key`) and enforces that VMs source traffic from their assigned
  MAC/IP. Softnet's docs corroborate this filtering model (it *"restricts the VM networking …
  limiting VMs to send traffic only from their assigned MAC address, DHCP‑allocated IP, and to
  gateway IPs — blocking … ARP spoofing"*).
  (<https://github.com/cirruslabs/softnet/blob/main/README.md>)

**Do Cilium L2 Announcements pass?** Cilium's L2 Announcements feature elects one node to answer
ARP/NDP for a service VIP, replying with **that node's own MAC**; the node then load‑balances the
traffic into the cluster. Cilium docs: *"these IPs are Virtual IPs (not installed on network
devices) on multiple nodes, so … one node at a time will respond to ARP/NDP queries and respond
with its MAC address"* and *"only one node in the cluster is allowed to reply to requests for a
given IP."* It relies on ordinary L2: *"It requires switches to forward these neighbor discovery
messages, which is typical behavior on standard Ethernet networks."*
(<https://docs.cilium.io/en/stable/network/l2-announcements/>)

Mapping that onto vmnet shared mode:
- The **Ethernet source MAC** of Cilium's ARP reply is the announcing node's real, allocated MAC
  → passes the MAC filter.
- The **IP source** of forwarded/return data traffic is the node's own node‑IP (with default
  SNAT) → passes the "different IPv4 address is dropped" filter.
- **The open question:** the ARP *reply/gratuitous‑ARP payload* carries **sender‑protocol‑address
  = the LB VIP**, which is *not* the node's assigned IP. **No primary source I found states
  whether vmnet's anti‑spoofing filter inspects the ARP sender‑IP field** (as opposed to the IP
  header of IP packets). If it does, L2 announcements for VIPs outside a node's own address will
  be silently dropped and LB IPs will be unreachable. **This must be validated empirically on the
  target macOS version before committing the design.** If it fails, fallbacks are: put the LB VIP
  as a *secondary IP on the node itself* (so the announced IP equals an assigned IP), or move to
  BGP mode (§3), or bridged mode (needs the entitlement, §6).

Known limitations to note in the CLI docs: single‑node ARP responder per VIP (no pre‑cluster
load balancing), incompatibility with `externalTrafficPolicy: Local`, and failover governed by
`l2announcements.leaseDuration` (15s), `leaseRenewDeadline` (5s), `leaseRetryPeriod` (2s). L2
Announcements also **requires kube‑proxy replacement enabled** and at least one
`CiliumL2AnnouncementPolicy`; selected devices must appear in Cilium's `--devices`/`devices`
list, and the `interfaces` field takes OR‑ed Go regexes.
(<https://docs.cilium.io/en/stable/network/l2-announcements/>)

---

## 2. IPs outside the vmnet DHCP range (Cilium LB‑IPAM)

This is directly answered by the header comment on `vmnet_start_address_key` (verbatim):

> *"This address is used as the gateway address. The subsequent address up to and including
> `vmnet_end_address_key` are placed in the DHCP pool. **All other addresses are available for
> static assignment.**"* (`vmnet.h`)

So a shared/host‑mode network is described by three keys that **must be supplied together**
(header: *"Must be specified along with …"*):

- `vmnet_start_address_key` — gateway (first usable); RFC‑1918 required.
- `vmnet_end_address_key` — last DHCP address.
- `vmnet_subnet_mask_key` — mask.

Everything between `end_address+1` and the subnet broadcast is **static space**. **Put the
Cilium LB‑IPAM pool there.** Because the host is attached to the same `bridge100` segment with
its own IP (the gateway/host IP), and the LB pool is inside the same subnet/mask, LB IPs are
**on‑link for the host** — no extra static route needed for reachability *to the segment*. What's
needed is for something to **answer ARP** for each LB IP; that is precisely Cilium L2
Announcements (§1). Once a node answers ARP with its MAC, the host's neighbor table maps
`LB‑IP → node‑MAC` and traffic flows.

If L2 Announcements can't be relied on (the §1 uncertainty), the host‑side workarounds are:
- **Static ARP / neighbor entries**: `arp -s <lb-ip> <node-mac>` (or `ndp` for v6) to pin each
  LB VIP to a chosen node's MAC. Brittle (fails over poorly) but deterministic.
- **Static route via a node IP**: `route -n add -host <lb-ip> <node-ip>` if you want the host to
  send LB traffic to a specific node's node‑IP as a gateway rather than resolving the VIP on‑link.
- **BGP mode** (§3): the node advertises the LB prefix and the host installs a real route.

Sizing rule for the CLI: choose the subnet, reserve `start..end` as a small DHCP band for node
provisioning, and expose the remaining upper range as the default `CiliumLoadBalancerIPPool`
CIDR. Document that the host's own address is the gateway (`vmnet_start_address_key`) — on the
new macOS‑26 network API the header notes *"The second address is reserved for the host"* when a
subnet is set via `vmnet_network_configuration_set_ipv4_subnet`.

---

## 3. Host‑side BGP peer on macOS

Cilium's BGP control plane (CiliumBGPPeeringPolicy / `CiliumBGPClusterConfig` advertisements) can
peer with a daemon on the macOS host and advertise pod/LB CIDRs. Two implementation options:

### (a) FRR via Homebrew — not recommended
FRR **compiles and runs on macOS** and zebra starts (vtysh works, `show ip route` works), but the
Darwin port is not a supported target and hits kernel‑interface gaps: the tracking issue reports
repeated `if_ioctl(SIOCGIFMEDIA) failed: Operation not supported on socket` and MPLS disabled
(*"no kernel support"*). Critically, **whether zebra reliably programs the macOS kernel FIB via
the BSD routing socket (PF_ROUTE) is not established** by the issue — connected routes appear but
active route installation is unconfirmed.
(<https://github.com/FRRouting/frr/issues/2972>) FRR's platform statement is GNU/Linux + BSD, with
platform‑specific code concentrated in zebra (<https://docs.frrouting.org/en/latest/zebra.html>).
Bundling FRR also means shipping a multi‑daemon C stack + Homebrew dependency — heavy for a CLI.

### (b) Embedded GoBGP — recommended
`github.com/osrg/gobgp/v3` embeds directly in the Go CLI. From GoBGP's library docs
(<https://github.com/osrg/gobgp/blob/master/docs/sources/lib.md>):

```go
s := server.NewBgpServer(server.LoggerOption(...))
go s.Serve()
s.StartBgp(ctx, &api.StartBgpRequest{ Global: {Asn, RouterId, ListenPort} })
s.AddPeer(ctx, &api.AddPeerRequest{Peer: &api.Peer{Conf: {NeighborAddress, PeerAsn}}})
```

Route observation: **`WatchEvent`** (with `WatchEventMessageCallbacks` / `OnPeerUpdate`) for
session + path events, and **`ListPath`** to enumerate learned paths, exposing `Best`, `PeerASN`,
`PeerAddress`, etc.

**FIB installation is the catch:** GoBGP has **no native macOS route programmer**. Its only FIB
path is **zebra** — *"GoBGP uses zebra included in Quagga or FRRouting to manage routes in the
Linux kernel"* (<https://github.com/osrg/gobgp/blob/master/docs/sources/zebra.md>), which is
Linux‑oriented and not usable as a clean macOS FIB backend. Confirmed by open issues asking for
kernel export as a non‑default capability (<https://github.com/osrg/gobgp/issues/1238>,
<https://github.com/osrg/gobgp/issues/2582>). The lib docs section on FIB is silent for non‑Linux.

**Therefore the CLI itself injects routes into the macOS kernel.** Two mechanisms:
- **Shell out** (simplest, proven): docker‑mac‑net‑connect does exactly this on Darwin —
  `route -q -n add -inet <subnet> -interface <iface>` to add and
  `route -q -n delete -inet <subnet>` to remove
  (<https://github.com/chipmk/docker-mac-net-connect/blob/main/networkmanager/networkmanager.go>).
- **PF_ROUTE socket** (no exec, cleaner lifecycle): open `AF_ROUTE`/`PF_ROUTE` and write
  `RTM_ADD`/`RTM_DELETE` messages; in Go, `golang.org/x/net/route` builds/parses these messages.

Recommended pattern for BGP mode: embed GoBGP → peer with each Cilium node's BGP speaker →
`WatchEvent`/`ListPath` to learn advertised LB/pod prefixes → install/withdraw them in the macOS
kernel via PF_ROUTE (fallback: `route` exec), next‑hop = the advertising node's on‑segment IP.
This keeps the whole substrate in one Go binary with no external routing daemon.

---

## 4. Wildcard DNS for ingress (`*.workshop.local` → ingress LB IP)

### (a) `/etc/resolver/<domain>` (`resolver(5)` + mDNSResponder)
macOS reads per‑domain resolver files from `/etc/resolver/`; **the filename is the domain** and
the file contains directives like `nameserver 127.0.0.1` (and optionally `port`). mDNSResponder
routes queries for that domain **and all of its subdomains** to the specified nameserver — this
is *domain scoping*, which yields effective wildcard behavior for anything under the domain.
Because it lives in the **system resolver path (`mDNSResponder`)**, applications using standard
resolution — **including browsers** — honor it. Caveat from the community writeups:
mDNSResponder loads these files at creation and needs `sudo killall -HUP mDNSResponder` to pick up
changes. (<https://til.simonwillison.net/macos/wildcard-dns-dnsmasq>,
<https://dev.to/timtsoitt/how-to-resolve-local-wildcard-domains-in-macos-h5e>)

Note: `/etc/resolver` points a domain **at a nameserver**; it does not itself map a name to an IP.
You still need a resolver listening on `127.0.0.1` that answers with the ingress IP. Options (b)/(c).

### (b) dnsmasq via Homebrew
dnsmasq answers a whole domain to one IP with a single line: `address=/workshop.local/<ingress-ip>`
(the leading form `address=/.domain/ip` is also used) — true wildcard for the domain and all
subdomains. Paired with `/etc/resolver/workshop.local` → `nameserver 127.0.0.1` (dnsmasq's port).
Downside for a CLI: an external Homebrew dependency and a separately managed launchd service.
(<https://til.simonwillison.net/macos/wildcard-dns-dnsmasq>)

### (c) Embedded DNS server in the Go CLI — cleanest to manage
Run a tiny authoritative resolver inside the CLI (e.g. `github.com/miekg/dns`) bound to
`127.0.0.1:<port>`, answering `*.workshop.local`/`*.cluster.test` with the current ingress LB IP
(which the CLI already knows). Wire it in by writing `/etc/resolver/workshop.local` containing
`nameserver 127.0.0.1` + matching `port`. This keeps DNS, IP allocation, and lifecycle in one
binary, avoids Homebrew, and updates instantly when ingress IPs change (no external file edits).
The only privileged step is writing `/etc/resolver/*` (root) — the same privilege boundary the
tool already crosses for vmnet/route work.

**Recommendation:** (c) embedded DNS + `/etc/resolver` file. It is self‑contained, wildcard‑capable
via domain scoping, honored by browsers through mDNSResponder, and needs no third‑party daemon.

---

## 5. Multiple isolated subnets for multiple named clusters

Three primary‑source‑backed approaches, in increasing isolation strength:

1. **Multiple concurrent shared‑mode networks, each pinned to its own subnet.** Supply distinct
   `vmnet_start_address_key`/`vmnet_end_address_key`/`vmnet_subnet_mask_key` triples per cluster.
   The header explicitly scopes shared‑mode reachability by subnet: *"If a subnet range is
   specified, the vmnet interface can communicate with other shared mode interfaces on the same
   subnet."* Different subnets ⇒ different broadcast domains. socket_vmnet demonstrates the
   "many independent networks" pattern operationally: *"Multiple independent `socket_vmnet`
   instances can run simultaneously, each managing separate virtual networks,"* selected by socket
   path (<https://github.com/lima-vm/socket_vmnet/blob/master/README.md>).

2. **Host mode + `vmnet_network_identifier_key` (macOS 11+).** Header:
   *"The identifier (uuid) to uniquely identify the network … If this property is set, the
   vmnet_interface is added to an isolated network with the specified identifier. **No DHCP
   service is provided on this network.**"* Companion keys `vmnet_host_ip_address_key` and
   `vmnet_host_subnet_mask_key` set the host's address on that isolated segment (both macOS 11+).
   This gives hard per‑cluster isolation with a UUID as the cluster network handle — but **you own
   addressing** (no DHCP), so the CLI must assign node IPs (e.g. via Talos static config). Also
   note there is a separate per‑*interface* isolation switch, `vmnet_enable_isolation_key`
   (macOS 11+): interfaces that both set it *cannot* talk to each other — that's the opposite of
   what we want *within* a cluster, so leave it off for intra‑cluster peers.

3. **macOS 26 network‑object API (available on this host, 26.5.2).** The header adds a first‑class
   network abstraction (all `API_AVAILABLE(macos(26.0))`):
   `vmnet_network_configuration_create(mode)` → configure → `vmnet_network_create()` →
   `vmnet_interface_start_with_network(network, …)`. Configuration setters:
   `vmnet_network_configuration_set_ipv4_subnet(subnet, mask)` (header: *"the second address is
   reserved for the host"*), `set_ipv6_prefix`, `set_external_interface`, `disable_nat44`,
   `disable_nat66`, `disable_dhcp`, `disable_dns_proxy`, `disable_router_advertisement`,
   `add_dhcp_reservation(mac → ip)`, `add_port_forwarding_rule`, `set_mtu`. Defaults if untouched:
   *"IPv4 subnet: a /24 under 192.168/16"*, NAT/DHCP/DNS‑proxy/RA enabled, MTU 1500. `mode` here is
   *"Shared mode or host‑only mode."* This is the **best multi‑cluster primitive when you can
   require macOS 26**: reserve a distinct subnet per cluster, keep DHCP for node bring‑up
   (or disable it and use DHCP reservations for stable node IPs), and each `vmnet_network_ref` is
   a clean isolated segment.

**CLI implication:** model a "cluster network" as either (2)/(3) a distinct vmnet network object
(preferred; UUID or network‑ref = the isolation boundary) or (1) a distinct shared‑mode subnet.
On macOS < 26, use approach 1 or 2; on macOS 26+, prefer approach 3.

---

## 6. Permissions / entitlements

From the header, forum, and Apple entitlement docs:

| Mode | Entitlement `com.apple.vm.networking`? | Privilege |
|---|---|---|
| `VMNET_SHARED_MODE` (NAT) | **Not required** | Create vmnet iface needs root (or a root helper) |
| `VMNET_HOST_MODE` | **Not required** | Same |
| `VMNET_BRIDGED_MODE` | **Required** | Root **and** the restricted entitlement |

- **`com.apple.vm.networking` is a *restricted* entitlement** that *"must be authorized by a
  provisioning profile"* and *"indicates whether the app manages virtual network interfaces
  without escalating privileges to the root user."* It is **granted only to virtualization‑
  software developers via an Apple representative / DTS incident**, then baked into the code
  signature. It **cannot be prototyped without it** — `VZVirtualMachineConfiguration.validate()`
  fails. socket_vmnet's README concurs: signing with this entitlement *"seems to require some
  contract with Apple."*
  (<https://developer.apple.com/documentation/bundleresources/entitlements/com.apple.vm.networking>,
  <https://developer.apple.com/forums/thread/656411>,
  <https://github.com/lima-vm/socket_vmnet/blob/master/README.md>)
- **Only bridged mode needs it.** Shared and host modes do **not**. (Corroborated by the QEMU
  vmnet backend series and multiple forum threads.) → **Our default (shared mode) is entitlement‑
  free**, which is the decisive reason to prefer it. Bridged mode's advantages (VMs get real‑LAN
  IPs, no anti‑spoofing filter) are unavailable to us in practice without the Apple contract, so
  treat bridged mode as out of scope for the shipped CLI.

**Privilege‑boundary patterns from prior art (how to avoid running the whole CLI as root):**
- **socket_vmnet**: a **privileged daemon** creates the vmnet interface and passes file
  descriptors over a **Unix socket** (`socket_vmnet_client`) to the unprivileged consumer, so the
  VM process runs rootless. Started via **launchd**. Multiple daemons ⇒ multiple networks.
  (<https://github.com/lima-vm/socket_vmnet/blob/master/README.md>)
- **softnet** (tart): a **SUID‑root (or passwordless‑sudo) helper** creates the vmnet interface
  and tweaks DHCP, then **drops privileges to the calling user** after init. Tart spawns it as a
  subprocess and talks over `socketpair(2)`. It also **lowers the macOS DHCP lease from 86,400s to
  600s** to avoid pool exhaustion with many ephemeral VMs, and acts as a userspace packet filter
  (anti‑ARP‑spoof, MAC/IP pinning).
  (<https://github.com/cirruslabs/softnet/blob/main/README.md>)

**Recommendation:** ship a **small root helper** (SUID or launchd‑managed) that owns vmnet
interface creation, `/etc/resolver` writes, and kernel‑route injection, and drops privileges after
setup — mirroring softnet/socket_vmnet. Keep the main CLI unprivileged.

---

## 7. Prior art — concrete techniques to reuse

- **lima‑vm/socket_vmnet** — privileged vmnet daemon + FD‑passing over a Unix socket so QEMU runs
  rootless; launchd‑managed; shared/bridged/host modes; per‑network isolation by running multiple
  daemons; subnet/DHCP tuned with `--vmnet-gateway`, `--vmnet-dhcp-end`, `--vmnet-mask`,
  `--vmnet-mode`, `--vmnet-network-identifier`. **Reuse:** the root‑helper + FD/socket boundary and
  the "one daemon = one isolated network" model.
  (<https://github.com/lima-vm/socket_vmnet/blob/master/README.md>)
- **cirruslabs/softnet** — SUID helper that creates vmnet, tunes DHCP lease time, enforces
  MAC/IP/ARP anti‑spoofing in userspace, drops privileges post‑init, IPC over `socketpair(2)`.
  **Reuse:** privilege‑drop pattern and DHCP‑lease shortening for many short‑lived VMs; **note the
  anti‑spoofing filter as the corroborating evidence for the §1 L2‑announcement risk.**
  (<https://github.com/cirruslabs/softnet/blob/main/README.md>)
- **chipmk/docker-mac-net-connect** — makes VM‑internal container IPs host‑routable using a
  **wireguard‑go** `utun` tunnel between macOS and the Linux VM, then **watches Docker network
  events** (`cli.Events(..., type=network)`) and **injects/removes each network's subnet into the
  macOS routing table** via `route -q -n add -inet <subnet> -interface <utun>` /
  `route ... delete`; interface address set with `ifconfig <utun> inet <ip>/32 <peer>`. NAT maps
  the host's WireGuard IP to the Docker bridge gateway so containers see in‑network source.
  **Reuse (directly):** the *route‑injection loop* — watch a route source (here GoBGP/Cilium
  instead of Docker) and program the macOS FIB. It proves `route`‑exec works on Darwin; a PF_ROUTE
  variant is the cleaner evolution.
  (<https://github.com/chipmk/docker-mac-net-connect/blob/main/networkmanager/networkmanager.go>,
  <https://github.com/chipmk/docker-mac-net-connect/blob/main/README.md>)

**vz runtime note (Code‑Hex/vz).** The Talos VMs run under Virtualization.framework via
`Code-Hex/vz`, which exposes three attachments: `VZNATNetworkDeviceAttachment` (shared/NAT, no
entitlement), `VZBridgedNetworkDeviceAttachment` (**needs `com.apple.vm.networking`**), and
`VZFileHandleNetworkDeviceAttachment` (raw packets over a file handle). To attach a VM to a *custom*
vmnet shared/host network created by our own helper (rather than vz's built‑in NAT), the
file‑handle/datagram‑socket attachment is the bridge between the helper's vmnet interface and the
vz guest — the same seam socket_vmnet uses. Confirm the exact attachment vz version supports for
your target OS. (<https://github.com/Code-Hex/vz>, <https://pkg.go.dev/github.com/Code-Hex/vz/v3>)

---

## Recommendation & implications for the CLI

1. **Default networking mode = vmnet shared mode, subnet pinned.** Set
   `vmnet_start_address_key`/`vmnet_end_address_key`/`vmnet_subnet_mask_key` (all three) per
   cluster. Entitlement‑free, gives host‑routable RFC‑1918 IPs on a shared L2 domain incl. the
   host (`bridge100`). On **macOS 26** prefer the `vmnet_network_*` object API for cleaner
   multi‑network handling.
2. **LB‑IPAM pool in the static range** above `vmnet_end_address_key`. It's on‑link for the host;
   Cilium L2 Announcements makes each VIP answerable. Ship a small DHCP band for node bring‑up and
   expose the rest as the default `CiliumLoadBalancerIPPool`.
3. **Cilium config:** enable kube‑proxy replacement + `CiliumL2AnnouncementPolicy`; document
   lease/failover knobs. Provide **BGP mode** as an alternative: embedded **GoBGP** peering with
   Cilium's BGP control plane + **CLI‑side route injection** into the macOS kernel (PF_ROUTE
   preferred, `route` exec as fallback, next‑hop = node on‑segment IP). Do **not** ship FRR.
4. **Multi‑cluster isolation:** one vmnet network per cluster — host‑mode+UUID
   (`vmnet_network_identifier_key`, macOS 11+, you own addressing) or the macOS‑26 network‑object
   API. Fall back to distinct shared‑mode subnets on older OSes.
5. **DNS:** embedded resolver in the CLI on `127.0.0.1` + `/etc/resolver/<domain>` →
   `nameserver 127.0.0.1`. Wildcard via domain scoping; browsers honor it via mDNSResponder;
   `killall -HUP mDNSResponder` after writing the file.
6. **Privilege model:** small root helper (SUID or launchd) owns vmnet creation, `/etc/resolver`,
   and route injection; drop privileges after setup. Main CLI unprivileged.

**Showstoppers / risks to design around:**
- **[HIGH, unresolved] vmnet anti‑spoofing vs. Cilium L2 ARP for VIPs.** No source confirms
  whether the host/shared‑mode "different IPv4 address is dropped" filter inspects ARP
  sender‑protocol‑address. If it drops VIP ARP replies, L2 Announcements won't make LB IPs
  reachable. **Prototype first.** Mitigations: VIP as node secondary IP; BGP mode; (last resort)
  bridged mode — which needs the Apple entitlement we can't assume.
- **[HIGH] Bridged mode requires `com.apple.vm.networking`**, an Apple‑granted restricted
  entitlement — assume unavailable; keep bridged mode out of the default path.
- **[MED] Host‑mode isolation disables DHCP** — the CLI must assign node IPs (Talos static config).
- **[MED] No native macOS FIB in GoBGP/FRR** — the CLI must own kernel‑route programming.
- **[LOW] macOS‑version gating** — the network‑object API is macOS 26 only; `vmnet_network_identifier_key`/host‑IP keys are macOS 11+; bridged mode macOS 10.15+. Gate features by `API_AVAILABLE` version.

---

## Sources

- Apple `vmnet.framework` SDK header (authoritative), `vmnet.h`, Copyright 2013–2025 Apple Inc.
  Read at `/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX.sdk/System/Library/Frameworks/vmnet.framework/Versions/A/Headers/vmnet.h`.
- vmnet API overview: <https://developer.apple.com/documentation/vmnet>
- `VMNET_SHARED_MODE`: <https://developer.apple.com/documentation/vmnet/operating_modes_t/vmnet_shared_mode>
- "Use multiple IPs with vmnet" (DTS engineer on anti‑spoofing + bridged mode): <https://developer.apple.com/forums/thread/693146>
- `com.apple.vm.networking` entitlement: <https://developer.apple.com/documentation/bundleresources/entitlements/com.apple.vm.networking>
- Requesting `com.apple.vm.*` entitlements: <https://developer.apple.com/forums/thread/656411>
- Cilium L2 Announcements / L2‑aware LB: <https://docs.cilium.io/en/stable/network/l2-announcements/>
- Cilium L2 announcement interface‑needs‑IP issue: <https://github.com/cilium/cilium/issues/41641>
- FRR on macOS (zebra/Darwin limits): <https://github.com/FRRouting/frr/issues/2972>
- FRR zebra docs: <https://docs.frrouting.org/en/latest/zebra.html>
- GoBGP library usage: <https://github.com/osrg/gobgp/blob/master/docs/sources/lib.md>
- GoBGP + zebra (FIB via FRR): <https://github.com/osrg/gobgp/blob/master/docs/sources/zebra.md>
- GoBGP kernel‑export issues: <https://github.com/osrg/gobgp/issues/1238>, <https://github.com/osrg/gobgp/issues/2582>
- GoBGP v3 package: <https://pkg.go.dev/github.com/osrg/gobgp/v3>
- lima‑vm/socket_vmnet: <https://github.com/lima-vm/socket_vmnet/blob/master/README.md>
- socket_vmnet network modes: <https://deepwiki.com/lima-vm/socket_vmnet/4.3-network-modes>
- cirruslabs/softnet: <https://github.com/cirruslabs/softnet/blob/main/README.md>
- chipmk/docker-mac-net-connect route injection: <https://github.com/chipmk/docker-mac-net-connect/blob/main/networkmanager/networkmanager.go>
- docker-mac-net-connect README: <https://github.com/chipmk/docker-mac-net-connect/blob/main/README.md>
- Code‑Hex/vz (Virtualization.framework in Go): <https://github.com/Code-Hex/vz>, <https://pkg.go.dev/github.com/Code-Hex/vz/v3>
- Wildcard DNS on macOS (resolver + dnsmasq): <https://til.simonwillison.net/macos/wildcard-dns-dnsmasq>, <https://dev.to/timtsoitt/how-to-resolve-local-wildcard-domains-in-macos-h5e>
