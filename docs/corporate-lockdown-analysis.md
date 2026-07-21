# Corporate lockdown analysis — what breaks talos-box on managed Macs

talos-box's target user runs it on a **corporate MacBook**: MDM-enrolled, EDR-monitored,
usually behind a VPN client and sometimes TLS interception. This analysis maps every host
mechanism talos-box relies on against the corporate controls that can break it, ranked by
likelihood. Grounded in a code sweep (file:line refs below), live testing on a machine
running GlobalProtect 6.2.8 + four other tunnel products, and an independent model review.

Mechanism inventory (from code):

- Outbound HTTPS from the **host** `tbxd` process only: `factory.talos.dev`
  (`internal/imagecache/cache.go:17`, default Go client, system roots, no explicit proxy
  config, no timeout) and registry upstreams + their token/CDN endpoints
  (`internal/mirror/mirror.go:45`, 5-min timeout, system roots).
- Guests reach registries only via plain-HTTP host mirrors on `172.30.<n>.1:5055-5058`
  (`internal/mirror/manager.go:69`).
- Embedded authoritative DNS on `127.0.0.1:5399`, wired via `/etc/resolver/k8s.test`
  (`internal/dns/server.go:10-13`, `internal/helper/server.go:19`).
- vmnet **shared/NAT mode** with hardcoded `172.30.<n>.0/24` subnets, no collision
  detection against existing routes (`internal/helper/vmnet_darwin.go:176-184`,
  `internal/cluster/cluster.go:97-111`).
- Root launchd helper `dev.talosbox.helper` doing vmnet attach, resolver install,
  `sysctl net.inet.ip.forwarding=1`, `/sbin/route` injection (`cmd/tbx/system.go:14-17`,
  `internal/helper/server.go`). Forwarding + resolver are set once at tbxd start and
  **never re-asserted** (`cmd/tbxd/main.go:86-99`).
- `tbx`/`tbxd` are **ad-hoc codesigned** with `com.apple.security.virtualization`;
  `tbx-helper` is unsigned (`Makefile:16-18`, `build/entitlements.plist`).
- No NTP config is generated; guests use Talos defaults (`time.cloudflare.com`, UDP 123).

## Risk matrix

Ranked by (likelihood on a typical corporate Mac) × (impact). "Doctor?" = whether
`tbx doctor` currently catches it.

### 1. TLS-inspecting VPN / ZTNA agents killing guest TLS — **mitigated by design, verify it stays that way**

- **Products**: Palo Alto GlobalProtect, Zscaler Client Connector, Netskope Client,
  Cisco Secure Client. Enforcement at the NE/utun layer RSTs TLS flows they cannot
  attribute to a host process — which is exactly what NAT'd guest traffic looks like.
- **Breaks**: any in-guest TLS: direct registry pulls, `talosctl image pull`, in-cluster
  controllers fetching over HTTPS (cert-manager ACME, ArgoCD, Helm charts from guests).
- **Status**: the registry-mirror architecture already routes the critical path (image
  pulls) through the host process; empirically confirmed working with GlobalProtect's
  tunnel up. But **anything a workshop deploys that dials TLS from a pod still breaks**,
  and no mirror exists for e.g. `gcr.io`, `mcr.microsoft.com`, PyPI, or ACME endpoints.
- **Symptom**: `connection reset by peer` from pods/nodes for HTTPS only; HTTP/DNS/NTP fine.
- **Doctor?** No. Doctor checks nothing guest-side.
- **Recommendation**: doctor check that dials TLS from a guest-attributed context and
  reports "guest TLS is filtered — use mirrors"; docs section for workshop authors;
  possibly a generic host-side HTTPS forward proxy as an opt-in escape hatch.

### 2. VPN routing table capture of `172.30.0.0/16` — **high likelihood, no detection**

- **Products**: any full-tunnel VPN, or split tunnels including RFC1918 space.
  `172.30.x.x` sits inside `172.16.0.0/12`, one of the three private blocks corporations
  route over VPN wholesale. Observed live: tunnel owning the default route — the
  connected `bridge100` /24 won by prefix length. But a VPN pushing `172.30.0.0/16` or
  exact /24s (common when the corporate LAN actually uses 172.30) wins or ties, and
  NE-based clients (GlobalProtect, AnyConnect) can also enforce *below* the routing
  table, capturing traffic regardless of routes.
- **Distinct sub-case — "no local LAN" policy**: GlobalProtect ("No direct access to
  local network"), Cisco Secure Client (local-LAN access disabled), and Zscaler's client
  firewall can drop *all* local-adapter traffic at the NE layer before routing is even
  consulted. Under that policy every vmnet path dies — host→node, host→VIP, and guest
  NAT egress — while `tbx doctor`'s four checks (helper ping, loopback DNS, sysctl,
  resolver file) all still pass. Everything starts working the moment the VPN
  disconnects, which is the diagnostic tell.
