# Plan 006: Rework install/update paths (agent binary resolution, release process, docs)

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md` — unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**:
> `git diff --stat 0e4a826..HEAD -- internal/shipbinary/ internal/agent/rpc.go internal/cli/root.go .github/workflows/release.yml`
> The excerpts below reflect commit `0e4a826` (clean tree). If any excerpt no
> longer matches the live code, STOP.

## Status

- **Priority**: P0 (Phase A), P1 (Phase B), P2 (Phase C)
- **Effort**: L total, phased — A: M, B: M, C: S
- **Risk**: MEDIUM (touches the deploy-critical binary resolution path; every step has tests)
- **Depends on**: none
- **Category**: bug + release process
- **Planned at**: commit `0e4a826` (clean), 2026-07-02

## Why this matters

Operators (and AI agents) repeatedly hit this during first-time server setup
and Kamal→Ship cutovers:

```
local ship binary is darwin/arm64 but remote host needs linux/amd64;
install a matching release binary or run ship from a checkout with Go installed
```

The agent auto-install fallback chain (`internal/shipbinary.Resolve`) exists
but fails in practice for compounding reasons, and when it fails it hides the
cause, so operators conclude manual install is the only path. Documented as
the P2 item in `docs/postmortems/2026-07-02-npxray-staging-deploy.md`; it
recurred even after that postmortem.

## Confirmed defects (all verified against source at `0e4a826`)

1. **Wrong-module early return (P1)** — `moduleRoot()`
   (`internal/shipbinary/shipbinary.go:174-185`) runs `go env GOMOD` in the
   *caller's cwd* and never verifies the module is `github.com/watzon/ship`.
   Running ship from any other Go module's root (e.g. deploying a Go app)
   makes `crossCompile` run `go build ./cmd/ship` in the wrong module, fail
   with a non-`errNoModuleRoot` error, and `Resolve()`
   (`shipbinary.go:118-122`) **returns early — the release download is never
   attempted**. Reproduced empirically with a scratch `module example.com/notship`.
2. **Swallowed download error (P1)** — `Resolve()` (`shipbinary.go:123-130`)
   discards `downloadRelease`'s error entirely. 404, missing `curl`, network
   failure, and bad-archive all render as the same generic message.
3. **No integrity check on downloaded bytes (P1)** — `downloadRelease`
   (`shipbinary.go:187-208`) never verifies against the published
   `checksums.txt`, and nothing verifies the extracted binary's platform
   before it is pushed to a production host and installed as the
   root-privileged agent (`internal/cli/root.go:1304-1312`).
4. **curl subprocess (P2)** — `defaultHTTPGet` (`shipbinary.go:219-222`)
   shells out to `curl -fsSL`; stderr (the actual HTTP error) is discarded,
   and machines without curl fail opaquely. `internal/cli/root.go` already
   uses `net/http` — house style supports the stdlib client.
5. **Version-identity landmine (P0 root cause)** — `AgentVersion` is a
   hand-bumped constant (`internal/agent/rpc.go:30`). Verified:
   `git show v0.1.0:internal/agent/rpc.go` → `var AgentVersion = "0.4.0"`.
   Every `go install …@v0.1.0` binary (the pin example in
   `skills/ship/SKILL.md`!) self-reports **0.4.0** and chases
   `v0.4.0` release assets that never existed → permanent 404 → defect 2
   hides it. Tags are immutable; this cannot be fixed on the tag.
6. **Malformed `checksums.txt` (P1)** — `.github/workflows/release.yml:207-208`
   writes `ls -1 ship_*.tar.gz > checksums.txt` **then** appends `sha256sum`
   output. Verified on the live v0.4.1 asset: four bare-filename preamble
   lines precede the digests, so `sha256sum -c checksums.txt` errors.
7. **Release-process windows (P1)** — the workflow triggers on
   `release: published`, so: a bare `git tag && git push --tags` never builds
   assets; assets finish uploading ~3 minutes *after* "published"
   (measured on v0.4.1: published 14:55:11Z, assets 14:57:56Z); a deleted bad
   tag (v0.4.0 was deleted) stays cached forever in the Go module proxy and
   there is no `retract` directive; nothing asserts the version constant
   matches the tag being released; macOS signing failure is warn-only
   (`release.yml:119`).
8. **No agent-upgrade rollback (P1)** — `uploadShipBinaryCommand`
   (`root.go:1388-1397`) and the agent's `installBinary`
   (`internal/agent/rpc.go:905-939`) overwrite the agent binary with no
   backup and no post-upgrade health check. A bad agent binary bricks the
   host's RPC channel; recovery is manual SSH. `installBinary` also only
   compares the SHA-256 the *client* supplied — the host never validates the
   bytes are an ELF for its own architecture.
9. **Skew detection exists but is never used (P1)** — `negotiate()`
   (`internal/agent/rpc.go:475`) implements protocol-range negotiation
   (`AgentMinProtocol=1`, `AgentProtocol=2`) and no CLI code path calls it
   before deploy/promote.
10. **Stale docs steer agents into the trap (P0, trivial)** — `README.md:28`
    ("once releases are tagged"), `skills/ship/SKILL.md` pin examples
    `@v0.1.0` (see defect 5) and `VERSION=v0.1.0`, and no doc explains that
    cross-compile only works from a Ship checkout or that pinned versions
    must have published assets.

## Design decisions (rationale, then the rules)

- **Keep push-from-client.** Pull-on-host requires egress + a bootstrap
  channel that doesn't exist on bare SSH hosts; no-agent (Kamal's answer) is
  a rearchitecture. The checksum-verified release download becomes the
  reliable backbone; cross-compile becomes a dev convenience gated to real
  Ship checkouts.
- **Invariant: no strategy early-returns on its own failure.** Each records
  an attempt (strategy, outcome, detail) and falls through; only after all
  strategies are exhausted does `Resolve` return one aggregated error.
- **Version identity from the build, not a hand-bump.** Release builds keep
  ldflags; `go install` builds self-report their true module version via
  `debug.ReadBuildInfo()`; plain `go build` gets a `-dev` sentinel that is
  never used to construct a download URL. This retroactively defuses the
  v0.1.0→"0.4.0" landmine for every consumer without touching the tag.
- **Checksum verification is transfer integrity, not supply-chain security.**
  `checksums.txt` comes from the same host/TLS channel as the tarball, so it
  catches corruption, truncated uploads, and wrong-asset redirects — nothing
  more. Docs and error copy must say exactly that. Signing (minisign, key
  baked into the CLI) is deferred; if added later, minisign over cosign
  (single offline key fits a solo maintainer).
- **Draft-then-publish closes the releases-API window only.** The Go module
  proxy keys off the *git tag*, which necessarily exists before assets under
  a tag-push trigger. What protects the go-install path during the window is
  download retry-with-backoff plus a clear error — treat those as first-class,
  not afterthoughts.
- **No "nearest release" API magic.** Discovering a substitute version via
  `api.github.com` adds a 60-req/hr unauthenticated rate-limit surface and
  silently installs a version the operator didn't ask for. Instead: probe the
  direct CDN asset URL (not rate-limited); on miss, STOP with a `Fix:` line.
- **Error contract:** every resolution/skew failure prints what was attempted
  and why each step failed, and — *for recoverable classes* — exactly one
  runnable command on the final line, prefixed `Fix:`. Integrity failures and
  airgap dead-ends print an explicit STOP with rationale instead of a
  fabricated command.
- **Agent upgrades get the fault tolerance, not the CLI.** `.bak` + health
  check + auto-restore on the *agent* path (whose failure bricks a host).
  `ship self-update`, Homebrew, and update notices are **cut** (see
  "Explicitly out of scope").

## Phase A — resolver correctness (P0)

All in `internal/shipbinary/shipbinary.go` unless noted.

### A1. Gate cross-compile on module identity

Replace `moduleRoot()`:

```go
func moduleRoot() (string, error) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Path}}::{{.Dir}}").Output()
	if err != nil {
		return "", errNoModuleRoot // no go toolchain or not in a module
	}
	path, dir, ok := strings.Cut(strings.TrimSpace(string(out)), "::")
	if !ok || path != modulePath {
		return "", errNoModuleRoot // some OTHER module → treat as no ship root
	}
	return dir, nil
}
```

### A2. Rewrite `Resolve()` with the no-early-return invariant

New order: (0) explicit override — Phase C — highest priority, fails loud;
(1) local platform match (**drop** the `PlatformOfExecutable` shellout for
this case — `os.Executable()` is by definition `runtime.GOOS/GOARCH`, which
removes an unnecessary `go`-toolchain dependency); (2) cross-compile, gated
by A1, failure recorded and falls through; (3) release download, failure
recorded and falls through; (4) return aggregated `*resolveError`.

```go
type attempt struct{ strategy, outcome, detail string }
type resolveError struct {
	target   Platform
	attempts []attempt
}
```

Rendering (the AI-agent-facing contract):

```
ship: could not obtain a linux/amd64 agent binary.

