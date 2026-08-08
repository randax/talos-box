# Walkthrough: from `tbx cluster create` to a browsable nginx URL

End-to-end recipe validated 2026-07-21 on an Apple Silicon Mac: create a cluster with
talosbox, configure it with `talosctl`, install Cilium (as CNI **and** ingress controller),
deploy nginx, and open `http://nginx.demo.k8s.test/` in a browser. Total time from
`cluster create` to HTTP 200 was about 10 minutes with a warm image cache.

Prerequisites: `tbx` installed with `sudo tbx system install` done and `tbx doctor` passing,
plus `talosctl` (matching the tbx-pinned Talos version, here v1.13.6), `kubectl`, and `helm`.

## 1. Create the cluster

```sh
tbx cluster create demo --cp 1 --workers 2
tbx status demo   # wait until all nodes reach PHASE maintenance (~1 minute)
```

Nodes boot unconfigured. `tbx status` prints the exact `talosctl gen config` and
`apply-config` commands for the state it sees — the steps below are those hints, plus the
Cilium-specific extras.

## 2. Generate machine config with the tbx patches

`tbx manifests demo` prints two machine-config patches: the **registry mirrors** patch
(pull-through mirrors served by `tbxd` on the host — required, since guest-originated
registry TLS is unreliable behind corporate agents like GlobalProtect) and the
**virtio_balloon** kernel module patch. Save them together as `patch-all.yaml`:

```yaml
machine:
  registries:
    mirrors:
      docker.io:
        endpoints:
          - http://172.30.0.1:5055
      ghcr.io:
        endpoints:
          - http://172.30.0.1:5056
      quay.io:
        endpoints:
          - http://172.30.0.1:5057
      registry.k8s.io:
        endpoints:
          - http://172.30.0.1:5058
  kernel:
    modules:
      - name: virtio_balloon
```

Cilium replaces both the default CNI and kube-proxy, so add `patch-cilium.yaml`:

```yaml
cluster:
  network:
    cni:
      name: none
  proxy:
    disabled: true
```

Generate the config (the endpoint DNS name comes from the `tbx status` hint;
`<node>.<cluster>.k8s.test` resolves via the resolver `tbx system install` set up):

```sh
talosctl gen config demo https://demo-cp-1.demo.k8s.test:6443 --output-dir . \
  --config-patch @patch-all.yaml --config-patch @patch-cilium.yaml
```

## 3. Apply config and bootstrap

Node IPs are in `tbx status` (here .2 = control plane, .3/.4 = workers):

```sh
talosctl apply-config --insecure --nodes 172.30.0.2 --file controlplane.yaml
talosctl apply-config --insecure --nodes 172.30.0.3 --file worker.yaml
talosctl apply-config --insecure --nodes 172.30.0.4 --file worker.yaml

export TALOSCONFIG=$PWD/talosconfig
talosctl config endpoint 172.30.0.2
talosctl config node 172.30.0.2
talosctl bootstrap        # retry until the configured apid is up (~1–2 min after apply)
talosctl kubeconfig ./kubeconfig
export KUBECONFIG=$PWD/kubeconfig
kubectl get nodes         # all 3 register within ~2 min, NotReady until Cilium lands
```

## 4. Install Cilium

Talos specifics: KubePrism serves the API on `localhost:7445`, cgroups are pre-mounted, and
the agent needs an explicit capability list. talosbox specifics: enable **L2 announcements**
(the default LB reachability mode) and the **ingress controller**, and pin the shared ingress
LoadBalancer to **`.200`** — the embedded DNS resolves `*.<cluster>.k8s.test` to the
cluster's `.200` by convention. The BGP control plane stays enabled but idle so the cluster can
later switch to the BGP resources rendered by `tbx manifests`.

```sh
tbx manifests demo cilium-values > cilium-values.yaml
helm repo add cilium https://helm.cilium.io/
helm install cilium cilium/cilium --version 1.19.6 -n kube-system \
  --values cilium-values.yaml
```

The rendered values include the Talos-specific settings above and size Cilium's Kubernetes
client for up to 40 single-address LB Services with the default 5-second lease renewal
deadline: `40 × (1 / 5s) = 8 QPS`, with burst 10 slightly above that calculated rate.

All images pull through the tbx mirrors; nodes go `Ready` in ~1–2 minutes. Then apply the
LB pool and L2 announcement policy (the `k8s` section of `tbx manifests`; the `talos` section
holds the machine-config patches already applied in step 2):

```sh
tbx manifests demo k8s | kubectl apply -f -
```

## 5. Deploy nginx and expose it

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
kubectl get svc -n kube-system cilium-ingress   # EXTERNAL-IP: 172.30.0.200
```

## 6. Open it

```sh
curl -i http://nginx.demo.k8s.test/
```

or open **<http://nginx.demo.k8s.test/>** in a browser — DNS resolves it to the L2-announced
ingress VIP `172.30.0.200`, and Cilium's envoy routes by `Host` header to the nginx service.
Any other hostname under `.demo.k8s.test` works the same way; just add Ingress rules.

## Observed gotchas

- Run `tbx doctor` before creating or starting a workshop cluster. Extreme host swap or
  data-volume pressure can reset guests during image unpack and corrupt Talos EPHEMERAL data;
  free memory/disk space or reduce the cluster size instead of overriding the preflight.
- If a Talos system service reports `exec format error` (or exits 139) after an unexpected
  guest reset, assume its unpacked EPHEMERAL snapshot is truncated. Re-pulling the image does
  not replace the pinned corrupt snapshot chain; destroy and recreate the affected node or
  cluster after relieving host pressure.
- The kube-apiserver refuses connections for a minute or two after `bootstrap` while
  control-plane images pull; keep polling.
- The default namespace's PodSecurity warning on the nginx deployment is harmless for a demo.
