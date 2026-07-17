# Plan 002: Restore pre-rollout service capacity when a rollout fails

> **Executor instructions**: Follow this plan step by step. Run every verification command and confirm the expected result before moving to the next step. If anything in the "STOP conditions" section occurs, stop and report; do not improvise. When done, update this plan's row in `plans/README.md` unless a reviewer told you they maintain the index.
>
> **Drift check (run first)**: `git diff --stat 93da974..HEAD -- internal/deployment/deployment.go internal/deployment/deployment_test.go internal/agent/rpc.go internal/agent/rpc_test.go internal/docker/docker.go internal/docker/docker_test.go internal/cli/root.go internal/cli/root_test.go`
> Plan 001 is an expected dependency. Compare the live RPC registry with Plan 001's final shape. Stop if deployment action ordering or rollout failure handling changed for any other reason.

## Status

- **Priority**: P1
- **Effort**: L
- **Risk**: HIGH
- **Depends on**: `plans/001-agent-rpc-registry.md`
- **Category**: bug
- **Planned at**: commit `93da974`, 2026-07-16

## Why this matters

`ExecuteActions` returns at the first failed start or health check but does not reverse earlier service mutations. Stop-first rollouts, including fixed host ports and `max_surge: 0`, may already have removed the previous healthy container. Multi-host failures can leave a mixed fleet while local and remote release metadata continue to identify the old release as current. The fix must preserve old containers until the new rollout commits, compensate all pre-commit service mutations on failure, and report cleanup failures without falsely promoting or failing a healthy release.

## Current state

- `internal/deployment/deployment.go` builds a flat ordered action list and executes it sequentially.
- `internal/agent/rpc.go` exposes `stop_container`, whose Docker operation stops **and removes** the container.
- `internal/docker/docker.go` implements the conflated lifecycle operation.
- `internal/cli/root.go` marks the candidate release failed after any `deployment.Rollout` error; it performs no service compensation.
- `internal/cli/root_test.go` has strong fake-agent deploy tests. Extend those instead of introducing a second harness.

Stop-first planning (`internal/deployment/deployment.go:209-258`):

```go
if usesFixedHostPorts(svc) {
	for _, old := range replacementStopCandidates(input.Config, input.EnvName, placement, name, input.Observed) {
		actions = appendStopActions(actions, old, svc)
		preStopped[observedKey(old)] = struct{}{}
	}
}
actions = append(actions, Action{
	Kind:           ActionStart,
	Host:           placement.Host,
	Service:        placement.Service,
	Replica:        placement.Replica,
```

Failure returns immediately (`internal/deployment/deployment.go:451-478`):

```go
for _, action := range actions {
	switch action.Kind {
	case ActionPull:
		client := agentFor(action.Host)
		if err := client.Call(ctx, "pull", map[string]string{"image": action.Image}, nil); err != nil {
			return fmt.Errorf("pull %s on %s: %w", action.Image, action.Host.Name, err)
		}
	case ActionStart:
		client := agentFor(action.Host)
		if err := ensureNetwork(ctx, client, action); err != nil {
			return fmt.Errorf("ensure network %s on %s: %w", action.Network, action.Host.Name, err)
		}
```

Docker currently removes stopped containers (`internal/docker/docker.go:649-656`):

```go
func (c Client) StopRemove(ctx context.Context, name string) error {
	if err := c.run(ctx, "docker", "stop", name); err != nil && !isNoSuchContainer(err) {
		return err
	}
	if err := c.run(ctx, "docker", "rm", name); err != nil && !isNoSuchContainer(err) {
		return err
	}
	return nil
}
```

Use the ingress snapshot rollback at `internal/deployment/deployment.go:523-652` as the local convention: capture live state, attempt mutation, reverse completed work, and include rollback failures in the returned error.

## Commands you will need

| Purpose | Command | Expected on success |
|---|---|---|
| Docker tests | `go test ./internal/docker -count=1` | exit 0 |
| Agent tests | `go test ./internal/agent -count=1` | exit 0 |
| Deployment tests | `go test ./internal/deployment -count=1` | exit 0 |
| CLI regressions | `go test ./internal/cli -run 'TestDeploy(FailedHealth|PartialHostFailure|StopsFixedPort)' -count=1` | exit 0; selected tests pass |
| Full verification | `go test -race ./...` | exit 0; all packages pass |
| Vet | `go vet ./...` | exit 0; no diagnostics |

