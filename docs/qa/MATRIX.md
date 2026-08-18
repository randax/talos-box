# QA coverage matrix

The named matrix for agent-run QA (map [#211](https://github.com/randax/talos-box/issues/211), decided in [#215](https://github.com/randax/talos-box/issues/215)). Every cell is explicit so gaps are visible. Cell values: **covered** (runbook executes there), **platform-N/A** (feature doesn't exist there — with the honest reason), **gap** (should be covered, isn't yet).

All runbooks follow the format decided in [#214](https://github.com/randax/talos-box/issues/214); results land as one `qa-run` issue per run.

## Runbook × platform

| Runbook | macOS (vz) | Linux (QEMU/KVM) | Notes |
|---|---|---|---|
| [smoke-macos](smoke-macos.md) | covered | platform-N/A | macOS-specific preflight/DNS mechanics |
| [smoke-linux](smoke-linux.md) | platform-N/A | covered | Linux-specific preflight (kvm, units, resolvectl) |
| [deep-cilium](deep-cilium.md) | covered | covered | C4 failover expectations differ per platform; on macOS a zero-outage result (no VIP outage at 2 s polling, post-GARP) is expected, with ARP revalidation up to ~60 s as the fallback |
| [deep-flannel](deep-flannel.md) | covered | covered | |
| [deep-storage](deep-storage.md) | covered | covered | |
| [deep-domains-dns](deep-domains-dns.md) | covered | covered | C4 hygiene charter is split: resolver files (macOS) vs resolved registration (Linux) |
| [deep-mirror-offline-cache](deep-mirror-offline-cache.md) | covered | covered | port-5000/AirPlay check is macOS-only |
| [deep-cluster-state](deep-cluster-state.md) | covered | covered | Suspend charters gate differently: macOS 14+/same-daemon vs QEMU 8.2+/identity; QEMU 6.2–8.1 hosts run the refusal check only |
| [deep-multicluster](deep-multicluster.md) | covered | covered | BGP-as-fast-failover comparison is macOS-flavored |
| [deep-host-integration](deep-host-integration.md) | covered | covered | C2 ballooning/overcommit is **macOS-only by implementation** — Linux side verifies the honest SKIP/absence |
| [scenario-offline-venue](scenario-offline-venue.md) | covered | covered | |
| [scenario-suspend-storage](scenario-suspend-storage.md) | covered (macOS 14+) | covered (QEMU 8.2+) | Refusal-only on older QEMU |
| [scenario-snapshot-provisioned](scenario-snapshot-provisioned.md) | covered | covered | Linux may exercise the full-copy fallback — disk-space preflight is higher |
| [scenario-multicluster-stack](scenario-multicluster-stack.md) | covered | covered | |
| [scenario-power-user-fork](scenario-power-user-fork.md) | covered | covered | |

## Known coverage gaps (deliberate, tracked)

| Gap | Why it is not covered | Where it lives |
|---|---|---|
| G4 — mirror through corporate security agents (GlobalProtect) | Needs a managed machine; `cache warm --check` explicitly does not prove it | SPEC §12 G4, open gate |
| G1 — macOS 14/15 boot floor | Needs physical macOS 14/15 hardware | v1 map ticket [#18](https://github.com/randax/talos-box/issues/18), non-blocking |
| Talos dashboard TUI interactivity on hvc0 | Logs verified; interactive rendering never verified (G6 residual) | deep-cilium/console charters observe logs only |
| Repeated-GARP bursts under vmnet | G2 residual; no harness | uncovered |
| Linux QEMU-identity-mismatch restore degradation | Requires two QEMU versions on one host | scenario-suspend-storage C4 marks it SKIPPED |
| Per-cluster Talos version / curated extensions ([#201](https://github.com/randax/talos-box/issues/201)) | Not yet shipped; runbooks cover main | new deep runbook when it lands (map #211 out-of-scope note) |
| Best-effort distros (Debian, openSUSE) and non-Ubuntu tier-one | Runbooks are platform-parameterized; no hardware in the executed set yet | same runbooks, new hardware |

## Tier semantics

- **Smoke**: default everything, one cluster, happy path — run per release and after substrate-touching changes.
- **Deep**: one feature area against defaults — run per release per platform.
- **Scenario**: hand-picked high-risk combinations — run pre-release and pre-venue.

(Cadence is provisional until the pilot (#217) settles it — map #211 fog.)
