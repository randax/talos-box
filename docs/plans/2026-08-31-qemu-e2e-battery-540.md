# Plan: QEMU e2e battery, QA runbook and macOS docs (#540)

Agreed between fable-5 and gpt-5.6-sol (two rounds), 2026-08-31. Parent spec #531
(docs/design-qemu-macos.md, ADR 0001). Blockers #537/#538 merged. No production
daemon or hypervisor behavior changes: selection, drift refusal, doctor
inventory, balloon readback, restart-safe suspend, and the inter-cluster check
all exist with unit coverage. This issue adds test orchestration, test-only
harness code, five live hardware cases, and documentation.

## Design decisions

### E2E location and portability

All live CLI/daemon tests stay in `cmd/tbx`, beside `TestConsoleE2E`; they
exercise public CLI behavior against the real user daemon, socket, log, and
installed helper.

- New `cmd/tbx/e2e_harness_test.go` — shared harness plus platform-neutral unit
  tests for its pure parts (doctor-inventory parsing, name generation, config
  rendering). Untagged, so plain `go test ./cmd/tbx` covers the pure helpers.
  Parts that shell out to real binaries carry `//go:build e2e`
  (`cmd/tbx/e2e_harness_e2e_test.go` if a split is needed).
- `cmd/tbx/console_e2e_test.go` — parameterized through the harness (YAML +
  `tbx up` instead of `cluster create`, hypervisor pinned).
- New `cmd/tbx/hypervisor_capabilities_e2e_darwin_test.go` — doctor + balloon.
- New `cmd/tbx/hypervisor_lifecycle_e2e_darwin_test.go` — suspend/restart + drift.
- New `cmd/tbx/hypervisor_mixed_e2e_darwin_test.go` — mixed host.

Hardware files: `//go:build e2e && darwin` (belt and braces with the `_darwin`
suffix). No `t.Parallel` anywhere: tests share one tbxd, DNS port 5399, helper
state, and `~/.talosbox`.

### Harness contract

1. Run `bin/tbx doctor`, parse every `INFO Hypervisors:` line. Do NOT use
   doctor's exit code as the availability signal (unrelated checks can fail
   while the inventory is trustworthy).
2. `TBX_E2E_HYPERVISOR` unset/blank → the unique `default=yes` line. Set →
   validate with `hypervisor.ParseName`; an invalid value FAILS (config error),
   it does not skip.
3. Selected backend `availability=unavailable` → `t.Skipf` with doctor's full
   reason and remediation.
4. Missing backend line, missing/duplicate default, malformed inventory,
   missing binaries, disabled ballooning where required, stale daemon,
   unavailable helper → FAIL (bad precondition), not skip. Absent-vs-stale
   follows the helper e2e precedent: absent skips, wrong fails.
5. Always render the resolved hypervisor explicitly into generated YAML
   (`config.Config` → `config.Marshal`, re-`config.Parse` as a self-check) so
   the daemon's `TBX_HYPERVISOR` default can never change what a test
   provisions. `TBX_E2E_HYPERVISOR` is a lane selector only; never translate it
   into `TBX_HYPERVISOR`.
6. Unique DNS-safe cluster names (short stem + PID + counter/timestamp). Never
   `qa-qemu`, `qa-qemu2`, `e2econ`, or any fixed name.
7. `requireNoForeignClusters(t)`: if any cluster not created by this run exists
   (running, stopped, or suspended), skip with the cluster names and
   state-aware remediation (destroy stopped / stop-or-destroy running /
   resume-or-destroy suspended / use a clean host). Required by the
   daemon-restarting tests (restart stops every VM the daemon runs) and the
   mixed test (doctor checks DNS for every persisted cluster).
8. Helpers: `runTBX`, expected-failure runner, log-offset capture + bounded
   polling over `~/.talosbox/tbxd.log`, config writer, cleanup registered
   immediately after each successful create (`cluster destroy <name> --force`,
   exact names only). On failure, dump `tbx status -o json`, the relevant
   tbxd.log tail, and cleanup output without masking the original failure.
   Command deadlines sized for real VMs (teardown ~20–30 s/node).
9. No test may launch a second daemon.

### Make targets

- `e2e` gains a build prerequisite, gated: Darwin → `build` (ad-hoc codesign),
  else → `binaries`; selector `UNAME_S ?= $(shell uname -s)` so the contract
  test can exercise both branches.
- The test invocation runs with `-count=1` and `-timeout 90m`; a private
  variable holds the common invocation so `e2e` and `e2e-all` cannot drift.
- `e2e-all`: one build, then two direct test invocations —
  `TBX_E2E_HYPERVISOR=vz` first, then `qemu` (not recursive `$(MAKE) e2e`).
- `.PHONY` gains `e2e-all`; the "vz-capable Mac" note becomes
  hypervisor-neutral.
