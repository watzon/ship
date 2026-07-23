#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

BUILD_OUTPUT="$(mktemp "${TMPDIR:-/tmp}/ship-ci.XXXXXX")"
trap 'rm -f "$BUILD_OUTPUT"' EXIT HUP INT TERM

echo "==> Verify module"
go mod verify

echo "==> Check formatting"
UNFORMATTED="$(gofmt -l .)"
if [ -n "$UNFORMATTED" ]; then
  echo "gofmt needed on:" && echo "$UNFORMATTED"
  exit 1
fi

echo "==> Vet"
go vet ./...

echo "==> Test with race detector"
CGO_ENABLED=1 go test -race ./...

echo "==> Vulnerability scan"
go run golang.org/x/vuln/cmd/govulncheck@latest ./...

echo "==> Build smoke test"
go build -o "$BUILD_OUTPUT" ./cmd/ship
