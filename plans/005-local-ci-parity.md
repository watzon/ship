# Plan 005: Make one local command execute the same gates as CI

> **Executor instructions**: Follow this plan step by step. Run every verification command and confirm the expected result before moving to the next step. If anything in the "STOP conditions" section occurs, stop and report; do not improvise. When done, update this plan's row in `plans/README.md` unless a reviewer told you they maintain the index.
>
> **Drift check (run first)**: `git diff --stat 93da974..HEAD -- .github/workflows/ci.yml .github/workflows/release.yml docs/development.md AGENTS.md skills/ship/SKILL.md scripts`
> If CI gates or release-only environment setup changed, stop and reconcile the command list before editing.

## Status

- **Priority**: P1
- **Effort**: S
- **Risk**: LOW
- **Depends on**: none
- **Category**: dx
- **Planned at**: commit `93da974`, 2026-07-16

## Why this matters

CI and release workflows enforce module verification, formatting, vet, race tests, vulnerability scanning, and a build smoke test. Contributor and agent guidance only says to run ordinary tests and build, so a locally “green” change can still fail required gates after push. A checked-in script used by both workflows and documentation makes the gate set executable and prevents future drift.

## Current state

- `.github/workflows/ci.yml` is the authoritative default pipeline.
- `.github/workflows/release.yml` duplicates the same test gates and sets `SHIP_DOCS_VERSION_TAG` for release documentation checks.
- `docs/development.md`, repository `AGENTS.md`, and `skills/ship/SKILL.md` list a reduced command set.
- Existing shell scripts use Bash with `set -euo pipefail`; match `scripts/sign-macos-binary.sh` style.

CI gates (`.github/workflows/ci.yml:27-53`):

```yaml
      - name: Verify module
        run: go mod verify

      - name: Check formatting
        run: |
          UNFORMATTED="$(gofmt -l .)"
          if [ -n "$UNFORMATTED" ]; then
            echo "gofmt needed on:" && echo "$UNFORMATTED"
            exit 1
          fi

      - name: Vet
        run: go vet ./...
```

The remaining required commands are `go test -race ./...`, `go run golang.org/x/vuln/cmd/govulncheck@latest ./...`, and `go build -o /tmp/ship ./cmd/ship`.

Local docs are incomplete (`docs/development.md:4-9`):

````markdown
Default CI-safe coverage:

```bash
go test ./...
go build ./cmd/ship
```
````

The agent skill repeats the reduced set (`skills/ship/SKILL.md:322`):

```markdown
When editing Ship itself (not deploying an app), run `go test ./...` and `go build ./cmd/ship`.
```

## Commands you will need

| Purpose | Command | Expected on success |
|---|---|---|
| Shell syntax | `bash -n scripts/ci-local.sh` | exit 0 |
| Local CI | `./scripts/ci-local.sh` | exit 0; all six gates pass |
| Workflow-focused tests | `go test ./... -run TestPinnedReleaseDocsUseCurrentOrNextReleaseTag -count=1` | exit 0 or documented skip when no tag context exists |
| Workflow YAML inspection | `git diff --check` | exit 0; no whitespace errors |

## Scope

**In scope**:

- `scripts/ci-local.sh` (create, executable)
- `.github/workflows/ci.yml`
- `.github/workflows/release.yml`
- `docs/development.md`
- `AGENTS.md`
- `skills/ship/SKILL.md`
- `plans/README.md` for status only

**Out of scope**:

- Installing Go, Caddy, Docker, or govulncheck globally.
- Optional live Hetzner and destructive gates.
- Optional local registry integration.
- Changing test behavior, Go versions, action versions, release build matrices, signing, or publish jobs.
- Adding Make, Task, golangci-lint, pre-commit, or a new dependency.
- Pinning a govulncheck tool version; keep exact CI semantics in this plan.

## Git workflow

- Branch: `advisor/005-local-ci-parity`
- Commit message: `chore(ci): share local verification gates`
- Ensure `scripts/ci-local.sh` is executable in Git.
- Do not push or open a PR unless instructed.

## Steps

### Step 1: Create the single-source script

Create `scripts/ci-local.sh` with Bash, `set -euo pipefail`, and repository-root resolution based on the script path so it works from any current directory.

Run these gates in the existing CI order:

1. `go mod verify`
2. collect `gofmt -l .`; print the same failing file list and exit nonzero when not empty
3. `go vet ./...`
4. `go test -race ./...`
5. `go run golang.org/x/vuln/cmd/govulncheck@latest ./...`
6. `go build` to a unique temporary path

