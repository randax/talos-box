# QA Runbook: Provisioning path — flannel

| | |
|---|---|
| **Tier** | Deep (one feature area exercised against defaults) |
| **Platform** | macOS + Linux |
| **Estimated duration** | 30–45 min |
| **Destructive** | Creates and destroys cluster `qa-fla`; does not touch other clusters or host config |
| **Runbook version** | against talos-box main @ the commit recorded in your report |

## How to execute this runbook (agent instructions)

You are running QA, not demos. For every charter: run the steps exactly, compare against **Expected observations**, and record **PASS**, **FAIL**, or **PASS-with-friction**. Friction — confusing messages, doc/behavior mismatch, missing knowledge, misleading output — is a first-class result even on passing charters. No improvised recovery: capture the **On failure** evidence, mark FAIL, continue unless a dependency broke.

**Report destination**: one `qa-run` issue, title `QA deep-flannel <platform> <date>`.

## Preflight

BLOCKED unless: `tbx version` recorded; `tbx doctor` exits 0; no cluster `qa-fla`; ≥ 8 GiB free RAM; online.

## Charters

### C1 — Flannel + MetalLB reaches a live ingress VIP

**Goal**: the second curated path: Talos-managed flannel plus tbx-shipped MetalLB (L2 only).

Steps:
1. `tbx cluster create qa-fla --cni flannel`
2. Follow `tbx status qa-fla` to the provisioned end state; export credentials per the hints.
3. `kubectl -n kube-system get pods | grep -i flannel` and `kubectl get pods -A | grep -i metallb`
4. `curl -sv http://<subnet>.200/ --max-time 10`

Expected observations: flannel running as the CNI; MetalLB pods Running with an IPAddressPool covering `.200–.239` and an L2Advertisement; `.200` accepts TCP; **status prints the flannel NetworkPolicy limitation** (flannel enforces no NetworkPolicies) — quote it in the report.

Pass criteria: nodes Ready, MetalLB announcing, `.200` answers, limitation hint present.

On failure: capture status JSON, MetalLB pod logs, `kubectl get ipaddresspools,l2advertisements -A -o yaml`.

### C2 — BGP is rejected on flannel

**Goal**: the intent chain holds: bgp requires cilium.

Steps:
1. `tbx bgp enable qa-fla` — expect refusal.
2. `tbx cluster create qa-bad --cni flannel --bgp` — expect refusal before mutation.

Expected observations: both rejections name the actual constraint (bgp requires `cni: cilium` and `lb`); no `qa-bad` exists after; `qa-fla` unaffected.

Pass criteria: both refused with specific errors; nothing created or changed.

On failure: capture the exact error text (or worse, evidence it succeeded).

### C3 — MetalLB manifests sections (depends on C1)

**Goal**: the flannel-only inspection sections work exactly there.

Steps:
1. `tbx manifests qa-fla metallb-values` and `tbx manifests qa-fla metallb-extras` — expect rendered content.
2. `tbx manifests qa-fla cilium-values` — expect a refusal/empty (cilium-only section on a flannel cluster); record the exact behavior.

Pass criteria: metallb sections render and match live objects; the cilium section's behavior is a clear error, not silent wrong output.

On failure: capture section output vs `kubectl get -A` equivalents.

### C4 — NetworkPolicy limitation is real (depends on C1)

**Goal**: honest verification that the documented limitation behaves as documented.

Steps:
1. Deploy two pods and a deny-all NetworkPolicy in a test namespace; verify traffic still flows (flannel does not enforce).

Expected observations: the policy is accepted by the API but not enforced — connectivity persists. This is expected behavior; the QA value is confirming the status hint tells the truth.

Pass criteria: behavior matches the printed limitation.

On failure: capture policy YAML and connectivity evidence.

### C5 — Destroy and cleanup (always run)

Steps: `tbx cluster destroy qa-fla --force`; verify no residue in `tbx status` / `~/.talosbox/clusters/`.

Pass criteria: no residue.

## Report template

```markdown
## QA deep-flannel <platform> — <date>

- tbx version / commit; platform details:
- Preflight: OK | BLOCKED (<why>)

| Charter | Verdict | Duration | Notes |
|---|---|---|---|
| C1 flannel+metallb VIP | | | |
| C2 bgp rejected | | | |
| C3 metallb manifests | | | |
| C4 netpol limitation | | | |
| C5 destroy | | | |

### Friction log
### Failures
```
