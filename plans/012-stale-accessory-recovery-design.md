# Plan 012: Design an explicit recovery path for stale accessory placement

> **Executor instructions**: This is a design spike, not an implementation plan. Follow it step by step, gather evidence from the live repository, and write the specified design artifact. Do not change production code. If anything in the "STOP conditions" section occurs, stop and report; do not improvise. When done, update this plan's row in `plans/README.md` unless a reviewer told you they maintain the index.
>
> **Drift check (run first)**: `git diff --stat 93da974..HEAD -- internal/accessory internal/cli internal/state docs`
> Plans 006, 007, and 009 may change event handling, migration tests, and CLI file paths. Stop if stale-placement recovery already landed or accessory state schema changed materially.

## Status

- **Priority**: Direction
- **Effort**: M
- **Risk**: HIGH
- **Depends on**: `plans/006-visible-race-safe-events.md`, `plans/007-migrate-failure-contracts.md`, `plans/009-split-cli-command-domains.md`
- **Category**: direction
- **Planned at**: commit `93da974`, 2026-07-16

## Why this matters

Ship intentionally refuses accessory placement when saved state points to a host no longer eligible in the pool. That prevents silent movement of a single-primary database, but it also blocks `accessory restore` and `accessory failover`, the commands an operator needs after a host disappears. Recovery must be explicit and auditable, and it must address split-brain risk when the old primary might still be running.

## Current state

Placement stops on stale state (`internal/accessory/accessory.go:100-117`):

```go
if saved, err := store.ReadAccessoryState(envName, name); err == nil {
	if host, ok := matchingHost(saved.Host, eligible); ok {
		return Placement{Name: name, Host: host, Persisted: true}, nil
	}
	return Placement{}, fmt.Errorf("accessory %q saved placement host %q is not eligible in pool %q; failover is not implemented", name, saved.Host.Name, acc.Pool)
}
```

The existing failover is safe only when the source is still resolvable (`internal/cli/root.go:6909-6977` at planning commit):

```go
current, err := accessory.PlacementForHosts(cfg, hosts, envName, name, store)
if err != nil {
	return fail(err)
}
// validate both source and target topology
result, err := startAccessoryWithRestore(ctx, opts, cfg, envName, name, target, artifact)
if err != nil {
	return fail(err)
}
if err := newDeployAgent(current.Host).Call(ctx, "stop_container", map[string]string{"name": containerName}, nil); err != nil {
	return fail(fmt.Errorf("stop old accessory %s on %s: %w", name, current.Host.Name, err))
}
saved.Host = accessory.HostFact(target)
```

Both restore and failover call `PlacementForHosts`, so the stale error is terminal. `docs/recovery.md` documents ordinary restore but not lost-host/fencing recovery.

## Commands you will need

| Purpose | Command | Expected on success |
|---|---|---|
| Map recovery paths | `git grep -n 'PlacementForHosts\|runAccessoryFailover\|runAccessoryRestore\|AccessoryState' -- internal docs` | current paths listed |
| Accessory tests | `go test ./internal/accessory ./internal/cli -run 'Test.*Accessory' -count=1` | exit 0 |
| State tests | `go test ./internal/state -count=1` | exit 0 |
| Design whitespace | `git diff --check` | exit 0 |

## Scope

**In scope**:

- Read-only investigation of `internal/accessory`, `internal/cli`, `internal/state`, `internal/provider`, and recovery/operator docs.
- Create `docs/design/stale-accessory-recovery.md`.
- `plans/README.md` for status only.
- Disposable fake-state experiments in a temporary directory.

**Out of scope**:

- Production Go changes or a new command.
- Automatic failover, leader election, consensus, replication, or database-specific promotion.
- Provider deletion or power-off implementation.
- Backup transport redesign.
- Service volumes.
- Silently editing accessory state or choosing the first eligible host.

## Git workflow

- Branch: `advisor/012-stale-accessory-recovery-design`
- Commit message: `docs(design): specify stale accessory recovery`
- Commit only the design artifact and plan index status.
- Do not push or open a PR unless instructed.

## Steps

### Step 1: Define the stale-placement states precisely

Document distinct cases that currently collapse into one error:

1. Saved host still exists but moved out of the accessory pool.
2. Saved logical host was repointed to a replacement server.
3. Saved host is absent from config/inventory but may still be running.
4. Saved host is known destroyed or powered off.
5. Saved host is reachable but its managed accessory container is absent/stopped.
6. Saved state references corrupt/ambiguous identity.

For each, state what Ship can prove from config, saved host facts, provider inventory, and agent observations. Never equate “not in current inventory” with “fenced.”

**Verify**: the artifact separates discovery uncertainty from actual source shutdown.

### Step 2: Compare recovery surfaces

Evaluate:

1. Extend `accessory failover` with a disaster-recovery mode.
2. Add a distinct `accessory recover` command for a stale/unreachable source.
3. Allow manual accessory-state reassignment and then ordinary restore.

Score clarity, accidental split-brain risk, auditability, dry-run quality, backward compatibility, provider-neutral behavior, and reuse of existing restore helpers.

Recommended default: a distinct `accessory recover` surface. Ordinary failover should continue requiring a resolvable source that Ship can stop; manual state editing should remain unsupported.

**Verify**: design chooses one surface and explains why operators cannot confuse planned live failover with disaster recovery.

### Step 3: Design a safety state machine

Specify states and allowed transitions, at minimum:

- `healthy_on_saved_host`;
- `saved_host_stale_source_unknown`;
- `source_fenced_or_destroyed`;
- `target_restore_started`;
- `target_restore_verified`;
- `state_committed_to_target`;
- `recovery_failed_before_commit`;
- `recovery_failed_after_target_start`.

