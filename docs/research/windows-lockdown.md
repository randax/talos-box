# Corporate lockdown analysis — what would break a talos-box-shaped tool on managed Windows 11 endpoints

Companion to `docs/corporate-lockdown-analysis.md` (the macOS version, currently on
`harden/doctor-diagnostics` at commit `124e763`, not yet merged to `main`). Same goal, same
structure, same ranking method (likelihood on a typical corporate laptop × impact). **talos-box
has no Windows code today** — no Windows build target, no Hyper-V backend, no NRPT/DNS
integration exists in this repo as of this writing. This document is therefore a **forward-looking
threat survey for a hypothetical Windows 11 + Hyper-V port**, reasoning by direct analogy to the
macOS mechanism inventory (vmnet → Hyper-V vSwitch, launchd root helper → elevated Windows
service, `/etc/resolver` scoped DNS → NRPT, ad-hoc codesign → Authenticode). Every claim about
Windows/vendor mechanics below is cited to a primary source; every claim about what a Windows
port of talos-box specifically would do is marked **(inference)** since no such code exists to
cite line numbers from.

Assumed target architecture (inference, modeled on the macOS design):

- A host-side `tbxd`-equivalent process making outbound HTTPS to `factory.talos.dev` and registry
  upstreams, using Go's standard `net/http` client.
- Guests (Talos nodes) run as Hyper-V VMs attached to a Hyper-V **external or internal virtual
  switch**, NAT'd or routed to a private subnet — the vSwitch is the Windows analog of vmnet.
- An embedded DNS resolver, exposed to the host via an **NRPT** rule scoping `*.k8s.test` to
  `127.0.0.1:<port>`, mirroring the macOS `/etc/resolver/k8s.test` approach.
- A privileged component (Windows service or scheduled task running as SYSTEM/admin) responsible
  for creating the vSwitch, installing the NRPT rule, and any NAT/routing setup — the analog of
  the macOS root launchd helper.