- New `scripts/ci/test_make_e2e_contract.sh` (Bash 3.2-compatible, `make -n`
  dry runs only, `UNAME_S=Darwin|Linux VERSION=contract`, never runs go or
  codesign): asserts the build prerequisite per branch, `-count=1`, the
  timeout, env preservation, `e2e-all` = exactly two invocations in vz→qemu
  order, `.PHONY` membership. Wired into `make test` alongside the existing
  `go test ./...`. (Nix does not route through `make test`; no accommodation
  needed.)

## The five e2e cases

### 1. TestQEMUBalloonReadbackInMaintenanceE2E (QEMU lane only)

1. `requireNoForeignClusters`; fail with remediation if `TBX_DISABLE_BALLOON`
   is set.
2. Capture the daemon's current effective `BalloonReserveMiB` from
   `daemon.info` (the test process env may not describe the running daemon).
3. Measure host available memory; restart the daemon (`tbx system restart
   --force`) with `TBX_BALLOON_RESERVE_MIB=<available+2048>` set explicitly on
   the restart command's `exec.Cmd.Env` (dedup the key). Darwin self-spawn
   inherits the CLI env. Register cleanup: destroy the owned cluster first,
   then restart with the captured reserve (omit the var if it was the default).
4. Capture the tbxd.log offset now, before `tbx up`.
5. Create one explicit-QEMU cluster via YAML: 1 cp / 0 workers (explicit — the
   default is two workers), no CNI, `memory: 4096`. `tbx up --force` (the
   synthetic deficit makes pressure findings blocking; force converts them to
   warnings; with no other guests there is no reclaim and no hold).
6. Assert `tbx status -o json`: backend qemu, `nodes[].phase == "maintenance"`.
7. Poll (early-return, 7 min deadline — conservative slack, not hold coverage)
   for a new applied-target line for this exact cluster/node:
   `balloon <cluster>/<node>: target=<n>MiB (configured=4096 hostFree=… reserve=… deficit=…)`
   and require `1024 <= target < 4096`.

### 2. TestQEMUSuspendSurvivesDaemonRestartE2E (QEMU lane only)

1. `requireNoForeignClusters`; create a unique explicit-QEMU 1cp/0w substrate
   cluster (defaults: 2048 MiB / 2 CPU / 20 GiB).
2. Wait for boot; capture the node name. Suspend; wait ≥5 s (restored-clock
   evidence); assert status suspended.
3. `tbx system restart` WITHOUT `--force` — acceptance is itself part of the
   restart-safe proof. Do NOT set `TBX_HYPERVISOR` on the replacement daemon:
   stored cluster state must win.
4. Status after restart: must not contain `will cold-boot`.
5. Resume: output contains `guest clocks resume about`, contains no
   `cold-boot`; status back to running on qemu.
6. `tbx console <node> --no-follow --lines 300`: no fresh `Linux version …#1`
   boot banner.

### 3. TestDoctorHypervisorsE2E (both lanes; green on VZ-only Macs)

Capture doctor output; assert one lexically ordered line per registered
backend; every line has `availability`, `default`, `balloon-readback`,
`suspend`, `suspend-survives-restart`, `guest-agent`; exactly one
`default=yes` with `source=compiled|TBX_HYPERVISOR`; the selected lane is
available. QEMU lane additionally requires all four QEMU gates `supported`.
A VZ-only Mac passes: its QEMU line may be unavailable but must carry a
reason and remediation. Contract pinned by `doctor_hypervisors_test.go`.

### 4. TestUpRefusesHypervisorDriftE2E (both lanes)

Create a 1cp/0w cluster with the lane's backend pinned in YAML; confirm via
status. Render a second YAML for the same name with the opposite backend
(availability irrelevant — drift validation precedes backend resolution);
`tbx up -f` must exit non-zero with exactly:
`cluster "<name>": hypervisor is immutable (cluster has "<old>", talosbox.yaml wants "<new>"); destroy and recreate the cluster to change the hypervisor`.
Status still reports the original hypervisor, running.

### 5. TestMixedHypervisorsInterClusterE2E (QEMU lane only)

VZ-lane skip message: `runs in the QEMU lane (TBX_E2E_HYPERVISOR=qemu)`.
Additionally requires VZ availability; skips on Intel (mixed topology
impossible) with the doctor explanation. `requireNoForeignClusters`.

One YAML, two clusters (one explicit vz, one explicit qemu), each 1cp/0w,
2048 MiB / 10 GiB, `cni: flannel`, `lb: true`. `tbx up --force -f`. Wait for
both to report their intended hypervisor, running, Kubernetes ready, live VIP.
Then doctor must print:
`PASS inter-cluster: 2 cluster VIP(s) reachable from the host and from each sibling`
— doctor exercises both directed workload-origin VIP paths; no lighter curl
substitute. Online-lane policy: a cold run needs factory + registry access;
the offline skip message describes test scope, not an architectural limit.

## TDD order