For every transition define:

- required observations;
- explicit user attestations;
- remote mutation;
- saved state/event update;
- safe retry behavior;
- output needed for incident handoff.

No transition from source-unknown to target start may be implicit. The design must require either provider-verifiable fencing/destruction or an explicit operator attestation flag with `--yes`. Pick the final flag language; avoid a vague `--force`.

**Verify**: no path can create a target primary merely because the source is absent from inventory.

### Step 4: Specify the command contract and preflight

Design the complete proposed command, including exact syntax. Recommended candidate:

```text
ship accessory recover ENV NAME --to HOST --artifact PATH --confirm-source-fenced --yes
```

Evaluate whether a provider-verified mode can omit the attestation when Ship can prove the old provider server is destroyed. Required preflight:

- accessory is single-primary and has restore commands;
- saved accessory state exists and is stale;
- target is eligible and has no managed accessory container;
- artifact passes current restore-path validation and is available on target;
- current observed topology has no other managed copy on reachable hosts;
- environment operation lock is held;
- maintenance mode guidance is emitted;
- source fencing is proved or explicitly attested;
- `--dry-run` makes no state/remote changes and prints every unresolved risk.

Specify refusal cases and exact actionable error guidance.

**Verify**: every flag, confirmation, and refusal case has a reason tied to a risk.

### Step 5: Specify commit point and partial-failure recovery

Use existing `startAccessoryWithRestore` behavior as the likely restore primitive, but design an explicit commit point:

- before target restore verification, saved placement remains the old host;
- after restore verification and before state commit, a running target may exist but is not authoritative in Ship state;
- state commit to target occurs only after restore check success;
- after commit, retries treat target as authoritative;
- events distinguish `started`, `target_started`, `committed`, and `failed` or explain why existing schema can encode these safely.

For each failure boundary specify whether the target container is stopped/removed, left for inspection, or requires a follow-on command. Never automatically reconnect to or delete an unknown source.

Address Plan 006 event-write warnings: recovery success must not be misreported as failure solely because event persistence failed, but the warning must be visible.

**Verify**: failure table covers preflight, target start, restore command, restore check, state save, and event warning.

### Step 6: Define identity and fencing evidence

Trace saved `HostFact`, provider IDs, logical names, and replacement-server handling. Specify which evidence is sufficient:

- provider reports saved provider ID absent;
- provider reports instance powered off/deleted;
- config merely omits host;
- SSH is unreachable;
- agent container observation says absent;
- operator attests external fencing.

Treat provider absence carefully: inventory APIs may filter by labels/environment. The design must not claim proof unless the provider contract guarantees it. If portable proof is impossible, make attestation mandatory and provider verification supplemental.

**Verify**: artifact has an evidence table with `proves fenced`, `supporting only`, or `insufficient` for each signal.

### Step 7: Produce an implementation-plan recommendation

End `docs/design/stale-accessory-recovery.md` with one verdict:

- `IMPLEMENT_DISTINCT_RECOVER_COMMAND`;
- `EXTEND_FAILOVER_WITH_RECOVERY_MODE`;
- `REJECT_WITH_DOCUMENTED_MANUAL_RUNBOOK`.

For an implementation verdict, list exact future files/symbols, state/event changes, tests, documentation changes, and rollout order. Include a full fake-based test matrix for split-brain refusal and every partial failure.

**Verify**: a fresh executor could create an implementation plan without deciding safety semantics.

### Step 8: Validate the artifact

Run current accessory/state tests and validate links/references.

**Verify**: `go test ./internal/accessory ./internal/state ./internal/cli -run 'Test.*Accessory|Test.*Event' -count=1` → exit 0; `git diff --check` → exit 0; only the design artifact and plan status changed.

## Test plan

This spike produces design evidence, not source tests. Required artifact tables:

- stale-state taxonomy;
- signal/fencing evidence strength;
- state machine transitions;
- command preflight/refusal cases;
- partial failure residual state and retry action;
- future fake-based implementation test matrix.

A future implementation must test source-unknown refusal, explicit attestation, provider proof where reliable, target already occupied, another observed primary, stale artifacts, restore/check failure, state-save failure, retry before/after commit, dry-run, and event-write warning.

## Done criteria

- [ ] `docs/design/stale-accessory-recovery.md` exists and is self-contained.
- [ ] Missing inventory is never treated as proof the old primary is fenced.
- [ ] Live failover and disaster recovery have unambiguous command semantics.
- [ ] Safety state machine and commit point are explicit.
- [ ] Every partial failure has residual state and a safe retry/recovery action.
- [ ] Fencing evidence table is provider-neutral and honest about uncertainty.
- [ ] Artifact ends with one machine-readable verdict and exact future scope.
- [ ] No production source or test file changed.
- [ ] Current focused tests pass.
- [ ] `git diff --check` exits 0.
- [ ] `plans/README.md` status row is updated.

## STOP conditions

Stop and report if:

- The proposed flow can start a second primary without proof or explicit attestation that the source is fenced.
- Saved host identity is insufficient to distinguish a replaced server from the old physical/provider server.
- Existing backup artifacts cannot be validated or made available on the target without a separate backup-transport design.
- A design relies on all providers reporting deleted/powered-off state consistently without contract evidence.
- Recovery would silently rewrite accessory state without restore verification.

## Maintenance notes

- This design must be reviewed like a disaster-recovery protocol, not ordinary CLI UX.
- Provider-specific fencing checks are defense in depth; the portable contract must remain safe without them.
- Any future multi-primary or replicated accessory model needs a separate state machine and must not reuse single-primary attestation semantics blindly.