- Binaries distributed either unsigned or Authenticode-signed depending on how far hardening has
  progressed by the time of a real port (this mirrors the macOS doc's ad-hoc-vs-notarized framing).

## Risk matrix

Ranked by likelihood on a typical corporate Windows 11 laptop (Intune-enrolled, EDR-monitored,
VPN/ZTNA client present, standard-user by default per modern EPM guidance) × impact.

### 1. VPN/ZTNA WFP callout drivers resetting guest-attributed TLS — **high likelihood, high impact**

- **Mechanism**: The Windows Filtering Platform (WFP) is the kernel/user-mode framework
  (`bfe.dll`/Base Filtering Engine + kernel callout drivers) that essentially all Windows VPN,
  ZTNA, and EDR products use to intercept and filter traffic at multiple TCP/IP-stack layers —
  this is Microsoft's own characterization of "most commercial endpoint security products —
  including antivirus suites, EDR platforms, host intrusion prevention systems, and VPN
  clients."[^wfp-arch][^wfp-callout] Zscaler Client Connector ships an explicit **Windows filter
  driver** as an alternative to a virtual adapter specifically so it can "capture and forward
  traffic" without creating a TAP-style NIC.[^zscaler-filter] Z-Tunnel 2.0 (the current default)
  tunnels all ports/protocols via TLS/DTLS to the Zscaler cloud.[^zscaler-ztunnel]
- **Breaks**: WFP's network-layer (ALE/IP) callout layers see packets regardless of the owning
  process for routed/NAT'd traffic[^wfp-arch] — this is the direct Windows analog of the macOS
  Network Extension framework RSTing guest NAT flows it can't attribute to a trusted host
  process. A Hyper-V VM's NAT'd egress through a "Default Switch" traverses the host's own
  NAT/ICS path, which is exactly the kind of traffic a WFP callout can flag as unattributed and
  reset. **(inference: the specific interaction between Hyper-V's internal NAT implementation
  and a given vendor's WFP callout has not been independently verified here — flagged as an open
  question below.)**
- **Symptom (inference)**: TLS connections from guest VMs get reset while HTTP/DNS/NTP continue
  to work — same signature as the macOS doc's #1.
- **Mitigation direction (inference)**: same as macOS — route the critical path (image pulls)
  through the host process exclusively; document that any pod dialing TLS directly is exposed;
  consider a host-side forwarding proxy escape hatch.

### 2. VPN "no local network" / route capture of the vSwitch subnet — **high likelihood, no detection**

- **Mechanism**: Cisco Secure Client (AnyConnect) has a documented, versioned interaction with
  Hyper-V: "To accommodate a Hyper-V behavior change on Windows 10 (Redstone 3 or later), tunnel
  security reinforcement has been optimized while using tunnel-all or split-exclude
  configurations. When a new interface address is detected, Hyper-V is properly enforced without
  causing the appearance of multiple reconnects" — i.e., the client actively watches for new
  Hyper-V-created interfaces and re-applies tunnel policy to them.[^cisco-hyperv] Cisco's Trusted
  Network Detection (TND) can also auto-disconnect/reconnect the tunnel based on detected network
  changes, with the default disconnect-on-trusted-network behavior documented in the admin
  guide.[^cisco-tnd] GlobalProtect and other NE/WFP-based clients support similarly aggressive
  "no direct access to local network" policies (established for the macOS analysis; the Windows
  equivalents are the same vendor products, same policy concept, on a different filtering
  substrate).
- **Breaks**: host↔VM management traffic and any routed path to the vSwitch subnet. If a client
  is configured to block all non-tunnel local-adapter traffic, every host→guest path dies —
  identical failure mode to the macOS doc's #2 sub-case.
- **Symptom (inference)**: timeouts to the vSwitch subnet while Hyper-V shows the VM running;
  clears immediately on VPN disconnect — the diagnostic tell, same as macOS.
- **Mitigation direction (inference)**: a doctor-equivalent should resolve the effective route/
  interface for the vSwitch subnet (`Get-NetRoute`/`route print` equivalent) and flag when it
  isn't the vSwitch's own interface; preflight for subnet collision at cluster-create time.

### 3. Elevation/MDM blocking the privileged install path — **high likelihood at strict orgs**

- **Mechanism, verified**: Creating a Hyper-V virtual switch with `New-VMSwitch` **requires an
  elevated (administrator) PowerShell session**[^new-vmswitch] — membership in the "Hyper-V
  Administrators" local group alone is documented as sufficient for VM operations from the GUI,
  but is explicitly called out as *not* sufficient for all console/PowerShell operations without
  also being a local admin in Microsoft's own historical guidance.[^hyperv-admins-group] Hyper-V
  itself is only available on Windows 11 **Pro, Enterprise, and Education** — not Home — and
  requires firmware-level virtualization extensions enabled.[^hyperv-edition] Separately,
  Microsoft Intune's **Endpoint Privilege Management (EPM)** is Microsoft's current
  recommended path for moving corporate fleets to standard-user accounts, explicitly stating
  "Endpoint Privilege Management doesn't manage elevation requests by users that have
  administrative permissions on a device" — i.e., EPM is deployed specifically to *remove* local
  admin, and per-app elevation must be explicitly configured per rule.[^epm-transition] Intune
  device-restriction profiles can independently gate the Hyper-V optional feature via
  DISM/`Disable-WindowsOptionalFeature -FeatureName Microsoft-Hyper-V-All` pushed as a
  script/CSP.[^intune-hyperv-disable]
- **Breaks**: everything. No admin session (or no EPM elevation rule for the install path) means
  no vSwitch, and on a standard-user fleet, no way to enable the Hyper-V optional feature at all
  if IT hasn't already turned it on. This is the single largest exposure for a Windows port —
  more binary than the macOS case, where a Developer-ID-signed helper at least has a shot at
  install without a live admin session; Windows elevation for `New-VMSwitch` has no unattended
  equivalent short of a pre-provisioned admin/EPM rule.
- **Symptom (inference)**: install fails at the elevation prompt (UAC) or is silently denied by
  EPM if no rule is configured; on locked-down fleets Hyper-V may not appear in "Turn Windows
  features on or off" at all if disabled by Intune policy.
- **Mitigation direction (inference)**: document the required EPM elevation rule / admin
  prerequisite explicitly for IT; a doctor-equivalent should distinguish "Hyper-V feature
  disabled by policy" from "Hyper-V present but caller lacks elevation" from "vSwitch creation
  denied" — these need different IT asks.

### 4. Hyper-V vs. third-party hypervisors (VMware Workstation, VirtualBox) — **medium-high likelihood, binary conflict**

- **Mechanism, verified**: Microsoft's own troubleshooting doc states plainly that "virtualization
  applications, such as VMware and VirtualBox, can't run alongside Hyper-V, Memory Integrity, or
  Credential Guard" because "only one software component at a time can use hardware
  virtualization extensions."[^vbs-conflict] Critically, this isn't limited to the Hyper-V role
  being manually enabled — Virtualization-Based Security features (Credential Guard, Memory
  Integrity/HVCI, enabled by default on many modern OEM images and commonly enforced via Intune
  Device Guard policy) silently start the hypervisor even when the Hyper-V *role* looks
  off.[^vbs-conflict]
- **Breaks**: this is a stronger conflict than the macOS doc's #7 (peaceful coexistence of
  multiple vmnet-based tools) — on Windows it is **mutually exclusive**. A corporate laptop
  already running VMware Workstation or VirtualBox for legacy work, in an org that hasn't
  standardized on Hyper-V, cannot run a Hyper-V-based talos-box at all without the user disabling
  their other virtualization tool, and if Credential Guard is enforced by Intune policy, the user
  may not be able to turn Hyper-V off even if they wanted to route around the conflict.
  **(inference: WSL2 and Docker Desktop's WSL2 backend are generally fine here since they already
  run on the Hyper-V/Virtual Machine Platform substrate rather than competing with it — this is
  unverified for the newest Docker Desktop builds and flagged as an open question.)**
