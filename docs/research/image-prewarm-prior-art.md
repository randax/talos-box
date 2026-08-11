# Prior art: image pre-warm list formats and warming mechanisms

Research for issue #124. Question: what prior art exists for (1) pinned image-list
formats and (2) mechanisms that warm a registry/mirror cache without a running
cluster — and what should talos-box copy or avoid?

Context recap (from `internal/mirror/mirror.go`, `internal/mirror/manager.go`):
talos-box runs one pull-through mirror `Server` per upstream
(docker.io → `https://registry-1.docker.io`, others → `https://<upstream>`),
caching under `~/.talosbox/cache/mirror/<upstream>/`:

- `blobs/sha256-<hex>` — blobs staged to a temp file, sha256-verified against the
  requested digest, then atomically renamed (`cacheBlob`).
- `manifests/<repo-with-/→_>_manifests_<ref>` (+ `.ct` content-type sidecar) —
  manifests keyed by **request path** (tag or digest reference), validated by
  `validateManifest` (media type, JSON, `Docker-Content-Digest` match, requested
  digest match) and written atomically. Offline pulls replay these caches.

The cache is therefore *registry-API-shaped*, not an OCI image layout: a warm
entry is "the bytes the /v2 API would have returned for this exact request path".
Every recommendation below is mapped onto that fact.

---

## 1. crane / gcrane (google/go-containerregistry)

Sources:
- https://github.com/google/go-containerregistry/blob/main/cmd/crane/doc/crane_copy.md
- https://github.com/google/go-containerregistry/blob/main/cmd/crane/doc/crane_pull.md
- https://github.com/opencontainers/image-spec/blob/main/image-layout.md

**List format:** none — crane is a per-ref CLI (`crane copy src dst`); callers
supply their own list (see the workshop repo below, which drives crane from a
flat text file).

**Materialization:** registry API on both sides. `crane copy` "efficiently
copies a remote image from src to dst **while retaining the digest value**" —
manifests move byte-for-byte, so digest-pinned refs stay resolvable at the
destination. `crane pull --format=oci` materializes into an OCI image layout
(`oci-layout` marker + `index.json` + `blobs/<alg>/<hex>`), with
`--annotate-ref` recording the source reference as an
`org.opencontainers.image.ref.name` annotation on the index descriptor — the OCI
layout spec's standard way to keep tags in a layout.

**Multi-arch:** without `--platform`, crane copies the **whole manifest
list/index**; `--platform os/arch[/variant]` filters to one child. Filtering an
index changes what lands at the destination (the platform child, under its own
digest — the index digest no longer exists there). go-containerregistry models
this distinction explicitly (`v1.ImageIndex` vs `v1.Image`; `remote.Get` returns
a descriptor you resolve either way).

**Verification:** content-addressed end-to-end — every manifest/blob is fetched
and checked by digest; `crane manifest <ref>` is a cheap existence/inspection
probe (one authenticated GET, no blob traffic).

## 2. skopeo

Source: https://raw.githubusercontent.com/containers/skopeo/main/docs/skopeo-sync.1.md

**List format:** `skopeo sync --src yaml` is the most-developed pinned-list
format in the ecosystem. YAML keyed by registry host, then per-repo:
- `images:` explicit tag lists (empty list = all tags), and **digest entries**
  (`sha256:…`) as explicit copy targets;
- `images-by-tag-regex:` (e.g. `^1\.13\.[12]-alpine-perl$`);
- `images-by-semver:` (e.g. `">= 3.12.0"`);
- per-registry `tls-verify`, credentials, cert dirs.

**Materialization:** registry API; destinations are `docker://` (a registry) or
`dir://` (one directory per image:tag). `--scoped` namespaces destination repos
by source host — the same "preserve repository path per upstream" idea as
talos-box's per-upstream cache directories.

**Multi-arch:** default copies only the current OS/arch instance; `--all` copies
every instance of a list. `--preserve-digests` makes digest preservation a hard
requirement ("fail if the digest cannot be preserved").

