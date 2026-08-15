# CONTEXT

Glossary of domain terms for talosbox. Terms only — no implementation detail.

## Substrate-only

The default posture of talosbox: it provides the substrate — VMs, networking, DNS, image delivery — and never touches cluster state. Machine config, bootstrapping, and CNI installation are the attendees' work.

## Provisioning path

The opt-in fast path that goes beyond the substrate: talosbox generates and applies machine config, bootstraps the cluster, and installs a curated CNI. Both curated paths continue through LB extras to a live ingress VIP by default — Cilium via its native LB-IPAM and L2/BGP announcements, flannel via talosbox-shipped MetalLB (L2 only). Choosing the provisioning path never changes the substrate-only default for other clusters.

## Curated CNI

A CNI from talosbox's fixed, tested set (initially `cilium` and `flannel`). Each curated CNI has a known answer to how it reaches the production-style networking model's end state — a working LoadBalancer path and the ingress VIP — whether natively (Cilium) or via a talosbox-shipped companion (flannel + MetalLB). Arbitrary user-supplied CNIs are not a supported concept.

## Curated CSI

A storage engine from talosbox's fixed, tested set (`longhorn` or `local-path`). It is available only through the provisioning path, where talosbox applies its pinned rendered objects and verifies a PersistentVolumeClaim write/readback path on every supported cluster shape. Bring-your-own CSI remains unsupported above the substrate; `tbx manifests <cluster> storage` prints the Talos mounts and PSA guidance needed to prepare a substrate-only cluster, and `talos.schematic` is the escape hatch for engines requiring other image extensions.

## Curated extension

A Talos system extension from talosbox's fixed, tested set (initially `qemu-guest-agent`, `gvisor`, `nfs-utils`), referenced by bare short name and toggleable per cluster on top of the always-on storage extensions. Membership requires a functional probe in the cluster e2e. An extension that cannot function on a given host substrate is capability-gated, not rejected. Arbitrary extension lists remain unsupported; `talos.schematic` is the escape hatch.

## Host substrate

The platform-specific VM, networking, DNS, and service-manager implementation beneath the shared talosbox cluster model. A host substrate may have different mechanics without changing the guest-visible contract.

## Capability gate

A host feature whose availability is detected at runtime and reported with a reason. An unavailable capability disables only that feature rather than making the whole host unsupported.

## Cluster domain

The DNS domain a cluster is reachable under on the host, from which its node records and ingress wildcard both derive. Chosen at cluster create and immutable thereafter; every cluster has exactly one, and no two clusters share one. A substrate concept — it exists regardless of provisioning path.

## Safe domain

A cluster domain under a top-level name reserved away from real DNS, so it can never collide with or shadow public names. Anything else is an **unsafe domain**: accepted only by explicit opt-in, because it can silently shadow real DNS for the host.

## L2 announcement path

The default ingress-VIP reachability mode in which the active cluster node announces ownership directly on the cluster's shared link.

## BGP mode

The optional ingress-VIP mode in which route advertisements integrate the cluster with routed networks. It supports ECMP and endpoint-local advertisement semantics.
