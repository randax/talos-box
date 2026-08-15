# QA Runbook: Provisioning path — Cilium

| | |
|---|---|
| **Tier** | Deep (one feature area exercised against defaults) |
| **Platform** | macOS + Linux (platform-specific charters marked) |
| **Estimated duration** | 45–60 min |
| **Destructive** | Creates and destroys clusters `qa-cil` and `qa-hub`; does not touch other clusters or host config |
| **Runbook version** | against talos-box main @ the commit recorded in your report |

## How to execute this runbook (agent instructions)

You are running QA, not demos. For every charter: run the steps exactly, compare what you see against **Expected observations**, and record a verdict — **PASS**, **FAIL**, or **PASS-with-friction**. Friction is anything a careful human would frown at: a confusing message, a doc/behavior mismatch, a step that needed knowledge this runbook didn't give you, output that is technically correct but misleading. Friction is a first-class result — report it even when the charter passes.

Never improvise recovery mid-charter: if a step fails, capture the evidence listed under **On failure**, mark FAIL, and continue to the next charter unless it depends on this one (dependencies are stated per charter).

**Report destination**: one GitHub issue per run, label `qa-run`, title `QA deep-cilium <platform> <date>`.

## Preflight

Abort (BLOCKED) unless: `tbx version` recorded; `tbx doctor` exits 0; no clusters named `qa-cil`/`qa-hub`; ≥ 10 GiB free RAM; network reaches factory.talos.dev and the public registries (this runbook is online; the mirror warms itself through use).

## Charters

### C1 — Provisioned create reaches a live ingress VIP

**Goal**: the curated Cilium path end state: config applied, bootstrap, CNI installed, LB extras, VIP answering.

Steps:
1. `tbx cluster create qa-cil --cni cilium`
2. Follow `tbx status qa-cil` until it reports the provisioned end state (record the phase progression it shows).
3. Export credentials exactly as the status hints print them (kubeconfig/talosconfig exports).
4. `kubectl get nodes -o wide` and `kubectl -n kube-system get pods -l k8s-app=cilium`
5. `curl -sv http://<cluster-subnet>.200/ --max-time 10` (the `.200` ingress VIP; expect connection-level success — any HTTP answer, including 404, proves the LB path).

Expected observations:
- Status narrates provisioning progress and lands on a state distinguishing Ready-without-LB from live-VIP; final state reports the VIP live.
- All 3 nodes `configured` (TLS probe), Kubernetes nodes Ready, Cilium pods Running.
- kube-proxy is absent (`kubectl -n kube-system get ds kube-proxy` → NotFound) — Cilium replaces it.
- The VIP at `.200` accepts TCP (L2-announced by default; BGP is off).
- Cilium's built-in ingress controller is disabled (no `cilium-ingress` service).

Pass criteria: nodes Ready, Cilium Running, `.200` answers on TCP.

On failure: capture `tbx status -o json qa-cil`, `kubectl -n kube-system get pods -A -o wide`, `cilium status` if available via `kubectl exec`, and the create narration.

### C2 — `manifests` fork parity (depends on C1)

**Goal**: the inspection surface matches what was applied.

Steps:
1. `tbx manifests qa-cil` (section `all`), then individually: `machine`, `values`, `objects`, `extras`, `cilium-values`, `lb-pool`, `l2`, `mirrors`, `balloon`, `k8s`, `talos`.
2. Spot-check three claims against the live cluster: the LB pool range in `lb-pool` matches the `.200–.239` convention; `mirrors` shows the single catch-all `http://<gateway>:5059` endpoint with `skipFallback: true`; `balloon` includes `virtio_balloon` in kernel modules.

Expected observations: every listed section renders without error; `metallb-values`/`metallb-extras` are refused or empty on the cilium path (flannel-only sections — record the exact behavior); rendered values match live objects (`kubectl get ciliumloadbalancerippools -o yaml` vs `lb-pool`).

Pass criteria: all sections render; the three spot-checks match.

On failure: capture the mismatching section output and the live object.

### C3 — Hubble toggle

**Goal**: `hubble: true` delivers Relay + UI; requires cilium.

Steps:
1. `tbx cluster create qa-hub --cni cilium --hubble`
2. When provisioned: `kubectl -n kube-system get pods -l k8s-app=hubble-relay` and `kubectl -n kube-system get svc | grep hubble`
3. Negative check: `tbx cluster create qa-bad --cni flannel --hubble` — expect refusal before any mutation (hubble requires cilium).

Expected observations: hubble-relay Running and UI service present on `qa-hub`; the flannel+hubble combination is rejected with a specific validation error and no `qa-bad` cluster exists afterwards (`tbx status` clean).

Pass criteria: hubble runs on qa-hub; invalid combo rejected with nothing created.

On failure: capture the validation error text and `tbx status`.

### C4 — L2 failover latency **[platform-marked]**

**Goal**: document the announced failover behavior per platform.

Steps:
1. On `qa-cil`, identify the node announcing `.200` (`kubectl -n kube-system get leases | grep -i l2announce` or Cilium's l2-announce lease).
2. `tbx node remove qa-cil <announcing-worker>` (or stop that node's workload path by cordon+drain if removal is disruptive — use node remove; it is the supported verb).
3. Time how long `.200` is unreachable (`while ! curl -s --max-time 1 http://<subnet>.200/ >/dev/null; do date; sleep 2; done`).

Expected observations:
- **macOS**: convergence in ~40–50 s is EXPECTED (vmnet ignores GARP; macOS ARP revalidation converges) — under ~60 s is a PASS, not friction. Materially faster or slower than the documented window is worth reporting.
- **Linux**: normal bridge L2 — convergence within a few seconds.

Pass criteria: VIP reachable again within 90 s (macOS) / 15 s (Linux).

On failure: capture timing log and `kubectl -n kube-system get leases`.

### C5 — Destroy and cleanup (always run)

Steps: `tbx cluster destroy qa-cil --force`, `tbx cluster destroy qa-hub --force`, verify `tbx status` clean and `~/.talosbox/clusters/` has no `qa-cil`/`qa-hub`.

Pass criteria: no residue.

## Report template

```markdown
## QA deep-cilium <platform> — <date>

- tbx version / commit; platform details:
- Preflight: OK | BLOCKED (<why>)

| Charter | Verdict | Duration | Notes |
|---|---|---|---|
| C1 provisioned VIP | | | |
| C2 manifests parity | | | |
| C3 hubble | | | |
| C4 L2 failover | | | |
| C5 destroy | | | |

### Friction log
### Failures
```
