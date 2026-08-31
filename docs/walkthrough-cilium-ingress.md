# Walkthrough: curated Cilium to a browsable nginx URL

This walkthrough uses talosbox's curated Cilium path. `tbx` generates and applies the
Talos machine configuration, bootstraps Kubernetes, renders the pinned Cilium chart on
the host, applies it with server-side apply, and waits for its own LoadBalancer probe to
answer through Cilium's shared ingress controller. Applications publish ordinary
`Ingress` objects at HTTPS names under the cluster domain.

Prerequisites: `tbx` installed, its platform helper active, and `tbx doctor` passing.
On macOS, activate the helper with `sudo tbx system install`. On Linux, follow the
[Linux host setup](linux.md); do not run the macOS installer there.
Install `kubectl` for the application steps.

## 1. Create and provision the cluster

```sh
tbx cluster create demo --cp 1 --workers 2 --cni cilium
```

The default `lb: true` selects Cilium LB-IPAM with L2 announcements. The address pool
is `172.30.<subnet>.200-172.30.<subnet>.239`; Cilium's shared `cilium-ingress`
LoadBalancer owns `.200`, and talosbox routes its durable probe through a wildcard
Ingress on that VIP. The command narrates each provisioning stage with its `≈` manual
equivalent. Add `--quiet` to suppress that narration without hiding the final result.

Provisioning is observed-state driven. If the command is interrupted, rerun:

```sh
tbx up
```

No progress marker is stored. `tbx` observes maintenance nodes, bootstrap/API state,
owned Cilium objects, Kubernetes Node readiness, and the probe VIP on every run.

## 2. Confirm readiness and use the derived credentials

```sh
tbx status demo
export TALOSCONFIG=~/.talosbox/clusters/demo/talosconfig
export KUBECONFIG=~/.talosbox/clusters/demo/kubeconfig
kubectl get nodes
```

The ready status names `https://probe.demo.k8s.test/` and, until trust is installed,
prints `tbx trust install demo`. Deleting either derived credential file is
safe: `tbx up` re-mints it from the cluster's `secrets.yaml` without modifying
`~/.talos/config` or `~/.kube/config`.

## 3. Inspect or fork the exact provisioning inputs

`tbx manifests` reads the cluster's persisted CNI intent and exposes the same inputs
used by reconciliation:

```sh
tbx manifests demo machine  > machine-patch.yaml
tbx manifests demo values   > cilium-values.yaml
tbx manifests demo objects  > cilium-objects.yaml
tbx manifests demo extras   > cilium-extras.yaml
```

- `machine` contains `cni.name: none`, `cluster.proxy.disabled: true`, and the
  subnet-specific catch-all registry mirror document.
- `values` contains the pinned Talos-compatible Cilium values. There is no arbitrary
  Helm-values passthrough; it enables the default shared ingress controller, assigns its
  Service `.200`, disables forced HTTPS redirects, and names the wildcard default Secret.
- `objects` contains the exact pinned chart render that `tbx` applies.
- `extras` contains the LB-IPAM pool, L2 or BGP announcements, and the talosbox-owned
  ClusterIP probe, wildcard Ingress, and TLS Secret reference. With `lb: false`, no LB
  or ingress extras are emitted.

These sections are the hand-managed fork surface. Apply the machine prerequisite when
generating Talos configuration for a substrate-only cluster, bootstrap it, then apply
`objects` and `extras` in that order with server-side apply. Wait for Cilium's operator
to establish its CRDs before applying `extras`. The output is subnet- and
cluster-specific, so review names and addresses before reusing a saved fork elsewhere.

## 4. Deploy nginx behind the shared ingress

The nginx Service stays inside the cluster. Its explicit Cilium Ingress claims the exact
`nginx.demo.k8s.test` host, which takes precedence over talosbox's wildcard probe route.
The TLS entry intentionally has no `secretName`: Cilium uses the curated default wildcard
Secret rather than looking for a Secret in the `default` namespace.

