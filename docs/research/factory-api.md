# Image Factory API: schematic retrieval, extension catalog, version listing

Research for issue #191 (part of #190). Verified 2026-08-15 against:

- Source: `siderolabs/image-factory` @ `main`, route table in
  `internal/frontend/http/http.go`, handlers in
  `internal/frontend/http/configuration.go` (schematics) and
  `internal/frontend/http/meta.go` (versions/extensions/overlays).
- Official API reference: `docs/api.md` in the same repo
  (<https://github.com/siderolabs/image-factory/blob/main/docs/api.md>).
- Live probes against `https://factory.talos.dev` (reported
  `Server: Image Factory v1.3.3`).

## 1. Schematic-by-id retrieval — YES, it exists

`GET /schematics/:schematic` returns the schematic's customization
definition as YAML (`Content-Type: application/yaml`); unknown ids return
`404 schematic not found`.

- Source: route registered in `internal/frontend/http/http.go`
  (`registerRoute(frontend.router.GET, "/schematics/:schematic", frontend.handleSchematicGet)`);
  handler `handleSchematicGet` in `internal/frontend/http/configuration.go`
  marshals the stored schematic back to YAML.
- Docs: `docs/api.md` section "`GET /schematics/:schematic`".
- Go client: `Client.SchematicGet(ctx, schematicID)` in `pkg/client/client.go`.

Live verification:

```console
$ curl -s -X POST https://factory.talos.dev/schematics -d '
customization:
  systemExtensions:
    officialExtensions:
      - siderolabs/qemu-guest-agent'
{"id":"ce4c980550dd2ab1b17bbf2b08801c7eb59418eafe8f279833297925d67c7515","schematic":"customization:\n    systemExtensions:\n        officialExtensions:\n            - siderolabs/qemu-guest-agent\n"}

$ curl -s https://factory.talos.dev/schematics/ce4c9805...c7515
customization:
    systemExtensions:
        officialExtensions:
            - siderolabs/qemu-guest-agent
```

**Verdict for talos-box:** re-composing a user-supplied schematic id with
extra extensions merged in is implementable as designed: `GET
/schematics/{id}` → parse YAML → merge `officialExtensions` → `POST
/schematics` → new id. `POST /schematics` also returns the normalized
schematic alongside the id (`{"id": ..., "schematic": ...}`), so the
round-trip is cheap to verify.

Version caveat: the GET endpoint landed 2025-11-06 (commit `d1bec579`,
"feat: implement schematic GET API") and first shipped in image-factory
v1.0.0 (2026-01-30). factory.talos.dev has it (v1.3.3), but a self-hosted
factory older than v1.0.0 returns 404/405 for it — worth a graceful error
if talos-box ever supports custom factory URLs.

## 2. Extension catalog — `GET /version/:version/extensions/official`

Lists official extensions valid for a given Talos version. `:version`
accepts `v1.10.0` or `1.10.0` (the handler prepends `v` if missing).
Response is a JSON array of objects (Go type `client.ExtensionInfo`):

```json
{
  "name": "siderolabs/qemu-guest-agent",
  "ref": "ghcr.io/siderolabs/qemu-guest-agent:10.0.2",
  "digest": "sha256:...",
  "author": "Sidero Labs",
  "description": "This system extension provides an implementation of QEMU guest agent.\n"
}
```

- Source: `handleOfficialExtensions` in `internal/frontend/http/meta.go`;
  docs: `docs/api.md` section "`GET /version/:version/extensions/official`".
- `name` matches the short form used in a schematic's
  `customization.systemExtensions.officialExtensions` list, so the catalog
  is directly usable to validate user input.
- `description` and `author` are present and human-readable — good raw
  material for validation errors and suggestions. The pinned extension
  version is embedded in `ref`'s tag (e.g. `:2.13.3-v1.10.0`); there is no
  separate version field.
- Companion endpoint with identical mechanics for SBC overlays:
  `GET /version/:version/overlays/official` →
  `[{name, image, ref, digest}]` (empty array for Talos versions without
  overlay support).
- An unknown/invalid version yields `400` (semver parse error) or `404`
  (artifacts not found).

## 3. Version listing — `GET /versions`

Returns a JSON array of all Talos versions the factory can build, sorted
ascending, `v`-prefixed:

```json
["v1.2.0","v1.2.1", ... ,"v1.10.x", ...]
```

- Source: `handleVersions` in `internal/frontend/http/meta.go`; docs:
  `docs/api.md` section "`GET /versions`".
- `GET /versions?broken=true` returns the list of versions known to be
  broken (currently `null` on factory.talos.dev, i.e. none) — useful to
  exclude from suggestions.
- Includes pre-releases when present (alpha/beta/rc tags of upcoming
  versions), so talos-box should filter with semver if it only wants
  stable releases.

## Auth and rate-limit considerations

- **Public factory.talos.dev: no authentication.** All five endpoints
  above are anonymous today (verified live). In the upstream source,
  authentication exists but is an *Enterprise* feature
  (`docs/authentication.md`: "Enterprise feature ... enabled with
  `authentication.enabled`"); when enabled, `POST /schematics`,
  `GET /schematics/:schematic`, and `/image/...` require an
  `Authorization` header, while `/versions` and
  `/version/:version/extensions|overlays/official` remain public.
  talos-box should thread through an optional `Authorization` header if
  it ever targets an Enterprise/self-hosted factory.
- **No documented or observed rate limits.** Neither `docs/api.md` nor
  the source implements request rate limiting, and no `RateLimit-*` /
  `Retry-After` headers were observed. Still, the polite pattern for a CLI:
  - At cluster-create time: one `GET /versions`, one
    `GET /version/{v}/extensions/official`, at most one
    `GET /schematics/{id}` and one `POST /schematics` — trivially cheap.
  - At offline-cache-pull time: cache `/versions` and the per-version
    extension catalog locally (the catalog for a released Talos version is
    effectively immutable); schematic YAML by id is content-addressed and
    permanently cacheable once fetched.
- **Idempotency:** `POST /schematics` is idempotent — the id is a hash of
  the normalized schematic, so re-posting the same customization returns
  the same id. Safe to call unconditionally instead of caching "did we
  already create this".
- CORS on the service allows only GET/HEAD/OPTIONS cross-origin; a CLI is
  unaffected.

## Endpoint summary

| Purpose | Endpoint | Response |
| --- | --- | --- |
| Create schematic | `POST /schematics` (body: customization YAML) | `201` JSON `{id, schematic}` |
| Fetch schematic by id | `GET /schematics/{id}` | `200` YAML customization; `404` if unknown |
| Extension catalog | `GET /version/{v}/extensions/official` | JSON `[{name, ref, digest, author, description}]` |
| Overlay catalog | `GET /version/{v}/overlays/official` | JSON `[{name, image, ref, digest}]` |
| Version list | `GET /versions` (`?broken=true` for broken list) | JSON `["v1.2.0", ...]` |