Tried:
  local binary   skipped   this CLI is darwin/arm64; host needs linux/amd64
  cross-compile  skipped   not inside the github.com/watzon/ship source tree
  release v0.4.1 FAILED    no asset ship_0.4.1_linux_amd64.tar.gz (HTTP 404)

Fix: go install github.com/watzon/ship/cmd/ship@v0.4.1
```

`Fix:` lines by failure class: version has no published assets → pin an
asset-backed release (`ship release check vX.Y.Z`, Phase B6, then
`go install …@vX.Y.Z`); dev build → STOP: "dev build — pin a release or use
--agent-binary"; network unreachable → `--agent-release-dir` pointing at
mirrored tarballs; checksum mismatch → STOP, do not retry blindly.

### A3. Version identity via build info

`internal/agent/rpc.go`: default becomes an empty sentinel; add `Version()`:

```go
var AgentVersion = ""        // release ldflags set the real value
const devVersion = "0.5.0-dev" // bump on main after each release

func Version() string {
	if v := strings.TrimSpace(AgentVersion); v != "" {
		return v // release build (ldflags)
	}
	if bi, ok := debug.ReadBuildInfo(); ok && semverTag(bi.Main.Version) {
		return strings.TrimPrefix(bi.Main.Version, "v") // go install @vX.Y.Z
	}
	return devVersion // plain `go build` in a checkout
}
```

Replace every read of the raw `AgentVersion` variable with `Version()`
(callers in `internal/agent/client.go`, `internal/agent/rpc.go`,
`internal/cli/root.go`, `internal/shipbinary/shipbinary.go`).
`downloadRelease` must refuse to build a URL from a `-dev` version (typed
error, not 404).

### A4. Replace curl with net/http; typed download errors

Drop `defaultHTTPGet`/`httpRequest` (`shipbinary.go:210-227`). Use
`http.NewRequestWithContext` + a shared client; cap the response body with
`io.LimitReader(resp.Body, maxAssetBytes)` (e.g. 256 MiB). Typed errors:
`errAssetNotFound` (404), transport error, `errBadChecksum`,
`errWrongPlatform`. Add retry-with-backoff on 404 and transport errors
(3 tries, 2s/6s) to absorb the measured ~3-minute publish→assets window.
Cap per-entry extraction in `extractShipBinary` with an absolute ceiling
independent of the untrusted `header.Size`.

### A5. Verify downloaded bytes

- **Checksum:** fetch `checksums.txt` for the same version, parse only lines
  with exactly two fields (the live v0.4.1 manifest has a bare-filename
  preamble — defect 6 — until Phase B1 fixes generation), find the asset,
  compare `sha256.Sum256(tarball)`. Missing entry or mismatch → hard error,
  fail closed.
- **Platform:** verify the *extracted binary* matches the target with stdlib
  `debug/elf` (linux: `EM_X86_64`/`EM_AARCH64`) and `debug/macho` (darwin:
  `CpuAmd64`/`CpuArm64`). Run on every non-local source (download and Phase C
  overrides). No `go` toolchain required.

### A6. Tests (the gaps that let defect 1 ship)

`internal/shipbinary/shipbinary_test.go` currently only exercises download
with `moduleRootFunc` mocked to `errNoModuleRoot` (line 95) — the
wrong-module path is never tested. Add: cross-compile failure in a foreign
module falls through to download; 404/transport/checksum-mismatch produce
their typed errors and the aggregated message contains the per-step detail
and correct `Fix:` line; `verifyBinaryPlatform` accepts/rejects; `Version()`
precedence (ldflags > build info > dev sentinel); dev version never builds a
URL.

### A7. De-stale the docs (ship with A, they prevent the exact trap)

- `README.md:28` — drop "(or @v0.1.0 once releases are tagged)"; point at the
  current asset-backed tag; state the two rules below.
- `skills/ship/SKILL.md` — replace both `@v0.1.0` pin examples and
  `VERSION=v0.1.0`; add: (1) `ship agent upgrade`/bootstrap cross-compiles
  only from a Ship checkout, otherwise it downloads a release asset, so
  **pin an asset-backed version** (never a bare tag, never `@latest` in CI);
  (2) how to confirm assets exist before pinning (Phase B6 verb).
- `docs/quickstart.md`, `docs/deploy-and-operate.md:72` — promote the
  assets-must-exist note to a visible prerequisite with the consequence
  spelled out.

## Phase B — release process + agent-upgrade safety + skew preflight (P1)

### B1. Fix `checksums.txt` generation

`.github/workflows/release.yml:204-208`: delete the `ls -1 … > checksums.txt`
line; write only `sha256sum ship_*.tar.gz > checksums.txt`. (Parser from A5
stays lenient anyway.)

### B2. Tag-push trigger with draft-then-publish

Trigger on `push: tags: ['v*']`. Publish job: create/reuse the GitHub release
**as a draft**, upload all four tarballs + `checksums.txt`, verify all five
assets are present, then `gh release edit "$TAG" --draft=false`. This makes
*published ⟺ assets exist* for releases-API consumers. **Note honestly:** it
does not protect `go install` — the proxy sees the tag immediately; A4's
retry + A2's `Fix:` line carry that window.

### B3. Version and signing assertions in CI

- In the build job (which already runs `dist/ship version`,
  `release.yml:157`): assert the printed version equals `${VERSION}`; fail
  otherwise. With A3 the source default is empty, so this validates the
  ldflags path specifically.
- Make macOS signing failure **fatal** when signing secrets are configured;
  keep the warn-only path only for forks without secrets
  (`release.yml:108-120`).
- Post-publish smoke job: `go install …@$TAG`, assert `ship version` == tag
  (validates the BuildInfo path); download one linux/amd64 asset and verify
  it against `checksums.txt`.

### B4. Agent upgrade rollback + host-side validation

- Client (`root.go:1388-1397` and the `install_binary` flow at
  `root.go:2013`): before overwrite, keep `ship.bak` beside the target; after
  install, health-check the agent (it already answers `status`); on failure,
  restore `ship.bak` and report.
- Agent (`internal/agent/rpc.go:905-939` `installBinary`): validate the
  incoming bytes are a static ELF (or Mach-O on darwin hosts) matching the
  agent's own `runtime.GOOS/GOARCH`; refuse otherwise. This closes the gap
  the push model structurally opens (client supplies both bytes and hash).
- An agent that predates `install_binary`/`negotiate` returns
  `unknown_method` — treat that as "agent too old → upgrade over SSH
  bootstrap path", not as an RPC failure.

### B5. Deploy/promote skew preflight

In `deploy` (`root.go:2381`) and `promote` (`root.go:2605`), before any
rollout RPC, negotiate per host (or reuse `status`, which returns version +
protocol). Protocol ranges overlap → proceed (info line if agent is older).
No overlap → **hard stop before touching anything**, `Fix: ship agent
upgrade <env>`. Default is warn-and-stop; `--auto-upgrade-agents` opt-in runs
the upgrade inline (CI ergonomics without surprise fleet mutation by
default). Handle `unknown_method` per B4.

### B6. Release hygiene artifacts

- `go.mod`: `retract v0.4.0 // tagged without release assets; use v0.4.1+`
  (the proxy caches deleted tags forever; retraction is the sanctioned
  disavowal). Policy going forward: never delete tags.