**Verification:** digest-addressed copy plus `--digestfile` (writes resolved
digests after copying — a machine-readable pin receipt).

## 3. oras / oras-go

Source: zarf's usage below (`oras.land/oras-go/v2`, `content/oci`), plus
https://oras.land docs. oras-go v2 treats "an OCI layout directory" and "a
remote repository" as interchangeable `Target`s and copies graphs of descriptors
between them (`oras.Copy`). No list format of its own; it is the library layer
that tools like zarf build list-driven warming on.

## 4. zarf

Sources:
- https://docs.zarf.dev/ref/components/
- https://github.com/zarf-dev/zarf/blob/main/src/pkg/images/pull.go (fetched via `gh api`)

**List format:** each package component has an `images:` YAML array of plain
refs (tag- or digest-pinned); `zarf dev find-images` scans manifests/charts to
generate the list — the drift problem ("does the list match what actually
deploys?") is solved by *generation*, where the workshop repo (below) solves it
by *checking*.

**Materialization:** `src/pkg/images/pull.go` pulls with go-containerregistry +
oras-go v2 into an **OCI layout** inside the package
(`oras.land/oras-go/v2/content/oci`), with a shared blob `CacheDirectory` and an
`Arch` option; at deploy time images are pushed (`push.go`) through the registry
API into zarf's in-cluster registry. So: registry-API in, OCI-layout at rest,
registry-API out — never direct writes into a registry's backing store.

**Multi-arch:** pull metadata records, per ref, whether it resolved to an index
and which platforms its leaf manifests cover (`imagePullInfo.platforms`);
packages are built per-`Arch`.

## 5. kind and minikube (the anti-pattern for this feature)

Sources:
- https://kind.sigs.k8s.io/docs/user/quick-start/
- https://minikube.sigs.k8s.io/docs/handbook/pushing/

`kind load docker-image` / `kind load image-archive` and `minikube image load`
side-load a tar archive directly into the **node's** containerd image store —
no registry involved, and it requires running nodes. `minikube cache add` keeps
a host-side archive cache at `$MINIKUBE_HOME/cache/images` and loads it into
every cluster's runtime automatically. Known sharp edge (documented by kind):
side-loaded images are invisible to `Always` pull policy / `:latest` tags —
because the *registry* was never warmed, only one node's store. talos-box's
mirror-warming design is strictly better for its use case: warm once at the
registry layer and every guest pull benefits, including by-digest pulls.

## 6. Talos Linux imageCacheConfig (closest upstream analog)

Source: https://docs.siderolabs.com/talos/v1.10/configure-your-talos-cluster/images-container-runtime/image-cache

- List: `talosctl images default` prints the system images for a Talos/K8s
  version; users append CNI/workload refs. (The workshop repo's
  `check-consistency.sh` asserts exactly this invariant: what you ask Talos for
  is what you pre-pull.)
- Warm: `talosctl images cache-create` materializes the list into
  `image-cache.oci`, an **OCI image layout** directory, pushable with
  `crane push`, bundled into boot media via the imager's `--image-cache`.
- Serve: on the node, `registryd` serves the cache as a local registry with
  hit/miss logging (`talosctl logs registryd`) and upstream fallback — the same
  shape as talos-box's mirror, but seeded offline from an OCI layout.

Relevance: `talosctl images default` is the authoritative generator for the
Talos/Kubernetes portion of any talos-box pin list — don't hand-maintain those
refs.

## 7. randax/Platform-Engineering-Workshop (direct in-house prior art)

Sources (all fetched from the repo via `gh api`):
- `scripts/images.txt`
- `scripts/cloudbox-init.sh`
- `scripts/check-consistency.sh`
- `.github/workflows/images-gate.yaml`

**List format (`scripts/images.txt`):** flat text, one ref per line, with:
- `[host]` / `[mirror]` section headers routing each ref to its warming
  mechanism (`docker pull` vs `crane copy` into a localhost:5001 registry);
- every ref pinned by tag and/or digest — the header states ":latest silently
  defeats pre-pulling";
- **full-line comments only** — both consumers "read image entries verbatim and
  only strip full-line comments" (a rule stated *in the file*, with the reason);
- machine-editable regions (`x-release-please-start-version` markers) so a
  release bot can rewrite first-party pins;
- a "verified against upstream on <date>" header comment that
  `images-gate.yaml` turned into a running weekly CI check.

**Materialization (`cloudbox-init.sh`):** four phases —
1. *Preflight:* `crane manifest <ref>` per ref; "a missing image should cost
   seconds here, not surface hours into a 7.5 GB pull". Hard-fail before any
   download.
2. Host pulls (`docker pull`).
3. Start a plain `registry:3` container (persistent volume) as the mirror.
4. `crane copy` each `[mirror]` ref, with the load-bearing per-ref branch:
   - **tag-only refs:** `--platform linux/<daemon-arch>` — copying the whole
     index roughly doubled the download ("registry.k8s.io/pause is 0.3 MB for
     one platform and 573 MB as a full index");
   - **digest-pinned refs (`…@sha256:`): copied whole, all architectures.**
     A pinned digest of a multi-arch image is the *index* digest;
     `crane copy --platform` stores only the child under a different digest, so
     the pinned ref 404s in the mirror and the node silently falls back to the
     internet — "works at home and hangs on conference WiFi". They also tested
     and rejected "push the original index but only one child's blobs":
     **containerd 2.x fetches every child manifest in an index regardless of
     platform and errors out on the missing ones.**
   - If a `--platform` copy fails (index without that arch), fall back to
     copying everything: "fatter, but never a missing image offline".
   Re-runs are cheap (already-present content skipped); the registry never
   garbage-collects, documented with the recovery step (delete the volume).

**Verification:**
- `check-consistency.sh` (offline, every push): greps every `image:` /
  `imageName:` / `--image=` ref out of gitops/lab/solutions, normalizes it the
  way containerd would (`docker.io/library/` prefixing), and fails if it's not
  covered by `images.txt` — the pre-pull list can't drift from what deploys. It
  also asserts the Talos-derived control-plane refs match `KUBERNETES_VERSION`.
- `images-gate.yaml` (weekly + on PRs touching the list): per ref,
  `crane manifest` must succeed; digest pins pass as-is ("mirrored
  byte-for-byte"); tag pins that are indexes must carry **both** linux/amd64 and
  linux/arm64 (because the warm step is per-arch, a single-arch pin only fails
  on the *other* arch's machine); bare single-arch manifests are allowed only
  via an explicit `MIRROR_ARCH_EXEMPT` allowlist in `versions.env`.

---

## What talos-box should copy / avoid

### Copy

1. **The workshop `images.txt` conventions, minus `[host]`.** Flat text, one
   pinned ref per line, full-line `#` comments only, no `:latest`, digest pins
   allowed as `repo:tag@sha256:…` or `repo@sha256:…`. talos-box has no host
   Docker engine to feed, so no sections are needed at v1 — the upstream host
   in each ref already routes it to the right `~/.talosbox/cache/mirror/<upstream>/`
   directory (same mapping `Manager`/`baseFor` uses). Reserve `[section]` syntax
   as an error so it can be added compatibly later.

2. **Warm through the mirror's own request path, not a new one.** The cheapest
   correct mechanism is to replay exactly the GETs a guest would issue against
   the existing `Server` (in-process handler invocation or a loopback bind):
   `GET /v2/<repo>/manifests/<tag-or-digest>` → parse → child manifests by
   digest → config + layer blobs. Everything then lands in the *current*
   layout keys (`manifestPath` request-path keying, `blobPath`), with the
   *current* verification (`validateManifest`, `cacheBlob` digest check) and
   atomic-write behavior, and offline replay works with zero new serving code.
   This is the zarf/Talos pattern (registry API in, never raw store writes)
   applied to a cache whose at-rest format is already API-shaped.

3. **The workshop's digest-vs-tag warming branch, adapted:**
   - For a **tag ref**: fetch and store the index under the tag's request path,
     store **every child manifest** by digest (they're tiny JSON, and containerd
     2.x fetches all of them anyway — the workshop's tested finding), but fetch
     **blobs only for the target platform(s)**. Because talos-box serves the /v2
     API rather than holding an opaque store, it can do what `crane copy
     --platform` cannot: keep the original index byte-for-byte *and* skip
     foreign-arch blobs. Guest VMs are a known platform (linux/arm64 on Apple
     Silicon, linux/amd64 on the Linux host port), so default to the host's
     guest arch with a `--platform` override.
   - For a **digest-pinned ref**: store the manifest under its digest request
     path; if it's an index, same child-manifest/platform-blob treatment. The
     workshop's index-digest 404 failure mode cannot occur here because the
     index bytes are cached under the pinned digest's own key.
   - If the index lacks the target platform, warn and warm all platforms
     (workshop fallback: "never a missing image offline").

4. **Preflight before bytes.** One manifest GET per ref (the `crane manifest`
   pattern), fail the whole run with a list of unresolvable refs before any blob
   download. talos-box's `Server.fetch` already does the anonymous-token dance;
   reuse it.

5. **Derive the Talos/K8s portion of the list, don't hand-write it.**
   `talosctl images default` (per pinned Talos version) is the generator; a
   consistency check in the style of `check-consistency.sh` step 3 should assert
   the pinned kubelet/apiserver/etc. tags match the Talos/K8s versions talos-box
   actually provisions. Also warm a manifest for the *tag* the nodes actually
   pull, not only digests — offline replay is keyed by request path, so a cached
   `@sha256:` entry does not satisfy a later by-tag pull.

6. **A CI-style re-verify gate** (`images-gate.yaml` pattern) if/when the pin
   list ships in-repo: every ref resolves upstream; tag pins must be multi-arch
   indexes covering both guest arches unless explicitly exempted; scheduled runs
   catch upstream retags/deletions.

7. **Idempotent re-runs**: skip blobs/manifests already on disk by key
   (`os.Stat` before fetch), so retrying after a partial warm is cheap — both
   crane and the workshop script lean on this.

### Avoid

1. **kind/minikube-style archive side-loading** (`docker save` tars into node
   stores). Requires running nodes, warms one node not the mirror, and breaks
   `Always`/`latest` pulls. Wrong layer for this feature.

2. **Materializing an OCI image layout as the cache format.** OCI layout
   (crane/zarf/Talos `image-cache.oci`) is the right *interchange* format, but
   talos-box's cache is keyed by upstream request path with a `.ct` sidecar —
   converting to layout would mean new serving code and losing offline replay of
   tag requests. If OCI-layout import/export is ever wanted (e.g. consume a
   Talos `image-cache.oci`), write a translator that *feeds the existing warm
   path*, not a second store.

3. **Platform-filtering digest-pinned refs at the manifest level** — i.e., any
   design where the pinned index digest's bytes aren't retrievable. The
   workshop paid for this lesson (silent internet fallback). In talos-box terms:
   never rewrite or re-serialize a fetched manifest; store upstream bytes
   verbatim (the current `Server` already does — keep it that way in the warm
   path).

4. **Skipping non-target child *manifests* of an index.** containerd 2.x reads
   them all; only blobs are safely platform-filterable.

5. **Inline/trailing comments or clever syntax in the list file.** The workshop
   file states the rule and the reason: multiple independent consumers parse
   entries verbatim. Keep the grammar so trivial that `grep -vE '^\s*(#|$)'` is
   a complete parser.

6. **`:latest` or floating tags in the list** — they defeat the pin and make the
   weekly gate meaningless. Reject them at parse time rather than by convention.

7. **Regex/semver tag selectors (skopeo `images-by-tag-regex`/`images-by-semver`)
   at v1.** They exist for mirroring *ranges*; a pre-warm list wants exact pins.
   Adopting skopeo's YAML would buy expressiveness talos-box doesn't need at the
   cost of the trivial-parser property above.
