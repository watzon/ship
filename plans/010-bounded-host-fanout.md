# Plan 010: Parallelize only independent host RPC phases with a bounded worker

> **Executor instructions**: Follow this plan step by step. Run every verification command and confirm the expected result before moving to the next step. If anything in the "STOP conditions" section occurs, stop and report; do not improvise. Benchmark before and after; do not claim a speedup from code shape alone. When done, update this plan's row in `plans/README.md` unless a reviewer told you they maintain the index.
>
> **Drift check (run first)**: `git diff --stat 93da974..HEAD -- internal/agent/client.go internal/deployment internal/cli internal/fanout`
> Plans 002, 004, 005, and 009 are expected dependencies. Re-anchor CLI paths on Plan 009's final files. Stop if rollout actions already execute concurrently or a shared fan-out helper already exists.

## Status

- **Priority**: P3
- **Effort**: L
- **Risk**: MED
- **Depends on**: `plans/002-compensate-failed-rollouts.md`, `plans/004-deduplicate-rollout-host-work.md`, `plans/005-local-ci-parity.md`, `plans/009-split-cli-command-domains.md`
- **Category**: perf
- **Planned at**: commit `93da974`, 2026-07-16

## Why this matters

Each agent call starts a new SSH process. Several fleet-wide phases contact hosts sequentially even though work on one host has no ordering dependency on another. Fleet latency therefore grows with host count and SSH handshake time. Bounded host-level fan-out can reduce wall time without changing rolling/canary order, but only after measurement and with deterministic errors/output.

## Current state

- `internal/agent/client.go` opens one SSH process per `Call`.
- `internal/deployment/deployment.go` inspects hosts sequentially.
- CLI preflight, bootstrap, registry-auth sync, secret writes, and release-state sync use serial loops.
- `ExecuteActions` intentionally encodes deployment ordering. It is explicitly out of scope for parallelization.
- Plan 009 will move CLI symbols into domain files. Follow symbols, not old line numbers, after that plan lands.

One-shot transport (`internal/agent/client.go:52-64`):

```go
payload, err := json.Marshal(params)
if err != nil {
	return err
}
requestID := c.requestID()
req, err := json.Marshal(Request{ID: requestID, Method: method, Params: payload, ProtocolVersion: AgentProtocol})
if err != nil {
	return err
}
command := agentRPCCommand(config.RemoteBinaryPath)
raw, err := c.SSH.RunWithStdin(ctx, command, string(req)+"\n")
```

Sequential inspection (`internal/deployment/deployment.go:155-169`):

```go
var observed []ObservedContainer
for _, host := range hosts {
	var containers []docker.ContainerSummary
	if err := agentFor(host).Call(ctx, "list_ship_containers", map[string]any{}, &containers); err != nil {
		return nil, fmt.Errorf("inspect observed containers on %s: %w", host.Name, err)
	}
	for _, container := range containers {
		observed = append(observed, ObservedContainer{Host: host, Container: container})
	}
}
```

Sequential release-state writes (`internal/cli/root.go:3697-3707` at planning commit):

```go
var failures []string
for _, host := range hosts {
	if err := newDeployAgent(host).Call(ctx, "write_release_state", agent.WriteReleaseStateParams{Release: release}, nil); err != nil {
		failures = append(failures, fmt.Sprintf("%s: %v", host.Name, err))
	}
}
```

## Commands you will need

| Purpose | Command | Expected on success |
|---|---|---|
| Fan-out tests | `go test -race ./internal/fanout -count=1` | exit 0 |
| Deployment tests | `go test -race ./internal/deployment -count=1` | exit 0 |
| CLI tests | `go test -race ./internal/cli -count=1` | exit 0 |
| Benchmark | `go test ./internal/cli -run '^$' -bench 'BenchmarkHostFanout' -benchmem -count=5` | benchmark output for serial and bounded cases |
| Full local CI | `./scripts/ci-local.sh` | exit 0 |

## Scope

**In scope**:

- `internal/fanout/fanout.go` (create)
- `internal/fanout/fanout_test.go` (create)
- `internal/deployment/deployment.go`
- `internal/deployment/deployment_test.go`
- After Plan 009, the final files containing these CLI symbols:
  - `preflightAgentProtocols`
  - provision host bootstrap loop
  - `syncRemoteRegistryAuth`
  - `syncRemoteReleaseState`
  - `writeRemoteSecretFiles`
- Matching focused CLI test files from Plan 009
- `plans/README.md` for status only

**Out of scope**:

- `deployment.ExecuteActions`, canary/drain/health order, compensation journal, ingress commit, or cleanup.
- Parallel operations within the same host.
- SSH ControlMaster defaults, connection pooling, or transport replacement.
- Configurable user-facing concurrency flags in this first change.
- Provider API reconciliation and cloud request parallelism.
- Accessory backup/restore/failover.
- Unbounded goroutines.

## Git workflow

- Branch: `advisor/010-bounded-host-fanout`
- Commit message: `perf(cli): bound independent host fanout`
- Keep helper/tests separate from caller migrations if using multiple commits.
- Do not push or open a PR unless instructed.

## Steps

### Step 1: Add a deterministic baseline benchmark

Before production changes, add a benchmark using fake host jobs that each block for a controlled short duration through channels, not real SSH or wall-clock sleeps. Measure serial execution and a bounded limit of four over representative fleet sizes 1, 4, 16, and 64.

The benchmark is evidence for scheduling overhead and concurrency behavior, not a production latency claim. Record baseline output in the PR description.

**Verify**: benchmark command runs and reports allocations/time for every fleet size.

### Step 2: Implement a small generic fan-out helper

