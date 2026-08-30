# Design: QEMU as a second macOS hypervisor

Consolidates the decisions on the wayfinder map [randax/talos-box#520](https://github.com/randax/talos-box/issues/520). Decision: [ADR 0001](adr/0001-qemu-as-second-macos-hypervisor.md). Research: [vmnet bridge](research/2026-08-30-qemu-macos-vmnet-bridge.md), [HVF capabilities](research/2026-08-30-qemu-macos-hvf-capabilities.md).

## Goal

QEMU/HVF as a user-selectable **hypervisor** on macOS alongside Virtualization.framework (VZ). VZ stays the default. Motivation: VZ stability; QEMU additionally offers balloon readback and restart-surviving suspend, which VZ cannot.

**Parity bar** (must work on darwin-QEMU): start/stop/status, memory balloon, suspend/resume, multi-cluster and inter-cluster paths, the provisioning path. Guest agent is capability-gated.

**Platforms**: Apple Silicon (`darwin/arm64`, HVF) is the verified target. Intel Macs (`darwin/amd64`, HVF) are best-effort: built via `runtime.GOARCH` like Linux, gated on `kern.hv_support`, community-verified, never on the parity bar.

**Out of scope**: x86_64 guests via TCG on Apple Silicon; bundling QEMU in the release; replacing VZ; migrating an existing cluster's disk between hypervisors.

## Prerequisites (land standalone, before any QEMU work)

1. [#528](https://github.com/randax/talos-box/issues/528) — `tbx_vmnet_start` must `setsockopt(SO_SNDBUF/SO_RCVBUF)` on both socketpair ends (default 2 KiB queues two frames, then drops). Affects VZ today; QEMU never adjusts a pre-opened fd.
2. [#529](https://github.com/randax/talos-box/issues/529) — send the QMP migrate channel offset as a JSON integer; make the fake QMP server decode it as `uint64`.

## Selection

| Layer | Surface |
|---|---|
| Cluster | `clusters[].hypervisor: vz \| qemu` in `talosbox.yaml` (per cluster only; no file-level block) |
| Host default | `TBX_HYPERVISOR=<name>` on the daemon, else compiled default (`vz` on darwin/arm64, `qemu` on linux) |

Precedence: cluster key > `TBX_HYPERVISOR` > compiled default.

**State**: `cluster.Cluster.Hypervisor string \`json:"hypervisor,omitempty"\``, written at create beside `ImageArchitecture`. Empty = the platform's compiled default (state never crosses platforms). Immutable: `tbx up` refuses a yaml/state mismatch with "hypervisor is immutable; destroy and recreate", as for cluster domain. A VZ cluster is untouched when the host default flips: state wins and a silent yaml says nothing.

**Daemon**: `hypervisor.New` becomes an eager registry, `NewAll(ctx) map[Name]Hypervisor`. Every backend the platform knows is constructed at daemon start; an unavailable one is recorded as a capability gate with a reason, never a start failure. Creating a cluster on an unavailable backend fails with the gate's reason. `Capabilities()`/`Architecture()` are per backend; every current `s.hypervisor` consumer (image architecture at create, balloon readback, suspend staleness, guest-agent gate, doctor) resolves through the cluster's hypervisor.

**QEMU availability on macOS** (three-way probe, distinct remediation each):
- binary: `qemu-system-<arch>` on PATH → "brew install qemu"
- `-accel help` lists `hvf` → else "HVF not built in: Homebrew builds without HVF on macOS 14; needs macOS 15+"
- `kern.hv_support`=1 and the `com.apple.security.hypervisor` signature → else "HVF denied"

**Reporting**: `tbx doctor` gains a *Hypervisors* section: one line per backend (availability + reason/remediation), the effective default and its source, and per-backend feature gates (balloon readback, suspend survives restart, guest agent). `tbx status` shows the hypervisor per cluster via a new additive `omitempty` info field (no protocol bump); `ProtocolVersion` 20→21 only if doctor needs a new operation shape.

## Backend

One shared QEMU backend (`qemu_backend/config/process/save/probe.go`, `qmp.go`) plus a thin `backend_qemu_darwin.go` alongside `backend_qemu_linux.go`. The darwin layer differs only in:

- **Accelerator**: `-accel hvf`; `-cpu host`; GICv3 only — `gic-version=host` is KVM-only and must never reach the macOS path.
- **Firmware**: `edk2-aarch64-code.fd` + `edk2-arm-vars.fd` (both 64 MiB) under `$(brew --prefix qemu)/share/qemu/` and the nixpkgs equivalent; the aarch64 code volume deliberately pairs with the 32-bit ARM vars template. Same candidate-list shape as `qemuFirmwareCandidates` on NixOS. Do not rely on Homebrew's descriptor JSON (hardcodes versioned Cellar paths).
- **Networking**: see below.

Version floors unchanged (QEMU 6.2 / 8.2 already). Real constraint is macOS 15 for an HVF-enabled Homebrew QEMU.

## Networking

The helper is untouched. It owns the per-subnet shared-mode vmnet interface and the per-node `AF_UNIX SOCK_DGRAM` socketpair; QEMU consumes exactly the FD VZ does and never runs privileged.

The QEMU backend drops its `AttachmentTapFD`-only check and maps attachment kind → netdev in `qemu_config.go`:

| `helper.AttachmentKind` | netdev |
|---|---|
| `tap-fd` (Linux) | `-netdev tap,fd=N` (unchanged) |
| `datagram-fd` (macOS) | `-netdev dgram,id=net0,local.type=fd,local.str=N` |

The FD is passed via `exec.Cmd.ExtraFiles` (clears the helper's `FD_CLOEXEC`). Framing matches exactly: one raw frame per datagram, no header (`-netdev stream` must not be used; it prepends a length). Measured cost of the extra hop is negligible (~8 Gbit/s at 1514 B).

MAC goes on `-device virtio-net-pci,mac=` as on Linux; vmnet learns it, so DHCP lease read-back (`lease_read_darwin.go`, keyed by MAC + subnet index), `/etc/resolver` DNS and synced reservations are unchanged. Clusters on different hypervisors are indistinguishable at the vmnet layer, so mixed VZ+QEMU hosts and their inter-cluster paths need no extra work.

## Memory and suspend

- darwin-QEMU is a **full balloon citizen**, not size-by-hand: balloon reserve, pre-balloon and balloon hold apply as on Linux. `BalloonReadback` is resolved **per node** from its cluster's hypervisor (`Balloonables`/`balloonCandidatesLocked` stop reading one host-wide capability), so a mixed host manages VZ nodes conservatively (TLS-observed only, maintenance nodes exempt) and QEMU nodes directly in one reconcile. `TBX_DISABLE_BALLOON` keeps working via `Spec.DisableBalloon`.
- `savedStateStale` becomes **per cluster**: QEMU saves survive a daemon restart (versioned save file with backend/architecture/machine header); VZ keeps the owner-sidecar check. The stale wording in `tbx status` applies only to VZ clusters. Suspend is on the parity bar (full migrate-to-file round trip verified under HVF). The glossary's *Suspended* term is unchanged.

## Verification

- **CI** (hosted runners have neither VZ nor HVF): unit + fake-QEMU on the existing `macos-15` job. Backend tests become build-tag-neutral; darwin cases added (dgram netdev emission, firmware candidates, three-way HVF probe against a fake `-accel help`); registry tests (daemon starts with QEMU unavailable, doctor reports the gate); fake QMP tightened to `uint64` offsets.
- **Local battery**: `make e2e` parameterized by `TBX_E2E_HYPERVISOR` (injects `hypervisor:` into the generated yaml; **skips** when doctor reports the hypervisor unavailable); `make e2e-all` runs both. New cases: balloon readback on a maintenance-mode node; suspend → `tbxd` restart → resume without cold boot; doctor Hypervisors section; `tbx up` refusing drift; mixed host (one VZ + one QEMU cluster, inter-cluster path both directions).
- **Runbook**: `docs/qa/deep-hypervisor-qemu.md` in the existing QA format: Homebrew-on-Sonoma remediation, and a 24h VZ-vs-QEMU stability soak on the same cluster spec.
- **Intel Macs**: unit/fake tests only; `docs/macos.md` marks them community-verified (`qa-run` issues); doctor says `qemu: available (best-effort platform)`.

## Open (not blocking implementation)

- Self-hosted Apple Silicon runner: only if darwin-QEMU regresses on release twice.
- Nix module: pinning QEMU on darwin.