Use `mktemp` for the build output and a trap that removes it on success, failure, or interruption. Do not write `./ship`, `dist/`, or any repository artifact. Preserve inherited environment variables, especially `SHIP_DOCS_VERSION_TAG`.

Print a short gate label before each command so local failures identify the stage.

**Verify**: `bash -n scripts/ci-local.sh` → exit 0; `test -x scripts/ci-local.sh` → exit 0.

### Step 2: Make CI call the script

In `.github/workflows/ci.yml`, keep checkout, setup-go, and Caddy installation. Replace the duplicated module/format/vet/test/vulnerability/build steps with one named step that runs `./scripts/ci-local.sh`.

Do not change permissions, triggers, runner, Go cache, or Caddy installation.

**Verify**: inspect `git diff -- .github/workflows/ci.yml` → only the duplicated gate steps are replaced.

### Step 3: Make release testing call the same script

In the release workflow's `test` job, keep checkout, setup-go, and Caddy installation. Replace duplicated gates with the same script invocation while retaining this environment value on that step:

```yaml
env:
  SHIP_DOCS_VERSION_TAG: ${{ github.event_name == 'workflow_dispatch' && github.event.inputs.tag || github.ref_name }}
```

Do not alter the release `build` or `publish` jobs; their matrix-specific smoke checks remain separate.

**Verify**: `git diff -- .github/workflows/release.yml` → only the test job's duplicated gates change; build/publish are untouched.

### Step 4: Point all contributor guidance at the script

Update:

- `docs/development.md`: make `./scripts/ci-local.sh` the default full local gate; retain `go test ./...` and `go build ./cmd/ship` as faster inner-loop commands with explicit wording.
- `AGENTS.md`: list the script as the pre-PR/full verification command and preserve optional integration/live gate warnings.
- `skills/ship/SKILL.md`: replace the closing sentence with the script for full verification and mention ordinary tests only as an inner loop.

Do not duplicate the six-command implementation in all docs; the script is the source of truth.

**Verify**: `grep -n 'scripts/ci-local.sh' docs/development.md AGENTS.md skills/ship/SKILL.md .github/workflows/ci.yml .github/workflows/release.yml` → at least one intended match in every file.

### Step 5: Run the script from outside the repository root

Run once from the repository root and once from `docs/` using `../scripts/ci-local.sh`. Both must resolve the module root and produce the same successful gates.

If Caddy is absent, the existing Caddy integration test may skip; document that optional behavior without making the script install packages.

**Verify**: `./scripts/ci-local.sh` → exit 0; `(cd docs && ../scripts/ci-local.sh)` → exit 0.

### Step 6: Check workflow and repository cleanliness

Confirm no binary or temporary file remains in the worktree, the script has executable mode, and only scoped files changed.

**Verify**: `git diff --check` → exit 0; `git status --short` contains only the in-scope files and `plans/README.md`.

## Test plan

This is tooling work; the primary behavior check is executing the actual script.

Required checks:

- Bash syntax succeeds.
- Script works from root and a subdirectory.
- Formatting failure path can be tested in a disposable worktree or by passing a deliberately unformatted temporary `.go` file, then removing it; do not leave the file committed.
- Temporary build output is removed on success and failure.
- Release docs test still sees `SHIP_DOCS_VERSION_TAG` when supplied.
- Existing `go test -race ./...` remains part of the script and passes.

## Done criteria

- [ ] `scripts/ci-local.sh` runs all six current CI gates in order.
- [ ] Build output is temporary and always cleaned.
- [ ] CI and release test jobs invoke the script.
- [ ] Release test invocation preserves `SHIP_DOCS_VERSION_TAG`.
- [ ] Contributor and agent docs point to the script as full verification.
- [ ] Optional destructive/live tests are not added to the script.
- [ ] `bash -n scripts/ci-local.sh` exits 0.
- [ ] Running the script from root and `docs/` exits 0.
- [ ] `git diff --check` exits 0.
- [ ] No files outside the in-scope list changed.
- [ ] `plans/README.md` status row is updated.

## STOP conditions

Stop and report if:

- CI or release gates differ from commit `93da974` beyond release-only environment setup.
- A required gate needs credentials, live infrastructure, or destructive access.
- The release test job cannot preserve `SHIP_DOCS_VERSION_TAG` through the shared script.
- Running from a subdirectory would require changing module-aware Go commands.
- The script leaves any binary or cache file inside the repository.

## Maintenance notes

- Future required gates must be added to `scripts/ci-local.sh`; workflows and docs should continue invoking the script rather than duplicating commands.
- Reviewers should compare script order against both workflows and verify release environment propagation.
- The script intentionally does not install Caddy or run opt-in integrations.