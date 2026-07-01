#!/usr/bin/env bash
set -euo pipefail

# Sign and optionally notarize a macOS CLI binary for GitHub release distribution.
#
# Required for signing:
#   APPLE_SIGNING_IDENTITY (defaults to Developer ID Application: Watzon Ventures LLc)
#
# Optional for notarization (either app-specific password or App Store Connect API key):
#   APPLE_TEAM_ID
#   APPLE_ID + APPLE_PASSWORD
#   APPLE_API_KEY + APPLE_API_KEY_ID + APPLE_API_ISSUER (+ APPLE_API_KEY_P8 in CI)

BINARY="${1:?usage: $0 <binary-path>}"
IDENTITY="${APPLE_SIGNING_IDENTITY:-Developer ID Application: Watzon Ventures LLc (MB5789APU7)}"
TEAM_ID="${APPLE_TEAM_ID:-MB5789APU7}"

if [ ! -f "$BINARY" ]; then
  echo "Binary not found: $BINARY" >&2
  exit 1
fi

echo "Signing $BINARY with identity: $IDENTITY"
codesign --force --sign "$IDENTITY" --options runtime --timestamp "$BINARY"
codesign --verify --verbose=2 "$BINARY"
echo "Signature verification passed"

if [ -z "${APPLE_ID:-}" ] && [ -z "${APPLE_API_KEY:-}" ] && [ -z "${APPLE_API_KEY_P8:-}" ]; then
  echo "Skipping notarization (APPLE_ID or App Store Connect API key not configured)"
  exit 0
fi

ARCHIVE="${BINARY}.zip"
rm -f "$ARCHIVE"
/usr/bin/zip -j "$ARCHIVE" "$BINARY"

NOTARY_ARGS=(--team-id "$TEAM_ID" --wait)
if [ -n "${APPLE_API_KEY_P8:-}" ]; then
  KEY_PATH="$(mktemp -t AuthKey).p8"
  trap 'rm -f "$KEY_PATH"' EXIT
  printf '%s' "$APPLE_API_KEY_P8" | base64 --decode >"$KEY_PATH"
  NOTARY_ARGS+=(--key "$KEY_PATH" --key-id "${APPLE_API_KEY_ID:?}" --issuer "${APPLE_API_ISSUER:?}")
elif [ -n "${APPLE_API_KEY:-}" ] && [ -n "${APPLE_API_KEY_ID:-}" ] && [ -n "${APPLE_API_ISSUER:-}" ]; then
  KEY_PATH="$(mktemp -t AuthKey).p8"
  trap 'rm -f "$KEY_PATH"' EXIT
  printf '%s' "$APPLE_API_KEY" >"$KEY_PATH"
  NOTARY_ARGS+=(--key "$KEY_PATH" --key-id "$APPLE_API_KEY_ID" --issuer "$APPLE_API_ISSUER")
elif [ -n "${APPLE_ID:-}" ] && [ -n "${APPLE_PASSWORD:-}" ]; then
  NOTARY_ARGS+=(--apple-id "$APPLE_ID" --password "$APPLE_PASSWORD")
else
  echo "Skipping notarization (incomplete Apple notarization credentials)" >&2
  exit 0
fi

echo "Submitting $ARCHIVE for notarization"
xcrun notarytool submit "$ARCHIVE" "${NOTARY_ARGS[@]}"

echo "Stapling notarization ticket to $BINARY"
if xcrun stapler staple "$BINARY"; then
  spctl --assess --type execute --verbose=2 "$BINARY"
else
  echo "Warning: stapler could not attach a ticket to this bare binary (common for CLI tools)."
  echo "Notarization was accepted; Gatekeeper will validate the ticket online on first launch."
  codesign --verify --verbose=2 "$BINARY"
fi
echo "Notarization complete"