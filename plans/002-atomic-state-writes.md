# Plan 002: Make all local state and secrets file writes atomic and validated

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md` — unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**: `git diff --stat 29fc466..HEAD -- internal/state/ internal/deployment/ internal/secrets/`
> Note: at planning time the working tree already contained uncommitted
> changes in `internal/deployment/deployment.go` and
> `internal/secrets/secrets.go`. The excerpts below reflect the on-disk
> working-tree state. If any excerpt no longer matches the live code, STOP.

## Status

- **Priority**: P1
- **Effort**: S
- **Risk**: LOW
- **Depends on**: none
- **Category**: bug
- **Planned at**: commit `29fc466` (dirty working tree), 2026-07-01

## Why this matters

Ship's local state store was designed around crash-safe writes — a private
`atomicWriteFile` (temp file + fsync + rename) is used for releases, locks,
events, and accessory state. But four writers bypass it:

1. `SaveHostFacts` writes `hosts.json` with plain `os.WriteFile`. This file is
   the host inventory produced by provisioning; a crash or full disk mid-write
   leaves truncated JSON, and every subsequent operation that resolves hosts
   for that environment fails until the file is hand-repaired.
2. `SaveHostFacts` also skips the `validateStateName` check every sibling
   reader/writer applies, so an environment name containing `/` or `..` writes
   outside the intended state directory.
3. The deploy path snapshots the "previous ingress Caddyfile" with plain
   `os.WriteFile`; that snapshot is exactly what a failed deploy rolls back
   to, so corrupting it degrades ingress recovery on the path meant to
   protect it.
4. The encrypted secrets store (`.age` file) and its `.recipients` file are
   rewritten in full on every `secret set`/`unset` with plain `os.WriteFile` —
   a crash mid-write destroys the entire secrets store.

The fix is one small shared helper package plus mechanical call-site swaps.

## Current state

- `internal/state/state.go:728-765` — the existing atomic writer (this is the
  implementation to lift into the shared package):

  ```go
  func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
  	dir := filepath.Dir(path)
  	if err := os.MkdirAll(dir, 0o755); err != nil {
  		return err
  	}
  	tmp, err := os.CreateTemp(dir, ".ship-state-*")
  	...
  	if err := os.Rename(tmpName, path); err != nil {
  		return err
  	}
  	cleanup = false
  	_ = syncDir(dir)
  	return nil
  }
  ```

  (plus `syncDir` at `state.go:767-774`). In-package callers:
  `SaveDeployLock` (state.go:165), `SaveAccessoryState` (294), `RecordEvent`
  (409), `SaveReleaseRecord` (488), `PromoteRelease` (503 and 506).

- `internal/state/state.go:106-119` — the non-atomic, non-validated writer:

  ```go
  func (s Store) SaveHostFacts(environment string, hosts []HostFact) error {
  	if strings.TrimSpace(environment) == "" {
  		return errors.New("environment is required")
  	}
  	dir := filepath.Join(s.Dir, "environments", environment)
  	if err := os.MkdirAll(dir, 0o755); err != nil {
  		return err
  	}
  	data, err := json.MarshalIndent(hosts, "", "  ")
  	if err != nil {
  		return err
  	}
  	return os.WriteFile(filepath.Join(dir, "hosts.json"), data, 0o644)
  }
  ```

  Contrast `ReadHostFacts` (state.go:121-135), which calls
  `validateStateName("environment", environment)` first. `validateStateName`
  is at state.go:718-726 (rejects empty, `.`, `..`, and any `/` or `\`).

- `internal/deployment/deployment.go:552` — inside `executeIngressAction`:

  ```go
  if err := os.WriteFile(action.IngressPath, []byte(action.IngressConfig), 0o644); err != nil {
  ```

  The file written here is read back by `readPreviousIngressConfig`
  (deployment.go:561) to roll back ingress on a later failed deploy.

- `internal/secrets/secrets.go:576-580` — end of `WriteStoreWithRecipients`:

  ```go
  path := StorePath(opts)
  if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
  	return err
  }
  return os.WriteFile(path, encrypted.Bytes(), 0o644)
  ```

  and `internal/secrets/secrets.go:541-545` — end of `WriteRecipients`:

  ```go
  path := RecipientsPath(opts)
  if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
  	return err
  }
  return os.WriteFile(path, []byte(b.String()), 0o644)
  ```

- Repo conventions: table-driven or focused single-behavior tests colocated as
  `*_test.go`; see `internal/state/state_test.go` for the state-store test
  style (uses `t.TempDir()` and `NewStore(dir)`).

## Commands you will need

| Purpose  | Command                         | Expected on success |
|----------|---------------------------------|---------------------|
| Build    | `go build ./cmd/ship`           | exit 0              |
| Tests    | `go test ./internal/...`        | all pass            |
| One pkg  | `go test ./internal/state`      | all pass            |
| Vet      | `go vet ./...`                  | exit 0              |

## Scope

**In scope** (the only files you should modify/create):
- `internal/fsatomic/fsatomic.go` (create)
- `internal/fsatomic/fsatomic_test.go` (create)
- `internal/state/state.go`
- `internal/state/state_test.go`
- `internal/deployment/deployment.go` (one line + import)
- `internal/secrets/secrets.go` (two call sites + import)

**Out of scope** (do NOT touch, even though they look related):
- `internal/agent/rpc.go` — it has its own `atomicWriteFile` (rpc.go:1299)
  that runs on the *remote host* inside the agent; it is already atomic and
  the agent binary should not grow a dependency for this.
- Every other `os.WriteFile` call site (e.g. `ship init` scaffolding in
  `internal/cli/root.go`) — those create brand-new files where a partial
  write is recoverable by re-running.
- Any behavior change to what is written (content, permissions, paths).

## Git workflow

- Branch: `advisor/002-atomic-state-writes`
- Commit style example from log: `Fix SSH transport tests on Linux CI runners`.
  Suggested: `fix(state): write hosts.json, ingress snapshots, and secret stores atomically`
- Do NOT push or open a PR unless the operator instructed it.

## Steps

### Step 1: Create `internal/fsatomic`

Create `internal/fsatomic/fsatomic.go` with package doc and one exported
function `WriteFile(path string, data []byte, mode os.FileMode) error`, moving
the exact implementation from `internal/state/state.go:728-774`
(`atomicWriteFile` + `syncDir`; rename the temp-file prefix to `.ship-tmp-*`).
No other dependencies — stdlib only.

Create `internal/fsatomic/fsatomic_test.go` covering: (a) writes content and
mode to a fresh path in `t.TempDir()`; (b) overwrites an existing file and
the old content is fully replaced; (c) creates missing parent directories;
(d) leaves no `.ship-tmp-*` litter in the directory after success.

**Verify**: `go test ./internal/fsatomic` → `ok`.

### Step 2: Switch `internal/state` to the shared helper

In `internal/state/state.go`: delete the private `atomicWriteFile` and
`syncDir`, import `github.com/watzon/ship/internal/fsatomic`, and replace all
in-package calls (`SaveDeployLock`, `SaveAccessoryState`, `RecordEvent`,
`SaveReleaseRecord`, `PromoteRelease` ×2) with `fsatomic.WriteFile`. The
compiler will find every site.

**Verify**: `go test ./internal/state` → all pass (no behavior change).

### Step 3: Fix `SaveHostFacts`

Rewrite `SaveHostFacts` (state.go:106-119) to match its siblings:

```go
func (s Store) SaveHostFacts(environment string, hosts []HostFact) error {
	environment = strings.TrimSpace(environment)
	if err := validateStateName("environment", environment); err != nil {
		return err
	}
	data, err := json.MarshalIndent(hosts, "", "  ")
	if err != nil {
		return err
	}
	return fsatomic.WriteFile(filepath.Join(s.Dir, "environments", environment, "hosts.json"), data, 0o644)
}
```

(`fsatomic.WriteFile` already does `MkdirAll`, so the explicit one is dropped.)

In `internal/state/state_test.go`, add:
- `TestSaveHostFactsRejectsInvalidEnvironmentName` — `SaveHostFacts("../evil", …)`
  and `SaveHostFacts("a/b", …)` both return an error and create no file
  (check with `os.Stat` under the temp store dir).
- A save→read round-trip if no existing test covers it (search the file for
  `SaveHostFacts` first; extend rather than duplicate).

**Verify**: `go test ./internal/state` → all pass, including the new tests.

### Step 4: Fix the ingress snapshot write

In `internal/deployment/deployment.go:552`, replace
`os.WriteFile(action.IngressPath, []byte(action.IngressConfig), 0o644)` with
`fsatomic.WriteFile(...)` (same args) and add the import. Leave the
surrounding rollback-on-error logic exactly as is. The `os.MkdirAll` call just
above it (deployment.go:545) may stay — it also guards the rollback branch.

**Verify**: `go build ./cmd/ship` → exit 0; `go test ./internal/deployment` → all pass.

### Step 5: Fix the secrets store and recipients writes

In `internal/secrets/secrets.go`, replace the final
`os.MkdirAll` + `os.WriteFile` pairs in `WriteStoreWithRecipients` (576-580)
and `WriteRecipients` (541-545) with single `fsatomic.WriteFile` calls (same
path, data, and `0o644` mode). Add the import.

**Verify**: `go test ./internal/secrets` → all pass (the round-trip test
`TestRenderForEnvUsesEncryptedStoreDotenvAndEnvPrecedence` exercises
`InitStore`/`SetStoredSecret`, which covers both writers).

## Test plan

- New: `internal/fsatomic/fsatomic_test.go` (4 cases listed in Step 1).
- New: `TestSaveHostFactsRejectsInvalidEnvironmentName` (+ round-trip if
  missing) in `internal/state/state_test.go`, modeled on the existing
  `t.TempDir()` + `NewStore` style in that file.
- Existing suites in `internal/state`, `internal/deployment`,
  `internal/secrets` act as regression gates.
- Verification: `go test ./internal/...` → all pass.

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `go test ./internal/...` exits 0; new fsatomic and SaveHostFacts tests exist and pass
- [ ] `grep -n "os.WriteFile" internal/state/state.go internal/deployment/deployment.go` → no matches
- [ ] `grep -c "os.WriteFile" internal/secrets/secrets.go` → 0
- [ ] `grep -n "func atomicWriteFile" internal/state/state.go` → no match (moved to fsatomic)
- [ ] `go vet ./...` exits 0
- [ ] `git status` shows changes only in the in-scope files
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back (do not improvise) if:

- Any "Current state" excerpt doesn't match the live code (especially
  `deployment.go` and `secrets.go`, which had uncommitted changes at planning
  time — the line numbers may have shifted; the *code shapes* must match).
- `internal/deployment` importing `internal/fsatomic` creates an import cycle
  (it should not — fsatomic is stdlib-only — but if the compiler says
  otherwise, stop).
- Existing tests fail after Step 2 (that swap must be behavior-neutral).

## Maintenance notes

- `internal/agent/rpc.go:1299` still has its own private `atomicWriteFile`
  for remote-host writes. Consolidating it into fsatomic was deliberately
  deferred: the agent runs on remote hosts and its code paths are
  release-sensitive. If someone later unifies them, keep the temp-prefix
  distinct so remote litter is identifiable.
- Any new state-file writer added to `internal/state` should use
  `fsatomic.WriteFile` — a reviewer seeing raw `os.WriteFile` in that package
  should push back.
- Reviewer focus: Step 3 tightens `SaveHostFacts` from "non-empty" to
  `validateStateName` — if some caller passes an environment name with a
  slash today it will now error. Grep `SaveHostFacts` callers
  (`internal/cli/root.go`) and confirm environment names come from config
  environment keys (they do — same names already pass `ReadHostFacts`).
