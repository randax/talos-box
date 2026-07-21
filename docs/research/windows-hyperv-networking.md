# Hyper-V Networking for Host-Routable Guest IPs (mirroring the macOS/vmnet substrate)

Research date: 2026-07-21. Ticket:
[#51 — Map Hyper-V networking options for host-routable guest IPs](https://github.com/randax/talos-box/issues/51)
(part of [#48](https://github.com/randax/talos-box/issues/48)). Companion to
[`docs/research/macos-networking-substrate.md`](./macos-networking-substrate.md), which this
document mirrors section-for-section. The macOS design being reproduced is
[`docs/SPEC.md` §5](../SPEC.md#5-networking): one shared-L2 subnet per cluster with
host-routable IPs, DHCP + NAT egress on the same switch, the host at `.1` as
gateway/NAT/DNS/BGP-peer/inter-cluster-router, a Cilium-announced LB VIP in `.200–.239` that
must be ARP-reachable from the host, and IP-forwarded routing between cluster subnets.

Sources are Microsoft Learn (`learn.microsoft.com`) cmdlet/concept docs, Microsoft's own
GitHub repos and archived TechCommunity/blog posts, and — for the "how do comparable tools cope"
survey — the minikube, WSL2, and podman docs/issue trackers. Every claim below is marked either
**verified** (with a citation) or **inference** (reasoning given, no primary source found).
Where sources conflict, both sides are shown.

---

## TL;DR / Recommendation

**One Hyper-V Internal vSwitch per cluster; host owns each switch's `.1` gateway IP; a single
host-wide NAT over a supernet that every cluster `/24` is carved out of; a userland (Go) DHCP
server bound to each vEthernet adapter; `MacAddressSpoofing On` on every node vNIC with no Port
ACLs; and per-adapter IP forwarding for inter-cluster routing.** This reproduces every property
of the macOS/vmnet substrate, but three structural differences are forced by Windows and must be
designed around rather than assumed away:

1. **NAT is host-wide, not per-network — a hard OS limit.** WinNAT supports exactly **one**
   NAT internal prefix per host, verified in Microsoft's own docs. You cannot give each cluster
   an independent NAT the way vmnet gives each shared-mode network its own. The Microsoft-blessed
   workaround is one NAT over a supernet (e.g. `172.30.0.0/16`) that every cluster `/24` sits
   inside.
2. **There is no built-in DHCP on Internal switches**, unlike vmnet shared mode which bundles
   DHCP for free. talosbox must run its own DHCP server bound to the host vEthernet adapters —
   no mainstream Windows tool (minikube, Docker Desktop, WSL2, podman) actually does this in the
   way talosbox needs; they fall back to static IPs, container-specific address assignment, or
   Microsoft's own hidden allocator. This is the one piece of new host-daemon surface with no
   existing template to copy.
3. **The ARP-anti-spoofing analogue is (probably) inverted relative to macOS.** On macOS the VIP
   works *because* vmnet's filter happens not to inspect ARP sender-protocol-address. On Hyper-V,
   IP/ARP-level filtering is **opt-in** (Port ACLs must be explicitly added) — so a guest-announced
   VIP should pass *by default*, with no configuration at all. But practitioner reports for
   MetalLB/VRRP-style L2 VIPs insist on setting `MacAddressSpoofing On`, and those reports are not
   fully consistent with the documented mechanics. The safe, cheap resolution is to just set
   `MacAddressSpoofing On` and add no Port ACLs — this is the item to **prototype first**, exactly
   as the macOS design flagged vmnet's ARP behavior as the biggest architectural risk.

Recommended mapping, vmnet → Hyper-V:

| vmnet (macOS) property | Hyper-V realization |
|---|---|
| One shared L2 domain incl. host, per cluster | **Internal** vSwitch per cluster (host gets a `vEthernet (SwitchName)` vNIC on it) |
| Host is `.1` gateway/NAT/DNS/BGP peer | `New-NetIPAddress 172.30.<n>.1/24` on each vEthernet adapter; host services bind those adapters |
| DHCP `.10–.179` on the switch | Own userland (Go) DHCP server bound to each vEthernet adapter — no built-in DHCP exists |
| NAT egress on the same switch | Single host-wide `New-NetNat -InternalIPInterfaceAddressPrefix 172.30.0.0/16` supernet covering all clusters (one-NAT-per-host limit) |
| VIP `.200` reachable via guest ARP | `MacAddressSpoofing On` on node vNICs, **no** Port ACLs → gratuitous/reply ARP for the VIP passes |
| Multiple isolated subnets, host routes between them | Per-cluster Internal switches + `Set-NetIPInterface -Forwarding Enabled` on each vEthernet adapter |
| Fixed per-VM MACs | `Set-VMNetworkAdapter -StaticMacAddress` per node |

---

## 1. Switch type semantics: Internal vs External vs Private vs Default Switch

**Verified** (Microsoft Learn):

- Hyper-V Virtual Switch is "a software-based **layer-2 Ethernet network switch**."
  (<https://learn.microsoft.com/en-us/windows-server/virtualization/hyper-v/virtual-switch>)
- Per "Create and configure a virtual switch"
  (<https://learn.microsoft.com/en-us/windows-server/virtualization/hyper-v/get-started/create-a-virtual-switch-for-hyper-v-virtual-machines>):
  - **External** — connects VMs to the physical network; Hyper-V takes ownership of a physical
    NIC; VMs, host, and physical LAN devices all communicate.
  - **Internal** — "connects to a network that can be used only by the virtual machines running
    on the host … **and between the host and the virtual machines**. An internal network is the
    same as a private network, except that **the parent partition (physical host) acquires a
    virtual network adapter** which is automatically connected to this virtual switch."
  - **Private** — VMs only; "**doesn't provide networking between the host and the virtual
    machines**."
- `New-VMSwitch -SwitchType` accepts only **Internal** and **Private**; supplying
  `-NetAdapterName`/`-NetAdapterInterfaceDescription` "implicitly set[s] the type of the virtual
  switch to External." `-AllowManagementOS` on an External switch controls "whether the parent
  partition … is to have access to the physical NIC."
  (<https://learn.microsoft.com/en-us/powershell/module/hyper-v/new-vmswitch>)

**Application to talosbox:**

- **Internal is the correct analogue of vmnet shared mode**: one shared L2 broadcast domain per
  switch that includes the host, host-routable RFC-1918 addresses, guest↔host works. Private is
  wrong (excludes the host — no gateway/NAT/DNS/BGP peer). External is wrong (bridges onto the
  physical LAN, needs a physical NIC, no per-cluster isolation, takes over the host's real NIC).
- **Key difference from vmnet (verified via the NAT setup doc, §2):** an Internal switch is
  *pure L2 plus a host vNIC and nothing else* — no DHCP, no NAT, no gateway IP. vmnet shared mode
  bundles DHCP + NAT automatically; Hyper-V does not. All three (gateway IP, NAT, DHCP) must be
  added by talosbox.
- **"Default Switch"** — the auto-created, GUID-suffixed `vEthernet (Default Switch)` used by
  WSL2/Quick Create — is a special Internal switch Microsoft auto-wires to NAT plus an
  ICS-like DHCP/DNS responder. **Not usable as a template**: its subnet is not user-controllable
  and is documented to change across host reboots — a known pain point for minikube users
  (<https://minikube.sigs.k8s.io/docs/drivers/hyperv/>). talosbox needs its own Internal switches
  with fixed, pinned subnets.

## 2. NetNat limits: the one-NAT-per-host constraint

**Verified, primary source** (Microsoft Learn, "Set up a NAT network,"
<https://learn.microsoft.com/en-us/windows-server/virtualization/hyper-v/setup-nat-network>):

- Verbatim: *"Currently, you're limited to **one NAT network per host**."*
- Troubleshooting section, verbatim: *"**Multiple NAT networks are not supported** … Since
  Windows (WinNAT) only supports **one internal NAT subnet prefix**, trying to create multiple
  NATs places the system into an unknown state."*

The commonly-cited constraint is **true and official** — a hard OS limit of the WinNAT stack,
not just a best-practice recommendation.

**Official workaround: one NAT over a supernet, with per-app subnets carved out of it.** The
same doc's "Multiple Applications using the same NAT" section works this exact scenario:

> `New-NetNat -Name DockerNAT -InternalIPInterfaceAddressPrefix 10.0.0.0/17`
> "In the end, you have two internal vSwitches … You only have **one NAT network
> (10.0.0.0/17)** … IP addresses … are assigned … from the 10.0.76.0/24 subnet [and]
> 10.0.75.0/24 subnet."

And: *"If you need to attach multiple VMs and containers to a single NAT, you will need to
ensure that the NAT internal subnet prefix is large enough to encompass the IP ranges being
assigned … This requires … manual configuration which must be done by an admin and guaranteed
not to re-use existing IP assignments on the same host."*

**Application to talosbox's multi-cluster model:** instead of N independent NATs (one per
`172.30.<n>.0/24`), create **one** `New-NetNat` over a supernet such as `172.30.0.0/16`. Each
cluster's Internal switch host adapter gets `172.30.<n>.1/24`; every cluster `/24` sits inside
the `/16`, so all clusters share the one NAT for internet egress. This maps directly onto
talosbox's existing subnet scheme — only the supernet needs to be pre-reserved. Inter-cluster
traffic is handled by IP forwarding (§6), not NAT; only genuinely internet-bound traffic should
hit the NAT translation table (conceptually confirmed by the NAT overview: NAT "uses a flow
table to route traffic from an external (host) IP Address and port number to the correct
internal IP address").

`New-NetNat` reference
(<https://learn.microsoft.com/en-us/powershell/module/netnat/new-netnat>):
`-InternalIPInterfaceAddressPrefix` "specifies the address prefix of the internal interface."
WinNAT keys purely off this single prefix — which is *why* only one can exist at a time.

**Alternatives considered:**

- **ICS (Internet Connection Sharing)** — also imposes single-shared-connection semantics
  (enabling public sharing on a new connection auto-disables any previous one, per
  <https://learn.microsoft.com/en-us/windows/win32/api/netcon/nf-netcon-inetsharingconfiguration-enablesharing>)
  and forces its own subnet (historically `192.168.137.0/24`) with a DHCP allocator talosbox
  doesn't control. Less flexible than WinNAT for a multi-subnet design, not more.
- **RRAS (Routing and Remote Access Service)** — can NAT multiple internal interfaces
  independently, sidestepping WinNAT's one-prefix limit, but is a Server role with a heavier
  footprint and a different management surface; its availability on Windows 10/11 **client**
  SKUs (talosbox's likely target, by analogy with the macOS Apple-Silicon-only scope) is an open
  question. Kept as a documented fallback, not the default.
- **Recommendation:** the supernet + single-WinNAT approach — it is officially documented,
  simplest, and directly analogous to how talosbox already carves `172.30.<n>.0/24` out of a
  shared numbering scheme.

*(The WinNAT capabilities/limitations TechCommunity post,
<https://techcommunity.microsoft.com/blog/virtualization/windows-nat-winnat----capabilities-and-limitations/382303>,
is the canonical deep-dive the Learn doc draws from; its body did not render through the fetch
tooling used for this research, but the Learn NAT doc reproduces its operative constraints
verbatim, so the one-prefix limit is attested from two Microsoft-owned pages.)*

## 3. DHCP on Internal switches: there is none — must be supplied

**Verified** (Microsoft Learn, NAT setup doc): *"Since WinNAT by itself **does not allocate and
assign IP addresses** to an endpoint (e.g. VM), you'll need to do this manually from within the
VM itself — i.e. set IP address … gateway … DNS."* Internal/Private switches ship with **no
DHCP server**. (The one exception is Windows containers, where HNS/HCS assigns addresses
directly — not applicable to full Talos VMs.)

**Survey of comparable tools:**

- **minikube's hyperv driver** — historically depended on the auto-NAT Default Switch's hidden
  DHCP, unreliable because that subnet changes on every host reboot. minikube's docs steer users
  toward an External switch (bridges to a real LAN DHCP server) and, as of v1.29, added a
  `--static-ip` flag rather than depend on Internal-switch DHCP at all.
  (<https://minikube.sigs.k8s.io/docs/drivers/hyperv/>,
  <https://minikube.sigs.k8s.io/docs/tutorials/static_ip/>). A minikube issue thread notes,
  usefully, that *"if you run a DHCP server on the NAT, everything works"*
  (<https://github.com/kubernetes/minikube/issues/1627>) — the closest real-world confirmation
  that userland DHCP on a Hyper-V Internal/NAT switch is viable.
- **Docker Desktop (legacy MobyLinuxVM / docker-machine hyperv)** — used the `DockerNAT`
  Internal switch with admin-assigned **static** IPs from a fixed sub-range (`10.0.75.0/24`,
  per the NAT setup doc's own Docker example), not a general DHCP server for arbitrary VMs.
- **WSL2** — gets its address via a Microsoft-managed, internal networking/ICS-style service on
  the Default Switch (NAT mode) — Microsoft's *own* userland allocator, not something reusable by
  third parties. In **mirrored mode** (Windows 11 22H2+), WSL bypasses the NAT subnet and mirrors
  the host's interfaces entirely instead.
  (<https://learn.microsoft.com/en-us/windows/wsl/networking>)
- **podman machine (hyperv)** — sidesteps L2 DHCP entirely via **gvproxy**, user-mode networking
  over vsock; guest addressing is handled by the user-mode stack, not switch DHCP. Not directly
  reusable for a shared-L2, host-routable model (closer to slirp/user-mode NAT than to vmnet
  shared mode).

**Net:** no mainstream Windows tool runs a general-purpose DHCP server on an Internal switch the
way vmnet does for free. They fall back to static IPs, container-specific allocation, Microsoft's
own hidden service, or user-mode networking. **talosbox will need to bring its own DHCP server**
— conceptually the same thing vmnet already does under the hood on macOS, just now talosbox's own
daemon instead of Apple's.

**Can a Go DHCP server on the host vEthernet adapter work? — inference, well-supported, needs a
prototype:**

- The host vEthernet adapter of an Internal switch is a real host NIC on the same L2 broadcast
  domain as the guests (verified: Internal switches give the parent partition a vNIC on the
  segment, §1). A guest's `DHCPDISCOVER` is an L2/UDP broadcast
  (`ff:ff:ff:ff:ff:ff` / `255.255.255.255`), and every port on an L2 switch forwards broadcasts by
  design — no promiscuous-mode requirement. A Go server binding UDP `:67` on that interface should
  see it.
- **Corroboration:** the minikube issue above is the closest real-world confirmation that DHCP on
  a Hyper-V NAT/Internal switch functions in practice.
- **Caveats to validate empirically:** the server must reply as broadcast (or unicast to the
  client's `chaddr`) since the client has no IP yet; Windows Defender Firewall will by default
  block inbound UDP 67 unless an explicit allow rule is added (§4); with N cluster switches, the
  server must be interface-aware and answer each subnet's correct scope.
- **ICS as an alternative DHCP source** — API surface is `HNetCfg.HNetShare` →
  `INetSharingManager`, with `INetSharingConfiguration::EnableSharing`
  (<https://learn.microsoft.com/en-us/windows/win32/api/netcon/nf-netcon-inetsharingconfiguration-enablesharing>;
  overview at
  <https://learn.microsoft.com/en-us/previous-versions/windows/desktop/ics/getting-started-using-the-ics-and-icf-api>).
  **Not recommended**: ICS forces its own subnet and single-shared-connection semantics, which
  conflicts with the multi-cluster supernet design. Prefer a self-hosted DHCP server.

## 4. Windows Defender Firewall interaction

**Two distinct firewalls — verified, do not conflate:**

1. **Host Windows Defender Firewall**, applied to the **host vEthernet adapter** of each Internal
   switch — the surface that matters for talosbox, since gateway/NAT/DNS/DHCP/BGP services run on
   the host and receive guest traffic through that adapter.
2. **"Hyper-V Firewall"** (Windows 11 22H2+) — a *separate* per-VM firewall that, per Microsoft,
   "enables filtering of inbound and outbound traffic **to/from containers hosted by Windows,
   including the Windows Subsystem for Linux (WSL)**," keyed by `VMCreatorId` (WSL's is a fixed
   GUID).
   (<https://learn.microsoft.com/en-us/windows/security/operating-system-security/network-security/windows-firewall/hyper-v-firewall>)
   **Verified and important:** this firewall targets WSL and Windows-container VMs, **not**
   ordinary Hyper-V VMs like Talos nodes — talosbox should not need
   `Set-NetFirewallHyperVRule`, only ordinary host firewall rules on the vEthernet adapter.

**Profile assignment gotcha (verified, high-impact):**

- The host vEthernet adapter of an Internal/NAT switch is commonly classified into the
  **Public** network profile by default (Windows cannot identify it as a known domain/private
  network), and firewall rules then follow Public-profile policy. (Widely reported behavior;
  treated here as **inference/known-behavior** rather than a single crisp Microsoft statement —
  validate on the target Windows build.)
- Real failure mode, documented in Microsoft's own repo
  (<https://github.com/microsoft/Windows-Containers/issues/203>): when the security policy
  **"Apply Local Firewall Rules = No"** is set on the Public profile, *"local firewall rules that
  Internet Connection Sharing and the Host Networking Service adds to permit DHCP and DNS for
  Hyper-V guests are no longer applied,"* breaking guest **DHCP and DNS** on the NAT network.
  These service-added rules are dynamically generated per interface/SID and are fragile under
  hardened/enterprise firewall policy — directly analogous to how the macOS design already treats
  corporate-managed-machine agents (GlobalProtect) as an unreliable-by-default variable for
  registry-mirror traffic (SPEC §5); a locked-down Windows box could hit the same class of issue.

**Concrete rules talosbox must add on the host:**

- Explicit **inbound allow** rules for the DHCP server (UDP 67), the DNS resolver (UDP/TCP 53),
  and ICMP, scoped to the vEthernet adapters/cluster subnets — do not rely on
  auto-generated ICS/HNS rules, since those are the ones documented to disappear under hardened
  policy.
- Consider forcing the vEthernet adapters into the **Private** profile
  (`Set-NetConnectionProfile -NetworkCategory Private`) so default rules are less restrictive.
- Egress/NAT path (guest→internet): WinNAT operates in the host networking stack; the outbound
  guest→NAT→internet path is generally not blocked by host inbound rules — the fragile parts are
  DHCP/DNS bootstrapping and the return path for host-bound guest traffic.

## 5. ARP/L2 behavior for a guest-announced VIP (the vmnet anti-spoofing analogue)

This is the direct counterpart to the macOS finding that vmnet's anti-spoofing filter does not
inspect ARP sender-protocol-address.

**a) MAC-level filtering (`MacAddressSpoofing`) — verified.** Default is **Off**: per
`Set-VMNetworkAdapter`
(<https://learn.microsoft.com/en-us/powershell/module/hyper-v/set-vmnetworkadapter>), Off means
the VM is allowed to use only the MAC address assigned to it. This blocks a VM from *changing
the source Ethernet MAC* on outgoing frames — it does not, by itself, inspect ARP payloads or
source IPs.

**b) IP/ARP-level filtering is opt-in, not default — verified, key finding.** Per Microsoft's
archived post "ARP Spoofing Prevention in Windows Server 2012 Hyper-V"
(<https://learn.microsoft.com/en-us/archive/blogs/wincat/arp-spoofing-prevention-in-windows-server-2012-hyper-v>),
the anti-ARP-spoofing feature is implemented as **Port Access Control Lists** and "must be
configured via PowerShell" using `Add-VMNetworkAdapterAcl`. Out of the box, **there is no
per-IP ARP/source-IP filter** — an administrator must explicitly add
`LocalIPAddress … Allow` / `LocalIPAddress ANY … Deny` ACLs to get one. Absent Port ACLs, a guest
can send ARP for, and source traffic from, an IP the switch never assigned.

**This means Hyper-V's default posture is structurally permissive toward a Cilium-style
L2-announced VIP** — provided the announcement uses the node's own MAC (which Cilium L2
announcement and MetalLB L2 both do; they answer ARP for the VIP with the node interface's real
MAC, no virtual MAC). The relevant frame: source Ethernet MAC = node's assigned MAC (passes
MacAddressSpoofing=Off); ARP sender-protocol-address = the VIP, an unassigned IP (not filtered
without a Port ACL). By the letter of the documented mechanics, this should pass **by default,
with no configuration at all** — structurally the same lucky-default outcome vmnet has for ARP
SPA.

**c) Conflicting practitioner guidance — flagged, not resolved:**

- **keepalived/VRRP** genuinely needs `MacAddressSpoofing On`: VRRP uses a **virtual MAC**
  (`00-00-5E-00-01-xx`), so the VM emits frames with a source MAC different from its assigned
  one — exactly what MacAddressSpoofing=Off blocks (multiple forum reports, e.g. the MikroTik
  forum). This is a genuine MAC-level mechanism and **does not apply** to Cilium/MetalLB, which
  use no virtual MAC.
- **MetalLB-on-Hyper-V writeups** (third-party, e.g. oneuptime) insist `MacAddressSpoofing On`
  is required, while in the same breath stating MetalLB "answers ARP requests … with the node
  interface's MAC address" (its own MAC). That reasoning is internally inconsistent given (b)
  above — if the node uses its own MAC, MAC-spoofing filtering by itself shouldn't fire. Either
  there is an additional, undocumented default behavior tying a port to its DHCP/ARP-learned IP
  even without an explicit Port ACL, or these writeups are over-generalizing from the VRRP case.
  **No primary Microsoft source was found stating that MacAddressSpoofing=Off filters by source
  IP** — this is an open conflict between documented mechanics and field reports.

**Recommendation:** set `Set-VMNetworkAdapter -MacAddressSpoofing On` on every Talos node vNIC
and add **no** Port ACLs. This is the safe superset: it removes all MAC-level doubt (covers the
VRRP-style case even though Cilium shouldn't need it) and, since no Port ACLs are configured,
there is no IP-level filter either — so both the VIP's gratuitous ARP and VIP-sourced return
traffic should pass. The only cost is losing inter-VM anti-spoofing protection, irrelevant for a
single-user dev tool where all VMs in a cluster are trusted. Related, left at default (Off):
**DhcpGuard** (irrelevant — talosbox's DHCP server runs on the host, not in a guest) and
**RouterGuard** (leave Off in case IPv6 RAs or guest-side BGP/router behavior are ever needed).

**Verified vs inference summary for §5:** (a) and (b) are verified against Microsoft's own
cmdlet/blog documentation. The conclusion that a Cilium-style VIP should work *by default* is a
well-reasoned inference from (a)+(b), not a source stating it outright, and it directly conflicts
with practitioner reports for adjacent (VRRP/MetalLB) scenarios. **This is the single highest-risk
open question in this whole document and should be prototyped first**, exactly as macOS's vmnet
ARP-SPA question was flagged in `docs/research/macos-networking-substrate.md`.

## 6. IP forwarding between Internal switches for inter-cluster routing

**Verified — this works and is the standard technique.** The Windows host routes between two
Internal switches' vEthernet adapters the same way a Linux/macOS host would, by enabling IP
forwarding on the host interfaces:

- `Set-NetIPInterface -InterfaceAlias 'vEthernet (<switch>)' -Forwarding Enabled`
  (<https://learn.microsoft.com/en-us/powershell/module/nettcpip/set-netipinterface>) — per
  interface, the modern targeted approach — or the legacy global
  `netsh interface ipv4 set interface <idx> forwarding=enabled`. Enabling forwarding on each
  cluster's vEthernet adapter turns the host into a router between the `172.30.<n>.0/24`
  subnets. This exact recipe ("Enabling Routing Between Internal Networks") is documented by
  third parties (<https://woshub.com/hyper-v-enable-routing/>,
  <https://igorpuhalo.wordpress.com/2023/02/09/enable-connectivity-between-hyper-v-internal-switches-or-create-ultimate-lab-on-one-pc/>)
  built directly on the Microsoft cmdlet.
- Because each cluster's host vEthernet adapter already holds that subnet's `.1`, the host has
  directly-connected routes to every cluster `/24` automatically; enabling forwarding lets it
  relay guest↔guest and VIP↔VIP traffic across clusters — the direct analogue of
  `net.inet.ip.forwarding` on macOS (SPEC §5, gate G5). No static routes are needed between the
  directly-connected subnets; the supernet NAT (§2) only needs to handle genuinely
  internet-bound traffic.
- A global registry switch (`IPEnableRouter=1` under `Tcpip\Parameters`) is the historical
  whole-host equivalent; the per-interface `Set-NetIPInterface -Forwarding Enabled` is preferred
  since it avoids turning the whole host into a router on the physical NIC too.

**Caveats — inference, to validate:**

- The host firewall (§4) must separately permit the forwarded/inter-subnet traffic — IP-layer
  forwarding and firewall admission are independent controls.
- Cross-cluster **VIP**-to-VIP reachability additionally depends on §5 resolving favorably (the
  announcing node must be allowed to source/receive VIP traffic), so it is gated on both
  forwarding *and* the MacAddressSpoofing/Port-ACL decision.
- All vEthernet adapters are virtual with a uniform MTU (typically 1500), so no fragmentation
  surprises are expected across the forwarded path, but this should be confirmed rather than
  assumed.

---

## Open questions / what needs prototyping

In priority order (highest risk first), mirroring how `docs/SPEC.md` §12 tracks macOS
verification gates:

1. **VIP via gratuitous ARP (§5) — highest risk.** Confirm whether a Talos node can announce and
   receive traffic for a `.200`-style VIP it wasn't assigned, both with `MacAddressSpoofing On`
   and (to resolve the documented-vs-practitioner conflict) with it Off. Test host-reachability
   and cross-cluster reachability separately.
2. **Userland DHCP on the vEthernet adapter (§3).** Confirm the host Go DHCP server receives
   guest `DHCPDISCOVER` broadcasts on the correct per-cluster adapter and can reply, through the
   host firewall (UDP 67 inbound allow), with correct per-subnet scoping across N cluster
   switches.
3. **Supernet NAT correctness (§2).** Confirm a single `172.30.0.0/16` WinNAT gives every
   cluster `/24` simultaneous internet egress, that inter-cluster traffic is routed (not NATed),
   and that WinNAT survives reboots / doesn't collide with a pre-existing Docker Desktop or WSL2
   NAT instance on the same machine (both create their own NAT and could trigger the
   documented "multiple NAT → unknown state" failure).
4. **Firewall/profile fragility (§4).** Confirm the vEthernet adapters land in a workable
   network profile by default, and that explicit DHCP/DNS/ICMP inbound rules survive on a
   hardened/enterprise-managed Windows box (the "Apply Local Firewall Rules = No" failure mode
   documented in `microsoft/Windows-Containers#203`).
5. **Client vs Server SKU support.** `New-NetNat`, Internal switches, `Set-NetIPInterface
   -Forwarding`, and `Set-VMNetworkAdapter` all appear to be available on Windows 10/11 client
   Hyper-V per the cmdlet docs, but this should be confirmed on the exact target SKU; RRAS (the
   multi-NAT fallback in §2) is effectively Server-only and would change the supported-platform
   story if ever needed.
6. **BGP host speaker FIB injection.** talosbox's GoBGP host speaker (SPEC §5) would need to
   inject learned routes into the Windows routing table over the vEthernet adapters — Windows'
   FIB-injection API surface is a different mechanism from macOS's PF_ROUTE and was not covered
   by this research; treat as a separate follow-up ticket if a Windows backend is pursued.
7. **Bootstrap ordering.** Because there's no DHCP until talosbox's own server is running, decide
   between (a) ensuring the DHCP server is up before a node's first boot (preferred — mirrors
   vmnet's always-on DHCP) or (b) falling back to static-IP injection into Talos machine config
   before boot (the path minikube and Docker Desktop's hyperv drivers both ended up on). Keep
   (b) as a documented fallback if broadcast DHCP on the vEthernet adapter proves unreliable.

## Sources cited

- <https://learn.microsoft.com/en-us/windows-server/virtualization/hyper-v/virtual-switch>
- <https://learn.microsoft.com/en-us/windows-server/virtualization/hyper-v/get-started/create-a-virtual-switch-for-hyper-v-virtual-machines>
- <https://learn.microsoft.com/en-us/windows-server/virtualization/hyper-v/setup-nat-network>
- <https://techcommunity.microsoft.com/blog/virtualization/windows-nat-winnat----capabilities-and-limitations/382303>
- <https://learn.microsoft.com/en-us/powershell/module/netnat/new-netnat>
- <https://learn.microsoft.com/en-us/powershell/module/hyper-v/new-vmswitch>
- <https://learn.microsoft.com/en-us/powershell/module/hyper-v/set-vmnetworkadapter>
- <https://learn.microsoft.com/en-us/archive/blogs/wincat/arp-spoofing-prevention-in-windows-server-2012-hyper-v>
- <https://learn.microsoft.com/en-us/windows/security/operating-system-security/network-security/windows-firewall/hyper-v-firewall>
- <https://learn.microsoft.com/en-us/windows/wsl/networking>
- <https://learn.microsoft.com/en-us/powershell/module/nettcpip/set-netipinterface>
- <https://learn.microsoft.com/en-us/powershell/module/nettcpip/new-netipaddress>
- <https://learn.microsoft.com/en-us/windows/win32/api/netcon/nf-netcon-inetsharingconfiguration-enablesharing>
- <https://learn.microsoft.com/en-us/previous-versions/windows/desktop/ics/getting-started-using-the-ics-and-icf-api>
- <https://github.com/microsoft/Windows-Containers/issues/203>
- <https://github.com/kubernetes/minikube/issues/1627>
- <https://minikube.sigs.k8s.io/docs/drivers/hyperv/>
- <https://minikube.sigs.k8s.io/docs/tutorials/static_ip/>
- <https://woshub.com/hyper-v-enable-routing/>
- <https://igorpuhalo.wordpress.com/2023/02/09/enable-connectivity-between-hyper-v-internal-switches-or-create-ultimate-lab-on-one-pc/>
- Third-party, cited for the conflicting practitioner claim only (not primary/authoritative):
  <https://oneuptime.com/blog/post/2026-02-20-metallb-anti-mac-spoofing/view>,
  <https://forum.mikrotik.com/t/vrrp-on-hyper-v-instance-ros-7-15-3-not-working-mac-spoofing-enabled/178332>,
  <https://deepwiki.com/containers/podman/13.2-machine-networking-and-file-sharing>
