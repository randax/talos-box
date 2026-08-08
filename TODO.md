# Talos Box open-issues implementation plan

Date: 2026-08-07

## Requirements summary

Plan every currently open issue in `randax/talos-box`, distinguish executable leaf work from specs/maps/human gates, and order delivery by product priority plus native and inferred dependencies. The recommended product order is:

1. Prevent confusing data-loss/corruption failure modes and repair manifest correctness.
2. Finish and validate the existing macOS v1 promise.
3. Land Linux support through its already-designed dependency graph.
4. Turn the CNI provisioning spec into executable leaf issues, then implement it without changing the substrate-only default.

The plan deliberately does not treat parent maps/specs (#13, #29, #102) as implementation tickets, or human/hardware tickets (#18, #19, #101) as agent work.

## Priority model

- **P0 — safety or unblocker:** data-integrity risk, current generated-output correctness, or an external prerequisite that gates a release.
- **P1 — current product completion:** work required to ship and validate the promised macOS v1.
- **P2 — committed platform expansion:** Linux parity work with a locked design and executable acceptance criteria.
- **P3 — next product capability:** CNI provisioning implementation; the spec is complete, but the implementation breakdown does not yet exist.
- **Tracking:** maps, specs, milestone shells, and non-blocking hardware verification. These close only when their leaf outcomes are demonstrated.

## Issue inventory and lane ownership

| Lane | Priority | Open issues | Role |
| --- | --- | --- | --- |
| 0. Triage and dependency hygiene | P0 | #84, #99, #100 | Convert ambiguous findings to bounded work; fix generated Cilium output correctness early. |
| 1. macOS reliability and v1 closure | P0/P1 | #42, #19, #22, #26, #27 | Resolve restore semantics, finish signing/packaging, then run the complete workshop validation. |
| 2. Linux foundation | P2 | #85, #86, #87, #88 | Create the platform boundaries, multi-arch cache, QEMU backend, and privileged networking substrate. |
| 3. Linux services and daemon integration | P2 | #89, #90, #91, #92, #93, #94 | Add DHCP/DNS/BGP/doctor/systemd and integrate QEMU balloon/suspend behavior. |
| 4. Linux delivery | P2 | #95, #96, #97, #98, #101 | Package, test in CI, publish, and document Linux support. |
| 5. CNI provisioning | P3 | #102 | Keep #102 as the PRD and create an implementation DAG before code starts. |
| 6. Tracking and non-blocking verification | Tracking | #13, #18, #29 | Close only from verified outcomes; #18 never blocks v1. |

All 27 open issues as of the plan date are assigned above.

## Dependency DAG and delivery waves

```text
Wave 0 — start immediately
  Human:       #19 --------------------------> #26
               #101 -------------------------> #95
  Correctness: #100, #99
  Safety:      #84 triage -> mitigation issue(s) -> #27
  Restore:     #42 bounded VZ investigation/fix
  Hygiene:     verify #22 exit; repair missing dependency edges

Wave 1 — macOS completion + Linux keystones
  macOS:       #19 -> #26 -> #27
  Linux:       #42 decision -> #85 ----\
               #86 ---------------------> #87 -> #92
               #88 -> #89 --------------/

Wave 2 — Linux fan-out after #88
               #88 -> {#89, #90, #91, #93, #94}
               #94 -> #96
               #94 + #101 + macOS release contract -> #95

Wave 3 — integration and release proof
               #87 + #89 -> #92
               #92 + #90 + #96 -> #97
               #95 + #97 + #96 + #91 + #93 -> #98

Wave 4 — CNI provisioning after decomposition
               #99 + #100 + #102 leaf breakdown
                  -> schema/intent
                  -> Talos credentials/config/bootstrap
                  -> render/apply infrastructure
                  -> Cilium and flannel/MetalLB paths
                  -> observed-state reconciliation
                  -> status/manifests/docs/e2e

Closure
  #27 closes the remaining macOS v1 outcome; then reconcile #29 and #13.
  CNI leaf completion and end-to-end proof close #102.
  #18 closes independently when physical macOS 14/15 hardware is available.
```

### Tracker dependency repairs

Keep the existing native edges, then add or explicitly document these inferred gates:

- #27 should be blocked by the resolution of #84 and the verified exit of #22, not only #26.
- #85 should follow the #42 restore decision because both reshape the current VZ lifecycle in `internal/vm/vm.go:227-251`; this is a merge-order constraint, not a reason to leave #85 idle during a long hardware wait.
- #95 should follow #26's release/versioning contract and #93's non-KVM doctor checks, in addition to its existing #94/#101 blockers.
- #97 should depend on #90 because a substrate-only Talos boot uses the Linux DNS path, and on #96 because the issue itself requires `nix flake check`.
- #98 should depend on #96, #91, and #93 as source-of-truth inputs in addition to #95/#97.
- #102 needs child implementation issues and native dependency edges; do not mark the PRD itself `ready-for-agent` as one giant change.

## Lane 0 — triage and correctness

### 0.1 Triage #84 as a safety issue, not a generic performance bug

Reproduce or bound the host-pressure thresholds, then split the outcome into the smallest executable slices:

1. A preflight/doctor warning for extreme swap use and data-volume fullness.
2. A create/start refusal threshold only if evidence shows a reliable unsafe boundary; otherwise warn and require `--force`.
3. A status signal for unexpected guest reboot/boot-ID change if it can be observed without introducing progress state.
4. Recovery guidance that explicitly says corrupt Talos EPHEMERAL snapshots require node wipe/recreate.

Reuse the existing overcommit path in `internal/daemon/balloon.go:57-80` and the established doctor style in `cmd/tbx/doctor.go:35-167`. Avoid a second, conflicting host-capacity policy.

Acceptance:

- Every proposed threshold is backed by a deterministic unit/fake test or documented as warning-only.
- The CLI names the recovery action; it does not imply that an image re-pull repairs a corrupt unpacked snapshot.
- #27 does not run under known-dangerous host pressure.

### 0.2 Implement #100 before expanding manifest consumers

Replace `CiliumBGPPeeringPolicy` at `internal/manifests/manifests.go:71-89` with the Cilium BGP v2 resource family and explicit LoadBalancer advertisement selection. Update the golden fixtures and any status/help text that names the old policy.

Acceptance:

- `tbx manifests <cluster> bgp` emits `CiliumBGPClusterConfig`, `CiliumBGPPeerConfig`, and `CiliumBGPAdvertisement` with a LoadBalancer service advertisement.
- Golden tests prove the complete multi-document output.
- The output is accepted by the pinned/supported Cilium version used by the walkthrough.

### 0.3 Implement #99 beside #100

Add the documented `k8sClientRateLimit.qps`/`burst` values to curated Cilium Helm values. Today the repository only renders manual manifests (`internal/manifests/manifests.go:112-149`), so if the values surface does not yet exist, attach this requirement to the future CNI render/apply leaf issue rather than inventing a temporary abstraction.

Acceptance:

- Both platform paths use the same facts-independent defaults.
- A render/golden test pins the exact values and confirms they are present whenever L2 announcements are enabled.

## Lane 1 — macOS reliability and v1 closure

### 1.1 Resolve #42 before extracting the VZ backend

Run a bounded investigation of the console/serial handle identity and EFI variable-store hypotheses around `internal/vm/vm.go:61-105`, `:131-149`, and `:194-202`. Prove memory preservation with guest uptime/boot logs, not merely a successful API return.

Exit in one of two valid states:

- Same-session and, if possible, cross-daemon restore works and is regression-covered; or
- VZ incompatibility is definitively documented, the cold-boot fallback remains explicit, and #85 preserves this capability status as data.

### 1.2 Start #19 immediately; implement #26 only after credentials exist

#19 is a human-only external prerequisite. Once complete, #26 owns Developer-ID signing, notarization, the brew tap, `tbx system install|uninstall`, and the versioned macOS release flow. The current build only performs ad-hoc signing (`Makefile:8-18`) and the current CI only builds/tests on macOS (`.github/workflows/ci.yml:9-24`).

Acceptance:

- A clean machine can install the signed/notarized brew artifact without Gatekeeper exceptions.
- Upgrade and uninstall preserve/remove the helper and resolver state exactly as documented.
- The release contract is reusable by #95 instead of creating competing tag workflows.

### 1.3 #22 verified; run #27

The mandatory physical-Mac test now passes with two helper-backed vmnet bridges on pinned `/24`s: freshly leased nodes reach each other in both directions before any priming traffic, host-to-node and host-to-VIP probes succeed, and nodes reach the remote VIP. The PID-gated regression test is `TestHelperNetworkingE2E`; #22 closes with its implementation PR.

#27 is the release gate: run the attendee flow from the brew-installed binary through cluster creation, manual Talos bootstrap, Cilium through mirrors, ingress VIP, snapshots, BGP, and the second routed cluster. Incorporate #84's safe-host preflight and #42's documented restore boundary.

Acceptance:

- The walkthrough succeeds using only published instructions and the installed artifact.
- Every SPEC guarantee has evidence; deviations become explicit follow-up issues before v1 is tagged.

## Lane 2 — Linux foundation

### 2.1 Parallel keystones: #85, #86, #88

Run these as independent branches/worktrees after the #42 decision is recorded:

- **#85 hypervisor boundary:** move the VZ-specific lifecycle currently concentrated in `internal/vm/vm.go:12-330` behind `internal/hypervisor`; inject it into the daemon, whose `Server` currently stores concrete `*vm.VM` values (`internal/daemon/server.go:23-30`). Preserve backend-neutral console code and move the 30-second stop timeout from the backend to the caller.
- **#86 multi-arch image cache:** add target architecture to cache keys, list metadata, migration/refetch behavior, and Factory URLs. The current cache is arm64-only at `internal/imagecache/cache.go:95-125` and has an arch-blind `<schematic>/<version>/disk.raw` layout.
- **#88 Linux helper substrate:** add Linux build-tag twins for bridge/tap/nftables convergence and SO_PEERCRED while preserving the privileged helper protocol boundary in `internal/helper/server.go:34-58` and `internal/helper/client.go:27-97`.

Merge order: #85 and #86 before #87; #88 can merge independently after cross-platform CI proves Darwin behavior unchanged.

### 2.2 QEMU backend #87

Implement QMP handshake, process supervision, devices, firmware discovery, capability reporting, ballooning, and QEMU >=8.2 suspend/resume behind the #85 interface. Depend on #86 for target-architecture image selection and on #88 for tap attachments.

Acceptance:

- Unit tests cover argv generation, QMP framing, process death, capability parsing, typed errors, and incompatible restore metadata.
- KVM-tagged integration tests boot both supported architectures where hardware exists; CI covers amd64 and manual hardware covers arm64.
- macOS build and unit tests remain green.

## Lane 3 — Linux services and daemon integration

After #88, fan out the following work with explicit file ownership:

- **#89 DHCP:** helper reservation server and authoritative MAC-to-IP lookup, replacing Linux lease scraping while leaving `internal/vm/lease.go:15-50` as the Darwin path.
- **#90 DNS:** fd-passed UDP/53 listener, upstream forwarding, and systemd-resolved registration. Extend the existing authoritative DNS server at `internal/dns/server.go:16-94`; never mutate `/etc/resolv.conf`.
- **#91 BGP:** Linux netlink FIB implementation and helper wiring while retaining the platform-neutral reconciler/speaker.
- **#93 doctor:** fakeable checks for KVM/QEMU/network filters/STP/rp_filter/ports/helper capabilities/resolved, using the current diagnostic surface in `cmd/tbx/doctor.go:35-167`.
- **#94 systemd:** socket/service/user-unit/sysusers/polkit assets plus a protocol-version handshake. Keep socket activation and stale-helper recovery testable without a live cluster.

Then implement **#92** after #87 and #89: route balloon and suspend decisions through `Capabilities()`, persist backend/version in save metadata, tolerate QEMU `ErrDeviceNotActive`, and preserve the VZ probe behavior. The current suspend/resume orchestration lives in `internal/daemon/suspend.go:17-126` and current balloon policy in `internal/daemon/balloon.go:16-89`.

Acceptance:

- Each leaf has unit/fake-backed fail-state coverage.
- A Linux cluster survives helper restart and host reboot through declarative convergence.
- Existing macOS behavior stays covered on every merge.

## Lane 4 — Linux delivery

### 4.1 System integration and packages

- **#96** follows #94 and adds a nixpkgs-only flake, overlay, and `virtualisation.talosbox` NixOS module.
- **#95** follows #94, #101, #26's release contract, and #93's non-KVM doctor checks. Add goreleaser/nfpm, handwritten scriptlets, Cloudsmith/AUR publication, and install/upgrade smoke tests.
- **#101** starts in Wave 0: create Cloudsmith/AUR accounts and store the required secrets, but never expose secret values in issue comments or logs.

### 4.2 CI #97

Extend `.github/workflows/ci.yml:1-24` into separate Linux build/unit matrices, amd64 KVM e2e, `nix flake check`, and nightly e2e. Keep the hard `/dev/kvm` writability gate so lack of acceleration fails immediately instead of timing out.

Required predecessors: #92, #90, and #96. The existing native edge only names #92 and should be expanded.

### 4.3 Documentation #98

Only finalize docs after packages and CI are proven. Update the Apple-only assumptions currently stated at `README.md:3-12`, the build/install flow at `README.md:14-37`, and the relevant SPEC sections. Document Linux BGP as the routed/ECMP/`externalTrafficPolicy: Local` path, not as macOS-style fast failover.

Acceptance:

- Fresh Ubuntu, Fedora, Arch, and NixOS users can reach a working ingress VIP from the published installation path.
- Unsupported/degraded suspend behavior is surfaced by version/capability, not by distro name alone.
- The docs match commands exercised by CI or a recorded manual test.

## Lane 5 — decompose and implement #102

Keep #102 as the parent PRD. Create these session-sized children with native dependencies:

1. **Schema and intent:** add `cni`, `lb`, `bgp`, and `hubble` validation/defaulting to `internal/config/config.go:19-39`, persist only intent in cluster state, and expose CLI flags/protocol fields. Absent `cni` must remain byte-for-byte behavior-compatible substrate-only mode.
2. **Talos credentials and machine config:** use the pinned Talos machinery SDK for secrets, machine config generation/application, bootstrap, talosconfig, and kubeconfig; enforce `0700` directories/`0600` files and derive replaceable credentials from `secrets.yaml`.
3. **Render/apply infrastructure:** pin and pre-cache charts/assets, render with the Helm Go SDK, and apply tbx-owned objects through client-go server-side apply. Integrate #99/#100 rather than duplicating their outputs.
4. **Cilium path:** `cni.name: none`, kube-proxy disabled, Talos-required values, optional Hubble, and Cilium-native LB/L2/BGP extras.
5. **Flannel/MetalLB path:** Talos-managed flannel plus pinned MetalLB, IPAddressPool, and L2Advertisement; reject BGP and surface the NetworkPolicy limitation.
6. **Observed-state reconciler:** apply machine config only in maintenance, bootstrap only when needed, unconditionally reconcile API-side owned objects after Kubernetes is reachable, enforce immutable/asymmetric toggles, and recover by rerunning `tbx up` without recording progress.
7. **Inspection and UX:** extend `tbx manifests`, stage narration/`--quiet`, status hints, export paths, documentation, and end-to-end probes for Ready-without-LB and live LoadBalancer VIP states.

Dependency order: 1 -> 2; 2 + 3 -> 4 and 5; 4 + 5 -> 6; 6 -> 7. Work on 3 may begin in parallel with 2 after #99/#100. Avoid concurrent edits to daemon/config hot spots while #85 is in flight.

Acceptance for the parent:

- No `cni` field preserves today's substrate-only behavior.
- Both curated paths converge after interruption and produce a responding LoadBalancer Service by default.
- Invalid option combinations fail before VM or cluster mutation.
- Credential permissions, immutability rules, Hubble deletion, and `lb: false` status are regression-covered.
- CNI, MetalLB, Talos, Helm, and Kubernetes client versions are pinned and exercised in e2e.

## Lane 6 — tracking issues and closure rules

- **#13 and #29:** retain as v1 map/PRD until #27 passes; then reconcile their checklists and close with links to release evidence. Do not implement against them directly.
- **#18:** run only on real macOS 14/15 Apple Silicon hardware. Record the verified floor or raise it; this remains explicitly non-blocking.
- **#22:** verified by its physical-Mac, PID-gated two-bridge networking exit test; close with the implementation PR.
- **#102:** close only after its new child DAG and end-to-end provisioning acceptance are complete.

## Verification strategy

Every implementation PR must run the cheapest proof first, then the platform proof it requires:

1. Targeted package tests for the changed component.
2. `make lint` and `make test` (`Makefile:20-28`).
3. Cross-platform build matrix after build-tag or interface changes.
4. Intentional golden regeneration/review for manifests; golden updates are never accepted without semantic diff review.
5. KVM-tagged Linux integration tests for #87/#88/#89/#90/#92 and the full #97 cluster-up job.
6. Physical Mac e2e for #42, #22, #26, and #27; the current e2e target is explicitly hardware-gated (`Makefile:23-25`).
7. Package install/upgrade/uninstall smoke tests before release publication.

Release gates:

- No known failing tests or lint errors.
- No unresolved P0 data-integrity or generated-manifest correctness issue.
- Fresh-install, upgrade, and uninstall evidence exists for every advertised package path.
- Documentation commands match a green automated or recorded manual flow.

## Risks and mitigations

| Risk | Mitigation |
| --- | --- |
| #42 or #84 becomes an open-ended hardware investigation | Time-box the hypothesis set; accept a definitive documented limitation where the issue already permits it, then preserve safe fallback behavior. |
| Linux branches conflict in shared helper/daemon files | Use the dependency fan-out above and assign file ownership; keep #85/#88 boundaries small before parallel service work. |
| Native dependency graph understates real prerequisites | Add the tracker edges listed in this plan before agents select work from the frontier. |
| Human credentials stall release work | Start #19/#101 in Wave 0; continue code/test preparation, but do not fake publication acceptance. |
| New platform dependencies inflate or complicate the binary | Add only dependencies already selected by issue/spec decisions; pin versions and record size/build consequences. |
| Linux CI silently runs under TCG | Hard-fail on writable `/dev/kvm` before boot and keep timeouts secondary. |
| CNI reconciliation overwrites attendee changes | Own only named SSA objects, apply machine config only in maintenance, and store intent rather than progress. |
| Parent maps/specs obscure unfinished leaf work | Keep them tracking-only and close from demonstrable leaf outcomes. |

## Recommended execution staffing

- **Wave 0:** one maintainer/human lane for #19/#101; one executor for #99/#100; one debugger for #84/#42.
- **Linux Wave 1:** three executor lanes for #85, #86, #88, with a single architect/reviewer guarding interface and build-tag boundaries.
- **Linux Wave 2:** up to five bounded lanes (#89/#90/#91/#93/#94) after #88, followed by one integration owner for #92.
- **Delivery:** one release owner for #26/#95, one Nix specialist for #96, one CI/test owner for #97, then a writer/verifier for #98.
- **CNI:** one schema/reconciliation owner, one Talos SDK owner, and one Kubernetes render/apply owner; converge before UX/e2e.

## Completion checklist

- [ ] Every open issue is either completed, explicitly deferred/non-blocking, or represented by a bounded executable child.
- [ ] Tracker dependencies match the actual delivery DAG.
- [ ] #84 has a safe, tested user-facing outcome.
- [ ] #99/#100 generated output is current and golden-tested.
- [ ] macOS v1 installs from brew and #27 passes.
- [ ] Linux build/unit/KVM/package/Nix/docs gates pass.
- [ ] #102's substrate-only compatibility and both curated CNI paths pass e2e.
- [ ] #13/#29/#102 close from evidence, not from code-complete assertions alone.