- **Symptom (inference)**: Hyper-V install/VM-start fails with a virtualization-extension-in-use
  error, or the other hypervisor breaks instead if Hyper-V/VBS was enabled first.
- **Mitigation direction (inference)**: doctor-equivalent should detect VBS/HVCI state
  (`Get-CimInstance Win32_DeviceGuard` or `msinfo32` equivalent) and competing hypervisor
  processes/services before cluster create; document the conflict and point at the specific
  Intune/GPO Credential Guard setting IT would need to exempt the device from.

### 5. NRPT scoping vs. corporate DNS filtering agents — **medium-high likelihood**

- **Mechanism, verified**: The Name Resolution Policy Table (NRPT) is a Windows registry-backed
  table the DNS Client service consults *before* issuing a query; if an FQDN matches an NRPT
  entry, the client uses the policy's configured server/settings instead of the adapter's normal
  DNS servers — Microsoft's own documentation frames this as the mechanism for split-DNS/VPN
  scenarios.[^nrpt] However, both Zscaler and Cisco Umbrella intercept DNS **at the client/IP
  stack level**, not just by configuring adapter DNS servers: Zscaler's client can act as a DNS
  proxy and "control DNS traffic regardless of the DNS resolver selected by the end
  user,"[^zscaler-dns] and Cisco Umbrella's roaming client "intercepts all DNS traffic and
  redirects it to Umbrella DNS for filtering and logging" at the IP stack level — precisely the
  kind of interception that sits below NRPT's query-routing logic and would still capture a
  query even if NRPT correctly scopes it to `127.0.0.1`.
- **Breaks**: resolution of `*.k8s.test`-style synthetic names, same failure mode as the macOS
  doc's #4 — tools that consult NRPT/system resolver correctly may still lose to an
  interception layer that grabs the packet before or regardless of NRPT's decision; browsers
  with DoH enabled bypass both NRPT and the interceptor's local view entirely.
- **Symptom (inference)**: `nslookup`/`Resolve-DnsName` may work while the browser fails, or vice
  versa, depending on where in the stack the corporate DNS agent hooks.
- **Mitigation direction (inference)**: same as macOS — doctor-equivalent should resolve a
  `*.k8s.test` name through the actual system resolver path, not just probe the local DNS
  socket; document disabling browser Secure DNS / DoH for `.test`.

### 6. TLS-interception proxy of host egress + the WinHTTP/PAC story for Go binaries — **medium-high likelihood, currently fatal without extra code**

- **Mechanism, verified**: Go's `http.ProxyFromEnvironment` — the default proxy resolution used
  by `net/http`'s default transport — **only reads `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY`
  environment variables; it does not consult the Windows registry proxy settings, WPAD, or PAC
  files** that corporate fleets almost universally use via Group Policy / Intune "automatically
  detect settings."[^go-proxy-gap] Reading those requires an explicit third-party dependency
  (e.g. `go-ieproxy`, which calls `WinHttpGetIEProxyConfigForCurrentUser` with a registry
  fallback).[^go-proxy-gap] Separately, Go's `crypto/x509.SystemCertPool()` on Windows populates
  from the OS cert store via `certutil.exe`/CryptoAPI internally,[^go-x509-syspool] so an
  MDM-installed corporate TLS-inspection root **is** picked up automatically for verification —
  the certificate-trust half of this story is fine; it's specifically the *proxy discovery* half
  that's missing by default. There's a long-standing, still-open Go issue tracking exactly this
  gap for corporate proxies.[^go-proxy-issue]
