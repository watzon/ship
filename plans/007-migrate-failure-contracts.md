# Plan 007: Characterize host migration failures at every irreversible boundary

> **Executor instructions**: Follow this plan step by step. Run every verification command and confirm the expected result before moving to the next step. If anything in the "STOP conditions" section occurs, stop and report; do not improvise. This plan is test-first and behavior-preserving. When done, update this plan's row in `plans/README.md` unless a reviewer told you they maintain the index.
>
> **Drift check (run first)**: `git diff --stat 93da974..HEAD -- internal/cli/migrate.go internal/cli/migrate_test.go internal/cli/acceptance_test.go`
> Plan 006 may mechanically affect event warning hooks. Stop if migration stage order, cleanup hints, host-fact repointing, or old-server deletion changed.

## Status

- **Priority**: P1
- **Effort**: M
- **Risk**: MED
- **Depends on**: `plans/006-visible-race-safe-events.md`
- **Category**: tests
- **Planned at**: commit `93da974`, 2026-07-16

## Why this matters

`ship migrate` creates a replacement server, bootstraps it, moves accessories, repoints the logical host fact, rolls services, stops old workloads, and deletes the old server. The happy path is covered, but no test injects failure after a mutation. Without explicit residual-state contracts, a refactor can leak a replacement, repoint facts too early, move an accessory without clear recovery, or delete the old server after incomplete convergence.

## Current state

- `internal/cli/migrate.go` is already separated from the CLI god file and exposes test hooks for binary resolution and remote copy.
- `internal/cli/migrate_test.go` covers flags, validation, dry-run text, artifact parsing, and successful service/accessory acceptance workflows.
- `internal/cli/acceptance_test.go` contains reusable fake Hetzner, Docker, and agent infrastructure; reuse it rather than contacting live systems.
- This plan adds characterization tests. If a test proves current behavior violates the safety contract stated below, stop and report the product defect instead of changing production behavior under a tests-only plan.

The failure helper changes its recovery note after host-fact repoint (`internal/cli/migrate.go:117-125,133-170`):

```go
recordEvent(store, state.Event{Environment: envName, Kind: "migrate", Status: "started", Host: hostName})
cleanupHint := ""
fail := func(err error) error {
	if cleanupHint != "" {
		fmt.Fprintln(w, cleanupHint)
	}
	recordEvent(store, state.Event{Environment: envName, Kind: "migrate", Status: "failed", Host: hostName, Message: err.Error()})
	runNotifications(ctx, store, cfg, envName, "migrate", "failed", "", err.Error(), nil)
	return err
}
```

Irreversible stage order (`internal/cli/migrate.go:160-195`):

```go
for _, name := range plan.accessories {
	if err := migrateAccessory(ctx, w, opts, cfg, env, envName, store, name, plan.source, replacement, overrides[name]); err != nil {
		return fail(err)
	}
}

oldFact, err := repointHostFact(store, envName, plan.source, prov.Name(), created)
if err != nil {
	return fail(err)
}
cleanupHint = fmt.Sprintf("note: host %s now points at %s; the old server (%s) is still running — after fixing the cause, run `ship deploy %s` to converge, then delete the old server with your provider", hostName, created.PublicAddress, plan.source.ContactTarget(), envName)
```

Old workload stop and provider deletion happen only after rollout/restart (`internal/cli/migrate.go:178-195`):

```go
if hasRelease && len(plan.services) > 0 {
	if err := migrateServiceRollout(ctx, opts, cfg, env, store, envName, stateDir, hostsAfter, replacement, current); err != nil {
		return fail(err)
	}
}
stopOldWorkloads(ctx, w, cfg, envName, plan.source)
if keepServer {
	fmt.Fprintf(w, "keeping old server %s (%s); `ship provision apply` will report it as extra until you delete it with your provider\n", oldServerLabel(oldFact, hostName), plan.source.ContactTarget())
} else if err := deleteOldServer(ctx, w, prov, cfg.Project, envName, oldFact, hostName, created.ID); err != nil {
	return fail(err)
}
```

## Commands you will need

| Purpose | Command | Expected on success |
|---|---|---|
| Migrate tests | `go test ./internal/cli -run 'Test.*Migrate' -count=1` | exit 0 |
| Migrate race tests | `go test -race ./internal/cli -run 'Test.*Migrate' -count=1` | exit 0 |
| Full CLI tests | `go test ./internal/cli -count=1` | exit 0 |
| Format check | `gofmt -l internal/cli/migrate_test.go internal/cli/acceptance_test.go internal/cli/migrate.go` | no output |
| Full suite | `go test -race ./...` | exit 0 |

## Scope

**In scope**:

- `internal/cli/migrate_test.go`
- `internal/cli/acceptance_test.go` only for reusable fake failure injection
- `internal/cli/migrate.go` only for package-private injectable function variables required to observe a boundary; no behavior changes
- `plans/README.md` for status only

**Out of scope**:

- Automatic rollback or cleanup behavior.
- Reordering migration stages.
- Changing cleanup hints, event schema, notifications, or CLI output except to expose an existing seam to tests.
- Live provider, Docker, SSH, or registry access.
- Service volume migration; Plan 013 covers that direction.
- Accessory stale-placement recovery; Plan 012 covers that direction.

## Git workflow

- Branch: `advisor/007-migrate-failure-contracts`
- Commit message: `test(migrate): characterize partial failures`
- Do not push or open a PR unless instructed.

## Steps

### Step 1: Build one stateful migration failure harness

In `internal/cli/migrate_test.go`, create a harness around the existing fake provider/agent conventions. It must track:

- created and deleted provider server IDs;
- bootstrap and protocol-preflight calls;
- agent methods by host and container;
- artifact copies/uploads;
- saved host facts;
- saved accessory state;
- current release state;
- emitted migrate events and command output.

Support named failure injection at exact boundaries without real sleeps or network. Prefer existing package-level hooks. If one boundary cannot be reached, add the smallest package-private function variable around that boundary in `migrate.go` and restore it with `t.Cleanup`.

**Verify**: existing successful `TestAcceptanceMigrateHostWorkflow` still passes using either the old harness or the shared replacement.

### Step 2: Cover failures before host-fact repoint

Add table-driven cases for:

- provider create failure: no replacement exists, no fact changes, no old delete, failed event, no cleanup note claiming a created server;
- created server has no public address: replacement remains, old fact remains, cleanup note names replacement, no bootstrap, no old delete;
- binary resolution or bootstrap failure: replacement remains, old fact remains, cleanup note names replacement, no accessory/service move, no old delete;
- replacement agent preflight failure: same residual contract as bootstrap failure;
- accessory backup/copy/restore failure before repoint: replacement remains, old fact remains, old server remains, event says failed.

Assert exact state transitions and key recovery-note substrings, not the entire formatted output.

**Verify**: `go test ./internal/cli -run 'TestMigrateFailureBeforeRepoint' -count=1` → exit 0.

### Step 3: Cover failures after accessory state changes

For a migration with a primary accessory, inject failure after the replacement accessory has been restored but before host-fact repoint, and another after accessory state is saved but before service restart.

Assert:

- which host the accessory state points to;
- whether the old accessory container was stopped;
- replacement and old servers both remain;
- the logical service host fact is old before repoint and new after repoint;
- the cleanup note tells the operator the actual residual state;
- no provider delete occurs.

If current cleanup text materially misstates residual accessory state, stop and report rather than weakening assertions.

**Verify**: `go test ./internal/cli -run 'TestMigrateAccessoryFailure' -count=1` → exit 0.

### Step 4: Cover failures after host-fact repoint

Inject failures in:

- resolving hosts after repoint;
- service rollout on the replacement;
- restart after accessory change.

For every case assert:

- host fact points to replacement;
- old server remains and old provider delete is absent;
- cleanup note includes `ship deploy <env>` guidance and both old/new contacts;
- current release remains unchanged;
- failed migrate event is persisted;
- old workloads have not been stopped before the relevant rollout/restart succeeds.

**Verify**: `go test ./internal/cli -run 'TestMigrateFailureAfterRepoint' -count=1` → exit 0.

### Step 5: Cover final cleanup failures

Add cases for:

- stopping one old workload fails: current code warns and continues; assert warning plus provider deletion behavior exactly;
- provider old-server deletion fails: migration returns error, fact remains pointed at replacement, both servers remain, failed event is recorded, and the cleanup note is actionable;
- `--keep-server`: old workloads stop, provider deletion is absent, success event is recorded, and output warns the server remains extra.

Do not convert warning behavior into failure in this plan.

**Verify**: `go test ./internal/cli -run 'TestMigrateFinalCleanup' -count=1` → exit 0.

### Step 6: Add a follow-on convergence characterization

For the most representative post-repoint rollout failure, invoke the documented recovery action through the fake harness: a subsequent deploy or resumed migration path, whichever current code supports without new behavior. Assert it converges host facts and service containers without deleting the wrong server.

If no safe documented follow-on path can be exercised, add a skipped-by-design test only after reporting the gap; do not invent recovery behavior.

**Verify**: `go test ./internal/cli -run 'TestMigrateFailureRecovery' -count=1` → exit 0.

### Step 7: Run all gates

Format any touched Go files, run migrate tests with the race detector, then the full race suite.

**Verify**: `gofmt -l internal/cli/migrate.go internal/cli/migrate_test.go internal/cli/acceptance_test.go` → no output; `go vet ./internal/cli` → exit 0; `go test -race ./...` → exit 0.

## Test plan

The required matrix covers every mutation boundary:

- create;
- public-address validation;
- binary resolve/bootstrap;
- replacement preflight;
- accessory transfer/restore;
- host-fact repoint;
- host re-resolution;
- service rollout;
- service restart after accessory move;
- old workload stop warning;
- old provider delete failure;
- keep-server success;
- one documented recovery/convergence path.

Each case must assert provider inventory, host facts, accessory state where relevant, release state, old/new container actions, delete calls, events, and recovery output.

## Done criteria

- [ ] Every listed irreversible boundary has deterministic failure injection and residual-state assertions.
- [ ] Tests use only fakes and temporary directories.
- [ ] No production behavior changes are included.
- [ ] Cleanup notes are checked against actual residual state.
- [ ] Old-server deletion never occurs in tests before required rollout/restart success.
- [ ] At least one post-failure recovery path is characterized or explicitly reported as a blocker.
- [ ] `gofmt -l` prints nothing for changed Go files.
- [ ] `go vet ./internal/cli` exits 0.
- [ ] `go test -race ./...` exits 0.
- [ ] No files outside the in-scope list changed.
- [ ] `plans/README.md` status row is updated.

## STOP conditions

Stop and report if:

- A test reveals old-server deletion after an incomplete service/accessory transition.
- Cleanup output claims a different host-fact/accessory state than the test observes.
- Current behavior loses the previous release or deletes both old and replacement servers.
- Failure injection requires exported production APIs or timing sleeps.
- A characterization assertion can pass without checking final state.
- Plan 006 changes event semantics beyond warning visibility/serialization.

## Maintenance notes

- Any future migration stage must add a failure-injected residual-state case before release.
- Reviewers should evaluate assertions as recovery contracts, not implementation-order snapshots.
- Plan 009 should land after this suite so CLI file moves retain migration failure coverage.