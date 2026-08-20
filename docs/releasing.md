# Releasing

Releases are cut from `v*` tags by `.github/workflows/release.yml`, which runs
GoReleaser (`.goreleaser.yaml`) on a macOS runner: darwin/arm64 is built
natively (cgo for Virtualization.framework, ad-hoc codesigned with the
virtualization entitlement), and linux/amd64 + linux/arm64 are cross-compiled
with `CGO_ENABLED=0`.

## Cutting a release

1. Bump `VERSION` to the new version without the `v` prefix (`0.2.0`,
   `0.2.0-rc.1`) and land it on `main` through a normal PR. `VERSION` is the
   single source of truth the Nix flake reads: a flake build reports
   `<VERSION>+<short commit>` (for example `0.2.0+abc1234`).
2. Tag the merged commit and push the tag:

   ```sh
   git tag v0.2.0 && git push origin v0.2.0
   ```

3. The tag triggers two things at once:
   - **Depot CI `Floor e2e`** — the floor-version KVM e2e against the oldest
     supported Talos (`TBX_E2E_TALOS_VERSION` in
     `.depot/workflows/floor-e2e.yml`). This is the release gate.
   - **GitHub Actions `Release`** — its `gate` job refuses a tag that disagrees
     with `VERSION`, then waits (up to 60 minutes) for the floor lane's check
     run to pass. A red floor blocks the release by construction.
4. Once the gate passes, GoReleaser publishes the GitHub Release with
   per-platform tarballs and `checksums.txt`, and pushes the `tbx` cask to
   [`randax/homebrew-tap`](https://github.com/randax/homebrew-tap) via the
   `HOMEBREW_TAP_TOKEN` secret (fine-grained PAT, Contents: read/write on the
   tap only).

Prerelease tags (`-rc.N`, `-beta.N`) are published as GitHub prereleases and
**skip the tap**, so `brew install randax/tap/tbx` always resolves to a stable
tag.

## Rehearsing without releasing

- `goreleaser release --snapshot --clean --skip=publish` builds everything
  locally into `dist/` without touching GitHub.
- Dispatching the `Release` workflow manually against an existing tag with
  `skip_floor_gate` set publishes without waiting for the floor lane. Use it
  only to rehearse the publish path with a prerelease tag.

## Install channels

| Channel | Platform | Source |
|---|---|---|
| `brew install randax/tap/tbx` | macOS (Apple Silicon) | cask in `randax/homebrew-tap` |
| GitHub Release tarballs | macOS arm64, Linux amd64/arm64 | `tbx_<version>_<os>_<arch>.tar.gz` |
| Nix flake (`github:randax/talos-box?ref=v<version>`) | Linux amd64/arm64 | builds from source at the tag |