Pure harness tests first (inventory classification incl. unavailable/missing/
malformed/duplicate-default, doctor-nonzero-with-valid-inventory, env
selection, ParseName failure, YAML round-trip pinning, name uniqueness);
contract script asserted failing before the Makefile change. Existing daemon
unit coverage (balloon eligibility, restart-safe summary, drift, doctor
rendering) is not duplicated. If hardware runs expose a production defect: add
a unit regression at the failing seam, smallest production fix, never weaken
the e2e assertion.

## Documentation

### docs/macos.md

1. Supported hosts: state plainly that Homebrew QEMU on macOS 14/Sonoma lacks
   HVF; recovery = upgrade to macOS 15+, reinstall QEMU, restart tbxd, rerun
   doctor. Keep Nix QEMU distinct from the Homebrew constraint.
2. Hypervisor selection: full precedence `clusters[].hypervisor` →
   `TBX_HYPERVISOR` (in tbxd's environment) → compiled default; persistence and
   immutability with the exact drift refusal; default changes affect only
   future silent creates.
3. New "Local e2e battery" subsection: `make e2e`,
   `TBX_E2E_HYPERVISOR=qemu make e2e`, `make e2e-all`; default resolution, YAML
   injection, unavailable-QEMU skips, Apple Silicon requirement.
4. Doctor output: add the full Apple-Silicon QEMU `available` example line, the
   `default=yes (source=TBX_HYPERVISOR)` variant, note unavailable lines keep
   reason/remediation, link `qa/deep-hypervisor-qemu.md`.

Stay consistent with docs/linux.md:50-52 and docs/SPEC.md:483,553.

### docs/qa/MATRIX.md

Keep the four-column shape. New row:
`| [deep-hypervisor-qemu](deep-hypervisor-qemu.md) | covered (VZ + QEMU/HVF) | platform-N/A | Apple Silicon backend-parity battery; Sonoma remediation charter is conditional; the 24h soak is human-run |`
Point the macOS 14/15 physical-hardware gap at the Sonoma charter; leave the
gap open until a physical run is attached.

### docs/qa/deep-hypervisor-qemu.md

Exact established runbook structure (deep-host-integration.md is the model):
title; metadata table (Tier: Deep — macOS QEMU/HVF parity; Platform: Apple
Silicon macOS 15+, conditional Sonoma charter; Duration: 60–90 min + separate
24 h human soak; Destructive: creates/destroys QA clusters and restarts the
unsupervised daemon, never uninstalls the helper; Runbook version); agent
instructions boilerplate with qa-run issue report destination; one-sentence
BLOCKED-unless preflight (incl. no reserved QA names, unsupervised tbxd,
online registries for provisioning charters; Sonoma hosts may run only C2).

Charters:
- C1 Doctor inventory and automated battery (doctor + all three make lanes).
- C2 Homebrew-on-Sonoma remediation [macOS 14 only] (no VM launch; on 15+
  record SKIPPED-not-Sonoma).
- C3 Selection precedence and immutable drift (env default vs explicit YAML vs
  stored state; exact refusal).
- C4 QEMU balloon readback in maintenance (synthetic-reserve procedure, new
  log lines only, restore daemon env).
- C5 Suspend survives daemon restart (no --force; memory-preserving path only).
- C6 Mixed VZ/QEMU inter-cluster paths (doctor PASS line).
- C7 24-hour matched VZ-vs-QEMU stability soak [human-run] (matched 1cp/1w
  flannel+lb clusters, identical workloads, 5-minute samples = 288, stability
  criteria; agent may prepare collection but must not claim completion).
- C8 Destroy and cleanup (always run).

Report template with C1–C8 rows, soak timestamps/sample count, friction log,
failures, cleanup evidence.

## Task breakdown

Wave 1 (independent, parallel): (1) harness + console parameterization;
(2) Makefile + contract script; (3) docs/macos.md; (4) runbook + MATRIX.
Wave 2 (after harness API frozen, parallel): (5) capabilities tests;
(6) lifecycle tests; (7) mixed test.
Wave 3: integration — build-tag/lint verification on both GOOS, conflict
resolution, hardware lanes, cleanup audit, docs cross-check.

## Verification

Static/unit: `bash scripts/ci/test_make_e2e_contract.sh`; `make test`;
`go test -race ./...`; `golangci-lint run`;
`GOOS=linux GOARCH=amd64 golangci-lint run`;
`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -exec=/usr/bin/true -tags=e2e ./...`.

Hardware (Apple Silicon, QEMU available): `make e2e`,
`TBX_E2E_HYPERVISOR=qemu make e2e`, `make e2e-all` — all five new cases plus
parameterized console green; afterwards no generated clusters or QEMU
processes remain. VZ-only behavior (QEMU-unavailable skips) is proven by
inspection of skip paths plus the harness unit tests. The 24 h soak is
human-run and out of scope for the PR gate.
