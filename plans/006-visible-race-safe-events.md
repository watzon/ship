# Plan 006: Make event writes race-safe and surface persistence failures

> **Executor instructions**: Follow this plan step by step. Run every verification command and confirm the expected result before moving to the next step. If anything in the "STOP conditions" section occurs, stop and report; do not improvise. When done, update this plan's row in `plans/README.md` unless a reviewer told you they maintain the index.
>
> **Drift check (run first)**: `git diff --stat 93da974..HEAD -- internal/state/state.go internal/state/state_test.go internal/cli/root.go internal/cli/root_test.go internal/cli/migrate.go internal/cli/migrate_test.go`
> Stop if event storage changed from one JSON array per environment or if `recordEvent` already returns/reports errors.

## Status

- **Priority**: P2
- **Effort**: S
- **Risk**: LOW
- **Depends on**: none
- **Category**: bug
- **Planned at**: commit `93da974`, 2026-07-16

## Why this matters

Event history underpins `ship events`, recovery output, hooks, notifications, and postmortems. `Store.RecordEvent` performs an unlocked read-modify-write, so concurrent Ship processes can lose events even when both calls return success. The CLI then discards every persistence error. Event logging should serialize independently of operation locks and should warn operators without turning a completed remote mutation into a misleading command failure.

## Current state

- `internal/state/state.go` persists all environment events in `events/<environment>.json` with `fsatomic.WriteFile`.
- The same package already uses file locking for operation locks; reuse that platform assumption instead of adding a dependency.
- `internal/cli/root.go` provides a package-wide `recordEvent` helper used by root and `migrate.go`.
- Repository warning style is `fmt.Fprintf(w, "warning: ...\n")`; see `internal/cli/migrate.go:318-322,505-528`.

Current read-modify-write (`internal/state/state.go:382-412`):

```go
func (s Store) RecordEvent(event Event) error {
	event.Environment = strings.TrimSpace(event.Environment)
	event.Kind = strings.TrimSpace(event.Kind)
	event.Status = strings.TrimSpace(event.Status)
	if err := validateStateName("environment", event.Environment); err != nil {
		return err
	}
	events, err := s.Events(event.Environment)
	if err != nil {
		return err
	}
	events = append(events, event)
	data, err := json.MarshalIndent(events, "", "  ")
	if err != nil {
		return err
	}
	return fsatomic.WriteFile(s.eventsPath(event.Environment), data, 0o644)
}
```

The CLI suppresses the error (`internal/cli/root.go:1457-1463`):

```go
func recordEvent(store state.Store, event state.Event) {
	if event.Time.IsZero() {
		event.Time = deployNow().UTC()
	}
	_ = store.RecordEvent(event)
}
```

Do not make every event failure fatal. After remote mutation, failing the command solely because the audit file is unwritable can cause unsafe retries. The required behavior is a deterministic warning plus preservation of the primary operation result.

## Commands you will need

| Purpose | Command | Expected on success |
|---|---|---|
| State tests | `go test ./internal/state -count=1` | exit 0 |
| CLI event tests | `go test ./internal/cli -run 'Test.*Event' -count=1` | exit 0 |
| Race verification | `go test -race ./internal/state ./internal/cli` | exit 0 |
| Format check | `gofmt -l internal/state/state.go internal/state/state_test.go internal/cli/root.go internal/cli/root_test.go` | no output |
| Full suite | `go test -race ./...` | exit 0 |

## Scope

**In scope**:

- `internal/state/state.go`
- `internal/state/state_test.go`
- `internal/cli/root.go`
- `internal/cli/root_test.go`
- `internal/cli/migrate.go` and `internal/cli/migrate_test.go` only if a warning-writer signature change requires mechanical callsite updates
- `plans/README.md` for status only

**Out of scope**:

- Changing event JSON schema, event ordering, retention, or CLI output formats.
- Making event failure automatically fail deploy, rollback, migrate, or accessory operations.
- Replacing JSON storage with a database or append-only journal.
- Reusing the environment operation lock; `RecordEvent` is called while that lock is already held.
- Logging secret values or adding telemetry.

## Git workflow

- Branch: `advisor/006-visible-race-safe-events`
- Commit message: `fix(state): serialize and report event writes`
- Do not push or open a PR unless instructed.

## Steps

### Step 1: Add concurrency and failure tests first

In `internal/state/state_test.go`, add a test that creates one `Store`, starts at least 32 concurrent `RecordEvent` calls for the same environment, waits for all calls, reads events, and asserts:

- every call returned nil;
- exactly 32 distinct event messages are present;
- JSON remains valid;
- chronological ordering behavior remains deterministic for distinct timestamps.

