#!/usr/bin/env bash
set -euo pipefail

# Export the Developer ID signing certificate for GitHub Actions secrets.
#
# Usage:
#   ./scripts/export-apple-certificate.sh [output.p12]
#
# After export, add repository secrets (same names as BlueOceanGames/attune):
#   APPLE_CERTIFICATE          — base64-encoded .p12 (see script output)
#   APPLE_CERTIFICATE_PASSWORD — export password you choose below
#   APPLE_TEAM_ID              — MB5789APU7
#   APPLE_SIGNING_IDENTITY     — Developer ID Application: Watzon Ventures LLc (MB5789APU7)
#
# For notarization (pick one approach):
#   APPLE_ID + APPLE_PASSWORD
#   APPLE_API_KEY + APPLE_API_KEY_ID + APPLE_API_ISSUER (+ APPLE_API_KEY_P8 in CI)

IDENTITY_SUBJECT="Developer ID Application: Watzon Ventures LLc"
OUTPUT="${1:-ship-signing.p12}"

if [ "$(uname -s)" != "Darwin" ]; then
  echo "This script must run on macOS." >&2
  exit 1
fi

echo "Looking for: $IDENTITY_SUBJECT"
security find-identity -v -p codesigning | grep -F "$IDENTITY_SUBJECT" || {
  echo "Developer ID certificate not found in the login keychain." >&2
  exit 1
}

echo ""
echo "Export the certificate and private key from Keychain Access:"
echo "  1. Open Keychain Access (login keychain → My Certificates)"
echo "  2. Expand '$IDENTITY_SUBJECT'"
echo "  3. Select the certificate row and the nested private key"
echo "  4. File → Export 2 items… → Format: Personal Information Exchange (.p12)"
echo "  5. Save as: $OUTPUT"
echo ""
read -r -p "Press Enter after the .p12 file exists at $OUTPUT…"

if [ ! -f "$OUTPUT" ]; then
  echo "Expected file not found: $OUTPUT" >&2
  exit 1
fi

BASE64_FILE="${OUTPUT}.base64"
base64 -i "$OUTPUT" -o "$BASE64_FILE"

echo ""
echo "Created $BASE64_FILE"
echo ""
echo "Add these GitHub repository secrets (Settings → Secrets and variables → Actions):"
echo "  APPLE_CERTIFICATE=$(wc -c <"$BASE64_FILE" | tr -d ' ') bytes from $BASE64_FILE"
echo "  APPLE_CERTIFICATE_PASSWORD=<the export password you chose>"
echo "  APPLE_TEAM_ID=MB5789APU7"
echo "  APPLE_SIGNING_IDENTITY=Developer ID Application: Watzon Ventures LLc (MB5789APU7)"
echo ""
echo "Optional notarization secrets:"
echo "  APPLE_ID=<your Apple ID email>"
echo "  APPLE_PASSWORD=<app-specific password from appleid.apple.com>"
echo ""
echo "To copy the base64 payload:"
echo "  pbcopy < $BASE64_FILE"