# Can helper-owned vmnet be bridged into unprivileged QEMU on macOS?

Research for [#521](https://github.com/randax/talos-box/issues/521) (map [#520](https://github.com/randax/talos-box/issues/520)), 2026-08-30.

## Answer

**Yes, and no new helper-side plumbing is needed.** The datagram FD the helper already hands
the VZ backend is byte-for-byte what QEMU's `-netdev dgram` wants: an `AF_UNIX` `SOCK_DGRAM`
socket carrying one raw Ethernet frame per datagram with no length prefix. QEMU consumes a
pre-opened FD via `local.type=fd,local.str=<n>`. The preferred design in the ticket works.

The `vmnet-shared` fallback is **not viable for an unprivileged QEMU**: it requires the entire
`qemu-system-*` process to run as root, and neither Homebrew nor nixpkgs ships (or will ship) a
QEMU signed with `com.apple.vm.networking`.

Verified empirically against the QEMU installed on this host (Homebrew `qemu` 11.1.1, arm64).

## 1. Can a `dgram` / `socket` / `stream` netdev carry frames from the helper-side bridge?

### What the helper produces today

`internal/helper/vmnet_darwin.go` starts vmnet in `VMNET_SHARED_MODE`, then:

- creates `socketpair(AF_UNIX, SOCK_DGRAM, 0)` (line 215) — `pump_fd` stays in the helper,
  `peer_fd` is the FD returned to the client as `AttachmentDatagramFD`;
- on `VMNET_INTERFACE_PACKETS_AVAILABLE`, `tbx_drain_vmnet` does
  `vmnet_read(...)` then `send(state->pump_fd, state->read_buffer, packet.vm_pkt_size, MSG_DONTWAIT)`
  — **one raw frame per datagram, no framing header**;
- the reverse path (`tbx_vmnet_write_frame`) takes one whole frame and `vmnet_write`s it.

`internal/helper/attachment.go` wraps that FD in an `*os.File` the caller owns.

### What `-netdev dgram` expects

From [`net/dgram.c`](https://gitlab.com/qemu-project/qemu/-/blob/master/net/dgram.c):

- `net_init_dgram()` accepts `SOCKET_ADDRESS_TYPE_FD` for `local`, resolving it with
  `monitor_fd_param()`. With `local.type=fd`, `dest_addr = NULL` / `dest_len = 0`, i.e. QEMU
  assumes an already-connected socket and uses plain `send()`. Setting `remote=` alongside
  `local.fd` is rejected: `"don't set remote with local.fd"`.
- TX (`net_dgram_receive`): `ret = send(s->fd, buf, size, 0);` — the guest frame goes out
  verbatim, **no length prefix, no virtio-net header**.
- RX (`net_dgram_send`): `size = recv(s->fd, s->rs.buf, sizeof(s->rs.buf), 0);` followed by
  `qemu_send_packet_async(&s->nc, s->rs.buf, size, ...)` — **one `recv()` is exactly one frame**.
  `net_fill_rstate()` is never called on this path, and `net_socket_rs_init(..., false)` sets
  `vnet_hdr = false`.

This is an exact match for the helper's socketpair. Contrast
[`net/socket.c`](https://gitlab.com/qemu-project/qemu/-/blob/master/net/socket.c#L80) in *stream*
mode and [`net/stream_data.c`](https://gitlab.com/qemu-project/qemu/-/blob/master/net/stream_data.c),
both of which prepend `uint32_t len = htonl(size);` and reassemble it in
[`net_fill_rstate()`](https://gitlab.com/qemu-project/qemu/-/blob/master/net/net.c#L2100).
Feeding the helper FD to `-netdev stream` would corrupt every frame.

Option spelling, from [`qemu-options.hx`](https://gitlab.com/qemu-project/qemu/-/blob/master/qemu-options.hx#L3088):

```
-netdev dgram,id=str,local.type=fd,local.str=file-descriptor
```

Note it is `local.str=`, **not** `fd=` — `-netdev dgram,id=n0,fd=3` is rejected with
`Parameter 'fd' is unexpected` (verified on 11.1.1).

`-netdev socket,fd=h` also works, and is arguably simpler: `net_init_socket` calls
`net_socket_fd_check(fd)`, sees `SOCK_DGRAM`, and routes to `net_socket_fd_init_dgram`, which is
unframed just like `dgram`. Both were verified to accept the socket type in question. `dgram` is
the modern spelling and is what should be used.

### Empirical verification (this host, Homebrew QEMU 11.1.1)

A Python harness created `socket.socketpair(AF_UNIX, SOCK_DGRAM)`, dup'd one end to fd 9 with
`set_inheritable(9, True)`, and exec'd:

```
qemu-system-aarch64 -machine virt -nographic -display none \
  -netdev dgram,id=n0,local.type=fd,local.str=9 \
  -device virtio-net-pci,netdev=n0 -S
```

QEMU started clean and stayed up — no error, no warning. The same harness with
`-netdev socket,id=n0,fd=9` also started clean. Negative controls confirm the parser is really
inspecting the FD: `local.str=0` against a non-socket stdin gives
`Unable to query local socket address: Socket operation on non-socket`, and
`-netdev socket,fd=0` gives `can't get socket option SO_TYPE`.

`-netdev help` on this host lists `socket`, `stream`, `dgram`, `vmnet-host`, `vmnet-shared`,
`vmnet-bridged`.

### Framing and MTU constraints

| Limit | Value | Source |
|---|---|---|
| QEMU receive buffer | `NET_BUFSIZE` = `4096 + 65536` = 69632 B | [`include/net/net.h:19`](https://gitlab.com/qemu-project/qemu/-/blob/master/include/net/net.h#L19) |
| QEMU truncation behaviour | silent — `recv()` without `MSG_TRUNC`, no truncation check | `net/dgram.c` |
| macOS `AF_UNIX` dgram default max datagram | `net.local.dgram.maxdgram` = 2048 | `sysctl` on this host |
| macOS `AF_UNIX` dgram max datagram with `SO_SNDBUF` raised | >= 200000 B (measured) | measured on this host |
| vmnet frame ceiling | `vmnet_max_packet_size_key`, read back at start | `vmnet_darwin.go:172`, enforced at `:327` |

So MTU is a non-issue at 1500 and would still be a non-issue at 9000, **provided the socket
buffers are sized**. QEMU never calls `setsockopt` for `SO_SNDBUF`/`SO_RCVBUF` in `net/dgram.c`
(only `SO_REUSEADDR` and the `IP_*` multicast options), so on a pre-opened FD the buffers are
whatever the creator left them at.

### The socket buffer default is the real hazard

Measured on this host, a fresh `socketpair(AF_UNIX, SOCK_DGRAM)`:

```
default SNDBUF 2048  RCVBUF 4096
default queued 1514B frames before EWOULDBLOCK: 2
```

**Two frames.** `tbx_drain_vmnet` sends with `MSG_DONTWAIT` and drops on backpressure (with a
power-of-two-throttled log line), so this is a live packet-loss source for the VZ path *today*,
not just a QEMU concern. `SO_SNDBUF`/`SO_RCVBUF` accept 64 KiB / 256 KiB / 1 MiB verbatim on
macOS (verified), so the fix is a `setsockopt` pair on both ends right after `socketpair()`.

Second gotcha: `vmnet_darwin.go:225` sets `FD_CLOEXEC` on `peer_fd`. Go's `exec.Cmd.ExtraFiles`
clears that for the child, so this is fine as long as the FD is passed through `ExtraFiles` (the
Linux `tap,fd=` path already does exactly this), but a raw `syscall.Exec` would silently lose it.

### Wiring change required

`internal/hypervisor/qemu_backend.go:131` currently hard-requires `helper.AttachmentTapFD`, and
`internal/hypervisor/qemu_config.go:286` emits `-netdev tap,id=net0,fd=<n>`. macOS needs the
darwin path to accept `AttachmentDatagramFD` and emit
`-netdev dgram,id=net0,local.type=fd,local.str=<n>` instead. Everything below that — the
helper, the vmnet interface, the `172.30.0.0/16` learning switch, DHCP — is unchanged and shared
with VZ.

## 2. Throughput and latency cost vs `-netdev vmnet-shared`

There is one extra hop: vmnet -> helper buffer -> `AF_UNIX` datagram -> QEMU, versus
vmnet -> QEMU directly. Measured cost of that hop on this host (Python, so an upper bound on
overhead — a C/Go sender is faster):

```
200000 frames of 1514B in 0.30s = 657k pps = 7.96 Gbit/s (single hop)
rtt over socketpair: 5.3 us
```

Two copies and ~2.5 us one-way per frame. For context, a 1 Gbit/s link at 1514 B is ~82k pps, so
the socketpair has roughly 8x headroom over line rate on a gigabit-class workload. **The extra
hop is not the bottleneck; the 2048-byte default `SO_SNDBUF` is.** With buffers raised, expect
the difference vs in-process `vmnet-shared` to be in the noise for a Talos control-plane
workload, and it is the identical datapath the VZ backend already ships on.

This is also the design lima uses: [socket_vmnet](https://github.com/lima-vm/socket_vmnet)
runs vmnet in a root daemon and relays frames to an unprivileged QEMU over a Unix socket —
"QEMU-builtin vmnet requires running the entire QEMU process as root. On the other hand,
socket_vmnet does not require the entire QEMU process to run as root."

## 3. Entitlement / root required for the `vmnet-shared` fallback

**QEMU must run as root.** There is no way around it with a distro-packaged QEMU.

- Apple: [`com.apple.vm.networking`](https://developer.apple.com/documentation/bundleresources/entitlements/com.apple.vm.networking)
  is "A Boolean that indicates whether the app manages virtual network interfaces **without
  escalating privileges to the root user**", "The entitlement is required to use the vmnet APIs",
  and "This entitlement is **restricted to developers of virtualization software**. To request
  this entitlement, contact your Apple representative." Root is the documented alternative path.
- QEMU source contains no `com.apple.vm.networking`. `scripts/entitlement.sh` signs with the
  single plist `accel/hvf/entitlements.plist`, which holds only `com.apple.security.hypervisor`.
  The only privilege statement in QEMU is the error string in
  [`net/vmnet-common.m`](https://gitlab.com/qemu-project/qemu/-/blob/master/net/vmnet-common.m):
  `case VMNET_FAILURE: return "general failure (possibly not enough privileges)";`
- QEMU's docs say **nothing** about vmnet privileges — no vmnet section in `qemu-options.hx`'s
  rST body, no mention on the [network emulation page](https://www.qemu.org/docs/master/system/devices/net.html).
  The statement lives in the patch series that added the backend
  ([qemu-devel 2021-02](https://lists.gnu.org/archive/html/qemu-devel/2021-02/msg04637.html)):
  "vmnet requires either a special entitlement, granted via a provisioning profile, or root
  access... Using this netdev currently requires that qemu be run with root access."
- [QEMU issue #1364](https://gitlab.com/qemu-project/qemu/-/issues/1364), "Support vmnet
  networking without elevated permissions", is **still open** (opened 2022-12-12).
- QEMU applies no per-mode privilege distinction. All three backends call the same
  `vmnet_start_interface()` in `net/vmnet-common.m:320`, differing only in
  `vmnet_operation_mode_key`. Any claim that host/shared works unprivileged while bridged does
  not is unsupported by the source.

### Is a Homebrew / Nix QEMU entitled? No.

Verified on this host:

```
$ codesign -d --entitlements - /opt/homebrew/bin/qemu-system-aarch64
[Dict]
	[Key] com.apple.security.hypervisor
	[Value]
		[Bool] true
$ codesign -dv /opt/homebrew/bin/qemu-system-aarch64
Signature=adhoc
TeamIdentifier=not set
```

`com.apple.security.hypervisor` only. And unprivileged `vmnet-shared` fails exactly as predicted:

```
$ qemu-system-aarch64 -machine virt -netdev vmnet-shared,id=n0 ...
qemu-system-aarch64: -netdev vmnet-shared,id=n0: cannot create vmnet interface: general failure (possibly not enough privileges)
```

- **Homebrew** ([`Formula/q/qemu.rb`](https://github.com/Homebrew/homebrew-core/blob/master/Formula/q/qemu.rb)):
  no `--enable-vmnet`/`--disable-vmnet` — the meson option is `value: 'auto'`, so vmnet *is*
  compiled in; the capability exists, the privilege does not. No codesign step in `install`;
  Homebrew's bottle relocation ad-hoc signs with
  `--preserve-metadata=entitlements,requirements,flags,runtime`, which never *adds* an
  entitlement. Homebrew's position, [discussion #5744](https://github.com/orgs/Homebrew/discussions/5744):
  **"We do not have a mechanism to do this, nor would we want to be distributing provisioning profiles."**
- **nixpkgs** ([`pkgs/by-name/qe/qemu/package.nix`](https://github.com/NixOS/nixpkgs/blob/master/pkgs/by-name/qe/qemu/package.nix)):
  zero `vmnet` hits, darwin flags are `--enable-cocoa --enable-hvf` only. It uses
  `darwin.sigtool` and `dontStrip = isDarwin` purely to avoid voiding QEMU's *own* ad-hoc
  hypervisor entitlement. Adds nothing.

Even if we self-signed, an ad-hoc signature cannot carry a restricted entitlement — that needs a
provisioning profile from an Apple-approved team.

## 4. Minimum QEMU version

| Option | Min version | Evidence |
|---|---|---|
| `-netdev dgram,local.type=fd,local.str=` | **7.2** | `qapi/net.json`: `# @dgram: since 7.2`; `net/dgram.c` created by [`5166fe0ae46d`](https://gitlab.com/qemu-project/qemu/-/commit/5166fe0ae46d) (2022-10-28); file 404 at `v7.1.0`, present at `v7.2.0` |
| `-netdev stream,addr.type=fd,addr.str=` | 7.2 | `qapi/net.json`: `# @stream: since 7.2`; fd form present in `v7.2.0` `qemu-options.hx` |
| `-netdev socket,fd=` (auto-detects `SOCK_DGRAM`) | 1.2 | `qapi/net.json` `NetdevSocketOptions`, `'*fd': 'str'`, `Since: 1.2` |
| `-netdev vmnet-shared` / `-host` / `-bridged` | **7.1** | `qapi/net.json` `Since: 7.1`; commits `81ad2964e938`, `73f99db534e3` (2022-05-17); `net/vmnet-shared.c` 404 at `v7.0.0` |

QEMU 7.2 shipped 2022-12; Homebrew is on 11.1.1. A `>= 7.2` floor is unproblematic, and
`-netdev socket,fd=` is available as a compatibility escape hatch back to 1.2 if a version floor
ever bites (same wire framing, since the FD is `SOCK_DGRAM`).

## 5. Why the helper cannot hand QEMU the vmnet interface itself

Confirmed against `vmnet.h` (MacOSX26.sdk and MacOSX12.1.sdk): **there is no file descriptor in
the vmnet API at all.** `grep -i "fd\|socket\|file descriptor"` over the 1401-line header yields
one hit, and it is about the `fd00::/8` ULA prefix. The handle is
`typedef struct vmnet_interface *interface_ref` — an opaque pointer backed by a private XPC
connection established inside `vmnet_start_interface`, with copy-based I/O
(`vmnet_interface_set_event_callback` / `vmnet_read` / `vmnet_write`). It is not dup-able, not
sendable over `SCM_RIGHTS`, and there is no `vmnet_get_fd` equivalent.

This is why the relay is mandatory rather than a design choice: a privileged helper can own the
interface, but it must stay in the datapath and shuttle *packets*. The FD we pass is our own
socketpair end, never anything from vmnet. `vmnet.h` also documents no per-mode privilege
difference — the header has zero hits for "root", "entitle", or "privile", and all three modes go
through the same `vmnet_start_interface`.

## Recommendation

Take the preferred path. Concretely:

1. On darwin, let `qemu_backend.go` accept `helper.AttachmentDatagramFD` and have
   `qemu_config.go` emit `-netdev dgram,id=net0,local.type=fd,local.str=<n>` (the tap form stays
   for Linux). Pass the FD via `exec.Cmd.ExtraFiles`, as the Linux path already does.
2. **Raise `SO_SNDBUF`/`SO_RCVBUF` on both socketpair ends in `tbx_vmnet_start`** (256 KiB-1 MiB).
   This is a standalone bug fix that benefits the VZ backend today — the current default queues
   two 1514-byte frames before `tbx_drain_vmnet` starts dropping.
3. Set the minimum QEMU version to 7.2 on darwin, and surface a clear error below that.
4. Drop `-netdev vmnet-shared` as a fallback. It would require running every `qemu-system-*` as
   root, and Homebrew has stated it will not ship the entitlement. If a fallback is wanted at
   all, `-netdev socket,fd=` on the same helper FD is the compatible one.

## Sources

- QEMU: [`net/dgram.c`](https://gitlab.com/qemu-project/qemu/-/blob/master/net/dgram.c),
  [`net/socket.c`](https://gitlab.com/qemu-project/qemu/-/blob/master/net/socket.c),
  [`net/stream_data.c`](https://gitlab.com/qemu-project/qemu/-/blob/master/net/stream_data.c),
  [`net/net.c`](https://gitlab.com/qemu-project/qemu/-/blob/master/net/net.c#L2100),
  [`include/net/net.h`](https://gitlab.com/qemu-project/qemu/-/blob/master/include/net/net.h#L19),
  [`qemu-options.hx`](https://gitlab.com/qemu-project/qemu/-/blob/master/qemu-options.hx),
  [`qapi/net.json`](https://gitlab.com/qemu-project/qemu/-/blob/master/qapi/net.json),
  [`net/vmnet-common.m`](https://gitlab.com/qemu-project/qemu/-/blob/master/net/vmnet-common.m),
  [`net/vmnet-shared.c`](https://gitlab.com/qemu-project/qemu/-/blob/master/net/vmnet-shared.c),
  [`net/vmnet-bridged.m`](https://gitlab.com/qemu-project/qemu/-/blob/master/net/vmnet-bridged.m),
  commits [`5166fe0ae46d`](https://gitlab.com/qemu-project/qemu/-/commit/5166fe0ae46d) /
  [`73f99db534e3`](https://gitlab.com/qemu-project/qemu/-/commit/73f99db534e3),
  [issue #1364](https://gitlab.com/qemu-project/qemu/-/issues/1364),
  [qemu-devel patch series](https://lists.gnu.org/archive/html/qemu-devel/2021-02/msg04637.html)
- Apple: [`com.apple.vm.networking`](https://developer.apple.com/documentation/bundleresources/entitlements/com.apple.vm.networking),
  [`vmnet_start_interface`](https://developer.apple.com/documentation/vmnet/),
  `MacOSX26.sdk/.../vmnet.framework/Headers/vmnet.h`
- Packaging: [Homebrew `qemu.rb`](https://github.com/Homebrew/homebrew-core/blob/master/Formula/q/qemu.rb),
  [Homebrew `keg.rb`](https://github.com/Homebrew/brew/blob/master/Library/Homebrew/extend/os/mac/keg.rb),
  [Homebrew discussion #5744](https://github.com/orgs/Homebrew/discussions/5744),
  [nixpkgs `qemu/package.nix`](https://github.com/NixOS/nixpkgs/blob/master/pkgs/by-name/qe/qemu/package.nix)
- Prior art: [lima-vm/socket_vmnet](https://github.com/lima-vm/socket_vmnet)
- This repo: `internal/helper/vmnet_darwin.go`, `internal/helper/attachment.go`,
  `internal/hypervisor/qemu_backend.go`, `internal/hypervisor/qemu_config.go`
- Local measurements: Homebrew QEMU 11.1.1 arm64, macOS 25.6.0