## Scope

**In scope**:

- `internal/docker/docker.go`
- `internal/docker/docker_test.go`
- `internal/agent/rpc.go`
- `internal/agent/rpc_test.go`
- `internal/deployment/deployment.go`
- `internal/deployment/deployment_test.go`
- `internal/cli/root.go`
- `internal/cli/root_test.go`
- `plans/README.md` for status only

**Out of scope**:

- Accessory rollback or data rollback.
- Rebuilding previous images or resolving mutable tags; use the existing stopped containers.
- Parallel action execution; Plan 010 handles independent fan-out.
- Changing rolling defaults, canary semantics, health definitions, or ingress generation.
- Automatic retry of a failed release.
- Backward-compatible fallbacks that silently use old destructive `stop_container` behavior. If required lifecycle RPCs are unavailable, preflight must stop before mutation.

## Git workflow

- Branch: `advisor/002-compensate-failed-rollouts`
- Commit message: `fix(deploy): restore services after failed rollout`
- Keep the lifecycle RPC addition, deployment transaction, and tests in reviewable logical commits if more than one commit is needed.
- Do not push or open a PR unless instructed.

## Steps

### Step 1: Lock in the current failure with characterization tests

Before production changes, extend `internal/cli/root_test.go` and `internal/deployment/deployment_test.go` with failing tests for these contracts:

1. Fixed-port old container is stopped before the candidate starts; candidate health fails; the old container is running again and the failed candidate is removed.
2. Two hosts begin rollout; host one reaches healthy candidate state; host two fails health; both old containers are restored, both candidate containers are absent, current release remains old, and ingress was not shifted.
3. A compensation failure is appended to the primary rollout error and recorded in the deploy failure event.
4. A fully successful rollout removes preserved old containers only after the commit point.

Use stateful fake agents that track `running` and `stopped` container names. Assertions must inspect final inventory, not only method order.

**Verify**: run the new focused tests before implementation → they fail for the missing compensation behavior, while unrelated existing tests still pass.

### Step 2: Separate stop, start-existing, and remove in Docker and agent RPC

In `internal/docker/docker.go`, add idempotent methods with exact Docker argv behavior:

- `Stop(ctx, name)`: `docker stop`; ignore only no-such-container.
- `Start(ctx, name)`: `docker start`; return an error when the preserved container no longer exists.
- `Remove(ctx, name)`: `docker rm`; ignore only no-such-container.

Keep `StopRemove` for existing non-transactional callers.

Extend `agent.DockerOps` and register three explicit RPC methods through Plan 001's registry:

- `stop_container_keep`
- `start_container`
- `remove_container`

Each validates a non-empty name and executes under the host lock. Add focused Docker argv tests and RPC validation/dispatch tests.

Bump the agent protocol only if existing negotiation cannot guarantee these methods before mutation. If a bump is needed, update protocol tests and require incompatible agents to fail preflight before rollout; do not fall back to destructive stop/remove.

**Verify**: `go test ./internal/docker ./internal/agent -count=1` → exit 0.

### Step 3: Represent reversible replacement actions explicitly

In `internal/deployment/deployment.go`, add distinct action kinds for preserving an old container and removing a preserved container. Keep `ActionStop` for intentional final deletion of true orphans and zero-scale containers.

For replacement candidates that currently go through `appendStopActions`, emit a preserve-stop action and retain enough action metadata to restart that exact existing container. Do not recreate it from current config; restart the stopped container by name.

Do not emit removal of preserved old containers interleaved with candidate start/health actions. Removal belongs to post-commit cleanup after the complete rollout and ingress action succeed.

Update action-order tests to assert:

- pull → preserve-stop → start → health for stop-first replacements;
- old-container removal is not part of the pre-commit action stream;
- orphan and zero-scale cleanup still uses destructive stop/remove.

**Verify**: `go test ./internal/deployment -run 'TestBuildActions' -count=1` → exit 0.

### Step 4: Add an execution journal and compensation

Refactor action execution so it tracks only completed, reversible service mutations:

- Every successful candidate `ActionStart` is journaled.
- Every successful preserve-stop of an old container is journaled.
- Pulls, health checks, delays, and reads are not journaled.

On any error before the commit point:

