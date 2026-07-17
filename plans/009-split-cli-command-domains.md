# Plan 009: Split the CLI control plane into command-domain files without behavior changes

> **Executor instructions**: Follow this plan step by step. Run every verification command and confirm the expected result before moving to the next step. If anything in the "STOP conditions" section occurs, stop and report; do not improvise. This is a move-only refactor. When done, update this plan's row in `plans/README.md` unless a reviewer told you they maintain the index.
>
> **Drift check (run first)**: `git diff --stat 93da974..HEAD -- internal/cli`
> Plans 002, 004, 006, and 007 are expected to change CLI code/tests. Rebuild the symbol inventory from the live files after those plans land. Stop if another CLI package split or command-framework migration occurred.

## Status

- **Priority**: P2
- **Effort**: L
- **Risk**: MED
- **Depends on**: `plans/005-local-ci-parity.md`, `plans/006-visible-race-safe-events.md`, `plans/007-migrate-failure-contracts.md`
- **Category**: tech-debt
- **Planned at**: commit `93da974`, 2026-07-16

## Why this matters

`internal/cli/root.go` is 7,410 lines and `root_test.go` is 7,248 lines. Provision, deploy, rollback, observability, accessories, secrets, hooks, schedules, rendering, and shared remote helpers all collide in the same files. The existing `migrate.go` extraction proves the package can be split without changing APIs. This plan creates reviewable ownership seams while preserving the `cli` package, symbol names, command output, test hooks, and behavior.

## Current state

- `internal/cli/root.go` contains `options`, injectable factories, `Execute`, nearly every Cobra constructor, command execution, rendering, and shared helpers.
- `internal/cli/root_test.go` contains more than 100 tests plus all fakes.
- `internal/cli/migrate.go` and `migrate_test.go` are the exemplar: same package, focused domain file, package-private shared helpers remain accessible.
- Do not create subpackages. A package split would create import cycles and force exported seams; file boundaries are sufficient.

Root wiring is already centralized (`internal/cli/root.go:158-244`):

```go
func Execute() error {
	opts := &options{}
	root := &cobra.Command{
		Use:           "ship",
		Short:         "Deploy Docker apps to ordinary servers with horizontal scaling",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVarP(&opts.configPath, "config", "c", config.DefaultConfigFile, "path to ship.yml")
	root.PersistentFlags().BoolVar(&opts.dryRun, "dry-run", false, "print the intended operation without mutating remote state")
```

A focused file already follows the intended convention (`internal/cli/migrate.go:24-30`):

```go
var copyRemoteArtifact = func(ctx context.Context, source scheduler.Host, readCommand string, dst scheduler.Host, writeCommand string, dryRun bool) error {
	return sshForHost(source, dryRun).CopyTo(ctx, readCommand, sshForHost(dst, dryRun), writeCommand)
}

var uploadLocalArtifact = func(ctx context.Context, dst scheduler.Host, localPath, writeCommand string, dryRun bool) error {
	return sshForHost(dst, dryRun).CopyFromLocal(ctx, localPath, writeCommand)
}
```

Key test style (`internal/cli/root_test.go:63-88`): tests install package-level hooks and restore them with `t.Cleanup`. Keep shared fakes package-private and move them only when ownership is unambiguous.

## Commands you will need

| Purpose | Command | Expected on success |
|---|---|---|
| Symbol inventory | `go doc github.com/watzon/ship/internal/cli` | exit 0 |
| Focused CLI suite | `go test ./internal/cli -count=1` | exit 0 |
| CLI race suite | `go test -race ./internal/cli -count=1` | exit 0 |
| Full local CI | `./scripts/ci-local.sh` | exit 0; all gates pass |
| Format check | `gofmt -l internal/cli` | no output |

## Scope

**In scope**:

- `internal/cli/root.go`
- `internal/cli/root_test.go`
- Create focused files in `internal/cli/`:
  - `shared.go`, `shared_test.go`
  - `config_commands.go`, `config_commands_test.go`
  - `provision_commands.go`, `provision_commands_test.go`
  - `deploy_commands.go`, `deploy_commands_test.go`
  - `observe_commands.go`, `observe_commands_test.go`
  - `recovery_commands.go`, `recovery_commands_test.go`
  - `accessory_commands.go`, `accessory_commands_test.go`
  - `secrets_commands.go`, `secrets_commands_test.go`
  - `hooks_notifications.go`, `hooks_notifications_test.go`
  - `schedules.go`, `schedules_test.go`
- `plans/README.md` for status only

**Out of scope**:

- `internal/cli/migrate.go` and `migrate_test.go`, except gofmt caused by shared declarations only if unavoidable.
- New packages or exported identifiers.
- Renaming command constructors, flags, helper functions, test names, event kinds, output text, or JSON fields.
- Deduplicating helpers, changing interfaces, replacing globals, changing Cobra wiring, or opportunistic cleanup.
- Source files outside `internal/cli`.
- Combining tests or replacing explicit fakes with a framework.

## Git workflow

- Branch: `advisor/009-split-cli-command-domains`
- Commit message: `refactor(cli): split command domains`
- Use one commit per extraction group if desired, but every commit must pass `go test ./internal/cli`.
- Prefer `git mv` only when a whole file moves; for declaration moves, preserve content exactly and let gofmt reorder imports.
- Do not push or open a PR unless instructed.

## Steps

### Step 1: Create a live symbol ownership inventory

Before moving code, list every top-level declaration in `root.go` and every test/helper in `root_test.go`. Assign each declaration to exactly one destination file.

Use these ownership rules:

- `root.go`: only `Execute`, root Cobra setup/group registration, and the `options` type.
- `shared.go`: SSH construction, host resolution/facts, local state/config context, agent factories, version/binary bootstrap helpers, common rendering utilities used by three or more domains.
- `config_commands.go`: init, config rendering, doctor, hosts, version, and agent install/upgrade command construction.
- `provision_commands.go`: provision plan/apply/decommission and provider host rendering.
- `deploy_commands.go`: plan, deploy, promote, scale preview, image preparation, registry auth, release commands, release-state sync, locks, and restart helpers directly coupled to deployment.
- `observe_commands.go`: status, ps, health, logs, exec, inspect, support, events, releases, maintenance, and prune.
- `recovery_commands.go`: rollback, recover, rollback blockers, and recovery rendering.
- `accessory_commands.go`: accessory command tree, deploy/status/backup/logs/exec/restore/failover, topology checks, and accessory-specific fakes/tests.
- `secrets_commands.go`: secrets command tree, scoped rendering, and remote secret file helpers.
- `hooks_notifications.go`: hook context, local hook execution, webhook delivery, and notification helpers.
- `schedules.go`: managed cron detection, rendering, and sync helpers.

If a declaration fits two files, place it with the command that owns its mutation; shared placement is only for three-or-more-domain use.

**Verify**: every declaration has one destination and no planned symbol rename.

### Step 2: Extract shared production declarations first

Create `shared.go` and move the shared types, factories, and helpers identified in Step 1 without changing bodies. Leave `Execute` and command constructors in `root.go` temporarily.

Run gofmt on `root.go` and `shared.go` to normalize imports. Do not reorder declarations for style beyond what file ownership requires.

**Verify**: `go test ./internal/cli -count=1` → exit 0; `go vet ./internal/cli` → exit 0.

### Step 3: Extract one production command domain at a time

Move production declarations in this order to minimize dependencies:

1. hooks/notifications;
2. schedules;
3. config commands;
4. provision commands;
5. secrets commands;
6. accessory commands;
7. observe commands;
8. recovery commands;
9. deploy commands.

After each file:

- gofmt the two affected files;
- run `go test ./internal/cli -count=1`;
- confirm `Execute` still references the same constructors and group IDs;
- confirm no duplicate or missing symbol.

Do not wait until all moves are complete to test.

**Verify**: nine consecutive focused test runs exit 0; final `root.go` contains only root wiring and `options`.

### Step 4: Split tests by the production owner

Move each `Test*` function to the matching `*_test.go` file without renaming it or changing assertions. Move a fake/helper with the narrowest domain that uses it. Put only broadly reused hook installers, config fixtures, and fake agent primitives in `shared_test.go`.

Rules:

- Keep `package cli`, not `cli_test`.
- Preserve `t.Cleanup` restoration for package globals.
- Do not combine fixtures merely because they look similar.
- Do not change test execution order assumptions; tests must already be isolated.
- Keep migration tests in `migrate_test.go`.

After each test file move, run the focused package suite.

**Verify**: `go test ./internal/cli -count=1` → exit 0 after every test-domain extraction.

### Step 5: Check file boundaries and residual size

At the end:

- `root.go` should be a small root constructor/wiring file, not a new shared dumping ground.
- `root_test.go` should contain only tests directly for root setup/help/error wiring, or be removed if empty.
- No new file should exceed roughly 2,000 lines without a written reason in the PR notes.
- No identifier should be exported solely to cross a file boundary.

Use `wc -l internal/cli/*.go` to report sizes; this command is evidence, not a target that justifies behavior changes.

**Verify**: `go test -race ./internal/cli -count=1` → exit 0.

### Step 6: Run full verification

Run the shared local CI script from Plan 005. Inspect the final diff for move-only behavior: function bodies and tests should be unchanged except imports and file placement.

**Verify**: `gofmt -l internal/cli` → no output; `./scripts/ci-local.sh` → exit 0; `git diff --check` → exit 0.

## Test plan

No new behavior tests are required. Existing tests are the safety net.

Verification requirements:

- `go test ./internal/cli` after every extraction;
- final `go test -race ./internal/cli`;
- final full local CI script;
- compare command help snapshot/output tests before and after;
- ensure all pre-existing `Test*` names still exist exactly once;
- ensure migrate failure tests from Plan 007 remain in `migrate_test.go` and pass.

Do not add source-layout tests; file ownership is a maintainer convention, not runtime behavior.

## Done criteria

- [ ] `root.go` contains only root wiring and root-owned options.
- [ ] Every moved production declaration retains its name and body behavior.
- [ ] Every pre-existing test name exists exactly once with unchanged assertions.
- [ ] No new package or exported API is introduced.
- [ ] `migrate.go` remains its focused domain file.
- [ ] No domain file becomes an undocumented replacement god file.
- [ ] `gofmt -l internal/cli` prints nothing.
- [ ] `go test -race ./internal/cli -count=1` exits 0.
- [ ] `./scripts/ci-local.sh` exits 0.
- [ ] No source files outside `internal/cli` changed.
- [ ] `plans/README.md` status row is updated.

## STOP conditions

Stop and report if:

- Any extraction requires renaming/exporting a symbol or changing an interface.
- A test depends on undeclared test order or shared mutable state exposed by moving files.
- Plans 002, 004, 006, or 007 have not landed and would cause large overlapping edits.
- `root.go` changed behavior after a move-only step.
- A file boundary creates an import cycle; files in one package should not require new imports between each other.
- A failing test tempts an unrelated behavior fix.

## Maintenance notes

- New commands should be added to the narrowest existing domain file and wired from `root.go`.
- Shared helpers need three-domain use; otherwise keep them with the owning command.
- Reviewers should use move-aware diff viewing and reject unrelated cleanup hidden in the large refactor.
- Plan 010 must rebase its CLI callsites onto these final files rather than recreating a large shared module.