- `RELEASING.md`: the one canonical sequence — bump dev sentinel → commit →
  tag → push → workflow drafts/builds/uploads/verifies/publishes → confirm
  smoke job. Include the never-delete-tags rule.
- `ship release check vX.Y.Z`: probes the five direct CDN asset URLs (not the
  rate-limited API) and reports which exist — lets CI gate a pin before
  committing it.

## Phase C — airgap overrides (P2)

Add to `provision apply`, `agent upgrade`, `deploy`:

- `--agent-binary <path>` (or `SHIP_AGENT_BINARY`, `agentBinary:` in
  ship.yml): a prebuilt binary or tarball for the target arch.
- `--agent-release-dir <dir>` (or `SHIP_AGENT_RELEASE_DIR`,
  `agentReleaseDir:`): a local mirror of the release tarballs +
  `checksums.txt`; fully offline.

Both run A5's platform verification; release-dir also checksum-verifies.
A set-but-wrong override **fails loud** — never falls through to a network
attempt. Document mirroring in `docs/airgap.md` (download four tarballs +
manifest once, point the flag at the directory).

Also C (held pending a hosting decision, do not start without maintainer
sign-off): `scripts/install.sh`, pull-style with checksum verification. If
adopted, serve it as a **per-version release asset**, not raw `main` (mutable
URL footgun). Low marginal value while `go install` is primary.

