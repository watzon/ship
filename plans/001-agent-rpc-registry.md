# Plan 001: Register every agent RPC method from one dispatch table

> **Executor instructions**: Follow this plan step by step. Run every verification command and confirm the expected result before moving to the next step. If anything in the "STOP conditions" section occurs, stop and report; do not improvise. When done, update this plan's row in `plans/README.md` unless a reviewer told you they maintain the index.
>
> **Drift check (run first)**: `git diff --stat 93da974..HEAD -- internal/agent/rpc.go internal/agent/rpc_test.go`
> If either file changed, compare the "Current state" excerpts with the live code. If the dispatch switch or supported-method list no longer matches these excerpts, stop and report the drift.

## Status

- **Priority**: P1
- **Effort**: S
- **Risk**: LOW
- **Depends on**: none
- **Category**: tech-debt
- **Planned at**: commit `93da974`, 2026-07-16

## Why this matters

Agent protocol negotiation advertises a hard-coded method list that is separate from the `Server.Handle` switch. A future method can therefore be callable but not advertised, or advertised but routed to `unknown_method`. Fleet preflight and mixed-version upgrades depend on this metadata. A single immutable dispatch table removes that drift class before plans add more lifecycle methods.

## Current state

- `internal/agent/rpc.go` owns request routing, handlers, and protocol negotiation.
- `internal/agent/rpc_test.go` uses in-package fakes and direct `Server.Handle` calls; add tests there.
- Keep the current package and error model. Unknown methods must still return `ErrorUnknownMethod` through `failure`.
- Avoid a per-request map allocation. A package-level dispatch table is initialized once.

Current routing starts with a switch (`internal/agent/rpc.go:374-392`):

```go
func (s Server) Handle(ctx context.Context, req Request) Response {
	switch req.Method {
	case "negotiate":
		return s.negotiate(req)
	case "status":
		return s.status(ctx, req)
	case "pull":
		return s.withHostLock(req, "pull", func() Response {
			var p struct {
				Image string `json:"image"`
			}
```

The same names are repeated for negotiation (`internal/agent/rpc.go:1225-1254`):

```go
func supportedMethods() []string {
	methods := []string{
		"accessory_backup",
		"accessory_restore",
		"caddy_reload",
		"docker_inspect",
		"exec_container",
		"health_check",
```

There is currently no test that proves these two surfaces are identical.

## Commands you will need

| Purpose | Command | Expected on success |
|---|---|---|
| Focused tests | `go test ./internal/agent -count=1` | exit 0; all agent tests pass |
| Format check | `gofmt -l internal/agent/rpc.go internal/agent/rpc_test.go` | no output |
| Vet | `go vet ./internal/agent` | exit 0; no diagnostics |
| Full race suite | `go test -race ./...` | exit 0; all packages pass |

## Scope

**In scope** (the only source files to modify):

- `internal/agent/rpc.go`
- `internal/agent/rpc_test.go`
- `plans/README.md` for status only

**Out of scope**:

- Protocol version changes (`AgentMinProtocol`, `AgentProtocol`).
- Request/response JSON shape changes.
- New RPC methods or renamed method strings.
- Changes to `internal/agent/client.go`, Docker operations, CLI preflight, or SSH transport.
- Changing which handlers take the host lock.

## Git workflow

- Branch: `advisor/001-agent-rpc-registry`
- Commit message: `refactor(agent): centralize RPC method registration`
- Do not push or open a PR unless instructed.

## Steps

### Step 1: Extract handlers for inline switch cases

In `internal/agent/rpc.go`, extract each switch case that currently contains inline validation or Docker calls into a `Server` method with this uniform shape:

```go
func (s Server) handlePull(ctx context.Context, req Request) Response
```

Use equivalent names for prune, run, stop, logs, inspect, and list operations. Existing methods such as `negotiate`, `status`, `healthCheck`, `execContainer`, `writeFile`, and `installBinary` stay intact and are wrapped by thin dispatch functions only when their signatures differ.