1. Remove successfully started candidate containers in reverse execution order.
2. Restart every successfully preserved old container in reverse execution order.
3. Attempt every compensation even if one fails.
4. Return the primary rollout error plus deterministic host/container-specific compensation failures.

The commit point is successful completion of the service action stream and the ingress action. After commit, remove preserved old containers. Expose post-commit cleanup failures separately from rollout failure so `cli` can keep the healthy release current while emitting warnings/events for leaked stopped containers.

Use a typed result rather than parsing error strings. A suitable internal shape is a rollout result containing actions plus cleanup warnings; choose exact names that match package conventions.

**Verify**: `go test ./internal/deployment -count=1` → exit 0; compensation tests pass.

### Step 5: Integrate cleanup warnings without false release state

Update `internal/cli/root.go` to distinguish:

- pre-commit rollout failure: mark candidate failed, sync failed release state, preserve previous current release;
- successful commit with post-commit cleanup warnings: mark candidate healthy/current, record one warning event per deterministic cleanup failure, print warnings, and return success;
- compensation failure: include it in the failed event and command error.

Do not mark a serving, committed release failed solely because an old stopped container could not be removed.

Update CLI tests for current-release state, event messages, remote release state, final container inventory, and warnings.

**Verify**: `go test ./internal/cli -run 'TestDeploy(FailedHealth|PartialHostFailure|StopsFixedPort)' -count=1` → exit 0.

### Step 6: Run all gates

Format all changed Go files, then run focused packages followed by the full race suite and vet.

**Verify**: `gofmt -l internal/docker/docker.go internal/docker/docker_test.go internal/agent/rpc.go internal/agent/rpc_test.go internal/deployment/deployment.go internal/deployment/deployment_test.go internal/cli/root.go internal/cli/root_test.go` → no output; `go vet ./...` → exit 0; `go test -race ./...` → exit 0.

## Test plan

Add observable-contract tests for:

- fixed-port health failure restores old capacity;
- partial multi-host failure restores all pre-rollout containers;
- candidate containers are removed during compensation;
- compensation continues after one cleanup error and reports all failures deterministically;
- successful rollout removes preserved old containers after commit;
- post-commit removal failure warns without changing healthy/current state;
- orphan and zero-scale cleanup remain destructive;
- old agents lacking required lifecycle RPC methods are rejected before service mutation when protocol negotiation requires a bump.

Model CLI fixtures after `TestDeployFailedHealthDoesNotShiftTrafficOrCurrent`, `TestDeployPartialHostFailureLeavesPreviousCurrent`, and `TestDeployStopsFixedPortOldBeforeStartAndPromotesHealthyRelease` in `internal/cli/root_test.go`.

## Done criteria

- [ ] A failed fixed-port or surge-disabled rollout restores every preserved old container.
- [ ] A failed rollout removes every candidate container it started before failure.
- [ ] Compensation attempts all journal entries and returns deterministic combined errors.
- [ ] Old containers are not removed before the rollout commit point.
- [ ] Post-commit cleanup errors do not mark a healthy serving release failed.
- [ ] Existing orphan, removed-service, and zero-scale behavior remains covered and passing.
- [ ] Required new agent methods are guaranteed before mutation; there is no destructive compatibility fallback.
- [ ] `gofmt -l` reports no changed file.
- [ ] `go vet ./...` exits 0.
- [ ] `go test -race ./...` exits 0.
- [ ] No files outside the in-scope list changed.
- [ ] `plans/README.md` status row is updated.

## STOP conditions

Stop and report if:

- Plan 001 has not landed or the RPC registry is absent.
- Docker cannot restart a stopped container with its original configuration by name on supported Docker versions.
- Existing `stop_container` semantics are relied on by a caller that would be silently changed.
- The rollout commit point cannot be represented without changing ingress or release-state public behavior.
- Tests reveal that successful service replacement requires deleting the old container before candidate health succeeds.
- Compatibility would require silently falling back to stop-and-remove on an old agent.
- Any new failure-path test remains nondeterministic after replacing timing with fakes.

## Maintenance notes

- Future action kinds must declare whether they are reversible, what journal entry they create, and whether they occur before or after commit.
- Reviewers should focus on fixed ports, canary/max-surge ordering, error aggregation, and release-state truth after post-commit cleanup failures.
- This plan intentionally does not roll back accessories or data migrations.
- Plan 004 must be rebased on the final action ordering from this plan.