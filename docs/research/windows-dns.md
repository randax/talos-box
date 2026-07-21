# Windows per-domain DNS options for `k8s.test`

Research ticket: [#52](https://github.com/randax/talos-box/issues/52) (part of
[#48](https://github.com/randax/talos-box/issues/48)). Question: what is the
`/etc/resolver/k8s.test` analogue on Windows — a way to route only `*.k8s.test` to a
local resolver?

## Recommended mechanism

**NRPT via `Add-DnsClientNrptRule -Namespace ".k8s.test" -NameServers "<local resolver IP>"`**,
installed by the elevated helper at the same time it does the equivalent of `/etc/resolver`
on macOS. This is the only Windows mechanism that matches the macOS per-domain resolver
model: it is scoped to a namespace (suffix match, including child domains), doesn't
require rewriting the whole system resolver, and is a first-class, scriptable OS feature
(no third-party proxy to install or trust).

Two hard constraints fall out of the research and should shape the implementation:

1. **NRPT rewrites the server, not the port.** All primary and community sources describe
   `-NameServers` as a bare IP (or list of IPs); nothing in the docs or WMI schema shows an
   `IP:port` form (contrast with `-DAProxyServerName`, which explicitly documents
   `hostname:port`), and Windows DNS traffic is documented as fixed to port 53 UDP/TCP.
   talos-box's embedded resolver (127.0.0.1:5399 on macOS) **cannot be reused as-is** — on
   Windows the local resolver instance for this NRPT rule needs to listen on port 53 (which
   requires it to bind as a different process/port than whatever `:5399`-style code exists
   today, and needs local-admin rights to bind a low port and to run `Add-DnsClientNrptRule`
   in the first place).
2. **Corporate NRPT in domain Group Policy can silently override or disable talos-box's
   local rule.** This is a documented failure mode (see below), not a hypothetical — it's
   the direct Windows analogue of the macOS "DoH/DNS filtering agent" risk category
   already tracked in `docs/corporate-lockdown-analysis.md` §4. A doctor-style check that
   resolves an actual `*.k8s.test` name via the system resolver (not just checks that the
   NRPT rule exists) is the mitigation that already worked for macOS and should be ported.

Hosts-file and "run a local DNS proxy as primary resolver" are both worse fits and are
covered under Alternatives Considered.

## Facts (with citations)

### NRPT mechanics — `Add-DnsClientNrptRule`

- The cmdlet's `-Namespace` parameter takes one or more DNS namespaces and does **not**
  need a leading wildcard; NRPT namespace matching is suffix-based by design — a `Suffix`
  rule for `contoso.com` "applies to any name that ends in `.contoso.com` and includes
  child domains." Namespace types available: Suffix, Prefix, FQDN, Subnet (IPv4/IPv6), Any.
  [Configure DNSSEC rules using the NRPT (Microsoft Learn)](https://learn.microsoft.com/en-us/windows-server/networking/dns/name-resolution-policy-table)
- `-NameServers` is typed `String[]`; every documented example (`Add-DnsClientNrptRule
  -Namespace "pqr.com" -NameServers "10.0.0.1"`) uses a bare IPv4 address. No IP:port
  syntax is documented for this parameter — contrast with `-DAProxyServerName` on the same
  cmdlet, whose docs explicitly spell out accepted formats `hostname:port`, `IPv4
  address:port`, `IPv6 address:port`. The absence of an equivalent format note for
  `-NameServers`, plus the general statement that Windows DNS servers/clients bind to UDP/TCP
  port 53, is the basis for "NRPT nameserver is IP-only, port 53" (**inferred**, not
  directly stated for this specific parameter).
  [Add-DnsClientNrptRule (Microsoft Learn)](https://learn.microsoft.com/en-us/powershell/module/dnsclient/add-dnsclientnrptrule?view=windowsserver2025-ps),
  [Network Ports Used by DNS (Microsoft Learn)](https://learn.microsoft.com/en-us/windows-server/networking/dns/network-ports)
- **Scope / admin rights**: configuring the NRPT (whether via GPO or `Add-DnsClientNrptRule`
  targeting the local computer) requires "Membership in the Administrators group, or
  equivalent" as the documented minimum. Running `Add-DnsClientNrptRule` with neither
  `-GpoName` nor `-Server` adds the rule "for the local client computer" (machine-scoped,
  not per-user).
  [Configure DNSSEC rules using the NRPT (Microsoft Learn)](https://learn.microsoft.com/en-us/windows-server/networking/dns/name-resolution-policy-table),
  [Add-DnsClientNrptRule (Microsoft Learn)](https://learn.microsoft.com/en-us/powershell/module/dnsclient/add-dnsclientnrptrule?view=windowsserver2025-ps)
- **GPO precedence / coexistence — this is the important corporate-lockdown risk**:
  - NRPT rules can live in local, site, domain, or OU-linked GPOs; normal GP processing
    order applies (local → site → domain → OU), and more specific namespaces win over
    more general ones across GPOs.
  - "NRPT rules don't overwrite each other. If two rules are created in two different GPOs
    that apply to the same namespace for the same user or computer, a conflict occurs. As
    a result, neither rule is applied... **This rule doesn't apply to local Group Policy,
    however. If any NRPT settings are configured in domain Group Policy, then all local
    Group Policy NRPT settings are ignored.**"
    [Configure DNSSEC rules using the NRPT (Microsoft Learn)](https://learn.microsoft.com/en-us/windows-server/networking/dns/name-resolution-policy-table)
  - This is corroborated independently by Microsoft's own Global Secure Access (GSA)
    client docs, which hit exactly the collision talos-box would: "The Global Secure
    Access client for Windows doesn't support Name Resolution Policy Table (NRPT) rules in
    Group Policy... **If NRPT rules are configured in Group Policy, they override local
    NRPT rules configured by the client and private DNS doesn't work.**" GSA also
    documents a nasty edge case: an NRPT section that was configured and then emptied in
    an old Windows version leaves a stale empty `DnsPolicyConfig` in `registry.pol` that
    still overrides local rules when the GPO applies.
    [Known Limitations for Global Secure Access (Microsoft Learn)](https://learn.microsoft.com/en-us/entra/global-secure-access/reference-current-known-limitations)
  - **Practical implication for talos-box**: on any domain-joined Windows machine where IT
    has pushed *any* NRPT rule via GPO (common for split-DNS/VPN setups, DirectAccess/Always
    On VPN, or Umbrella/Zscaler-style NRPT carve-outs), talos-box's locally-added
    `k8s.test` rule is silently ignored — `*.k8s.test` falls through to whatever GPO NRPT
    (or, absent a match, the adapter's DNS servers) resolves it as, i.e. NXDOMAIN. This
    fails the same way macOS's `/etc/resolver` file can be pruned by compliance tooling
    (`corporate-lockdown-analysis.md` §6), except here it's a documented *design*
    behavior, not an incidental side effect — there is no known bypass short of getting a
    domain-side NRPT rule added for the child namespace (which, per the precedence rules,
    would take priority as the more specific rule) or having IT exempt the machine's OU.

### Hosts file

- Windows' hosts file (`%SystemRoot%\System32\drivers\etc\hosts`) has no wildcard support;
  this is confirmed as a still-current limitation, with the workaround suggestions in the
  wild (Acrylic DNS Proxy, third-party filtering software) themselves confirming there is
  no native fix. Disqualifying for `*.k8s.test` for the same reason `/etc/hosts` is
  disqualifying on macOS: it needs one static entry per hostname the workshop happens to
  create (ingress names, per-service VIPs), which talos-box cannot predict ahead of time.
  [Is there any way to make hosts file to support wildcards? (Microsoft Q&A)](https://answers.microsoft.com/en-us/windows/forum/all/is-there-any-way-to-make-hosts-file-to-support/09ae90da-477d-459a-bc97-ecd0f1c1b838)

### Local DNS proxy as system-wide resolver — risk framing

Running a local DNS proxy and pointing the whole machine at it (`Set-DnsClientServerAddress
127.0.0.1`) is the option closest to "just always intercept," and it is the option that
best matches the threat model already documented for macOS in
`docs/corporate-lockdown-analysis.md` §4 (DNS filtering agents vs `/etc/resolver`). The
Windows-specific evidence is arguably *more* alarming than the macOS case, because on
Windows the same trick (a local loopback proxy claimed as primary resolver) is the
mechanism corporate DNS agents themselves use, and it's documented as actively conflicting
with other tools' loopback proxies:

- Cisco Umbrella's Windows Roaming Client uses a **loopback DNS proxy** approach ("Classic
  DNS Filtering relies on Loopback Proxy DNS (127.0.0.2)") and is documented to conflict
  with Zscaler Private Access, which "acts as a DNS proxy" itself — DNS either fails to
  resolve or resolves to the wrong (ZPA) IPs when both are present. Cisco's own
  documented workaround for the conflict is, notably, **an NRPT rule that bypasses the
  roaming client for the affected namespace and routes those queries directly** — i.e.
  Cisco's own guidance converges on the same NRPT mechanism recommended above, not on
  "run another loopback proxy."
  [Configure Umbrella Roaming Client and ZScaler Private Access (Cisco)](https://www.cisco.com/c/en/us/support/docs/security/umbrella/225332-configure-umbrella-roaming-client-and.html),
  [Resolve Zero Trust VPN conflicts with the Windows Roaming Client (DNSFilter)](https://help.dnsfilter.com/hc/en-us/articles/46193117967379-Resolve-Zero-Trust-VPN-conflicts-with-the-Windows-Roaming-Client)
- If talos-box set itself up as the primary loopback resolver, it would be putting itself
  in the exact seat these DNS-security agents already occupy, competing for the same
  127.0.0.x binding and the same "primary resolver" adapter setting — a fight talos-box
  will not win against IT-managed software, and one likely to trip endpoint-security
  alerting (an unrecognized process rewriting `Set-DnsClientServerAddress` looks like the
  DNS-hijacking pattern these agents exist to catch).
- **Inference, not directly sourced**: no Microsoft doc says "don't run a local resolver as
  primary DNS," but the pattern of documented conflicts between *legitimate* vendor
  loopback-proxy DNS tools (Umbrella vs. ZPA above) is strong circumstantial evidence that
  a second, uncoordinated loopback resolver is fragile on a managed Windows machine —
  exactly mirroring the macOS finding that `dig`/raw-DNS/DoH-browser paths never consult a
  scoped resolver and get silently hijacked or blocked by the corporate agent instead.

### Windows 11 DoH and browser interplay with NRPT

- Windows DoH is configured per DNS-server-address entry (Settings → Network & Internet →
  adapter → DNS server properties → "DNS over HTTPS" toggle), and only takes effect if that
  IP is on Windows' **known DoH servers list** (Cloudflare/Google/Quad9 by default;
  extensible via `Add-DnsClientDohServerAddress`). Group Policy exposes the same control as
  **Allow DoH / Require DoH / Prohibit DoH** under `Computer Configuration\Policies\
  Administrative Templates\Network\DNS Client`, with Microsoft explicitly warning **not**
  to set Require DoH on domain-joined machines because AD's own DNS server doesn't support
  DoH.
  [Secure DNS Client over HTTPS (DoH) (Microsoft Learn)](https://learn.microsoft.com/en-us/windows-server/networking/dns/doh-client-support)
- **NRPT + DoH compose, they don't conflict**: "You can use [NRPT] to configure queries to
  a specific DNS namespace to use a specific DNS server. If the DNS server is known to
  support DoH, queries related to that domain will be performed using DoH rather than in
  an unencrypted manner." So an NRPT rule pointing `*.k8s.test` at talos-box's local
  resolver is unaffected by the *system's* DoH policy as long as the local resolver's IP
  isn't itself a "known DoH server" (it won't be) — this is a point in NRPT's favor versus
  worrying about DoH breaking the scoped rule.
  [Secure DNS Client over HTTPS (DoH) (Microsoft Learn)](https://learn.microsoft.com/en-us/windows-server/networking/dns/doh-client-support)
- **The real DoH risk is browser-native DoH, which bypasses the OS resolver (and NRPT)
  entirely** — this is the direct Windows analogue of the macOS finding "curl works,
  Chrome says site can't be reached." Chrome/Edge/Firefox ship their own DoH resolvers
  that talk directly to a cloud provider over HTTPS, never touching the Windows DNS client
  service (and therefore never consulting NRPT). Evidence that this is a known,
  actively-fought-over problem on managed Windows fleets:
  - Chrome disables its Secure DNS UI/behavior automatically when the browser detects it
    is enterprise-managed (AD-joined or Chrome Enterprise policy present), but this is a
    Chrome-side heuristic, not something NRPT causes or controls.
    [Disable DNS over HTTPS on enterprise browsers (Akamai/Enterprise Threat Protector docs)](https://techdocs.akamai.com/etp/docs/disable-doh-browsers)
  - Microsoft's own Global Secure Access client — a first-party Microsoft product — has to
    tell admins to disable each browser's built-in DNS client via registry policy
    (`BuiltInDnsClientEnabled=0` for Edge/Chrome, plus disabling Chrome's
    `chrome://flags` "Async DNS resolver") because otherwise the browser's DoH path
    routes around the OS resolver the same way it does on macOS. This is the single
    strongest primary-source confirmation that "browser DoH bypasses NRPT" is a fact
    Microsoft itself designs mitigations around, not user speculation.
    [Known Limitations for Global Secure Access (Microsoft Learn)](https://learn.microsoft.com/en-us/entra/global-secure-access/reference-current-known-limitations)
  - Cisco documents the equivalent GPO-based lockdown for Umbrella deployments (forcing
    Firefox/Chrome to respect the system DNS config rather than their own DoH).
    [Maintain DoH in Firefox and Chrome Using GPO (Cisco)](https://www.cisco.com/c/en/us/support/docs/security/umbrella/224957-maintain-doh-in-firefox-and-chrome.html)

## Recommendation detail

- Install (helper, elevated): `Add-DnsClientNrptRule -Namespace ".k8s.test" -NameServers
  "<resolver-IP>"`. Requires the same local-admin/elevated-helper model talos-box already
  uses on macOS (`dev.talosbox.helper` doing privileged setup) — Windows equivalent would
  be a privileged service/installer, since NRPT changes need Administrators-group rights.
- Doctor check should resolve a live `*.k8s.test` name through the **system path**
  (`Resolve-DnsName` without `-Server`, or the `dnsapi`/`res_query` equivalent) — not just
  confirm the NRPT rule object exists via `Get-DnsClientNrptRule` — because a GPO-level
  NRPT rule silently wins even when the local rule is present and looks correct. This
  mirrors the macOS recommendation already written up in
  `corporate-lockdown-analysis.md` §4 ("doctor should resolve a `*.k8s.test` name via the
  system resolver... not just probe the socket").
- Docs for workshop users should call out: (1) if resolution fails only in the browser,
  check the browser's own Secure DNS / DoH setting first (Chrome/Edge `Settings → Privacy
  and security → Security → Use secure DNS`, Firefox `about:preferences` DoH) before
  assuming talos-box is broken; (2) on a domain-joined corporate machine, a working `nslookup`/
  `Resolve-DnsName` result but a browser failure points at browser DoH, while an
  `Resolve-DnsName` failure despite the NRPT rule being present (`Get-DnsClientNrptRule`)
  points at a GPO-level NRPT override — different fixes, different owners (user vs. IT).

## Alternatives considered (rejected)

- **Hosts file**: no wildcard support (see above) — disqualifying on its own, independent
  of the GPO-override risk NRPT carries.
- **Local DNS proxy as system-wide resolver**: matches macOS's `/etc/resolver` scenario
  worst, not best — it discards the one advantage NRPT has (being a first-class, narrowly
  scoped OS mechanism) in exchange for fighting corporate DNS-security agents for the
  primary-resolver slot, a fight documented (Umbrella-vs-ZPA) to already happen *between
  legitimate vendor tools* and resolved by falling back to... an NRPT rule. There's no
  scenario surfaced in this research where the local-proxy-as-primary-resolver approach
  wins over NRPT on Windows.

## Open questions

1. **Can talos-box's embedded resolver bind to port 53 on Windows guests/host without
   colliding with the Windows DNS Client service (`Dnscache`) or another local resolver?**
   Not researched here — this is a Windows-networking-stack question (does `Dnscache`
   itself bind :53 loopback, the way `mDNSResponder`/`systemd-resolved` sometimes do on
   other platforms?), separate from the NRPT policy question this ticket covers.
2. **No Microsoft doc was found that states outright whether `-NameServers` accepts a
   port suffix.** The "IP-only" conclusion here is inferred from (a) absence of documented
   format vs. the explicitly-documented `-DAProxyServerName` format, and (b) the general
   "Windows DNS uses port 53" statement — worth an empirical test
   (`Add-DnsClientNrptRule -Namespace test -NameServers "127.0.0.1:5399"`) on a real
   Windows box before committing to it in code, since a hard failure or silent truncation
   at that call site would need to be handled either way.
3. **Whether talos-box can/should proactively re-assert its own NRPT rule** the way the
   macOS hardening list proposes periodic re-assertion of the resolver file
   (`corporate-lockdown-analysis.md` recommendation #6) — plausible given NRPT rules are
   just `Add-DnsClientNrptRule` calls, but interacting with a *domain* GPO's NRPT (rather
   than another local mechanism getting reset) is a different failure mode: re-asserting
   the local rule doesn't help since the GPO rule already wins by design, not by timing.
   Needs its own investigation into whether OU-level exemption/allowlisting is a
   realistic ask for IT depts, which is an organizational question outside this ticket's
   scope.
4. **Whether Windows' own DNS Client service (`Dnscache`) caches negative (NXDOMAIN)
   answers for `.test` names in a way that would make an NRPT rule added after a failed
   lookup ineffective until cache flush** (`ipconfig /flushdns`) — not covered by the
   sources found; worth a doctor-hint if empirically confirmed.