Create `internal/fanout` with one generic function that:

- accepts a parent context, ordered input slice, positive concurrency limit, and callback;
- runs no more than `limit` callbacks concurrently;
- stores one result/error slot per input index;
- returns results in input order;
- stops scheduling new work when the parent context is canceled;
- waits for already-started callbacks to finish;
- does not cancel sibling jobs merely because one job fails, allowing complete fleet diagnostics;
- has no package-global worker state.

Use standard library synchronization only. Validate `limit > 0`; an empty input returns immediately. Avoid worker leaks when callbacks return early or the context is canceled.

Add race tests for maximum concurrency, ordered results, multiple errors, empty input, invalid limit, and cancellation while work is queued.

**Verify**: `go test -race ./internal/fanout -count=1` → exit 0.

### Step 3: Parallelize observed-state inspection by host

Update `InspectObservedOnHosts` to fan out one list call per host with a conservative package constant of four. Collect each host's containers in the corresponding input slot, then flatten in original host order so plans and output remain deterministic.

If multiple hosts fail, return a deterministic combined error naming every failed host. Do not return partially observed state to callers that currently require a complete snapshot.

Add tests for limit enforcement, stable flatten order despite out-of-order completion, and deterministic multi-host errors.

**Verify**: `go test -race ./internal/deployment -run 'TestInspectObserved' -count=1` → exit 0.

### Step 4: Parallelize read/preflight CLI phases by host

After Plan 009, update `preflightAgentProtocols` using fan-out. Each callback returns negotiation data or an error; the caller prints version-skew and incompatibility lines in original host order after all callbacks complete.

Preserve semantics:

- transport/unknown errors remain fatal;
- incompatible hosts are all reported;
- auto-upgrade begins only after the complete preflight result;
- output ordering remains host order;
- no agent upgrade itself is parallelized in this step.

Add tests with callbacks completing in reverse order and assert output/error order remains configured host order.

**Verify**: `go test -race ./internal/cli -run 'Test.*Agent.*Preflight' -count=1` → exit 0.

### Step 5: Parallelize independent write phases by host

Migrate these phases one at a time, with tests after each:

- `syncRemoteReleaseState`: one write per host; aggregate all host failures in host order.
- `writeRemoteSecretFiles`: group writes by host, process each host's files serially, run hosts concurrently. Never concurrently write multiple files on one host.
- `syncRemoteRegistryAuth`: compute credentials locally once, group by host, process registries serially on each host because Docker config merge is read-modify-write.
- provision bootstrap: one job per host; buffer per-host output and flush in host order. Preserve “all eligible hosts attempted” versus fail-fast behavior exactly as current code documents.

Use the same conservative limit constant. Do not add duplicate concurrency helpers.

**Verify**: after each migration, run the nearest focused CLI tests with `-race`; all pass and new limit/order tests pass.

### Step 6: Measure and inspect resource behavior

Re-run the benchmark. Add a focused integration-style fake SSH benchmark or test proving at most four one-shot calls are active and output remains deterministic.

Compare allocations and elapsed benchmark output with Step 1. If the helper adds material overhead at fleet size 1 or 4 without measurable concurrency benefit at 16/64, stop and report rather than landing complexity.

**Verify**: benchmark shows bounded concurrency behavior; `go test -race ./internal/fanout ./internal/deployment ./internal/cli` exits 0.

### Step 7: Run full verification

Format changed files and run Plan 005's local CI script.

**Verify**: `gofmt -l internal/fanout internal/deployment internal/cli` → no output; `./scripts/ci-local.sh` → exit 0.

## Test plan

Fan-out helper:

- empty input;
- invalid limit;
- limit never exceeded;
- result order independent of completion order;
- multiple errors retained by input index;
- parent cancellation stops queued work and joins active work;
- race detector clean.

Callers:

- observed containers flatten in host order;
- preflight output/errors deterministic;
- release-state write attempts all hosts;
- secrets and registry auth serialize operations within a host while hosts overlap;
- bootstrap output order stable;
- Plan 002 rollout compensation/action order unchanged.

## Done criteria

- [ ] A single tested helper bounds concurrent callbacks and preserves input order.
- [ ] No migrated phase exceeds four active host jobs.
- [ ] No two secret or registry-auth mutations run concurrently on the same host.
- [ ] All user-visible output and aggregated errors remain deterministic.
- [ ] Parent cancellation stops queued work and leaves no goroutine leak.
- [ ] `ExecuteActions` and rolling/canary/compensation order are unchanged.
- [ ] Before/after benchmark output is recorded; benefit is demonstrated for larger fake fleets.
- [ ] `go test -race ./internal/fanout ./internal/deployment ./internal/cli` exits 0.
- [ ] `./scripts/ci-local.sh` exits 0.
- [ ] No files outside the in-scope list changed.
- [ ] `plans/README.md` status row is updated.

## STOP conditions

Stop and report if:

- Plan 009 has not established the referenced CLI file boundaries.
- A target phase has hidden per-host or cross-host ordering requirements.
- Docker registry config or secret writes would overlap on the same host.
- Stable output requires printing concurrently from callbacks.
- Benchmark evidence does not show meaningful benefit at 16 or 64 fake hosts.
- Race tests expose package-global hooks that cannot safely run concurrently without a separate refactor.
- Any proposal includes parallel `ExecuteActions` work.

## Maintenance notes

- Keep the initial limit internal and conservative. A user-facing flag requires separate UX/config work and evidence.
- Future callers must document why jobs are independent before using the helper.
- Reviewers should focus on same-host serialization, deterministic error ordering, cancellation, and absence of rollout-order changes.