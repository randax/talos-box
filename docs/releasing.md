# Releasing

Releases are cut from `v*` tags by `.github/workflows/release.yml`, which runs
GoReleaser (`.goreleaser.yaml`) on a macOS runner: darwin/arm64 is built
natively (cgo for Virtualization.framework), signed with the Developer ID
Application identity (hardened runtime, virtualization entitlement) and
notarized by Apple; linux/amd64 + linux/arm64 are cross-compiled with
`CGO_ENABLED=0`.

## Signing and notarization

Each darwin build's post hook, `scripts/release/sign-darwin.sh`, signs the
binary with the Developer ID identity (hardened runtime, virtualization
entitlement) and submits it to the notary service with `xcrun notarytool`,
waiting for acceptance; the three submissions overlap because the builds do.
Bare binaries cannot be stapled, so Gatekeeper fetches the ticket online. The
workflow imports the identity into a throwaway keychain first. (GoReleaser's
own `notarize.macos` pipe was tried and dropped: its submissions failed with
`401 Unauthenticated` from hosted runners while the same key worked
elsewhere.) The release gate refuses to publish when any of these secrets is
missing:

| Secret | Content |
|---|---|
| `MACOS_SIGN_P12` | base64 of the `Developer ID Application: Øyvind Randa (F469T889XJ)` identity as PKCS#12 |
| `MACOS_SIGN_P12_PASSWORD` | the `.p12` password |
| `MACOS_NOTARY_KEY` | base64 of the App Store Connect API key (`.p8`, role Developer) |
| `MACOS_NOTARY_KEY_ID` | the key's ID |
| `MACOS_NOTARY_ISSUER_ID` | the App Store Connect issuer UUID |

The identity, key and `.p12` live outside the repo on the owner's machine
(`~/Library/Application Support/talos-box-signing/`); the certificate expires
2031-08-22. A local `goreleaser release --snapshot --skip=publish` without
`MACOS_SIGN_P12` in the environment builds unsigned darwin binaries, which is
fine for checking the pipeline but not for running them: Virtualization.framework
needs the entitlement, so a local build for use goes through `make build`.

## Cutting a release

1. Bump `VERSION` to the new version without the `v` prefix (`0.2.0`,
   `0.2.0-rc.1`) and land it on `main` through a normal PR. `VERSION` is the
   single source of truth the Nix flake reads: a flake build reports
   `<VERSION>+<short commit>` (for example `0.2.0+abc1234`), or
   `<VERSION>+dirty` when built from a working tree with uncommitted changes.
2. Tag the merged commit and push the tag:

   ```sh
   git tag v0.2.0 && git push origin v0.2.0
   ```

3. The tag triggers two things at once:
   - **GitHub Actions `Floor e2e`** — the floor-version KVM e2e against the
     oldest supported Talos (`TBX_E2E_TALOS_VERSION` in
     `.github/workflows/floor-e2e.yml`). This is the release gate.
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
