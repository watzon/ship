# Plan 004: Pull each image and ensure each network once per host rollout

> **Executor instructions**: Follow this plan step by step. Run every verification command and confirm the expected result before moving to the next step. If anything in the "STOP conditions" section occurs, stop and report; do not improvise. When done, update this plan's row in `plans/README.md` unless a reviewer told you they maintain the index.
>
> **Drift check (run first)**: `git diff --stat 93da974..HEAD -- internal/deployment/deployment.go internal/deployment/deployment_test.go`
> Plan 002 is an expected dependency and may change action kinds and execution results. Re-anchor this plan on its final rollout structure. Stop if image pull or network ensure semantics changed independently.

## Status

- **Priority**: P1
- **Effort**: S
- **Risk**: LOW
- **Depends on**: `plans/002-compensate-failed-rollouts.md`
- **Category**: perf
- **Planned at**: commit `93da974`, 2026-07-16

## Why this matters

`BuildActions` emits one image pull for every placement, even when several replicas on the same host use the same immutable digest. `ExecuteActions` also calls `ensure_network` before every container start. Because each agent RPC opens a fresh SSH process, repeated idempotent work adds Docker calls and SSH handshakes without changing state. Deduplicating by host and immutable resource preserves rollout ordering while reducing scale-out latency.

## Current state

- `internal/deployment/deployment.go` is the only production file in scope.
- `internal/deployment/deployment_test.go` already asserts exact action order and uses fake agents; follow those patterns.
- Host names are unique within one resolved environment. Use explicit composite keys with a NUL separator, matching `observedKey` and other repository helpers.
- Do not parallelize work in this plan.

One pull is emitted per placement (`internal/deployment/deployment.go:189-208`):

```go
for _, placement := range placements {
	svc := input.Config.Services[placement.Service]
	image := input.Images[placement.Service]
	if strings.TrimSpace(image) == "" {
		return nil, fmt.Errorf("missing image for service %q", placement.Service)
	}
	name := ContainerName(input.Config.Project, input.EnvName, placement.Service, placement.Replica, input.ReleaseID)
	desiredNames[name] = struct{}{}
	actions = append(actions, Action{
		Kind:          ActionPull,
		Host:          placement.Host,
		Service:       placement.Service,
		Replica:       placement.Replica,
		Release:       input.ReleaseID,
		ContainerName: name,
		Image:         image,
	})
```

Network ensure is per start (`internal/deployment/deployment.go:458-473`):

```go
case ActionStart:
	client := agentFor(action.Host)
	if err := ensureNetwork(ctx, client, action); err != nil {
		return fmt.Errorf("ensure network %s on %s: %w", action.Network, action.Host.Name, err)
	}
	params := agent.RunContainerParams{
		Name:           action.ContainerName,
		Image:          action.Image,
```

Current action-order tests expect repeated pulls for separate placements (`internal/deployment/deployment_test.go:168-175`):

```go
got := actionKinds(actions)
want := []ActionKind{
	ActionPull, ActionStart, ActionHealth, ActionStop,
	ActionPull, ActionStart, ActionHealth, ActionStop,
}
```

Those two placements are on different hosts, so that expectation remains valid. New tests must place multiple replicas on the **same** host.

## Commands you will need

| Purpose | Command | Expected on success |
|---|---|---|
| Focused deployment tests | `go test ./internal/deployment -count=1` | exit 0 |
| Dedupe tests | `go test ./internal/deployment -run 'Test.*(Pull|Network).*Once' -count=1` | exit 0 |
| Format check | `gofmt -l internal/deployment/deployment.go internal/deployment/deployment_test.go` | no output |
| Vet | `go vet ./internal/deployment` | exit 0 |
| Full race suite | `go test -race ./...` | exit 0 |

## Scope

**In scope**:

- `internal/deployment/deployment.go`
- `internal/deployment/deployment_test.go`
- `plans/README.md` for status only

**Out of scope**:

- Agent RPC, SSH transport, Docker client, image build/push/resolve, or registry auth.
- Pull caching across separate `ship` commands.
- Parallel execution.
- Reordering start, health, canary, ingress, drain, or compensation actions from Plan 002.
- Deduplicating pulls across different image strings, even if tags might resolve to the same digest.
- Skipping network ensure based on observed Docker state.

## Git workflow

- Branch: `advisor/004-deduplicate-rollout-host-work`
- Commit message: `perf(deploy): deduplicate per-host rollout setup`
- Do not push or open a PR unless instructed.

