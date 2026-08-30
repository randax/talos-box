# QA Runbook: macOS QEMU/HVF hypervisor parity

| | |
|---|---|
| **Tier** | Deep — macOS QEMU/HVF parity |
| **Platform** | Apple Silicon macOS 15+ (conditional Sonoma charter — see C2) |
| **Estimated duration** | 60–90 min + separate 24h human soak |
| **Destructive** | Creates/destroys QA clusters and restarts the unsupervised daemon; never uninstalls the helper |
| **Runbook version** | against talos-box main @ the commit recorded in your report |

## How to execute this runbook (agent instructions)

You are running QA, not demos. For every charter: run the steps exactly, compare against **Expected observations**, and record **PASS**, **FAIL**, or **PASS-with-friction**. Friction — confusing messages, doc/behavior mismatch, missing knowledge, misleading output — is a first-class result even on passing charters. No improvised recovery: capture the **On failure** evidence, mark FAIL, continue unless a dependency broke.

**Report destination**: one `qa-run` issue, title `QA deep-hypervisor-qemu <platform> <date>`.

## Preflight

BLOCKED unless: `tbx version` recorded; `tbx cluster list -o json` reports an empty cluster inventory (zero clusters); tbxd is running unsupervised, with no attached debugger or foreground session that would make daemon restarts unrepresentative; for provisioning charters C1, C4, C5, C6, and C7 the host has online access to the Talos factory and image registries; on a macOS 14/Sonoma host this runbook may run **only C2**, and all other charters require macOS 15+ on Apple Silicon.

## Charters

### C1 — Doctor inventory and automated battery

**Goal**: the hypervisor inventory is complete and internally consistent, and every supported local e2e lane passes.

Steps:
1. Run `tbx doctor | tee /tmp/qa-hypervisor-doctor.txt`; extract the inventory with `grep 'INFO Hypervisors:' /tmp/qa-hypervisor-doctor.txt`.
2. For every inventory line, confirm the real output shape is `INFO Hypervisors: <name>: availability=<value>; default=<yes-or-no>; balloon-readback=<value>; suspend=<value>; suspend-survives-restart=<value>; guest-agent=<value>`. An unavailable line may add a parenthesized reason and `remediation:` after `availability=unavailable`; an available default line names its source, for example `INFO Hypervisors: qemu: availability=available; default=yes (source=TBX_HYPERVISOR); balloon-readback=supported; suspend=supported; suspend-survives-restart=supported; guest-agent=supported`.
3. Confirm exactly one inventory line contains `default=yes`, and that it includes `source=compiled` or `source=TBX_HYPERVISOR`.
4. Run the default lane with `make e2e`.
5. Run the explicit QEMU lane with `TBX_E2E_HYPERVISOR=qemu make e2e`.
6. Run both backends in order with `make e2e-all`.
7. Run `tbx status -o json | python3 -m json.tool` as a final status-output sanity check.

Expected observations: all three make invocations are green on an Apple Silicon host where QEMU/HVF is available; the inventory contains one lexically ordered line per registered backend, every line contains all six fields, and exactly one backend is the default. On a VZ-only host, QEMU is `availability=unavailable` with a reason and remediation, and the explicit QEMU lane skips cleanly rather than failing.

Pass criteria: doctor inventory is parseable and honest, exactly one default is identified, all available lanes pass, unavailable QEMU is explained and skipped cleanly, and status emits valid JSON. The run report explicitly lists every skipped test and its stated skip reason. If doctor reports QEMU as `availability=available`, any skipped test case in a QEMU lane is unexpected and makes C1 FAIL; documented conditional charter skips, including C2's `SKIPPED-not-Sonoma`, remain valid and do not by themselves fail C1.

On failure: capture the complete doctor transcript, the failing make command and output, `tbx status -o json`, and the relevant tail of `~/.talosbox/tbxd.log`.

### C2 — Homebrew-on-Sonoma remediation **[macOS 14 only]**

**Goal**: a Sonoma host that cannot use Homebrew QEMU/HVF receives complete, runnable remediation without launching a VM.

