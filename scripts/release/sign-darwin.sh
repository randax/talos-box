#!/usr/bin/env bash
# Sign one darwin binary with the virtualization entitlement and, when the
# notary credentials are present, notarize it with Apple's own client.
#
# GoReleaser runs this as the post-build hook of each darwin build, so the
# three submissions overlap instead of queueing. Bare Mach-O binaries cannot
# be stapled; Gatekeeper fetches the ticket online, so the only thing that
# has to happen before archiving is that Apple accepted the submission.
#
# Without MACOS_SIGN_IDENTITY (a local snapshot) the binary is ad-hoc signed
# and not notarized, which is enough to inspect the pipeline but not to run
# against Virtualization.framework.
set -euo pipefail

binary=$1
entitlements=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/build/entitlements.plist
identity=${MACOS_SIGN_IDENTITY:--}

codesign --force --timestamp --options runtime \
  --sign "$identity" --entitlements "$entitlements" "$binary"
codesign --verify --strict "$binary"

if [[ "$identity" == "-" || -z "${MACOS_NOTARY_KEY_FILE:-}" ]]; then
  echo "sign-darwin: $binary signed (${identity}); notarization skipped" >&2
  exit 0
fi

archive=$(mktemp -d)/$(basename "$binary").zip
ditto -c -k "$binary" "$archive"
# App Store Connect answers 401 Unauthenticated intermittently from hosted
# runners with credentials that work elsewhere; retry the submission start
# only — a timed-out wait must not submit the same binary again.
submission=""
for attempt in 1 2 3 4; do
  if output=$(xcrun notarytool submit "$archive" --output-format json \
      --key "$MACOS_NOTARY_KEY_FILE" --key-id "$MACOS_NOTARY_KEY_ID" --issuer "$MACOS_NOTARY_ISSUER_ID" 2>&1); then
    submission=$(printf '%s' "$output" | /usr/bin/plutil -extract id raw -o - - 2>/dev/null || printf '%s' "$output" | sed -n 's/.*"id" *: *"\([^"]*\)".*/\1/p' | head -1)
    break
  fi
  echo "sign-darwin: submission attempt $attempt for $binary failed: $output" >&2
  sleep 30
done
[[ -n "$submission" ]] || { echo "sign-darwin: $binary could not be submitted" >&2; exit 1; }
echo "sign-darwin: $binary submitted as $submission" >&2
status=$(xcrun notarytool wait "$submission" --output-format json \
  --key "$MACOS_NOTARY_KEY_FILE" --key-id "$MACOS_NOTARY_KEY_ID" --issuer "$MACOS_NOTARY_ISSUER_ID" \
  --timeout "${MACOS_NOTARY_TIMEOUT:-60m}" | sed -n 's/.*"status" *: *"\([^"]*\)".*/\1/p' | head -1)
if [[ "$status" != "Accepted" ]]; then
  echo "sign-darwin: $binary notarization ended with status '${status:-unknown}' (submission $submission):" >&2
  xcrun notarytool log "$submission" --key "$MACOS_NOTARY_KEY_FILE" --key-id "$MACOS_NOTARY_KEY_ID" --issuer "$MACOS_NOTARY_ISSUER_ID" >&2 || true
  exit 1
fi
echo "sign-darwin: $binary notarized ($submission)" >&2
