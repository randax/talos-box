# Walkthrough: curated Cilium to a browsable nginx URL

This walkthrough uses talosbox's curated Cilium path. `tbx` generates and applies the
Talos machine configuration, bootstraps Kubernetes, renders the pinned Cilium chart on
the host, applies it with server-side apply, and waits for its own LoadBalancer probe to
answer. Cilium's ingress controller is deliberately disabled; applications request
ordinary `LoadBalancer` Services instead.

Prerequisites: `tbx` installed, `sudo tbx system install` completed, `tbx doctor`
passing, and `kubectl` available for the application steps.

## 1. Create and provision the cluster

```sh
tbx cluster create demo --cp 1 --workers 2 --cni cilium
```

The default `lb: true` selects Cilium LB-IPAM with L2 announcements. The address pool
is `172.30.<subnet>.200-172.30.<subnet>.239`; talosbox reserves `.200` for its durable
end-state probe. The command narrates each provisioning stage with its `≈` manual
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

The ready status names the live probe URL. Deleting either derived credential file is
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
  Helm-values passthrough, and `ingressController.enabled` is false.
- `objects` contains the exact pinned chart render that `tbx` applies.
- `extras` contains the LB-IPAM pool, L2 or BGP announcements, and the talosbox-owned
  probe objects. With `lb: false`, no LB extras are emitted.

These sections are the hand-managed fork surface. Apply the machine prerequisite when
generating Talos configuration for a substrate-only cluster, bootstrap it, then apply
`objects` and `extras` in that order with server-side apply. Wait for Cilium's operator
to establish its CRDs before applying `extras`. The output is subnet- and
cluster-specific, so review names and addresses before reusing a saved fork elsewhere.

## 4. Deploy nginx as a LoadBalancer Service

The talosbox probe owns `.200`, so this example requests `.201` from the same pool:

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
  annotations:
    lbipam.cilium.io/ips: 172.30.0.201
spec:
  type: LoadBalancer
  selector:
    app: nginx
  ports:
    - port: 80
      targetPort: 80
```

```sh
kubectl apply -f nginx.yaml
kubectl get service nginx --watch
curl -i http://172.30.0.201/
```

For a cluster whose subnet index is not `0`, replace the third octet in the requested
address. `tbx status demo` shows the cluster subnet and the talosbox probe VIP.

## Optional Cilium features

Declare `hubble: true` (or create with `--hubble`) to add Hubble Relay and UI. Declare
`bgp: true` only with Cilium and `lb: true`; it replaces L2 announcements with the
host-routed BGP path. Change these declaratively and rerun `tbx up`. CNI changes and
LoadBalancer disablement require destroy and recreate because those mutations are not
safe once provisioning has begun.

To keep bootstrapping and CNI installation entirely attendee-managed, omit `cni` when
creating the cluster. That substrate-only behavior remains unchanged and does not run
the curated provisioning pipeline.