```yaml
# nginx.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: nginx
spec:
  replicas: 2
  selector:
    matchLabels:
      app: nginx
  template:
    metadata:
      labels:
        app: nginx
    spec:
      containers:
        - name: nginx
          image: nginx:1.27
          ports:
            - containerPort: 80
---
apiVersion: v1
kind: Service
metadata:
  name: nginx
spec:
  type: ClusterIP
  selector:
    app: nginx
  ports:
    - port: 80
      targetPort: 80
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: nginx
spec:
  ingressClassName: cilium
  tls:
    - hosts:
        - nginx.demo.k8s.test
  rules:
    - host: nginx.demo.k8s.test
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: nginx
                port:
                  number: 80
```

```sh
kubectl apply -f nginx.yaml
kubectl get ingress nginx
tbx trust install demo
```

On macOS, approve the interactive login-keychain trust prompt. On Linux, `tbx` selects the
host's conventional trust-store driver and uses `sudo` only for the anchor write and store
refresh; on NixOS it prints the `security.pki.certificates` declaration to apply instead.
Restart any browser that was already open, then visit:

```text
https://nginx.demo.k8s.test/
```

The page should load without a certificate warning. As a browser-independent fallback, use
the generated CA directly; `--resolve` also separates ingress verification from host DNS:

```sh
curl --cacert ~/.talosbox/clusters/demo/ingress-ca.crt \
  --resolve nginx.demo.k8s.test:443:<demo-vip> \
  https://nginx.demo.k8s.test/
```

Take `<demo-vip>` from `tbx status demo`. If the cluster uses a custom domain, replace
`demo.k8s.test` in the Ingress, browser URL, and curl command with that exact domain. When the
cluster no longer needs browser trust, run `tbx trust remove demo`; on NixOS remove the
declarative entry and rebuild. Destroying the cluster does not remove trust automatically.

## Optional Cilium features

Declare `hubble: true` (or create with `--hubble`) to add Hubble Relay and UI. Declare
`bgp: true` only with Cilium and `lb: true`; it replaces L2 announcements with the
host-routed BGP path. Change these declaratively and rerun `tbx up`. CNI changes and
LoadBalancer disablement require destroy and recreate because those mutations are not
safe once provisioning has begun.

To keep bootstrapping and CNI installation entirely attendee-managed, omit `cni` when
creating the cluster. That substrate-only behavior remains unchanged and does not run
the curated provisioning pipeline.

## Observed gotchas

- Run `tbx doctor` before creating or starting a workshop cluster. On Linux, run it again once
  the cluster bridge exists and use the [Linux doctor reference](linux.md#what-tbx-doctor-checks-on-linux)
  for the platform-specific checks. Extreme host swap or
  data-volume pressure can reset guests during image unpack and corrupt Talos EPHEMERAL data;
  free memory/disk space or reduce the cluster size instead of overriding the preflight.
- If a Talos system service reports `exec format error` (or exits 139) after an unexpected
  guest reset, assume its unpacked EPHEMERAL snapshot is truncated. Re-pulling the image does
  not replace the pinned corrupt snapshot chain; destroy and recreate the affected node or
  cluster after relieving host pressure.
- The default namespace's PodSecurity warning on the nginx deployment is harmless for a demo. Any
  namespace without its own `pod-security.kubernetes.io/*` labels — `default` included — is
  **enforced at `baseline`** and warned/audited at `restricted`, so `kubectl apply` prints a long
  `Warning: would violate PodSecurity "restricted:latest"` block for a pod that is not
  restricted-compliant. A violation of `restricted` alone is a warning, not a rejection: the
  object is admitted (the nginx demo is this case). A pod that also violates `baseline` —
  `hostNetwork`, `privileged`, host paths — is **rejected** in `default` with
  `violates PodSecurity "baseline:latest"` and never created. For the warning, either make the pod
  compliant (`runAsNonRoot: true`, `seccompProfile.type: RuntimeDefault`,
  `allowPrivilegeEscalation: false`, `capabilities.drop: ["ALL"]`). For a workload that genuinely
  needs privileges or `hostNetwork`, giving it its own labelled namespace is not cosmetic — it is
  what makes the pod admissible at all: label that namespace the way
  `tbx manifests <cluster> storage` prints for a BYO CSI namespace
  (`pod-security.kubernetes.io/enforce=privileged` and the matching `audit`/`warn` labels).
  Curated CNI/CSI namespaces carry those labels already.
