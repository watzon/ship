# Plan 001: Add formatting, vet, race, and vulnerability gates to CI

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md` — unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**: `git diff --stat 29fc466..HEAD -- .github/workflows/ci.yml internal/cli/ internal/shipbinary/`
> Note: at planning time the working tree already contained uncommitted
> changes (including `.github/workflows/ci.yml` and the new
> `internal/shipbinary/` package). The "Current state" excerpts below reflect
> the on-disk working tree, not the last commit. If the live code no longer
> matches the excerpts, treat it as a STOP condition.

## Status

- **Priority**: P1
- **Effort**: S
- **Risk**: LOW
- **Depends on**: none
- **Category**: dx
- **Planned at**: commit `29fc466` (dirty working tree), 2026-07-01

## Why this matters

CI currently runs only `go test ./...` and a build smoke test. Nothing blocks
vet-level bugs, data races, formatting drift, or known-vulnerable dependency
versions from landing on `main`. The drift is not hypothetical: `gofmt -l .`
already flags 11 files today, and `golang.org/x/crypto` (which backs the
secrets encryption via `filippo.io/age`) sits 8 minor releases behind with no
automated check that would notice a published vulnerability. Separately,
another planned change will introduce the repo's first goroutines
(parallelizing per-host deploys), and that work is only safe to trust if
`go test -race` is already a CI gate — this plan is its prerequisite.

## Current state

- `.github/workflows/ci.yml` — the only CI workflow. One `test` job:

  ```yaml
  # .github/workflows/ci.yml:25-37 (working-tree state)
        - name: Verify module
          run: go mod verify

        - name: Install Caddy
          run: |
            sudo apt-get update
            sudo apt-get install -y caddy

        - name: Test
          run: go test ./...

        - name: Build smoke test
          run: go build -o /tmp/ship ./cmd/ship
  ```

- `gofmt -l .` output at planning time (11 files, all needing `gofmt -w`):

  ```
  internal/cli/root.go
  internal/cli/ui/args.go
  internal/cli/ui/color.go
  internal/cli/ui/error.go
  internal/cli/ui/help.go
  internal/cli/ui/render.go
  internal/cli/ui/table.go
  internal/cli/ui/terminal.go
  internal/cli/ui/ui_test.go
  internal/shipbinary/shipbinary.go
  internal/shipbinary/shipbinary_test.go
  ```

  Re-run `gofmt -l .` yourself and format whatever it lists at execution time —
  the exact set may have shifted.

- There is no lint config (`.golangci.yml` does not exist) and no Makefile.
  `go vet ./...` passes cleanly today. `go test -race` has never been run in CI.
- Repo conventions: `AGENTS.md` ("Coding Style") mandates gofmt-formatted Go.
  Commit style from `git log`: concise imperative, optionally Conventional
  Commits (e.g. `feat(cli): add structured tables, grouped help, and clearer errors`).

## Commands you will need

| Purpose        | Command                          | Expected on success            |
|----------------|----------------------------------|--------------------------------|
| Format check   | `gofmt -l .`                     | empty output                   |
| Vet            | `go vet ./...`                   | exit 0, no output              |
| Tests (race)   | `go test -race ./...`            | all packages `ok`              |
| Build          | `go build ./cmd/ship`            | exit 0                         |
| Vuln scan      | `go run golang.org/x/vuln/cmd/govulncheck@latest ./...` | exit 0 (or reported findings — see Step 4) |

## Scope

**In scope** (the only files you should modify):
- `.github/workflows/ci.yml`
- The files listed by `gofmt -l .` (formatting-only changes via `gofmt -w`)

**Out of scope** (do NOT touch, even though they look related):
- `.github/workflows/release.yml` — release pipeline, not a quality gate.
- Adding golangci-lint or any third-party linter — a larger tooling decision
  for the maintainer; this plan is stdlib-toolchain gates only.
- Any non-whitespace code change to the gofmt'd files. If `gofmt -w` produces
  anything beyond whitespace/alignment changes, STOP.
- `go.mod` / `go.sum` — do not bump dependencies here (govulncheck runs via
  `go run`, which does not modify the module files).

## Git workflow

- Branch: `advisor/001-ci-quality-gates`
- Two commits: `style: gofmt all Go files` (formatting only), then
  `ci: add gofmt, vet, race, and govulncheck gates`.
- Do NOT push or open a PR unless the operator instructed it.

## Steps

### Step 1: Format the drifted files

Run `gofmt -l .` to get the current list, then `gofmt -w` each listed file.

**Verify**: `gofmt -l .` → empty output. `git diff --stat` → only the files
that were listed, whitespace-level changes. `go build ./cmd/ship` → exit 0.

### Step 2: Add format and vet steps to CI

In `.github/workflows/ci.yml`, after the `Verify module` step and before
`Install Caddy`, add:

```yaml
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

**Verify**: locally run the same two commands (`gofmt -l .` empty,
`go vet ./...` exit 0).

### Step 3: Enable the race detector in the test step

Change the `Test` step's run line from `go test ./...` to
`go test -race ./...`.

**Verify**: `go test -race ./...` locally → all packages `ok` (expect roughly
2× the normal test wall time; the suite is hermetic so no external services
are needed).

### Step 4: Add a govulncheck step

After the `Test` step, add:

```yaml
      - name: Vulnerability scan
        run: go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

**Verify**: run `go run golang.org/x/vuln/cmd/govulncheck@latest ./...`
locally. If it exits 0, done. If it reports findings that are *called* from
this codebase, record the exact output in your final report — do NOT fix
dependency versions in this plan (see STOP conditions).

## Test plan

No new Go tests — this plan adds CI gates. The verification is that all four
gate commands pass locally on the branch:

- `gofmt -l .` → empty
- `go vet ./...` → exit 0
- `go test -race ./...` → all pass
- `go build ./cmd/ship` → exit 0

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `gofmt -l .` prints nothing
- [ ] `.github/workflows/ci.yml` contains steps running `gofmt -l`, `go vet ./...`, `go test -race ./...`, and `govulncheck`
- [ ] `go test -race ./...` exits 0
- [ ] `git diff 29fc466..HEAD --stat` (plus `git status`) shows changes only in `.github/workflows/ci.yml` and gofmt-listed files
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back (do not improvise) if:

- `go test -race ./...` fails — that means a real pre-existing race or a test
  that can't run under the race detector. Report the failing package and
  output; do not "fix" the test to make it pass.
- `govulncheck` reports a reachable (called) vulnerability. Report it; the
  dependency bump belongs to the maintainer (likely a one-line
  `go get golang.org/x/crypto@latest && go mod tidy`, but they must decide).
- `gofmt -w` produces non-whitespace diffs in any file.
- `.github/workflows/ci.yml` on disk no longer matches the excerpt above.

## Maintenance notes

- The `-race` gate exists partly to de-risk a future plan that parallelizes
  per-host deploy execution (the repo currently has zero goroutines outside
  tests). Keep it even if CI time increases.
- Follow-up explicitly deferred: adopting golangci-lint (staticcheck/errcheck)
  and pinning govulncheck to a version instead of `@latest` — maintainer
  decisions, not taken here.
- `golang.org/x/crypto` is at v0.45.0 (v0.53.0 available) and sits on the
  age-encryption path; the govulncheck gate is the systematic answer, but a
  routine `go get golang.org/x/crypto@latest` bump is cheap and worth doing
  whenever convenient.
