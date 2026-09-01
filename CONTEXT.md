# CONTEXT

Glossary of domain terms for talosbox. Terms only — no implementation detail.

## Substrate-only

The default posture of talosbox: it provides the substrate — VMs, networking, DNS, image delivery — and never touches cluster state. Machine config, bootstrapping, and CNI installation are the attendees' work.

## Provisioning path

The opt-in fast path that goes beyond the substrate: talosbox generates and applies machine config, bootstraps the cluster, and installs a curated CNI. Cilium continues through a shared ingress controller with wildcard TLS; flannel continues only through a LoadBalancer path backed by talosbox-shipped MetalLB. Choosing the provisioning path never changes the substrate-only default for other clusters.

## Curated CNI

A CNI from talosbox's fixed, tested set (initially `cilium` and `flannel`). Cilium owns the curated ingress and LoadBalancer end state; flannel owns only the LoadBalancer end state through its talosbox-shipped MetalLB companion. Arbitrary user-supplied CNIs are not a supported concept.

## Ingress trust

Explicit host trust in a curated Cilium cluster's own ingress CA, making its wildcard HTTPS names browser-trusted without joining ingress trust to Talos control-plane trust.

## Curated CSI

A storage engine from talosbox's fixed, tested set (`longhorn` or `local-path`). It is available only through the provisioning path, where talosbox applies its pinned rendered objects and verifies a PersistentVolumeClaim write/readback path on every supported cluster shape. Bring-your-own CSI remains unsupported above the substrate; `tbx manifests <cluster> storage` prints the Talos mounts and PSA guidance needed to prepare a substrate-only cluster, and `talos.schematic` is the escape hatch for engines requiring other image extensions.

## Curated extension

A Talos system extension from talosbox's fixed, tested set, referenced by bare short name in a `talos.extensions` list and composed into the cluster's schematic by tbx. Arbitrary extension lists are not a supported concept; `talos.schematic` remains the escape hatch.

## Supported version window

