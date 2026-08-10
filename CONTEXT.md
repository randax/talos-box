# CONTEXT

Glossary of domain terms for talosbox. Terms only — no implementation detail.

## Substrate-only

The default posture of talosbox: it provides the substrate — VMs, networking, DNS, image delivery — and never touches cluster state. Machine config, bootstrapping, and CNI installation are the attendees' work.

## Provisioning path

The opt-in fast path that goes beyond the substrate: talosbox generates and applies machine config, bootstraps the cluster, and installs a curated CNI. Both curated paths continue through LB extras to a live ingress VIP by default — Cilium via its native LB-IPAM and L2/BGP announcements, flannel via talosbox-shipped MetalLB (L2 only). Choosing the provisioning path never changes the substrate-only default for other clusters.

## Curated CNI

A CNI from talosbox's fixed, tested set (initially `cilium` and `flannel`). Each curated CNI has a known answer to how it reaches the production-style networking model's end state — a working LoadBalancer path and the ingress VIP — whether natively (Cilium) or via a talosbox-shipped companion (flannel + MetalLB). Arbitrary user-supplied CNIs are not a supported concept.

## Host substrate

The platform-specific VM, networking, DNS, and service-manager implementation beneath the shared talosbox cluster model. A host substrate may have different mechanics without changing the guest-visible contract.

## Capability gate

A host feature whose availability is detected at runtime and reported with a reason. An unavailable capability disables only that feature rather than making the whole host unsupported.

## L2 announcement path

The default ingress-VIP reachability mode in which the active cluster node announces ownership directly on the cluster's shared link.

## BGP mode

The optional ingress-VIP mode in which route advertisements integrate the cluster with routed networks. It supports ECMP and endpoint-local advertisement semantics.