Use a start barrier so calls overlap. The test must fail reliably without serialization; if the scheduler does not expose the race, add a package-private test hook around the read/write boundary rather than sleeps.

Add a CLI test that points a store at a path which cannot be used as a directory, invokes `recordEvent`, and asserts one warning containing event kind/status and the underlying error without event message payload leakage.

**Verify**: run the new tests before implementation → concurrency or warning test fails for current behavior.

### Step 2: Serialize event read-modify-write with a dedicated file lock

Add a private event lock path adjacent to the event file, for example `events/<environment>.lock`. In `RecordEvent`:

1. Validate the environment before constructing the path.
2. Ensure the events directory exists with existing state-directory conventions.
3. Open/create the lock file with least-privilege mode.
4. Acquire an exclusive file lock.
5. Perform the existing read, append, sort, marshal, and atomic write while holding the lock.
6. Always unlock and close; preserve the primary read/write error and append unlock/close errors only when they are the sole failure.

Do not call `AcquireOperationLock`, because commands already hold it and event recording must not deadlock. Keep `Events` read-only and lock-free unless tests prove readers can observe an invalid intermediate state; atomic rename already protects readers.

**Verify**: `go test -race ./internal/state -count=1` → exit 0; concurrency test passes.

### Step 3: Report CLI event failures exactly once

Change the CLI helper so a failed `Store.RecordEvent` prints one warning. Prefer an injectable package-level warning sink matching existing test-hook conventions in `internal/cli/root.go`, with default behavior writing to `os.Stderr`. Do not change every caller to handle an error unless that is clearly smaller and keeps output routing testable.

Warning requirements:

- prefix `warning:`;
- include environment, event kind, and status;
- include the storage error;
- do not include arbitrary `event.Message`, because it may contain command output or sensitive operational details;
- do not change the caller's returned error or success.

Tests must restore the injected sink with `t.Cleanup`.

**Verify**: `go test ./internal/cli -run 'Test.*Event' -count=1` → exit 0.

### Step 4: Protect event ordering and corruption behavior

Add state tests proving:

- an existing corrupt JSON file returns an error and is not overwritten;
- two different environments do not block or mix events;
- equal timestamps retain stable insertion order under serialized writes;
- lock acquisition failure is returned;
- event values are never printed by the CLI warning sink.

Do not add automatic repair or truncation.

**Verify**: `go test -race ./internal/state ./internal/cli -count=1` → exit 0.

### Step 5: Run all gates

Format changed files, run focused race tests, then full vet and race suite.

**Verify**: `gofmt -l internal/state/state.go internal/state/state_test.go internal/cli/root.go internal/cli/root_test.go internal/cli/migrate.go internal/cli/migrate_test.go` → no output; `go vet ./...` → exit 0; `go test -race ./...` → exit 0.

## Test plan

- Concurrent same-environment writes preserve every distinct event.
- Different environments remain isolated.
- Corrupt event JSON is not overwritten.
- Lock errors propagate from `Store.RecordEvent`.
- CLI warns once, names the event metadata, omits event message content, and preserves the primary command result.
- Existing event listing/history tests continue to pass.

Model state persistence tests after existing `Store.RecordEvent`/`Events` tests in `internal/state/state_test.go`. Model CLI output capture after existing command tests in `internal/cli/root_test.go`.

## Done criteria

- [ ] Concurrent same-environment `RecordEvent` calls cannot lose successful writes.
- [ ] Event locking is independent from operation locking and cannot self-deadlock.
- [ ] Corrupt existing history is preserved and reported.
- [ ] Every CLI event persistence failure produces one warning without changing operation success/failure.
- [ ] Warnings omit `event.Message` content.
- [ ] Event schema and `ship events` output remain unchanged.
- [ ] `gofmt -l` prints nothing for changed files.
- [ ] `go vet ./...` exits 0.
- [ ] `go test -race ./...` exits 0.
- [ ] No source files outside the in-scope list changed.
- [ ] `plans/README.md` status row is updated.

## STOP conditions

Stop and report if:

- File locking is unavailable on either supported release target (Linux and macOS).
- `RecordEvent` is called while already holding an event-specific lock.
- A deterministic concurrency test requires production sleeps or timing assumptions.
- Warning output would expose event messages or secret-bearing command output.
- Fixing event corruption requires silently discarding existing history.
- More than mechanical signature changes are required in `migrate.go`.

## Maintenance notes

- Any future event compaction/retention must run under the same event lock.
- Reviewers should verify lock ordering: operation lock may be acquired before event lock, but no path may acquire them in reverse.
- Event warnings are deliberately nonfatal; commands must remain safe to retry based on their primary result, not audit-file availability.