Steps:
1. **[macOS 14/Sonoma]** Run `tbx doctor | tee /tmp/qa-sonoma-doctor.txt`; do not run `tbx up`, `tbx cluster create`, or any make e2e lane in this charter.
2. Confirm the QEMU `INFO Hypervisors:` line reports `availability=unavailable` because HVF is missing and its `remediation:` field contains exactly `HVF not built in: Homebrew builds without HVF on macOS 14; upgrade to macOS 15+ and reinstall QEMU`.
3. After the operator completes that upgrade and reinstall, the runbook separately directs them to restart the daemon with `tbx system restart` and rerun `tbx doctor` to confirm QEMU availability; these commands are not part of the doctor's remediation text.
4. **[macOS 15+]** Record this charter as `SKIPPED-not-Sonoma` and continue to C3; do not attempt to reproduce or emulate a Sonoma environment.

Expected observations: Sonoma reports QEMU unavailable with the doctor remediation `HVF not built in: Homebrew builds without HVF on macOS 14; upgrade to macOS 15+ and reinstall QEMU`; no VM launches. The runbook separately directs the post-remediation daemon restart and doctor recheck. A macOS 15+ run records the explicit conditional skip.

Pass criteria: on physical Sonoma hardware, the doctor truthfully reports that HVF is not built in and its remediation directs the upgrade and QEMU reinstall; the run report separately records the daemon restart and doctor recheck after remediation. On macOS 15+, the report says `SKIPPED-not-Sonoma`.

On failure: capture the full doctor transcript, Homebrew QEMU version/package facts, `sw_vers`, and `uname -m`; do not attempt an improvised QEMU launch.

### C3 — Selection precedence and immutable drift

**Goal**: explicit YAML wins over the daemon environment, the daemon environment wins over the compiled default, and stored backend identity cannot drift.

Steps:
1. Record the current daemon default and source with `tbx doctor | grep 'INFO Hypervisors:'`; record whether the original effective default came from `compiled` or `TBX_HYPERVISOR` so it can be restored, and if it came from `TBX_HYPERVISOR`, record that original value.
2. With no clusters running, run `TBX_HYPERVISOR=qemu tbx system restart --force`, then `tbx doctor | grep 'INFO Hypervisors:'`; the QEMU line must contain `default=yes (source=TBX_HYPERVISOR)`. Next run `TBX_HYPERVISOR=vz tbx system restart --force`, then rerun doctor; the VZ line must contain the same source field. This proves `TBX_HYPERVISOR` overrides the compiled default.
3. Write `/tmp/qa-hv-qemu.yaml` with the following content, run `tbx up -f /tmp/qa-hv-qemu.yaml`, then run `tbx status qa-hv -o json | python3 -m json.tool`. With the daemon default still VZ, status must report QEMU, proving `clusters[].hypervisor` wins over `TBX_HYPERVISOR`.

   ```yaml
   version: 1
   clusters:
     - name: qa-hv
       controlPlanes: 1
       workers: 0
       hypervisor: qemu
   ```

4. Write `/tmp/qa-hv-vz.yaml` with the following content and run `tbx up -f /tmp/qa-hv-vz.yaml`; it must exit non-zero with exactly `cluster "qa-hv": hypervisor is immutable (cluster has "qemu", talosbox.yaml wants "vz"); destroy and recreate the cluster to change the hypervisor`.

   ```yaml
   version: 1
   clusters:
     - name: qa-hv
       controlPlanes: 1
       workers: 0
       hypervisor: vz
   ```

5. Run `tbx status qa-hv -o json | python3 -m json.tool`; it must still report the original QEMU hypervisor and a running cluster.
6. Capture the evidence, then run `tbx cluster destroy qa-hv --force`. Restore the daemon's original environment: if the original source was compiled, run `env -u TBX_HYPERVISOR tbx system restart --force`; otherwise rerun the restart with the originally recorded `TBX_HYPERVISOR=<value>`.

Expected observations: doctor identifies the selected host default and its source; explicit QEMU YAML creates QEMU while the daemon default is VZ; the opposite YAML is rejected before any backend change; persisted state remains QEMU and running after the refusal.

Pass criteria: the observed order is `clusters[].hypervisor` > `TBX_HYPERVISOR` > compiled default, the refusal text matches exactly, no mutation follows the refusal, and the original daemon default is restored.

