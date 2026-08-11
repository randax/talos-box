# Research: arm64-capable replicated storage engines on Talos Linux

Ticket: randax/talos-box#151 (part of #143). Researched 2026-08-11 against primary
sources only (Sidero Labs Talos docs, longhorn/longhorn + longhorn/website repos,
rook/rook repo + Rook docs, Ceph docs, piraeusdatastore/piraeus-operator repo,
siderolabs/extensions catalog, and registry manifests verified with
`crane manifest` — every arch claim below was checked against the actual image,
not prose).

Background: Mayastor was ruled out as amd64-only
(`docs/research/mayastor-on-talos.md`, branch `research/mayastor-on-talos`,
randax/talos-box#144). tbx targets: primary **arm64** (Apple Silicon hosts),
secondary **Linux/amd64**; cluster shapes 1–3 nodes, modest RAM, 20 GB sparse
node disks (single system disk per node).

Latest stable at time of writing: Longhorn **v1.12.0** (`gh api
repos/longhorn/longhorn/releases/latest`), Rook **v1.20.3**, Piraeus Operator
**v2.11.0** (GitHub releases, checked 2026-08-11).

## TL;DR for talos-box

**Longhorn v1 is the fit.** All 14 Longhorn v1.12.0 images are
amd64+arm64 manifest lists, Talos ships a dedicated Longhorn guide, it needs
only two catalog extensions (`siderolabs/iscsi-tools`,
`siderolabs/util-linux-tools`), and its data path is plain files on a mounted
filesystem under `/var` — sparse-disk friendly, no second disk required.
**Piraeus/LINSTOR is a credible arm64 runner-up** (all images multi-arch,
first-class Talos how-to, sparse-file-backed `fileThinPool`, `drbd` extension
in the Sidero catalog) with more moving parts. **Rook/Ceph is arm64-clean but
resource-implausible** on 1–3 modest nodes and demands a raw, filesystem-free
device tbx nodes don't have. **Longhorn v2 (SPDK) and every OpenEBS replicated
option are out** — v2 needs a raw disk + hugepages + a dedicated core;
OpenEBS's only replicated engine is Mayastor, which is amd64-only.

The Talos storage overview
([docs.siderolabs.com/kubernetes-guides/csi/storage](https://docs.siderolabs.com/kubernetes-guides/csi/storage))
blesses exactly four replicated options — Longhorn, Rook/Ceph,
Mayastor/OpenEBS, Piraeus/LINSTOR — and warns "if your cluster is small, just
running Ceph may eat up a significant amount of the resources you have
available." This survey covers all four.

---

## 1. Longhorn v1 engine — arm64 VERIFIED, best fit

### arm64 + amd64 image verification (high confidence)

`crane manifest` on every image in the official release image list
([`deploy/longhorn-images.txt` @ v1.12.0](https://github.com/longhorn/longhorn/blob/v1.12.0/deploy/longhorn-images.txt)),
verified 2026-08-11 — **all are manifest lists containing both amd64 and
arm64**:

| image (docker.io) | archs |
|---|---|
| `longhornio/longhorn-manager:v1.12.0` | amd64, arm64 |
| `longhornio/longhorn-engine:v1.12.0` | amd64, arm64 |
| `longhornio/longhorn-instance-manager:v1.12.0` | amd64, arm64 |
| `longhornio/longhorn-ui:v1.12.0` | amd64, arm64 |
| `longhornio/longhorn-share-manager:v1.12.0` | amd64, arm64 |
| `longhornio/backing-image-manager:v1.12.0` | amd64, arm64 |
| `longhornio/longhorn-cli:v1.12.0` | amd64, arm64 |
| `longhornio/support-bundle-kit:v0.0.86` | amd64, arm64 |
| `longhornio/csi-attacher:v4.12.0` | amd64, arm, arm64, s390x |
| `longhornio/csi-provisioner:v5.3.0-20260514` | amd64, arm, arm64, ppc64le, s390x |
| `longhornio/csi-resizer:v2.1.0-20260514` | amd64, arm, arm64, ppc64le, s390x |
| `longhornio/csi-snapshotter:v8.5.0-20260514` | amd64, arm, arm64, ppc64le, s390x |
| `longhornio/csi-node-driver-registrar:v2.17.0` | amd64, arm, arm64, s390x |
| `longhornio/livenessprobe:v2.19.0` | amd64, arm, arm64, s390x |

Longhorn's [best practices](https://github.com/longhorn/website/blob/master/content/docs/1.12.0/best-practices.md)
list supported architectures as exactly "AMD64, ARM64", and **Talos Linux
1.11.5 is on the verified-OS table** for the v1.12 release.

### Talos extensions / schematic

Both the [Talos Longhorn guide](https://docs.siderolabs.com/kubernetes-guides/csi/longhorn)
and [Longhorn's own Talos support doc](https://github.com/longhorn/website/blob/master/content/docs/1.12.0/advanced-resources/os-distro-specific/talos-linux-support.md)
require two catalog extensions (both present in
[siderolabs/extensions](https://github.com/siderolabs/extensions) `storage/`):

```yaml
customization:
  systemExtensions:
    officialExtensions:
      - siderolabs/iscsi-tools      # iscsid + iscsiadm for PV attach
      - siderolabs/util-linux-tools # fstrim for volume trimming
```

No kernel args, no sysctls for the v1 engine.

### Machine-config patches

Longhorn's Talos doc (Talos ≥ 1.10 section) gives two pieces:

1. Kubelet bind mount of the data path (required — kubelet runs in a
   container on Talos):

   ```yaml
   machine:
     kubelet:
       extraMounts:
         - destination: /var/mnt/longhorn   # or /var/lib/longhorn (default data path)
           type: bind
           source: /var/mnt/longhorn
           options: [bind, rshared, rw]
   ```

2. Optionally a `UserVolumeConfig` to dedicate a disk/partition, auto-mounted
   at `/var/mnt/<name>`, with helm value
   `defaultSettings.defaultDataPath=/var/mnt/longhorn`. For tbx's
   single-system-disk nodes the simpler documented alternative is bind-mounting
   the default `/var/lib/longhorn` on the system disk (the "Up to Talos v1.9.x"
   pattern, still valid) and optionally
   `defaultSettings.storageReservedPercentageForDefaultDisk` to protect the OS.

3. PSA namespace label — Talos enforces `baseline` by default; Longhorn
   requires privileged
   ([Talos guide](https://docs.siderolabs.com/kubernetes-guides/csi/longhorn)):

   ```bash
   kubectl create ns longhorn-system
   kubectl label ns longhorn-system pod-security.kubernetes.io/enforce=privileged
   ```

### Data path — filesystem-backed, fits sparse disks

V1 replicas are sparse files on an existing mounted filesystem (the guide's
`UserVolumeConfig` provisions a formatted volume; default data path is
`/var/lib/longhorn` on `/var`'s xfs). **No raw device needed** — this is the
only surveyed engine whose documented Talos path works with tbx's single
20 GB sparse system disk as-is. Longhorn thin-provisions and over-provisions by
default ([best practices, "Minimal Available Storage and
Over-provisioning"](https://github.com/longhorn/website/blob/master/content/docs/1.12.0/best-practices.md)).

### Min nodes / resource cost

- Recommended: "3 nodes, 4 vCPUs per node, 4 GiB per node"
  ([best practices](https://github.com/longhorn/website/blob/master/content/docs/1.12.0/best-practices.md)).
  This is a production recommendation, not a hard floor; replica count is a
  StorageClass parameter, so 1-node (replica 1) and 2-node (replica 2)
  clusters work degraded.
- Reserved CPU: "Guaranteed Instance Manager CPU" defaults to **12% of node
  allocatable per node** for the v1 instance-manager pod (same doc,
  "Guaranteed Instance Manager CPU" — configurable, overridable per node).

### Chart / image pinning for pre-warming

- Chart: `longhorn` from `https://charts.longhorn.io`, chart version
  **1.12.0** (chart version tracks app version).
- Images: the 14 images above — **single registry `docker.io/longhornio/`**,
  exact tags from `deploy/longhorn-images.txt` in the pinned release tag.

### Longhorn v2 / SPDK engine — arm64-capable but NOT a fit (note separately)

Unlike Mayastor's SPDK, Longhorn v2 **does support arm64**: the architecture
list is "AMD64, ARM64" and the SSE4.2 requirement is scoped to "AMD64 CPUs
require SSE4.2 instruction support"
([best practices, V2 section](https://github.com/longhorn/website/blob/master/content/docs/1.12.0/best-practices.md)).
But its requirements rule it out for tbx shapes:

- "a raw, unformatted block device" (`RawVolumeConfig` on Talos; "block disks
  that already contain a filesystem or partition table are rejected" —
  [Talos guide](https://docs.siderolabs.com/kubernetes-guides/csi/longhorn)) —
  tbx nodes have no spare disk;
- "Additional 1 CPU core per node" (spdk_tgt polls at 100% of a dedicated
  core) and "Additional 2 GiB memory per node reserved for huge pages
  (1024 × 2 MiB)";
- kernel modules `nvme_tcp`, `vfio_pci`, `uio_pci_generic` (+ `ublk_drv`),
  `vm.nr_hugepages=1024` sysctl; kernel ≥ 6.7 recommended.

---

## 2. Piraeus / LINSTOR (DRBD) — arm64 VERIFIED, viable runner-up

### arm64 + amd64 image verification (high confidence)

Piraeus Operator v2.11.0's pinned component versions
([`config/manager/0_piraeus_datastore_images.yaml` @ v2.11.0](https://github.com/piraeusdatastore/piraeus-operator/blob/v2.11.0/config/manager/0_piraeus_datastore_images.yaml)),
all verified multi-arch with `crane manifest` 2026-08-11:

| image | archs |
|---|---|
| `quay.io/piraeusdatastore/piraeus-operator:v2.11.0` | amd64, arm64 |
| `quay.io/piraeusdatastore/piraeus-server:v1.34.2` (LINSTOR controller + satellite) | amd64, arm64 |
| `quay.io/piraeusdatastore/piraeus-csi:v1.12.0` | amd64, arm64 |
| `quay.io/piraeusdatastore/drbd-reactor:v1.12.0` | amd64, arm64 |
| `quay.io/piraeusdatastore/piraeus-ha-controller:v1.3.3` | amd64, arm64 |
| `quay.io/piraeusdatastore/drbd-shutdown-guard:v1.1.2` | amd64, arm64 |
| `registry.k8s.io/sig-storage/csi-provisioner:v6.2.0` (+ other sidecars) | amd64, arm, arm64, … |

The `drbd-module-loader` compile-from-source image is **not used on Talos** —
the kernel module comes from the Talos extension instead (below), so its arch
support is irrelevant.

### Talos extensions / schematic — first-class support

Piraeus ships an official Talos how-to
([`docs/how-to/talos.md` @ v2.11.0](https://github.com/piraeusdatastore/piraeus-operator/blob/v2.11.0/docs/how-to/talos.md)):

```yaml
customization:
  systemExtensions:
    officialExtensions:
      - siderolabs/drbd   # in the catalog's storage/ directory
```

The extension packages the DRBD kernel module built against each Talos
kernel; current tags are `9.3.3-<talos-version>` (`crane ls
ghcr.io/siderolabs/drbd`), i.e. DRBD 9.3.3 — matching the operator's expected
`drbd-module-loader v9.3.3`. **Coupling caveat: the extension tag is bound to
the exact Talos release**, so tbx's schematic pin and Talos upgrades must move
together.

### Machine-config patches

From the same Talos how-to:

```yaml
machine:
  kernel:
    modules:
      - name: drbd
        parameters: [usermode_helper=disabled]
      - name: drbd_transport_tcp
      # - name: dm-thin-pool   # only for LVM_THIN pools
```

Plus a documented `LinstorSatelliteConfiguration` patch (`talos-loader-override`)
that deletes the module-loader/systemd bits and redirects LVM state to
`/var/etc/lvm/*` (verbatim in the how-to). The operator's release manifest
creates the `piraeus-datastore` namespace **already labeled
`pod-security.kubernetes.io/enforce: privileged`**
([release manifest.yaml v2.11.0](https://github.com/piraeusdatastore/piraeus-operator/releases/download/v2.11.0/manifest.yaml)),
so no extra PSA step.

### Data path — sparse-file-backed pool available

LINSTOR storage pools do not require a raw disk: the operator supports
`fileThinPool` — "file system based storage pool … files will be thinly
allocated on file systems that support sparse files"
([LinstorSatelliteConfiguration reference](https://github.com/piraeusdatastore/piraeus-operator/blob/v2.11.0/docs/reference/linstorsatelliteconfiguration.md)) —
and the official
[get-started tutorial](https://github.com/piraeusdatastore/piraeus-operator/blob/v2.11.0/docs/tutorial/get-started.md)
itself uses `fileThinPool: {directory: /var/lib/piraeus-datastore/pool1}`
(FILE_THIN provider, loopback over sparse files). That directory lives under
`/var` on Talos — fits tbx's 20 GB sparse system disk with no extra device.
`lvmThinPool`/`zfsPool` remain options if tbx ever adds a second disk.

### Min nodes / resource cost

- DRBD replicates per-resource; replica count is per StorageClass
  (`placementCount`), so 1–3 nodes all work (tutorial shows a 3-node pool).
  Talos docs characterize it as "replicated block storage with low overhead"
  ([storage overview](https://docs.siderolabs.com/kubernetes-guides/csi/storage)).
- No official CPU/RAM floor is published (checked operator docs). Structural
  cost: one JVM LINSTOR controller (Deployment) + one JVM satellite per node
  (`piraeus-server` image) + drbd-reactor + CSI pods; replication itself is
  in-kernel (DRBD), which is where the "low overhead" claim comes from.
  **Unverified estimate:** plan roughly 0.5–1 GiB per node for the Java
  components — measure before adopting.

### Pinning for pre-warming

- Install is a single pinned manifest:
  `https://github.com/piraeusdatastore/piraeus-operator/releases/download/v2.11.0/manifest.yaml`
  (kustomize/`kubectl apply --server-side`; README-documented method).
- Registries: `quay.io/piraeusdatastore/*` (versions per the image-config
  table above) + `registry.k8s.io/sig-storage/*` CSI sidecars +
  `ghcr.io/siderolabs/drbd:<9.3.3-talosver>` baked into the boot image.

---

## 3. Rook / Ceph — arm64 VERIFIED, resource-implausible for tbx

### arm64 + amd64 image verification (high confidence)

Rook v1.20.3's official image list
([`deploy/examples/images.txt`](https://github.com/rook/rook/blob/v1.20.3/deploy/examples/images.txt));
key images `crane`-verified 2026-08-11:

- `docker.io/rook/ceph:v1.20.3` → amd64, arm64
- `quay.io/ceph/ceph:v20.2.2` (default `CephCluster` image in
  [`cluster.yaml`](https://github.com/rook/rook/blob/v1.20.3/deploy/examples/cluster.yaml)) → amd64, arm64
- `quay.io/cephcsi/cephcsi:v3.17.0` (v3.15.0 also checked) → amd64, arm64
- `registry.k8s.io/sig-storage/csi-*` sidecars → multi-arch (verified above)

[Rook prerequisites](https://rook.io/docs/rook/v1.20/Getting-Started/Prerequisites/prerequisites/)
state support for "amd64 / x86_64 and arm64" outright. Full pre-warm list is
the 12 images in `images.txt` across four registries (docker.io, quay.io,
registry.k8s.io, gcr.io). Charts: `rook-ceph` + `rook-ceph-cluster`
v1.20.3 from `https://charts.rook.io/release`.

### Requirements that break tbx fit

- **Raw device mandatory**: OSDs need "raw devices (no partitions or formatted
  filesystems)", raw partitions, or LVM LVs with no filesystem
  ([prerequisites](https://rook.io/docs/rook/v1.20/Getting-Started/Prerequisites/prerequisites/)).
  tbx nodes have a single formatted 20 GB system disk → **every node would
  need a second virtual disk added to the VM shape**. No file-backed mode.
- **Resource floor**: Ceph's own
  [hardware recommendations](https://docs.ceph.com/en/latest/start/hardware-recommendations/):
  OSD "≥ 4 GB per daemon" (`osd_memory_target` default 4 GB), mon "2 cores
  minimum", "≥ 5 GB per daemon". A minimal HA cluster (3 mons + 3 OSDs + mgr +
  CSI) wants more RAM than an entire modest tbx node has. Talos docs echo
  this: "Ceph can be rather slow for small clusters … may eat up a significant
  amount of the resources you have available"
  ([storage overview](https://docs.siderolabs.com/kubernetes-guides/csi/storage)).
- Production needs 3 mon nodes; Rook's `cluster-test.yaml` single-node mode is
  explicitly non-production.
- Extensions: none required (RBD/CephFS are in-kernel in Talos; Rook
  prerequisites only ask for the `rbd` module and udev). PSA privileged label
  needed on `rook-ceph` namespace like the others.

Verdict: arm64 is fine; the shape is not. Only plausible if tbx grows a
"big cluster" profile with dedicated data disks and ≥8 GB nodes.

---

## 4. OpenEBS and everything else

- **OpenEBS has no arm64-viable replicated engine.** Current OpenEBS's only
  replicated engine is Replicated PV Mayastor ("OpenEBS Replicated Storage"
  creates "an NVMe target accessible over TCP" —
  [openebs.io/docs](https://openebs.io/docs/)), which is amd64-only
  (`docs/research/mayastor-on-talos.md`; openebs/mayastor#1568 open, no
  arm64 images at v2.11.1). The older replicated engines cStor and Jiva were
  deprecated through the 3.x line in favor of CSI/Mayastor and are legacy in
  4.x ([OpenEBS v3 deprecation notices](https://github.com/orgs/openebs/discussions/3446));
  the remaining OpenEBS engines (LocalPV hostpath/LVM/ZFS) are local, not
  replicated.
- **siderolabs/extensions storage catalog** (`storage/` directory, checked via
  `gh api`): `btrfs, drbd, fuse3, iscsi-tools, mdadm, multipath-tools,
  nfs-utils, nfsd, nfsrahead, px-fuse, trident-iscsi-tools, zfs`. The only
  entries backing a replicated engine are `iscsi-tools`/`util-linux-tools`
  (Longhorn) and `drbd` (Piraeus/LINSTOR) — consistent with the four options
  the Talos storage overview names. `px-fuse` backs Portworx (commercial,
  amd64-focused, not Talos-docs-blessed) — not surveyed further.

---

## Ranked fit for tbx (1–3 nodes, modest RAM, 20 GB sparse single disk)

| # | engine | arm64+amd64 | raw disk? | extensions | fit |
|---|--------|-------------|-----------|------------|-----|
| 1 | **Longhorn v1.12.0** | verified, all 14 images | **no** — sparse files under `/var` | iscsi-tools, util-linux-tools | **Recommended.** Talos-documented, single registry to pre-warm, replica count 1–3 matches tbx shapes, ~12% CPU/node reserved. |
| 2 | **Piraeus/LINSTOR v2.11.0** | verified, all images | **no** — `fileThinPool` sparse files under `/var` | drbd (Talos-version-coupled) | Viable fallback / performance option (in-kernel DRBD replication). More moving parts: kernel-module params, satellite patch, extension↔Talos version lockstep, JVM RAM cost unquantified. |
| 3 | Longhorn v2 (SPDK) | verified (same images) | **yes** + 2 GiB hugepages + 1 dedicated core/node | + nvme_tcp/vfio kernel modules | Not fit for modest RAM / single-disk nodes; revisit only with a dedicated-disk node shape. |
| 4 | Rook/Ceph v1.20.3 | verified | **yes**, filesystem-free device per OSD node | none | Resource floor (≥4 GB/OSD, ≥5 GB/mon) exceeds tbx node budgets; Talos docs themselves warn against Ceph on small clusters. |
| 5 | OpenEBS (any) | ✗ | — | — | No arm64 replicated engine exists (Mayastor amd64-only; cStor/Jiva deprecated). |

Practical note for tbx: on 1-node clusters every engine degrades to
unreplicated storage; Longhorn handles this most gracefully (StorageClass
`numberOfReplicas: 1`, and `defaultSettings.replicaSoftAntiAffinity` /
"Allow Volumes Creation with Degraded Availability" cover the 2-node case —
[best practices](https://github.com/longhorn/website/blob/master/content/docs/1.12.0/best-practices.md)).
