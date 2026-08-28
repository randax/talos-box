# QA Runbook: Provisioning path — Cilium

| | |
|---|---|
| **Tier** | Deep (one feature area exercised against defaults) |
| **Platform** | macOS + Linux (platform-specific charters marked) |
| **Estimated duration** | 75–90 min (includes the 30-minute steady-state blackout baseline) |
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
1. `tbx manifests qa-cil` (section `all`), then individually: `machine`, `values`, `objects`, `extras`, `cilium-values`, `lb-pool`, `l2`, `mirrors`, `k8s`, `talos`. Also run `tbx manifests qa-cil balloon` once: that section is deprecated and MUST error, pointing at `machine` — record the exact text.
2. Spot-check three claims against the live cluster: the LB pool range in `lb-pool` matches the `.200–.239` convention; `mirrors` shows the single catch-all `http://<gateway>:5059` endpoint with `skipFallback: true`; `machine` carries the `machine.kernel.modules` entry for `virtio_balloon` (SPEC §8's printed-snippet MUST — the `balloon` alias is gone; whether ballooning is actually running is attested from `~/.talosbox/tbxd.log` lines of the form `balloon <cluster>/<node>: target=<n>MiB (configured=… hostFree=… reserve=… deficit=…)`, macOS only).

Expected observations: every listed section renders without error; `balloon` errors with a redirect to `machine`; `metallb-values`/`metallb-extras` are refused or empty on the cilium path (flannel-only sections — record the exact behavior); rendered values match live objects (`kubectl get ciliumloadbalancerippools -o yaml` vs `lb-pool`).

Pass criteria: all listed sections render, `balloon` errors as documented, and the three spot-checks match.

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
2. Force the lease to move off that node. Which verb is legitimate depends on the topology — a defaults cluster has ONE control-plane node, and removing it would be cluster-fatal, so check the announcer's role first (`kubectl get node <announcer> -o wide` / the `node-role.kubernetes.io/control-plane` label):
   - **Announcer is a worker**: `tbx node remove qa-cil <announcing-worker>` (the supported verb; do not improvise cordon+drain). Since #314 the RPC answers as soon as the node is gone and the post-mutation reconcile continues in the background — follow it in `~/.talosbox/tbxd.log` rather than waiting on the CLI.
   - **Announcer is the sole control-plane node**: do NOT remove it. Hand the lease over instead by bouncing that node's Cilium agent — `kubectl -n kube-system delete pod -l k8s-app=cilium --field-selector spec.nodeName=<announcer>` — and record that this charter ran the handover variant, which measures lease re-announcement rather than node loss. (Alternative, if you want the removal variant on a defaults cluster: `tbx node add qa-cil --role worker`, wait for the lease to land on a worker, then remove that worker.)
3. Time how long `.200` is unreachable (`while ! curl -s --max-time 1 http://<subnet>.200/ >/dev/null; do date; sleep 2; done`).

Expected observations:
- **macOS**: since the GARP work, no observed VIP outage at 2 s polling is the expected result — a zero-outage measurement is a clean PASS, not an anomaly. If GARP does not take, the fallback is macOS ARP revalidation (tens of seconds); anything under ~60 s is still a PASS. Record the measured window either way; only materially slower than ~60 s is worth reporting as friction.
- **Linux**: normal bridge L2 — convergence within a few seconds.

Pass criteria: VIP reachable again within 90 s (macOS) / 15 s (Linux).

On failure: capture timing log and `kubectl -n kube-system get leases`.

#### VIP blackout investigation (#484)

Run a 30-minute steady-state baseline **before** any deliberate Cilium-pod deletion. Keep three
timestamped streams: a fast probe loop that records HTTP status (including `000`), a watch of the
L2-announcement lease holder and `renewTime`, and Cilium logs containing leader-election lines.
For example, run these concurrently and retain their raw output:

```sh
while true; do code="$(curl -sS -o /dev/null --max-time 1 -w '%{http_code}' http://<subnet>.200/ || true)"; printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "${code:-000}"; sleep 0.2; done
kubectl -n kube-system get leases -w -o yaml
kubectl -n kube-system logs -l k8s-app=cilium --prefix --timestamps -f
```

Use `tcpdump` for ARP only when it is available and the extra capture is needed; it is optional.
Count a blackout only with at least two consecutive failed/`000`/non-200 samples or at least one
second of failure. Treat a lone sample as curl/host scheduling noise unless ARP and Kubernetes
evidence agrees.

- **Curated reproduces:** any qualifying blackout while the Service still has `.200`, endpoints
  remain Ready, and Cilium pods do not restart. talos-box owns the follow-up. Correlate the start
  within ±5 seconds of lease holder/`renewTime` changes and Cilium leader-election log lines. ARP
  requests without replies with a stable holder point toward vmnet/L2 delivery; holder churn or
  stalled renewals point toward Cilium/API rate/lease behavior.
- **Curated does not reproduce:** zero qualifying blackouts across 30 minutes. Hand back to the
  workshop and A/B its values, beginning with `bpf.hostLegacyRouting` and
  `k8sClientRateLimit`; do not alter talos-box defaults.

Only after the baseline, run a deliberate Cilium-pod deletion as a separate follow-up. The
documented 40–50 second macOS failover must not contaminate the steady-state baseline. This
procedure classifies ownership and captures correlations; it makes no root-cause claim.

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