## Explicitly out of scope (evaluated and cut)

- `ship self-update` / update notices — for a go-install-first tool this
  mostly prints `go install …@latest`; the fault-tolerance budget belongs on
  the agent path (B4).
- Homebrew tap — ongoing solo-maintainer tax; revisit on demand.
- "Nearest release" auto-substitution via the GitHub API — rate-limit surface
  plus silent version substitution; replaced by direct-URL probe + STOP.
- `--from-source`/`--no-from-source` toggles — unnecessary once A1 gates by
  module identity.
- Asset signing (minisign) — deferred; when added, note that `checksums.txt`
  is transfer-integrity only.
- Windows operators and 32-bit/exotic arches (`armv7l`, `386`, riscv64) —
  unsupported; state the support matrix (linux/darwin × amd64/arm64)
  explicitly in README instead of implying coverage.

## STOP conditions

- Any code excerpt above no longer matches the live file (drift check).
- `internal/shipbinary` or the release workflow has uncommitted changes.
- A test that passed before your change fails afterward for a reason you
  cannot explain in one sentence.
- You find yourself adding a network call to any path that runs on every
  `Resolve()` (e.g. version discovery) — that is the rate-limit trap this
  plan deliberately avoids; stop and report.

## Execution notes (2026-07-02, executed same day as planning)

