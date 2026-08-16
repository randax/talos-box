# Depot.dev Linux runner catalog and cache story (issue #258)

Research date: 2026-08-17. Primary sources only (depot.dev docs, pricing, blog). Gaps are marked explicitly.

## 1. Linux runner labels, sizes, and pricing

Source: [Runner types](https://depot.dev/docs/github-actions/runner-types), confirmed by [Pricing](https://depot.dev/pricing).

Both architectures share the same size ladder, disk allotments, and per-minute prices. Ubuntu 24.04 and 22.04 images are offered for each; labels below show 24.04 (substitute `22.04` for the older image).

### amd64 (4th Gen AMD EPYC Genoa EC2 instances)

| Label | vCPU | RAM | Disk | $/min |
|---|---|---|---|---|
| `depot-ubuntu-24.04` | 2 | 8 GB | 100 GB | $0.004 |
| `depot-ubuntu-24.04-4` | 4 | 16 GB | 130 GB | $0.008 |
| `depot-ubuntu-24.04-8` | 8 | 32 GB | 150 GB | $0.016 |
| `depot-ubuntu-24.04-16` | 16 | 64 GB | 180 GB | $0.032 |
| `depot-ubuntu-24.04-32` | 32 | 128 GB | 200 GB | $0.064 |
| `depot-ubuntu-24.04-64` | 64 | 256 GB | 250 GB | $0.128 |

### arm64 (AWS Graviton4 EC2 instances)

| Label | vCPU | RAM | Disk | $/min |
|---|---|---|---|---|
| `depot-ubuntu-24.04-arm` | 2 | 8 GB | 100 GB | $0.004 |
| `depot-ubuntu-24.04-arm-4` | 4 | 16 GB | 130 GB | $0.008 |
| `depot-ubuntu-24.04-arm-8` | 8 | 32 GB | 150 GB | $0.016 |
| `depot-ubuntu-24.04-arm-16` | 16 | 64 GB | 180 GB | $0.032 |
| `depot-ubuntu-24.04-arm-32` | 32 | 128 GB | 200 GB | $0.064 |
| `depot-ubuntu-24.04-arm-64` | 64 | 256 GB | 250 GB | $0.128 |

Pricing scales linearly with vCPU ($0.002/vCPU/min), so a bigger runner that halves wall-clock time is roughly cost-neutral. Billing is "per-minute basis, tracked per second" with no one-minute minimum ([Pricing](https://depot.dev/pricing)). Plans: Developer includes 2,000 GA minutes/month, Startup 20,000, Business custom ([Pricing](https://depot.dev/pricing)).

### Disk and network

- EBS root volumes on both architectures are "provisioned with 8000 IOPS and 250 MB/s throughput" — the same for every size ([Runner types](https://depot.dev/docs/github-actions/runner-types)).
- Every Linux runner additionally gets a "disk accelerator" backed by RAM disk, buffering root-disk I/O; its size scales with runner size, 2 GB (2 vCPU) up to 32 GB (64 vCPU) ([Runner types](https://depot.dev/docs/github-actions/runner-types)).
- Runners run in AWS us-east-1 with 12.5 Gbps network throughput ([GA runners overview](https://depot.dev/docs/github-actions/overview)).
- **Gap:** the docs do not state whether network bandwidth differs per size; the 12.5 Gbps figure is quoted for the cache path generally. Larger sizes get more RAM-disk accelerator, which is the only documented per-size I/O difference.

### Provisioning model (spot vs reserved)

Jobs are assigned "a fresh instance from a standby pool" of pre-provisioned ephemeral EC2 instances, terminated after the job ([GA runners overview](https://depot.dev/docs/github-actions/overview); [5-second launch blog](https://depot.dev/blog/github-actions-breaking-five-second-barrier)). Depot states there are "no concurrency limits, cache size limits, or network limits" — "run as many jobs as you want in parallel" ([GA runners overview](https://depot.dev/docs/github-actions/overview)). **Gap:** the docs do not say whether the underlying instances are spot or on-demand, and document no spot-interruption behavior; nothing user-facing suggests jobs can be preempted.

## 2. Cache integration

- "Depot GitHub Actions runners are pre-configured to use Depot Cache for all GitHub Actions cache operations" — anything using the GitHub Actions cache API (`actions/cache`, setup-* built-in caches) is transparently routed to Depot's backend "with no changes to your workflow file" ([Cache × GitHub Actions](https://depot.dev/docs/cache/integrations/github-actions)). The docs name `setup-node`/`setup-python`/`setup-java` as examples; `setup-go` uses the same `actions/cache` toolkit API, so it is covered by the same mechanism (inference, not an explicit doc claim).
- Claimed performance: "up to 10x faster caching" than GitHub-hosted runners; cache upload/download "up to 1000 MiB/s on 12.5 Gbps of network throughput" ([GA runners overview](https://depot.dev/docs/github-actions/overview)).
- Limits/retention: default is "14 days with no limit on total cache size"; retention configurable to 7/14/30 days, size cap configurable 25 GB–500 GB or unlimited, per org ([Cache × GitHub Actions](https://depot.dev/docs/cache/integrations/github-actions); [Cache overview](https://depot.dev/docs/cache/overview)). Storage above the plan allowance bills at $0.20/GB/month (Developer plan includes 25 GB, Startup 250 GB) ([Pricing](https://depot.dev/pricing)).
- Go-specific bonus: runners launch with `GOCACHEPROG` pre-populated, so Go ≥ 1.24 writes its build cache straight to Depot Cache with zero config — "Run your Go builds as normal" ([Cache × Go](https://depot.dev/docs/cache/integrations/gocache)). This can be disabled in org settings.
- Depot Cache only applies on Depot-hosted runners; GitHub-hosted/self-hosted runners fall back to the standard GitHub cache ([Cache × GitHub Actions](https://depot.dev/docs/cache/integrations/github-actions)).

## 3. Critical caveat for our e2e lanes: no KVM

"Depot GitHub Actions runners don't currently provide `/dev/kvm`" — QEMU fails with "Could not access KVM kernel module" etc. Nested virtualization is only available in Depot's separate "Depot CI sandboxes" product ([Troubleshooting](https://depot.dev/docs/github-actions/troubleshooting)). **Our three QEMU/KVM e2e lanes cannot move to Depot GA runners as-is**; options are keeping them on GitHub-hosted runners (which do expose KVM on Linux), running QEMU in TCG mode (slow), or evaluating Depot CI sandboxes separately.

## 4. Recommendations for talos-box

- **Build/lint/test matrix:** `depot-ubuntu-24.04-4` amd64 + `depot-ubuntu-24.04-arm-4` (4 vCPU/16 GB, $0.008/min) as the starting point; bump to `-8` if `go build`/`golangci-lint` are still CPU-bound — linear pricing makes the speed-first choice nearly free in net cost. This also replaces the current ubuntu-24.04-arm GitHub runner one-for-one.
- **e2e lanes:** stay on GitHub-hosted runners (KVM requirement) until Depot ships `/dev/kvm` or we trial Depot CI sandboxes.
- **Go cache:** re-enabling `cache: true` in `actions/setup-go` is likely a clear win on Depot runners — it transparently hits Depot's 1000 MiB/s backend, and the usual "restore slower than rebuild" argument against setup-go caching does not hold at that throughput. Independently, `GOCACHEPROG` gives incremental build caching for free (Go ≥ 1.24) even without setup-go's tarball cache; setup-go's cache still adds value for the module download cache.

## Sources

- https://depot.dev/docs/github-actions/runner-types
- https://depot.dev/docs/github-actions/overview
- https://depot.dev/docs/github-actions/troubleshooting
- https://depot.dev/docs/cache/integrations/github-actions
- https://depot.dev/docs/cache/integrations/gocache
- https://depot.dev/docs/cache/overview
- https://depot.dev/pricing
- https://depot.dev/blog/github-actions-breaking-five-second-barrier