On failure: capture both YAML files, both doctor inventories, the exact failing command and output, status JSON before and after the drift attempt, and the relevant daemon log tail.

### C4 — QEMU balloon readback in maintenance

**Goal**: a maintenance-phase QEMU guest accepts a synthetic balloon target and reports the applied target through the daemon log.

Steps:
1. Run `tbx doctor | tee /tmp/qa-balloon-doctor-before.txt`. Record the daemon's current effective `BalloonReserveMiB` from the `host-pressure` line (or directly from `daemon.info` when using an instrumented client), including whether it is the compiled default; also record whether doctor says `TBX_DISABLE_BALLOON` is active. If ballooning is disabled, mark FAIL with remediation rather than continuing.
2. Read the same doctor `host-pressure` line's `<n> MiB free memory` value as `hostAvailable`, corroborate it with `memory_pressure`, and set `syntheticReserve = hostAvailable + 2048`.
3. In one shell, run `export TBX_BALLOON_RESERVE_MIB=<syntheticReserve>` followed by `tbx system restart --force`; rerun doctor and confirm its balloon-reserve arithmetic reflects the synthetic reserve.
4. Record the current end of `~/.talosbox/tbxd.log` before provisioning with `offset=$(wc -l < ~/.talosbox/tbxd.log)`; only lines written after this offset count.
5. Write `/tmp/qa-balloon.yaml` with the following content; the absent `cni` is intentional. Run `tbx up --force -f /tmp/qa-balloon.yaml`.

   ```yaml
   version: 1
   clusters:
     - name: qa-balloon
       controlPlanes: 1
       workers: 0
       hypervisor: qemu
       node:
         memory: 4096
         cpus: 1
         diskSize: 20GiB
   ```

6. Run `tbx status qa-balloon -o json | python3 -m json.tool`; confirm the backend is `qemu` and every `nodes[].phase` equals `maintenance`.
7. Starting from the recorded offset, watch only newly appended log output with `tail -n +$((offset+1)) -f ~/.talosbox/tbxd.log | grep 'balloon qa-balloon/'` until a line appears in the shape `balloon qa-balloon/<node>: target=<n>MiB (configured=4096 hostFree=... reserve=... deficit=...)`. Stop the tail and verify `1024 <= target < 4096`.
8. Run `tbx cluster destroy qa-balloon --force`. Restore the captured reserve immediately: if it was the compiled default, run `unset TBX_BALLOON_RESERVE_MIB` and then `tbx system restart --force`; otherwise export the captured value and restart. Confirm doctor reports the restored reserve.

Expected observations: the QEMU cluster remains in maintenance, a new cluster-specific balloon line reports an applied target between 1024 MiB inclusive and 4096 MiB exclusive, and the synthetic reserve is removed after cleanup.

Pass criteria: backend and phase match, a post-offset applied-target line has the required arithmetic and range, the cluster is destroyed, and the daemon's original effective reserve is restored.

On failure: capture both doctor transcripts, the YAML, status JSON, the saved log offset and all new balloon lines, the calculated `hostAvailable` and reserve values, and cleanup/restart output.

### C5 — Suspend survives daemon restart

**Goal**: QEMU suspension remains memory-preserving across a normal unsupervised daemon restart.

Steps:
1. Write `/tmp/qa-suspend.yaml` with the following content; the absent `cni` is intentional. Run `tbx up -f /tmp/qa-suspend.yaml`, wait for boot, and record the control-plane node name.

   ```yaml
   version: 1
   clusters:
     - name: qa-suspend
       controlPlanes: 1
       workers: 0
       hypervisor: qemu
   ```

2. Run `tbx cluster suspend qa-suspend`, wait at least 5 seconds, then run `tbx status qa-suspend`; status must report the cluster suspended.
3. Run `tbx system restart` without `--force`. Acceptance is part of the proof; do not set `TBX_HYPERVISOR` on the replacement daemon.
4. Run `tbx status qa-suspend`; its output must not contain `will cold-boot`.
5. Run `tbx cluster resume qa-suspend | tee /tmp/qa-suspend-resume.txt`; output must contain `guest clocks resume about` and must not contain `cold-boot`.
6. Run `tbx status qa-suspend -o json | python3 -m json.tool`; confirm the cluster is running on QEMU.
7. Run `tbx console qa-suspend qa-suspend-cp-1 --no-follow --lines 300`; there must be no fresh `Linux version ...#1` boot banner after the resume point. The continued console stream is memory-preserving evidence, not merely a successful restart.
8. After capturing the evidence, run `tbx cluster destroy qa-suspend --force` so C6 starts without a foreign cluster.

