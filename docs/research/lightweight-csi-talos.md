# Lightweight local (non-replicated) storage options on Talos Linux

Research for issue #145. Verified against primary sources on 2026-08-11.

Scope: node-local, non-replicated persistent storage for small Talos clusters
(1–3 nodes, ~20 GB sparse node disks). Replicated engines (Longhorn, Mayastor,
Rook/Ceph) are out of scope.

## Summary table

| Option | Extensions needed | Machine-config prerequisite | Default SC | Access modes | Images |
|---|---|---|---|---|---|
| Talos User Volumes + hostPath | None | `UserVolumeConfig` document | n/a (no SC) | n/a (hostPath) | none |
| rancher/local-path-provisioner | None | `machine.kubelet.extraMounts` bind of the node path, or a `UserVolumeConfig` named for the path | `local-path`, not default upstream | RWO (RWX only with `sharedFileSystemPath`) | 2 |
| OpenEBS LocalPV hostpath | None | `machine.kubelet.extraMounts` bind of `/var/openebs/local` | `openebs-hostpath`, not default | RWO | 2 |

All three require the workload/provisioner namespace to be labeled
`pod-security.kubernetes.io/enforce: privileged` because Talos enforces PSA
`baseline` cluster-wide and hostPath mounts violate it
([Talos local storage guide](https://docs.siderolabs.com/kubernetes-guides/csi/local-storage),
[OpenEBS Talos platform doc](https://github.com/openebs/dynamic-localpv-provisioner/blob/develop/docs/installation/platforms/talos.md)).
None of them require a schematic / system extension — a vanilla Talos image
works for all three.

Shared warning from the Talos docs: "Local storage is not replicated, so in
case of a machine failure contents of the local storage will be lost."
([local storage guide](https://docs.siderolabs.com/kubernetes-guides/csi/local-storage))

## 1. Talos-native User Volumes (`UserVolumeConfig`)

Source: [Talos disk management / user volumes](https://docs.siderolabs.com/talos/v1.11/configure-your-talos-cluster/storage-and-disk-management/disk-management/user),
[Talos local storage guide](https://docs.siderolabs.com/kubernetes-guides/csi/local-storage).

- A machine-config document (multi-doc YAML), no extensions needed:

  ```yaml
  apiVersion: v1alpha1
  kind: UserVolumeConfig
  name: local-storage
  provisioning:
    diskSelector:
      match: "!system_disk"    # or e.g. disk.transport == 'nvme'
    minSize: 2GB
    maxSize: 2GB
  ```

- Talos partitions the matching disk (partition label `u-<name>`), formats it
  (XFS in the documented examples), and mounts it at `/var/mnt/<volume-name>`.
  The mount is "automatically propagated into the kubelet container to provide
  additional features like subPath mounts" — i.e. no `extraMounts` patch is
  needed for user volumes ([user volumes doc](https://docs.siderolabs.com/talos/v1.11/configure-your-talos-cluster/storage-and-disk-management/disk-management/user)).
- Name: 1–34 chars, ASCII letters/digits/dashes. Optional disk encryption is
  supported. Removal = delete the document; data survives until
  `talosctl wipe disk <partition> --drop-partition`.
- Pairing: use directly as a pod `hostPath: /var/mnt/<name>` volume (namespace
  must be PSA-privileged), or point a provisioner's node path at it (see below).
  Zero images, zero controllers — the simplest possible option, but no dynamic
  PVC provisioning and no StorageClass by itself.
- Fit for talos-box: on a single 20 GB sparse disk there is no second disk to
  match, so partition-type user volumes compete with the system disk's
  EPHEMERAL partition for space; sizing (`minSize`/`maxSize`) must be chosen
  up front.

## 2. rancher/local-path-provisioner

Sources: [README](https://github.com/rancher/local-path-provisioner) (v0.0.37,
latest release, published 2026-08-05 per GitHub API),
[upstream v0.0.37 manifest](https://raw.githubusercontent.com/rancher/local-path-provisioner/v0.0.37/deploy/local-path-storage.yaml),
[Talos local storage guide](https://docs.siderolabs.com/kubernetes-guides/csi/local-storage).

- Install: single manifest
  `deploy/local-path-storage.yaml` at a pinned tag, or Helm chart. Requires
  Kubernetes >= 1.12, nothing else.
- StorageClass `local-path`: `volumeBindingMode: WaitForFirstConsumer`,
  `reclaimPolicy: Delete`, **not** marked default upstream — add
  `storageclass.kubernetes.io/is-default-class: "true"` yourself.
- Access modes: RWO; ROX/RWX only via the `sharedFileSystemPath` config (an
  externally provided shared mount — not applicable to node-local use).
- Images (2): `docker.io/rancher/local-path-provisioner:v0.0.37` and a helper
  pod `docker.io/library/busybox` which upstream ships **unpinned** — pin it
  (e.g. `busybox:1.37.0`) if you pre-pull or mirror images.
- Talos prerequisites — two documented ways to make the node path writable:
  1. **User volume** (what the Talos docs show): a `UserVolumeConfig` named
     `local-path-provisioner` gives `/var/mnt/local-path-provisioner`; set
     `nodePathMap` to that path ([local storage guide](https://docs.siderolabs.com/kubernetes-guides/csi/local-storage)).
  2. **`machine.kubelet.extraMounts`**: bind-mount a directory under `/var`
     (Talos root FS is immutable; only `/var` is writable) into the kubelet,
     e.g. `/var/local-path-provisioner`, and set `nodePathMap` accordingly.
     This is the approach validated by the workshop repo (below).
  Either way the upstream default `/opt/local-path-provisioner` does not work
  on Talos. The `local-path-storage` namespace needs the PSA `privileged`
  label because helper pods mount hostPath.
- Fit: one small Deployment (1 replica), dynamic PVC provisioning, no
  disk-partition sizing decisions — space is shared with the EPHEMERAL
  partition, which is exactly right for 20 GB sparse disks. The de-facto
  default for small/dev Talos clusters and the option the Talos docs document
  end-to-end.

### What randax/Platform-Engineering-Workshop validates

`gitops/apps/local-path-provisioner.yaml` (Argo CD app, sync-wave 0, cluster
gate for everything stateful) points at a vendored copy of the upstream
v0.0.37 manifest in `gitops/components/local-path-provisioner/` with a
`VENDOR.md` documenting the Talos-specific curation (verified 2026-08-11 via
GitHub API):

- Namespace label `pod-security.kubernetes.io/enforce: privileged` — without
  it "every PVC hangs Pending" ("violates PodSecurity baseline:latest:
  hostPath", found in CI rehearsal).
- `nodePathMap` changed `/opt/local-path-provisioner` →
  `/var/local-path-provisioner`, bind-mounted into the kubelet via
  `machine.kubelet.extraMounts`.
- SC `local-path` annotated as cluster default.
- Helper image pinned to `docker.io/library/busybox:1.37.0`; provisioner image
  digest-verified `rancher/local-path-provisioner:v0.0.37`.
- v0.0.37's only upstream change is a health server + startup/liveness/
  readiness probes on the Deployment — relevant when a bootstrap waits on
  rollout Ready.

This is direct evidence that v0.0.37 + extraMounts + PSA label is a working,
CI-rehearsed Talos configuration.

## 3. OpenEBS LocalPV hostpath (dynamic-localpv-provisioner)

Sources: [OpenEBS Talos platform doc](https://github.com/openebs/dynamic-localpv-provisioner/blob/develop/docs/installation/platforms/talos.md),
[OpenEBS install docs](https://openebs.io/docs/quickstart-guide/installation),
[chart values v4.5.1](https://raw.githubusercontent.com/openebs/dynamic-localpv-provisioner/v4.5.1/deploy/helm/charts/values.yaml).

- Install: Helm chart `openebs` from `https://openebs.github.io/openebs`
  (Helm >= 3.2); for localpv-only add
  `--set engines.replicated.mayastor.enabled=false`. Latest
  dynamic-localpv-provisioner release: v4.5.1 (2026-06-09, GitHub API).
- StorageClass `openebs-hostpath`, base path `/var/openebs/local`,
  `WaitForFirstConsumer`, not default. RWO only.
- Images (2): `docker.io/openebs/provisioner-localpv` and helper
  `docker.io/openebs/linux-utils` (chart pins tags).
- Talos prerequisites (from OpenEBS's own Talos doc): no extensions; a
  kubelet extraMounts patch is **required**:

  ```yaml
  machine:
    kubelet:
      extraMounts:
        - destination: /var/openebs/local
          type: bind
          source: /var/openebs/local
          options: [rbind, rshared, rw]
  ```

  plus PSA `privileged` labels on the `openebs` namespace, and the caveat:
  "you must remember to pass the `--preserve` argument when running
  `talosctl upgrade`" or hostpath data is lost.
- Fit: functionally equivalent to local-path-provisioner for this use case but
  carried by a much larger umbrella chart (full OpenEBS pulls in Mayastor,
  LVM/ZFS operators unless disabled) and its Talos guidance lives outside the
  Talos docs. Fine, but heavier to pin and mirror than one vendored manifest.

## 4. Anything else the Talos docs bless

The Talos local-storage guide documents exactly three things: user volumes,
plain `hostPath` pods on a user volume, and local-path-provisioner
([guide](https://docs.siderolabs.com/kubernetes-guides/csi/local-storage)).
A fourth zero-image variant follows from the same primitives: static
Kubernetes `local`-type PVs (`kubernetes.io/no-provisioner` StorageClass,
`WaitForFirstConsumer`) backed by a user-volume mount — no controller at all,
but PVs must be hand-created per node. Everything else in the Talos storage
docs (Longhorn, Rook/Ceph, OpenEBS Mayastor) is replicated and out of scope;
Mayastor is also the only one needing schematic changes (hugepages), which
none of the local options do.

## Recommendation shape for talos-box (1–3 nodes, 20 GB sparse disks)

local-path-provisioner v0.0.37, vendored+pinned manifest, `nodePathMap` under
`/var`, `machine.kubelet.extraMounts` bind, PSA-privileged namespace, SC
`local-path` as cluster default, busybox helper pinned — i.e. the exact
configuration the Platform-Engineering-Workshop repo already rehearses in CI.
User volumes remain the right primitive if a dedicated data partition/disk is
ever added, and they need no manifest at all for single-app hostPath use.