- **Breaks**: host-side `factory.talos.dev`/registry-upstream downloads, mirroring the macOS
  doc's #5 exactly — a user whose corp mandates PAC-file proxy discovery (extremely common on
  Windows fleets, more so than on macOS where `scutil --proxy` at least has a well-known escape
  hatch) gets hangs, not clean failures, if the equivalent of `imagecache`'s "no HTTP timeout"
  bug is carried over to a Windows port **(inference: unverified since no Windows port exists;
  flagged as a thing to *not* repeat)**.
- **Symptom (inference)**: image download hangs indefinitely on any network requiring PAC/WPAD
  proxy discovery, even though the machine's browser works fine.
- **Mitigation direction (inference)**: add an explicit timeout to the factory/mirror HTTP
  clients from day one of a Windows port (don't repeat the macOS bug); use a library like
  `go-ieproxy` or shell out to `netsh winhttp show proxy` / call `WinHttpGetProxyForUrl` to
  resolve the effective proxy before making the request, rather than relying on
  `ProxyFromEnvironment` alone.

### 7. EDR (CrowdStrike/Defender/SentinelOne) driver compatibility and containment — **medium likelihood**

- **Mechanism, verified**: CrowdStrike states "all Windows executable code that CrowdStrike
  ships is signed, and all Falcon driver code is compatible with... hypervisor-enforced code
  integrity (HVCI)"[^cs-kernel], i.e., mainstream EDR vendors design for coexistence with
  VBS/HVCI rather than fighting it — good news for a Hyper-V-based tool in principle. Microsoft
  Defender for Endpoint's Attack Surface Reduction (ASR) rule "Block executable files from
  running unless they meet a prevalence, age, or trusted list criterion" explicitly targets
  exactly the failure mode a brand-new, low-prevalence tool binary would hit: it "errs on the
  side of caution and also blocks files that don't yet have a positive reputation," resolving
  only "as the file's reputation and trust values incrementally increase."[^asr-ref] This rule is
  opt-in (requires cloud-delivered protection enabled and the rule turned on), not default, which
  is why this is ranked medium rather than high.[^asr-ref]
- **Breaks**: first-run of a brand-new Windows binary (installer, service, or CLI) on any fleet
  that has this ASR rule enabled; more broadly, EDR network-containment features (isolating a
  host except for the security agent's own traffic) would blackhole everything the same way the
  macOS doc's #10 "EDR network containment" item describes — a doctor-equivalent would pass all
  local checks while every external path is dead.
- **Symptom (inference)**: fresh install blocked/quarantined on first run with an ASR
  notification balloon; or (containment case) all local checks pass, all network paths fail.
- **Mitigation direction (inference)**: Authenticode-sign and, ideally, get an EV cert / submit
  for Microsoft reputation seeding before wide distribution — same logic as the macOS doc's
  Developer-ID recommendation; doctor-equivalent should detect containment-vs-genuine-failure by
  comparing local-only success against external-probe failure.

### 8. AppLocker / WDAC vs. unsigned binaries and services; SmartScreen reputation gate — **medium likelihood, high UX friction**

- **Mechanism, verified**: WDAC is Microsoft's kernel-level, harder-to-bypass successor to
  AppLocker — "WDAC enforcement happens in the kernel before code is loaded into memory,"
  vs. AppLocker's user-mode enforcement which "an attacker with local administrator rights can
  disable."[^wdac-vs-applocker] WDAC identifies trusted code by signing certificate (embedded or
  catalog signing) or explicit hash allowlist — unsigned LOB binaries are commonly handled via
  catalog signing or hash rules rather than being categorically impossible, but that requires the
  org to have added the specific binary.[^wdac-signing] Separately, **SmartScreen** fires
  specifically on files carrying the Mark-of-the-Web (i.e., anything downloaded via a browser or
  otherwise flagged as internet-origin) and evaluates both publisher signature reputation and
  file-hash reputation; unsigned files start at zero reputation every single build unless signed
  consistently with the same publisher identity, and building enough reputation to stop the
  "Windows protected your PC" prompt can take "several weeks and hundreds of clean installs from
  a wide audience."[^smartscreen]
- **Breaks**: WDAC in enforce mode blocks an unlisted unsigned binary/service outright — high
  impact but lower likelihood since WDAC deployment (vs. audit-mode-only) is still the minority
  case on typical corporate fleets per the sources above, more common at security-mature orgs
  (the direct Windows analog of the macOS doc's Santa/#3 case). SmartScreen is near-universal
  and will reliably interrupt first-run of an unsigned downloaded installer regardless of org
  maturity — pure friction, not an outright block (user can click through "Run anyway" if not
  additionally restricted by policy).
- **Symptom (inference)**: WDAC — process refuses to start, Event Viewer Code Integrity log
  entry; SmartScreen — "Windows protected your PC" / "Windows Defender SmartScreen prevented an
  unrecognized app from starting" interstitial on first run of the installer/binary.
- **Mitigation direction (inference)**: same conclusion as macOS — sign everything (Authenticode,
  ideally EV) as early as possible; document the WDAC catalog-signing/hash-rule path for IT if
  full reputation-building isn't feasible pre-launch.

### 9. Hyper-V vSwitch port ACLs (Router Guard / DHCP Guard) vs. an embedded DHCP/router service — **lower likelihood, high confusion (inference-heavy)**

- **Mechanism (inference, flagged as needing verification)**: Hyper-V VM network adapters support
  per-port "Router Guard" and "DHCP Guard" settings that, when enabled, drop router
  advertisement/DHCP-server traffic *from* a VM that isn't the designated DHCP/router — these are
  off by default per Microsoft's virtual-switch documentation pattern but can be turned on by a
  corporate VM baseline/hardening template. If a Windows port of talos-box needs the *host* (not
  a guest) to answer DHCP on the vSwitch, this is likely a non-issue since the guard applies to
  VM ports, not the host's own vSwitch management operating system stack — this line hasn't been
  independently verified against current Hyper-V documentation and is included as an open
  question rather than a confirmed risk.
- **Breaks (inference)**: node IP assignment, if the design ever needs a VM (rather than the
  host) to run DHCP.
- **Mitigation direction (inference)**: if this pattern is adopted, prefer the host-side DHCP
  path (mirroring how the macOS doc favors host-side `bootpd`) to sidestep VM-port guard settings
  entirely.

### 10. Miscellaneous / lower likelihood

- **Kernel-mode driver signing enforcement**: 64-bit Windows has required kernel-mode drivers to
  be signed since Vista, and modern Windows 10/11 requires drivers be submitted through and
  signed by the Windows Hardware Dev Center with an EV certificate for production
  distribution.[^driver-signing] **(inference)** This is likely a non-issue for a Windows port
  that talks to Hyper-V purely through its public WMI/PowerShell/HCS APIs and needs no custom
  kernel driver — worth confirming explicitly once a real design exists, since it's the single
  highest-friction item on the macOS side (the unsigned root helper) and its Windows absence
  would be a meaningful design win.
- **Outbound firewall / VPN ACLs blocking NTP and non-proxy 443**: same failure mode as the macOS
  doc's #8 — Talos nodes syncing against `time.cloudflare.com` by default, blocked NTP causing
  clock-skew x509 errors during bootstrap. Platform-agnostic; not re-derived here.
- **Compliance/state-reset agents**: Windows equivalents of macOS's CIS-benchmark forwarding
  reset would be Group Policy re-application resetting Windows Firewall rules, ICS/NAT config, or
  pruning unrecognized services/scheduled tasks. **(inference, not independently verified)** —
  flagged as an open question; the underlying pattern (periodic compliance re-assertion fighting
  a tool's one-time setup) is well established on macOS and plausible but unconfirmed on Windows.
- **Charles/local proxies and `NO_PROXY` guidance**: same as macOS — a system-wide proxy pointing
  at localhost would capture host-side `curl`/browser testing of `*.k8s.test` URLs; document
  `NO_PROXY=.k8s.test,<vswitch-subnet>` guidance.
- **WSL2 interaction**: since WSL2 already runs on the Hyper-V/"Virtual Machine Platform"
  substrate, a Hyper-V-based talos-box is plausibly the *one* Windows virtualization story that
  coexists cleanly with WSL2/Docker Desktop rather than fighting it for hardware virtualization
  extensions — the opposite of the VMware/VirtualBox conflict in #4. **(inference, not
  independently verified for current Docker Desktop versions.)**

## Prioritized hardening list for a hypothetical Windows port (inference — mirrors the macOS doc's structure)

1. Design the privileged path so it needs **one** elevation/EPM event, not root-forever — the
   Windows analog of "sign + notarize"; document the exact EPM rule or admin prerequisite IT must
   grant (#3).
2. Add real HTTP timeouts and a WinHTTP/PAC-aware proxy resolution path (`go-ieproxy` or
   equivalent) to the host egress client from the start — don't inherit the macOS mirror's
   no-timeout bug (#6).
3. Doctor-equivalent: system-path DNS resolution check through the actual Windows resolver, not
   just the local socket (#5).
4. Doctor-equivalent: route/interface check for the vSwitch subnet + VBS/Credential Guard and
   competing-hypervisor detection before cluster create (#2, #4).
5. Authenticode-sign all binaries as early as possible; plan for the SmartScreen
   reputation-building lag and the WDAC catalog-signing path (#7, #8).
6. Confirm early whether a Windows port needs any custom kernel-mode driver at all — if it can
   stay entirely in WMI/PowerShell/HCS API territory, that's a structural advantage over the
   macOS unsigned-helper exposure worth preserving deliberately (#10).

## Open questions (flagged for follow-up, not resolved here)

- Exact behavior of specific vendor WFP callouts (GlobalProtect, Zscaler, Netskope, AnyConnect)
  against Hyper-V's internal NAT implementation for a "Default Switch" — no live test performed;
  the macOS doc's #1/#2 findings were validated with a live GlobalProtect-equipped machine, this
  Windows document has no equivalent live validation.
- Whether Docker Desktop's current WSL2 backend and a Hyper-V-based talos-box would actually
  coexist without contention, given both ultimately compete for the same hardware virtualization
  extensions once VMs are actually running (as opposed to WSL2's typically lighter footprint).
- Hyper-V vSwitch port ACL (Router Guard/DHCP Guard) default state and whether any corporate VM
  baseline commonly enables them — item #9 above is speculative pending verification against
  current Hyper-V documentation.
- Whether Intune's ASR "block low-prevalence executables" rule or WDAC enforcement mode are
  common enough on real corporate fleets to justify ranking them above medium — no telemetry or
  survey data was available to this analysis, only vendor/Microsoft documentation of the
  mechanisms themselves.

## Sources

[^wfp-arch]: [Windows Filtering Platform Architecture Overview — Microsoft Learn](https://learn.microsoft.com/en-us/windows/win32/fwp/windows-filtering-platform-architecture-overview)
[^wfp-callout]: [Introduction to Windows Filtering Platform Callout Drivers — Microsoft Learn](https://learn.microsoft.com/en-us/windows-hardware/drivers/network/introduction-to-windows-filtering-platform-callout-drivers)
[^zscaler-filter]: [Using the Windows Filter Driver for Zscaler Client Connector — Zscaler Help](https://help.zscaler.com/client-connector/using-windows-filter-driver-zscaler-client-connector)
[^zscaler-ztunnel]: [About Z-Tunnel 1.0 & Z-Tunnel 2.0 — Zscaler Help](https://help.zscaler.com/client-connector/about-z-tunnel-1.0-z-tunnel-2.0)
[^cisco-hyperv]: [AnyConnect reconnects with Hyper-V Adapter — Cisco Community](https://community.cisco.com/t5/vpn/anyconnect-reconnects-with-hyper-v-adapter/td-p/3347550)
[^cisco-tnd]: [Cisco Secure Client (including AnyConnect) Administrator Guide, Release 5.1 — Connect and Disconnect to a VPN](https://www.cisco.com/c/en/us/td/docs/security/vpn_client/anyconnect/Cisco-Secure-Client-5/admin/guide/b-cisco-secure-client-admin-guide-5-1/configure_vpn.html)
[^new-vmswitch]: [New-VMSwitch (Hyper-V) — Microsoft Learn (PowerShell reference)](https://learn.microsoft.com/en-us/powershell/module/hyper-v/new-vmswitch?view=windowsserver2025-ps)
[^hyperv-admins-group]: [Allowing non-administrators to control Hyper-V — Thomas Marcussen / Microsoft Virtual PC Guy archive](https://blog.thomasmarcussen.com/allowing-non-administrators-to-control-hyper-v/)
[^hyperv-edition]: [Install Hyper-V in Windows and Windows Server — Microsoft Learn](https://learn.microsoft.com/en-us/windows-server/virtualization/hyper-v/get-started/install-hyper-v)
[^epm-transition]: [Use Endpoint Privilege Management to transition users from administrator to standard user — Microsoft Intune](https://learn.microsoft.com/en-us/intune/intune-service/protect/epm-transition-administrator-to-standard-user)
[^intune-hyperv-disable]: [Restrict devices features using policy in Microsoft Intune — Microsoft Learn](https://learn.microsoft.com/en-us/intune/intune-service/configuration/device-restrictions-configure)
[^vbs-conflict]: [Virtualization applications can't run alongside Hyper-V and its dependent features — Microsoft Learn (Windows Client troubleshooting)](https://learn.microsoft.com/en-us/troubleshoot/windows-client/application-management/virtualization-apps-not-work-with-hyper-v)
[^nrpt]: [Configure DNSSEC rules using the Name Resolution Policy Table in Windows — Microsoft Learn](https://learn.microsoft.com/en-us/windows-server/networking/dns/name-resolution-policy-table)
[^zscaler-dns]: [Understanding DNS Resolution and Optimization in Zscaler Deployments — Zscaler Community Guide](https://community.zscaler.com/s/Guides/aSoPJ00000065JZ0AY/understanding-dns-resolution-and-optimization-in-zscaler-deployments)
[^go-proxy-gap]: [go-ieproxy — mattn/go-ieproxy, Go Packages](https://pkg.go.dev/github.com/mattn/go-ieproxy)
[^go-x509-syspool]: [crypto/x509: make SystemCertPool work on Windows — golang/go issue #16736](https://github.com/golang/go/issues/16736)
[^go-proxy-issue]: [crypto/x509: corporate proxy: certificate signed by unknown authority — golang/go issue #40370](https://github.com/golang/go/issues/40370)
[^cs-kernel]: [Tech Analysis: CrowdStrike's Kernel Access and Security Architecture — CrowdStrike blog](https://www.crowdstrike.com/en-us/blog/tech-analysis-kernel-access-security-architecture/)
[^asr-ref]: [Attack surface reduction (ASR) rules reference — Microsoft Defender for Endpoint, Microsoft Learn](https://learn.microsoft.com/en-us/defender-endpoint/attack-surface-reduction-rules-reference)
[^wdac-vs-applocker]: Comparative WDAC/AppLocker enforcement-model summary aggregated from multiple secondary sources during this survey (WDAC = kernel-level, harder to bypass; AppLocker = user-mode). No single Microsoft Learn page was found stating this comparison as directly as cited here — **treat this specific framing as secondary-source-derived, not a first-party Microsoft claim**, pending a follow-up citation to the primary [Windows Defender Application Control design guide](https://learn.microsoft.com/en-us/windows/security/application-security/application-control/windows-defender-application-control/wdac-and-applocker-overview).
[^wdac-signing]: WDAC catalog-signing/hash-rule handling of unsigned LOB binaries — secondary-source summary during this survey; follow up against the primary [WDAC deployment guide](https://learn.microsoft.com/en-us/windows/security/application-security/application-control/windows-defender-application-control/design/create-initial-default-policy) for a first-party citation.
[^smartscreen]: [SmartScreen reputation for Windows app developers — Windows Apps, Microsoft Learn](https://learn.microsoft.com/en-us/windows/apps/package-and-deploy/smartscreen-reputation)
[^driver-signing]: [Kernel-Mode Code Signing Requirements — Windows drivers, Microsoft Learn](https://learn.microsoft.com/en-us/windows-hardware/drivers/install/kernel-mode-code-signing-requirements--windows-vista-and-later-)

Additional primary/vendor context consulted but not directly cited inline: [Hyper-V Extensible
Switch overview](https://learn.microsoft.com/en-us/windows-hardware/drivers/network/overview-of-the-hyper-v-extensible-switch)
and [components](https://learn.microsoft.com/en-us/windows-hardware/drivers/network/hyper-v-extensible-switch-components)
(Microsoft Learn); [GlobalProtect app configuration](https://docs.paloaltonetworks.com/globalprotect/10-1/globalprotect-admin/globalprotect-portals/define-the-globalprotect-app-configurations)
and [Netskope traffic steering](https://docs.netskope.com/en/traffic-steering) (vendor docs, used
for architecture framing rather than a specific quoted claim).