Expected observations: restart is accepted without force because the saved QEMU state survives daemon replacement; status never warns of a cold boot; resume prints the guest-clock warning; the same guest memory resumes without a new kernel boot banner.

Pass criteria: normal restart succeeds, `will cold-boot` and `cold-boot` are absent where specified, `guest clocks resume about` is present, status returns to running on QEMU, and console evidence shows no reboot.

On failure: capture suspend, restart, status, and resume transcripts; status JSON; the 300-line console capture; node name; and the relevant daemon log tail.

### C6 — Mixed VZ/QEMU inter-cluster paths

**Goal**: one VZ cluster and one QEMU cluster share working host, DNS, and directed sibling-to-VIP paths.

Steps:
1. Run `tbx doctor | tee /tmp/qa-mixed-doctor-before.txt`; confirm both VZ and QEMU report `availability=available`. This charter skips on Intel hosts, where the mixed topology is impossible, and records the doctor explanation; it also skips when VZ is unavailable.
2. Write `/tmp/qa-mixed.yaml` with the following content.

   ```yaml
   version: 1
   clusters:
     - name: qa-mixed-vz
       controlPlanes: 1
       workers: 0
       hypervisor: vz
       cni: flannel
       lb: true
       node:
         memory: 2048
         cpus: 2
         diskSize: 10GiB
     - name: qa-mixed-qemu
       controlPlanes: 1
       workers: 0
       hypervisor: qemu
       cni: flannel
       lb: true
       node:
         memory: 2048
         cpus: 2
         diskSize: 10GiB
   ```

3. Run `tbx up --force -f /tmp/qa-mixed.yaml`.
4. Poll `tbx status qa-mixed-vz -o json` and `tbx status qa-mixed-qemu -o json` until each reports its intended hypervisor, running nodes, Kubernetes ready, and a live VIP.
5. Run `tbx doctor | tee /tmp/qa-mixed-doctor-after.txt`; it must print exactly `PASS inter-cluster: 2 cluster VIP(s) reachable from the host and from each sibling`.
6. After capturing the evidence, run `tbx cluster destroy qa-mixed-vz --force` and `tbx cluster destroy qa-mixed-qemu --force` so the soak starts with only its matched pair.

Expected observations: VZ and QEMU run concurrently with their pinned backends; both Kubernetes clusters and VIPs become ready; doctor proves host reachability and both directed workload-origin sibling paths with the exact PASS line.

Pass criteria: all readiness facts hold and doctor prints the exact inter-cluster PASS line; a capability-gated skip includes the doctor explanation.

On failure: capture the YAML, both status JSON documents, both doctor transcripts, VIP addresses, and the relevant log tail; do not substitute a lighter host-only curl check for doctor.

### C7 — 24-hour matched VZ-vs-QEMU stability soak **[human-run]**

**Goal**: compare VZ and QEMU stability under identical topology, workload, and sampling over a full 24-hour wall-clock window.

Steps:
1. Write `/tmp/qa-soak.yaml` with the following matched clusters; keep any later resource or Talos overrides identical between them.

   ```yaml
   version: 1
   clusters:
     - name: qa-soak-vz
       controlPlanes: 1
       workers: 1
       hypervisor: vz
       cni: flannel
       lb: true
     - name: qa-soak-qemu
       controlPlanes: 1
       workers: 1
       hypervisor: qemu
       cni: flannel
       lb: true
   ```

