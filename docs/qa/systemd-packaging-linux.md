# QA Runbook: Packaged systemd integration on Linux

| | |
|---|---|
| **Tier** | Deep (packaged Linux host integration) |
| **Platform** | Linux with systemd + QEMU/KVM |
| **Estimated duration** | 45–90 min |
| **Destructive** | **Yes** — installs checkout binaries and package assets under `/usr`, creates system users/state, enables units, and creates/destroys clusters `qa-sd-a` and `qa-sd-b` |
| **Runbook version** | against talos-box main @ the commit recorded in your report |

## How to execute this runbook (agent instructions)

This is a disposable-host harness, not a workstation smoke test. It validates the checked-in package against a real systemd PID 1 and deliberately mutates system paths. Run it only on a fresh host that can be discarded afterwards. Record **PASS**, **FAIL**, or **PASS-with-friction** and retain the harness diagnostics for every failure.

**Report destination**: one `qa-run` issue, title `QA systemd-packaging-linux <distro> <date>`.

## Scope

This lane covers the Linux package installation and live system/user unit boundary that the init-agnostic CI KVM harnesses intentionally cannot cover. It exercises the packaged helper user, capabilities, socket activation, persistent reservation state, DHCP and nftables convergence, real two-cluster forwarding, helper restart recovery, periodic daemon reconciliation, and supervised daemon restart refusal.

## Prerequisites

BLOCKED unless all of the following are true:

- The host is disposable and PID 1 is systemd in `running` or `degraded` state.
- `/dev/kvm` is writable and the current login session has active `tbx` and `kvm` group membership. Log out and back in after adding either group.
- At least 4 logical CPUs, 8 GiB available memory, and 40 GiB free disk are available.
- `sudo` authorization is available for installation and failure diagnostics; authorize it at the harness prompt.
- Go, QEMU/KVM, nftables, iproute2, curl, jq, make, systemd-sysusers, and the other commands named by the harness preflight are installed.
- No clusters are configured under the current user's `~/.talosbox/clusters` directory.
- Outbound network access is available for Talos images and curated Flannel provisioning dependencies.

The harness refuses to proceed without a deliberately alarming opt-in flag. The flag acknowledges system mutation; it is not a bypass for any other preflight refusal.

## Execute

From the repository root, run:

```sh
bash scripts/qa/linux-systemd-packaging-e2e.sh --i-know-this-installs-system-files
```

Do not run the helper or daemon by hand alongside the harness. Do not substitute a container whose PID 1 is not systemd.

## What the harness asserts

1. The checkout builds and its three binaries are installed under `/usr/bin`; the `packaging/linux/` tree is copied verbatim, sysusers is applied, both managers are reloaded, and packaged helper/daemon sockets are enabled.
2. The live helper runs as `tbx:tbx`, has exactly `CAP_NET_ADMIN`, `CAP_NET_BIND_SERVICE`, and `CAP_NET_RAW` in its effective and bounding sets, uses a mode-`0700` state directory, and exposes the packaged mode-`0660` socket. Installed units and binaries match the checkout.
3. Cluster `qa-sd-a` enables global and bridge forwarding. Its reservation file is `tbx:tbx` mode `0600` and contains the cluster's actual node name, MAC, and reserved IP. DHCP listens on that cluster bridge, `table inet tbx` names its bridge/subnet, and the guest answers at its reserved address.
4. Minimal provisioned cluster `qa-sd-b` supplies a second real data plane. Direct VIP requests and `/dial` requests run in both directions, proving traffic crosses host routing/nftables rather than merely reading a sysctl or curling one host-local VIP.
5. A normal `systemctl restart tbx-helper.service` restores both DHCP listeners.
6. The stronger #514 sequence stops the helper, moves `reservations.json` aside, starts from empty persisted state, and polls for up to about 90 seconds for periodic `net.sync` to restore the file and both DHCP listeners.
7. A node stop/start after recovery returns to the same reserved address with the maintenance API reachable, requiring a fresh DHCP exchange.
8. An unforced `tbx system restart` is refused under supervision: both running cluster names precede `systemctl --user restart tbxd.service`, no `--force` suggestion appears, and the daemon PID is unchanged.
9. Both clusters are destroyed and their bridges, DHCP listeners, reservation entries, and nftables bridge/subnet entries disappear.

On failure the trap captures system and user unit status/journals, sockets, processes, host interfaces/routes/neighbors, bridge state, nftables/iptables, forwarding sysctls, reservation metadata/content, daemon logs, and both cluster statuses.

## Manual follow-ups

After a PASS, attach the full transcript and record the distro, kernel, systemd, QEMU, nft, Go, and talos-box commit versions. Confirm whether the host will be discarded. If it will be retained for further QA, run `tbx doctor` once and save the complete output; do not interpret that optional follow-up as part of this harness verdict.

The shared QA matrix should gain a Linux-only row for this runbook when the shared-documentation task integrates the batch. This task intentionally does not edit `docs/qa/MATRIX.md`.

## Known gaps

- This is operator-run and is not a merge-gating CI lane.
- It covers one real systemd distribution at a time; distro/package-manager breadth needs separate executions.
- It does not test upgrades, package-manager uninstall/rollback, SELinux/AppArmor policy, multi-user reservation partition collisions, suspend/resume, or host reboot persistence.
- It does not add or validate a `sysctl.d` asset: forwarding is converged dynamically by the helper.
- The harness leaves the packaged binaries, system user, enabled sockets, and helper state directory installed. Discarding the host is the supported cleanup.
