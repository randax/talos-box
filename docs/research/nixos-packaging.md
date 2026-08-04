# Packaging talos-box for NixOS: flake, module, and privileged-helper design

**Audience:** talos-box maintainers doing the Linux port (map issue #71, wayfinder #76). Assumes you know Go and systemd, but not Nix.

## TL;DR recommendation

1. **Ship a plain flake with no flake inputs beyond `nixpkgs`.** No `flake-utils`, no `flake-parts`. A 6-line `forAllSystems` helper covers everything a 3-binary Go project needs, and a dependency-free flake is much less painful for consumers and for a later nixpkgs submission. Expose `packages.<system>.{default,talos-box}`, `overlays.default`, `nixosModules.default`, `devShells.<system>.default`, `apps.<system>.{tbx,tbxd}`, and `checks.<system>.*`. These are exactly the attribute names `nix flake check` knows how to validate ([`nix flake check`, Nix manual](https://nix.dev/manual/nix/latest/command-ref/new-cli/nix3-flake-check.html)).
2. **Namespace the module `virtualisation.talosbox`, not `services.talosbox`.** Every comparable tool in nixpkgs lives under `virtualisation.*`: `virtualisation.libvirtd` ([libvirtd.nix:12](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/virtualisation/libvirtd.nix#L12)), `virtualisation.incus`, `virtualisation.podman`, `virtualisation.lxd`, `virtualisation.waydroid`.
3. **Run `tbx-helper` as a socket-activated systemd *system* service with `AmbientCapabilities`, not via `security.wrappers`.** `security.wrappers` exists to solve a problem talos-box does not have (an unprivileged user `execve()`-ing a privileged binary). The helper is a daemon reached over a unix socket — the same shape as `incus.socket` → `incusd`, which NixOS models with `SocketMode = "0660"; SocketGroup = "incus-admin";` ([incus.nix:521-530](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/virtualisation/incus.nix#L521-L530)). **Confidence: high.**
4. **Run `tbxd` as a `systemd.user.service` with socket activation**, mirroring `systemd.user.sockets.podman` ([podman/default.nix:304-305](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/virtualisation/podman/default.nix#L304-L305)). Membership in a `talosbox` group is what authorizes a user to reach the helper socket; membership in `kvm` is what gets them `/dev/kvm`.
5. **Do not attempt `setcap` on a `/nix/store` path — it is structurally impossible.** Nix canonicalises every store path's mode to `0444`/`0555` on registration, destroying setuid/setgid/file-capability bits ([`posix-fs-canonicalise.cc:20-24`](https://github.com/NixOS/nix/blob/master/src/libstore/posix-fs-canonicalise.cc#L20-L24)). This is *the* NixOS-specific constraint and it is why `security.wrappers` copies a helper binary into a tmpfs at `/run/wrappers/bin` and `setcap`s *that*.
6. **`nixosTest` for e2e is viable but needs nested virt on the builder** — the outer test VM already runs under `-machine accel=kvm:tcg -cpu max`, and the test framework requires the `kvm` system feature ([running-nixos-tests.section.md:22-26](https://github.com/NixOS/nixpkgs/blob/master/nixos/doc/manual/development/running-nixos-tests.section.md#L22-L26)). Nested KVM inside it depends on the *host* kernel's `kvm_intel.nested` / `kvm_amd.nested` — **confidence: medium**, see Open questions.

## Comparison: how to grant the helper its privileges

| Approach | Mechanism | Fits talos-box? | Notes |
|---|---|---|---|
| `security.wrappers` with `capabilities` | copies a static musl `security-wrapper` into `/run/wrappers/bin`, `setcap "cap_setpcap,<caps>"` on the copy, wrapper raises caps into the *ambient* set then `execve`s the store binary ([wrappers/default.nix:133-147](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/security/wrappers/default.nix#L133-L147)) | **No** (for the helper) | Only makes sense when an *unprivileged user* must exec the binary. Adds a tmpfs copy, an activation script, and an AppArmor include per wrapper. |
| `security.wrappers` with `setuid` | copies wrapper, `chown root:root`, `chmod u+s` ([wrappers/default.nix:161-169](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/security/wrappers/default.nix#L161-L169)) | **No** | Explicitly ruled out by the project's "capabilities over setuid" decision. This is what libvirtd uses for `qemu-bridge-helper` — see below. |
| systemd system service + `AmbientCapabilities` + `.socket` with `SocketGroup` | systemd sets the ambient set at exec; socket unit owns the FD and its group/mode | **Yes** | Direct analog of today's root launchd daemon. `AmbientCapabilities` is documented as "useful if you want to execute a process as a non-privileged user but still want to give it some capabilities" ([systemd.exec(5)](https://man7.org/linux/man-pages/man5/systemd.exec.5.html)). |
| polkit | D-Bus-mediated authorization, per-action rules | **No** | libvirtd *requires* polkit ([libvirtd.nix:410-411](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/virtualisation/libvirtd.nix#L410-L411)), but that's because libvirt speaks D-Bus/polkit natively. Adding a D-Bus dependency for a unix-socket daemon is pure overhead. |

---

## 1. Flake structure

### Why no flake-parts / flake-utils

`nix flake check` validates a fixed set of attribute paths, and none of them require a framework ([`nix3-flake-check`](https://nix.dev/manual/nix/latest/command-ref/new-cli/nix3-flake-check.html)):

- `packages.<system>.default`, `packages.<system>.<name>` — must be derivations
- `checks.<system>.<name>` — derivations
- `devShells.<system>.default`, `devShells.<system>.<name>`
- `apps.<system>.default`, `apps.<system>.<name>`
- `overlays.default`, `overlays.<name>`
- `nixosModules.default`, `nixosModules.<name>`
- `nixosConfigurations.<name>.config.system.build.toplevel`
- `templates.*`, `bundlers.*`, `hydraJobs`, `legacyPackages.<system>`

`flake-parts` earns its keep on repos with many `perSystem` outputs and cross-cutting module composition; a three-binary Go tool has one package and one module. Adding an input means every consumer's `flake.lock` gains a node, and a nixpkgs submission later cannot use it at all. **Confidence: high** that plain is right here; this is a judgement call, not a documented rule.

### Illustrative `flake.nix`

**Not copied from a source — written for this doc**, but every attribute name and `buildGoModule` argument used below is verified against the cited nixpkgs docs.

```nix
{
  description = "talos-box: Talos Linux VM clusters on your laptop";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system:
        f nixpkgs.legacyPackages.${system});
    in {
      overlays.default = final: prev: {
        talos-box = final.callPackage ./nix/package.nix { };
      };

      packages = forAllSystems (pkgs: rec {
        talos-box = pkgs.callPackage ./nix/package.nix { };
        default = talos-box;
      });

      nixosModules.default = import ./nix/module.nix;

      apps = forAllSystems (pkgs: {
        tbx = { type = "app"; program = "${self.packages.${pkgs.system}.default}/bin/tbx"; };
      });

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = [ pkgs.go pkgs.golangci-lint pkgs.qemu_kvm pkgs.iproute2 pkgs.nftables ];
        };
      });

      checks = forAllSystems (pkgs:
        { package = self.packages.${pkgs.system}.default; }
        // nixpkgs.lib.optionalAttrs pkgs.stdenv.hostPlatform.isLinux {
          module = pkgs.testers.runNixOSTest (import ./nix/tests/basic.nix {
            inherit (self) nixosModules;
          });
        });
    };
}
```

Two wiring notes that matter for consumers:

- **`nixosModules.default` must not close over `self.packages`.** Import the package via `pkgs.callPackage` inside the module using the *consumer's* nixpkgs, or make the module's `package` option default to `pkgs.talos-box` and tell consumers to add `overlays.default` to `nixpkgs.overlays`. This is the standard escape from "my flake's nixpkgs vs. your nixpkgs" duplication. Cite by analogy: every in-tree module uses `mkPackageOption pkgs "qemu" { }` ([libvirtd.nix:49](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/virtualisation/libvirtd.nix#L49)).
- **`checks` on Darwin must exclude the NixOS test.** NixOS VM tests need a Linux builder; on macOS "you also need a 'remote' builder for Linux" ([running-nixos-tests.section.md:30](https://github.com/NixOS/nixpkgs/blob/master/nixos/doc/manual/development/running-nixos-tests.section.md#L30)).

### `nix/package.nix` — multiple binaries

`buildGoModule` builds all `main` packages under the source root unless restricted. The nixpkgs Go manual shows exactly the `cmd/` layout talos-box has:

> Many Go projects keep the main package in a `cmd` directory. Following example could be used to only build the example-cli and example-server binaries:
> ```nix
> { subPackages = [ "cmd/example-cli" "cmd/example-server" ]; }
> ```
> — [go.section.md:158-172](https://github.com/NixOS/nixpkgs/blob/master/doc/languages-frameworks/go.section.md#L158-L172)

Since talos-box's `cmd/` contains exactly `tbx`, `tbxd`, `tbx-helper` and nothing else, `subPackages` is optional; listing it explicitly is still worth it so a future `cmd/internal-tool` doesn't silently ship.

Version stamping maps 1:1 onto the existing `Makefile` (`-X github.com/randax/talos-box/internal/version.Version=$(VERSION)`):

> The most common use case for this argument is to make the resulting executable aware of its own version by injecting the value of string variable using the `-X` flag.
> ```nix
> { ldflags = [ "-X main.Version=${version}" "-X main.Commit=${version}" ]; }
> ```
> — [go.section.md:121-133](https://github.com/NixOS/nixpkgs/blob/master/doc/languages-frameworks/go.section.md#L121-L133)

**`vendorHash` maintenance:** the docs give an explicit escape hatch —

> `vendorHash` can be set to `null`. In that case, rather than fetching the dependencies, the dependencies already vendored in the `vendor` directory of the source repo will be used. To avoid updating this field when dependencies change, run `go mod vendor` in your source repo and set `vendorHash = null;`.
> — [go.section.md:59-67](https://github.com/NixOS/nixpkgs/blob/master/doc/languages-frameworks/go.section.md#L59-L67)

Recommendation: **keep `vendorHash` (do not vendor)**, and add a CI job that runs `nix build` on dependabot PRs so a stale hash fails loudly. Committing `vendor/` to dodge one hash is a bad trade for a repo that already gets bot dependency bumps (see recent commits bumping `golang.org/x/net`, `grpc`). To refresh: set `vendorHash = lib.fakeHash;` and read the hash out of the build error ([go.section.md:69](https://github.com/NixOS/nixpkgs/blob/master/doc/languages-frameworks/go.section.md#L69)).

### Runtime deps: wrapper vs. `path`

Lima — the closest analog in nixpkgs, also a Go VM manager that shells out to QEMU — wraps its CLI:

```nix
wrapProgram $out/bin/limactl \
  --prefix PATH : ${lib.makeBinPath [ qemu ]}
```
— [pkgs/by-name/li/lima/package.nix:71-72](https://github.com/NixOS/nixpkgs/blob/master/pkgs/by-name/li/lima/package.nix#L71-L72)

For talos-box, prefer a **split**:

- `tbx` (the user-facing CLI): no wrapper needed, it talks to `tbxd`.
- `tbxd` / `tbx-helper`: **do not `wrapProgram`** — set `path = [ pkgs.qemu_kvm pkgs.iproute2 pkgs.nftables ]` on the systemd unit in the module instead. This is what both libvirtd and incus do (`systemd.services.libvirtd.path`, [libvirtd.nix:556-560](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/virtualisation/libvirtd.nix#L556-L560); `systemd.services.incus = { inherit environment path; }`, [incus.nix:427](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/virtualisation/incus.nix#L427)). It keeps the package itself lean and lets the module override QEMU via a `package` option.

A `makeWrapper` fallback on `tbxd` is still worth having so the flake's `packages.default` works standalone (`nix run`) without the NixOS module.

---

## 2. NixOS module design

### Namespace and option shape

Use `virtualisation.talosbox`. Precedent, verified in-tree: `virtualisation.libvirtd` ([libvirtd.nix:12](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/virtualisation/libvirtd.nix#L12)), `virtualisation.incus`, `virtualisation.podman`, `virtualisation.waydroid`, `virtualisation.lxd`. `services.*` is for network services; `virtualisation.*` is where hypervisor/container managers live.

Core options, following libvirtd/incus conventions:

- `enable` — `lib.mkEnableOption`
- `package` — `lib.mkPackageOption pkgs "talos-box" { }`
- `qemu.package` — `mkPackageOption pkgs "qemu_kvm" { }`. libvirtd documents the trade-off precisely: "`pkgs.qemu` can emulate alien architectures (e.g. aarch64 on x86); `pkgs.qemu_kvm` saves disk space allowing to emulate only host architectures" ([libvirtd.nix:50-53](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/virtualisation/libvirtd.nix#L50-L53)). Default to `qemu_kvm` since talos-box only runs native-arch guests.
- `allowedBridges` / `bridgeInterface` — mirror libvirtd's `allowedBridges` (`types.listOf types.str`, default `[ "virbr0" ]`, [libvirtd.nix:346-352](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/virtualisation/libvirtd.nix#L346-L352)).
- `openFirewall` / trusted interface handling — see below.

### Users, groups, socket access

```nix
users.groups.talosbox = { };
```

Both incus and podman create bare groups exactly like this (`users.groups.incus = { }; users.groups.incus-admin = { };` — [incus.nix:561-562](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/virtualisation/incus.nix#L561-L562); `users.groups.podman = { };` — [podman/default.nix:329](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/virtualisation/podman/default.nix#L329)).

Do **not** allocate a static GID from `nixos/modules/misc/ids.nix` — that file is reserved for in-tree modules, and an out-of-tree flake taking a GID there will collide. Only take a static ID if talos-box lands in nixpkgs proper.

The helper's socket is the authorization boundary. Copy incus verbatim in shape:

```nix
systemd.sockets.incus = {
  description = "Incus UNIX socket";
  wantedBy = [ "sockets.target" ];
  socketConfig = {
    ListenStream = "/var/lib/incus/unix.socket";
    SocketMode = "0660";
    SocketGroup = "incus-admin";
  };
};
```
— [incus.nix:521-530](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/virtualisation/incus.nix#L521-L530)

For talos-box: `ListenStream = "/run/talosbox/helper.sock"; SocketMode = "0660"; SocketGroup = "talosbox";`. Your existing peer-credential check in the helper stays — the socket group is defence in depth, not a replacement.

### `/dev/kvm` and the `kvm` group

**Do not write a udev rule.** NixOS reserves GID 302 for `kvm` with the comment "default udev rules from systemd requires these" ([ids.nix:668](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/misc/ids.nix#L668)), and the rule ships with systemd itself:

```
KERNEL=="kvm", GROUP="kvm", MODE="{{DEV_KVM_MODE}}", OPTIONS+="static_node=kvm"
KERNEL=="vhost-vsock", GROUP="kvm", MODE="{{DEV_KVM_MODE}}", OPTIONS+="static_node=vhost-vsock"
KERNEL=="vhost-net", GROUP="kvm", MODE="{{DEV_KVM_MODE}}", OPTIONS+="static_node=vhost-net"
```
— [systemd rules.d/50-udev-default.rules.in:116-123](https://github.com/systemd/systemd/blob/main/rules.d/50-udev-default.rules.in#L116-L123)

`DEV_KVM_MODE` is a build-time meson option; nixpkgs' systemd derivation does not override it (grepped `pkgs/os-specific/linux/systemd/default.nix` for `kvm` — no hits), so upstream's default applies. **Confidence: medium** on the exact numeric mode (`0666` upstream) — I could not fetch `meson.options` to confirm. Design defensively: the module should document `users.users.<name>.extraGroups = [ "kvm" "talosbox" ]`, and `tbx doctor` should check `access("/dev/kvm", R_OK|W_OK)` rather than assuming.

Note `vhost-net` is also `kvm`-grouped — relevant if talos-box uses `vhost=on` on its tap devices.

### Kernel modules, sysctls, firewall

- **`boot.kernelModules`.** libvirtd loads `[ "tun" ]` ([libvirtd.nix:432](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/virtualisation/libvirtd.nix#L432)); incus loads `[ "br_netfilter" "veth" "xt_comment" "xt_CHECKSUM" "xt_MASQUERADE" "vhost_vsock" ]` ([incus.nix:343-351](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/virtualisation/incus.nix#L343-L351)). talos-box needs at minimum `tun`, plus `bridge` and (if it uses `vhost=on`) `vhost_net`.
- **`boot.kernel.sysctl` for forwarding.** Don't invent your own priority. NixOS's NAT module sets forwarding at override priority 99 so user config still wins:
  ```nix
  boot.kernel.sysctl = {
    "net.ipv4.conf.all.forwarding" = mkOverride 99 true;
    "net.ipv4.conf.default.forwarding" = mkOverride 99 true;
  };
  ```
  — [nat.nix:199-202](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/services/networking/nat.nix#L199-L202)

  Use the same `mkOverride 99` idiom (or better: set `networking.nat.enable` / let the user do it, and only *assert* that forwarding is on).
- **Firewall.** `networking.firewall.trustedInterfaces` is "Traffic coming in from these interfaces will be accepted unconditionally" ([firewall.nix:164-173](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/services/networking/firewall.nix#L164-L173)). Adding the talos-box bridge here is the right move behind an `openFirewall`-style option, defaulting to `true` since a cluster bridge is by definition local. Precedent for the option pattern: podman sets `networking.firewall.interfaces.<iface>.allowedUDPPorts = [ 53 ]` for its DNS ([podman/default.nix:255](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/virtualisation/podman/default.nix#L255)) — a good model for talos-box's `:53`.
- **nftables vs iptables.** Incus flatly asserts iptables is unsupported:
  > `message = "Incus on NixOS is unsupported using iptables. Set 'networking.nftables.enable = true;'";` — [incus.nix:312-321](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/virtualisation/incus.nix#L312-L321)

  talos-box should decide the same way and encode it as an `assertion`, rather than trying to support both backends silently.
- **`environment.systemPackages`.** libvirtd ships the client, the daemon package, QEMU and `config.networking.firewall.package` ([libvirtd.nix:420-430](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/virtualisation/libvirtd.nix#L420-L430)). For talos-box, put `cfg.package` (which carries `tbx`) on the system path when `enable = true`; keep `tbxd`/`tbx-helper` off `$PATH` if you can — but note the current design auto-starts `tbxd` from `tbx`, which under NixOS should become "socket-activate the user unit" instead (see below).

### `tbxd` as a user service

```nix
systemd.user.services.podman.environment = config.networking.proxy.envVars;
systemd.user.sockets.podman.wantedBy = [ "sockets.target" ];
```
— [podman/default.nix:304-305](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/virtualisation/podman/default.nix#L304-L305)

This is the exact precedent for a per-user daemon under NixOS. Define `systemd.user.sockets.tbxd` (`ListenStream = "%t/talosbox/tbxd.sock"`) and `systemd.user.services.tbxd`. Consequences worth designing for now:

- **Drop the "`tbx` forks `tbxd`" auto-start on Linux** in favour of socket activation. It's more robust (no race, no orphan) and it's what the platform expects. Keep the fork path as a fallback for non-systemd Linux and macOS.
- `~/.talosbox/` should become `$XDG_STATE_HOME/talosbox` on Linux, or use systemd's `StateDirectory=` in the user unit.
- User units only run when the user has a session unless you set `users.users.<n>.linger` — worth an option if clusters should survive logout.

### Unfree / KVM gating

Nothing here is unfree — QEMU, Go, and Talos images are all free software, so no `nixpkgs.config.allowUnfree` interaction. There is **no NixOS option that "enables KVM"**; KVM is autoloaded by the kernel and gated purely by `/dev/kvm` permissions. The right gate is an assertion on `pkgs.stdenv.hostPlatform.isLinux` plus a runtime check in `tbx doctor`.

---

## 3. Privileged-helper patterns, and why `security.wrappers` is the wrong tool here

### Why NixOS needs wrappers at all

Nix registers store paths by canonicalising their metadata. The relevant code:

```cpp
mode_t mode = st.st_mode & ~S_IFMT;
bool isDir = S_ISDIR(st.st_mode);
if ((mode != 0444 || isDir) && mode != 0555) {
    mode = (st.st_mode & S_IFMT) | 0444 | (st.st_mode & S_IXUSR || isDir ? 0111 : 0);
    chmod(path, mode);
```
— [nix/src/libstore/posix-fs-canonicalise.cc:20-24](https://github.com/NixOS/nix/blob/master/src/libstore/posix-fs-canonicalise.cc#L20-L24)

Every file becomes `0444` or `0555`. Setuid and setgid bits are erased; the store is world-readable by design; and `setcap` on a store path would be both a mutation of an immutable-by-contract path and lost on any GC/rebuild. Hence `security.wrappers`.

### How `security.wrappers` actually works

For a capability wrapper, the activation script does:

```bash
cp ${securityWrapper source}/bin/security-wrapper "$wrapperDir/${program}"
# Prevent races
chmod 0000 "$wrapperDir/${program}"
chown ${owner}:${group} "$wrapperDir/${program}"
# Set desired capabilities on the file plus cap_setpcap so
# the wrapper program can elevate the capabilities set on
# its file into the Ambient set.
${pkgs.libcap.out}/bin/setcap "cap_setpcap,${capabilities}" "$wrapperDir/${program}"
chmod ${permissions} "$wrapperDir/${program}"
```
— [wrappers/default.nix:133-147](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/security/wrappers/default.nix#L133-L147)

Key details, all verified:

- The wrapper is a **statically linked musl** binary, deliberately: "musl is security-focused and generally more minimal... the wrappers are quite small, so linking it statically is more appropriate" ([wrappers/default.nix:15-18](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/security/wrappers/default.nix#L15-L18)).
- **`cap_setpcap` is added to the file caps but deliberately not left ambient**: "`cap_setpcap`, which is required for the wrapper program to be able to raise caps into the Ambient set is NOT raised to the Ambient set so that the real program cannot modify its own capabilities!!" ([wrappers/default.nix:99-106](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/security/wrappers/default.nix#L99-L106)). The ambient set is what survives `execve()` of an unprivileged program ([capabilities(7)](https://man7.org/linux/man-pages/man7/capabilities.7.html)); this is exactly how the wrapper hands caps to the real store binary.
- **setuid and capabilities are mutually exclusive**, enforced by an assertion: `assertion = opts.setuid || opts.setgid -> opts.capabilities == "";` ([wrappers/default.nix:265-271](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/security/wrappers/default.nix#L265-L271)).
- `wrapperDir` is `/run/wrappers/bin` and is `internal = true` — "It should not be overridden" ([wrappers/default.nix:251-259](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/security/wrappers/default.nix#L251-L259)) — backed by a dedicated tmpfs mount with `nodev,mode=755` ([wrappers/default.nix:307-318](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/security/wrappers/default.nix#L307-L318)), and prepended to `PATH` for every shell ([wrappers/default.nix:290-293](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/security/wrappers/default.nix#L290-L293)).
- Option surface is exactly: `enable`, `source`, `program`, `owner`, `group`, `permissions` (default `"u+rx,g+x,o+x"`), `capabilities` (a `types.commas` string in `cap_from_text(3)` syntax), `setuid`, `setgid` ([wrappers/default.nix:53-120](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/security/wrappers/default.nix#L53-L120)).

**Limits:** the caps live on the `/run/wrappers/bin` copy only. Anything that execs the store path directly (`${pkgs.talos-box}/bin/tbx-helper`) gets zero capabilities. Wrappers are also global — installing one puts a privileged binary on every user's `PATH`, which for talos-box means anyone on the box, not just the `talosbox` group.

### The recommended shape

`tbx-helper` is a **daemon**, started by init, that talks to `tbxd` over a socket. The wrapper mechanism is for the `ping`/`mount`/`sudo` shape — user execs binary, binary needs privilege. talos-box already made the architectural choice that avoids that.

Illustrative unit (**not copied from a source**, but every directive is documented in [systemd.exec(5)](https://man7.org/linux/man-pages/man5/systemd.exec.5.html) / [systemd.socket(5)](https://man7.org/linux/man-pages/man5/systemd.socket.5.html)):

```nix
systemd.sockets.tbx-helper = {
  description = "talos-box privileged helper socket";
  wantedBy = [ "sockets.target" ];
  socketConfig = {
    ListenStream = "/run/talosbox/helper.sock";
    SocketMode = "0660";
    SocketGroup = "talosbox";
  };
};

systemd.services.tbx-helper = {
  description = "talos-box privileged helper";
  requires = [ "tbx-helper.socket" ];
  after = [ "tbx-helper.socket" "network-pre.target" ];
  path = [ pkgs.iproute2 pkgs.nftables ];
  serviceConfig = {
    ExecStart = "${cfg.package}/bin/tbx-helper";
    Type = "notify";

    # privilege
    User = "root";                       # see note below
    CapabilityBoundingSet = [ "CAP_NET_ADMIN" "CAP_NET_RAW" "CAP_NET_BIND_SERVICE" ];
    AmbientCapabilities  = [ "CAP_NET_ADMIN" "CAP_NET_RAW" "CAP_NET_BIND_SERVICE" ];
    NoNewPrivileges = true;

    # hardening
    ProtectSystem = "strict";
    ProtectHome = true;
    PrivateTmp = true;
    RestrictAddressFamilies = [ "AF_UNIX" "AF_INET" "AF_INET6" "AF_NETLINK" ];
    RestrictSUIDSGID = true;
    MemoryDenyWriteExecute = true;
    RuntimeDirectory = "talosbox";
    ReadWritePaths = [ "/etc/resolv.conf" ];   # or use resolved/openresolv instead
  };
};
```

Notes and gotchas, each load-bearing:

- **Drop to a dedicated user if you can.** `AmbientCapabilities` is precisely for this: "Ambient capability sets are useful if you want to execute a process as a non-privileged user but still want to give it some capabilities. Note that, in this case, option **keep-caps** is automatically added to `SecureBits=` to retain the capabilities over the user change." ([systemd.exec(5)](https://man7.org/linux/man-pages/man5/systemd.exec.5.html)). The blocker is `/etc/resolv.conf` writes and `/dev/kvm` — audit whether the helper genuinely needs root, or whether `User=talosbox-helper` + `SupplementaryGroups=[ "kvm" ]` + `ReadWritePaths` suffices. **This is the single highest-value hardening decision.**
- **`CapabilityBoundingSet` must be a superset of `AmbientCapabilities`.** capabilities(7): "no capability can ever be ambient if it is not both permitted and inheritable" ([capabilities(7)](https://man7.org/linux/man-pages/man7/capabilities.7.html)). Setting only `AmbientCapabilities` while leaving the bounding set at default works, but setting a *narrower* bounding set silently drops the ambient caps.
- **`NoNewPrivileges = true` is compatible with `AmbientCapabilities`** — it blocks gaining privilege via `execve()` of setuid/file-cap binaries ("If true, ensures that the service process and all its children can never gain new privileges through `execve()`", [systemd.exec(5)](https://man7.org/linux/man-pages/man5/systemd.exec.5.html)), not caps systemd itself grants at exec time. But it *does* mean the helper cannot exec a setuid helper of its own — relevant if you ever considered `qemu-bridge-helper`.
- **Do not use `DynamicUser`.** It implies `NoNewPrivileges=`, `RemoveIPC=` and `RestrictSUIDSGID=` ([systemd.exec(5)](https://man7.org/linux/man-pages/man5/systemd.exec.5.html)) and gives you a UID that changes across restarts — useless for a socket whose group is the authorization boundary and for state that must persist.
- **`ProtectSystem = "strict"` "mounts the entire file system hierarchy read-only, except for API file system subtrees"** ([systemd.exec(5)](https://man7.org/linux/man-pages/man5/systemd.exec.5.html)). If the helper writes DNS config, you need `ReadWritePaths`, or better, stop writing `/etc/resolv.conf` directly and integrate with `systemd-resolved` (which on NixOS owns that file) — see Open questions.
- **`AF_NETLINK` must be in `RestrictAddressFamilies`** or every `ip`/rtnetlink operation fails with EAFNOSUPPORT. This is the most common way a hardened networking daemon breaks.
- **Skip `SystemCallFilter` initially.** A Go binary's runtime makes broad syscall use; `@system-service` is the usual starting allowlist but needs real testing. Ship it in a second pass with a NixOS test that exercises the full VM lifecycle.

### The one place `security.wrappers` *is* relevant: `qemu-bridge-helper`

If talos-box ever lets an unprivileged QEMU attach to a bridge itself (rather than the helper pre-creating the tap and passing the FD), NixOS handles it like this:

```nix
security.wrappers.qemu-bridge-helper = {
  setuid = true;
  owner = "root";
  group = "root";
  source = "${cfg.qemu.package}/libexec/qemu-bridge-helper";
};
```
— [libvirtd.nix:457-462](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/virtualisation/libvirtd.nix#L457-L462)

and the ACL file is generated into `/etc/qemu/bridge.conf` — note the comment about the path:

```nix
# this file is expected in /etc/qemu and not sysconfdir (/var/lib)
etc."qemu/bridge.conf".text = lib.concatMapStringsSep "\n" (e: "allow ${e}") cfg.allowedBridges;
```
— [libvirtd.nix:421-422](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/virtualisation/libvirtd.nix#L421-L422)

Two things follow. First, nixpkgs' `qemu-bridge-helper` is **setuid, not capability-based** — so if you go this route you inherit exactly the setuid dependency the project decided to avoid. Second, on NixOS the helper QEMU actually execs is `/run/wrappers/bin/qemu-bridge-helper`, not the store path — QEMU finds it via `PATH`, so a hardened `tbxd`/`tbx-helper` unit with a restricted `path` would not see it. **Recommendation: have `tbx-helper` create the tap device itself with `CAP_NET_ADMIN` and pass the fd to QEMU (`-netdev tap,fd=N`), avoiding `qemu-bridge-helper` and `/etc/qemu/bridge.conf` entirely.** This is portable across all four tier-one distros and drops a setuid binary from the story. **Confidence: high.**

---

## 4. Testing with `nixosTest`

Outside nixpkgs, the entry point is `pkgs.testers.runNixOSTest`:

```nix
pkgs.testers.runNixOSTest {
  imports = [ ./test.nix ];
  defaults.services.foo.package = mypkg;
}
```
— [writing-nixos-tests.section.md:95-111](https://github.com/NixOS/nixpkgs/blob/master/nixos/doc/manual/development/writing-nixos-tests.section.md#L95-L111)

Hardware requirement, stated explicitly:

> NixOS tests using QEMU virtual machine `nodes` require virtualization support. This means that the machine must have `kvm` in its system features list, or `apple-virt` in case of macOS. These features are autodetected locally, but `apple-virt` is only autodetected since Nix 2.19.0.
> — [running-nixos-tests.section.md:22-26](https://github.com/NixOS/nixpkgs/blob/master/nixos/doc/manual/development/running-nixos-tests.section.md#L22-L26)

The outer VM's QEMU command line, for `x86_64-linux`:

```nix
x86_64-linux = "${qemuPkg}/bin/qemu-system-x86_64 -machine accel=${accel "kvm"} -cpu max";
```
— [nixos/lib/qemu-common.nix:42](https://github.com/NixOS/nixpkgs/blob/master/nixos/lib/qemu-common.nix#L42)

with `accel = accelName: if forceAccel then accelName else "${accelName}:tcg"` ([qemu-common.nix:39](https://github.com/NixOS/nixpkgs/blob/master/nixos/lib/qemu-common.nix#L39)) — i.e. **KVM with silent TCG fallback by default**. `virtualisation.qemu.forceAccel` defaults to `false` ([qemu-vm.nix:756-765](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/virtualisation/qemu-vm.nix#L756-L765)); enabling it turns a silent 20× slowdown into a hard error with a helpful message about the `kvm` group ([qemu-vm.nix:298-311](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/virtualisation/qemu-vm.nix#L298-L311)). **Set `virtualisation.qemu.forceAccel = true` in talos-box's tests** — a talos-box e2e test that silently falls back to TCG will time out mysteriously rather than fail clearly.

`-cpu max` is what makes nested KVM plausible: it exposes the host CPU's full feature set including `vmx`/`svm`, *provided* the host kernel has `kvm_intel.nested=1` / `kvm_amd.nested=1`. **Confidence: medium.** I did not find an in-tree NixOS test that boots a nested KVM guest and asserts hardware acceleration, and the nested-virt knob is a host kernel module parameter that the test framework does not control. Plan for two tiers:

- **Tier A (always runs, in `checks`):** boot a NixOS VM with `virtualisation.talosbox.enable = true`, assert the units start, the socket exists with mode `0660` and group `talosbox`, `tbx doctor` passes its non-VM checks, the bridge/tap gets created, and `tbx` can reach `tbxd` which can reach the helper. No nested guest. This validates 90% of the module.
- **Tier B (opt-in, self-hosted runner):** actually boot a Talos guest nested. Gate it behind a flake check that is not in `checks.<system>` by default.

GitHub-hosted Linux runners do not expose `/dev/kvm`, so even Tier A needs a self-hosted or KVM-enabled runner, or must accept TCG (slow but functional if `forceAccel = false`). **Confidence: medium** on current GitHub runner capabilities — verify before wiring CI.

---

## 5. Getting into nixpkgs proper

Recommendation: **flake first, nixpkgs later, and only for the package.** Concretely:

- Publish `flake.nix` in the repo now. Consumers get `packages` + `nixosModules.default` immediately with zero review latency.
- Once the Linux port is stable, submit `pkgs/by-name/ta/talos-box/package.nix` to nixpkgs. This is low-friction (a `buildGoModule` with `subPackages` and `ldflags`) and gets you `nixpkgs-update` bot bumps and binary cache coverage.
- Submitting the **NixOS module** to nixpkgs is a much bigger commitment: it means matching nixpkgs' release cadence, an in-tree `nixos/tests/talos-box.nix` that must pass on Hydra, and a static GID in `ids.nix`. Defer until the module's option surface has stopped moving. Keep the flake's `nixosModules.default` as the single source of truth until then.

Keeping the module out-of-tree also means you keep the freedom to `import` it from the flake with the flake's own package pinned, which is a real advantage during the port.

---

## Open questions / risks

1. **Does `tbx-helper` actually need `User=root`?** Unresolved. It writes DNS config and needs `/dev/kvm`. If DNS integration can go through `systemd-resolved` (D-Bus `SetLinkDNS`) or `resolvconf` instead of writing `/etc/resolv.conf`, a non-root helper with three ambient caps plus `SupplementaryGroups=[ "kvm" ]` becomes achievable — a materially better security posture than the current root launchd daemon. **Recommend prototyping this early**, because it constrains the DNS design.
2. **`/etc/resolv.conf` on NixOS is managed.** Depending on config it's a symlink into `/run/systemd/resolve/` or generated by `networking.resolvconf`. A helper that writes it directly will be clobbered or will fail under `ProtectSystem=strict`. NixOS-specific and needs its own design pass; I did not research NixOS's resolvconf module in depth here. **Confidence: high that this is a problem, low on the best fix.**
3. **Nested KVM in `nixosTest`** — see §4. Unconfirmed by an in-tree example. Budget time to discover this empirically rather than assuming.
4. **`DEV_KVM_MODE` numeric default** — could not fetch systemd's `meson.options`; nixpkgs does not override it. If it's `0666`, `kvm` group membership is unnecessary on NixOS; if `0660`, it's mandatory. Handle both.
5. **BGP on `:179` and DNS on `:53`** need `CAP_NET_BIND_SERVICE`, which is fine — but if the helper drops to a non-root user *and* `tbxd` (unprivileged, per-user) is the one that wants to serve DNS, the port has to be bound by the helper and the fd passed, or moved to a high port with an nftables redirect. Not resolved; affects the helper's IPC surface.
6. **`ids.nix` GID collision.** If talos-box later lands in nixpkgs with a static `talosbox` GID, existing flake users who got a dynamically-allocated GID will need a migration. Cheap mitigation: never claim a static GID from the flake; use `users.groups.talosbox = { };` as incus/podman do.
7. **arm64 flake support** — `qemu_kvm` on `aarch64-linux` is fine, but `nixos/lib/qemu-common.nix` shows aarch64 guests need `-machine virt,gic-version=max` ([qemu-common.nix:44](https://github.com/NixOS/nixpkgs/blob/master/nixos/lib/qemu-common.nix#L44)). Whatever QMP command line talos-box builds must special-case aarch64 similarly. Not NixOS-specific but easy to miss.
8. **`security.wrappers` on non-NixOS Nix (nix-darwin, Home Manager)** does not exist. If talos-box ever wants a Home-Manager-only install path on a non-NixOS Linux distro, the privileged helper cannot be provisioned by Nix at all — it needs the deb/rpm/AUR path. Worth stating explicitly in docs so users don't expect `nix profile install` to give them a working cluster.

## Sources

- [nixos/modules/virtualisation/libvirtd.nix](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/virtualisation/libvirtd.nix) — `virtualisation.libvirtd` namespace, `libvirtd` group, `qemu.runAsRoot`, `security.wrappers.qemu-bridge-helper`, `/etc/qemu/bridge.conf`, polkit requirement, `systemd.sockets.virtlogd`/`virtlockd`.
- [nixos/modules/security/wrappers/default.nix](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/security/wrappers/default.nix) — full option schema, `mkSetcapProgram`/`mkSetuidProgram` activation scripts, `cap_setpcap` ambient trick, `/run/wrappers` tmpfs, setuid⊕capabilities assertion.
- [nixos/modules/virtualisation/incus.nix](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/virtualisation/incus.nix) — socket activation with `SocketMode`/`SocketGroup`, `users.groups`, nftables assertion, `boot.kernel.sysctl`, `boot.kernelModules`, `--group` daemon flag.
- [nixos/modules/virtualisation/podman/default.nix](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/virtualisation/podman/default.nix) — `systemd.user.services`/`systemd.user.sockets` for a per-user daemon, `users.groups.podman`, per-interface firewall ports for DNS.
- [nixos/modules/services/networking/nat.nix](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/services/networking/nat.nix) — `mkOverride 99` forwarding sysctls.
- [nixos/modules/services/networking/firewall.nix](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/services/networking/firewall.nix) — `networking.firewall.trustedInterfaces`.
- [nixos/modules/misc/ids.nix](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/misc/ids.nix) — `kvm = 302; # default udev rules from systemd requires these`.
- [nixos/modules/virtualisation/qemu-vm.nix](https://github.com/NixOS/nixpkgs/blob/master/nixos/modules/virtualisation/qemu-vm.nix) — `virtualisation.qemu.forceAccel`, `/dev/kvm` preflight checks.
- [nixos/lib/qemu-common.nix](https://github.com/NixOS/nixpkgs/blob/master/nixos/lib/qemu-common.nix) — `-machine accel=kvm:tcg -cpu max` per-architecture matrix.
- [nixos/doc/manual/development/running-nixos-tests.section.md](https://github.com/NixOS/nixpkgs/blob/master/nixos/doc/manual/development/running-nixos-tests.section.md) — `kvm` system feature requirement, macOS remote builder note.
- [nixos/doc/manual/development/writing-nixos-tests.section.md](https://github.com/NixOS/nixpkgs/blob/master/nixos/doc/manual/development/writing-nixos-tests.section.md) — `pkgs.testers.runNixOSTest` outside nixpkgs.
- [doc/languages-frameworks/go.section.md](https://github.com/NixOS/nixpkgs/blob/master/doc/languages-frameworks/go.section.md) — `buildGoModule`, `vendorHash` (incl. `null` + `lib.fakeHash`), `ldflags`, `subPackages`, `tags`, `proxyVendor`, `env.CGO_ENABLED`.
- [pkgs/by-name/li/lima/package.nix](https://github.com/NixOS/nixpkgs/blob/master/pkgs/by-name/li/lima/package.nix) — Go VM manager packaged with `buildGoModule` + `wrapProgram --prefix PATH` for QEMU.
- [nix/src/libstore/posix-fs-canonicalise.cc](https://github.com/NixOS/nix/blob/master/src/libstore/posix-fs-canonicalise.cc) — store path mode canonicalisation to 0444/0555.
- [`nix flake check` reference](https://nix.dev/manual/nix/latest/command-ref/new-cli/nix3-flake-check.html) — the validated flake output attribute schema.
- [systemd.exec(5)](https://man7.org/linux/man-pages/man5/systemd.exec.5.html) — `AmbientCapabilities`, `CapabilityBoundingSet`, `NoNewPrivileges`, `DynamicUser`, `ProtectSystem`, `PrivateTmp`.
- [capabilities(7)](https://man7.org/linux/man-pages/man7/capabilities.7.html) — ambient set semantics, file capability sets, `CAP_NET_ADMIN`/`CAP_NET_RAW`/`CAP_NET_BIND_SERVICE`.
- [systemd rules.d/50-udev-default.rules.in](https://github.com/systemd/systemd/blob/main/rules.d/50-udev-default.rules.in) — `/dev/kvm`, `/dev/vhost-net`, `/dev/vhost-vsock` group/mode rules.