The Talos versions a cluster may request: from the pinned floor (`MinTalosVersion`, the tested default's previous minor) up. Below the floor or malformed is refused at every request boundary; between floor and the tested default passes silently; above the default is accepted with a single newer-than-tested warning at create. Both pins live in `internal/talosversion` and move together in one diff. Stored cluster state is exempt — a floor bump never retroactively refuses an existing cluster.

## Image combination

A distinct schematic, Talos version, and architecture triple. It is what the disk-image cache is keyed by: two clusters agreeing on all three boot the same cached image, and any divergence is a separate combination to be fetched and held.

## Pin

A marker recording that a combination was explicitly pulled for offline use. A pinned combination is spared by the default prune even when no cluster references it, and only `cache prune --all` clears it.

## Host substrate

The platform-specific VM, networking, DNS, and service-manager implementation beneath the shared talosbox cluster model. A host substrate may have different mechanics without changing the guest-visible contract.

## Hypervisor

The VM engine a cluster's nodes run on, one of a fixed set the host substrate knows (`vz`, `qemu`). Chosen per cluster at create and immutable thereafter; absent, it is the host default. A host substrate may offer more than one hypervisor, each with its own capability gates, so "which hypervisor" is a property of a cluster, not of the host.

## Capability gate

A host feature whose availability is detected at runtime and reported with a reason. An unavailable capability disables only that feature rather than making the whole host unsupported.

## Doctor verdict

The classification of a host observation by its effect on cluster viability. `FAIL` means this host cannot run clusters and is the only verdict that makes doctor exit non-zero; breakage of a capability that disables only one feature is at most `WARN`.

## Subnet index

The internal allocation slot that selects a cluster's fixed `172.30.<n>.0/24` from the shared `172.30.0.0/16` pool. It is assigned and retained with the cluster rather than requested by the user, so an existing cluster whose slot collides is a re-attachment problem, not a rejected user-facing subnet-index choice.

## Ingress wildcard

The catch-all below a cluster domain: every non-node name resolves to the cluster's ingress VIP, which talosbox's own end-state probe permanently holds. It proves the curated ingress path; it does not provide name-based access to attendee applications, whose own LoadBalancer addresses are reached by IP literal.

## Size-by-hand host

A host substrate whose memory readings cannot be trusted for automatic management, so balloon and host-pressure features stand down permanently there and the user sizes resources manually. Declared per substrate, not detected: being size-by-hand is a property of the platform, not a capability gate.

## Synced reservations

The copy of every cluster's DHCP reservations — subnet plus each node's MAC and IP — that the daemon pushes to the privileged helper, which is the helper's only source of cluster state. The helper persists it and reconverges host networking from it, so it never reads the user's cluster state itself.

## Cluster domain

The DNS domain a cluster is reachable under on the host, from which its node records and ingress wildcard both derive. Chosen at cluster create and immutable thereafter; every cluster has exactly one, and no two clusters share one. A substrate concept — it exists regardless of provisioning path.

## Safe domain

A cluster domain under a top-level name reserved away from real DNS, so it can never collide with or shadow public names. Anything else is an **unsafe domain**: accepted only by explicit opt-in, because it can silently shadow real DNS for the host.

## L2 announcement path

The default ingress-VIP reachability mode in which the active cluster node announces ownership directly on the cluster's shared link.

## BGP mode

The optional ingress-VIP mode in which route advertisements integrate the cluster with routed networks. It supports ECMP and endpoint-local advertisement semantics.

## Suspended

The phase of a node whose VM is not running but whose guest memory is saved on disk, and of a cluster whose nodes are all in it. Suspended is a kind of stopped: nothing in the guest is executing. It is distinct from stopped because a resume can put the saved memory back rather than cold-booting, and that distinction only holds while whatever saved the memory is still able to restore it.

## Balloon reserve

The amount of host memory talosbox keeps out of the guests' hands. Guests are inflated — their memory handed back to the host — whenever free host memory falls below it, and a planned cluster whose memory exceeds what is left above it is what the overcommit warning is about.

## Pre-balloon

Memory reclaimed from the guests already running, before a new guest is admitted, so the new one starts against real free memory instead of pushing the host into swap. It is asked for at admission: a start that cannot be covered even after pre-ballooning is refused rather than attempted.

## Balloon hold

A claim on memory a pre-balloon just reclaimed, kept for as long as the guests it was taken for are still coming up. Without it the ordinary reconcile would hand that memory straight back to the guests it came from, re-creating the squeeze the pre-balloon was taken to prevent. A hold is released when the guests it covers are up, or when the operation that took it fails.

## Convergence gate

A named wait inside a provisioning pass that holds the pass until the cluster reaches one specific observed state. Every long stretch a provision can stall in is one of these, and a gate that can name itself is what makes a twenty-minute wait distinguishable from a hang.

## Blocker

What a convergence gate's last failed check observed — the reason the gate is still waiting. A blocker is a fact about right now, not a failure: it is only meaningful while it is fresh, and it goes away when the gate passes.

## Settling

The state of a cluster whose nodes are up but which still has things coming back on their own — an ingress VIP not yet announced, storage not yet proven live. A settling cluster needs no operator action, only time, which is why it reads apart from both converged and failed. Each thing still outstanding is a **converging** reason, and a single sample of `tbx status` can read green while they are in flight.

## Inter-cluster path

A route between two clusters on the same host — node to node, or node to the other cluster's ingress VIP — carried by the host rather than by either cluster. It is a first-class part of the multi-cluster contract, so it is checked in its own right and needs at least two running clusters to check.

## Project name

The project is written **talos-box** in every user-facing place — the public site (talos-box.dev), the repository, and prose. `tbx` is the command, not a name for the project. The one-word spelling is legacy and not used for anything new.