Preserve every existing `withHostLock` boundary and error code. This step is behavior-preserving; do not clean up validation or error strings.

**Verify**: `go test ./internal/agent -count=1` → exit 0.

### Step 2: Add one package-level dispatch table

Define a named handler type that accepts `Server`, `context.Context`, and `Request`, then create one package-level map keyed by the exact current method strings. Each entry must route to the same handler and lock behavior as the old switch.

Replace `Server.Handle` with a lookup:

1. Look up `req.Method`.
2. Return `failure(req.ID, ErrorUnknownMethod, fmt.Errorf("unknown method %q", req.Method))` when absent.
3. Invoke the registered handler when present.

Do not construct the map inside `Handle` and do not use reflection.

**Verify**: `go test ./internal/agent -run 'Test.*(Unknown|Method|Negotiate|Status)' -count=1` → exit 0.

### Step 3: Derive supported methods from registry keys

Rewrite `supportedMethods` to allocate a slice sized to the registry, append each key, sort it with `sort.Strings`, and return it. Remove the duplicate string list completely.

Keep deterministic lexical ordering because negotiation responses and tests may compare slices.

**Verify**: `go test ./internal/agent -count=1` → exit 0.

### Step 4: Add drift-proof tests

In `internal/agent/rpc_test.go`, add tests that:

- Assert `supportedMethods()` is sorted, contains no duplicates, and has exactly `len(rpcHandlers)` entries.
- Assert every key in the registry appears in `supportedMethods()`.
- Assert a made-up method still returns `ErrorUnknownMethod`.
- Assert representative locked and unlocked methods retain behavior using existing fakes; at minimum cover `pull`, `status`, and `write_file` or the nearest existing equivalents.

Do not assert source text or switch absence. Test observable registration behavior.

**Verify**: `go test ./internal/agent -count=1` → exit 0; new tests pass.

### Step 5: Run repository gates

Run formatting before the checks:

```bash
gofmt -w internal/agent/rpc.go internal/agent/rpc_test.go
```

Then run the focused and full gates.

**Verify**: `gofmt -l internal/agent/rpc.go internal/agent/rpc_test.go` → no output; `go vet ./internal/agent` → exit 0; `go test -race ./...` → exit 0.

## Test plan

- Extend `internal/agent/rpc_test.go` using the existing direct `Server.Handle` and fake-Docker style.
- Cover registry/list equality, deterministic sorting, unknown methods, and lock-preserving representative dispatch.
- Do not add snapshots or tests that parse Go source.
- Final verification: `go test -race ./...` → all packages pass.

## Done criteria

- [ ] `Server.Handle` routes exclusively through one package-level registry.
- [ ] `supportedMethods` is derived exclusively from registry keys.
- [ ] Every pre-existing method string remains registered exactly once.
- [ ] Existing error codes, params, results, and host-lock behavior are unchanged.
- [ ] `gofmt -l internal/agent/rpc.go internal/agent/rpc_test.go` prints nothing.
- [ ] `go vet ./internal/agent` exits 0.
- [ ] `go test -race ./...` exits 0.
- [ ] No source files outside the in-scope list changed.
- [ ] `plans/README.md` status row is updated.

## STOP conditions

Stop and report if:

- The live method set differs from the switch/list at commit `93da974`.
- A handler cannot be expressed through the common function type without changing request or response behavior.
- Preserving lock behavior requires edits outside `internal/agent/rpc.go`.
- Any method name or protocol version appears to need a compatibility migration.
- A focused test still fails after two reasonable behavior-preserving attempts.

## Maintenance notes

- New RPC methods must be added only to the registry; negotiation will advertise them automatically.
- Reviewers should compare the registry against the removed switch one-for-one and scrutinize host-lock preservation.
- Plan 002 depends on this registry because it may add container lifecycle RPC methods.