All three phases implemented. Deviations from the plan as written:

- **No Makefile** — the release sequence lives in `RELEASING.md` (bump the
  `devVersion` sentinel by hand); a Makefile for one sed-able edit wasn't
  worth the surface.
- **ship.yml `agentBinary:`/`agentReleaseDir:` keys deferred** — flags and
  `SHIP_AGENT_BINARY`/`SHIP_AGENT_RELEASE_DIR` env vars cover the airgap
  need without touching the config schema.
- Post-implementation adversarial review (4 lenses + per-finding refutation)
  confirmed and led to fixes for: `moduleRoot()` misparsing multi-line
  `go list -m` output under go.work workspaces; the post-upgrade version
  check wrongly rolling back `--agent-binary`/`--agent-release-dir` installs
  whose version differs from the CLI; fat Mach-O accepted client-side but
  rejected by the agent; and the release workflow accepting prerelease tags
  it could only fail on after publishing.
- Live-verified against the real v0.4.1 release: download + checksum +
  platform verification (`SHIP_LIVE_DOWNLOAD_TEST=1 go test
  ./internal/shipbinary/ -run TestLiveReleaseDownload`), and
  `ship release check` against both a complete (v0.4.1) and missing (v0.3.0)
  release.

## Verification

- `go test ./...` and `go vet ./...` green.
- New shipbinary tests from A6 all pass; specifically, a test that runs
  resolution from a foreign Go module reaches the download path.
- `gofmt -l .` clean.
- Manual: from a non-Go directory on a darwin/arm64 machine with a release
  build, `ship --dry-run agent upgrade <env>` against a linux/amd64 host
  resolves via download, checksum-verifies, and platform-verifies; with the
  network blocked, the error output lists every attempted strategy and ends
  with a single `Fix:` line.
- After B2/B3 ship: cut the next release via tag push and confirm the smoke
  job passes and `checksums.txt` has no preamble lines.
