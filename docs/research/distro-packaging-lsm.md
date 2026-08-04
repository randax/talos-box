# Research: Linux distro packaging and LSM requirements

Resolves [#75](https://github.com/randax/talos-box/issues/75) (part of the Linux port map, [#71](https://github.com/randax/talos-box/issues/71)).

**Question.** What do deb, rpm, and AUR packaging pipelines require for a Go binary plus a
privileged systemd helper (CAP_NET_ADMIN; creates tap devices, bridges, and nftables rules)?
Do we need to ship SELinux (Fedora) or AppArmor (Ubuntu) policy modules, and what do
comparable tools do?

Researched against primary sources: Debian Policy Manual and debhelper manpages, Fedora
Packaging Guidelines (upstream `forge.fedoraproject.org` sources) and `go-rpm-macros`, ArchWiki
and the Arch Developer Manual, nfpm/GoReleaser docs **and source**, systemd manpages and
`src/core/`, Linux kernel source (`tun.c`, `rtnetlink.c`, `nfnetlink.c`, `af_netlink.c`),
`fedora-selinux/selinux-policy`, the AppArmor/Ubuntu docs, and the packaging trees of libvirt,
incus, moby/docker, container-selinux, and tailscale.

---

## TL;DR

1. **No LSM policy module is required for the software to work.** Ubuntu confines only what has
   an AppArmor profile — no profile means unconfined means it works. Fedora auto-transitions
   `init_t` → `unconfined_service_t` for anything labelled `bin_t`, which grants every netlink,
   tun, and capability permission we need. **The only load-bearing SELinux requirement is
   installing to `/usr/bin` and `/usr/libexec` (both `bin_t`) rather than `/opt` (`usr_t`).**
2. **Do not ship file capabilities. Use `AmbientCapabilities=CAP_NET_ADMIN` in the unit.** This
   single decision removes a blocker from every packaging system at once (dpkg cannot carry
   xattrs at all; nfpm has no caps field in any packager; Arch needs an `.install` scriptlet)
   *and* is more secure — a `setcap` binary is executable by any local user, bypassing our
   socket and `SO_PEERCRED` gate entirely.
3. **CAP_NET_ADMIN alone is sufficient** for tap creation, bridge creation, and nftables — but
   only in the **initial user namespace**. `PrivateUsers=`/userns is a hard blocker, not a
   tunable.
4. **Ship unofficial packages first**: GoReleaser + nfpm for `.deb`/`.rpm`, COPR for Fedora/EL,
   an AUR `-bin` package. Debian-archive inclusion would require de-vendoring every transitive
   Go dependency into separate source packages; Fedora proper now *requires* vendoring but
   demands a per-dependency license audit on every rebase.
5. **Nobody comparable ships a static SELinux module from their own repo.** Moby tried in 2016
   and deleted it within a year, handing it to `container-selinux`. Tailscale — our closest
   analogue (Go binary, root, tun, nftables) — ships **zero** LSM policy and no hardening
   directives at all.

---

## 1. Packaging mechanics

### 1.1 Debian

**Paths.** Policy §10.1 is now mandatory and enforced: packages must not install to
`/bin/*`, `/lib/*`, `/lib*/*`, `/sbin/*` — installing to an aliased path can make dpkg delete
another package's files ([Policy ch.10](https://www.debian.org/doc/debian-policy/ch-files.html),
[DEP-17](https://dep-team.pages.debian.net/deps/dep17/)). Note that the `/usr/sbin` → `/usr/bin`
merge has *not* happened and is not planned
([wiki.debian.org/UsrMerge](https://wiki.debian.org/UsrMerge): "*No, there are no plans to do
that*"). Debian would prefer `/usr/libexec/tbx/tbx-helper` (FHS 3.0 §4.7), but the
[Arch package guidelines](https://wiki.archlinux.org/title/Arch_package_guidelines) say
`/usr/libexec/` "should be avoided — use `/usr/lib/$pkgname/`". Standardising on
**`/usr/lib/tbx/tbx-helper`** satisfies both.

**Systemd units go in `/usr/lib/systemd/system/`**, changed in debhelper 13.4 (Debian #987989);
[`dh_installsystemd(1)`](https://manpages.debian.org/unstable/debhelper/dh_installsystemd.1.en.html)
confirms. The `wiki.debian.org/Teams/pkg-systemd/Packaging` page still says `/lib/systemd/system`
and `dh --with systemd` — it is stale.

**Maintainer scripts.** Policy ch.6 requires idempotence and `set -e`. Critically, debhelper
generates calls to `deb-systemd-helper`, **not** `systemctl enable`; per
[deb-systemd-helper(1p)](https://manpages.debian.org/unstable/init-system-helpers/deb-systemd-helper.1p.en.html)
the enable "*will only be performed once (when first installing the package)*" and is
state-tracked, so a user's later `systemctl disable` survives upgrades. Never hand-roll
`systemctl enable` in postinst. `deb-systemd-invoke` also honours `/usr/sbin/policy-rc.d`
(chroots/containers), and generated snippets are guarded by
`[ -z "$DPKG_ROOT" ] && [ -d /run/systemd/system ]`.

```make
override_dh_installsystemd:
	dh_installsystemd --name=tbx-helper.socket
	dh_installsystemd --no-enable --no-start --name=tbx-helper.service
```

Consider `--no-stop-on-upgrade` so taps and bridges are not torn down mid-upgrade.
`dh_installsystemd` is not idempotent — run `dh_prep` between invocations.

**File capabilities: dpkg cannot do it.** [Debian #970827](https://bugs.debian.org/cgi-bin/bugreport.cgi?bug=970827)
is open and unfixed; there is no `debian/*.caps` mechanism. The canonical workaround is
`setcap` in postinst (see `iputils-arping.postinst`, which needs `Depends: libcap2-bin` and
`dpkg-divert --truename`), but it leaves a window during unpack where the binary has neither
setuid nor caps, and fails outright in unprivileged LXC or on xattr-hostile filesystems.
**Use `AmbientCapabilities=` instead.**

**Users and groups.** Policy §9.2 forbids modifying `/etc/passwd` directly; use
`adduser --system` (dynamic 100–999), new system usernames should start with an underscore,
home is `/nonexistent`. `dh-sysuser`/sysusers.d is **only in experimental** — Debian is the one
distro where the sysusers.d file we ship everywhere else does not work. Needs
`addgroup --system _tbx` in postinst plus `Depends: adduser`. A group alone (for `SocketGroup=`)
is probably sufficient for us.

**Go vendoring.** Policy §4.13 forbids convenience copies. Archive inclusion means deleting
`vendor/` and packaging every transitive dependency as a `golang-*-dev` source package;
`dh-golang` has [no supported "use my vendor dir" mode](https://manpages.debian.org/unstable/dh-golang/Debian::Debhelper::Buildsystem::golang.3pm.en.html).
Realistically 40–120 new source packages, each needing ITP + NEW review, and we would get
Debian's dependency versions rather than our `go.sum` pins — against a ~2-year freeze cycle for
a tool tracking Talos releases.

**Recommendation:** unofficial `.deb`, but keep `debhelper-compat (= 13)` and skip `dh-golang`.
We keep debhelper's battle-tested autoscripts (where the correctness risk actually lives) and
pay none of the archive tax. Everything dpkg enforces still binds us: maintainer-script
correctness, the /usr-merge path ban, `/usr/lib/systemd/system` + `deb-systemd-helper`,
conffile semantics (Policy §10.7: config in `/etc`, local changes survive upgrade, maintainer
scripts must not modify conffiles), and declared `Depends`.

### 1.2 Fedora / RHEL

**Vendoring is now mandatory, in our favour.** Per
[Golang.adoc](https://docs.fedoraproject.org/en-US/packaging-guidelines/Golang/) and
[Changes/GolangPackagesVendoredByDefault](https://fedoraproject.org/wiki/Changes/GolangPackagesVendoredByDefault):
"*new Go packages MUST be built with vendored dependencies… New Golang library packages are no
longer allowed.*" `bundled(golang(...))` Provides are auto-generated from `vendor/modules.txt`.
The real cost is **licensing**: every vendored module must have a license file, the `License:`
tag is a cumulative SPDX expression, and `go_vendor_license report` must be re-run on every
dependency bump.

**`%gobuild` gotchas** (verified in
[`macros.go-compilers-golang`](https://gitlab.com/fedora/sigs/go/go-rpm-macros/-/blob/main/rpm/macros.d/macros.go-compilers-golang)):

- `-buildmode pie` is set; do not `%undefine _hardened_build`. The guidelines
  [forbid disabling PIE](https://docs.fedoraproject.org/en-US/packaging-guidelines/#_pie) for
  packages with capabilities or that run as root — so this matters to us specifically.
- **`-trimpath` is NOT set anywhere.** Add it.
- **`GO111MODULE` defaults to `off`.** Must set `%global gomodulesmode GO111MODULE=on` or the
  vendor dir is ignored and `go:embed` breaks, with a confusing failure mode.
- Use `export GO_LDFLAGS=` for version stamping; `$LDFLAGS` is a deprecated alias.

**Systemd scriptlets** (`BuildRequires: systemd-rpm-macros`):

```rpm-spec
%post
%systemd_post tbx-helper.service tbx-helper.socket
%preun
%systemd_preun tbx-helper.service tbx-helper.socket
%postun
%systemd_postun_with_restart tbx-helper.service
```

Units live in `%{_unitdir}` (`/usr/lib/systemd/system`), must be 0644, must not be `%config`.
**Do not add `%{?systemd_requires}`** —
[Systemd.adoc](https://docs.fedoraproject.org/en-US/packaging-guidelines/Systemd/) states those
dependencies are not required for these macros. Units are disabled by default and enabling them
by default requires a FESCo preset ticket whose criteria include "must not alter other
services", which we arguably fail by definition. Design for `systemctl enable --now`, or
socket-activate (a local AF_UNIX socket is not a network listener and needs no approval).

**File capabilities** are expressible here and only here:

```rpm-spec
%caps(cap_net_admin=ep) %attr(0750,root,tbx) %{_libexecdir}/tbx/tbx-helper
```

Do not run `setcap` in `%install` — it will not work in mock. `rpm -V` reports `P` for
differing caps. [rpm-spec(5)](https://rpm-software-management.github.io/rpm/man/rpm-spec.5.html)
warns that many filesystems (NFS) do not support capabilities, causing install-time failures.
Fedora has **no policy** on file caps — one human reviewer's judgement decides, and
`cap_net_admin=ep` on a user-executable binary will (correctly) draw fire. Another reason to
prefer `AmbientCapabilities=`.

**Users/groups**: ship `%{_sysusersdir}/tbx.conf` with `%{?sysusers_requires_compat}` and
`%sysusers_create_compat` in `%pre`. Verified working on F42+, EL9 and EL10 from **one spec**
(`go-rpm-macros` 3.8.x is in CentOS Stream 9/10 AppStream; `go-vendor-tools` 0.12.0 in
EPEL 9/10). `%attr(0750,root,tbx)` auto-generates `Requires: group(tbx)`.

**firewalld.** Verified from [firewalld source](https://github.com/firewalld/firewalld/blob/master/src/firewall/core/nftables.py):
firewalld owns only the `firewalld`, `firewalld_policy_drop`, `firewalld_probe` tables and never
issues `flush ruleset` — so `table inet tbx` survives reload and restart. But surviving is not
the same as working: every table with a chain on a hook is evaluated, and any `drop` is final.
The relevant knob is `StrictForwardPorts` in
[firewalld.conf(5)](https://firewalld.org/documentation/man-pages/firewalld.conf.html), which
defaults to `no` (so we work out of the box), but **`StrictForwardPorts=yes` silently breaks
port forwarding** — worth a `tbx doctor` check. Never write into `table inet firewalld`; never
use direct/passthrough. We may ship `/usr/lib/firewalld/services/tbx.xml` (installing it enables
nothing).

**COPR** is the realistic channel: its
[Terms of Use](https://docs.pagure.org/copr.copr/user_documentation.html) state "*You do not need
to comply with Packaging Guidelines*", packages get a per-project GPG key, and builds run in
mock so `mock -r fedora-42-x86_64 --rebuild` reproduces exactly. Note `repo_gpgcheck=0` —
metadata is unsigned, weaker than Fedora proper.

### 1.3 Arch / AUR

Binaries in `/usr/bin`, config in `/etc/<pkg>`, private helpers in **`/usr/lib/$pkgname/`**
(`/usr/libexec` is discouraged), licenses in `/usr/share/licenses/<pkg>`; `license` must be SPDX;
quote `"$pkgdir"`.

The [AUR submission guidelines](https://wiki.archlinux.org/title/AUR_submission_guidelines)
require the **`-bin` suffix** for prebuilt deliverables, forbid `replaces=` (use `conflicts=`
plus versioned `provides=("tbx=$pkgver")`), and require both `PKGBUILD` and `.SRCINFO` to be
committed (push is rejected without `.SRCINFO`).

**Pacman handles systemd for us — do not write scriptlets.** The `systemd` package ships pacman
hooks on `usr/lib/systemd/system/*`, `usr/lib/sysusers.d/*.conf`, and `usr/lib/tmpfiles.d/*.conf`,
dispatched through `systemd-hook`, which correctly short-circuits in chroots
(`systemd-detect-virt --chroot`). So: ship `/usr/lib/sysusers.d/tbx.conf` and pacman creates the
user; never call `systemctl daemon-reload` yourself (it breaks inside `pacstrap`). Arch never
enables units.

`.install` gotchas: each function runs **chrooted inside the pacman install root** (hence
relative paths, e.g. `usr/bin/fping` in `fping.install`), and "*do not end the script with
`exit`*". `backup=()` entries are relative, no wildcards.

Build flags, verbatim from the [Arch Developer Manual](https://manual.archlinux.page/package-guidelines/go/):

```bash
export GOFLAGS="-buildmode=pie -trimpath -ldflags=-linkmode=external -mod=readonly -modcacherw"
```

Note `-linkmode=external` implies cgo — with `CGO_ENABLED=0` we lose external-link hardening but
keep pie and trimpath. The [Capabilities](https://wiki.archlinux.org/title/Capabilities) wiki page
says outright that `AmbientCapabilities`/`CapabilityBoundingSet` is "*much more safe than setting
capabilities on binaries*".

### 1.4 nfpm / GoReleaser — capabilities and gaps

**`contents:` types** (from [`files/files.go`](https://github.com/goreleaser/nfpm/blob/main/files/files.go)):
`dir`, `tree`, `symlink`, `config`, `config|noreplace`, `config|missingok`, `config|tree`,
`ghost`, `doc`, `license`, `readme`, `debian changelog`. `config|noreplace` maps to
`%config(noreplace)` on rpm, a conffile on deb, a `backup =` line in `.PKGINFO` on Arch, and
nothing meaningful on apk. `ghost` is RPM-only.

**Gaps that bite a privileged helper:**

1. **No file capabilities, in any packager.** Confirmed by source inspection: `ContentFileInfo`
   has no xattr/caps field, and grepping `deb/deb.go`, `rpm/rpm.go`, `arch/arch.go` for
   `xattr`/`capabilit` returns nothing. RPM supports `%caps` natively but nfpm's RPM writer
   (`google/rpmpack`) exposes no such field — **unreachable through nfpm even on rpm.**
2. **No systemd scriptlets** — no `%systemd_post` equivalent, no `dh_installsystemd` equivalent.
   [nfpm Tips](https://nfpm.goreleaser.com/docs/tips/) hands you a hand-rolled portable script.
   You must branch on the per-format argument divergence yourself: deb passes `configure` + old
   version, rpm passes `1`/`2`, **apk passes no arguments at all**.
3. **Upgrade ordering trap**, quoted from nfpm's own docs: `pretrans` (new) → `preinstall` (new)
   → `postinstall` (new) → **`preremove` (old)** → **`postremove` (old)** → `posttrans` (new).
   The *old* package's remove scripts run *after* the new one's `postinstall`. A naive
   `postremove` that stops `tbx-helper.service` kills the service we just started.
4. **No sysusers/tmpfiles automation outside Arch** — hand-write idempotent `useradd --system`
   for deb/rpm/apk.
5. **Arch: the generic `postinstall` becomes `post_install` in `.INSTALL`** — an unconditional
   `daemon-reload` there breaks `pacstrap`/containers.
6. nfpm's `archlinux` packager is the least mature: no signing, writes `.PKGINFO`/`.INSTALL`
   directly without makepkg, so no namcap validation. Use a real PKGBUILD (GoReleaser `aurs:`)
   for AUR.
7. **nfpm does not template** — "*templating is not and will not be supported*"; env expansion
   only. GoReleaser adds path templating; full content templating is Pro-only.

Signing: deb via `dpkg-sig`/`debsign` (docs warn dpkg-sig is unsupported on newer Debian);
rpm via PGP; **apk needs an RSA PEM key, not PGP**; **archlinux and ipk have no signature support
at all**.

GoReleaser adds `nfpms:`, `aurs:` (enforces the `-bin` suffix; `private_key` must not be
password-protected; the default `package:` block installs only one binary, so override it),
`aur_sources:` (v2.5+, whose documented `build:` already matches the Arch Go guidelines), plus
homebrew/nix/scoop/winget, checksums, SBOMs, attestations. Note GoReleaser's `.SRCINFO` template
omits `backup` and `install` — the PKGBUILD is correct so `makepkg` behaves; only AUR-displayed
metadata is incomplete.

---

## 2. The privileged helper: systemd unit design

### 2.1 Recommended unit

```ini
[Unit]
Description=talos-box privileged network helper
Requires=tbx-helper.socket
After=tbx-helper.socket
Wants=modprobe@tun.service modprobe@bridge.service modprobe@nf_tables.service
After=modprobe@tun.service modprobe@bridge.service modprobe@nf_tables.service

[Service]
Type=notify
ExecStart=/usr/lib/tbx/tbx-helper
User=tbx-helper
Group=tbx-helper

CapabilityBoundingSet=CAP_NET_ADMIN
AmbientCapabilities=CAP_NET_ADMIN
NoNewPrivileges=yes

DevicePolicy=closed
DeviceAllow=/dev/net/tun rw
PrivateDevices=no          # MUST stay off
PrivateNetwork=no          # MUST stay off
PrivateUsers=no            # MUST stay off

RestrictAddressFamilies=AF_UNIX AF_NETLINK
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallFilter=~@mount
SystemCallErrorNumber=EPERM

ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
RuntimeDirectory=tbx-helper
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectKernelTunables=no   # see table below
ProtectControlGroups=yes
ProtectProc=invisible
RestrictNamespaces=yes     # drop to `net` if we ever setns()
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
UMask=0077
```

```ini
# tbx-helper.socket
[Socket]
ListenStream=/run/tbx/helper.sock
SocketMode=0660
SocketUser=root
SocketGroup=tbx
Accept=no
RemoveOnStop=yes

[Install]
WantedBy=sockets.target
```

`SocketMode=` **defaults to 0666** — a world-writable control socket into a CAP_NET_ADMIN
process. Setting it explicitly is not optional.

### 2.2 Why ambient capabilities, not setcap

Per [capabilities(7)](https://git.kernel.org/pub/scm/docs/man-pages/man-pages.git/plain/man/man7/capabilities.7),
`P'(permitted) = … | P'(ambient)` and `P'(effective) = F(effective) ? P'(permitted) : P'(ambient)`.
Crucially: "*Executing a program that … has any file capabilities set will clear the ambient
set.*" **So setcap'ing the binary silently zeroes the ambient set and the helper becomes
unprivileged.** The two mechanisms are mutually exclusive, and only one of them can be expressed
in dpkg/nfpm.

`NoNewPrivileges=yes` is compatible with ambient caps (systemd raises them in the child before
`execve`) but **incompatible with file caps** — per
[execve(2)](https://git.kernel.org/pub/scm/docs/man-pages/man-pages.git/plain/man/man2/execve.2),
under `no_new_privs` "*the capabilities of the program file … are also ignored*". NNP defaults to
false and is *not* implicitly enabled for system units when PID 1 has `CAP_SYS_ADMIN`. Set it
explicitly.

`CapabilityBoundingSet=` masks ambient: omitting `CAP_NET_ADMIN` there while listing it in
`AmbientCapabilities=` makes the unit fail to start with `EXIT_CAPABILITIES`. List it in both.

### 2.3 Is CAP_NET_ADMIN sufficient? Yes, with two caveats

Verified against kernel source:

- **tap creation** — [`drivers/net/tun.c`](https://raw.githubusercontent.com/torvalds/linux/master/drivers/net/tun.c)
  `tun_set_iff()`: `if (!ns_capable(net->user_ns, CAP_NET_ADMIN)) return -EPERM;`.
  `TUNSETPERSIST`/`TUNSETOWNER`/`TUNSETGROUP` carry **no additional** capability check — the gate
  was `TUNSETIFF`. Plus DAC on `/dev/net/tun`.
- **bridge create/enslave** — [`net/core/rtnetlink.c`](https://raw.githubusercontent.com/torvalds/linux/master/net/core/rtnetlink.c):
  one check, `netlink_net_capable(skb, CAP_NET_ADMIN)`, covering `RTM_NEWLINK`/`RTM_SETLINK`/
  `RTM_NEWADDR`/`RTM_NEWROUTE`.
- **nftables** — [`net/netfilter/nfnetlink.c`](https://raw.githubusercontent.com/torvalds/linux/master/net/netfilter/nfnetlink.c)
  `nfnetlink_rcv()`: `netlink_net_capable(skb, CAP_NET_ADMIN)` gates *every* NETLINK_NETFILTER
  message including batches. No CAP_SYS_ADMIN, no CAP_NET_RAW.

**Caveat 1 — namespace relativity.** Every check above resolves against the netns's owning user
namespace, and capabilities flow downward only
([user_namespaces(7)](https://man7.org/linux/man-pages/man7/user_namespaces.7.html)).
**CAP_NET_ADMIN in a child user namespace is worthless for host networking.** `PrivateUsers=`,
`UserNamespacePath=`, and `unshare -Ur` are hard blockers, not tunables.

**Caveat 2 — netlink fds cannot be delegated.** `__netlink_ns_capable()` in
[`af_netlink.c`](https://raw.githubusercontent.com/torvalds/linux/master/net/netlink/af_netlink.c)
requires the capability **both** at socket-open time and at every message send. Passing a
pre-opened netlink fd to `tbxd` confers nothing — that clause exists specifically to close that
hole. **Pass the tap fd instead**; its gate is at `TUNSETIFF`, already passed.

### 2.4 Silent-breakage table

| Risk | Symptom |
|---|---|
| `PrivateUsers=yes` / any userns | **Hard blocker.** All netlink + tuntap ops → `EPERM` against the host netns. |
| `PrivateNetwork=yes` | **Silent.** Bridges/taps/nft rules created successfully — in a throwaway netns that vanishes on stop. No error. |
| `ProtectKernelTunables=yes` | `EROFS` on `/proc/sys/net/ipv4/ip_forward`, `bridge-nf-call-iptables`, sysfs bridge knobs — deep in a setup path, not at startup. Move static sysctls to `sysctl.d(5)` if we want this on. |
| `PrivateDevices=yes` | `/dev/net/tun` absent. The node list is hardcoded in `namespace.c` `mount_private_dev()` (null, zero, full, random, urandom, tty) with no way to extend. |
| `DeviceAllow=/dev/net/tun` with `tun` unloaded at start | **Silent.** systemd.resource-control: "*device groups not resolvable then are not added to the allow list*". Unit starts fine; `open()` fails later. Fix via `modprobe@tun.service` ordering. |
| `RestrictAddressFamilies=` missing `AF_NETLINK` | rtnetlink (NETLINK_ROUTE) and nftables (NETLINK_NETFILTER) both dead. |
| `CapabilityBoundingSet=` omitting an ambient cap | Unit fails, `EXIT_CAPABILITIES`. (Loud — the good kind.) |
| `setcap` on the binary | Any local user execs it → CAP_NET_ADMIN, bypassing the socket entirely. Also voids NNP and the ambient set. |
| Default `SocketMode=0666` | World-writable control socket into a privileged helper. |
| Abstract (`@`) socket | No filesystem permissions, and netns-scoped. Use `/run/…`. |
| Hardened `/dev/net/tun` (0600 root:root) + non-root `User=` | `EACCES` before any capability check. CAP_NET_ADMIN does not override DAC. |
| `RestrictNamespaces=yes` + `setns()` | `EPERM`, *including* the zero-flags form. Narrow to `RestrictNamespaces=net`. |

`SystemCallFilter=@system-service` covers everything we need — verified in
[`seccomp-util.c`](https://raw.githubusercontent.com/systemd/systemd/main/src/shared/seccomp-util.c):
it includes `ioctl` (for `TUNSETIFF`) and `@network-io`. `ProtectKernelModules=yes` is safe;
kernel-side autoload via `request_module()` runs in a kernel thread unaffected by seccomp — what
it blocks is shelling out to `modprobe`, hence the `modprobe@*.service` ordering.

### 2.5 Peer authentication

Per [unix(7)](https://git.kernel.org/pub/scm/docs/man-pages/man-pages.git/plain/man/man7/unix.7),
`SO_PEERCRED` returns credentials "*in effect at the time of the call to connect(2), listen(2),
or socketpair(2)*" — kernel-snapshotted, unforgeable, nothing the peer opts into (unlike
`SCM_CREDENTIALS`, which a peer with `CAP_SETUID`/`CAP_SETGID`/`CAP_SYS_ADMIN` can set to
non-matching values). **Authorize on `ucred.uid`/`ucred.gid`; treat `ucred.pid` as advisory**
(PID reuse race). For process identity use `SO_PEERPIDFD` (Linux 6.5+, returns a pidfd) with a
fallback to `SO_PEERCRED` on `ENOPROTOOPT` — it is not yet documented in `unix(7)`/`socket(7)`.

Socket activation: `SD_LISTEN_FDS_START` = 3; pair `FileDescriptorName=control` with
`sd_listen_fds_with_names()`. `Accept=no` for a long-lived helper; the helper must not unlink
the socket.

---

## 3. SELinux on Fedora

### 3.1 Default posture: unconfined, and that is sufficient

Fedora's targeted policy confines a domain only if a module names it. With no module, the
outcome is decided entirely by **the label on our binary**, via one rule chain.
`policy/modules/system/init.te` calls `unconfined_server_domtrans(init_t)` inside an
`optional_policy` block
([init.te](https://github.com/fedora-selinux/selinux-policy/blob/rawhide/policy/modules/system/init.te)),
and that interface is literally
([unconfined.if:247](https://github.com/fedora-selinux/selinux-policy/blob/rawhide/policy/modules/system/unconfined.if)):

```
interface(`unconfined_server_domtrans',`
	gen_require(` type unconfined_service_t; ')
	corecmd_bin_domtrans($1, unconfined_service_t)
')
```

So **`init_t` executing a file labelled `bin_t` auto-transitions to `unconfined_service_t`**,
which `unconfined.te` (52 lines total) hands to `unconfined_domain()` →
`kernel_unconfined`/`corenet_unconfined`/`dev_unconfined`/`files_unconfined`/`fs_unconfined`.

**Consequence: `capability:net_admin`, `netlink_route_socket`, `netlink_netfilter_socket`,
`tun_socket`, and `chr_file` access to `/dev/net/tun` are all granted.** Nothing we do — tap,
bridge, nftables — is denied. SELinux does separately mediate CAP_NET_ADMIN via the `capability`
class, but only for *confined* domains.

Residual restrictions that still apply to unconfined domains: the `deny_execmem` /
`selinuxuser_execstack` / `selinuxuser_execheap` booleans still gate `process:execmem`/
`execstack`/`execheap`. Pure-Go binaries are fine; cgo with a JIT or a `dlopen`'d plugin might
not be. MCS constraints are also not bypassed.

### 3.2 What actually breaks: labelling

This is the real risk, and it is silent.

- `/usr/bin` and `/usr/libexec(/.*)?` → **`bin_t`**
  ([corecommands.fc](https://github.com/fedora-selinux/selinux-policy/blob/rawhide/policy/modules/kernel/corecommands.fc)).
  Both are fine for the helper.
- `/opt` and `/opt/.*` → **`usr_t`**, *not* `bin_t`
  ([files.fc](https://github.com/fedora-selinux/selinux-policy/blob/rawhide/policy/modules/kernel/files.fc)).
  Only `/opt/(.*/)?libexec(/.*)?` and `/opt/(.*/)?bin` get `bin_t`.

**If the binary is not `bin_t`, the transition does not fire and the daemon runs as `init_t`**,
which *is* confined. The result is a cascade of AVC denials on netlink, `/dev/net/tun`, and our
own state directory that look like random permission bugs, not a labelling problem. This is the
#1 silent failure mode.

Corollaries:

- Never install into `/opt/talos-box/bin` without a `.fc` rule. Use `/usr/bin` + `/usr/lib/tbx`.
- A `curl | tar -C /usr/local` installer produces the same breakage — files get the *creating
  process's* label, not the path default. Any tarball installer must `restorecon` afterwards.
- **Atomic-replace label loss is a code concern that applies to us today.** If we write a temp
  file and `rename()` it over a host config file (resolv.conf, an nftables include, anything in
  `/etc`), the new inode inherits the *parent directory's* label. This bit tailscale on RHEL 10
  — [tailscale#20149](https://github.com/tailscale/tailscale/issues/20149): resolv.conf inherited
  `etc_t` instead of `net_conf_t`, NetworkManager's later `unlink` was denied, DNS silently
  dropped. Fix with `setfscreatecon`/`matchpathcon` before the write, or `restorecon` after.
- **Unix socket labelling.** `/run/tbx/helper.sock` will be `var_run_t` with no policy. Fine
  while both peers are unconfined; it breaks the moment a *confined* third party needs to reach
  it — exactly [tailscale#5622](https://github.com/tailscale/tailscale/issues/5622), where
  `httpd_t` (Caddy) cannot open `tailscaled.sock` and the maintainers reject the `audit2allow`
  output as too broad. Still open. This is the strongest argument for eventually shipping a
  `.fc` + `.if` pair.

### 3.3 `nnp_transition` — only matters if we ever confine

`init_nnp_daemon_domain` is
([init.if:151](https://github.com/fedora-selinux/selinux-policy/blob/rawhide/policy/modules/system/init.if)):

```
allow init_t $1:process2 { nnp_transition nosuid_transition };
```

`unconfined_service_t` already has it, so `NoNewPrivileges=` on our unit is safe today. But
systemd sets NNP implicitly for `SystemCallFilter=`, `RestrictAddressFamilies=`,
`RestrictNamespaces=`, `PrivateDevices=`, `ProtectKernelTunables=`, `ProtectKernelModules=`,
`MemoryDenyWriteExecute=` and friends ([systemd#3845](https://github.com/systemd/systemd/issues/3845)).
**If we ever ship our own `tbxd_t`, we must call `init_nnp_daemon_domain(tbxd_t)` or every
hardening directive silently kills the domain transition.** Prior art of this exact breakage:
[RHBZ#1507909 (systemd-networkd)](https://bugzilla.redhat.com/show_bug.cgi?id=1507909),
[cockpit#10586 (systemd-timesyncd)](https://github.com/cockpit-project/cockpit/issues/10586).

### 3.4 If we ever ship a module

Per [Fedora wiki: SELinux/IndependentPolicy](https://fedoraproject.org/wiki/SELinux/IndependentPolicy)
— a `%{name}-selinux` subpackage with `.te`/`.fc`/`.if`:

```rpm-spec
%global selinuxtype targeted
%global modulename talosbox
BuildRequires: selinux-policy-devel
Requires:      selinux-policy-%{selinuxtype}
BuildArch:     noarch

%pre
%selinux_relabel_pre -s %{selinuxtype}
%post
%selinux_modules_install -s %{selinuxtype} \
  %{_datadir}/selinux/packages/%{selinuxtype}/%{modulename}.pp.bz2
%postun
if [ $1 -eq 0 ]; then
    %selinux_modules_uninstall -s %{selinuxtype} %{modulename}
fi
%posttrans
%selinux_relabel_post -s %{selinuxtype}
```

Modules install at **priority 200** (distro policy is 100); the 200 is baked into the macro and
must not be changed. The best real-world exemplar is
[`container-selinux.spec`](https://github.com/containers/container-selinux/blob/main/rpm/container-selinux.spec),
which adds `Requires(post): selinux-policy-base >= %_selinux_policy_version`,
`policycoreutils >= 3.10`, `libselinux-utils`, builds via
`make -f /usr/share/selinux/devel/Makefile`, and installs the `.if` into
`/usr/share/selinux/devel/include/services/` so other packages can call it.

**The maintenance cost is real and version-coupled.** We would `BuildRequires:
selinux-policy-devel`, compile against the distro's refpolicy headers, and a refpolicy interface
rename in a Fedora release breaks our build. `container.te` is at `policy_module(container,
2.250.0)` — 250 minor revisions of churn. That is the ongoing tax.

Note that `virt.{te,fc,if}` lives in `fedora-selinux/selinux-policy` `policy/modules/contrib/`,
not in libvirt. And there is **no `tailscale.te`** anywhere in contrib.

---

## 4. AppArmor on Ubuntu

**Default posture: nothing is confined without a profile.** Ubuntu's own docs state AppArmor is
installed and loaded by default but "*only programs with explicit profiles are confined;
unconfined applications operate without restrictions*"
([Ubuntu Server docs](https://ubuntu.com/server/docs/how-to/security/apparmor/)). There is no
`unconfined_service_t` analogue and no labelling requirement. **A packaged `tbxd`/`tbx-helper`
with no profile is simply unconfined and works.** Materially lower-risk than Fedora's default.

**Shipping a profile** is cheap: install to `/etc/apparmor.d/<name>`, then
`dh_apparmor --profile-name=<name> -p<package>` in `debian/rules`
([dh_apparmor(1)](https://manpages.debian.org/bookworm/dh-apparmor/dh_apparmor.1.en.html)). It
generates the maintainer-script snippets, creates/removes the `/etc/apparmor.d/local/<name>`
user-override include, and reloads with `apparmor_parser -r -W -T /etc/apparmor.d/<name>`.
Build-dep `dh-apparmor`. Cautionary note: docker's
[`deb/common/rules`](https://github.com/docker/docker-ce-packaging/blob/master/deb/common/rules)
calls `dh_apparmor --profile-name=docker-ce` but **no profile file exists in the tree** — the
call is vestigial and installs nothing. Easy trap.

**`kernel.apparmor_restrict_unprivileged_userns` does not affect us.** Enabled by default in
24.04 LTS ([release notes](https://documentation.ubuntu.com/release-notes/24.04/)), it targets
programs that are "*unprivileged and unconfined*". `tbx-helper` runs privileged; the whole point
of the mitigation is that root already has the capabilities a namespace would confer. Two
caveats: it **would** matter if the *unprivileged* `tbxd` ever creates a user namespace (a
rootless VM/sandbox path would break on 24.04+ and need a `userns` rule), and the mitigation has
a known bypass history ([Qualys: three bypasses](https://www.qualys.com/2025/three-bypasses-of-Ubuntu-unprivileged-user-namespace-restrictions.txt))
— do not design a security boundary around it.

`/dev/net/tun` has no special AppArmor interaction: an ordinary `rw` file rule plus
`capability net_admin`, if a profile exists at all.

---

## 5. What comparable tools actually ship

| Project | Ships AppArmor? | Ships SELinux? | Generated at runtime? |
|---|---|---|---|
| **tailscale** | **Nothing** | **Nothing** (and no distro module either) | — |
| **libvirt** | **Yes**, upstream, in `src/security/apparmor/` | No — zero `.te` files in repo | Yes, `virt-aa-helper` per-VM |
| **incus / LXD** | No static profile | No | **Yes**, `internal/server/apparmor/*.profile.go` |
| **moby/docker** | Generated in-process, never written to disk | Two additive `.cil` deltas only | **Yes**, piped to `apparmor_parser -Kr` |

### tailscale — our closest analogue, ships nothing

A full tree scan of `tailscale/tailscale@main` for `apparmor|selinux|\.te$|\.fc$|\.if$` returns
**zero** matches. The deb payload from
[`release/dist/unixpkgs/pkgs.go`](https://github.com/tailscale/tailscale/blob/main/release/dist/unixpkgs/pkgs.go)
is six files, `Depends: ["iptables"]`, no `selinux-policy`/`apparmor` dependency. The
[rpm](https://github.com/tailscale/tailscale/blob/main/release/rpm/rpm.postinst.sh) and
[deb](https://github.com/tailscale/tailscale/blob/main/release/deb/debian.postinst.sh) postinst
scripts contain no `semodule`, no `restorecon`, no `apparmor_parser`.

Their [unit](https://raw.githubusercontent.com/tailscale/tailscale/main/cmd/tailscaled/tailscaled.service)
has **no** `CapabilityBoundingSet=`, `AmbientCapabilities=`, `NoNewPrivileges=`, `ProtectSystem=`,
`PrivateTmp=`, `RestrictAddressFamilies=`, `SystemCallFilter=`, `AppArmorProfile=`, or
`SELinuxContext=` — full root, no sandboxing. The only structure is the
`RuntimeDirectory`/`StateDirectory`/`CacheDirectory` triple.

What it costs them: [#4908 "ssh: handle SELinux somehow?"](https://github.com/tailscale/tailscale/issues/4908),
open since 2022, with the founder writing "*I don't know enough about SELinux to construct,
distribute, install and apply a better policy though*"; duplicates
[#4914](https://github.com/tailscale/tailscale/issues/4914),
[#4975](https://github.com/tailscale/tailscale/issues/4975),
[#5769](https://github.com/tailscale/tailscale/issues/5769),
[#12442](https://github.com/tailscale/tailscale/issues/12442); plus the socket-access issue
[#5622](https://github.com/tailscale/tailscale/issues/5622) and the resolv.conf relabel bug
[#20149](https://github.com/tailscale/tailscale/issues/20149).

**Critically, none of tailscale's SELinux pain is in the tun/netlink/iptables path.** It is
concentrated in (a) exec'ing a login shell, (b) other confined domains touching its socket, and
(c) atomic file replacement in `/etc`. We potentially do (c); we do not do (a).

### libvirt — ships AppArmor, consumes SELinux

[`src/security/apparmor/`](https://github.com/libvirt/libvirt/tree/master/src/security/apparmor)
contains `usr.sbin.libvirtd.in`, `usr.sbin.virtqemud.in`, `usr.lib.libvirt.virt-aa-helper.in`,
`libvirt-qemu`, `libvirt-lxc`, `TEMPLATE.qemu`, `TEMPLATE.lxc`, installed to `/etc/apparmor.d/`
and `/etc/apparmor.d/libvirt/`. `virt-aa-helper` reads domain XML, substitutes into the
template, writes `/etc/apparmor.d/libvirt/libvirt-<uuid>`, and execs `apparmor_parser`. The
daemon profile deliberately forbids doing this itself:

```
audit deny /etc/apparmor.d/libvirt/** wxl,
audit deny /{usr/,}sbin/apparmor_parser rwxl,
```

A privilege-separation pattern structurally similar to our `tbxd`/`tbx-helper` split, and worth
copying if we ever ship AppArmor.

For SELinux libvirt has only `security_selinux.c` — it *applies* labels and allocates MCS
categories. Zero `.te` files. Fedora's spec **disables AppArmor entirely** and has **no
`Requires: selinux-policy`** — it just assumes `selinux-policy-targeted` is in the base install.

### incus / LXD — generate at runtime, no SELinux

[`internal/server/apparmor/`](https://github.com/lxc/incus/tree/main/internal/server/apparmor)
holds Go `text/template` constants; `instanceProfile()` renders, writes to
`/var/lib/incus/security/apparmor/profiles/`, and execs `apparmor_parser -rWL`. **No static
profile for the daemon itself.** Zero `.te` files in either incus or LXD.
`internal/server/sys/selinux.go` sniffs the daemon's own context: `container_runtime_t` →
container-selinux present; `incusd_t` → refpolicy module; anything else → **integration disabled
with a warning**.

The history is directly instructive: incus users hit exactly our situation —
[ganto/copr-lxc4#40](https://github.com/ganto/copr-lxc4/issues/40) documents `incusd` running as
`init_t` writing `var_lib_t`. The policy eventually went **upstream to refpolicy, not into
incus** ([SELinuxProject/refpolicy#951](https://github.com/SELinuxProject/refpolicy/pull/951),
merged 2025-07), and Fedora's `incus-selinux` subpackage was then **dropped** in favour of
container-selinux.

### moby/docker — runtime generation, distro-owned SELinux

`docker-default` is **never written to disk**. `installDefault()` in
[`moby/profiles/apparmor`](https://github.com/moby/profiles/blob/main/apparmor/apparmor.go)
probes the host (`macroExists` for `abi/3.0`, `tunables/global`, `abstractions/base`), reads
`/proc/self/attr/current`, renders the template, and pipes it to `apparmor_parser -Kr` (`-K` so
it never writes to a read-only filesystem). The reason it is generated is the lesson: **the
profile must be parameterized by host state a static file cannot know.**

For SELinux, moby *did* ship `contrib/selinux/docker-engine-selinux/{docker.te,docker.if,docker.fc}`
at v1.12.6 — deleted within a year, commit
[`adb2ddf`](https://github.com/moby/moby/commits/adb2ddf288a893609f902ba9d4f4d4c45e4e730f) titled
"*Rely on container-selinux for centos/fedora25/rhel*". `container-selinux.spec` carries
`Obsoletes: docker-selinux <= 2:1.12.4-28` and `container.te` still has
`typealias container_runtime_t alias docker_t;`. What moby holds today is two small CIL deltas
that *depend on* container-selinux. Its scriptlet is worth copying for defensiveness:

```rpm-spec
%post
if command -v semodule > /dev/null 2>&1 && selinuxenabled 2>/dev/null; then
    if ! semodule -i %{_datadir}/docker-ce/selinux/docker-af-alg-deny.cil 2>/dev/null; then
        echo "warning: could not load ... SELinux policy; ... is not active" >&2
    fi
fi
```

Guards on both `command -v semodule` and `selinuxenabled`, and **warns rather than failing the
install**.

---

## 6. Recommendation

### Must have (required for the software to work)

1. **Install to `/usr/bin` and `/usr/lib/tbx`, never `/opt`.** The only genuinely load-bearing
   SELinux item. Wrong label → daemon runs as `init_t` → confusing denials.
2. **`restorecon` in any non-RPM install path** (tarball, curl-installer, `make install`). RPM
   does this for free; nothing else does.
3. **Do not lose labels on atomic file replacement.** `restorecon` after any `rename()` over a
   file in `/etc` (or `setfscreatecon` before). A *code* fix, and the one item here that has
   actually broken our closest analogue in production.
4. **`AmbientCapabilities=CAP_NET_ADMIN` + `NoNewPrivileges=yes`, never setcap.** Solves the
   packaging problem and the security problem simultaneously.
5. **Nothing at all on Ubuntu.** No profile → unconfined → works.

### Nice to have (deferrable good citizenship)

6. A static AppArmor profile for `tbx-helper` via `dh_apparmor` — one file plus one
   `debian/rules` line, no build-time coupling to distro policy versions, and it can ship in
   complain mode first so it cannot break users. **If we do only one optional thing, do this
   one** — it is an order of magnitude cheaper than SELinux.
7. A `-selinux` subpackage with `tbxd.te/.fc/.if` giving named types. The payoff is mostly that
   *other* confined domains can be granted access to our socket via a published interface —
   the wall tailscale has been stuck against since 2022.

### Incremental path

1. **Now** — correct install paths; `restorecon` in non-RPM installers; label-safe atomic writes;
   ambient caps in the unit. Add `tbx doctor` checks that report the daemon's SELinux context and
   warn if it is not `unconfined_service_t` (~20 lines; turns the silent failure loud). Incus
   does exactly this in `internal/server/sys/selinux.go`.
2. **On first real user report** — ship an AppArmor profile in complain mode via `dh_apparmor`,
   promote to enforce a release later.
3. **Only if a confined third party needs our socket, or a distro asks** — write the SELinux
   module, following container-selinux's spec, with a docker-ce-style non-fatal scriptlet.
4. **If it gets serious** — upstream it to `SELinuxProject/refpolicy` rather than carrying it,
   the path incus took (after which Fedora dropped its own `incus-selinux` subpackage).

### Distribution shape

- **GoReleaser → nfpm** (deb/rpm/apk) for GitHub Releases; **COPR** for Fedora/EL from one spec
  (F42+/EL9/EL10 all work unchanged); **`aurs:`** for `tbx-bin`.
- Consider a hand-rolled `debian/` with debhelper 13 instead of nfpm's deb output — the
  `deb-systemd-helper` enable-once semantics are the biggest correctness delta, and nfpm cannot
  produce them.
- Layout: `/usr/bin/tbx`, `/usr/bin/tbxd`, `/usr/lib/tbx/tbx-helper`, units in
  `/usr/lib/systemd/system/`, `/usr/lib/sysusers.d/tbx.conf`, `/usr/lib/tmpfiles.d/tbx.conf`.
- Group creation: the sysusers.d file works free on Arch (pacman hook) and Fedora
  (`%sysusers_create_compat`); Debian needs `addgroup --system _tbx` in postinst, and
  nfpm-built packages need a hand-written idempotent `systemd-sysusers`/`groupadd`.

### `tbx doctor` checks worth adding on Linux

- SELinux context of the running daemon; warn if not `unconfined_service_t`.
- firewalld running, backend, and `StrictForwardPorts` (silently breaks port forwarding).
- `table inet tbx` present; the bridge interface's firewalld zone.
- `tun` / `br_netfilter` modules loaded; `/dev/net/tun` mode.
- Whether the helper's ambient capabilities actually landed.

### One framing point

Across this entire sample, **nobody ships a static `.te`/`.pp` from their own upstream repo.**
The one project that tried (moby, 2016–2017) deleted it within a year and handed it to the
distro. The two viable postures are tailscale's (ship nothing, accept a long tail of open
SELinux issues) and moby/incus's (generate confinement at runtime from host-probed state, let
the distro own the static module). We are tailscale-shaped, and tailscale's posture works — its
SELinux pain is concentrated in features we do not have.