- **Breaks**: all host→cluster traffic: talosctl, kubectl, the browser URL, DNS answers
  pointing at unreachable VIPs. Worst case is silent blackholing mid-workshop when the
  VPN reconnects and re-pushes routes.
- **Symptom**: timeouts to `172.30.x.x` while `tbx status` shows nodes healthy (tbxd
  reaches guests via vmnet internally); or works until VPN reconnect, then dies.
- **Doctor?** No. Nothing inspects the routing table.
- **Recommendation**: doctor should `route -n get` a sample of each cluster's subnet and
  flag when the egress interface isn't the vmnet bridge; cluster create should warn when
  an existing route/interface already covers `172.30.<n>.0/24` and pick the next free
  index (the allocator only avoids talos-box's own clusters,
  `internal/cluster/cluster.go:97-111`). Consider a configurable base CIDR.

### 3. MDM blocking the privileged install path — **high likelihood at strict orgs**

- **Products**: Jamf/Kandji/Intune restrictions: no admin rights (can't `sudo tbx system
  install`), managed `sudo`, blocked `launchctl bootstrap system`, MDM policies
  restricting third-party LaunchDaemons, or binary allowlisting (Santa, Jamf Protect)
  refusing **ad-hoc-signed** binaries outright.
- **Breaks**: everything — no helper means no vmnet, no resolver, no forwarding.
  Ad-hoc signing (`Makefile:16-18`) is the biggest single exposure: Santa in lockdown
  mode and many EDR policies block unsigned/ad-hoc code by default; the unsigned
  `tbx-helper` is worse (it's the thing that runs as root).
- **Symptom**: install fails at sudo/launchctl; or the helper binary is killed/blocked
  on first launch; Gatekeeper prompts if distributed without notarization.
- **Doctor?** Partially — helper ping fails, but with no explanation of *why*.
- **Recommendation**: ship Developer-ID-signed + notarized binaries (SPEC §11 already
  plans this — it's a hard requirement for managed Macs, not a nicety); sign the helper
  too; doctor should distinguish "helper never installed" / "plist present but process
  not running (likely blocked by endpoint security)" and say which vendor profile to
  request from IT.

### 4. DNS filtering agents vs `/etc/resolver/k8s.test` — **medium-high likelihood**

- **Products**: Cisco Umbrella, Zscaler/Netskope DNS control, DNSFilter; also browser
  DoH (Chrome "Secure DNS", Firefox DoH) which bypasses the system resolver entirely.
- **Breaks**: name resolution of `*.k8s.test`. Umbrella-style agents intercept port-53
  and DoH traffic at the NE layer and answer from corporate resolvers → NXDOMAIN for
  `k8s.test` (it's not a real TLD, by design). The scoped resolver on **port 5399**
  helps: interceptors that only grab :53 miss it. But apps that don't use the macOS
  scoped-resolver API (`dig`, `nslookup`, anything doing raw DNS, browsers with DoH
  enabled) never consult it — the user's browser is the highest-visibility victim:
  **curl works, Chrome says site can't be reached**.
- **Doctor?** Partially: doctor probes the DNS server and stats the resolver file, but
  resolves nothing through the *system* path, so interception passes unnoticed.
- **Recommendation**: doctor should resolve a `*.k8s.test` name via the system resolver
  (`dscacheutil -q host` / `res_query`), not just probe the socket; docs should tell
  users to disable browser Secure DNS for `.test` or use the IP; status hints could
  print the VIP alongside the URL.

### 5. TLS interception (MITM proxy) of host egress — **medium likelihood, currently fatal when present without keychain trust**

- **Products**: Zscaler/Netskope/Palo Alto SSL inspection, Blue Coat; corporate PAC/
  explicit proxies.
- **Breaks**: host-side `factory.talos.dev` downloads and mirror upstream pulls. Two
  distinct failure modes: (a) interception with the corporate CA properly MDM-installed
  in the System keychain — Go uses system roots, so this **works**; (b) proxy-only
  egress (direct 443 blocked) — Go honors `HTTPS_PROXY` env only if set in the shell
  that spawned `tbxd` (`cmd/tbx/client.go:109` inherits env), and macOS system proxy /
  PAC settings are **ignored** by Go's default transport. A user whose corp requires a
  PAC file gets hangs — and `imagecache` has **no HTTP timeout at all**
  (`internal/imagecache/cache.go:39`), so `cluster create` hangs indefinitely rather
  than failing.
- **Symptom**: image download hangs forever or x509 errors; mirror 502s on first pull
  of an uncached image.
- **Allowlist trap**: even when IT allowlists the four registry hostnames, pulls still
  fail — the mirror follows `WWW-Authenticate` token realms and CDN blob redirects
  (`internal/mirror/mirror.go:252-265`, redirect-following client), so the real
  hostname set includes `auth.docker.io`, `production.cloudflare.docker.com`,
  `pkg-containers.githubusercontent.com`, GCS/Fastly blob hosts, etc.
- **Doctor?** No egress check at all.
- **Recommendation**: add timeouts to the factory client; doctor check that HEADs
  `factory.talos.dev` and one registry, reporting x509-vs-timeout-vs-reset distinctly;
  a doctor mode that performs one tiny real pull per upstream and *records the full
  hostname chain* (DNS → token realm → redirect target) to emit an IT-ready allowlist;
  document that proxy env must be present in the shell that first runs `tbx` (or read
  macOS proxy settings via `scutil --proxy`); surface mirror upstream errors in
  `tbx status` hints.

### 6. Endpoint security resetting host state — **medium likelihood, insidious**

- **Products**: CIS-benchmark compliance scripts (Jamf remediation), EDR hardening.
  `net.inet.ip.forwarding=0` is a literal CIS macOS benchmark item; compliance agents
  re-assert it on schedule. Some also prune unknown `/etc/resolver/` entries and
  unknown LaunchDaemons.
- **Breaks**: forwarding off → guests lose NAT egress (registry mirror still reachable
  — it's on the gateway IP — but external HTTP, NTP die); resolver removed → URLs stop
  resolving. talos-box sets both **once** at tbxd startup and never re-asserts
  (`cmd/tbxd/main.go:86-99`).
- **Symptom**: cluster that worked yesterday has no pod egress today; DNS gone after
  "IT pushed something".
- **Doctor?** Yes — this is the one category doctor catches well (forwarding + resolver
  checks). But it only diagnoses; nothing repairs.
- **Recommendation**: tbxd should periodically re-assert forwarding + resolver (it
  already owns a root-helper channel); `tbx doctor --fix` would be a natural addition.

### 7. Sharing the vmnet/NAT substrate with other virtualization — **medium likelihood on developer Macs**

- **Products**: Docker Desktop (`com.docker.vmnetd`), OrbStack, UTM, Tart, colima —
  observed three of these coexisting on the test machine. All use vmnet shared mode;
  macOS `bootpd` serves DHCP for all of them. Conflicts: overlapping subnet choices
  (OrbStack/Docker default to 192.168.x but are configurable; some tools pick 172.x),
  `bootpd` state confusion, and InternetSharing/pf NAT rules interacting.
- **Breaks**: DHCP leases not appearing (talos-box reads `/var/db/dhcpd_leases`,
  `internal/vm/lease.go`), or cross-tool subnet collision (same class as #2).
- **Doctor?** No.
- **Recommendation**: cluster create preflight: scan existing interfaces/routes and
  `dhcpd_leases` for the chosen subnet before attaching.

### 8. Outbound firewall blocking guest-side basics — **lower likelihood, high confusion**

- **Products**: strict egress firewalls / VPN ACLs blocking UDP 123 (NTP) and
  non-proxy 443.
- **Breaks**: Talos nodes sync time from `time.cloudflare.com` by default (talos-box
  generates no `machine.time` config). Blocked NTP → clock skew → x509 "certificate
  not yet valid" errors during bootstrap that look like PKI bugs.
- **Doctor?** No.
- **Recommendation**: document adding a `machine.time.servers` patch pointing at
  `172.30.<n>.1` if tbxd ever grows an NTP shim, or at the corporate NTP server;
  status hint when node clock skew is detected via talosctl.

### 9. Interception-induced code defects — **must fix regardless of environment**

Three real defects in current code that stay latent until a proxy/SWG gets between
talos-box and its upstreams (independent-review findings, verified against source):

- **Manifest cache poisoning by block pages**: the mirror stores *any* HTTP 200 body
  as a cached manifest without validating media type, JSON structure, or digest
  (`internal/mirror/mirror.go:95,217-244`). A proxy's HTML block page becomes the
  cluster's permanent "manifest", served even offline. Fix: validate OCI media type +
  `Docker-Content-Digest` before caching; detect `text/html` responses and report
  "blocked by web filter: <url>" instead.
- **Blobs verified only at EOF, after streaming to the guest**
  (`internal/mirror/mirror.go:189-209`): the cache is protected, but the first puller
  receives intercepted/corrupted bytes. Fix: stage-verify-serve on first fetch.
- **Image Factory downloads have no integrity validation**
  (`internal/imagecache/cache.go:158-176`): any 2xx body is stored and piped to `xz`.
  A block page yields a cryptic xz failure (or worse, a cached corrupt image). Fix:
  check XZ magic/content type and a published checksum before accepting.

Related robustness gaps in the same category: the embedded DNS is **UDP-only**
(`internal/dns/server.go:15-26`; some resolvers/filters mandate TCP retry), and a
`/var/db/dhcpd_leases` read error silently becomes "node has no IP"
(`internal/vm/lease.go:11-16`) — indistinguishable from a boot failure.

### 10. Miscellaneous / lower likelihood

- **Content-filter NE extensions throttling vmnet**: rare, but NE filter-data providers
  can inspect every flow; worst observed effect is throughput collapse. No mitigation
  beyond documenting.
- **Cleartext-HTTP policy**: content filters that flag "web on nonstandard port" can
  reset guest→mirror traffic (plain HTTP on 5055–5058) even though it never leaves the
  host. Escape hatch would be an opt-in HTTPS mirror mode with a generated CA delivered
  via machine-config trust patch.
- **DLP / antimalware scanning of large artifacts**: on-access scanners chewing through
  multi-GB `.raw`/`.xz`/blob files under `~/.talosbox` can stall downloads past the
  mirror's 5-minute timeout or quarantine cache files. Mitigation: publish narrow
  path/process exclusions for IT rather than asking for broad exceptions.
- **EDR network containment**: CrowdStrike containment / Defender isolation cut all
  traffic except the security agent's. Doctor passes fully (all four checks are local).
  Doctor should compare local-pass vs external-fail and name the pattern.
- **App-control ergonomics**: `tbxd` runs as a parentless, ad-hoc-signed background
  process launched from an arbitrary path, and `tbx system install` points launchd at
  the in-place sibling binary rather than a signed copy under
  `/Library/PrivilegedHelperTools` (`cmd/tbx/system.go:103-116`) — both are patterns
  application-control products score badly. Moving to SMAppService/PrivilegedHelperTools
  with stable signing identity would help materially.
- **Port 179 (BGP mode)**: EDR may flag a root process listening on a routing-protocol
  port; bind failure only surfaces as a GoBGP start error. Doctor could preflight-bind.
- **Charles/local proxies**: a system-wide HTTP proxy pointing at localhost captures
  host curl testing (`curl` honors proxy env; the browser honors system proxy) — user
  sees proxy errors for `http://*.k8s.test` URLs. Recommend `NO_PROXY=.k8s.test,172.30.0.0/16`
  guidance in docs.
- **World-writable helper socket** (`/var/run/tbx-helper.sock`, 0666, no peer auth,
  TODO at `internal/helper/server.go:69-70`): not a lockdown-breakage issue but the
  kind of finding a corporate security review flags before permitting the tool. Worth
  fixing (peer-credential check) before pitching IT departments.

## Prioritized hardening list for talos-box

1. **Developer-ID sign + notarize all three binaries**, install the helper as a signed
   copy under `/Library/PrivilegedHelperTools` (unblocks MDM/EDR allowlisting) — #3.
2. **Mirror/factory integrity validation** (block-page detection, digest-before-cache,
   stage-verify-serve, factory checksum + timeout) — #9; these are correctness bugs,
   not just corporate-compat issues.
3. **Doctor: system-path DNS resolution check** (catches Umbrella/DoH class) — #4.
4. **Doctor: route-egress check per cluster subnet + create-time collision preflight;
   make the subnet base a single shared, configurable constant** — #2, #7.
5. **Doctor: host egress probe with distinct x509/timeout/reset verdicts + hostname-
   chain recording for IT allowlists** — #5.
6. **Periodic re-assertion of forwarding + resolver (or `doctor --fix`)** — #6.
7. **Guest-side TLS canary + workshop-author docs about the mirror boundary** — #1.
8. **Helper socket peer-credential auth; drop the 0666 mode** — #10.
9. **Doctor: environment inventory** — enumerate `systemextensionsctl list`, active
   DNS proxies/content filters/VPN providers and map known bundle IDs to specific
   warnings (e.g. "GlobalProtect present: guest TLS will be reset; mirrors required").

## Verified-working baseline

For calibration: on a Mac running GlobalProtect 6.2.8 (NE extension active, corporate
tunnel owning the default route, corporate DNS at 10.95.1.6) plus ProtonVPN, UniFi
Identity, netbird, Tailscale, Docker, and OrbStack installed, the full workshop path —
`cluster create` → talosctl → Cilium → browsable ingress URL — worked on the first
attempt. The mirror + scoped-resolver + connected-route design choices are carrying
that result; the risks above are the configurations one step stricter.
