# Releasing Ship

The one canonical sequence. Every step exists because skipping it has already
caused a production incident (see
`docs/postmortems/2026-07-02-npxray-staging-deploy.md`).

## Why this is touchy

Agent installs (`ship provision apply`, `ship agent upgrade`) download
`ship_<version>_<os>_<arch>.tar.gz` from GitHub Releases **for the version the
CLI reports**. Release builds report the tag via ldflags; `go install
...@vX.Y.Z` builds report their true module version via build info; plain
`go build` checkouts report a `-dev` sentinel and never attempt downloads. A
tag that resolves without published assets strands every consumer who
installs it.

## Sequence

1. **Bump the dev sentinel** — edit `devVersion` in `internal/agent/rpc.go`
   to the *next* version with a `-dev` suffix (e.g. releasing `v0.5.0` →
   set `devVersion = "0.6.0-dev"`). Do NOT touch `AgentVersion`; it stays
   empty in source and is stamped by the release workflow.
2. **Commit and push to `main`.** CI must be green.
3. **Tag and push the tag:**

   ```bash
   git tag vX.Y.Z
   git push origin vX.Y.Z
   ```

   The release workflow (`.github/workflows/release.yml`) now: tests →
   builds all four platforms (asserting each binary self-reports the tag
   version and linux binaries are static) → uploads tarballs +
   `checksums.txt` to a **draft** release → verifies all five assets exist →
   publishes → smoke-tests (`go install @tag` self-reports the tag; a
   published asset downloads and checksum-verifies).
4. **Confirm the smoke job is green.** Optionally run
   `ship release check vX.Y.Z` locally.
5. **Upgrade your fleets:** `ship agent upgrade <env>` per environment, then
   deploy normally.

## Rules

- **Tags are exact `vX.Y.Z`.** No prerelease tags (`v0.5.0-rc1`) — the CLI's
  release-download path only consumes final versions, and the workflow
  refuses to build anything else.
- **Never delete a tag.** The Go module proxy caches every version it has
  ever served, forever — deleting the tag strands proxy users on a version
  you can no longer fix. Disavow a bad release with a `retract` directive in
  `go.mod` (see the `v0.4.0` entry) and cut the next patch release.
- **Published ⟺ assets exist.** Only the release workflow may publish a
  release; never create one by hand in the GitHub UI without assets, and
  never tag without pushing the tag through the workflow.
- **Consumers pin releases.** CI that deploys with Ship pins
  `go install ...@vX.Y.Z` — never `@latest` (it can race a publishing
  release) and never `@main` (dev builds cannot download agent binaries).
- **Keep the binary static.** `CGO_ENABLED=0` everywhere; the workflow fails
  if a linux binary picks up dynamic linkage. No cgo dependencies.
