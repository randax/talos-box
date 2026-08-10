# Walkthrough: from `tbx cluster create` to a browsable nginx URL

This guest-side recipe is shared by macOS and Linux: create a cluster with talosbox, configure
it with `talosctl`, install Cilium (as CNI **and** ingress controller), deploy nginx, and open
`http://nginx.demo.k8s.test/` in a browser. It was validated end to end on 2026-07-21 on an
Apple Silicon Mac; total time from `cluster create` to HTTP 200 was about 10 minutes with a
warm image cache. The Linux substrate implements the same contract, but its full-cluster CI
gate remains [#97](https://github.com/randax/talos-box/issues/97).

Prerequisites: `tbx` installed, its platform helper active, and `tbx doctor` passing.
On macOS, activate the helper with `sudo tbx system install`. On Linux, follow the
[Linux host setup](linux.md); do not run the macOS installer there.

## Install the cluster administration tools

The walkthrough pins `talosctl` to the Talos version used by talosbox and `kubectl` to the
matching Kubernetes minor. These Linux commands support both host architectures and verify
the downloaded clients before installing them:

```sh
case "$(uname -m)" in
  x86_64) TOOL_ARCH=amd64 ;;
  aarch64|arm64) TOOL_ARCH=arm64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

TALOS_VERSION=v1.13.6
curl -fLO \
  "https://github.com/siderolabs/talos/releases/download/${TALOS_VERSION}/talosctl-linux-${TOOL_ARCH}"
curl -fLO "https://github.com/siderolabs/talos/releases/download/${TALOS_VERSION}/sha256sum.txt"
grep "  talosctl-linux-${TOOL_ARCH}$" sha256sum.txt | sha256sum --check
sudo install -m 0755 "talosctl-linux-${TOOL_ARCH}" /usr/local/bin/talosctl
rm "talosctl-linux-${TOOL_ARCH}" sha256sum.txt

KUBERNETES_VERSION=v1.36.2
curl -fLo kubectl \
  "https://dl.k8s.io/release/${KUBERNETES_VERSION}/bin/linux/${TOOL_ARCH}/kubectl"
curl -fLo kubectl.sha256 \
  "https://dl.k8s.io/release/${KUBERNETES_VERSION}/bin/linux/${TOOL_ARCH}/kubectl.sha256"
printf '%s  kubectl\n' "$(cat kubectl.sha256)" | sha256sum --check
sudo install -m 0755 kubectl /usr/local/bin/kubectl
rm kubectl kubectl.sha256
```

Install Helm 3 with its official, checksum-verifying installer. Review the downloaded script
before executing it if required by local policy:

```sh
curl -fsSLo get-helm-3 https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3
chmod 700 get-helm-3
DESIRED_VERSION=v3.21.1 ./get-helm-3
rm get-helm-3

talosctl version --client
kubectl version --client
helm version --short
```

macOS users can install the same three clients with Homebrew, keeping `talosctl` on v1.13.6.

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
`<node>.<cluster>.k8s.test` resolves through `/etc/resolver` on macOS or the bridge's
systemd-resolved route-only domain on Linux):

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
cluster's `.200` by convention. The BGP control plane is enabled but remains idle until BGP
resources are applied.

```sh
tbx manifests demo cilium-values > cilium-values.yaml
helm repo add cilium https://helm.cilium.io/
helm install cilium cilium/cilium --version 1.19.6 -n kube-system \
  --values cilium-values.yaml
```

The rendered values include the Talos-specific settings above. Forty single-address LB Services
with the default 5-second lease renewal deadline require 8 QPS (`40 × (1 / 5s)`), below Cilium
1.19.6's 10 QPS / 20 burst defaults, so talosbox explicitly preserves that higher floor. On
Linux hosts, the default L2 announcement path already converges quickly on the helper-owned
bridge; reach for BGP when your upstream is routed, when you want ECMP, or when
`externalTrafficPolicy: Local` should advertise only nodes with local backends. On macOS, BGP
also doubles as the fast-failover path because vmnet does not honor gratuitous ARP.

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

- `kubectl apply` does not prune the announcement mode you previously applied. Before switching
  an existing L2 cluster to BGP, run `tbx bgp enable <cluster>`, rerender `cilium-values`, apply
  them with `helm upgrade`, and delete the old policy with
  `kubectl delete ciliuml2announcementpolicy <cluster>-l2 --ignore-not-found`; then apply
  `tbx manifests <cluster> k8s`. When migrating a pre-1.19 Cilium cluster, also delete its
  `CiliumBGPPeeringPolicy` named `<cluster>-bgp` while that legacy API is still served, before
  upgrading Cilium and applying the v2 resources.
- Run `tbx doctor` before creating or starting a workshop cluster. On Linux, run it again once
  the cluster bridge exists and use the [Linux doctor reference](linux.md#what-tbx-doctor-checks-on-linux)
  for the platform-specific checks. Extreme host swap or
  data-volume pressure can reset guests during image unpack and corrupt Talos EPHEMERAL data;
  free memory/disk space or reduce the cluster size instead of overriding the preflight.
- If a Talos system service reports `exec format error` (or exits 139) after an unexpected
  guest reset, assume its unpacked EPHEMERAL snapshot is truncated. Re-pulling the image does
  not replace the pinned corrupt snapshot chain; destroy and recreate the affected node or
  cluster after relieving host pressure.
- The kube-apiserver refuses connections for a minute or two after `bootstrap` while
  control-plane images pull; keep polling.
- The default namespace's PodSecurity warning on the nginx deployment is harmless for a demo.