2. Run `tbx up --force -f /tmp/qa-soak.yaml`; wait for both clusters to report their intended backend, all nodes running, Kubernetes ready, and a live VIP.
3. Apply the same recorded workload manifest and request pattern to both clusters. Record the manifest, image digests, workload start time, and expected resource envelope.
4. Record an ISO-8601 start timestamp, take an initial sample at t=0, then sample both clusters at every 5-minute mark through t=24h: 289 endpoint-inclusive samples per cluster (0, 5, 10, ..., 1440 minutes). Each sample must record node phase, pod health and restart counts, VIP reachability, memory use, CPU use, and a timestamp.
5. After the full elapsed interval, record an ISO-8601 end timestamp and attach the sample log or CSV plus the collector output. Check for unexpected restarts, phase flaps, any unreachable VIP sample, and memory or CPU outside the workload's expected bounds.
6. An agent may prepare the clusters and start the sampling/collection job, but must **not** claim this charter complete without actually waiting the full elapsed wall-clock time and attaching the resulting artifacts, including the sample log/CSV and timestamps of the first and last samples. A report claiming PASS without those artifacts is invalid.

Expected observations: both matched clusters remain stable for the entire 24 hours; every one of the 289 endpoint-inclusive samples per cluster reports steady phases, healthy pods, reachable VIPs, and resources inside the recorded envelope.

Pass criteria: full wall-clock duration elapsed; first and last timestamps span at least 24 hours; 289 endpoint-inclusive samples per cluster are attached; there are no unexpected restarts or phase flaps; every VIP sample succeeds; memory and CPU remain within expected bounds.

On failure: preserve the complete collector artifacts, workload manifest and digests, first failing sample, surrounding status/doctor/log evidence, and all timestamps; do not restart the sample count after hiding an incident.

### C8 — Destroy and cleanup (always run)

**Goal**: remove every cluster and temporary daemon override created by this runbook without altering the installed helper.

Steps:
1. Destroy every cluster created in this run by exact name: run `tbx cluster destroy qa-hv --force`, `tbx cluster destroy qa-balloon --force`, `tbx cluster destroy qa-suspend --force`, `tbx cluster destroy qa-mixed-vz --force`, `tbx cluster destroy qa-mixed-qemu --force`, `tbx cluster destroy qa-soak-vz --force`, and `tbx cluster destroy qa-soak-qemu --force` for each name that exists.
2. Run `tbx status -o json | python3 -m json.tool` and `tbx cluster list -o json | python3 -m json.tool`; confirm none of those exact names remains.
3. If C4's `TBX_BALLOON_RESERVE_MIB` override was not already restored, restore the captured value now, omitting the variable entirely when the captured value was the compiled default, and run `tbx system restart --force`.
4. If C3's `TBX_HYPERVISOR` daemon default was not already restored, restore the value captured in C3 step 1: if its original source was compiled, run `env -u TBX_HYPERVISOR tbx system restart --force`; otherwise run `TBX_HYPERVISOR=<original-value> tbx system restart --force` with the recorded original value.
5. Run `pgrep -fl 'qemu-system-(aarch64|x86_64)'`; expect no QEMU process left by this runbook. Do not run `tbx system uninstall` and do not remove the helper.

Expected observations: every runbook cluster is absent from status and cluster-list JSON, the daemon has its original balloon reserve and `TBX_HYPERVISOR` default, no runbook-owned QEMU process remains, and the helper installation is untouched.

Pass criteria: zero cluster or QEMU-process residue, original daemon balloon configuration and `TBX_HYPERVISOR` default restored, and helper still installed.

On failure: capture destroy output for each exact name, both JSON listings, doctor reserve evidence, and `pgrep`/`ps` details for every residual QEMU process.

## Report template

```markdown
## QA deep-hypervisor-qemu <platform> — <date>

- tbx version / commit:
- Platform / chip (Apple Silicon) / macOS version:
- QEMU and Homebrew versions:
- Initial daemon default/source and BalloonReserveMiB:
- Preflight: OK | BLOCKED (<why>)

| Charter | Verdict | Duration | Notes |
|---|---|---|---|
| C1 doctor/battery | | | |
| C2 Sonoma remediation | | | |
| C3 precedence/drift | | | |
| C4 balloon readback | | | |
| C5 restart-safe suspend | | | |
| C6 mixed inter-cluster | | | |
| C7 24h matched soak | | | |
| C8 destroy/cleanup | | | |

### C7 soak evidence

- Start timestamp:
- End timestamp:
- Sample count:
- Artifact location:

### Friction log
### Failures
```