## Steps

### Step 1: Add same-host regression fixtures

In `internal/deployment/deployment_test.go`, construct an environment with one host and at least three placements on that host. Use two services where useful:

- two replicas of one service using the same image;
- a second service using a different image;
- all services using the same configured Docker network.

Add a `BuildActions` test asserting exactly one `ActionPull` for each `(host, image)` pair. Preserve first-use placement: the pull must appear before the first start that needs it.

Add an execution test with a fake agent that counts `ensure_network` calls and assert exactly one successful ensure per `(host, network)` pair.

**Verify**: run the new tests before implementation → they fail by observing repeated work.

### Step 2: Deduplicate pull actions during planning

In `BuildActions`, maintain a local set keyed by host identity and exact image string. Before appending `ActionPull`:

1. Build the key from `placement.Host.Name`, a NUL separator, and `image`.
2. Append the action only when the key has not been seen.
3. Mark the key seen only after the action is appended.

Keep all existing missing-image validation per service. Do not hoist pulls globally to the beginning; first-use placement minimizes unrelated work before a later planning error and preserves readable action order.

Action metadata for the first pull may use the first placement's service/replica. No code may depend on pull actions existing once per replica; update only tests that asserted that incidental detail.

**Verify**: `go test ./internal/deployment -run 'Test.*Pull.*Once' -count=1` → exit 0.

### Step 3: Deduplicate successful network ensures during execution

Within one execution call, maintain a set keyed by host name and network name. Before a start action:

- Skip ensure only when that exact key completed successfully earlier in the same execution.
- If network is empty and existing `ensureNetwork` treats it as no-op, preserve that behavior without adding an empty-key entry.
- If ensure fails, return the existing error and do not mark it seen.
- Do not share cache state across rollout calls.

If Plan 002 refactored execution into a transaction object, store the set on that per-rollout executor rather than as package-global state.

**Verify**: `go test ./internal/deployment -run 'Test.*Network.*Once' -count=1` → exit 0.

### Step 4: Protect non-deduplication boundaries

Add or update tests proving:

- Same image on two different hosts still pulls once per host.
- Two different image digests on one host each pull once.
- Two different networks on one host each ensure once.
- A failed ensure is not cached.
- Plan 002 compensation/retry within the same execution does not incorrectly assume a network that never completed.

**Verify**: `go test ./internal/deployment -count=1` → exit 0.

### Step 5: Run all gates

Format both files and run focused, vet, and full race checks.

**Verify**: `gofmt -l internal/deployment/deployment.go internal/deployment/deployment_test.go` → no output; `go vet ./internal/deployment` → exit 0; `go test -race ./...` → exit 0.

## Test plan

Use table-driven subtests where keys differ by one dimension:

- same host/same image;
- different host/same image;
- same host/different image;
- same host/same network;
- same host/different network;
- ensure failure followed by command failure, proving failed ensures are not cached.

Model action-order assertions after `TestBuildActionsHonorsMaxSurgeOneForRollingReplacement`. Model fake-agent method recording after `fakeAgent` in `internal/deployment/deployment_test.go`.

## Done criteria

- [ ] One rollout emits at most one pull per exact `(host, image)` pair.
- [ ] One rollout performs at most one successful network ensure per exact `(host, network)` pair.
- [ ] Different hosts/images/networks are never conflated.
- [ ] Failed ensures are not cached.
- [ ] Pull remains before first dependent start.
- [ ] Plan 002 compensation tests still pass.
- [ ] `gofmt -l` prints nothing for both files.
- [ ] `go vet ./internal/deployment` exits 0.
- [ ] `go test -race ./...` exits 0.
- [ ] No source files outside the in-scope list changed.
- [ ] `plans/README.md` status row is updated.

## STOP conditions

Stop and report if:

- Plan 002 changed image pulling or network setup into a different phase that invalidates first-use deduplication.
- Host names are not unique in the resolved placement set.
- Any caller relies on an `ActionPull` for every replica for user-visible output or progress accounting.
- Network ensure has non-idempotent per-container behavior.
- The optimization requires persistent cache state or Docker inspection.

## Maintenance notes

- Cache keys deliberately use the exact immutable image string; do not normalize tags or repositories here.
- If progress reporting later counts pull actions, report skipped duplicate work explicitly rather than restoring redundant RPCs.
- Plan 010 may parallelize independent host phases; it must preserve this per-host dedupe.