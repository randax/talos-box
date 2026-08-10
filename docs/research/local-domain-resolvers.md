# Host resolver behavior for per-cluster domains

Research notes for [#116](https://github.com/randax/talos-box/issues/116), feeding the
per-cluster-domain validation policy and resolver lifecycle work ([#115](https://github.com/randax/talos-box/issues/115)).
All claims cite primary sources (man pages, RFCs, ICANN, first-party vendor docs).

## 1. macOS `/etc/resolver/<name>` semantics

Source of truth: `resolver(5)` man page (macOS; mirror: <https://www.manpagez.com/man/5/resolver/>).

- **The filename is the domain selector.** Per `resolver(5)`, "the file name is used as the
  domain name". The `domain` key inside the file is only needed when a resolver client
  cannot be expressed by a filename — notably when multiple resolver configurations exist
  for the *same* domain (disambiguated by `search_order`). Multi-label filenames work:
  `/etc/resolver/demo.k8s.test` scopes the resolver to `demo.k8s.test` exactly as
  `domain demo.k8s.test` would.
- **Longest-suffix match.** The system picks the resolver configuration with "the maximum
  number of matching domain components" — a query for `x.a.b.c` matches a client for
  `a.b.c` over one for `b.c` (`resolver(5)`). So per-cluster files
  (`/etc/resolver/demo.k8s.test`, `/etc/resolver/ci.k8s.test`) coexist cleanly, and a
  more-specific file shadows a shared-suffix file for its subtree.
- **`search_order`** only orders multiple clients that share the *same* domain ("queries
  will be sent to these clients in order by ascending search_order value", `resolver(5)`).
  It is not a cross-domain priority knob; talos-box does not need it as long as each
  cluster gets a unique domain.
- **Supported keys** (all from `resolver(5)`): `nameserver` (with optional `.port`),
  `port`, `domain`, `search` (max 6 domains), `search_order`, `sortlist`, `timeout`,
  `options` (`debug`, `timeout:n`, `ndots:n`). Non-53 ports (talos-box uses 5399) are
  supported via `port` or `nameserver 127.0.0.1.5399`-style syntax.
- **Pickup/caching:** `resolver(5)` itself documents no refresh semantics. In practice the
  configuration is generally noticed promptly, but the widely used and Apple-adjacent
  convention (used verbatim in minikube's ingress-dns docs,
  <https://github.com/kubernetes/minikube/blob/master/site/content/en/docs/handbook/addons/ingress-dns.md>)
  is to nudge the daemon after writing a file:
  `sudo killall -HUP mDNSResponder` (optionally `dscacheutil -flushcache`). The minikube
  docs note that on Big Sur+ the reliable reload is
  `sudo launchctl {enable,disable} system/com.apple.mDNSResponder.reloaded`. talos-box
  should send the HUP after creating/removing a resolver file rather than assuming
  instant pickup, and tell users to check `scutil --dns` (the resolver files appear there
  as scoped "resolver #N" entries; `dig`/`nslookup` do *not* use them, so `scutil --dns`
  plus `dscacheutil -q host` is the correct verification path).
- **The `.local` pitfall:** Apple reserves `.local` for Bonjour/mDNS per RFC 6762. Apple's
  own support doc "Apple devices might not open your internal network's '.local' domain"
  (<https://support.apple.com/en-us/101903>) states that queries for `.local` names are
  routed to the mDNS resolver instead of unicast DNS, and recommends internal networks
  avoid `.local` (and all IANA special-use names) entirely. A user-chosen
  `<cluster>.local` domain would collide with Bonjour and resolve unreliably (multi-label
  `.local` names typically fail or time out via mDNS). OrbStack makes it work only by
  *deliberately* answering mDNS for `*.orb.local` — a very different mechanism from an
  `/etc/resolver` file, and not one talos-box implements. Verdict: reject `.local`.
- **Other special-cased names on macOS:** `localhost` is synthesized, and mDNSResponder
  also handles reverse zones for link-local space; nothing else is special-cased at the
  `/etc/resolver` layer, but everything in the IANA special-use registry (RFC 6761,
  below) should be treated per its registry semantics.
- **DoH / filtering caveats:** Anything that bypasses the system resolver bypasses
  `/etc/resolver`. Firefox's DoH ("TRR") "bypasses system DNS", though in its default
  TRR-first mode it falls back to the native resolver when DoH resolution fails, and
  ships `localhost,local` as built-in exclusions
  (<https://wiki.mozilla.org/Trusted_Recursive_Resolver>) — so a made-up TLD usually
  works via NXDOMAIN fallback, but adds latency and is not guaranteed. Chrome only
  auto-upgrades to DoH when the system's configured resolver is a known DoH provider
  (<https://www.chromium.org/developers/dns-over-https/>), which preserves scoped
  resolvers in the common case. VPN/security agents (e.g. GlobalProtect-style DNS
  proxies) that install their own resolver rank can also shadow scoped resolvers —
  worth a `doctor` check rather than a hard guarantee.

## 2. systemd-resolved route-only domains (`~domain`)

Sources: `resolved.conf(5)` (<https://man7.org/linux/man-pages/man5/resolved.conf.5.html>),
`systemd-resolved.service(8)` "Protocols and Routing"
(<https://man7.org/linux/man-pages/man8/systemd-resolved.service.8.html>).

- **Semantics:** `Domains=` entries prefixed with `~` are pure routing domains: they
  direct queries under that suffix to the link's DNS servers without acting as a search
  suffix (`resolved.conf(5)`). Multi-label routing domains (e.g. `~demo.k8s.test`) are
  fully supported.
- **No documented limit** on the number of routing domains per link — unlike search
  domains, which inherit classic resolver limits only when written to `resolv.conf`.
  Neither man page states a cap on `~` domains; adding one per cluster is fine.
- **Precedence is longest-suffix (most labels) wins:** the "best matching" routing domain
  is "the matching one with the most labels", and the query goes to all DNS servers of
  all links carrying that best-match domain, in parallel, first answer wins
  (`systemd-resolved.service(8)`). A `~demo.k8s.test` on the talos-box interface
  therefore beats global DNS (which only gets the query via the default-route fallback
  when *no* routing domain matches). If the same name is also resolvable publicly, the
  scoped link wins deterministically — good for us, and the root of the split-horizon
  confusion when users pick real domains (section 3).
- **`~.`** routes *all* domains to a link's servers, effectively making it the preferred
  default route for DNS (`resolved.conf(5)`). talos-box must never install `~.`; it would
  hijack the whole host's DNS.
- **`.local`:** systemd-resolved reserves `.local` for MulticastDNS; "lookups for domains
  with the '.local' suffix are not routed to DNS servers, unless the domain is specified
  explicitly as routing or search domain" (`systemd-resolved.service(8)`). So `~foo.local`
  *can* be forced through, but it fights LLMNR/mDNS expectations — same verdict as macOS:
  reject.
- **Single-label names** are never sent to unicast DNS by default
  (`systemd-resolved.service(8)`), so a bare single-label cluster domain (`~demo`) is a
  bad idea; require at least two labels.
- **DNSSEC:** default is `allow-downgrade` (`resolved.conf(5)`), and many distros ship
  `DNSSEC=no`. Made-up TLDs would fail validation under strict DNSSEC (the root zone
  provably denies them); with the shipped defaults this is a non-issue, but a `doctor`
  check for `DNSSEC=yes` hosts is cheap insurance.

## 3. Which TLDs are safe

### Standards / registry ground truth

- **RFC 2606 / BCP 32** (<https://www.rfc-editor.org/rfc/rfc2606>) reserves `.test`,
  `.example`, `.invalid`, `.localhost`. `.test` is explicitly "recommended for use in
  testing of current or new DNS related code".
- **RFC 6761** (<https://www.rfc-editor.org/rfc/rfc6761>) turns these into the IANA
  Special-Use Domain Names registry
  (<https://www.iana.org/assignments/special-use-domain-names/>) with per-name resolver
  rules: `.invalid` must fail immediately, `.localhost` must loop back, `.test` may be
  handled specially by local resolvers — exactly the license talos-box relies on.
- **RFC 6762 §3 + Appendix G** (<https://www.rfc-editor.org/rfc/rfc6762>) reserves
  `.local` for mDNS (see Apple doc above).
- **RFC 8375** (<https://www.rfc-editor.org/rfc/rfc8375>) designates `home.arpa.` for
  residential home networks — usable locally, but semantically "home router" territory
  and resolvers may special-case it.
- **`.internal`:** ICANN Board resolution 2024.07.29.06 (29 July 2024,
  <https://www.icann.org/en/board-activities-and-meetings/materials/approved-resolutions-special-meeting-of-the-icann-board-29-07-2024-en>)
  permanently reserves `.internal` from root-zone delegation for private-use
  applications, per SSAC's SAC113 recommendation. It is the DNS analogue of RFC 1918
  address space and safe for local use, though (unlike RFC 6761 names) resolvers have no
  mandated special handling for it.

### What comparable tools chose

- **mkcert** exists precisely because "using certificates from real certificate
  authorities (CAs) for development can be dangerous or impossible (for hosts like
  `example.test`, `localhost` or `127.0.0.1`)" (<https://github.com/FiloSottile/mkcert>)
  — i.e. its README's canonical example of a dev domain is under `.test`, paired with a
  local CA because no public CA can issue for fake TLDs.
- **Tailscale MagicDNS** uses `<tailnet>.ts.net`, a real registered domain
  (<https://tailscale.com/kb/1081/magicdns>), specifically so nodes can get *public*
  Let's Encrypt certificates via DNS-01 challenges
  (<https://tailscale.com/blog/tls-certs>). That trade only works if you own the domain
  and run the issuance infrastructure — not applicable to talos-box users' arbitrary
  choices.
- **OrbStack** uses `*.orb.local` and restricts custom domains to `.local`
  (<https://docs.orbstack.dev/docker/domains>: "Only domains under the `.local` TLD are
  supported at this time") — a deliberate bet on answering mDNS itself rather than
  configuring unicast resolvers. It proves `.local` is *makeable*, but only with an mDNS
  responder, which talos-box's resolver-file/route-domain architecture does not have.
- **minikube ingress-dns** documents the same `/etc/resolver` + `.test` pattern talos-box
  uses today
  (<https://github.com/kubernetes/minikube/blob/master/site/content/en/docs/handbook/addons/ingress-dns.md>).
  kind and Docker Desktop do no host-side wildcard DNS at all (Docker Desktop's
  `host.docker.internal` is container-side only).

### The hijack question (user supplies a real registrable domain)

- **Resolution hijack:** on both platforms the scoped resolver wins for its suffix
  (longest-suffix match, sections 1–2), so pointing a cluster at `mycompany.com` silently
  shadows the real domain for the whole host — split-horizon confusion that outlives
  debugging sessions if teardown ever fails.
- **TLS/HSTS:** Google Registry TLDs (`.dev`, `.app`, `.page`, …) are HSTS-preloaded at
  the TLD level — browsers force HTTPS with a valid, publicly trusted certificate and
  offer no plaintext fallback (Wikipedia/.dev citing Google Registry policy,
  <https://en.wikipedia.org/wiki/.dev>; <https://kb.porkbun.com/article/96-hsts-preload-and-google-registry>).
  No public CA can issue for a hijacked name the user doesn't control, so `.dev` clusters
  break in browsers unless the user runs mkcert-style local CA trust.
- **DoH:** browser DoH resolves real domains at the public resolver, returning the *real*
  records and bypassing the local hijack entirely (Firefox TRR-first only falls back on
  failure — a successfully resolving public name never falls back;
  <https://wiki.mozilla.org/Trusted_Recursive_Resolver>).

## Implications for talos-box

A validation policy for the per-cluster domain can safely be:

1. **Default and recommend `<cluster>.k8s.test`** (or any suffix under `.test`): RFC
   6761 grants local resolvers explicit license, minikube sets precedent, and both host
   mechanisms handle multi-label suffixes with longest-match precedence — so per-cluster
   subdomains of a shared suffix and fully distinct domains both work with one
   `/etc/resolver/<domain>` file or one `~<domain>` route-only entry per cluster.
2. **Allow without warning:** anything under `.test`, `.example`, `.invalid` is reserved —
   but only `.test` behaves usefully (`.invalid` must fail, `.localhost` must loop back),
   so practically: allow `.test`; also allow `.internal` (ICANN 2024 reservation) and
   `home.arpa` (RFC 8375), perhaps with an informational note that resolvers have no
   special obligations for `.internal`.
3. **Reject:** `.local` (mDNS collision on both OSes — Apple 101903, RFC 6762,
   systemd-resolved's MulticastDNS reservation), `.localhost`, `.invalid`, single-label
   domains (systemd-resolved won't route them to unicast DNS), and the bare `~.`
   equivalent (empty/root domain).
4. **Warn-and-confirm on any other TLD** (i.e. potentially registrable or delegated
   names): explain the split-horizon shadowing, HSTS-preloaded-TLD breakage (`.dev` et
   al.), and DoH bypass. Don't hard-block — power users with real internal domains they
   own are a legitimate case — but make the hijack consequences explicit.
5. **Lifecycle mechanics:** on macOS, write one `/etc/resolver/<domain>` file per cluster
   and HUP mDNSResponder on create/delete; verify via `scutil --dns`, never `dig`. On
   Linux, one `~<domain>` route-only entry per cluster link; no documented count limit.
   `doctor` should flag DoH-only browsers/agents and `DNSSEC=yes` resolved configs as
   environments where scoped resolution may be bypassed.
