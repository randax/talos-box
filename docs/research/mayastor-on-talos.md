# Research: OpenEBS Mayastor on Talos Linux

Ticket: randax/talos-box#144. Researched 2026-08-11 against primary sources only
(OpenEBS docs, openebs/mayastor + openebs/mayastor-extensions + openebs/mayastor-docs
repos, siderolabs/extensions catalog, Sidero Labs Talos docs, Docker Hub image
manifests via `crane`).

Latest stable at time of writing: Mayastor **v2.11.1** (2026-06-17), shipped by
umbrella chart **openebs/openebs 4.5.1**
([releases](https://github.com/openebs/mayastor/releases),
[chart index](https://openebs.github.io/openebs/index.yaml)).

## TL;DR for talos-box

**Mayastor does not run on arm64.** Every v2.11.1 image on Docker Hub is
amd64-only, arm64 support is an open unplanned issue, and the docs require an
x86-64 CPU with SSE4.2. Since talos-box's primary architecture is arm64 (Apple
Silicon hosts), Mayastor is a non-starter there today; it is only viable for
amd64 clusters.

## 1. arm64 support status — NOT SUPPORTED (high confidence)

Verdict: **no arm64 builds exist; not on the roadmap.**

Evidence:

- Image manifests: `crane config docker.io/openebs/mayastor-io-engine:v2.11.1`
  (and `mayastor-agent-core`, `mayastor-csi-node`) report `architecture: amd64`,
  single-arch manifests — no manifest list, no arm tags in
  `crane ls docker.io/openebs/mayastor-io-engine` (verified 2026-08-11).
- [openebs/mayastor#1568](https://github.com/openebs/mayastor/issues/1568)
  "Not working on arm64 architecture" — **still open** (filed Dec 2023 by a
  Talos 1.6 arm64 user). Maintainer (tiagolobocastro, 2024-07):
  "Leaving this open as the open issue for arm64 support. This is not on the
  roadmap, but if some external contributor has some arm servers for us to
  test on, we'd be happy to consider arm64 support."
- [openebs/mayastor#1147](https://github.com/openebs/mayastor/issues/1147)
  (multi-arch request, closed without shipping): maintainers state CI has no
  arm64 machines and they won't release untested images; only the
  `kubectl-mayastor` plugin binary is built for aarch64.
- [OpenEBS prerequisites](https://openebs.io/docs/quickstart-guide/prerequisites):
  "x86-64 architecture with SSE4.2 instruction support required" for
  Replicated PV Mayastor (the io-engine embeds SPDK, which is the hard
  dependency; arm64 SPDK builds failed historically —
  [#1080](https://github.com/openebs/mayastor/issues/1080),
  [#1201](https://github.com/openebs/mayastor/issues/1201)).

Confidence: high — three independent primary signals (registry manifests,
maintainer statements, official docs) all agree, checked against the newest
release (v2.11.1, June 2026).

## 2. Talos system extensions / Image Factory schematic

**No system extension is required.** The
[siderolabs/extensions](https://github.com/siderolabs/extensions) catalog has
no mayastor/SPDK entry (its `storage/` directory holds btrfs, drbd, fuse3,
iscsi-tools, mdadm, multipath-tools, nfs*, px-fuse, zfs — nothing for
Mayastor). The `nvme_tcp` kernel module Mayastor needs is **built into the
Talos kernel**; the [Sidero storage guide](https://docs.siderolabs.com/kubernetes-guides/csi/storage)
says to disable the chart's module-loading init container for exactly that
reason:

```yaml
mayastor:
  csi:
    node:
      initContainers:
        enabled: false
```

So tbx's Factory schematic (`internal/imagecache/schematic.go`, which today
only sets `extraKernelArgs: [console=tty0, console=hvc0]`) needs **no new
extensions and no new kernel args** for Mayastor. Hugepages are configured via
machine sysctls (below), not kernel args. Optional: OpenEBS recommends
`nvme_core.multipath=Y` as a kernel parameter for the HA/multipath feature
([prerequisites](https://openebs.io/docs/quickstart-guide/prerequisites)) —
that one, if wanted, would go into the schematic's `extraKernelArgs`.

## 3. Machine-config patches (Talos-specific)

Source: [OpenEBS "Replicated PV Mayastor Installation on Talos"](https://openebs.io/docs/main/Solutioning/openebs-on-kubernetes-platforms/talos).

Worker/storage node patch:

```yaml
machine:
  sysctls:
    vm.nr_hugepages: "1024"          # 1024 x 2MiB = 2GiB hugepages
  nodeLabels:
    openebs.io/engine: "mayastor"    # io-engine DaemonSet node selector
  kubelet:
    extraMounts:
      - destination: /var/local
        type: bind
        source: /var/local
        options: [bind, rshared, rw]
```

Control-plane patch — exempt the namespace from Talos's default `baseline`
Pod Security enforcement:

```yaml
cluster:
  apiServer:
    admissionControl:
      - name: PodSecurity
        configuration:
          apiVersion: pod-security.admission.config.k8s.io/v1beta1
          kind: PodSecurityConfiguration
          exemptions:
            namespaces: [openebs]
```

Notes:
- Hugepages are 2MiB-sized; minimum 1024 pages (2GiB) reserved **exclusively**
  for the io-engine pod per storage node
  ([prerequisites](https://openebs.io/docs/quickstart-guide/prerequisites)).
- Expressed as a **sysctl** (`vm.nr_hugepages`), not kernel args and not
  `machine.kernel.modules`. After changing it on a live node, kubelet must be
  restarted (`talosctl -n <ip> service kubelet restart`) or the node rebooted
  so kubelet re-discovers the hugepages resource
  ([Talos solutioning doc](https://openebs.io/docs/main/Solutioning/openebs-on-kubernetes-platforms/talos)).
- Talos >= v1.8 preserves `/var` across upgrades automatically; v1.7 and lower
  need `talosctl upgrade --preserve` (same doc).

## 4. DiskPool requirements

Source: [mayastor-docs quickstart/configure-mayastor.md](https://github.com/openebs/mayastor-docs/blob/develop/quickstart/configure-mayastor.md).

- One pool = **exactly one block device**, owned exclusively by one node.
  "it should not be partitioned, formatted, or shared with another application
  or process. Any pre-existing data on the device will be destroyed." Pool
  size is fixed at creation, immutable
  ([FAQ](https://github.com/openebs/mayastor-docs/blob/develop/quickstart/faqs.md)).
- Permissible `spec.disks` schemes: bare device path (defaults to AIO),
  `aio:///dev/...`, `uring:///dev/...`; best practice is
  `/dev/disk/by-id/...` links so names survive reboots.
- CR shape (namespace must match the install namespace):

```yaml
apiVersion: openebs.io/v1beta1
kind: DiskPool
metadata:
  name: pool-on-node-1
  namespace: openebs
spec:
  node: <node-hostname>
  disks: ["aio:///dev/disk/by-id/<id>"]
```

- **Loop devices / file-backed pools:** not documented as a supported target.
  The aio/uring schemes take any block-device file, so `aio:///dev/loopN`
  works mechanically for dev setups (community-reported), but this is
  unsupported/undocumented — inference, not a doc guarantee. The docs do
  explicitly warn that RAM-backed disk emulation draws from the hugepages
  pool and is not production-suitable. For tbx dev clusters the honest
  answer is: attach a second raw virtio/NVMe disk to the VM instead.

## 5. Node count, CPU/RAM cost

- **Minimum 3 worker nodes supported**; pools >= desired replication factor,
  and the control plane never places two replicas of one volume on the same
  node ([prerequisites](https://openebs.io/docs/quickstart-guide/prerequisites),
  [FAQ](https://github.com/openebs/mayastor-docs/blob/develop/quickstart/faqs.md)).
- At 1–2 nodes it deploys but you are limited to replication factor 1–2 and
  are outside the supported envelope; the bundled etcd (3 replicas by
  default) also wants 3 nodes. Single-node = no redundancy, i.e. Mayastor's
  raison d'être is gone — local-path/ZFS-localpv is cheaper there.
- Per storage node (chart defaults,
  [mayastor-extensions v2.11.1 chart/values.yaml](https://github.com/openebs/mayastor-extensions/blob/v2.11.1/chart/values.yaml)):
  `io_engine.cpuCount: "2"` (2 dedicated, spinning poller cores — docs call
  for them to be "free and exclusive"), `memory: 1Gi` request/limit,
  `hugepages-2Mi: 2Gi` request/limit. So budget **2 pinned cores + ~3GiB RAM
  (1Gi + 2Gi hugepages) per storage node**, before etcd/loki/agents.

## 6. Helm chart structure, versions, image list

- Umbrella chart `openebs` at `https://openebs.github.io/openebs`
  (source: [openebs/openebs](https://github.com/openebs/openebs)). Version
  4.5.1 pins subcharts: `openebs-crds 4.5.1`, `localpv-provisioner 4.5.1`,
  `zfs-localpv 2.10.1`, `lvm-localpv 1.9.1`, `rawfile-localpv 0.14.1`,
  `mayastor 2.11.1`, plus `loki`/`alloy` observability
  ([index.yaml](https://openebs.github.io/openebs/index.yaml)).
- The mayastor subchart's source lives in
  [openebs/mayastor-extensions `chart/`](https://github.com/openebs/mayastor-extensions/tree/v2.11.1/chart);
  chart version == app version == mayastor release tag (v-prefixed image tags).
- Replicated storage is enabled with
  `--set engines.replicated.mayastor.enabled=true`; on Talos additionally
  `--set mayastor.csi.node.initContainers.enabled=false`
  ([Sidero storage guide](https://docs.siderolabs.com/kubernetes-guides/csi/storage)).
- Full image list for pre-warming, from `helm template openebs/openebs 4.5.1`
  with mayastor enabled (default values; mayastor-specific images marked *):

  | Registry | Image |
  |---|---|
  | docker.io | openebs/mayastor-io-engine:v2.11.1 * |
  | docker.io | openebs/mayastor-agent-core:v2.11.1 * |
  | docker.io | openebs/mayastor-agent-ha-cluster:v2.11.1 * |
  | docker.io | openebs/mayastor-agent-ha-node:v2.11.1 * |
  | docker.io | openebs/mayastor-api-rest:v2.11.1 * |
  | docker.io | openebs/mayastor-csi-controller:v2.11.1 * |
  | docker.io | openebs/mayastor-csi-node:v2.11.1 * |
  | docker.io | openebs/mayastor-metrics-exporter-io-engine:v2.11.1 * |
  | docker.io | openebs/mayastor-obs-callhome:v2.11.1 * |
  | docker.io | openebs/mayastor-obs-callhome-stats:v2.11.1 * |
  | docker.io | openebs/mayastor-operator-diskpool:v2.11.1 * |
  | docker.io | openebs/etcd:3.6.4-debian-12-r0 * |
  | docker.io | openebs/alpine-bash:4.5.0, openebs/alpine-sh:4.5.0 |
  | docker.io | openebs/provisioner-localpv:4.5.1, openebs/lvm-driver:1.9.1, openebs/zfs-driver:2.10.1 |
  | registry.k8s.io | sig-storage/csi-provisioner:{v5.2.0,v6.1.0}, csi-attacher:v4.8.1, csi-node-driver-registrar:v2.13.0, csi-resizer:{v1.13.2,v2.0.0}, csi-snapshotter:{v7.0.0,v8.2.0}, snapshot-controller:{v7.0.0,v8.2.0} |
  | docker.io | grafana/loki:3.4.2, grafana/alloy:v1.8.1, kiwigrid/k8s-sidecar:1.30.2, nats:2.9.17-alpine, natsio/nats-box:0.13.8, natsio/nats-server-config-reloader:0.10.1, natsio/prometheus-nats-exporter:0.11.0 |
  | quay.io | minio/minio:RELEASE.2024-12-18T13-15-44Z, minio/mc:RELEASE.2024-11-21T17-21-54Z, prometheus-operator/prometheus-config-reloader:v0.81.0 |

  (loki/minio/nats stack is loki-chart plumbing; disable observability to
  shrink the pre-warm set substantially.)

## Implications for talos-box

1. Do not offer Mayastor on arm64 clusters — gate any integration on
   `amd64` and re-check [#1568](https://github.com/openebs/mayastor/issues/1568)
   before revisiting.
2. If offered for amd64: no schematic change needed (`schematic.go` stays as
   is), only machine-config patches (sysctl + label + kubelet mount + PSA
   exemption) and the helm values above.
3. Dev-cluster ergonomics are poor: 2 pinned cores + 3GiB per node, 3 nodes,
   and a dedicated raw disk per node. For tbx-sized clusters,
   zfs-localpv/rawfile-localpv from the same umbrella chart cover the arm64
   and small-footprint cases.
