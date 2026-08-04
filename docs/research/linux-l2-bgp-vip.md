# Linux L2/BGP path for curated CNI ingress VIPs

Research date: 2026-08-04. Ticket:
[#73 — Research: Linux L2/BGP path for curated CNI ingress VIPs](https://github.com/randax/talos-box/issues/73)
(part of [#71 — Wayfinder: Linux support at full parity](https://github.com/randax/talos-box/issues/71)).

The macOS behaviour being reproduced is [`docs/SPEC.md` §5](../SPEC.md#5-networking): one shared-L2
subnet per cluster (`172.30.<n>.0/24`), host at `.1` as gateway/NAT/DNS/BGP-peer/inter-cluster
router, LB-IPAM pool at `.200–.239` with `.200` the ingress VIP by convention, reachable from the
host either by L2 announcement (default) or BGP (`tbx bgp enable`). In CONTEXT.md terms this is the
**provisioning path**'s "LB extras" leg for both **curated CNIs** — Cilium via native LB-IPAM +
L2/BGP, flannel via talosbox-shipped MetalLB (L2 only).

Sources are Cilium's official docs and its own source tree, MetalLB's official docs and source, the
Linux kernel's `Documentation/networking/` files, FRR's docs, and RFC 5227. Every claim below is
either **verified** (cited) or marked **[inference]** with the reasoning given.

---

## TL;DR / Recommendation

**On Linux the shared-L2 path is strictly better than on macOS and needs no host-side machinery at
all: a Linux bridge with tap-attached VMs makes an ARP-announced VIP reachable from the host with
zero extra work, provided the VIP sits inside the bridge's own subnet — which the existing
`172.30.<n>.0/24` layout already guarantees.** No route, no proxy ARP, no sysctl.

Two consequences for the parity design:

1. **The macOS L2-failover caveat disappears.** SPEC §5 records gate G2: macOS ignores gratuitous
   ARP through vmnet and converges only on its own ARP revalidation, ~40–50 s. Linux does not have
   this problem — the kernel updates an *existing* ARP entry from a GARP regardless of `arp_accept`,
   so failover collapses to the announcer's own election window (Cilium 10–20 s, MetalLB "a few
   seconds"). **BGP mode is therefore not required for correctness on Linux; it stays as a
   teachable alternative and the ECMP/`externalTrafficPolicy: Local` answer.**
2. **The BGP port is small and mostly already done.** `internal/bgp` (GoBGP speaker + `Reconciler`)
   is already platform-neutral and Linux-buildable today. Only two Darwin-gated files need Linux
   twins: `fib_darwin.go` (→ netlink `RTM_NEWROUTE`/`RTM_DELROUTE` instead of `/sbin/route`) and
   `bgp_darwin.go`'s enable/disable wiring (currently stubbed out by `bgp_stub.go`).

| macOS (vmnet) | Linux (bridge + tap) |
|---|---|
| `VMNET_SHARED_MODE` interface, host at `172.30.<n>.1` | `br-tbx<n>` bridge + one `tapN` per node, host `ip addr add 172.30.<n>.1/24 dev br-tbx<n>` |
| ARP for never-assigned addresses passes vmnet's anti-spoofing (empirically verified, SPEC §5) | No anti-spoofing to defeat; the bridge is protocol-independent L2 forwarding |
| Host ignores GARP → L2 failover ~40–50 s (gate G2) | GARP updates an existing ARP entry unconditionally → failover = election window only |
| `routeFIB` shells out to `/sbin/route -n add -host` | netlink route add/del (`CAP_NET_ADMIN`) |
| GoBGP binds `172.30.<n>.1:179` as root | Same, needs `CAP_NET_BIND_SERVICE` (+ `CAP_NET_ADMIN` for the FIB) |
| BGP mode is the *fast-failover* path | BGP mode is the *ECMP / ETP=Local / teaching-contrast* path |

---

## 1. Does the VIP become reachable from the host without extra work?

**Yes, when the VIP is inside the bridge subnet.** The kernel's bridge documentation describes the
bridge as forwarding "transparently… between multiple network interfaces" to "form one larger
(logical) Ethernet network", and that "the bridge sees all frames, but it *uses* only L2
headers/information. As such, the bridging functionality is protocol independent"
([bridge.rst](https://www.kernel.org/doc/html/latest/networking/bridge.html)). ARP (EtherType
0x0806) is therefore flooded and forwarded like any other frame **[inference — entailed by the
quoted text plus standard 802.1D behaviour; the kernel docs never name ARP]**.

**[inference]** The host's own `br-tbx<n>` netdevice participates as one more port on the bridge:
frames to its MAC or to broadcast go up the host stack, and packets the host sends `dev br-tbx<n>`
are switched out the right port. This is not spelled out in `bridge.rst`, but it is why
`ip addr add … dev br0` works at all.

Concrete flow for `172.30.0.200`:

1. Host has `172.30.0.1/24 dev br-tbx0`; the VIP is in the connected route.
2. Host broadcasts an ARP request out `br-tbx0`; the bridge floods it to every `tapN`.
3. The node holding the lease answers with its own NIC MAC (see §2/§3 for how).
4. Host caches it. Done.

### Sysctls — which actually matter

Quoted from [`ip-sysctl.rst`](https://www.kernel.org/doc/html/latest/networking/ip-sysctl.html):

- **`arp_accept` (default 0) — the one that matters, and the default is correct.** "Define behavior
  for accepting gratuitous ARP (garp) frames from devices that are **not already present** in the
  ARP table: 0 — don't create new entries…" and, critically, "**If the ARP table already contains
  the IP address of the gratuitous arp frame, the arp table will be updated regardless if this
  setting is on or off.**" Also: "Both replies and requests type gratuitous arp will trigger the ARP
  table to be updated." Since tbx's host learns the VIP by ARPing for it normally, the entry always
  exists by the time a failover GARP arrives. **This is the exact behaviour macOS lacks.**
- **`arp_ignore` / `arp_filter` — irrelevant here.** Both govern how the host answers ARP for *its
  own* addresses ("IP addresses are owned by the complete host on Linux, not by particular
  interfaces"). The VIP is never configured on the host **[inference from the quoted semantics]**.
- **`rp_filter`** — "Default value is 0. Note that some distributions enable it in startup scripts",
  and "the max value from `conf/{all,interface}/rp_filter` is used". Single-bridge traffic is
  symmetric so this is a non-issue **[inference]**; it can bite a multi-homed host (corporate NIC +
  VPN) where strict mode silently drops the reply path. Set to `2` (loose) or fix routing.
- **`arp_announce`** — affects only the sender IP the host picks in its own ARP requests; default is
  fine.

### `br_netfilter` — the one real Linux-specific trap

From `bridge.rst`'s Netfilter section: "The bridge netfilter module is a legacy feature… Its use is
discouraged." "The `br_netfilter` module intercepts packets entering the bridge… and then
**pretends that these packets are being routed, not bridged**. `br_netfilter` then calls the ip and
ipv6 netfilter hooks from the bridge layer, i.e. ip(6)tables rulesets will also see these packets."
"For pure link layer filtering, this module isn't needed."

**[inference from that mechanism]** On a host where Docker (or a hardened distro) has loaded
`br_netfilter` and set `net.bridge.bridge-nf-call-iptables=1` **with a default-DROP `FORWARD`
policy** — Docker sets exactly this — bridged IPv4 traffic across `br-tbx<n>` gets dropped. ARP is
*not* affected, because the module scopes itself to "ipv4 and ipv6 packets" and ARP goes to the
separate `bridge-nf-call-arptables` hook. That produces a highly diagnostic signature:

> **`ip neigh` shows the VIP resolved to the node's MAC, but TCP to the VIP hangs.**

This is a `tbx doctor` check on Linux, and the direct parallel to the macOS corporate-lockdown
findings in [`docs/corporate-lockdown-analysis.md`](../corporate-lockdown-analysis.md). The `physdev`
iptables match is the escape hatch for a targeted `ACCEPT`.

### STP

`bridge.rst` documents the port state machine; a newly enslaved `tapN` goes Blocking → Listening →
Learning → Forwarding. **[inference]** With STP on and default timers that is ~30 s of silence after
each VM start. tbx should create its bridges with STP off (`ip link add … type bridge stp_state 0`)
— there are no loops in a per-cluster tap bridge. Symptom if missed: "everything works, but only
after ~30 s".

---

## 2. Cilium L2 announcements

Sources: [docs](https://docs.cilium.io/en/stable/network/l2-announcements/) ·
[doc source](https://github.com/cilium/cilium/blob/main/Documentation/network/l2-announcements.rst) ·
[`bpf/lib/l2_responder.h`](https://github.com/cilium/cilium/blob/main/bpf/lib/l2_responder.h) ·
[`pkg/datapath/gneigh/gneigh.go`](https://github.com/cilium/cilium/blob/main/pkg/datapath/gneigh/gneigh.go) ·
[`pkg/datapath/linux/devices_controller.go`](https://github.com/cilium/cilium/blob/main/pkg/datapath/linux/devices_controller.go) ·
[LB-IPAM](https://docs.cilium.io/en/stable/network/lb-ipam/)

**Mechanism.** Two distinct behaviours:

1. **Steady state — ARP *replies*, in eBPF.** `bpf_host.c` intercepts `ETH_P_ARP` and calls
   `handle_l2_announcement()`, which looks up `{ip4, ifindex}` in the pinned map
   `/sys/fs/bpf/tc/globals/cilium_l2_responder_v4` and answers with the interface's own MAC. Two
   load-bearing details in `l2_responder.h`: the key uses `ctx->ingress_ifindex`, so **it answers
   only on the exact interface programmed**; and there is an agent-liveness gate — if the agent has
   been silent longer than `l2_announcements_max_liveness`, the datapath stops answering.
2. **Failover only — gratuitous ARP.** The docs say Cilium "will send out a gratuitous ARP reply
   over all of the configured interfaces", but `gneigh.go` actually sends
   `arp.NewPacket(arp.OperationRequest, …)` to `ethernet.Broadcast` with sender IP == target IP —
   an **ARP Request**, i.e. the RFC 5227 §1.1 "ARP Announcement" form. Docs and code disagree on the
   wording; the request form is the more widely honoured one, and Linux's `arp_accept` text confirms
   "both replies and requests type gratuitous arp will trigger the ARP table to be updated".

**Prerequisites** (all confirmed, and all already satisfied by
[`docs/walkthrough-cilium-ingress.md`](../walkthrough-cilium-ingress.md) except the last):

- `kubeProxyReplacement=true` is **mandatory**, with `k8sServiceHost`/`k8sServicePort` (Talos
  KubePrism `localhost:7445`).
- `l2announcements.enabled=true`.
- Announced devices must be in the `--devices` / `devices` Helm option if that option is set
  explicitly. tbx does not set it, so auto-detection applies.
- **`k8sClientRateLimit.qps`/`.burst` must be raised** — docs give `QPS = #services *
  (1 / leaseRenewDeadline)` and say the 5 QPS / 10 burst default "is quickly reached". Symptom:
  `Waited for 1.39s due to client-side throttling`. **tbx's Helm values in the walkthrough do not
  set these** — worth adding for the curated Cilium path, cheap insurance for a workshop cluster.

**Policy CRD.** `CiliumL2AnnouncementPolicy` (`cilium.io/v2alpha1`, matching
`internal/manifests/manifests.go`): `serviceSelector` (unset = all), `nodeSelector`, `interfaces`
(Go regexes, unset = all), and `externalIPs`/`loadBalancerIPs` which **both default `false`**. tbx's
rendered policy sets `loadBalancerIPs: true` with empty selectors — correct. Services must have
`loadBalancerClass` unset or `io.cilium/l2-announcer`.

**Leader election.** One Kubernetes `Lease` per service, `cilium-l2announce-<ns>-<svc>`,
first-come-first-serve (docs: "might cause asymmetric traffic distribution"). Documented failover
window: `leaseDuration ± leaseRenewDeadline`, "so with the default values, failover occurs between
10s and 20s". RBAC gotcha: the `cilium` ClusterRole needs `coordination.k8s.io/leases`.

**Documented limitations:**

1. One node receives all ARP for a given IP — "no load balancing can happen before traffic hits the
   cluster", and nodes may be asymmetrically loaded.
2. **Incompatible with `externalTrafficPolicy: Local`** — "it may cause service IPs to be announced
   on nodes without pods causing traffic drops". Use `Cluster`. This is the single most likely
   silent-drop cause and appears in both Limitations and Troubleshooting.
3. Clients that reject GARP see longer downtime than the lease config implies.
4. L2 *Pod* Announcements (a separate feature) has no IPv6.

**Not confirmed:** any documented incompatibility between L2 announcements and `bpf.masquerade`. No
such statement exists in Cilium's docs. What *is* real and adjacent:
[cilium#25303](https://github.com/cilium/cilium/issues/25303) reports BPF masquerading broken when
the masquerade device is a **Linux bridge**. Other reported-but-not-documented ARP bugs worth
knowing during Linux bring-up: [#38223](https://github.com/cilium/cilium/issues/38223),
[#37959](https://github.com/cilium/cilium/issues/37959) (VLAN trunk: sends GARP but won't answer
ARP), [#41641](https://github.com/cilium/cilium/issues/41641) (interface with no IPv4 address),
[#26586](https://github.com/cilium/cilium/issues/26586) (stops working after a while).

### Bridge caveat that does *not* bite tbx (but is worth recording)

`devices_controller.go`'s `isSelectedDevice()` refuses to auto-select bridge-like devices and
anything enslaved to one:

```go
// Ignore bridge and bonding children devices, but allow VRF device children.
…  return false, fmt.Sprintf("bridged or bonded to ifindex %d", d.MasterIndex)
…
case "bridge", "openvswitch":
    // Skip bridge devices as they're very unlikely to be used for K8s
    return false, "bridge-like device, use --devices to override"
```

This is about devices **inside the Talos node**, not the host bridge. tbx nodes have a plain
virtio NIC (`eth0`-ish) with the default route, so auto-detection selects it. It would only matter
if a node ever bridged internally.

**LB-IPAM** is advertisement-agnostic: it only writes the allocated IP into
`.status.loadBalancer.ingress` from a `CiliumLoadBalancerIPPool`, works with either L2 or BGP, is
on by default in the operator, and does not itself need KPR.

---

## 3. MetalLB L2 mode (the flannel path)

Sources: [Layer 2 concepts](https://metallb.io/concepts/layer2/) ·
[advanced L2 config](https://metallb.io/configuration/_advanced_l2_configuration/) ·
[installation](https://metallb.io/installation/) · [troubleshooting](https://metallb.io/troubleshooting/) ·
[`internal/layer2/arp.go`](https://github.com/metallb/metallb/blob/main/internal/layer2/arp.go)

**Mechanism.** A **userspace** raw-socket responder, not eBPF. `processRequest()` drops ARP replies
and frames not addressed to broadcast or the local MAC, then replies with the receiving interface's
own MAC. `Gratuitous()` sends **both** forms —
`for _, op := range []arp.Operation{arp.OperationRequest, arp.OperationReply}` — strictly more
compatible than Cilium's request-only GARP. IPv4 via ARP, IPv6 via NDP.

**Election is not memberlist-based**, a common misconception the docs correct explicitly: each
speaker independently computes "a sorted list of a hash of 'node+VIP' elements and announces the
service if it is the first item of the list. This removes the need of having to keep memory of which
speaker is in charge." [memberlist](https://github.com/hashicorp/memberlist) supplies only
**liveness** — "the failed node is detected using memberlist, at which point new nodes take over".
There is a documented **"brain split behaviour"** where speakers disagreeing on the active set
produce multiple or zero announcers.

**Config.** `IPAddressPool` **plus a mandatory `L2Advertisement`** (post-0.13 MetalLB idles without
one). `L2Advertisement` takes `ipAddressPools`, `nodeSelectors`, `serviceSelectors`, `interfaces`.
Warning worth internalising: "**The interface selector won't affect how MetalLB is choosing the
leader for a given L2 IP**" — if the elected node lacks the selected interface the service is simply
not announced. Multiple advertisements over one pool take the **union** of interfaces.

**Limitations.** "In layer2 mode a single leader-elected node receives all traffic for a service IP…
This is a fundamental limitation of using ARP and NDP to steer traffic", and "layer 2 does not
implement a load balancer. Rather, it implements a failover mechanism." Failover is "within a few
seconds" on modern client stacks; the docs advise keeping the old leader up for a couple of minutes
on *planned* failover. Traffic is spread to pods by **kube-proxy** afterwards, so MetalLB L2 does
**not** want kube-proxy replacement — the opposite posture from Cilium, and consistent with tbx
pairing MetalLB only with flannel. If kube-proxy runs in IPVS mode, `ipvs.strictARP: true` is
required.

**Documented failure signature relevant here:** multiple MACs answering one IP means multiple
speakers announcing, an IP conflict, or **CNI interference with ARP responses**.

### Side by side

| | Cilium L2 announcements | MetalLB L2 |
|---|---|---|
| Responder | eBPF at tc ingress, per-`{ip, ifindex}` map | userspace raw socket per interface |
| Requires kube-proxy replacement | **yes** | no (relies on kube-proxy) |
| Election | k8s Leases, first-come-first-serve | stateless hash(node+VIP); memberlist = liveness only |
| Failover window | `leaseDuration ± leaseRenewDeadline` → 10–20 s default | "within a few seconds" |
| GARP form | ARP Request only (per source) | ARP Request **and** Reply |
| `externalTrafficPolicy: Local` | documented **incompatible** | supported, and affects election |

Neither behaves differently on a Linux bridge than on any other shared L2 segment — both just need
their ARP to reach the host, which §1 establishes it does.

---

## 4. What replaces `bgp_darwin.go` / `fib_darwin.go`

The split in the tree today is already close to right:

| File | Status for Linux |
|---|---|
| `internal/bgp/` (`speaker.go`, `reconcile.go`) | **Platform-neutral already.** GoBGP + the `FIB` interface + `Reconciler`. No build tags. Compiles and unit-tests on Linux as-is. |
| `internal/helper/bgp_type.go` | Platform-neutral (`bgpSpeaker` interface). No change. |
| `internal/helper/fib_darwin.go` | **Needs a `fib_linux.go` twin.** `routeFIB` execs `/sbin/route -n add -host <ip> <gw>` and string-matches "File exists"/"not in table". |
| `internal/helper/bgp_darwin.go` | **Needs a `bgp_linux.go` twin** — the `enableBGP`/`disableBGP` wiring is pure Go over `bgp.StartSpeaker` plus the `172.30.<n>.1` / `172.30.<n>.0/24` derivation. Essentially a build-tag change. |
| `internal/helper/bgp_stub.go` (`//go:build !darwin`) | Must narrow to `!darwin && !linux`, or be deleted once Linux is supported. Today it returns `errBGPUnsupported = "BGP is only available on macOS"`. |

**Recommendation for `fib_linux.go`: netlink, not `exec("ip route")`.** Route add/delete is
`RTM_NEWROUTE`/`RTM_DELROUTE` over `AF_NETLINK`, which gives typed errors (`EEXIST`, `ESRCH`)
instead of the brittle stdout string-matching `fib_darwin.go` is forced into, and drops a runtime
dependency on iproute2. The `FIB` interface (`AddHostRoute(prefix, nexthop)` /
`DeleteHostRoute(prefix)`) is already the right shape; `AddHostRoute` maps to a `/32` route with
`Gw` set and `Dst` the VIP, `NLM_F_REPLACE` giving idempotency for free.

**Privileges.** Under the split-privilege model #71 mandates (systemd service with capabilities, not
setuid), the helper needs:

- `CAP_NET_ADMIN` — netlink route manipulation, bridge and tap creation.
- `CAP_NET_BIND_SERVICE` — GoBGP binds `172.30.<n>.1:179` (`speaker.go`'s `bgpPort = 179`).

**[inference]** Both are grantable via `AmbientCapabilities=`/`CapabilityBoundingSet=` in a systemd
unit, so no setuid binary and no root is needed for the BGP path — a genuine improvement over the
macOS helper, which needs full root for `/sbin/route`.

**Cilium's BGP side.** Per the
[BGP Control Plane docs](https://docs.cilium.io/en/stable/network/bgp-control-plane/), Cilium is
**advertise-only**: "Because BGP Control Plane does not program the datapath, do not use it to
establish reachability within the cluster." It announces; the host must be the speaker that installs
routes. With FRR that is `zebra`, "a kernel routing table manager… responsible for coordinating
routing decisions and talking to the dataplane"
([FRR docs](https://docs.frrouting.org/en/latest/zebra.html)) — but tbx does not need FRR, because
`internal/bgp` already *is* the host speaker and `Reconciler` already *is* the FIB manager.

Two things to flag on the cluster-side manifest, both cross-platform rather than Linux-specific:

- `internal/manifests/manifests.go` renders the **legacy** `CiliumBGPPeeringPolicy`
  (`cilium.io/v2alpha1`). Current Cilium supersedes it with `CiliumBGPClusterConfig` +
  `CiliumBGPPeerConfig` + `CiliumBGPAdvertisement` (v2). The VIP advertisement in the new API is a
  `CiliumBGPAdvertisement` of `advertisementType: Service` with
  `service.addresses: [LoadBalancerIP]`, selected by a **label selector** on the peer config —
  with the explicit warning "**Without matching advertisements, no prefix will be advertised to the
  peer.**"
- The v2 API also has `autoDiscovery` with `DefaultGateway` mode, which would derive the peer
  address from the node's default route instead of tbx hard-coding `172.30.<n>.1` — attractive, but
  documented as unusable for multiple sessions per address family in multi-homing setups.

**Is BGP needed at all on Linux?** **No, for reachability. [inference, strongly grounded]** BGP's
only job here is to put a `/32` for the VIP in the host FIB with the right next-hop; on a shared L2
bridge, ARP does that with zero configuration. Cilium's own docs scope L2 announcements to
deployments "within networks **without BGP based routing** such as office or campus networks" — which
is exactly the tbx laptop. BGP on Linux earns its keep for four other reasons:

1. **ECMP / real load balancing** — both L2 modes are documented single-node funnels.
2. **`externalTrafficPolicy: Local`** — Cilium BGP "keeps track of the endpoints for the service on
   the local node and stops advertisement when there's no local endpoint", where L2 announcements is
   documented incompatible.
3. **VIP outside the bridge subnet**, or a routed/NAT'd guest network with no shared L2 — see below.
4. **Teaching the contrast**, which SPEC §5 already names as the point.

### When plain L2 is not enough

- **VIP outside the bridge subnet, still shared L2** — one host route suffices:
  `ip route add <VIP>/32 dev br-tbx<n>` (scope link) makes the host ARP for it directly. **[inference
  from standard routing semantics]** No BGP. Not tbx's situation: `.200–.239` is inside
  `172.30.<n>.0/24`.
- **Routed/NAT'd guest network** (the libvirt `forward mode='nat'` shape) — the guest net is a
  separate L3 segment behind the host; ARP announcements are invisible upstream and inbound needs
  DNAT. **[inference]** #71 already puts rootless/NAT-only networking (passt/slirp4netns) out of
  scope for exactly this reason.
- **Clients not on the bridge** (another VLAN, another host) — L2 announcements is scoped to one
  broadcast domain by its own docs; BGP is the answer.
- **macOS-style userspace networking** — no shared broadcast domain with the host at all;
  port-forwarding territory. Not applicable to the Linux design.

---

## 5. Verification ladder for Linux bring-up

Host side:

```sh
ip -d link show br-tbx0                 # stp_state should be 0
bridge link                             # tapN ports present and forwarding
sysctl net.bridge.bridge-nf-call-iptables   # 0 preferred; if 1, check FORWARD policy
iptables -S FORWARD | head              # a default DROP here is the classic silent failure
arping -I br-tbx0 172.30.0.200
ip neigh show 172.30.0.200              # should show the announcing node's MAC
curl -sv http://172.30.0.200/           # if ARP resolves but this hangs → br_netfilter/FORWARD
```

Cluster side (from Cilium's own troubleshooting section):

```sh
cilium-dbg config --all | grep -E 'EnableL2Announcements|KubeProxyReplacement'
kubectl -n kube-system get lease | grep cilium-l2announce
cilium-dbg shell -- db/show devices     # target device must show Selected=true
cilium-dbg shell -- db/show l2-announce # {IP, NetworkInterface} present
bpftool map dump pinned /sys/fs/bpf/tc/globals/cilium_l2_responder_v4  # responses_sent > 0
kubectl get svc -A -o wide              # externalTrafficPolicy must be Cluster
```

`responses_sent > 0` is the cleanest way to split "the ARP request never arrived" from "the return
path is broken" — worth wiring into `tbx doctor` on Linux.

---

## 6. Open items for the map

- **Add `k8sClientRateLimit.qps`/`.burst`** to the curated Cilium Helm values (both platforms) — the
  documented default is "quickly reached" with L2 announcements enabled.
- **Migrate `BGPPolicy` off `CiliumBGPPeeringPolicy`** to the v2 `CiliumBGPClusterConfig` family
  (cross-platform; the existing comment in `manifests.go` already anticipates an API bump).
- **Decide whether Linux keeps `tbx bgp enable` as opt-in at all** given L2 failover is now fast.
  Recommendation: keep it — parity is the stated destination and the teaching contrast is the point
  — but the docs' framing must change from "BGP is the fast-failover path" (a macOS-specific truth)
  to "BGP is the ECMP / ETP=Local / routed-network path" on Linux.
- **`tbx doctor` on Linux**: `br_netfilter` + `FORWARD` policy, STP state, `rp_filter` on a
  multi-homed/VPN host, and a preflight bind of `172.30.<n>.1:179`.

---

## Sources

- [Cilium — L2 Announcements / L2 Aware LB (Beta)](https://docs.cilium.io/en/stable/network/l2-announcements/) ·
  [doc source](https://github.com/cilium/cilium/blob/main/Documentation/network/l2-announcements.rst)
- [Cilium — LB-IPAM](https://docs.cilium.io/en/stable/network/lb-ipam/)
- [Cilium — BGP Control Plane](https://docs.cilium.io/en/stable/network/bgp-control-plane/) ·
  [configuration](https://github.com/cilium/cilium/blob/main/Documentation/network/bgp-control-plane/bgp-control-plane-configuration.rst)
- Cilium source: [`bpf/lib/l2_responder.h`](https://github.com/cilium/cilium/blob/main/bpf/lib/l2_responder.h) ·
  [`bpf/bpf_host.c`](https://github.com/cilium/cilium/blob/main/bpf/bpf_host.c) ·
  [`pkg/datapath/gneigh/gneigh.go`](https://github.com/cilium/cilium/blob/main/pkg/datapath/gneigh/gneigh.go) ·
  [`pkg/datapath/l2responder/l2responder.go`](https://github.com/cilium/cilium/blob/main/pkg/datapath/l2responder/l2responder.go) ·
  [`pkg/datapath/linux/devices_controller.go`](https://github.com/cilium/cilium/blob/main/pkg/datapath/linux/devices_controller.go)
- Cilium issues (reported, not documented): [#25303](https://github.com/cilium/cilium/issues/25303),
  [#26586](https://github.com/cilium/cilium/issues/26586),
  [#37959](https://github.com/cilium/cilium/issues/37959),
  [#38223](https://github.com/cilium/cilium/issues/38223),
  [#41641](https://github.com/cilium/cilium/issues/41641)
- [MetalLB — Layer 2 mode](https://metallb.io/concepts/layer2/) ·
  [Advanced L2 configuration](https://metallb.io/configuration/_advanced_l2_configuration/) ·
  [Installation](https://metallb.io/installation/) · [Troubleshooting](https://metallb.io/troubleshooting/) ·
  [`internal/layer2/arp.go`](https://github.com/metallb/metallb/blob/main/internal/layer2/arp.go)
- [Linux kernel — `Documentation/networking/bridge.rst`](https://www.kernel.org/doc/html/latest/networking/bridge.html)
- [Linux kernel — `Documentation/networking/ip-sysctl.rst`](https://www.kernel.org/doc/html/latest/networking/ip-sysctl.html)
- [RFC 5227 — IPv4 Address Conflict Detection](https://www.rfc-editor.org/rfc/rfc5227.html)
- [FRRouting — Zebra](https://docs.frrouting.org/en/latest/zebra.html)
- [Talos/Sidero — Deploying Cilium CNI](https://docs.siderolabs.com/kubernetes-guides/cni/deploying-cilium)
