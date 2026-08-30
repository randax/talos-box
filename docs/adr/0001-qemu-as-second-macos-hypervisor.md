---
status: accepted
---

# QEMU as a second macOS hypervisor, chosen per cluster

Virtualization.framework (VZ) has shown stability problems on macOS, and it cannot report the guest-visible balloon size or restore a suspended guest across a daemon restart. We add QEMU/HVF as a second **hypervisor** on macOS rather than replacing VZ: it is chosen per cluster (`clusters[].hypervisor`), immutable after create, with VZ remaining the compiled default. QEMU is user-installed and reported as a capability gate, never bundled.

## Considered options

- **Replace VZ with QEMU.** Rejected: QEMU depends on a Homebrew/Nix binary the user must install and on macOS 15+ for an HVF-enabled Homebrew build; VZ works out of the box and must remain the zero-setup path.
- **Per-host hypervisor switch.** Rejected: A/B-ing stability requires running both side by side, and a per-host switch would force every cluster through a migration to test one.
- **QEMU's own `-netdev vmnet-shared`.** Rejected: it requires the whole `qemu-system-*` process to run as root; Apple restricts the `com.apple.vm.networking` entitlement to approved vendors and Homebrew will not ship one. Instead the privileged helper keeps owning the vmnet interface and QEMU consumes the same datagram FD VZ does via `-netdev dgram,local.type=fd`.
- **Bundling QEMU in the signed release.** Rejected for now: large notarization and licensing surface for a second substrate; revisit on demand.

## Consequences

- One host substrate can run clusters on different hypervisors, so every capability (balloon readback, suspend surviving restart, guest agent) is resolved **per cluster/node**, never host-wide.
- The helper is untouched; the QEMU backend is shared with Linux and differs on macOS only in netdev, accelerator and firmware paths.
- Two latent bugs found while researching become prerequisites: the vmnet socketpair's 2 KiB send buffer (#528) and the hex-encoded QMP migrate offset (#529).
