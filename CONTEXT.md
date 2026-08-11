# CONTEXT

Glossary of domain terms for talosbox. Terms only — no implementation detail.

## Substrate-only

The default posture of talosbox: it provides the substrate — VMs, networking, DNS, image delivery — and never touches cluster state. Machine config, bootstrapping, and CNI installation are the attendees' work.

## Provisioning path

The opt-in fast path that goes beyond the substrate: talosbox generates and applies machine config, bootstraps the cluster, and installs a curated CNI. Both curated paths continue through LB extras to a live ingress VIP by default — Cilium via its native LB-IPAM and L2/BGP announcements, flannel via talosbox-shipped MetalLB (L2 only). Choosing the provisioning path never changes the substrate-only default for other clusters.

## Curated CNI

A CNI from talosbox's fixed, tested set (initially `cilium` and `flannel`). Each curated CNI has a known answer to how it reaches the production-style networking model's end state — a working LoadBalancer path and the ingress VIP — whether natively (Cilium) or via a talosbox-shipped companion (flannel + MetalLB). Arbitrary user-supplied CNIs are not a supported concept.

## Cluster domain

The DNS domain a cluster is reachable under on the host, from which its node records and ingress wildcard both derive. Chosen at cluster create and immutable thereafter; every cluster has exactly one, and no two clusters share one. A substrate concept — it exists regardless of provisioning path.

## Safe domain

A cluster domain under a top-level name reserved away from real DNS, so it can never collide with or shadow public names. Anything else is an **unsafe domain**: accepted only by explicit opt-in, because it can silently shadow real DNS for the host.
